package peers

import (
	"fmt"
	"strings"
	"time"

	"github.com/AusDavo/winthistle/internal/prose"
)

// Summary is the one line a list wants.
func (f Facts) Summary() string {
	if !f.KeyOK {
		return fmt.Sprintf("%s — %s", short(f.Want.Pubkey), f.KeyProblem)
	}
	name := f.Alias
	if name == "" {
		name = short(f.Want.Pubkey)
	}
	if !f.InGraph {
		return fmt.Sprintf("%s — %s, and not in this node's gossip graph",
			name, f.Connection)
	}
	line := fmt.Sprintf("%s — %s, %d channels, %s total, smallest %s",
		name, f.Connection, f.NumChannels, prose.Sats(f.TotalCapacitySat),
		prose.Sats(f.SmallestSat))
	if n := len(f.Pending); n > 0 {
		line += fmt.Sprintf(", %d already pending", n)
	}
	return line
}

// Report is the operator-facing text for one peer.
//
// It is careful about one thing above all: saying which of these figures is
// evidence and which is a verdict. Nothing on this page is a verdict. The peer's
// minimum channel size is enforced in accept_channel and published nowhere, so
// the smallest channel it already has is the best the graph can do — and a
// report that let that read as a limit would send an operator into the armed
// window confident about a number nobody stated.
func (f Facts) Report() string {
	var b strings.Builder

	if !f.KeyOK {
		b.WriteString(prose.Para(fmt.Sprintf(
			"%s is not a usable peer key: %s.", f.Want.Pubkey, f.KeyProblem)))
		b.WriteString("\n")
		b.WriteString(prose.Para(
			"Nothing was asked of the network. This is a local check — 33 bytes, " +
				"compressed, a real point on secp256k1 — and it catches a " +
				"transposed or truncated key at the only moment when catching it " +
				"is free."))
		return b.String()
	}

	name := f.Alias
	if name == "" {
		name = "(no alias in the graph)"
	}
	b.WriteString(fmt.Sprintf("%s\n%s\n\n", name, f.Want.Pubkey))

	b.WriteString(fmt.Sprintf("  connection    %s\n", f.Connection))
	if f.ConnectDetail != "" {
		b.WriteString(prose.Indent(f.ConnectDetail, "                "))
	}

	if !f.InGraph {
		b.WriteString("\n")
		b.WriteString(prose.Para(
			"This node's gossip graph has never heard of that key. That is not " +
				"proof of anything: a new node, or one with only private " +
				"channels, looks exactly like this. It does mean there is no " +
				"capacity evidence below, and the only way to learn what this " +
				"peer accepts is to ask it — see the shim probe."))
		// Last, not in the middle of the figures: this is interpretation, and
		// for a peer the graph does not know it is the only interpretation there
		// is.
		b.WriteString(pendingSection(f))
		return b.String()
	}

	b.WriteString(fmt.Sprintf("  channels      %d\n", f.NumChannels))
	b.WriteString(prose.Table([]prose.Row{
		prose.Line("total capacity", f.TotalCapacitySat),
		prose.Note("smallest channel", f.SmallestSat, "evidence, not a limit"),
		prose.Line("median channel", f.MedianSat),
		prose.Line("this batch would ask for", f.Want.AmountSat),
	}))
	if !f.LastUpdate.IsZero() {
		b.WriteString(fmt.Sprintf("\n  last gossip update  %s\n",
			f.LastUpdate.Format(time.RFC3339)))
	}

	b.WriteString("\n")
	if f.BelowSmallest() {
		b.WriteString(prose.Para(fmt.Sprintf(
			"The batch would ask for less than the smallest channel this peer "+
				"already has (%s). That is a reason to check, not a refusal: "+
				"there is no gossip field for a minimum channel size, and a peer's "+
				"existing channels are evidence about its policy rather than a "+
				"statement of it. The authoritative answer comes from "+
				"accept_channel and nowhere else.", prose.Sats(f.SmallestSat))))
	} else {
		b.WriteString(prose.Para(
			"Nothing here is authoritative about what this peer will accept. " +
				"Minimum channel size is enforced conversationally, in " +
				"accept_channel, and is published nowhere — so the figures above " +
				"are the local graph's evidence and the probe is the answer."))
	}
	b.WriteString(pendingSection(f))
	return b.String()
}

// pendingSection is what this node already has pending with the peer.
//
// It is the one figure in this report that is not evidence about somebody else's
// policy — it is a fact about our own node, read for free. What it costs to
// learn the hard way is a probe: a peer at its limit answers step 2 with
// ErrMaxPendingChannels, which arrives as TooManyPending with the cold wallet
// already out.
func pendingSection(f Facts) string {
	if !f.HasCompetingOpen() {
		return ""
	}

	var b strings.Builder
	b.WriteString("\n")
	b.WriteString(prose.Para(fmt.Sprintf(
		"This node already has %d channel%s pending open with this peer, before "+
			"the batch adds one. A peer running LND's default of one pending "+
			"channel has no room left: fundeeProcessOpenChannel counts its "+
			"reservations for us plus its pending channels with us, and refuses "+
			"past --maxpendingchannels. That refusal would arrive at step 2, on "+
			"the peers' clock, with the cold wallet out.",
		len(f.Pending), prose.Plural(len(f.Pending)))))
	b.WriteString("\n")

	for _, po := range f.Pending {
		who := "the peer opened it"
		if po.Ours {
			who = "this node opened it"
		}
		if po.Private {
			who += ", unannounced"
		}
		b.WriteString(fmt.Sprintf("  %s  (%s)\n", prose.Sats(po.CapacitySat), who))
		// The outpoint on its own line at a small indent: 64 hex characters plus
		// an index, and it is the string an operator pastes elsewhere.
		b.WriteString("    " + po.ChannelPoint + "\n")
	}

	b.WriteString("\n")
	b.WriteString(prose.Para(
		"This is a reason to look, not a refusal: --maxpendingchannels is the " +
			"peer's own configuration and is published nowhere, so a peer that " +
			"allows several will take this batch. Two things it cannot tell you. " +
			"An empty answer is not proof of a free slot — AbandonChannel is " +
			"local-only, so a channel this node abandoned is gone from this list " +
			"while the peer still counts it, until 2016 blocks pass from its " +
			"funding height. And one this node opened may be an earlier run of " +
			"this tool that did not finish, which `winthistle recover` is for."))
	return b.String()
}

// Summary is the one line a probe wants.
func (p Probe) Summary() string {
	if p.Accepted {
		return fmt.Sprintf("%s accepted %s", short(p.Pubkey), prose.Sats(p.AmountSat))
	}
	switch p.Rejection.Kind {
	case TooSmall:
		return fmt.Sprintf("%s refused %s: its minimum is %s", short(p.Pubkey),
			prose.Sats(p.AmountSat), prose.Sats(p.Rejection.MinChanSat))
	case TooLarge:
		return fmt.Sprintf("%s refused %s: its maximum is %s", short(p.Pubkey),
			prose.Sats(p.AmountSat), prose.Sats(p.Rejection.MaxChanSat))
	default:
		return fmt.Sprintf("%s refused %s — %s", short(p.Pubkey),
			prose.Sats(p.AmountSat), p.Rejection.Kind)
	}
}

// Report is the operator-facing text for one probe.
func (p Probe) Report(now time.Time) string {
	var b strings.Builder

	took := p.Answered.Sub(p.Started).Round(time.Millisecond)

	if p.Accepted {
		b.WriteString(prose.Para(fmt.Sprintf(
			"%s accepted a channel of %s. It sent accept_channel and LND named a "+
				"funding output, which is the only authoritative answer there is: "+
				"this peer will take this channel at this size, right now.",
			short(p.Pubkey), prose.Sats(p.AmountSat))))
		b.WriteString(fmt.Sprintf("\n  answered in   %s\n", took))
		b.WriteString(cancelLine(p))
		// On its own line at a small indent: a P2WSH bech32 address is 62
		// characters and cannot be wrapped or abbreviated, so the layout gives
		// way to it rather than the other way round.
		b.WriteString("  funding to\n    " + p.FundingAddress + "\n")
		b.WriteString("\n")
		b.WriteString(prose.Para(fmt.Sprintf(
			"This answer cost one of that peer's pending-channel slots, and " +
				"cancelling the shim did not give it back. shim_cancel is local: " +
				"CancelFundingIntent deletes an entry in our own wallet and sends " +
				"the peer nothing. Only the peer's own zombie sweeper releases the " +
				"reservation, after ten minutes with up to a minute of slack.")))
		b.WriteString("\n")
		if p.Held(now) {
			b.WriteString(prose.Para(fmt.Sprintf(
				"Assume it is held for another %s. A peer running LND's default "+
					"of one pending channel will refuse the real open until then, "+
					"with \"Number of pending channels exceed maximum\" — arriving "+
					"at step 2, with the cold wallet out.",
				roundSeconds(p.Remaining(now)))))
		} else {
			b.WriteString(prose.Para(
				"That window has passed, so the reservation can be assumed gone."))
		}
		b.WriteString("\n")
		b.WriteString(prose.Para(
			"If the batch was ready to arm, this probe was the wrong call: it is " +
				"step 2 with the answer thrown away. arm.Open asks the same " +
				"question, holds the same reservation, and keeps what comes back."))
		return b.String()
	}

	b.WriteString(prose.Para(fmt.Sprintf("%s refused a channel of %s.",
		short(p.Pubkey), prose.Sats(p.AmountSat))))
	b.WriteString(fmt.Sprintf("\n  answered in   %s\n\n", took))

	// Wrapped, and labelled by who actually said it. For a peer that was never
	// reached the only words available are our own node's, and presenting those
	// as the peer's answer would attribute a local failure to somebody else's
	// policy.
	said, who := p.Rejection.Peer, "The peer said:"
	if said == "" {
		said, who = p.Rejection.Raw, "This node said:"
	}
	b.WriteString("  " + who + "\n")
	b.WriteString(prose.Indent(said, "    "))
	b.WriteString("\n")

	switch p.Rejection.Kind {
	case TooSmall:
		b.WriteString(prose.Table([]prose.Row{
			prose.Line("this batch asked for", p.AmountSat),
			prose.Note("the peer's minimum", p.Rejection.MinChanSat, "its own figure"),
			prose.Line("short by", p.Rejection.MinChanSat-p.AmountSat),
		}))
		b.WriteString("\n")
		b.WriteString(prose.Para(
			"That figure is the peer's own, out of accept_channel's refusal, and " +
				"it is the authoritative one. Raise this channel to at least the " +
				"minimum, or drop this peer from the batch."))
	case TooLarge:
		b.WriteString(prose.Table([]prose.Row{
			prose.Line("this batch asked for", p.AmountSat),
			prose.Note("the peer's maximum", p.Rejection.MaxChanSat, "its own figure"),
		}))
	case TooManyPending:
		b.WriteString(prose.Para(
			"The peer is already holding as many pending channels from us as it " +
				"will. Very often that is our own doing: an earlier probe, or an " +
				"aborted batch. AbandonChannel is local-only, so a channel we " +
				"abandoned is still pending on the peer until 2016 blocks pass " +
				"from its funding height — which, on a chain that is not mining, " +
				"is never."))
		b.WriteString("\n")
		b.WriteString(prose.Para(
			"A cancelled shim clears in ten minutes. A channel that reached " +
				"chan_pending does not."))
	case Internal:
		b.WriteString(prose.Para(
			"LND's generic refusal. failFundingFlow forwards the real text only " +
				"for a reservation error, a funding error or a channel-acceptor " +
				"error; everything else becomes \"funding failed due to internal " +
				"error\". The reason exists, on the peer's machine, in its log. " +
				"There is nothing more to read from here."))
	case NotConnected:
		b.WriteString(prose.Para(
			"The question was never put to the peer: this node could not reach " +
				"it. Costs nothing, says nothing about the peer's policy."))
	default:
		b.WriteString(prose.Para(
			"This build does not recognise that refusal. The peer's own words are " +
				"above; a peer that is not LND can refuse in any words it likes."))
	}

	// Not for a peer that was never reached: nothing was asked, so there is
	// nothing to say about what asking costs.
	if p.Rejection.Kind != NotConnected {
		b.WriteString("\n")
		b.WriteString(prose.Para(
			"A refusal costs nothing. Every limit check in " +
				"fundeeProcessOpenChannel runs before the peer creates a " +
				"reservation, so there is no slot held and no window to wait " +
				"out — probing downwards to find a peer's minimum is free, and " +
				"only the probe that succeeds has to be the last one."))
	}

	if strings.Contains(p.Rejection.Raw, remoteCanceled) {
		b.WriteString("\n")
		b.WriteString(prose.Para(
			"LND presented this as \"remote canceled funding, possibly timed " +
				"out\". It did not time out — it answered in the time shown " +
				"above. handleErrorMsg wraps every peer error in that phrase when " +
				"the reservation is a PSBT one, whatever the peer actually said."))
	}
	return b.String()
}

func cancelLine(p Probe) string {
	if p.CancelErr != nil {
		return fmt.Sprintf("  shim          NOT cancelled: %v\n", p.CancelErr)
	}
	return "  shim          cancelled\n"
}
