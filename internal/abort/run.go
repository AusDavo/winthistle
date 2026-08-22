package abort

import (
	"context"
	"errors"
	"fmt"

	"github.com/AusDavo/winthistle/internal/bitcoind"
	"github.com/AusDavo/winthistle/internal/lnd"
	"github.com/lightningnetwork/lnd/lnrpc"
)

// LockReleaser is the slice of Core the abort needs. Narrow on purpose: an abort
// must not be able to spend, sign or broadcast anything, and the type says so.
type LockReleaser interface {
	// ReleaseLocks returns the outpoints it actually freed, which is not
	// necessarily the ones it was asked about — see bitcoind.ReleaseLocks.
	ReleaseLocks(ctx context.Context, ops []bitcoind.Outpoint) ([]bitcoind.Outpoint, error)
}

// Target is what a run left behind, as recorded in the journal.
//
// All three lists are independent. A batch can have channels that reached
// chan_pending, streams that never got that far, and locked coins, all at once —
// which is precisely the state a mid-window failure leaves.
type Target struct {
	Channels []lnd.ChannelPoint  // reached chan_pending
	Shims    []lnd.PendingChanID // streams that never finalized
	Locks    []bitcoind.Outpoint // inputs locked in Core for this run
}

// ShimOutcome records one shim cancellation. AlreadyGone separates "we cleaned
// it up" from "there was nothing to clean up", which matters when reading a
// journal after the fact.
type ShimOutcome struct {
	ID          lnd.PendingChanID
	AlreadyGone bool
}

// Report is the whole abort, item by item. Written to the journal and shown on
// the recovery screen; the screen's job is to say what happened and what is
// left, so partial success has to be representable.
type Report struct {
	Abandoned  []AbandonOutcome
	Cancelled  []ShimOutcome
	LocksFreed []bitcoind.Outpoint
	Failures   []error
}

// Clean reports whether nothing was left behind.
func (r *Report) Clean() bool { return len(r.Failures) == 0 }

// Run takes a batch apart: abandon the channels that reached pending, cancel the
// shims that did not, then release Core's coin locks.
//
// Nothing here stops on the first failure. An abort runs when something has
// already gone wrong, and the alternative — returning early — is what leaves an
// operator with a wallet that silently refuses to spend its own coins because a
// lock release was queued behind an abandon that failed. Every step is
// attempted, every failure is collected, and the joined error is returned once
// at the end alongside a Report that says exactly which parts did work.
//
// The order is deliberate and not merely tidy. Channels first, because a pending
// channel is the only item here that LND will otherwise keep watching; shims
// second, because cancelling one releases LND's own coin locks; Core's locks
// last, since they are ours alone and nothing depends on them.
//
// Safe to call twice. An already-cancelled shim reports AlreadyGone rather than
// failing, and an already-abandoned channel is not an error in LND.
func Run(ctx context.Context, cli lnrpc.LightningClient, core LockReleaser,
	t Target, confirm Confirmation) (*Report, error) {

	rep := &Report{}

	for _, cp := range t.Channels {
		outcome, err := AbandonPending(ctx, cli, cp, confirm)
		if err != nil {
			rep.Failures = append(rep.Failures, err)
			continue
		}
		rep.Abandoned = append(rep.Abandoned, outcome)
	}

	for _, id := range t.Shims {
		err := CancelShim(ctx, cli, id)
		switch {
		case err == nil:
			rep.Cancelled = append(rep.Cancelled, ShimOutcome{ID: id})
		case errors.Is(err, ErrNoShim):
			rep.Cancelled = append(rep.Cancelled, ShimOutcome{ID: id, AlreadyGone: true})
		default:
			rep.Failures = append(rep.Failures, err)
		}
	}

	// Guarded rather than passed straight through: ReleaseLocks refuses an empty
	// list, because Core reads that as unlock-everything. A run that locked
	// nothing is normal, and must not turn into a wallet-wide unlock here.
	if len(t.Locks) > 0 {
		freed, err := core.ReleaseLocks(ctx, t.Locks)
		if err != nil {
			rep.Failures = append(rep.Failures,
				fmt.Errorf("releasing %d coin lock(s): %w", len(t.Locks), err))
		} else {
			// What Core actually freed, not what we asked about. A second abort
			// over the same journal entry legitimately frees nothing.
			rep.LocksFreed = freed
		}
	}

	if len(rep.Failures) > 0 {
		return rep, fmt.Errorf("abort did not fully complete: %w", errors.Join(rep.Failures...))
	}
	return rep, nil
}
