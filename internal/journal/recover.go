package journal

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/AusDavo/winthistle/internal/abort"
	"github.com/AusDavo/winthistle/internal/lnd"
	"github.com/lightningnetwork/lnd/lnrpc"
)

// Run is one batch as the journal holds it.
type Run struct {
	ID    string
	State State
	// TxID is the txid LND pinned, recorded before the first psbt_verify. RawTx
	// is the signed transaction, hex, and stays "" until the signing round has
	// happened — which is after the gate, so an armed run with no RawTx is
	// ordinary rather than broken.
	TxID      string
	RawTx     string
	CreatedAt time.Time
	UpdatedAt time.Time
	Channels  []Channel
	Signers   []Signer
}

// Channel is one member of the batch, with whatever handles exist for it yet.
type Channel struct {
	PendingChanID lnd.PendingChanID
	PeerPubkey    string
	AmountSat     int64
	State         ChannelState

	// Outpoint is zero until chan_pending arrived. It is what an abandon needs;
	// the pending channel id is what a shim cancel needs. Which of the two is
	// available is exactly what State says.
	Outpoint lnd.ChannelPoint
}

// Signer is one cold-storage device and how far it got. A label and a state —
// never a key, a descriptor, or a PSBT.
type Signer struct {
	Label     string
	State     SignerState
	UpdatedAt time.Time
}

// Load reads one run back out of the journal.
func (j *Journal) Load(ctx context.Context, runID string) (*Run, error) {
	var (
		r            Run
		txid, rawTx  sql.NullString
		created, upd string
		state        string
	)
	err := j.db.QueryRowContext(ctx,
		`SELECT id, state, txid, raw_tx, created_at, updated_at FROM runs WHERE id = ?`,
		runID).Scan(&r.ID, &state, &txid, &rawTx, &created, &upd)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("run %s: %w", runID, ErrNoRun)
	}
	if err != nil {
		return nil, fmt.Errorf("reading run %s: %w", runID, err)
	}
	r.State = State(state)
	r.TxID, r.RawTx = txid.String, rawTx.String
	r.CreatedAt, r.UpdatedAt = parseTime(created), parseTime(upd)

	if err := j.loadChannels(ctx, &r); err != nil {
		return nil, err
	}
	if err := j.loadSigners(ctx, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// Unfinished lists the runs the journal shows as neither published nor aborted.
//
// That is the whole of what it establishes, and a caller must not read more into
// it. The journal records what a run wrote, not whether it is still writing: a
// run is in here from the moment Begin records its first stream, so a batch
// being armed in another terminal right now is in this list beside three that
// died in August. This comment used to say that anything in here is a run that
// stopped somewhere it should not have, which is how that claim reached the two
// screens that print it.
//
// The hedge belongs at the callers rather than here, because the narrower
// question cannot be answered honestly: a run that died mid-arming and a run
// being armed right now write identical rows, and the journal carries no
// heartbeat to tell them apart. Both callers make it — prose.RecoveryList and
// doctor's journal check.
//
// It includes runs in StatePublishing, deliberately — those are the ones an
// operator most needs to see, and the ones Run.AbortTarget will refuse to tear
// down.
func (j *Journal) Unfinished(ctx context.Context) ([]*Run, error) {
	rows, err := j.db.QueryContext(ctx,
		`SELECT id FROM runs WHERE state NOT IN (?, ?) ORDER BY created_at`,
		string(StatePublished), string(StateAborted))
	if err != nil {
		return nil, fmt.Errorf("listing unfinished runs: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("listing unfinished runs: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("listing unfinished runs: %w", err)
	}
	rows.Close()

	out := make([]*Run, 0, len(ids))
	for _, id := range ids {
		r, err := j.Load(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// AbortTarget turns a journalled run back into the abort that would undo it.
//
// The split is the one abort.Target already models, and the journal is what
// decides which side each channel falls on: a channel that reached chan_pending
// has to be abandoned by outpoint, one that did not has to be cancelled by
// pending channel id. Channels already abandoned or cancelled are left out, so a
// second recovery reports honestly rather than re-listing work that is done.
//
// It refuses a run that reached the publish call. That transaction may be in a
// mempool or already mined, and abandoning a pending channel whose funding
// transaction then confirms leaves its funds with no force-close path — the
// worst outcome this design has. Such a run needs the operator to look at the
// chain, not an abort.
func (r *Run) AbortTarget() (abort.Target, error) {
	switch r.State {
	case StatePublishing, StatePublished:
		return abort.Target{}, fmt.Errorf(
			"run %s is %s and funds %d channel(s) on %s: %w — check whether %s is "+
				"in the mempool or a block before touching anything",
			r.ID, r.State, len(r.Channels), r.TxID, ErrMayBePublished, r.TxID)
	}

	var t abort.Target
	for _, c := range r.Channels {
		switch c.State {
		case ChanPending:
			if c.Outpoint.TxID == "" {
				return abort.Target{}, fmt.Errorf("run %s: channel %s is journalled "+
					"as pending with no funding outpoint, so it cannot be abandoned",
					r.ID, c.PendingChanID)
			}
			// No shim cancel for these. CompleteReservation deletes the funding
			// intent (handleFundingCounterPartySigs in lnwallet/wallet.go), so
			// by the time chan_pending is emitted there is nothing left to
			// cancel.
			t.Channels = append(t.Channels, c.Outpoint)

		case ChanShimRegistered, ChanVerified:
			// ChanVerified reads differently after the inversion, and the honest
			// version of the difference is narrow. psbt_verify now carries
			// skip_finalize, so it does not park the funding flow — it completes
			// it, and the channel's own chan_pending follows on its own. So a
			// channel that verified is one whose channel LND has probably
			// already created.
			//
			// Cancelling is still the answer, and this is still right, because
			// arm.Receipts asks PendingChannels whenever a receipt does not
			// arrive and journals what it finds. A row left saying verified is
			// one LND did not list as a pending open when it was asked.
			//
			// The exception is a *crashed* process, which died between the
			// psbt_verify and the receipt and never got to ask. Then the shim
			// cancel reports AlreadyGone — a success — for a channel that is
			// actually pending, and the peer keeps its side until clock B runs
			// out. That window is the same one the pre-inversion sequence had
			// between psbt_finalize and its receipt, unchanged in kind and in
			// size, and closing it needs a lookup this function cannot make: it
			// has no client, and the journal cannot map a pending channel point
			// back to a pending channel id.
			t.Shims = append(t.Shims, c.PendingChanID)
		}
	}
	return t, nil
}

// Recover is the entry point that turns a crashed run back into an executed
// abort: read the row, build the target, run it, and write down what happened.
//
// The move to StateAborting is written before the first RPC, for the same reason
// StatePublishing is: an abort that is itself interrupted has to be visible as
// one. And the outcome is recorded even when the abort failed, so a second
// Recover over the same run picks up only what is genuinely left.
//
// Safe to call twice. Everything underneath it is.
func (j *Journal) Recover(ctx context.Context, cli lnrpc.LightningClient,
	runID string, confirm abort.Confirmation) (*abort.Report, error) {

	r, err := j.Load(ctx, runID)
	if err != nil {
		return nil, err
	}
	target, err := r.AbortTarget()
	if err != nil {
		return nil, err
	}

	if r.State != StateAborting {
		if err := j.tx(ctx, func(tx *sql.Tx) error {
			return j.setState(ctx, tx, runID, StateAborting)
		}); err != nil {
			return nil, err
		}
	}

	rep, runErr := abort.Run(ctx, cli, target, confirm)

	// Recorded whether or not the abort succeeded: what did work must not have
	// to be discovered again.
	recErr := j.recordAbort(ctx, runID, rep)

	if runErr == nil && recErr == nil && rep.Clean() {
		if err := j.tx(ctx, func(tx *sql.Tx) error {
			return j.setState(ctx, tx, runID, StateAborted)
		}); err != nil {
			return rep, err
		}
	}
	return rep, errors.Join(runErr, recErr)
}

// recordAbort writes an abort.Report back into the journal.
func (j *Journal) recordAbort(ctx context.Context, runID string, rep *abort.Report) error {
	if rep == nil {
		return nil
	}
	return j.tx(ctx, func(tx *sql.Tx) error {
		for _, a := range rep.Abandoned {
			_, err := tx.ExecContext(ctx,
				`UPDATE channels SET state = ?
				 WHERE run_id = ? AND funding_txid = ? AND funding_index = ?`,
				string(ChanAbandoned), runID, a.Channel.TxID, a.Channel.Index)
			if err != nil {
				return fmt.Errorf("recording the abandon of %s in run %s: %w",
					a.Channel, runID, err)
			}
		}
		for _, c := range rep.Cancelled {
			if err := j.setChannelState(ctx, tx, runID, c.ID, ChanCancelled); err != nil {
				return err
			}
		}
		return j.touch(ctx, tx, runID)
	})
}

func (j *Journal) loadChannels(ctx context.Context, r *Run) error {
	rows, err := j.db.QueryContext(ctx,
		`SELECT pending_chan_id, peer_pubkey, amount_sat, state, funding_txid, funding_index
		   FROM channels WHERE run_id = ? ORDER BY pending_chan_id`, r.ID)
	if err != nil {
		return fmt.Errorf("reading the channels of run %s: %w", r.ID, err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			idHex, pubkey, state string
			amount               int64
			txid                 sql.NullString
			index                sql.NullInt64
		)
		if err := rows.Scan(&idHex, &pubkey, &amount, &state, &txid, &index); err != nil {
			return fmt.Errorf("reading the channels of run %s: %w", r.ID, err)
		}
		id, err := lnd.ParsePendingChanID(idHex)
		if err != nil {
			return fmt.Errorf("run %s: %w", r.ID, err)
		}
		c := Channel{
			PendingChanID: id,
			PeerPubkey:    pubkey,
			AmountSat:     amount,
			State:         ChannelState(state),
		}
		if txid.Valid && txid.String != "" {
			c.Outpoint = lnd.ChannelPoint{TxID: txid.String, Index: uint32(index.Int64)}
		}
		r.Channels = append(r.Channels, c)
	}
	return rows.Err()
}

func (j *Journal) loadSigners(ctx context.Context, r *Run) error {
	rows, err := j.db.QueryContext(ctx,
		`SELECT label, state, updated_at FROM signers WHERE run_id = ? ORDER BY label`, r.ID)
	if err != nil {
		return fmt.Errorf("reading the signers of run %s: %w", r.ID, err)
	}
	defer rows.Close()

	for rows.Next() {
		var label, state, upd string
		if err := rows.Scan(&label, &state, &upd); err != nil {
			return fmt.Errorf("reading the signers of run %s: %w", r.ID, err)
		}
		r.Signers = append(r.Signers, Signer{
			Label:     label,
			State:     SignerState(state),
			UpdatedAt: parseTime(upd),
		})
	}
	return rows.Err()
}
