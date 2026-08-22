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
