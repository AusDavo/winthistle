package abort

import (
	"context"
	"errors"
	"fmt"

	"github.com/AusDavo/winthistle/internal/lnd"
	"github.com/lightningnetwork/lnd/lnrpc"
)

// Target is what a run left behind, as recorded in the journal.
//
// Both lists are independent. A batch can have channels that reached
// chan_pending and streams that never got that far, at once — which is precisely
// the state a mid-window failure leaves.
//
// There was a third list: the coins this run had locked in Core, released here
// last because they were ours alone and nothing depended on them. The app
// selects no coins and dials no Bitcoin node, so nothing takes a lock and there
// is nothing to release. A lock a build before the inversion took is Core's to
// forget, and it does: locks live in memory and a restart clears them.
type Target struct {
	Channels []lnd.ChannelPoint  // reached chan_pending
	Shims    []lnd.PendingChanID // streams that never finalized
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
	Abandoned []AbandonOutcome
	Cancelled []ShimOutcome
	Failures  []error
}

// Clean reports whether nothing was left behind.
func (r *Report) Clean() bool { return len(r.Failures) == 0 }

// Run takes a batch apart: abandon the channels that reached pending, then
// cancel the shims that did not.
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
// second, because cancelling one releases LND's own coin locks.
//
// Safe to call twice. An already-cancelled shim reports AlreadyGone rather than
// failing, and an already-abandoned channel is not an error in LND.
func Run(ctx context.Context, cli lnrpc.LightningClient, t Target,
	confirm Confirmation) (*Report, error) {

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

	if len(rep.Failures) > 0 {
		return rep, fmt.Errorf("abort did not fully complete: %w", errors.Join(rep.Failures...))
	}
	return rep, nil
}
