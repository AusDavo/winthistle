package doctor

import (
	"fmt"
	"strings"

	"github.com/AusDavo/winthistle/internal/prose"
)

// Report renders the whole pre-flight.
//
// One block per check, in the order they were made, and the fix underneath the
// finding it fixes rather than collected at the end. A list of commands at the
// bottom of a report is a list the operator has to match back up to the
// problems, and this is read at a terminal by somebody who wants to paste one
// line and try again.
func (r *Report) Report() string {
	var b strings.Builder

	failed, warned := 0, 0
	for _, c := range r.Checks {
		switch c.Status {
		case Fail:
			failed++
		case Warn:
			warned++
		}
	}

	switch {
	case failed > 0:
		fmt.Fprintf(&b, "Not ready: %d check%s failed", failed, prose.Plural(failed))
	case warned > 0:
		fmt.Fprintf(&b, "Ready, with %d thing%s worth reading", warned, prose.Plural(warned))
	default:
		b.WriteString("Ready")
	}
	fmt.Fprintf(&b, " — %d checks\n\n", len(r.Checks))

	for _, c := range r.Checks {
		fmt.Fprintf(&b, "%-5s %s\n", c.Status, c.Name)
		for _, line := range c.Lines {
			// A line that is already indented is a list item the check laid out
			// itself — an outpoint, a method path — and must not be re-wrapped.
			if strings.HasPrefix(line, "    ") {
				b.WriteString("  " + line + "\n")
				continue
			}
			b.WriteString(prose.Wrap(line, "      ", "      "))
		}
		// A command is printed whole, at the shallowest indent that still reads
		// as part of the block, and it is the one thing here allowed past the
		// pane: it has to be pasteable, and a shell command cannot be wrapped
		// without changing it. A long path is a long line.
		for _, fix := range c.Fix {
			b.WriteString("\n")
			for _, line := range strings.Split(fix, "\n") {
				b.WriteString("  $ " + line + "\n")
			}
		}
		b.WriteString("\n")
	}

	if failed > 0 {
		b.WriteString(prose.Para("Every failure above has a command under it. " +
			"Nothing here has been changed on your behalf: doctor reads, and the " +
			"one thing it creates is the journal file, because a journal that does " +
			"not exist yet is not a fault."))
	}
	return b.String()
}

// Summary is the one line a log or a status pane wants.
func (r *Report) Summary() string {
	failed, warned := 0, 0
	for _, c := range r.Checks {
		switch c.Status {
		case Fail:
			failed++
		case Warn:
			warned++
		}
	}
	return fmt.Sprintf("%d checks: %d failed, %d warned", len(r.Checks), failed, warned)
}
