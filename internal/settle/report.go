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
		// "left on the horizon" and not "before the peer gives up". The count
		// is this node's own; see horizonNote for whose number it is not.
		line += fmt.Sprintf("; %d blocks left on the funding horizon", n)
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
	b.WriteString(linkNote(r))
	b.WriteString(depthNote(r))
	b.WriteString(horizonNote(r))
	return b.String()
}

// linkNote says what "not forwarding" was read off, because the column has room
// for two words and the field has three causes.
//
// State.Active is ListChannels' own active field, which LND computes as
// peerOnline && link.EligibleToForward(). So what a false one establishes is
// that this node's link is not eligible to forward, and an offline peer is only
// one of the ways that happens: our own link still coming up after a restart, and
// a link deliberately out of service, are the others. The old column said "peer
// offline", which picked one of the three and put the cause on somebody else's
// node.
//
// Only when a row shows it, and once for the screen.
func linkNote(r *Result) string {
	for _, s := range r.States {
		if s.Open && !s.Active {
			return "\n" + prose.Para(
				"\"not forwarding\" is ListChannels' active field, which LND "+
					"computes as the peer being online and this node's link being "+
					"eligible to forward. A false one says the link is not "+
					"forwarding and does not say which side is why: a peer that is "+
					"offline, a link of ours still coming up, and a link out of "+
					"service all read the same from here. The policy does not wait "+
					"on it — only \"open\" does.")
		}
	}
	return ""
}

// detailIndent is the column the rows' second line and their continuations
// start at: two spaces, the eighteen the peer column is wide, and the space
// after it. Written out rather than computed because the row above is a format
// string and the two have to agree by eye.
const detailIndent = "                     "

// line is one channel's row.
//
// The third column is LND's own refusal text and nothing bounds it: "invalid
// parameter (time lock delta of 4 is too small)" renders this row at 93 columns
// against a 78-column pane, measured off a real refusal. So it wraps under the
// row when it does not fit, the way prose.Table puts an over-wide note on its
// own line.
//
// Wrapped rather than truncated, and that is the decision rather than the
// obvious tidier one. A truncated refusal is a cause the operator cannot read at
// all, which is the defect this whole file was just audited for; a line this
// program wrapped on purpose is merely wider than it wanted to be. The emulator
// would wrap it anyway, at whatever column the pane happens to be, and at that
// point the alignment of every row below it is gone too.
func (s State) line() string {
	var b strings.Builder

	where := "pending"
	switch {
	case s.Open && s.Active:
		where = "open, active"
	case s.Open:
		where = "open, not forwarding"
	}
	row := fmt.Sprintf("  %-18s %-20s", short(s.Member.Peer), where)
	outcome := s.Policy.String()
	if len([]rune(row))+1+len([]rune(outcome)) <= prose.PaneWidth {
		b.WriteString(row + " " + outcome + "\n")
	} else {
		b.WriteString(strings.TrimRight(row, " ") + "\n")
		b.WriteString(prose.Wrap(outcome, detailIndent, detailIndent))
	}

	detail := []string{}
	if s.Confs >= 0 {
		detail = append(detail, fmt.Sprintf("%d conf", s.Confs))
	}
	if d := depthDetail(s); d != "" {
		detail = append(detail, d)
	}
	if s.StillPending {
		detail = append(detail, fmt.Sprintf("%d blocks to the horizon", s.ExpiryBlocks))
	}
	if s.Attempts > 1 {
		detail = append(detail, fmt.Sprintf("%d policy attempts", s.Attempts))
	}
	if len(detail) > 0 {
		b.WriteString(prose.Wrap(strings.Join(detail, ", "), detailIndent, detailIndent))
	}
	return b.String()
}

// depthDetail is the depth figures for one row, and it names which figure it is
// showing rather than printing a bare number.
//
// Three of them exist and they establish different things — see the package
// comment. "asked for" is the peer's own, off PendingChannels while the funding
// transaction was unconfirmed; "expect ~" is LND's default policy for a channel
// of this size, which is a prediction; "opened at" is what the harness watched
// happen. A row that showed a number without saying which one it was would be
// asserting the peer's answer on a run that only ever guessed at it.
func depthDetail(s State) string {
	switch {
	case s.ObservedDepth > 0 && s.PeerDepth > 0:
		return fmt.Sprintf("opened at %d, asked for %d", s.ObservedDepth, s.PeerDepth)
	case s.ObservedDepth > 0:
		return fmt.Sprintf("opened at %d, predicted %d", s.ObservedDepth, s.ExpectedDepth)
	case s.Open:
		// Open, and neither reading was taken: the channel was already past
		// this loop's questions when it first asked. Nothing to say.
		return ""
	case s.PeerDepth > 0:
		return fmt.Sprintf("peer asked for %d confirmation%s", s.PeerDepth,
			prose.Plural(int(s.PeerDepth)))
	default:
		return fmt.Sprintf("expect ~%d", s.ExpectedDepth)
	}
}

// policyNote explains why a channel is still on LND's defaults.
//
// Five reasons, and they are not interchangeable. Two of them are the loop
// working — a pending channel and a missing graph edge both resolve themselves —
// one is a refusal still being retried, and two are members the loop has given
// up on, for opposite reasons.
//
// The copy for the last of those used to say "that is the policy itself, not the
// channel", which asserted something the app cannot know and was false about the
// one live batch that reached it: LND had refused with its catch-all while a peer
// was offline, and the same update applied by hand minutes later. Only
// INVALID_PARAMETER earns a "this will not change" sentence, so only
// INVALID_PARAMETER gets one — and it earns it for the opposite mechanism to the
// one the copy claimed. See PolicyOutcome.Terminal.
func policyNote(r *Result) string {
	var pending, missing, invalid, silent, retrying int
	for _, s := range r.States {
		switch {
		case s.Policy.Applied:
		case s.Policy.Terminal():
			invalid++
		case s.Stuck():
			silent++
		case s.Policy.Unexplained():
			retrying++
		case s.Policy.Reason == lnrpc.UpdateFailure_UPDATE_FAILURE_PENDING:
			pending++
		case s.Policy.Reason == lnrpc.UpdateFailure_UPDATE_FAILURE_NOT_FOUND:
			missing++
		}
	}
	if pending+missing+invalid+silent+retrying == 0 {
		return ""
	}

	var b strings.Builder
	if invalid > 0 {
		b.WriteString(prose.Para(fmt.Sprintf(
			"%d channel%s refused the policy against bounds it negotiated when it "+
				"opened, and that refusal will not change: those bounds are fixed "+
				"for the life of the channel. The figures are what is wrong, not "+
				"the timing — read the detail beside each one below.",
			invalid, prose.Plural(invalid))))
		b.WriteString("\n")
	}
	if silent > 0 {
		b.WriteString(prose.Para(fmt.Sprintf(
			"%d channel%s %s refused for %s without LND saying why, so the loop has "+
				"stopped asking about %s and has kept running for everything else. "+
				"The reason LND gives is its catch-all: not a verdict on the policy, "+
				"and not a fact about the channel either. A peer that is briefly "+
				"offline lands here, and the same update often applies by hand a few "+
				"minutes later.",
			silent, prose.Plural(silent), prose.WasWere(silent), silence(r),
			itThem(silent))))
		b.WriteString("\n")
	}
	if stuck := r.Stuck(); len(stuck) > 0 {
		b.WriteString(prose.Para(fmt.Sprintf(
			"Apply %s by hand, and check that failed_updates came back empty — this "+
				"call reports a refusal inside a success, so a nil error is not "+
				"evidence. The figures are in the plan above and in the run journal:",
			itThem(len(stuck)))))
		b.WriteString("\n")
		for _, s := range stuck {
			// The channel point on its own line and never wrapped: it is 66
			// characters, it is what the next command has to be given, and a
			// wrapped outpoint is a transcription error waiting to happen.
			detail := fmt.Sprintf("%s — %s", short(s.Member.Peer), s.Policy)
			if s.Attempts > 1 {
				detail += fmt.Sprintf(", after %d attempt%s", s.Attempts,
					prose.Plural(s.Attempts))
			}
			b.WriteString(fmt.Sprintf("  - %s\n", s.Member.Channel))
			b.WriteString(prose.Wrap(detail, "    ", "    "))
		}
		b.WriteString("\n")
	}
	if retrying > 0 {
		b.WriteString(prose.Para(fmt.Sprintf(
			"%d channel%s %s being refused without a reason given, and still "+
				"being retried. LND's catch-all covers the transient as well as the "+
				"permanent — a peer briefly offline is one — so it is retried for up "+
				"to %s from the first refusal, and then reported rather than being "+
				"read as a verdict on arrival.",
			retrying, prose.Plural(retrying), prose.IsAre(retrying),
			r.retryWindow())))
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
//
// Up to three paragraphs, and every one of them is conditional on a row above
// having shown the thing it explains. That discipline was the whole of issue
// #21: a note that fires on a branch the operator never reaches leaves the one
// number they do see unexplained.
//
// The first two are the two halves of depthDetail, and a batch can contain both.
// A channel that was pending and unconfirmed when this loop first asked has the
// peer's own figure; one that had already confirmed by then has only the
// prediction, and there is no second chance at the reading.
//
// The third is the confirmation count, and it says the design fact rather than a
// fault. It used to say "Core is not connected, or it has not seen the
// transaction": the first half read as something to go and fix and sent the
// operator to debug a bitcoind this application has not dialled since item 5,
// and the second was a cause nothing here established — no question was asked of
// anything about the transaction's propagation.
func depthNote(r *Result) string {
	stalled := r.Stalled()
	if len(stalled) == 0 {
		return ""
	}

	var asked, predicted, counted int
	for _, s := range stalled {
		if s.PeerDepth > 0 {
			asked++
		} else {
			predicted++
		}
		if s.Confs >= 0 {
			counted++
		}
	}

	var b strings.Builder
	b.WriteString("\n")

	if asked > 0 {
		b.WriteString(prose.Para(fmt.Sprintf(
			"%d channel%s %s showing the depth its peer asked for, which is a "+
				"reading and not a guess. The peer states a minimum_depth in "+
				"accept_channel, LND stores what it sent, and PendingChannels "+
				"reports it back as confirmations_until_active for as long as the "+
				"funding transaction is unconfirmed. One hedge on it: a peer that "+
				"sent zero is stored as one, so the figure is what the channel will "+
				"wait for rather than what the peer wrote on the wire.",
			asked, prose.Plural(asked), prose.IsAre(asked))))
	}

	if predicted > 0 {
		if asked > 0 {
			b.WriteString("\n")
		}
		b.WriteString(prose.Para(fmt.Sprintf(
			"%d channel%s %s showing an expected depth instead, which is a "+
				"prediction: LND's own default policy for a channel of that size, "+
				"which binds a "+
				"peer running stock LND and nobody else. The peer's own figure is "+
				"readable only while the funding transaction is unconfirmed, and "+
				"%s already past that when this loop first asked.",
			predicted, prose.Plural(predicted), prose.IsAre(predicted),
			wasWereThey(predicted))))
	}

	if counted == 0 {
		b.WriteString("\n")
		b.WriteString(prose.Para(
			"No confirmation count is reported, and none will be: this build dials " +
				"no Bitcoin node, so there is nothing in it that can count blocks " +
				"over the funding transaction. That is what this program is, not a " +
				"fault to go and fix. The channel leaving LND's pending list is the " +
				"authoritative signal and needs nobody's help — it simply arrives " +
				"without a countdown in front of it."))
	}
	return b.String()
}

// wasWereThey keeps the prediction paragraph grammatical for one channel and for
// several, without naming the count a second time.
func wasWereThey(n int) string {
	if n == 1 {
		return "it was"
	}
	return "they were"
}

// depthLesson reports what the batch actually taught us about each peer.
//
// Two kinds of row, and until issue #47 only the second existed — which meant
// this section never printed on a production run at all, because the depth it
// reported is read off Confs and no production run has a Chain to read. The
// peer's own figure needs none: it comes off the PendingChannels call this
// package already makes, so the lesson is now something a real batch leaves
// behind rather than something only the harness ever saw.
func depthLesson(r *Result) string {
	var (
		rows  []string
		asked int
		seen  int
	)
	for _, s := range r.States {
		switch {
		case s.PeerDepth > 0:
			note := "as predicted"
			if s.PeerDepth != s.ExpectedDepth {
				note = fmt.Sprintf("predicted %d", s.ExpectedDepth)
			}
			row := fmt.Sprintf("  %-18s asked for %d confirmation%s (%s)",
				short(s.Member.Peer), s.PeerDepth,
				prose.Plural(int(s.PeerDepth)), note)
			if s.ObservedDepth > 0 {
				row += fmt.Sprintf(", opened at %d", s.ObservedDepth)
			}
			rows = append(rows, row)
			asked++
		case s.ObservedDepth > 0:
			note := "as predicted"
			if s.ObservedDepth != s.ExpectedDepth {
				note = fmt.Sprintf("predicted %d", s.ExpectedDepth)
			}
			rows = append(rows, fmt.Sprintf("  %-18s opened at %d confirmations (%s)",
				short(s.Member.Peer), s.ObservedDepth, note))
			seen++
		}
	}
	if len(rows) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("\nWhat this batch learned:\n\n")
	b.WriteString(strings.Join(rows, "\n"))
	b.WriteString("\n\n")

	if asked > 0 {
		b.WriteString(prose.Para(
			"\"Asked for\" is the peer's own figure, read off PendingChannels while " +
				"the funding transaction was still unconfirmed, and it is exact " +
				"apart from the floor LND puts under a zero. Worth keeping for the " +
				"next batch: it is readable from the moment a channel is pending, " +
				"which is early enough to tell an operator when each channel will " +
				"be usable."))
	}
	if seen > 0 {
		if asked > 0 {
			b.WriteString("\n")
		}
		b.WriteString(prose.Para(
			"\"Opened at\" is the depth the channel was seen open at, which is an " +
				"upper bound rather than the peer's number — the loop polls, so a " +
				"channel may have been open for part of a block interval before it " +
				"was seen. It stands in where the peer's own figure was not " +
				"readable, and where both are shown it is the independent check on " +
				"the other."))
	}
	return b.String()
}

// horizonNote is the warning about LND's funding horizon.
//
// Every figure here is ours. The countdown is funding_expiry_blocks off
// PendingChannels, which is this node's own arithmetic against LND's own
// default, and ForgetHorizonBlocks is that default written down. Nothing in this
// build has heard from the peer about any of it, and that is what separates this
// figure from the minimum_depth reported beside it on the same message: a peer
// states its minimum_depth in accept_channel, so LND has something of the peer's
// to store and hand back. A peer's funding horizon is in no message it ever
// sends, so there is nothing to store and nothing to read.
//
// So this used to quote LND's proto hedge — "very likely cancelled" — and
// override it in the same sentence with "and that is what has happened", then
// print our own constant as the number of blocks the peer waited. Both are
// causes the program cannot know, and it said them at the moment the operator is
// under the most pressure and the remedy is still live. The remedy did not
// change; the certainty did.
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
			"This node's count of the funding horizon has run out: %d block%s past "+
				"it. The count is ours — funding_expiry_blocks off PendingChannels, "+
				"measured against LND's own default of %d blocks from the broadcast "+
				"height — and LND's proto says a negative value means the responder "+
				"has very likely cancelled the funding.", -blocks,
			prose.Plural(int(-blocks)), ForgetHorizonBlocks)))
		b.WriteString("\n")
		b.WriteString(prose.Para(
			"Very likely is as far as this goes. Nothing here has heard from the " +
				"peer, and its horizon is its own: another implementation, another " +
				"version, or a non-default --maxwaitnumblocksfundingconf gives up " +
				"somewhere else. A peer's minimum_depth can be read because the " +
				"peer stated it in accept_channel and LND kept it. Its horizon it " +
				"never stated, so there is nothing stored anywhere to read."))
		b.WriteString("\n")
		b.WriteString(prose.Para(
			"This node has not given up, and will not: waitForFundingWithTimeout " +
				"only starts the timeout when we are NOT the initiator, and we are. " +
				"So if the transaction confirms now and the peer has in fact " +
				"stopped waiting, this node opens a channel with nobody on the " +
				"other side of it, and the only way out of that is a force-close."))
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
			"%d blocks — roughly a day — left on this funding transaction's "+
				"horizon. Act now.", blocks)))
	case blocks < HorizonWarn:
		b.WriteString(prose.Para(fmt.Sprintf(
			"%d blocks left on this funding transaction's horizon. There is time, "+
				"and there is no reason to leave it.", blocks)))
	default:
		b.WriteString(prose.Para(fmt.Sprintf(
			"%d blocks before the funding horizon, which is a long way off. The "+
				"coins are still ours while the transaction is unconfirmed, and "+
				"every channel in the batch is already recoverable.", blocks)))
		return b.String()
	}

	b.WriteString("\n")
	b.WriteString(prose.Para(fmt.Sprintf(
		"That %d is LND's own default: a responder running stock LND stops waiting "+
			"that far from the broadcast height and closes its side as "+
			"FundingCanceled. It binds a peer running stock LND and nobody else — "+
			"a peer's real horizon is its own, and no RPC reports it to the "+
			"initiator. This node has no horizon at all: waitForFundingWithTimeout "+
			"arms the timeout only for the other side, so a transaction that "+
			"confirms after a peer has stopped waiting leaves this node holding a "+
			"channel with nobody on the other side of it.", ForgetHorizonBlocks)))
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

// itThem keeps the instructions grammatical without naming a count twice.
func itThem(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}

// silence renders how long the most patiently retried member was refused for.
//
// Below a second there is nothing worth naming and "0s" would read as a defect;
// only a test drives the loop fast enough for that, so the window it was
// measured against stands in.
func silence(r *Result) string {
	if d := longestSilence(r).Round(time.Second); d > 0 {
		return d.String()
	}
	return r.retryWindow().String()
}

// longestSilence is how long the most patiently retried member was refused for,
// which is the figure the copy should name: the window is a ceiling, and a
// member reaches it one poll past it rather than exactly on it.
func longestSilence(r *Result) time.Duration {
	var out time.Duration
	for _, s := range r.States {
		if s.RefusedFor > out {
			out = s.RefusedFor
		}
	}
	return out
}
