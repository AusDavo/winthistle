package journal

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/AusDavo/winthistle/internal/lnd"
)

// NewChannel is one member of a batch at the moment its funding stream opens.
//
// The pending channel id comes first because it is the only handle that exists
// this early: the funding outpoint is not known until psbt_verify, and the
// channel point not until chan_pending.
type NewChannel struct {
	PendingChanID lnd.PendingChanID
	PeerPubkey    string
	AmountSat     int64
}

// Begin records a new run and one row per funding stream in it.
//
// Called after the streams are open and before anything is shown to a Core
// wallet, so that the pending channel ids — the only handles that can cancel
// those streams — are on disk before a crash could orphan them.
func (j *Journal) Begin(ctx context.Context, runID string, chans []NewChannel) error {
	if runID == "" {
		return errors.New("a run needs an id")
	}
	if len(chans) == 0 {
		return fmt.Errorf("run %s: a batch with no channels in it is not a batch", runID)
	}

	return j.tx(ctx, func(tx *sql.Tx) error {
		stamp := now()
		_, err := tx.ExecContext(ctx,
			`INSERT INTO runs (id, state, created_at, updated_at) VALUES (?, ?, ?, ?)`,
			runID, string(StateArming), stamp, stamp)
		if err != nil {
			return fmt.Errorf("recording run %s: %w", runID, err)
		}
		for _, c := range chans {
			_, err := tx.ExecContext(ctx,
				`INSERT INTO channels
				   (run_id, pending_chan_id, peer_pubkey, amount_sat, state)
				 VALUES (?, ?, ?, ?, ?)`,
				runID, c.PendingChanID.String(), c.PeerPubkey, c.AmountSat,
				string(ChanShimRegistered))
			if err != nil {
				return fmt.Errorf("recording channel %s of run %s: %w",
					c.PendingChanID, runID, err)
			}
		}
		return nil
	})
}

// MarkVerified records that psbt_verify succeeded for one channel. From here
// LND has committed to that channel's funding outpoint and will accept only
// added signatures (I-3).
//
// The run does not move. It used to go to signing here, because psbt_verify was
// the last thing that happened before the PSBT went out to the signers; after
// the inversion the next thing is the channel's own chan_pending, arriving over
// the unsigned transaction, and nothing is asked of a wallet until every one of
// them is in. So a run between the first verify and the last receipt stays in
// StateArming — the gate is not open — and the channel rows are what say how far
// each member got. That is the split abort.Target needs and the only one it
// reads.
func (j *Journal) MarkVerified(ctx context.Context, runID string, id lnd.PendingChanID) error {
	return j.tx(ctx, func(tx *sql.Tx) error {
		if err := j.setChannelState(ctx, tx, runID, id, ChanVerified); err != nil {
			return err
		}
		return j.touch(ctx, tx, runID)
	})
}

// RecordPinnedTxID stores the txid LND is about to commit to, before the first
// psbt_verify of the batch.
//
// Before, not after, and that is the same discipline MarkPublishing follows for
// the same reason. A psbt_verify with skip_finalize does not pause the funding
// flow, it completes it: LND takes the unsigned transaction's outpoints as final
// and goes on to exchange funding_created and funding_signed with the peer. So
// from the first of these calls onwards a peer may be storing a commitment
// signature against an outpoint of this transaction, and a run that lost the
// txid would have no handle on what it had committed to.
//
// It refuses to move a txid that is already recorded. The pin is what I-3 is
// checked against, and a run whose pin could be rewritten has no pin.
func (j *Journal) RecordPinnedTxID(ctx context.Context, runID, txid string) error {
	if txid == "" {
		return fmt.Errorf("run %s: a pinned txid cannot be empty", runID)
	}
	return j.tx(ctx, func(tx *sql.Tx) error {
		var had sql.NullString
		err := tx.QueryRowContext(ctx,
			`SELECT txid FROM runs WHERE id = ?`, runID).Scan(&had)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("run %s: %w", runID, ErrNoRun)
		}
		if err != nil {
			return fmt.Errorf("reading run %s: %w", runID, err)
		}
		if had.Valid && had.String != "" && had.String != txid {
			return fmt.Errorf("run %s is pinned to %s and this is %s: %w",
				runID, had.String, txid, ErrTxIDMoved)
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE runs SET txid = ?, updated_at = ? WHERE id = ?`, txid, now(), runID)
		if err != nil {
			return fmt.Errorf("pinning the txid of run %s: %w", runID, err)
		}
		return affectedOne(res, runID)
	})
}

// RecordSigner notes how far one cold-storage signer has got. The label is
// whatever the operator calls the device; no key material is stored, here or
// anywhere else in the journal.
func (j *Journal) RecordSigner(ctx context.Context, runID, label string, st SignerState) error {
	if label == "" {
		return fmt.Errorf("run %s: a signer needs a label", runID)
	}
	if err := j.mustExist(ctx, runID); err != nil {
		return err
	}
	return j.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO signers (run_id, label, state, updated_at) VALUES (?, ?, ?, ?)
			 ON CONFLICT (run_id, label) DO UPDATE SET state = excluded.state,
			                                           updated_at = excluded.updated_at`,
			runID, label, string(st), now())
		if err != nil {
			return fmt.Errorf("recording signer %q of run %s: %w", label, runID, err)
		}
		return j.touch(ctx, tx, runID)
	})
}

// RecordFinalizedTx stores the signed transaction.
//
// This is written *before* the publish call, and it is the first moment the bytes
// exist: after the inversion nothing signs anything until every channel has
// already reached chan_pending. We own rebroadcast — no_publish sets
// NoFundingTxBit, which also gates rebroadcastFundingTx — so losing the bytes
// after they can be in a mempool is a rebroadcast obligation we could not meet.
//
// It refuses a transaction whose txid is not the run's pinned one. That is I-3,
// enforced by the component that holds the pin rather than only at the call site:
// LND committed to the funding outpoints at psbt_verify, so different bytes fund
// nothing, and a journal that accepted them would be recording a rebroadcast
// duty for a transaction no channel in the run depends on.
func (j *Journal) RecordFinalizedTx(ctx context.Context, runID, txid, rawTxHex string) error {
	if txid == "" || rawTxHex == "" {
		return fmt.Errorf("run %s: the finalized transaction needs both a txid and its bytes", runID)
	}
	return j.tx(ctx, func(tx *sql.Tx) error {
		var had sql.NullString
		err := tx.QueryRowContext(ctx,
			`SELECT txid FROM runs WHERE id = ?`, runID).Scan(&had)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("run %s: %w", runID, ErrNoRun)
		}
		if err != nil {
			return fmt.Errorf("reading run %s: %w", runID, err)
		}
		if had.Valid && had.String != "" && had.String != txid {
			return fmt.Errorf("run %s pinned %s at psbt_verify and these bytes are "+
				"%s: %w", runID, had.String, txid, ErrTxIDMoved)
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE runs SET txid = ?, raw_tx = ?, updated_at = ? WHERE id = ?`,
			txid, rawTxHex, now(), runID)
		if err != nil {
			return fmt.Errorf("recording the finalized tx of run %s: %w", runID, err)
		}
		return affectedOne(res, runID)
	})
}

// MarkPending records one chan_pending receipt: the channel is force-closeable,
// and its funding outpoint is now known.
//
// When the last channel in the batch arrives here the run becomes armed, and the
// journal decides that for itself by counting rows. That count is I-1: the gate
// opens when *every* channel is recoverable, and no caller gets to assert it.
func (j *Journal) MarkPending(ctx context.Context, runID string,
	id lnd.PendingChanID, cp lnd.ChannelPoint) error {

	if cp.TxID == "" {
		return fmt.Errorf("run %s: channel %s reached pending with no funding outpoint", runID, id)
	}
	return j.tx(ctx, func(tx *sql.Tx) error {
		// What is already on disk for this channel, before writing over it.
		//
		// A repeat of the same receipt is harmless and stays harmless: the write
		// is idempotent, and arming counts channels in the pending state rather
		// than receipts, so n receipts for one channel can never stand in for
		// n channels.
		//
		// A receipt naming a *different* outpoint is not a repeat. The
		// journalled outpoint is what an abort abandons, so accepting the second
		// one would silently point the teardown at a channel this run may not
		// own — and abandoning the wrong pending channel is the worst thing in
		// this package's reach. There is also no legitimate route here: LND
		// commits to the funding outpoint at psbt_verify, and only signatures
		// may be added afterwards (I-3).
		var was string
		var hadTxID sql.NullString
		var hadIndex sql.NullInt64
		err := tx.QueryRowContext(ctx,
			`SELECT state, funding_txid, funding_index FROM channels
			 WHERE run_id = ? AND pending_chan_id = ?`,
			runID, id.String()).Scan(&was, &hadTxID, &hadIndex)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("run %s has no channel %s: %w", runID, id, ErrNoRun)
		}
		if err != nil {
			return fmt.Errorf("reading channel %s of run %s: %w", id, runID, err)
		}
		if hadTxID.Valid && hadTxID.String != "" {
			had := lnd.ChannelPoint{
				TxID: hadTxID.String, Index: uint32(hadIndex.Int64),
			}
			if had != cp {
				return fmt.Errorf("run %s, channel %s: already journalled at %s "+
					"and this receipt says %s. That outpoint is what an abort "+
					"abandons, so it is not overwritten: %w",
					runID, id, had, cp, ErrOutpointMoved)
			}
		}

		res, err := tx.ExecContext(ctx,
			`UPDATE channels SET state = ?, funding_txid = ?, funding_index = ?
			 WHERE run_id = ? AND pending_chan_id = ?`,
			string(ChanPending), cp.TxID, cp.Index, runID, id.String())
		if err != nil {
			return fmt.Errorf("recording chan_pending for %s of run %s: %w", id, runID, err)
		}
		if err := affectedOneChannel(res, runID, id); err != nil {
			return err
		}

		notPending, err := j.countChannelsNotIn(ctx, tx, runID, ChanPending)
		if err != nil {
			return err
		}
		if notPending == 0 {
			return j.setState(ctx, tx, runID, StateArmed)
		}
		return j.touch(ctx, tx, runID)
	})
}

// MarkSigning records that the unsigned transaction has gone out to be signed.
//
// It comes after the gate, and it used to come before. It refuses a run that is
// not armed, which is what makes the state mean something: a run in StateSigning
// has every one of its channels at chan_pending, so the abort it needs is n
// abandons rather than n free shim cancels, and the recovery screen must say so.
// Nothing derives that from the state — abort.Target reads the channel rows — but
// the copy does, and a state that could be reached without the receipts would
// make the copy a lie.
//
// Idempotent: a run already in StateSigning stays there.
func (j *Journal) MarkSigning(ctx context.Context, runID string) error {
	return j.tx(ctx, func(tx *sql.Tx) error {
		var state string
		err := tx.QueryRowContext(ctx,
			`SELECT state FROM runs WHERE id = ?`, runID).Scan(&state)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("run %s: %w", runID, ErrNoRun)
		}
		if err != nil {
			return fmt.Errorf("reading run %s: %w", runID, err)
		}
		if State(state) == StateSigning {
			return j.touch(ctx, tx, runID)
		}
		if State(state) != StateArmed {
			return fmt.Errorf("run %s is %s, and a batch does not go out to be "+
				"signed until every channel in it is already recoverable: %w",
				runID, state, ErrNotArmed)
		}
		return j.setState(ctx, tx, runID, StateSigning)
	})
}

// MarkPublishing is the write that must land before WalletKit.PublishTransaction
// is called, and the reason this package exists.
//
// It refuses unless every channel in the run is at chan_pending, so the I-1 gate
// is enforced by the component that actually knows whether every receipt arrived
// rather than by the loop that happens to be calling. A run found in this state
// afterwards is a run whose transaction may be public — see Run.AbortTarget,
// which will not abort one.
//
// The gate is a count, not a state transition, and that is deliberate. Before the
// inversion "the run is armed" was itself the proof, because armed was the state
// immediately before publishing and only MarkPending could write it. Now
// StateSigning sits in between, so trusting the state would be trusting that
// nothing else can ever reach it — a property of the whole package rather than of
// this function. Counting the rows here costs one query and depends on nothing.
//
// The state is still checked, for the other thing it settles: a run already in
// publishing, published, aborting or aborted is refused, which is what stops a
// second publish of a batch that may already be out and an abort's leftovers from
// being broadcast.
func (j *Journal) MarkPublishing(ctx context.Context, runID string) error {
	return j.tx(ctx, func(tx *sql.Tx) error {
		var state string
		var rawTx sql.NullString
		err := tx.QueryRowContext(ctx,
			`SELECT state, raw_tx FROM runs WHERE id = ?`, runID).Scan(&state, &rawTx)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("run %s: %w", runID, ErrNoRun)
		}
		if err != nil {
			return fmt.Errorf("reading run %s: %w", runID, err)
		}
		switch State(state) {
		case StateArmed, StateSigning:
		default:
			return fmt.Errorf("run %s is %s, and a publish is only made from %s or "+
				"%s: %w", runID, state, StateArmed, StateSigning, ErrNotArmed)
		}
		notPending, err := j.countChannelsNotIn(ctx, tx, runID, ChanPending)
		if err != nil {
			return err
		}
		if notPending != 0 {
			return fmt.Errorf("run %s has %d channel(s) that never reached "+
				"chan_pending: %w", runID, notPending, ErrNotArmed)
		}
		if !rawTx.Valid || rawTx.String == "" {
			return fmt.Errorf("run %s is armed but no signed transaction was "+
				"journalled — there would be nothing to rebroadcast: %w", runID, ErrNotArmed)
		}
		return j.setState(ctx, tx, runID, StatePublishing)
	})
}

// MarkPublished records that the publish call returned without error.
func (j *Journal) MarkPublished(ctx context.Context, runID string) error {
	return j.tx(ctx, func(tx *sql.Tx) error {
		return j.setState(ctx, tx, runID, StatePublished)
	})
}

// ---- helpers ----

// tx runs fn in a transaction, rolling back on error. Every write in this
// package goes through it, so a partially-applied journal entry is not a state
// the recovery path has to be able to read.
func (j *Journal) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning a journal transaction: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing to the journal: %w", err)
	}
	return nil
}

func (j *Journal) setState(ctx context.Context, tx *sql.Tx, runID string, st State) error {
	res, err := tx.ExecContext(ctx,
		`UPDATE runs SET state = ?, updated_at = ? WHERE id = ?`, string(st), now(), runID)
	if err != nil {
		return fmt.Errorf("moving run %s to %s: %w", runID, st, err)
	}
	return affectedOne(res, runID)
}

func (j *Journal) touch(ctx context.Context, tx *sql.Tx, runID string) error {
	res, err := tx.ExecContext(ctx,
		`UPDATE runs SET updated_at = ? WHERE id = ?`, now(), runID)
	if err != nil {
		return fmt.Errorf("updating run %s: %w", runID, err)
	}
	return affectedOne(res, runID)
}

// channelStateIn reads one channel's current state inside a transaction, so a
// write can be guarded by what the journal has already established rather than
// simply overwriting it. MarkPending's outpoint guard is the precedent.
func (j *Journal) channelStateIn(ctx context.Context, tx *sql.Tx, runID string,
	id lnd.PendingChanID) (ChannelState, error) {

	var st string
	err := tx.QueryRowContext(ctx,
		`SELECT state FROM channels WHERE run_id = ? AND pending_chan_id = ?`,
		runID, id.String()).Scan(&st)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("run %s has no channel %s: %w", runID, id, ErrNoRun)
	}
	if err != nil {
		return "", fmt.Errorf("reading channel %s of run %s: %w", id, runID, err)
	}
	return ChannelState(st), nil
}

func (j *Journal) setChannelState(ctx context.Context, tx *sql.Tx, runID string,
	id lnd.PendingChanID, st ChannelState) error {

	res, err := tx.ExecContext(ctx,
		`UPDATE channels SET state = ? WHERE run_id = ? AND pending_chan_id = ?`,
		string(st), runID, id.String())
	if err != nil {
		return fmt.Errorf("moving channel %s of run %s to %s: %w", id, runID, st, err)
	}
	return affectedOneChannel(res, runID, id)
}

// countChannelsNotIn is how the gate is counted: how many of this run's channels
// have not yet reached the given state.
func (j *Journal) countChannelsNotIn(ctx context.Context, tx *sql.Tx, runID string,
	st ChannelState) (int, error) {

	var n int
	err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM channels WHERE run_id = ? AND state != ?`,
		runID, string(st)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("counting the channels of run %s: %w", runID, err)
	}
	return n, nil
}

func (j *Journal) mustExist(ctx context.Context, runID string) error {
	var one int
	err := j.db.QueryRowContext(ctx, `SELECT 1 FROM runs WHERE id = ?`, runID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("run %s: %w", runID, ErrNoRun)
	}
	if err != nil {
		return fmt.Errorf("reading run %s: %w", runID, err)
	}
	return nil
}

// affectedOne turns "the UPDATE matched nothing" into ErrNoRun. A silent no-op
// here would mean the journal disagreed with the RPC that just succeeded, which
// is the one failure mode a journal must not have.
func affectedOne(res sql.Result, runID string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("run %s: %w", runID, err)
	}
	if n == 0 {
		return fmt.Errorf("run %s: %w", runID, ErrNoRun)
	}
	return nil
}

func affectedOneChannel(res sql.Result, runID string, id lnd.PendingChanID) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("run %s, channel %s: %w", runID, id, err)
	}
	if n == 0 {
		return fmt.Errorf("run %s has no channel %s: %w", runID, id, ErrNoRun)
	}
	return nil
}
