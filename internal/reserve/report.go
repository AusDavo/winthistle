package reserve

import (
	"fmt"
	"strings"
)

// Summary is the one line a list or a log wants.
func (f Finding) Summary() string {
	switch f.Verdict() {
	case WouldBeRefused:
		return fmt.Sprintf("the node's own wallet is %s short of the reserve "+
			"psbt_verify will demand", sats(f.ShortfallAtVerify()))
	case ShortAfterBatch:
		return fmt.Sprintf("the batch will verify, but it leaves the node's own "+
			"wallet %s under the reserve for %d pending channels",
			sats(f.ShortfallAfterBatch()), f.Batch.Public)
	case NotApplicable:
		return "every channel in this batch is private, so LND's anchor reserve " +
			"check does not run"
	default:
		return fmt.Sprintf("the node's own wallet clears the anchor reserve, at "+
			"verify and after the batch (%s available, %s needed)",
			sats(f.Available), sats(f.AfterBatch))
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

	rows := []row{
		{"LND will require at verify", f.AtVerify, ""},
		{"unlocked and available", f.Available, "the node's own coins"},
		{"short by", f.ShortfallAtVerify(), ""},
	}
	if f.Leased > 0 {
		rows = append(rows, row{"leased and not counted", f.Leased,
			"LND skips leased coins"})
	}
	b.WriteString(table(rows))

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
	b.WriteString(bullet(fmt.Sprintf(
		"Add a top-up output paying this node's own wallet to the batch. " +
			"CheckReservedValue credits outputs that pay into the wallet, so a " +
			"top-up inside the batch counts at step 5 with no extra transaction " +
			"and no wait.")))
	b.WriteString(bullet(fmt.Sprintf(
		"Or send %s to the node's on-chain wallet from anywhere. It counts as "+
			"soon as it is in the mempool — LND reads this balance at zero "+
			"confirmations.", sats(target))))
	if f.Leased > 0 {
		b.WriteString(bullet(fmt.Sprintf(
			"Or free the %s leased above by taking down the funding attempts "+
				"holding it. That is what the abort path is for; leases are "+
				"released as their shims are cancelled.", sats(f.Leased))))
	}

	if f.AfterBatch > f.AtVerify {
		b.WriteString("\n")
		b.WriteString(para(fmt.Sprintf("Aim at %s rather than %s. Verify needs the "+
			"smaller figure, because none of the batch is in the channel database "+
			"yet; once all %d are pending the node wants the larger one.",
			sats(f.AfterBatch), sats(f.AtVerify), f.Batch.Public)))
	}
	b.WriteString(privateNote(f.Batch))
	return b.String()
}

func (f Finding) reportShortAfter() string {
	var b strings.Builder

	b.WriteString("This batch will verify. It leaves the node's own wallet under\n")
	b.WriteString("the reserve, though, and now is the cheapest moment to fix that.\n\n")

	rows := []row{
		{"required at verify", f.AtVerify, "met"},
		{fmt.Sprintf("required with %d pending", f.Batch.Public), f.AfterBatch, ""},
		{"unlocked and available", f.Available, ""},
		{"short by, afterwards", f.ShortfallAfterBatch(), ""},
	}
	if f.Leased > 0 {
		rows = append(rows, row{"leased and not counted", f.Leased,
			"in flight elsewhere"})
	}
	b.WriteString(table(rows))

	b.WriteString("\nNothing will refuse the batch. What changes is afterwards: below\n")
	b.WriteString("the reserve LND declines further on-chain spends and public\n")
	b.WriteString("channel opens from this wallet, and there is less on hand to\n")
	b.WriteString("fee-bump a force-close of these channels than LND has decided\n")
	b.WriteString("there should be.\n")

	b.WriteString("\nWhat to do:\n")
	b.WriteString(bullet(fmt.Sprintf(
		"Add a top-up output for %s to the batch, which costs one output's "+
			"worth of fee and nothing else.", sats(f.ShortfallAfterBatch()))))
	b.WriteString(bullet(
		"Or top the node's wallet up separately, any time before the channels " +
			"go to chain."))
	b.WriteString(privateNote(f.Batch))
	return b.String()
}

func (f Finding) reportClear() string {
	var b strings.Builder
	b.WriteString("The node's own on-chain wallet clears LND's anchor reserve, at\n")
	b.WriteString("verify and after the batch.\n\n")
	b.WriteString(table([]row{
		{"required at verify", f.AtVerify, ""},
		{fmt.Sprintf("required with %d pending", f.Batch.Public), f.AfterBatch, ""},
		{"unlocked and available", f.Available, ""},
	}))
	if f.Leased > 0 {
		b.WriteString(fmt.Sprintf("\n%s is leased to work already in flight and does not "+
			"count towards\nthe figures above.\n", sats(f.Leased)))
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
	b.WriteString(table([]row{
		{"the node's reserve, unchanged", f.NowRequired, "channels it already has"},
		{"unlocked and available", f.Available, ""},
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
	return "\n" + para(fmt.Sprintf("%d of the %d channels in this batch %s private, "+
		"and %s not counted above: the reserve is worked out from announced "+
		"channels only.", b.Private, b.Total(), isAre(b.Private), wasWere(b.Private)))
}

type row struct {
	label  string
	amount int64
	note   string
}

// table renders the arithmetic with the amounts right-aligned, because the
// operator is reading it to check a subtraction.
//
// A note that would push the line past the pane goes underneath instead. Amounts
// here span single satoshis to whole bitcoin, so the widths are not knowable when
// the copy is written.
func table(rows []row) string {
	labelWidth, amountWidth := 0, 0
	for _, r := range rows {
		if n := len(r.label); n > labelWidth {
			labelWidth = n
		}
		if n := len(sats(r.amount)); n > amountWidth {
			amountWidth = n
		}
	}
	var b strings.Builder
	for _, r := range rows {
		line := fmt.Sprintf("  %-*s  %*s", labelWidth, r.label, amountWidth, sats(r.amount))
		if r.note == "" {
			b.WriteString(line + "\n")
			continue
		}
		note := "(" + r.note + ")"
		if len([]rune(line))+3+len([]rune(note)) <= paneWidth {
			b.WriteString(line + "   " + note + "\n")
			continue
		}
		b.WriteString(line + "\n")
		b.WriteString(wrap(note, "      ", "      "))
	}
	return b.String()
}

// The copy is written to a fixed column: narrow enough to survive a half-screen
// terminal, which is where this actually gets read. Prose wraps at proseWidth;
// paneWidth is the hard limit, and the arithmetic tables are allowed to use the
// extra room because a wrapped subtraction is harder to check than a wide one.
const (
	proseWidth = 70
	paneWidth  = 78
)

// bullet wraps one instruction to a readable width, hanging-indented under its
// dash.
func bullet(text string) string { return wrap(text, "  - ", "    ") }

// para wraps a paragraph flush left.
func para(text string) string { return wrap(text, "", "") }

// wrap is greedy and deliberately dumb: this is terminal copy, and a fixed
// column keeps the arithmetic tables and the prose lining up in the same pane.
func wrap(text, first, rest string) string {
	const width = proseWidth

	var (
		b      strings.Builder
		line   = first
		filled bool
	)
	for _, w := range strings.Fields(text) {
		if filled && len(line)+1+len(w) > width {
			b.WriteString(line + "\n")
			line, filled = rest, false
		}
		if filled {
			line += " "
		}
		line += w
		filled = true
	}
	if filled {
		b.WriteString(line + "\n")
	}
	return b.String()
}

// sats renders an amount the way an operator checks it: grouped, and with the
// unit, so a figure can never be mistaken for BTC.
func sats(n int64) string {
	neg := ""
	if n < 0 {
		neg, n = "-", -n
	}
	digits := fmt.Sprintf("%d", n)
	var parts []string
	for len(digits) > 3 {
		parts = append([]string{digits[len(digits)-3:]}, parts...)
		digits = digits[:len(digits)-3]
	}
	parts = append([]string{digits}, parts...)
	return neg + strings.Join(parts, ",") + " sat"
}

func isAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

func wasWere(n int) string {
	if n == 1 {
		return "was"
	}
	return "were"
}
