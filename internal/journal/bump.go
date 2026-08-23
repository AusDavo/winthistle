package journal

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/AusDavo/winthistle/internal/abort"
	"github.com/AusDavo/winthistle/internal/bitcoind"
)

// BumpPlan is what is known about a CPFP child before Core is asked to build it.
//
// Every figure here is read rather than assumed: the parent's size and fee come
// from Core's own mempool entry, and the change outpoint from the cold wallet's
// listunspent. That is the point of writing them down — a bump found in the
// journal has to say what arithmetic it was built against, because the mempool
// it was built against will have moved on.
type BumpPlan struct {
	ParentTxID     string
	ParentVsizeVB  int64
	ParentFeeSat   int64
	Change         bitcoind.Outpoint
	ChangeSat      int64
	TargetSatPerVB float64
}

// Bump is one CPFP child as the journal holds it.
type Bump struct {
	RunID string

	// Seq numbers the children of one run, from 1. It is the key rather than the
	// child's txid because the txid is not known until Core has built the
	// transaction — and the row has to exist before that call, so that the coin
	// lock it takes has an owner on disk.
	Seq int64

	State BumpState
	Plan  BumpPlan

	// ChildTxID and ChildFeeSat are empty until the build returns, RawTx until
	// the partials are merged and finalized.
	ChildTxID   string
	ChildFeeSat int64
	RawTx       string

	CreatedAt time.Time
	UpdatedAt time.Time

	Signers []Signer
	Locks   []Lock
}

// MayBePublic reports whether this child's bytes may have reached a mempool.
func (b *Bump) MayBePublic() bool {
	return b.State == BumpPublishing || b.State == BumpPublished
}

// Finished reports whether there is nothing left for an operator to decide.
func (b *Bump) Finished() bool {
	return b.State == BumpPublished || b.State == BumpAbandoned
}

// AbortTarget is what giving up on this child would undo.
//
// One list, where a run has three, and that asymmetry is the whole reason a bump
// is not a run. There is no shim to cancel — a child talks to no peer — and
// nothing to abandon, because a child creates no channel. What is left is the
// coin lock Core is holding on the parent's change output.
//
// It does not refuse a child that may be public, which is where it parts company
// with Run.AbortTarget, and the difference is not laxity. Run.AbortTarget refuses
// because the action it would take is destructive: abandoning a pending channel
// whose funding transaction then confirms strands its funds. Releasing a coin
// lock takes nothing back and cannot strand anything — the lock is a local note
// saying "do not spend this", and the child either confirms or it does not,
// regardless. What the caller has to be told is that releasing the lock is not
// un-broadcasting the child, and that is the report's job rather than a refusal.
func (b *Bump) AbortTarget() abort.Target {
	var t abort.Target
	for _, l := range b.Locks {
		if !l.Released {
			t.Locks = append(t.Locks, l.Outpoint)
		}
	}
	return t
}

// BeginBump records a CPFP child before Core is asked to build it.
//
// Two things happen here that are deliberately in this order. The row is written
// before walletcreatefundedpsbt is called, and the change outpoint is recorded
// as a held lock before Core is holding it. Both are the journal's usual
// discipline pointed at the one window this command has: that call is what takes
// the lock, so a crash inside it would otherwise leave a locked coin that only
// Core knows about and nothing on disk claiming it. Claiming it early costs a
// release attempt for a lock that was never taken, which ReleaseLocks filters
// out because it asks Core what it actually holds first.
//
// It refuses a run whose funding transaction never reached the publish call.
// That is a gate rather than a validation: a CPFP child accelerates a
// transaction that is already in a mempool, and a child of a transaction nobody
// has is one that can never confirm — it would spend an output that does not
// exist. The journal is the component that knows which runs got that far, the
// same way it is the one that knows whether every chan_pending arrived.
func (j *Journal) BeginBump(ctx context.Context, runID string, p BumpPlan) (int64, error) {
	switch {
	case p.ParentTxID == "":
		return 0, errors.New("a bump needs the parent's txid")
	case p.Change.TxID == "":
		return 0, errors.New("a bump needs the parent's change outpoint: it is the " +
			"only thing the child spends")
	case p.ParentVsizeVB <= 0 || p.ParentFeeSat <= 0:
		return 0, fmt.Errorf("the parent's size (%d vB) and fee (%d sat) are what "+
			"decide the child's, and one of them is missing",
			p.ParentVsizeVB, p.ParentFeeSat)
	case p.ChangeSat <= 0:
		return 0, fmt.Errorf("the parent's change output is %d sat", p.ChangeSat)
	case p.TargetSatPerVB <= 0:
		return 0, fmt.Errorf("a package target of %g sat/vB is not a rate", p.TargetSatPerVB)
	}

	var seq int64
	err := j.tx(ctx, func(tx *sql.Tx) error {
		var state string
		var txid sql.NullString
		err := tx.QueryRowContext(ctx,
			`SELECT state, txid FROM runs WHERE id = ?`, runID).Scan(&state, &txid)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("run %s: %w", runID, ErrNoRun)
		}
		if err != nil {
			return fmt.Errorf("reading run %s: %w", runID, err)
		}
		switch State(state) {
		case StatePublishing, StatePublished:
		default:
			return fmt.Errorf("run %s is %s, so nothing of it is in any mempool: %w",
				runID, state, ErrNotPublic)
		}
		if txid.String != p.ParentTxID {
			return fmt.Errorf("run %s published %s, not %s. A child of the wrong "+
				"parent accelerates nothing and cannot confirm",
				runID, orNone(txid.String), p.ParentTxID)
		}

		var next sql.NullInt64
		if err := tx.QueryRowContext(ctx,
			`SELECT MAX(seq) FROM bumps WHERE run_id = ?`, runID).Scan(&next); err != nil {
			return fmt.Errorf("numbering the bumps of run %s: %w", runID, err)
		}
		seq = next.Int64 + 1

		stamp := now()
		_, err = tx.ExecContext(ctx,
			`INSERT INTO bumps (run_id, seq, state, parent_txid, parent_vsize_vb,
			                    parent_fee_sat, change_txid, change_vout, change_sat,
			                    target_sat_per_vb, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			runID, seq, string(BumpBuilding), p.ParentTxID, p.ParentVsizeVB,
			p.ParentFeeSat, p.Change.TxID, p.Change.Vout, p.ChangeSat,
			p.TargetSatPerVB, stamp, stamp)
		if err != nil {
			return fmt.Errorf("recording bump %d of run %s: %w", seq, runID, err)
		}
		return j.addBumpLocks(ctx, tx, runID, seq, []bitcoind.Outpoint{p.Change})
	})
	if err != nil {
		return 0, err
	}
	return seq, nil
}

// RecordBumpChild stores what Core built, and the coins it actually locked.
//
// The inputs are recorded as Core reports them rather than as they were
// predicted. They should be the one change outpoint BeginBump already claimed —
// add_inputs is off, so there is nothing for coin selection to add — and
// recording Core's answer anyway is what would make a disagreement visible
// instead of leaving a lock nothing releases.
func (j *Journal) RecordBumpChild(ctx context.Context, runID string, seq int64,
	childTxID string, childFeeSat int64, inputs []bitcoind.Outpoint) error {

	if childTxID == "" {
		return fmt.Errorf("run %s bump %d: the child has no txid", runID, seq)
	}
	return j.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE bumps SET state = ?, child_txid = ?, child_fee_sat = ?, updated_at = ?
			 WHERE run_id = ? AND seq = ?`,
			string(BumpSigning), childTxID, childFeeSat, now(), runID, seq)
		if err != nil {
			return fmt.Errorf("recording the child of bump %d in run %s: %w", seq, runID, err)
		}
		if err := affectedOneBump(res, runID, seq); err != nil {
			return err
		}
		return j.addBumpLocks(ctx, tx, runID, seq, inputs)
	})
}

// RecordBumpSigner notes how far one cold-storage signer has got with the child.
//
// A separate table from the batch's signers rather than a column on it, because
// this is a second cold-wallet session minutes or days after the first and the
// two answers are not interchangeable. No key material, here as there.
func (j *Journal) RecordBumpSigner(ctx context.Context, runID string, seq int64,
	label string, st SignerState) error {

	if label == "" {
		return fmt.Errorf("run %s bump %d: a signer needs a label", runID, seq)
	}
	return j.tx(ctx, func(tx *sql.Tx) error {
		if err := j.mustExistBump(ctx, tx, runID, seq); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO bump_signers (run_id, seq, label, state, updated_at)
			 VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT (run_id, seq, label) DO UPDATE SET state = excluded.state,
			                                                updated_at = excluded.updated_at`,
			runID, seq, label, string(st), now())
		if err != nil {
			return fmt.Errorf("recording signer %q of bump %d in run %s: %w",
				label, seq, runID, err)
		}
		return j.touchBump(ctx, tx, runID, seq)
	})
}

// RecordBumpRawTx stores the finalized child and moves the bump to signed.
//
// The txid is checked against the one the build reported rather than overwritten.
// Nothing should have moved it — the child's inputs and outputs are fixed at
// build time and only witnesses were added — and a mismatch here would mean the
// merge produced a different transaction from the one whose fee and rate were
// verified, which is the child's own version of I-3.
func (j *Journal) RecordBumpRawTx(ctx context.Context, runID string, seq int64,
	childTxID, rawTxHex string) error {

	if childTxID == "" || rawTxHex == "" {
		return fmt.Errorf("run %s bump %d: the finalized child needs both a txid "+
			"and its bytes", runID, seq)
	}
	return j.tx(ctx, func(tx *sql.Tx) error {
		var was sql.NullString
		err := tx.QueryRowContext(ctx,
			`SELECT child_txid FROM bumps WHERE run_id = ? AND seq = ?`,
			runID, seq).Scan(&was)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("run %s bump %d: %w", runID, seq, ErrNoBump)
		}
		if err != nil {
			return fmt.Errorf("reading bump %d of run %s: %w", seq, runID, err)
		}
		if was.String != "" && was.String != childTxID {
			return fmt.Errorf("run %s bump %d was built as %s and finalized as %s. "+
				"Only signatures were added, so the txid could not have moved — do "+
				"not broadcast this child", runID, seq, was.String, childTxID)
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE bumps SET state = ?, raw_tx = ?, updated_at = ?
			 WHERE run_id = ? AND seq = ?`,
			string(BumpSigned), rawTxHex, now(), runID, seq)
		if err != nil {
			return fmt.Errorf("recording the finalized child of bump %d in run %s: %w",
				seq, runID, err)
		}
		return affectedOneBump(res, runID, seq)
	})
}

// MarkBumpPublishing is the write that must land before the child is broadcast.
//
// The same shape as MarkPublishing and for one of the same two reasons. It is
// not the I-1 gate — there is no gate here, because the parent is already public
// and every channel in the batch was recoverable before it went out — but the
// other half holds exactly: a child found in this state may be in a mempool, and
// nothing else on disk would say so.
//
// It refuses a child that is not signed, and refuses a signed child with no
// bytes. Both are the same refusal from different sides: the only thing worth
// broadcasting is a transaction that was combined in-app, finalized in-app and
// checked against the parent it claims to accelerate.
func (j *Journal) MarkBumpPublishing(ctx context.Context, runID string, seq int64) error {
	return j.tx(ctx, func(tx *sql.Tx) error {
		var state string
		var rawTx sql.NullString
		err := tx.QueryRowContext(ctx,
			`SELECT state, raw_tx FROM bumps WHERE run_id = ? AND seq = ?`,
			runID, seq).Scan(&state, &rawTx)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("run %s bump %d: %w", runID, seq, ErrNoBump)
		}
		if err != nil {
			return fmt.Errorf("reading bump %d of run %s: %w", seq, runID, err)
		}
		if BumpState(state) != BumpSigned {
			return fmt.Errorf("bump %d of run %s is %s, not %s: %w",
				seq, runID, state, BumpSigned, ErrBumpNotSigned)
		}
		if !rawTx.Valid || rawTx.String == "" {
			return fmt.Errorf("bump %d of run %s is signed but no finalized child "+
				"was journalled — there would be nothing to broadcast: %w",
				seq, runID, ErrBumpNotSigned)
		}
		return j.setBumpState(ctx, tx, runID, seq, BumpPublishing)
	})
}

// MarkBumpPublished records that the child's publish call returned without error.
func (j *Journal) MarkBumpPublished(ctx context.Context, runID string, seq int64) error {
	return j.tx(ctx, func(tx *sql.Tx) error {
		return j.setBumpState(ctx, tx, runID, seq, BumpPublished)
	})
}

// AbandonBump gives up on a child and releases the coin lock it was holding.
//
// Not the same act as aborting a run, and the report says so. Nothing is undone
// here: if the child was already broadcast, releasing the lock does not recall
// it, and the state stays whatever it was so that remains visible. What this
// fixes is the other failure — a cold wallet that quietly declines to spend its
// own change because a child that will never be signed is still holding it.
func (j *Journal) AbandonBump(ctx context.Context, core abort.LockReleaser,
	runID string, seq int64) (*abort.Report, error) {

	b, err := j.LoadBump(ctx, runID, seq)
	if err != nil {
		return nil, err
	}

	rep := &abort.Report{}
	target := b.AbortTarget()
	if len(target.Locks) > 0 {
		freed, err := core.ReleaseLocks(ctx, target.Locks)
		if err != nil {
			rep.Failures = append(rep.Failures,
				fmt.Errorf("releasing the %d coin lock(s) bump %d of run %s holds: %w",
					len(target.Locks), seq, runID, err))
		} else {
			rep.LocksFreed = freed
		}
	}

	err = j.tx(ctx, func(tx *sql.Tx) error {
		for _, op := range rep.LocksFreed {
			_, err := tx.ExecContext(ctx,
				`UPDATE bump_locks SET released = 1
				 WHERE run_id = ? AND seq = ? AND txid = ? AND vout = ?`,
				runID, seq, op.TxID, op.Vout)
			if err != nil {
				return fmt.Errorf("recording the release of %s in bump %d of run %s: %w",
					op, seq, runID, err)
			}
		}
		if !rep.Clean() {
			return j.touchBump(ctx, tx, runID, seq)
		}
		// Core's locks are memory-only, so "we did not free it" and "nothing is
		// holding it" are the same end state — ReleaseLocks reports only what it
		// actually freed, having filtered against what Core holds. On a clean
		// release nothing of this child's is locked any more, and saying so is
		// what stops every later listing re-offering the same outpoints.
		if _, err := tx.ExecContext(ctx,
			`UPDATE bump_locks SET released = 1 WHERE run_id = ? AND seq = ?`,
			runID, seq); err != nil {
			return fmt.Errorf("closing out the coin locks of bump %d in run %s: %w",
				seq, runID, err)
		}
		// A child that may be public keeps its state. "Abandoned" would be a
		// claim about the network that this call cannot make: the bytes are out,
		// or they are not, and releasing a lock changed neither.
		if b.MayBePublic() {
			return j.touchBump(ctx, tx, runID, seq)
		}
		return j.setBumpState(ctx, tx, runID, seq, BumpAbandoned)
	})
	if err != nil {
		return rep, err
	}
	if !rep.Clean() {
		return rep, fmt.Errorf("giving up on bump %d of run %s did not fully "+
			"complete: %w", seq, runID, errors.Join(rep.Failures...))
	}
	return rep, nil
}

// LoadBump reads one child back out of the journal.
func (j *Journal) LoadBump(ctx context.Context, runID string, seq int64) (*Bump, error) {
	b := &Bump{RunID: runID, Seq: seq}
	var (
		state, created, upd string
		childTxID, rawTx    sql.NullString
		childFee            sql.NullInt64
		changeVout          int64
	)
	err := j.db.QueryRowContext(ctx,
		`SELECT state, parent_txid, parent_vsize_vb, parent_fee_sat, change_txid,
		        change_vout, change_sat, target_sat_per_vb, child_txid, child_fee_sat,
		        raw_tx, created_at, updated_at
		   FROM bumps WHERE run_id = ? AND seq = ?`, runID, seq).
		Scan(&state, &b.Plan.ParentTxID, &b.Plan.ParentVsizeVB, &b.Plan.ParentFeeSat,
			&b.Plan.Change.TxID, &changeVout, &b.Plan.ChangeSat, &b.Plan.TargetSatPerVB,
			&childTxID, &childFee, &rawTx, &created, &upd)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("run %s bump %d: %w", runID, seq, ErrNoBump)
	}
	if err != nil {
		return nil, fmt.Errorf("reading bump %d of run %s: %w", seq, runID, err)
	}
	b.State = BumpState(state)
	b.Plan.Change.Vout = uint32(changeVout)
	b.ChildTxID, b.ChildFeeSat, b.RawTx = childTxID.String, childFee.Int64, rawTx.String
	b.CreatedAt, b.UpdatedAt = parseTime(created), parseTime(upd)

	if err := j.loadBumpSigners(ctx, b); err != nil {
		return nil, err
	}
	if err := j.loadBumpLocks(ctx, b); err != nil {
		return nil, err
	}
	return b, nil
}

// Bumps lists every child of one run, oldest first.
func (j *Journal) Bumps(ctx context.Context, runID string) ([]*Bump, error) {
	seqs, err := j.bumpSeqs(ctx,
		`SELECT seq FROM bumps WHERE run_id = ? ORDER BY seq`, runID)
	if err != nil {
		return nil, err
	}
	out := make([]*Bump, 0, len(seqs))
	for _, seq := range seqs {
		b, err := j.LoadBump(ctx, runID, seq)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

// UnfinishedBumps lists the children that are neither published nor abandoned.
//
// The counterpart of Unfinished, and it has the same two jobs: it is what a
// recovery screen lists, and it is where `winthistle doctor` learns that a coin
// lock has an owner. Without it every child's lock reads as an orphan, because
// the run that owns it is published and published runs are not unfinished.
func (j *Journal) UnfinishedBumps(ctx context.Context) ([]*Bump, error) {
	rows, err := j.db.QueryContext(ctx,
		`SELECT run_id, seq FROM bumps WHERE state NOT IN (?, ?)
		 ORDER BY created_at, run_id, seq`,
		string(BumpPublished), string(BumpAbandoned))
	if err != nil {
		return nil, fmt.Errorf("listing unfinished bumps: %w", err)
	}
	type key struct {
		run string
		seq int64
	}
	var keys []key
	for rows.Next() {
		var k key
		if err := rows.Scan(&k.run, &k.seq); err != nil {
			rows.Close()
			return nil, fmt.Errorf("listing unfinished bumps: %w", err)
		}
		keys = append(keys, k)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("listing unfinished bumps: %w", err)
	}
	rows.Close()

	out := make([]*Bump, 0, len(keys))
	for _, k := range keys {
		b, err := j.LoadBump(ctx, k.run, k.seq)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

// ---- helpers ----

func (j *Journal) bumpSeqs(ctx context.Context, query string, args ...any) ([]int64, error) {
	rows, err := j.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing bumps: %w", err)
	}
	defer rows.Close()

	var out []int64
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			return nil, fmt.Errorf("listing bumps: %w", err)
		}
		out = append(out, seq)
	}
	return out, rows.Err()
}

func (j *Journal) addBumpLocks(ctx context.Context, tx *sql.Tx, runID string,
	seq int64, ops []bitcoind.Outpoint) error {

	for _, op := range ops {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO bump_locks (run_id, seq, txid, vout) VALUES (?, ?, ?, ?)
			 ON CONFLICT (run_id, seq, txid, vout) DO NOTHING`,
			runID, seq, op.TxID, op.Vout)
		if err != nil {
			return fmt.Errorf("recording coin lock %s of bump %d in run %s: %w",
				op, seq, runID, err)
		}
	}
	return nil
}

func (j *Journal) setBumpState(ctx context.Context, tx *sql.Tx, runID string,
	seq int64, st BumpState) error {

	res, err := tx.ExecContext(ctx,
		`UPDATE bumps SET state = ?, updated_at = ? WHERE run_id = ? AND seq = ?`,
		string(st), now(), runID, seq)
	if err != nil {
		return fmt.Errorf("moving bump %d of run %s to %s: %w", seq, runID, st, err)
	}
	return affectedOneBump(res, runID, seq)
}

func (j *Journal) touchBump(ctx context.Context, tx *sql.Tx, runID string, seq int64) error {
	res, err := tx.ExecContext(ctx,
		`UPDATE bumps SET updated_at = ? WHERE run_id = ? AND seq = ?`,
		now(), runID, seq)
	if err != nil {
		return fmt.Errorf("updating bump %d of run %s: %w", seq, runID, err)
	}
	return affectedOneBump(res, runID, seq)
}

func (j *Journal) mustExistBump(ctx context.Context, tx *sql.Tx, runID string, seq int64) error {
	var one int
	err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM bumps WHERE run_id = ? AND seq = ?`, runID, seq).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("run %s bump %d: %w", runID, seq, ErrNoBump)
	}
	if err != nil {
		return fmt.Errorf("reading bump %d of run %s: %w", seq, runID, err)
	}
	return nil
}

func (j *Journal) loadBumpSigners(ctx context.Context, b *Bump) error {
	rows, err := j.db.QueryContext(ctx,
		`SELECT label, state, updated_at FROM bump_signers
		  WHERE run_id = ? AND seq = ? ORDER BY label`, b.RunID, b.Seq)
	if err != nil {
		return fmt.Errorf("reading the signers of bump %d in run %s: %w", b.Seq, b.RunID, err)
	}
	defer rows.Close()

	for rows.Next() {
		var label, state, upd string
		if err := rows.Scan(&label, &state, &upd); err != nil {
			return fmt.Errorf("reading the signers of bump %d in run %s: %w",
				b.Seq, b.RunID, err)
		}
		b.Signers = append(b.Signers, Signer{
			Label:     label,
			State:     SignerState(state),
			UpdatedAt: parseTime(upd),
		})
	}
	return rows.Err()
}

func (j *Journal) loadBumpLocks(ctx context.Context, b *Bump) error {
	rows, err := j.db.QueryContext(ctx,
		`SELECT txid, vout, released FROM bump_locks
		  WHERE run_id = ? AND seq = ? ORDER BY txid, vout`, b.RunID, b.Seq)
	if err != nil {
		return fmt.Errorf("reading the coin locks of bump %d in run %s: %w",
			b.Seq, b.RunID, err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			txid     string
			vout     int64
			released int
		)
		if err := rows.Scan(&txid, &vout, &released); err != nil {
			return fmt.Errorf("reading the coin locks of bump %d in run %s: %w",
				b.Seq, b.RunID, err)
		}
		b.Locks = append(b.Locks, Lock{
			Outpoint: bitcoind.Outpoint{TxID: txid, Vout: uint32(vout)},
			Released: released != 0,
		})
	}
	return rows.Err()
}

func affectedOneBump(res sql.Result, runID string, seq int64) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("run %s bump %d: %w", runID, seq, err)
	}
	if n == 0 {
		return fmt.Errorf("run %s bump %d: %w", runID, seq, ErrNoBump)
	}
	return nil
}

// orNone renders an empty string as something an operator can read in a
// sentence, so a missing txid does not print as an empty gap.
func orNone(s string) string {
	if s == "" {
		return "nothing"
	}
	return s
}
