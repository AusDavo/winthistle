// Package prose renders the operator-facing copy this tool prints.
//
// It exists so that every screen wraps to the same column and renders a
// satoshi amount the same way. The reports are read in a terminal pane, often
// beside something else, and often at the moment the operator is deciding
// whether to bring a cold wallet out — so a report that overruns its pane, or
// that renders one figure as BTC and the next as satoshis, is a real hazard and
// not a cosmetic one.
//
// Deliberately dumb: greedy wrapping, no hyphenation, no terminal width
// detection. A fixed column keeps the arithmetic tables and the prose lining up
// in the same pane, which is the property that matters when a human is checking
// a subtraction.
package prose

import (
	"fmt"
	"strings"
)

// The copy is written to a fixed column: narrow enough to survive a half-screen
// terminal, which is where this actually gets read. Prose wraps at ProseWidth;
// PaneWidth is the hard limit, and the arithmetic tables are allowed to use the
// extra room because a wrapped subtraction is harder to check than a wide one.
const (
	ProseWidth = 70
	PaneWidth  = 78
)

// Row is one line of an arithmetic table.
type Row struct {
	Label  string
	Amount int64
	Note   string
}

// Line is a table row with no annotation.
func Line(label string, amount int64) Row { return Row{Label: label, Amount: amount} }

// Note is a table row with an annotation, which Table places beside the figure
// or underneath it depending on what fits.
func Note(label string, amount int64, note string) Row {
	return Row{Label: label, Amount: amount, Note: note}
}

// Table renders the arithmetic with the amounts right-aligned, because the
// operator is reading it to check a subtraction.
//
// A note that would push the line past the pane goes underneath instead.
// Amounts here span single satoshis to whole bitcoin, so the widths are not
// knowable when the copy is written.
func Table(rows []Row) string {
	labelWidth, amountWidth := 0, 0
	for _, r := range rows {
		if n := len(r.Label); n > labelWidth {
			labelWidth = n
		}
		if n := len(Sats(r.Amount)); n > amountWidth {
			amountWidth = n
		}
	}
	var b strings.Builder
	for _, r := range rows {
		line := fmt.Sprintf("  %-*s  %*s", labelWidth, r.Label, amountWidth, Sats(r.Amount))
		if r.Note == "" {
			b.WriteString(line + "\n")
			continue
		}
		note := "(" + r.Note + ")"
		if len([]rune(line))+3+len([]rune(note)) <= PaneWidth {
			b.WriteString(line + "   " + note + "\n")
			continue
		}
		b.WriteString(line + "\n")
		b.WriteString(Wrap(note, "      ", "      "))
	}
	return b.String()
}

// Bullet wraps one instruction to a readable width, hanging-indented under its
// dash.
func Bullet(text string) string { return Wrap(text, "  - ", "    ") }

// Para wraps a paragraph flush left.
func Para(text string) string { return Wrap(text, "", "") }

// Indent wraps a paragraph under a fixed indent.
func Indent(text, prefix string) string { return Wrap(text, prefix, prefix) }

// Wrap is greedy and deliberately dumb; see the package comment.
func Wrap(text, first, rest string) string {
	const width = ProseWidth

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

// Sats renders an amount the way an operator checks it: grouped, and with the
// unit, so a figure can never be mistaken for BTC.
func Sats(n int64) string {
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

// BTC renders satoshis as a BTC amount, as text, because a float would be wrong
// here for the same reason it is wrong everywhere else in Bitcoin. Core parses
// this without loss.
func BTC(sat int64) string {
	neg := ""
	if sat < 0 {
		neg, sat = "-", -sat
	}
	return fmt.Sprintf("%s%d.%08d", neg, sat/1e8, sat%1e8)
}

// StockLNDNote is the attribution a screen owes when it has quoted one of LND's
// own configuration defaults as a peer's behaviour.
//
// Both figures this build quotes about a peer are defaults rather than protocol
// constants. The funding horizon is lncfg.DefaultMaxWaitNumBlocksFundingConf,
// 2016 blocks, and the hold on a cancelled reservation is
// chanfunding.DefaultReservationTimeout plus lncfg.DefaultZombieSweeperInterval,
// ten minutes and one. Neither is adjustable in a release build of LND, and
// neither binds a peer that is running something else, a different version, or a
// non-default flag — and no RPC reports either figure to an initiator, so this
// build cannot know which case it is in.
//
// Called once per screen, not once per number. internal/settle's horizonNote is
// the model — it says of the same 2016 that "it binds a peer running stock LND
// and nobody else" — and a screen that hedged under every figure would be noise
// rather than attribution. The counts themselves stay: recovery copy names clock
// B in blocks, and that rule is untouched.
//
// Both false renders nothing, so a caller may pass what its own branches
// actually printed.
func StockLNDNote(horizon, hold bool) string {
	var what string
	switch {
	case horizon && hold:
		what = "The 2016 blocks and the eleven minutes above are"
	case horizon:
		what = "The 2016 blocks above are"
	case hold:
		what = "The eleven minutes above are"
	default:
		return ""
	}
	return Para(what + " LND's own defaults, not the protocol's. They bind a " +
		"peer running stock LND and nobody else: another implementation, another " +
		"version, or a peer that changed the flag gives up somewhere else, and " +
		"no RPC reports either figure to the side that opened the channel.")
}

// IsAre and WasWere keep the reports grammatical when a count is one.
func IsAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

// WasWere is IsAre in the past tense.
func WasWere(n int) string {
	if n == 1 {
		return "was"
	}
	return "were"
}

// Plural returns "" for one and "s" for anything else.
func Plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// Stage is how far a run has got, for the one line that says what stopping
// costs.
//
// Four values, because the answer changes exactly three times in a run and each
// change is a thing the operator has to understand. The boundaries are the
// facts, not the headings: a stage's line is printed at the moment the thing
// that makes it true has happened, which is why StageArmed is not step 6's
// heading but the sentence after its receipts arrived.
//
// No LND default appears in any of these. The eleven minutes and the 2016
// blocks live on the screens that own them — step 2's note, step 7's, and the
// recovery copy — and StockLNDNote attributes them there. A line that repeated
// either figure on four more screens would owe four more attributions, which is
// how a rule that exists to stop noise becomes the noise.
type Stage int

const (
	// StageNothingAsked: before any funding stream exists.
	StageNothingAsked Stage = iota

	// StageStreamsOpen: the shims are registered and no channel has reached
	// chan_pending. Steps 2 to 5.
	StageStreamsOpen

	// StageArmed: n receipts, nothing signed and nothing broadcast. Step 6 on.
	StageArmed

	// StagePublished: step 8 was taken.
	StagePublished
)

// StoppingHere is the standing line under a run's screens: what it costs to
// stop, right now.
//
// n is the count the line is about — streams at StageStreamsOpen, channels at
// StageArmed — and is ignored where the line names no count.
func StoppingHere(s Stage, n int) string {
	switch s {
	case StageNothingAsked:
		return Para("If you stop here: nothing. No peer has been asked for " +
			"anything, and this node has done nothing it would have to undo.")

	case StageStreamsOpen:
		return Para(fmt.Sprintf("If you stop here: the batch is taken apart for "+
			"you. Nothing has been broadcast and no channel has reached "+
			"chan_pending, so the teardown cancels the %d shim%s and abandons "+
			"anything LND created behind them. Each peer holds the slot it "+
			"reserved until its own sweeper releases it.", n, Plural(n)))

	case StageArmed:
		return Para(fmt.Sprintf("If you stop here: the batch is abandoned. %d "+
			"channel%s already recoverable by force-close, and nothing has been "+
			"broadcast — but abandoning is local and tells the peer nothing, so "+
			"each peer keeps its side pending and goes on holding one of its own "+
			"slots.", n, isAreChannels(n)))

	case StagePublished:
		return Para("If you stop here: nothing stops. The transaction is public " +
			"and every channel in it is recoverable, so closing this program does " +
			"not recall it — and winthistle recover refuses to abort a run that " +
			"reached the publish.")
	}

	// Not silence. An unhandled Stage is this program failing to say what a
	// stop would cost, at the one moment that question is worth answering, and
	// a line that rendered as nothing would look like a screen where stopping
	// was free. It says which of the two it is and asserts nothing about the
	// batch, because it knows nothing about the batch.
	return Para("If you stop here: this program cannot say. That is a defect in " +
		"it and not a state of your batch — read the screens above, and treat " +
		"winthistle recover as the authority on what is still standing.")
}

// isAreChannels keeps "1 channel is" and "2 channels are" grammatical.
func isAreChannels(n int) string {
	if n == 1 {
		return " is"
	}
	return "s are"
}
