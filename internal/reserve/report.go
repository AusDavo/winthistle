package reserve

import (
	"fmt"
	"strings"

	"github.com/AusDavo/winthistle/internal/prose"
)

// Summary is the one line a list or a log wants.
func (f Finding) Summary() string {
	switch f.Verdict() {
	case WouldBeRefused:
		return fmt.Sprintf("the node's own wallet is %s short of the reserve "+
			"psbt_verify will demand", prose.Sats(f.ShortfallAtVerify()))
	case ShortAfterBatch:
		return fmt.Sprintf("the batch will verify, but it leaves the node's own "+
			"wallet %s under the reserve for %d pending channels",
			prose.Sats(f.ShortfallAfterBatch()), f.Batch.Public)
	case NotApplicable:
		return "every channel in this batch is private, so LND's anchor reserve " +
			"check does not run"
	default:
		return fmt.Sprintf("the node's own wallet clears the anchor reserve, at "+
			"verify and after the batch (%s available, %s needed)",
			prose.Sats(f.Available), prose.Sats(f.AfterBatch))
	}
}

// Report is the operator-facing text.
//
// It is written to LND's own numbers and against LND's own wording. The error the
// operator would otherwise see — "reserved wallet balance invalidated: transaction
// would leave insufficient funds for fee bumping anchor channel closings" — names
// no cause, no figure, and no wallet, and its most natural reading is that
// something is wrong with the money that was just brought out of cold storage.
// Nothing is. Saying so plainly, with the arithmetic, is the whole job here.
func (f Finding) Report() string {
	switch f.Verdict() {
	case WouldBeRefused:
		return f.reportRefused()
	case ShortAfterBatch:
		return f.reportShortAfter()
	case NotApplicable:
		return f.reportNotApplicable()
	default:
		return f.reportClear()
	}
}

func (f Finding) reportRefused() string {
	var b strings.Builder

	b.WriteString("Stop here, before the cold wallet comes out.\n\n")
	b.WriteString("This node's own on-chain wallet is short of the reserve LND\n")
	b.WriteString("requires, and psbt_verify will refuse the batch at step 5 —\n")
	b.WriteString("with every signer already waiting and every peer's window open.\n\n")

	rows := []prose.Row{
		prose.Line("LND will require at verify", f.AtVerify),
		prose.Note("unlocked and available", f.Available, "the node's own coins"),
		prose.Line("short by", f.ShortfallAtVerify()),
	}
	if f.Leased > 0 {
		rows = append(rows, prose.Note("leased and not counted", f.Leased,
			"LND skips leased coins"))
	}
	b.WriteString(prose.Table(rows))

	b.WriteString("\nThe cold wallet is not the problem, and neither are the signers or\n")
	b.WriteString("the plan. LND keeps a reserve in its own wallet so that it can\n")
	b.WriteString("fee-bump a force-close, and it re-checks that reserve inside\n")
	b.WriteString("psbt_verify. The batch's coins cannot help: they belong to the\n")
	b.WriteString("cold wallet, and LND counts only what it can sign for itself.\n")

	b.WriteString("\nLeft as it is, step 5 fails with\n\n")
	b.WriteString("    reserved wallet balance invalidated: transaction would leave\n")
	b.WriteString("    insufficient funds for fee bumping anchor channel closings\n\n")
	b.WriteString("which names none of the above. The real figures appear only in\n")
	b.WriteString("LND's own debug log, on the node.\n")

	target := f.AtVerify
	if f.AfterBatch > target {
		target = f.AfterBatch
	}

	b.WriteString("\nWhat to do:\n")
	b.WriteString(prose.Bullet(fmt.Sprintf(
		"Add a top-up output paying this node's own wallet to the batch. " +
			"CheckReservedValue credits outputs that pay into the wallet, so a " +
			"top-up inside the batch counts at step 5 with no extra transaction " +
			"and no wait.")))
	b.WriteString(prose.Bullet(fmt.Sprintf(
		"Or send %s to the node's on-chain wallet from anywhere. It counts as "+
			"soon as it is in the mempool — LND reads this balance at zero "+
			"confirmations.", prose.Sats(target))))
	if f.Leased > 0 {
		b.WriteString(prose.Bullet(fmt.Sprintf(
			"Or free the %s leased above by taking down the funding attempts "+
				"holding it. That is what the abort path is for; leases are "+
				"released as their shims are cancelled.", prose.Sats(f.Leased))))
	}

	if f.AfterBatch > f.AtVerify {
		b.WriteString("\n")
		b.WriteString(prose.Para(fmt.Sprintf("Aim at %s rather than %s. Verify needs the "+
			"smaller figure, because none of the batch is in the channel database "+
			"yet; once all %d are pending the node wants the larger one.",
			prose.Sats(f.AfterBatch), prose.Sats(f.AtVerify), f.Batch.Public)))
	}
	b.WriteString(privateNote(f.Batch))
	return b.String()
}

func (f Finding) reportShortAfter() string {
	var b strings.Builder

	b.WriteString("This batch will verify. It leaves the node's own wallet under\n")
	b.WriteString("the reserve, though, and now is the cheapest moment to fix that.\n\n")

	rows := []prose.Row{
		prose.Note("required at verify", f.AtVerify, "met"),
		prose.Line(fmt.Sprintf("required with %d pending", f.Batch.Public), f.AfterBatch),
		prose.Line("unlocked and available", f.Available),
		prose.Line("short by, afterwards", f.ShortfallAfterBatch()),
	}
	if f.Leased > 0 {
		rows = append(rows, prose.Note("leased and not counted", f.Leased,
			"in flight elsewhere"))
	}
	b.WriteString(prose.Table(rows))

	b.WriteString("\nNothing will refuse the batch. What changes is afterwards: below\n")
	b.WriteString("the reserve LND declines further on-chain spends and public\n")
	b.WriteString("channel opens from this wallet, and there is less on hand to\n")
	b.WriteString("fee-bump a force-close of these channels than LND has decided\n")
	b.WriteString("there should be.\n")

	b.WriteString("\nWhat to do:\n")
	b.WriteString(prose.Bullet(fmt.Sprintf(
		"Add a top-up output for %s to the batch, which costs one output's "+
			"worth of fee and nothing else.", prose.Sats(f.ShortfallAfterBatch()))))
	b.WriteString(prose.Bullet(
		"Or top the node's wallet up separately, any time before the channels " +
			"go to chain."))
	b.WriteString(privateNote(f.Batch))
	return b.String()
}

func (f Finding) reportClear() string {
	var b strings.Builder
	b.WriteString("The node's own on-chain wallet clears LND's anchor reserve, at\n")
	b.WriteString("verify and after the batch.\n\n")
	b.WriteString(prose.Table([]prose.Row{
		prose.Line("required at verify", f.AtVerify),
		prose.Line(fmt.Sprintf("required with %d pending", f.Batch.Public), f.AfterBatch),
		prose.Line("unlocked and available", f.Available),
	}))
	if f.Leased > 0 {
		b.WriteString(fmt.Sprintf("\n%s is leased to work already in flight and does not "+
			"count towards\nthe figures above.\n", prose.Sats(f.Leased)))
	}
	b.WriteString(privateNote(f.Batch))
	return b.String()
}

func (f Finding) reportNotApplicable() string {
	var b strings.Builder
	b.WriteString("Every channel in this batch is private, so LND's anchor reserve\n")
	b.WriteString("check does not run: enforceNewReservedValue returns early for an\n")
	b.WriteString("unannounced channel, and a private channel is not counted when the\n")
	b.WriteString("reserve is worked out either.\n\n")
	b.WriteString(prose.Table([]prose.Row{
		prose.Note("the node's reserve, unchanged", f.NowRequired, "channels it already has"),
		prose.Line("unlocked and available", f.Available),
	}))
	b.WriteString("\nNothing to clear. If any channel in the batch is announced after\n")
	b.WriteString("all, run this again — the answer changes.\n")
	return b.String()
}

// privateNote says out loud that the private members were seen and skipped, so
// that a batch of 5 reported against a figure for 2 does not read as a bug.
func privateNote(b Batch) string {
	if b.Private == 0 {
		return ""
	}
	return "\n" + prose.Para(fmt.Sprintf("%d of the %d channels in this batch %s private, "+
		"and %s not counted above: the reserve is worked out from announced "+
		"channels only.", b.Private, b.Total(), prose.IsAre(b.Private), prose.WasWere(b.Private)))
}
