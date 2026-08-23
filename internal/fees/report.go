package fees

import (
	"fmt"
	"strings"

	"github.com/AusDavo/winthistle/internal/prose"
)

// Summary is the one line a log or a status pane wants.
func (r Rate) Summary() string {
	switch r.Source {
	case FromEstimate:
		return fmt.Sprintf("%s from Core's %s estimate for %d blocks",
			rate(r.SatPerVB), strings.ToLower(r.Mode), r.AnsweredForBlocks)
	case FromConfiguredFloor:
		if r.Estimated() {
			return fmt.Sprintf("%s, the configured floor — Core estimated %s",
				rate(r.SatPerVB), rate(r.EstimateSatPerVB))
		}
		return fmt.Sprintf("%s, the configured floor — Core has no estimate", rate(r.SatPerVB))
	default:
		return fmt.Sprintf("%s, this node's relay floor — nothing below it would propagate",
			rate(r.SatPerVB))
	}
}

// Report is the operator-facing text.
//
// Its job is to make the *provenance* of the number visible. A fee rate is the
// one figure in this batch that has no right answer, and the difference between
// "the mempool says so" and "your config file says so, because the node had
// nothing to say" changes what the operator should do about it — and I-4 means
// the decision cannot be revisited once the transaction is out.
func (r Rate) Report() string {
	var b strings.Builder

	b.WriteString(fmt.Sprintf("Fee rate: %s, from %s.\n\n", rate(r.SatPerVB), r.Source))

	rows := []string{
		line("Core's estimate", estimateCell(r)),
		line("the configured floor", floorCell(r.ConfiguredFloorSatPerVB)),
		line("this node's relay floor", rate(r.RelayFloorSatPerVB)),
	}
	b.WriteString(strings.Join(rows, "\n"))
	b.WriteString("\n\n")
	b.WriteString(prose.Para(fmt.Sprintf(
		"The batch is built at the largest of the three. %d transaction%s in "+
			"this node's mempool.", r.MempoolTxs, prose.Plural(int(r.MempoolTxs)))))

	if r.AnsweredForBlocks > 0 && r.AnsweredForBlocks != r.TargetBlocks {
		b.WriteString("\n")
		b.WriteString(prose.Para(fmt.Sprintf(
			"Asked about %d blocks; Core answered about %d. It answers for the "+
				"nearest target it has data for, so the rate is for the longer wait.",
			r.TargetBlocks, r.AnsweredForBlocks)))
	}

	if !r.Estimated() {
		b.WriteString("\n")
		b.WriteString(prose.Para(fmt.Sprintf(
			"Core could not estimate: %q. That is not a fault — estimatesmartfee "+
				"needs block history it does not have on regtest, on a freshly "+
				"synced node, or on one that has been offline. The rate above is "+
				"the configured floor, and it is a number somebody chose rather "+
				"than a number the market produced.", r.CoreSaid)))
	}

	if r.Source == FromRelayFloor {
		b.WriteString("\n")
		b.WriteString(prose.Para(
			"The relay floor is the highest of the three, so this rate is the least " +
				"this node will propagate at. There is no cheaper option: below it " +
				"the transaction is not slow, it is absent."))
	}

	b.WriteString("\n")
	b.WriteString(prose.Para(fmt.Sprintf(
		"There is no RBF on this transaction (I-4), so this rate is not "+
			"revisable. If it turns out to be too low the remedy is a CPFP child "+
			"spending the change output, and the change is sized here to keep one "+
			"viable at %s.", rate(r.Fee().CPFPTargetSatPerVB))))
	return b.String()
}

// estimateCell renders Core's estimate, or says why there is none.
func estimateCell(r Rate) string {
	if r.Estimated() {
		return fmt.Sprintf("%s (%s, %d blocks)", rate(r.EstimateSatPerVB),
			strings.ToLower(r.Mode), r.AnsweredForBlocks)
	}
	if r.CoreSaid == "" {
		return "none"
	}
	return "none — " + r.CoreSaid
}

func floorCell(v float64) string {
	if v <= 0 {
		return "not configured"
	}
	return rate(v)
}

// line renders one label/value pair at the reports' usual indent.
func line(label, value string) string {
	return fmt.Sprintf("  %-24s  %s", label, value)
}

// rate renders a fee rate. Two decimal places, always, so a column of them
// lines up and a rate of 1 does not read as an integer count of something.
func rate(v float64) string { return fmt.Sprintf("%.2f sat/vB", v) }
