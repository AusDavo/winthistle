package rehearsal

import (
	"fmt"
	"strings"
	"time"

	"github.com/AusDavo/winthistle/internal/prose"
)

// Summary is the one line a log or a status pane wants.
func (m *Measurement) Summary() string {
	if m == nil {
		return "no rehearsal has been run"
	}
	if n := m.failures(); n > 0 {
		return fmt.Sprintf("%d of %d signer%s failed", n, len(m.Rounds),
			prose.Plural(len(m.Rounds)))
	}
	if !m.Accepted {
		return "the mempool would have refused the rehearsal transaction: " + m.RejectReason
	}
	verdict := "under the gate"
	if m.Signing > m.Limit {
		verdict = "OVER the gate"
	}
	return fmt.Sprintf("signing round %s, %s of %s",
		round(m.Signing), verdict, round(m.Limit))
}

// Report is the operator-facing text.
//
// The two things it has to get across are what was measured and what it is being
// compared against — because the number on its own reads as a stopwatch reading
// and it is in fact the decision about whether the batch may be armed at all.
func (m *Measurement) Report() string {
	if m == nil {
		return prose.Para("No rehearsal has been run.")
	}

	var b strings.Builder

	b.WriteString("Dress rehearsal — a decoy transaction, signed for real and\n")
	b.WriteString("thrown away. Nothing was broadcast and nothing changed hands.\n\n")

	b.WriteString(prose.Table([]prose.Row{
		prose.Note("mirrored the batch's outputs", m.Decoy.OutputSat,
			fmt.Sprintf("%d output%s, paid back to the cold wallet",
				m.Decoy.Outputs, prose.Plural(m.Decoy.Outputs))),
		prose.Line("fee", m.Decoy.FeeSat),
	}))
	b.WriteString(fmt.Sprintf("  %-28s  %d input(s), %d vB\n", "spent",
		len(m.Decoy.Inputs), m.Decoy.VsizeVB))
	// On its own line: a txid is 64 characters and the pane is 78, so a label
	// beside it cannot fit and abbreviating it would make it useless.
	b.WriteString("  decoy txid, for the log\n    " + m.Decoy.TxID + "\n")

	b.WriteString("\nSigning round, device by device:\n\n")
	for _, r := range m.Rounds {
		status := round(r.Elapsed).String()
		if r.Err != nil {
			status = "FAILED — " + r.Err.Error()
		}
		b.WriteString(fmt.Sprintf("  %-16s %s\n", r.Label, status))
	}

	b.WriteString("\n")
	b.WriteString(fmt.Sprintf("  %-28s  %s\n", "measured signing round", round(m.Signing)))
	b.WriteString(fmt.Sprintf("  %-28s  %s\n", "the abort gate", round(m.Limit)))
	b.WriteString(fmt.Sprintf("  %-28s  %s\n", "the peers' window", round(PeerWindow)))
	b.WriteString(fmt.Sprintf("  %-28s  %s\n", "left of it after signing",
		round(m.Headroom())))
	b.WriteString(fmt.Sprintf("  %-28s  %s\n", "whole rehearsal", round(m.Total)))

	b.WriteString("\n")
	switch {
	case m.failures() > 0:
		b.WriteString(prose.Para(
			"At least one device did not return a usable partial signature. That is " +
				"one of the two failures this phase exists to catch, and catching " +
				"it here costs nothing: no stream is open, no peer is waiting, and " +
				"the batch has not been armed."))
	case !m.Accepted:
		b.WriteString(prose.Para(fmt.Sprintf(
			"The signatures combined and finalized, and then testmempoolaccept "+
				"refused the result: %q. testmempoolaccept validates without "+
				"relaying, so nothing went out — but the real batch would have "+
				"been refused the same way, after the signing round, with the "+
				"peers' windows open.", m.RejectReason)))
	case m.Signing > m.Limit:
		b.WriteString(prose.Para(fmt.Sprintf(
			"This batch will not be armed. The signing round took %s and the gate "+
				"is %s.", round(m.Signing), round(m.Limit))))
		b.WriteString("\n")
		if worst, ok := m.Slowest(); ok {
			b.WriteString(prose.Bullet(fmt.Sprintf(
				"%s took %s, the longest of the %d. That is the one to do "+
					"something about.", worst.Label, round(worst.Elapsed),
				len(m.Rounds))))
		}
		b.WriteString(prose.Bullet(
			"Or open fewer channels per transaction. Fewer outputs is less for " +
				"each device to display and confirm, and the sequential fallback — " +
				"one channel per transaction — completes at any signing speed, at " +
				"the cost of more fees and no atomicity."))
	default:
		b.WriteString(prose.Para(fmt.Sprintf(
			"Cleared. A signing round of %s leaves %s of a peer's ten minutes for "+
				"everything else in the window: the build, n psbt_verify calls, the "+
				"merge, n psbt_finalize calls each waiting for its own chan_pending, "+
				"and the channel backup export.",
			round(m.Signing), round(m.Headroom()))))
	}

	b.WriteString("\n")
	b.WriteString(prose.Para(
		"What this did not test: it opened no funding stream, which is what makes " +
			"it free and repeatable and also why it cannot vouch for LND's funding " +
			"flow. Signers, descriptors and fee arithmetic are what it proves."))
	return b.String()
}

// round keeps a duration readable in a sentence a human is reading under time
// pressure, without rounding the measurement away.
//
// Whole seconds for anything a minute or longer, because that is the register
// the gate is argued in. Milliseconds below a second, because a signing round
// against a scripted signer really does take single-digit milliseconds and
// printing it as "0s" reads as "not measured" rather than as "instant".
func round(d time.Duration) time.Duration {
	switch {
	case d >= time.Minute:
		return d.Round(time.Second)
	case d >= time.Second:
		return d.Round(100 * time.Millisecond)
	default:
		return d.Round(time.Millisecond)
	}
}
