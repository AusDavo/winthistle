package bump

import (
	"context"
	"fmt"
	"strings"

	"github.com/AusDavo/winthistle/internal/abort"
	"github.com/AusDavo/winthistle/internal/prose"
	"github.com/AusDavo/winthistle/internal/settle"
)

// Report is the parent as the bump found it: what it pays, what it has to spend,
// and how long the peers will wait.
func (l *Located) Report() string {
	if l == nil {
		return prose.Para("No batch was found.")
	}
	var b strings.Builder

	b.WriteString("  funding transaction\n    " + l.ParentTxID + "\n\n")
	b.WriteString(prose.Table([]prose.Row{
		prose.Note("the batch pays", l.Parent.FeeSat,
			fmt.Sprintf("%.2f sat/vB over %d vB", l.Parent.Rate(), l.Parent.VsizeVB)),
		prose.Note("change available", l.Parent.ChangeSat,
			fmt.Sprintf("output %d", l.Parent.Change.Vout)),
	}))
	b.WriteString("\n")

	b.WriteString(prose.Para("Both of those figures are Core's, out of its own " +
		"mempool entry for this transaction, rather than anything remembered from " +
		"the run that built it. A transaction being bumped is unconfirmed by " +
		"definition, so the node already holds its exact virtual size and its " +
		"exact fee — and re-deriving either by summing the inputs would be a " +
		"second opinion that could quietly disagree with the one the fee market " +
		"uses."))

	if l.Entry.HasUnconfirmedAncestors() {
		b.WriteString("\n")
		b.WriteString(prose.Para(fmt.Sprintf(
			"The figures above are the *ancestor* figures, and that is deliberate: "+
				"this batch depends on %d unconfirmed transaction(s) of its own, so it "+
				"is not the bottom of its own package. walletcreatefundedpsbt charges "+
				"the fee that lifts the whole unconfirmed ancestor package to the rate "+
				"it is given, so the arithmetic has to be about the same package Core "+
				"is charging for. On its own the batch is %d vB paying %s.",
			l.Entry.AncestorCount-1, l.Entry.VsizeVB, prose.Sats(l.Entry.FeeSat))))
	}

	b.WriteString("\n")
	b.WriteString(prose.Para("The change output is the only output of a batch the " +
		"cold wallet can see, which is what makes finding it a fact rather than a " +
		"guess: the funding outputs are the peers' own 2-of-2 scripts, and the " +
		"reserve top-up pays this node's wallet rather than the Core one."))

	b.WriteString(l.horizon())
	return b.String()
}

// horizon is the countdown, and what it is a countdown to.
func (l *Located) horizon() string {
	if !l.HasExpiry {
		return "\n" + prose.Para(
			"LND lists no pending channel for this transaction, so there is no "+
				"funding countdown to report. Either every channel in the batch is "+
				"already open — in which case a bump buys nothing — or this node no "+
				"longer has them.")
	}

	var b strings.Builder
	b.WriteString("\n")

	switch {
	case l.ExpiryBlocks < 0:
		b.WriteString(prose.Para(fmt.Sprintf(
			"The funding horizon has already passed: %d block(s) beyond it. LND's "+
				"own proto says a negative value means the channel responder has very "+
				"likely cancelled the funding, and that is what has happened — each "+
				"peer waited %d blocks from the broadcast height and closed its side "+
				"as FundingCanceled.", -l.ExpiryBlocks, settle.ForgetHorizonBlocks)))
		b.WriteString("\n")
		b.WriteString(prose.Para(
			"A child is still worth building, and this is the one place where that " +
				"needs saying out loud. What expired is the peers' patience, not this " +
				"transaction: the coins are sitting in an unconfirmed transaction that " +
				"I-4 forbids replacing, so confirming it is the only way they ever " +
				"become spendable again. The channels are a separate loss, and they " +
				"are already lost."))
		b.WriteString("\n")
		b.WriteString(prose.Bullet("Expect to force-close whatever opens. This node " +
			"is the initiator and never times out, so if the transaction confirms it " +
			"holds channels the peers have forgotten. The funds come back; the " +
			"channels are not usable."))
		b.WriteString(prose.Bullet("Never replace the transaction instead. Replacing " +
			"it moves every funding outpoint, and every peer holds a commitment " +
			"signature against the old ones (I-4)."))
		return b.String()

	case l.ExpiryBlocks < settle.HorizonUrgent:
		b.WriteString(prose.Para(fmt.Sprintf(
			"%d blocks — roughly a day — before the first peer gives up on this "+
				"funding transaction. This is the right thing to be doing.",
			l.ExpiryBlocks)))
	case l.ExpiryBlocks < settle.HorizonWarn:
		b.WriteString(prose.Para(fmt.Sprintf(
			"%d blocks before the first peer gives up on this funding transaction. "+
				"There is time, and there is no reason to leave it.", l.ExpiryBlocks)))
	default:
		b.WriteString(prose.Para(fmt.Sprintf(
			"%d blocks before the funding horizon, which is a long way off. Every "+
				"channel in the batch is already recoverable and the coins are still "+
				"yours while the transaction is unconfirmed — so this is about the "+
				"fee market rather than about a deadline.", l.ExpiryBlocks)))
		return b.String()
	}

	b.WriteString("\n")
	b.WriteString(prose.Para(fmt.Sprintf(
		"Worth knowing what that clock is. After %d blocks from its broadcast "+
			"height the responder stops waiting and closes its side as "+
			"FundingCanceled. This node does not — waitForFundingWithTimeout only "+
			"arms the timeout for the side that is not the initiator — so a "+
			"transaction confirming after that leaves this node holding channels "+
			"the peers have forgotten.", settle.ForgetHorizonBlocks)))
	b.WriteString("\n")
	b.WriteString(prose.Para("It is also the clock the signing round below is " +
		"racing, and it is the only race in this design where the thing being " +
		"raced does not wait. At roughly ten minutes a block that is about " +
		blocksAsTime(l.ExpiryBlocks) + " of wall clock. Nothing here enforces it: " +
		"if the horizon passes while the devices are signing, the child is still " +
		"the right thing to broadcast, because the coins need the transaction " +
		"confirmed either way."))
	return b.String()
}

// blocksAsTime renders a block count as wall clock, at ten minutes a block.
//
// An estimate and presented as one. Block intervals are not ten minutes, they
// are exponentially distributed with a ten-minute mean, so a specific count is
// a coin-flip either side of this. It is still the number an operator deciding
// whether to fetch two hardware devices from two places needs.
func blocksAsTime(blocks int32) string {
	mins := int64(blocks) * 10
	switch {
	case mins < 90:
		return fmt.Sprintf("%d minutes", mins)
	case mins < 48*60:
		return fmt.Sprintf("%d hours", mins/60)
	default:
		return fmt.Sprintf("%d days", mins/(24*60))
	}
}

// Report is the verifier's findings.
//
// The `what` names which pass this is, because there are two and they are not
// interchangeable: the first stops an unverified PSBT reaching a device, and the
// second is the one that can measure rather than estimate.
func (v *Verification) Report(what string) string {
	if v == nil {
		return prose.Para("Nothing was verified.")
	}
	var b strings.Builder

	b.WriteString(fmt.Sprintf("\nVerified — %s\n\n", what))
	b.WriteString("  child txid\n    " + v.TxID + "\n\n")

	sizeNote := fmt.Sprintf("%d vB, an upper bound", v.VsizeVB)
	if v.Signed {
		sizeNote = fmt.Sprintf("%d vB, measured", v.VsizeVB)
	}
	b.WriteString(prose.Table([]prose.Row{
		prose.Note("the child pays", v.FeeSat, sizeNote),
		prose.Note("the pair pays", v.PackageFee,
			fmt.Sprintf("%.2f sat/vB over %d vB", v.PackageRate, v.PackageVB)),
		prose.Line("returned to cold storage", v.OutputSat),
	}))
	b.WriteString(fmt.Sprintf("\n  %-24s  %.0f sat/vB\n", "the child's own rate",
		v.ChildRate))

	if v.OK() {
		b.WriteString("\n")
		if v.Signed {
			b.WriteString(prose.Para("Every witness is present, so the size above is " +
				"a measurement and the rates are the rates this transaction will " +
				"actually pay rather than floors. It spends the batch's change and " +
				"nothing else, pays the one cold-wallet script it was built to pay, " +
				"is not replaceable, and its fee is the figure the arithmetic called " +
				"for."))
		} else {
			b.WriteString(prose.Para("It spends the batch's change output and nothing " +
				"else, pays back to the one cold-wallet script the app asked Core for, " +
				"signals no replaceability, and its fee is the figure the package " +
				"arithmetic called for. The size is an upper bound — every signature " +
				"is counted at the largest a DER encoding can be — so the rates above " +
				"are floors rather than estimates."))
		}
		return b.String()
	}

	b.WriteString("\n")
	b.WriteString(prose.Para(fmt.Sprintf("%d problem%s. Every one of them is a "+
		"refusal: this verifier runs between a transaction being built and a "+
		"cold-storage device being asked to sign it, and a finding worth printing "+
		"at that moment is worth stopping for.",
		len(v.Problems), prose.Plural(len(v.Problems)))))
	b.WriteString("\n")
	for _, p := range v.Problems {
		b.WriteString(prose.Bullet(p.String()))
		if p.Detail != "" {
			b.WriteString(prose.Indent(p.Detail, "      "))
		}
	}
	return b.String()
}

// buildOnly is the dry run's ending.
func buildOnly(child *settle.Child) string {
	var b strings.Builder
	b.WriteString("\n")
	b.WriteString(prose.Para("Built and verified, and no device was asked for " +
		"anything. The arithmetic is the part of a bump that can be wrong, so " +
		"checking it before a cold wallet comes out is worth the one Core call it " +
		"costs."))
	b.WriteString("\n")
	b.WriteString(prose.Para(fmt.Sprintf(
		"Run it again without --build-only to sign and broadcast. The child will "+
			"be rebuilt rather than reused: Core's fee arithmetic is about the "+
			"mempool as it is at that moment, and this one (%s) was priced against "+
			"the mempool as it was at this one.", child.TxID)))
	return b.String()
}

// reportRace is the funding horizon, re-read after the signing round.
//
// This is the one gate in the design that is re-checked after a signing round
// and deliberately does not change the decision, which is the opposite of how
// the armed window treats a stale reserve finding. The reason is what each
// staleness costs. A stale reserve finding means psbt_verify will refuse, so
// continuing achieves nothing. A passed horizon means the channels are lost —
// and the coins are still in an unconfirmed transaction that I-4 forbids
// replacing, so getting it confirmed is still the only way they come back.
// Withholding the publish here would spend the coins to save channels that are
// already gone.
func reportRace(ctx context.Context, d Deps, before *Located) string {
	after, has, err := nearestExpiry(ctx, d.LND, before.ParentTxID)
	if err != nil || !has {
		return ""
	}
	if !before.HasExpiry {
		return ""
	}

	var b strings.Builder
	switch {
	case after < 0 && before.ExpiryBlocks >= 0:
		b.WriteString("\n")
		b.WriteString(prose.Para(fmt.Sprintf(
			"The funding horizon passed while the devices were signing: %d blocks "+
				"remained when this started and it is now %d past. The peers have "+
				"very likely cancelled.", before.ExpiryBlocks, -after)))
		b.WriteString("\n")
		b.WriteString(prose.Para("The child is still being broadcast, and that is " +
			"the right call rather than a fallback. What ran out was the peers' " +
			"patience; the coins are in a transaction that cannot be replaced, so " +
			"confirming it is the only way they become spendable again. Expect to " +
			"force-close whatever opens — this node is the initiator and never " +
			"stops waiting, so a confirmation now produces channels the peers have " +
			"forgotten."))
	case after < before.ExpiryBlocks:
		b.WriteString(fmt.Sprintf("\n%d block(s) passed during the signing round; "+
			"%d remain before the first peer gives up.\n",
			before.ExpiryBlocks-after, after))
	}
	return b.String()
}

// published is the ending when the child went out.
func published(s *Signed, l *Located) string {
	var b strings.Builder
	b.WriteString("\nPublished the CPFP child.\n\n")
	b.WriteString("  child txid\n    " + s.TxID + "\n")
	b.WriteString("  parent txid\n    " + l.ParentTxID + "\n\n")
	b.WriteString(prose.Table([]prose.Row{
		prose.Note("the pair now pays", s.FeeSat+l.Parent.FeeSat,
			fmt.Sprintf("%.2f sat/vB", s.PackageRate)),
	}))
	b.WriteString("\n")
	b.WriteString(prose.Para("A miner takes the pair or neither: the child is worth " +
		"nothing without the parent it spends, which is the whole mechanism and the " +
		"reason the child was built with add_inputs off. Both transactions are in " +
		"LND's wallet-level rebroadcaster now, so it keeps offering them until they " +
		"confirm, and both are in the journal to re-send by hand if they need it."))
	b.WriteString("\n")
	b.WriteString(prose.Para("Nothing was replaced. The batch's outpoints are what " +
		"they were, so every commitment signature every peer holds is still against " +
		"the right transaction (I-4)."))
	b.WriteString("\n")
	b.WriteString(prose.Para("`winthistle run` is not what watches this — the " +
		"settlement pass ended when that command did. Watch it with Core, or run " +
		"the settlement again to pick up the confirmations and any policy that has " +
		"not landed yet."))
	return b.String()
}

// gaveUp is the teardown's report: the coin lock, and what it does not mean.
func gaveUp(rep *abort.Report, err error) string {
	var b strings.Builder
	b.WriteString("\n")

	if err != nil {
		b.WriteString(prose.Para(fmt.Sprintf(
			"Giving up on the child did not finish: %v", err)))
		b.WriteString("\n")
		b.WriteString(prose.Para("What is left is a coin lock in Core on the batch's " +
			"change output. Nothing is lost and nothing is spent — it is a wallet " +
			"that will decline to spend its own money without saying why. Core's " +
			"locks live in memory, so restarting Core clears them all at once, and " +
			"`winthistle doctor` lists them."))
		return b.String()
	}

	n := 0
	if rep != nil {
		n = len(rep.LocksFreed)
	}
	if n == 0 {
		b.WriteString(prose.Para("Nothing was left behind: no coin lock of this " +
			"child's is still held. That is the whole of a bump's teardown — there " +
			"is no shim to cancel and no channel to abandon, because a child talks " +
			"to no peer and creates nothing."))
		return b.String()
	}
	b.WriteString(prose.Para(fmt.Sprintf(
		"Released %d coin lock%s on the batch's change output, so the cold wallet "+
			"can spend it again. Nothing else was undone: no channel was touched, no "+
			"peer was told anything, and the batch is exactly where it was.",
		n, prose.Plural(n))))
	return b.String()
}
