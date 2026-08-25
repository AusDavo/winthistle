package run

import (
	"errors"
	"fmt"
	"strings"

	"github.com/AusDavo/winthistle/internal/plan"
	"github.com/AusDavo/winthistle/internal/prose"
)

// feeFor is the rate the batch is expected to be built at.
//
// Declared, not fetched, and that is the answer item 5 of docs/replan-2026-08.md
// had to give. It came from Core's estimatesmartfee until Core was removed, and
// CLAUDE.md's rule against a third party rules out the obvious substitute: a fee
// API is handed the size of what is being built and the moment it is being
// built, which together are most of what this tool exists not to leak.
//
// The three options on the table were to ask the operator, to derive something
// from LND's own relay floor, or to drop the fee finding entirely. Asking wins,
// and not only by elimination. This program does not build the transaction and
// does not choose the fee — Sparrow does, at step 4, with whatever estimate the
// operator trusts — so what the verifier needs is not a fee estimate at all. It
// needs something to hold the built transaction to, and "the rate you told me
// you were aiming at" is a stronger thing to check against than a number this
// program looked up on the operator's behalf. It is also how everything else
// here works: you declare the batch, it checks the transaction.
//
// What was not on the table is a rate that quietly becomes zero. config.Load
// refuses a file with no target_sat_per_vb and plan.Build refuses a plan whose
// target is not positive, so there are two independent refusals between a
// missing number and a batch built against nothing.
func feeFor(o Options) (plan.Fee, error) {
	rate := o.Config.Fees.TargetSatPerVB
	if o.FeeRateSatPerVB > 0 {
		rate = o.FeeRateSatPerVB
	}
	if rate <= 0 {
		return plan.Fee{}, errors.New("no fee rate: set [fees] target_sat_per_vb " +
			"in winthistle.toml, or pass --fee-rate, to the rate in sat/vB you mean " +
			"to build this batch at. Nothing here will guess one")
	}
	return plan.Fee{TargetSatPerVB: rate}, nil
}

// feeReport says what the number is, where it came from, and what it is for.
//
// Provenance is the point, as it was when Core answered this: a fee rate is the
// one figure in a batch that has no right answer, and I-4 means the decision
// cannot be revisited once the transaction is out. What changed is that the
// provenance is now always the same — the operator — so the report says what
// that does and does not buy them.
func feeReport(f plan.Fee, o Options) string {
	var b strings.Builder
	source := "winthistle.toml"
	if o.FeeRateSatPerVB > 0 {
		source = "--fee-rate"
	}
	fmt.Fprintf(&b, "Fee rate: %.2f sat/vB, from %s.\n\n", f.TargetSatPerVB, source)

	b.WriteString(prose.Para(fmt.Sprintf(
		"This tool does not choose the fee and does not ask anything what it "+
			"should be. You pick the fee in Sparrow at step 4; step 5 checks that "+
			"what you built pays between %.2f and %.2f sat/vB, which is the number "+
			"above and a quarter either side of it. A mis-typed fee is the one "+
			"arithmetic error in a batch that costs money and cannot be undone.",
		f.Low(), f.High())))
	b.WriteString("\n")
	b.WriteString(prose.Para(
		"Bitcoin Core's estimatesmartfee used to answer this and no longer does. " +
			"Nothing replaced it, on purpose: a fee API is handed the size of what " +
			"is being built and the moment it is being built, which together are " +
			"most of what this tool exists not to leak. Your own wallet or mempool " +
			"can tell you the number; this program will not go and ask."))
	b.WriteString("\n")
	b.WriteString(prose.Para(fmt.Sprintf(
		"There is no RBF on this transaction (I-4), so the rate is not revisable. "+
			"If it turns out too low the remedy is a CPFP child spending the change "+
			"output, built in your own wallet, and step 5 says whether your change "+
			"is big enough to keep one viable at %.2f sat/vB — it says so rather "+
			"than refusing, because the change is yours.",
		f.CPFPTarget())))
	return b.String()
}
