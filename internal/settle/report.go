package settle

import (
	"fmt"
	"strings"
	"time"

	"github.com/AusDavo/winthistle/internal/prose"
	"github.com/lightningnetwork/lnd/lnrpc"
)

// Horizon thresholds, in blocks remaining before the responder gives up.
//
// Far apart on purpose. The horizon is 2016 blocks — about two weeks — and a
// batch that is anywhere near it has been stuck for so long that the operator
// needs telling long before it is urgent. Below Urgent there is roughly a day
// left, which is still time to build and confirm a CPFP child.
const (
	HorizonWarn   = 432 // ~3 days
	HorizonUrgent = 144 // ~1 day
)

// Summary is the one line a status pane wants.
func (r *Result) Summary() string {
	if r == nil || len(r.States) == 0 {
		return "nothing to settle"
	}
	var open, policied int
	for _, s := range r.States {
		if s.Open {
			open++
		}
		if s.Policy.Applied {
			policied++
		}
	}
	line := fmt.Sprintf("%d of %d open, %d policied", open, len(r.States), policied)
	if n, ok := r.NearestExpiry(); ok {
		line += fmt.Sprintf("; %d blocks before the first peer gives up", n)
	}
	return line
}

// Report is the operator-facing text for the settlement pass.
func (r *Result) Report() string {
	if r == nil || len(r.States) == 0 {
		return prose.Para("Nothing to settle.")
	}

	var b strings.Builder

	b.WriteString(fmt.Sprintf("Settlement, %s in.\n\n", r.Elapsed.Round(time.Second)))
	for _, s := range r.States {
		b.WriteString(s.line())
	}

	b.WriteString("\n")
	if r.Done() {
		b.WriteString(prose.Para(
			"Every channel is open and carrying its intended policy. The batch is " +
				"finished."))
		b.WriteString(depthLesson(r))
		return b.String()
	}

	b.WriteString(policyNote(r))
	b.WriteString(depthNote(r))
	b.WriteString(horizonNote(r))
	return b.String()
}

// line is one channel's row.
func (s State) line() string {
	var b strings.Builder

	where := "pending"
	switch {
	case s.Open && s.Active:
		where = "open, active"
	case s.Open:
		where = "open, peer offline"
	}
	b.WriteString(fmt.Sprintf("  %-18s %-20s %s\n", short(s.Member.Peer), where,
		s.Policy))

	detail := []string{}
	if s.Confs >= 0 {
		detail = append(detail, fmt.Sprintf("%d conf", s.Confs))
	}
	if s.ObservedDepth > 0 {
		detail = append(detail, fmt.Sprintf("opened at %d, predicted %d",
			s.ObservedDepth, s.ExpectedDepth))
	} else if !s.Open {
		detail = append(detail, fmt.Sprintf("expect ~%d", s.ExpectedDepth))
	}
	if s.StillPending {
		detail = append(detail, fmt.Sprintf("%d blocks to the horizon", s.ExpiryBlocks))
	}
	if s.Attempts > 1 {
		detail = append(detail, fmt.Sprintf("%d policy attempts", s.Attempts))
	}
	if len(detail) > 0 {
		b.WriteString(fmt.Sprintf("  %-18s %s\n", "", strings.Join(detail, ", ")))
	}
	return b.String()
}

// policyNote explains why a channel is still on LND's defaults.
func policyNote(r *Result) string {
	var pending, missing, stuck int
	for _, s := range r.States {
		switch {
		case s.Policy.Applied:
		case s.Stuck():
			stuck++
		case s.Policy.Reason == lnrpc.UpdateFailure_UPDATE_FAILURE_PENDING:
			pending++
		case s.Policy.Reason == lnrpc.UpdateFailure_UPDATE_FAILURE_NOT_FOUND:
			missing++
		}
	}
	if pending+missing+stuck == 0 {
		return ""
	}

	var b strings.Builder
	if stuck > 0 {
		b.WriteString(prose.Para(fmt.Sprintf(
			"%d channel%s refused the policy for a reason that polling will not "+
				"fix. That is the policy itself, not the channel: LND checks the "+
				"CLTV delta and the inbound fees against its own bounds before it "+
				"looks at anything else.", stuck, prose.Plural(stuck))))
		b.WriteString("\n")
	}
	if pending > 0 {
		b.WriteString(prose.Para(fmt.Sprintf(
			"%d channel%s still pending, so the policy call is refused with "+
				"\"not yet confirmed\". Worth knowing how that refusal arrives: "+
				"UpdateChannelPolicy returns a nil error and puts the failure in "+
				"the response's failed_updates list, so it looks like a success to "+
				"anything that only reads the error. Every attempt below was "+
				"checked against that list.", pending, prose.Plural(pending))))
		b.WriteString("\n")
	}
	if missing > 0 {
		b.WriteString(prose.Para(fmt.Sprintf(
			"%d channel%s confirmed but has no edge in the graph database yet. "+
				"That resolves itself; the loop keeps asking.",
			missing, prose.Plural(missing))))
		b.WriteString("\n")
	}
	b.WriteString(prose.Para(fmt.Sprintf(
		"Until the policy lands, those channels forward at LND's defaults: %d msat "+
			"base and %d ppm, with a CLTV delta of %d. On a large channel that is "+
			"close to free routing for whoever notices first, which is why this "+
			"loop exists.",
		DefaultBaseFeeMsat, DefaultFeeRatePPM, DefaultTimeLockDelta)))
	return b.String()
}

// depthNote says what is being waited for, and admits what cannot be known.
func depthNote(r *Result) string {
	stalled := r.Stalled()
	if len(stalled) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n")

	known := false
	for _, s := range stalled {
		if s.Confs >= 0 {
			known = true
		}
	}
	if !known {
		b.WriteString(prose.Para(
			"No confirmation count is being reported: Core is not connected, or it " +
				"has not seen the transaction. The channel leaving LND's pending " +
				"list is still the authoritative signal and needs nobody's help — " +
				"but without Core there is no way to say how close it is."))
		return b.String()
	}

	b.WriteString(prose.Para(
		"The expected depth is a prediction, not a reading. A peer states its " +
			"minimum_depth in accept_channel; LND stores it and exposes it over no " +
			"RPC — min_accept_depth exists in lnrpc only on ChannelAcceptResponse, " +
			"which is the responder's side of somebody else's channel. So the " +
			"figure shown is LND's own default policy for a channel of this size, " +
			"which binds a peer running stock LND and nobody else."))
	return b.String()
}

// depthLesson reports what the batch actually taught us about each peer.
func depthLesson(r *Result) string {
	var rows []string
	for _, s := range r.States {
		if s.ObservedDepth <= 0 {
			continue
		}
		note := "as predicted"
		if s.ObservedDepth != s.ExpectedDepth {
			note = fmt.Sprintf("predicted %d", s.ExpectedDepth)
		}
		rows = append(rows, fmt.Sprintf("  %-18s opened at %d confirmations (%s)",
			short(s.Member.Peer), s.ObservedDepth, note))
	}
	if len(rows) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nWhat this batch learned:\n\n")
	b.WriteString(strings.Join(rows, "\n"))
	b.WriteString("\n\n")
	b.WriteString(prose.Para(
		"Those are the only authoritative readings of these peers' minimum_depth " +
			"available to an initiator, and they are upper bounds — the loop polls, " +
			"so a channel may have been open for part of a block interval before it " +
			"was seen. Worth keeping for the next batch, since Phase 0 has no way " +
			"to ask."))
	return b.String()
}

// horizonNote is the warning about LND's funding horizon.
func horizonNote(r *Result) string {
	blocks, ok := r.NearestExpiry()
	if !ok {
		return ""
	}

	var b strings.Builder
	b.WriteString("\n")

	switch {
	case blocks < 0:
		b.WriteString(prose.Para(fmt.Sprintf(
			"The funding horizon has passed: %d block%s past it. LND's own proto "+
				"says a negative value means the responder has very likely "+
				"cancelled the funding, and that is what has happened — the peer "+
				"waited %d blocks from the broadcast height and closed its side as "+
				"FundingCanceled.", -blocks, prose.Plural(int(-blocks)),
			ForgetHorizonBlocks)))
		b.WriteString("\n")
		b.WriteString(prose.Para(
			"This node has not given up, and will not: waitForFundingWithTimeout " +
				"only starts the timeout when we are NOT the initiator, and we are. " +
				"So if the transaction confirms now, this node opens a channel the " +
				"peer has forgotten, and the only way out of that is a force-close."))
		b.WriteString("\n")
		b.WriteString(prose.Bullet(
			"Get the transaction confirmed anyway if it is close, and expect to " +
				"force-close what opens. The funds are recoverable; the channel is " +
				"not usable."))
		b.WriteString(prose.Bullet(
			"Never replace the transaction to try to fix this. Replacing it moves " +
				"every funding outpoint, and every peer holds a commitment " +
				"signature against the old ones (I-4)."))
		return b.String()

	case blocks < HorizonUrgent:
		b.WriteString(prose.Para(fmt.Sprintf(
			"%d blocks — roughly a day — before the first peer gives up on this "+
				"funding transaction. Act now.", blocks)))
	case blocks < HorizonWarn:
		b.WriteString(prose.Para(fmt.Sprintf(
			"%d blocks before the first peer gives up on this funding transaction. "+
				"There is time, and there is no reason to leave it.", blocks)))
	default:
		b.WriteString(prose.Para(fmt.Sprintf(
			"%d blocks before the funding horizon, which is a long way off. The "+
				"coins are still ours while the transaction is unconfirmed, and "+
				"every channel in the batch is already recoverable.", blocks)))
		return b.String()
	}

	b.WriteString("\n")
	b.WriteString(prose.Para(fmt.Sprintf(
		"After %d blocks from its broadcast height the responder stops waiting and "+
			"closes its side as FundingCanceled. This node does not — it is the "+
			"initiator, and waitForFundingWithTimeout only arms the timeout for the "+
			"other side. A transaction that confirms after that leaves this node "+
			"holding a channel the peer has forgotten.", ForgetHorizonBlocks)))
	b.WriteString("\n")
	b.WriteString(prose.Bullet(
		"Build a CPFP child spending the change output, in your own wallet. It is " +
			"the only acceleration available, and this tool does not build it: it " +
			"holds no keys and selects no coins. The change output is named in the " +
			"plan and in the run journal."))
	b.WriteString(prose.Bullet(
		"Never a replacement. Replacing the funding transaction changes every " +
			"outpoint in it and destroys every channel in the batch (I-4)."))
	return b.String()
}
