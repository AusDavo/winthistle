package journal

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/AusDavo/winthistle/internal/bitcoind"
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

// RecordLocks notes the inputs Core is holding unspendable for this run.
//
// Written after walletcreatefundedpsbt returns, which is the earliest we know
// them — and the reason bitcoind.ListLocks is exported: a crash in between
// leaves locks with no owner, and only Core can then say what is held.
func (j *Journal) RecordLocks(ctx context.Context, runID string, ops []bitcoind.Outpoint) error {
	if err := j.mustExist(ctx, runID); err != nil {
		return err
	}
	return j.tx(ctx, func(tx *sql.Tx) error {
		for _, op := range ops {
			_, err := tx.ExecContext(ctx,
				`INSERT INTO locks (run_id, txid, vout) VALUES (?, ?, ?)
				 ON CONFLICT (run_id, txid, vout) DO NOTHING`,
				runID, op.TxID, op.Vout)
			if err != nil {
				return fmt.Errorf("recording coin lock %s of run %s: %w", op, runID, err)
			}
		}
		return j.touch(ctx, tx, runID)
	})
}

// MarkVerified records that psbt_verify succeeded for one channel. From here
// LND has committed to that channel's funding outpoint and will accept only
// added signatures (I-3).
//
// The run moves to signing once every channel has verified, because that is the
// point at which the transaction can go to the signers.
func (j *Journal) MarkVerified(ctx context.Context, runID string, id lnd.PendingChanID) error {
	return j.tx(ctx, func(tx *sql.Tx) error {
		if err := j.setChannelState(ctx, tx, runID, id, ChanVerified); err != nil {
			return err
		}
		unverified, err := j.countChannelsNotIn(ctx, tx, runID, ChanVerified)
		if err != nil {
			return err
		}
		if unverified == 0 {
			return j.setState(ctx, tx, runID, StateSigning)
		}
		return j.touch(ctx, tx, runID)
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

// RecordFinalizedTx stores the combined, finalized transaction.
//
// This is written *before* the first psbt_finalize, not after the last one. From
// the moment LND is handed this transaction the peers start storing commitment
// signatures against its outpoints, and if we then lose the transaction we own a
// rebroadcast obligation (I-1 leaves rebroadcast to us) that we cannot meet.
func (j *Journal) RecordFinalizedTx(ctx context.Context, runID, txid, rawTxHex string) error {
	if txid == "" || rawTxHex == "" {
		return fmt.Errorf("run %s: the finalized transaction needs both a txid and its bytes", runID)
	}
	return j.tx(ctx, func(tx *sql.Tx) error {
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

// MarkPublishing is the write that must land before WalletKit.PublishTransaction
// is called, and the reason this package exists.
//
// It refuses unless the run is armed, so the I-1 gate is enforced by the
// component that actually knows whether every chan_pending arrived rather than
// by the loop that happens to be calling. A run found in this state afterwards is
// a run whose transaction may be public — see Run.AbortTarget, which will not
// abort one.
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
		if State(state) != StateArmed {
			return fmt.Errorf("run %s is %s, not %s: %w",
				runID, state, StateArmed, ErrNotArmed)
		}
		if !rawTx.Valid || rawTx.String == "" {
			return fmt.Errorf("run %s is armed but no finalized transaction was "+
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
