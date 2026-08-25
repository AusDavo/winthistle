package abort

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/AusDavo/winthistle/internal/lnd"
	"github.com/lightningnetwork/lnd/lnrpc"
)

// CallBudget is how long each LND call in a teardown gets.
//
// It bounds the calls and deliberately does not bound the operator. This used to
// be one deadline around the whole teardown — run.TeardownBudget, five minutes —
// sized on the reasoning that the slow part of an abort is not the RPCs but the
// blunt confirmation, which asks a human once per channel. That reasoning was
// right about where the time goes and wrong about what to do with it, because
// the confirmation sits *between* two of these calls:
//
//	AbandonChannel(safe flag)  →  refused
//	PendingChannels            →  is it really pending?
//	confirm(...)               →  the human
//	AbandonChannel(blunt flag) →  the call that does the work
//
// An operator who took longer than the budget over that middle step got the last
// call refused with a deadline **after they had already answered yes** — the one
// outcome that leaves somebody believing they authorised something that did not
// happen. Deliberation is not a hang.
//
// So the clock is here, on each call, and there is no clock on the human at all.
// A teardown still cannot hang against an unresponsive node, which is what the
// old budget was really protecting; and winthistle recover, which never had the
// old budget, is bounded now for the first time.
const CallBudget = 30 * time.Second

// call bounds one LND call. Cancellation still comes from the parent, which is
// what lets `winthistle recover` be interrupted and what recoverRun deliberately
// severs with context.WithoutCancel.
func call(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, CallBudget)
}

// callAfterConsent bounds the one LND call that happens *after* a human has
// authorised it, and it takes neither the parent's deadline nor the parent's
// cancellation.
//
// Both omissions are the same argument. Once the operator has answered yes to
// i_know_what_i_am_doing, the worst available outcome is not finishing the call
// — it is leaving them believing they authorised something that did not happen.
// A deadline inherited from upstream has been running through their
// deliberation and may already be spent, which is precisely the defect that
// retired run.TeardownBudget; and a Ctrl-C landing between the answer and the
// RPC would produce the same ambiguity by another route.
//
// It is still bounded, by a fresh CallBudget, so this cannot hang. Anything a
// caller wants to say about giving up has to be said *before* the confirmation,
// where the human has not committed to anything yet.
func callAfterConsent(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), CallBudget)
}

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
