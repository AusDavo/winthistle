package plan

import (
	"fmt"
	"strings"

	"github.com/AusDavo/winthistle/internal/policy"
	"github.com/AusDavo/winthistle/internal/prose"
)

// Document is the batch plan as the operator reads it: what to build, exactly.
//
// It is the reviewable artifact the design asks for. Everything in it is a
// constraint the verifier then checks, with one exception that is called out in
// the text — the confirmation floor, which cannot be read back out of a PSBT.
func (p *Plan) Document() string {
	var b strings.Builder

	named, err := p.Outputs()
	if err != nil {
		return "This plan is not usable: " + err.Error() + "\n"
	}

	b.WriteString(fmt.Sprintf("Batch plan — %d channel%s on %s\n\n",
		len(p.Channels), prose.Plural(len(p.Channels)), p.Chain))
	b.WriteString(prose.Para("Build one transaction with exactly the outputs below. " +
		"Every amount is exact to the satoshi: LND compares its own funding output " +
		"including its value, so a rounded amount is a channel that never opens."))
	b.WriteString("\n")

	// Only when there is an alias to be misled by.
	if anyAlias(p.Channels) {
		b.WriteString(prose.Para("Peer names below come from this node's gossip " +
			"graph and are whatever each node says about itself: not unique, not " +
			"verified by anybody, and two nodes may claim the same one. The key " +
			"beside each name is the identifier. The name is there to make the key " +
			"readable, not to stand in for it."))
		b.WriteString("\n")
	}

	// The address goes on its own line. A bech32 P2WSH address is 62 characters
	// and a taproot one 62 too, so anything sharing a line with one overruns the
	// pane — and this is copy the operator reads to check an output, not a string
	// a program consumes.
	funding := 0
	for _, n := range named {
		if n.Kind == ChangeOut {
			continue
		}
		label := n.Label
		if label == "" {
			label = n.Kind.String()
		}
		b.WriteString("  " + label + "\n")
		b.WriteString("      " + n.Address + "\n")
		b.WriteString("      " + prose.Sats(n.AmountSat) + "\n")
		// Outputs sorts by kind and is stable, so the funding outputs arrive in
		// channel order — which is what lets the policy sit beside the amount it
		// belongs to rather than in a table somewhere else.
		if n.Kind == Funding && funding < len(p.Channels) {
			b.WriteString(policyLines(p.Channels[funding]))
			funding++
		}
		b.WriteString("\n")
	}

	b.WriteString(prose.Table([]prose.Row{
		prose.Note("total to pay out", p.TotalOutSat(), "before fee and change"),
	}))

	if p.TopUp != nil {
		b.WriteString("\n")
		b.WriteString(prose.Para(fmt.Sprintf(
			"The reserve top-up is not part of any channel. It pays %s to this "+
				"node's own on-chain wallet, because LND re-checks its anchor reserve "+
				"inside psbt_verify and this node is %s short of it. An output inside "+
				"this transaction counts: CheckReservedValue credits outputs paying an "+
				"address the wallet owns, so no second transaction and no wait.",
			prose.Sats(p.TopUp.AmountSat), prose.Sats(p.TopUp.Shortfall))))
	}

	b.WriteString("\nChange\n")
	switch {
	case p.Change.Address != "":
		b.WriteString(prose.Bullet("Send change to this address:"))
		b.WriteString("      " + p.Change.Address + "\n")
	default:
		b.WriteString(prose.Bullet("Change goes back to the cold wallet's own change " +
			"branch, which is what your wallet software does by default. The " +
			"returned transaction is checked for the key origin information that " +
			"proves the change output is yours."))
	}
	b.WriteString(prose.Bullet(
		"A change output is worth having. This batch can never be replaced " +
			"(I-4) — replacing it moves every outpoint and destroys every channel " +
			"in it — so a CPFP child spending the change is the only lever there " +
			"will ever be on it. Nothing is at risk without one; what is missing " +
			"is the lever. How big it needs to be is a question about the fee " +
			"market when you build the child, which is yours and not this " +
			"program's: step 5 says what your change came out as and nothing " +
			"grades it."))

	b.WriteString("\nFee and replacement\n")
	b.WriteString(prose.Bullet("The fee is yours. This app does not build the " +
		"transaction and does not choose the rate, so it names no target and holds " +
		"you to none — step 5 reports what the transaction you built came out at, " +
		"and that is the whole of it. There is no RBF here (I-4), so pick the rate " +
		"against the mempool you can see when you build."))
	b.WriteString(prose.Bullet("Replace-by-fee off — in Sparrow, the RBF toggle on " +
		"the transaction. Nothing here reads the sequence numbers. Core relays a " +
		"higher-fee conflict whatever they signal, so the flag is a statement of " +
		"intent rather than a defence; what keeps this transaction from being " +
		"replaced is that only you can sign its inputs, and no code path in this " +
		"program replaces one (I-4)."))

	b.WriteString("\nInputs\n")
	b.WriteString(prose.Bullet("SegWit only. LND rejects a funding transaction with " +
		"any legacy input outright, for malleability — a malleable input is a TXID " +
		"that can move after LND has committed to it (I-3)."))
	if p.Inputs.MinConfirmations > 0 {
		b.WriteString(prose.Bullet(fmt.Sprintf(
			"Confirmed: at least %d confirmation%s. This is the one constraint here "+
				"that cannot be read back out of a PSBT, so it is on you.",
			p.Inputs.MinConfirmations, prose.Plural(p.Inputs.MinConfirmations))))
	}
	if n := len(p.Inputs.Allowed); n > 0 {
		b.WriteString(prose.Bullet(fmt.Sprintf(
			"Spend only these %d coin%s:", n, prose.Plural(n))))
		for _, op := range p.Inputs.Allowed {
			b.WriteString("      " + op.String() + "\n")
		}
	}
	if n := len(p.Inputs.Excluded); n > 0 {
		b.WriteString(prose.Bullet(fmt.Sprintf(
			"%d coin%s left out of this batch, so your wallet's balance and this "+
				"plan will disagree:", n, prose.Plural(n))))
		for _, e := range p.Inputs.Excluded {
			b.WriteString(prose.Wrap(e, "      ", "        "))
		}
	}
	return b.String()
}

func anyAlias(chans []Channel) bool {
	for _, ch := range chans {
		if ch.Alias != "" {
			return true
		}
	}
	return false
}

// policyLines is the forwarding policy shown beside a channel's amount.
//
// It is here rather than in a table of its own because it is the same decision:
// the operator is looking at what this channel costs, and this is what it will
// earn. After this document is approved nobody is asked again — Phase 2 applies
// the policy in a loop with no human in it.
func policyLines(ch Channel) string {
	var b strings.Builder
	switch {
	case ch.Private && ch.Policy == nil:
		b.WriteString("      unannounced, so it routes for nobody and its " +
			"policy is moot\n")
	case ch.Private:
		b.WriteString("      unannounced — " + ch.Policy.Summary() + "\n")
		b.WriteString(prose.Wrap("An unannounced channel is not in the graph, so "+
			"nobody will route through it and this policy only applies to whatever "+
			"the peer sends directly.", "      ", "      "))
	case ch.Policy == nil:
		b.WriteString(prose.Wrap(fmt.Sprintf(
			"No forwarding policy chosen, so this channel will route at LND's "+
				"defaults — %d msat base and %d ppm, CLTV delta %d — from the moment "+
				"it goes active, and stay there. On a channel this size that is close "+
				"to free routing for whoever notices first.",
			policy.DefaultBaseFeeMsat, policy.DefaultFeeRatePPM,
			policy.DefaultTimeLockDelta), "      ", "      "))
	case ch.Policy.IsLNDDefault():
		b.WriteString("      policy " + ch.Policy.Summary() + "\n")
		b.WriteString(prose.Wrap("That is exactly what LND would have used anyway, "+
			"so the policy pass has nothing to win here.", "      ", "      "))
	default:
		b.WriteString("      policy " + ch.Policy.Summary() + "\n")
	}
	return b.String()
}

// Report is what the operator sees after handing a transaction back.
func (v *Verification) Report() string {
	var b strings.Builder

	// Wrapped rather than written straight out: the count and the reported-count
	// clause together outran the pane the day the second was added.
	if v.OK() {
		head := "The transaction matches the plan."
		switch n := len(v.Reports); {
		case n == 1:
			head += " One thing below is reported and not refused over."
		case n > 1:
			head += fmt.Sprintf(" %d things below are reported and not refused over.", n)
		}
		b.WriteString(prose.Para(head))
		b.WriteString("\n")
	} else {
		verb := "does not"
		if len(v.Problems) != 1 {
			verb = "do not"
		}
		b.WriteString(prose.Para(fmt.Sprintf("Do not sign this. %d thing%s about "+
			"this transaction %s match the plan.",
			len(v.Problems), prose.Plural(len(v.Problems)), verb)))
		b.WriteString("\n")
	}

	b.WriteString(fmt.Sprintf("  txid   %s\n", v.UnsignedTxID))
	b.WriteString(fmt.Sprintf("  size   %d vB\n", v.Size.Vsize))
	if v.Size.Estimated {
		// Its own line rather than trailing the size. Beside it, this note came
		// to 79 characters against a 78-column pane — found by rendering the
		// screen in a browser, not by a test, because this was the one report
		// package with no pane test. Trailing it also made the width depend on
		// the vsize's digit count, which a big batch grows.
		b.WriteString("         (estimated, and an upper bound — so the rate " +
			"below is a floor)\n")
	}
	b.WriteString("\n")

	b.WriteString(prose.Table([]prose.Row{
		prose.Note("inputs", v.InputSat, fmt.Sprintf("%d", len(v.Inputs))),
		prose.Note("outputs", v.OutputSat, fmt.Sprintf("%d", len(v.Outputs))),
		prose.Note("fee", v.FeeSat, fmt.Sprintf("%.2f sat/vB", v.FeeRate)),
	}))

	b.WriteString("\nOutputs\n")
	for _, a := range v.Outputs {
		what := "NOT IN THE PLAN"
		switch {
		case a.Recognised:
			what = a.Label
		case a.Named:
			what = a.Label
		}
		b.WriteString(fmt.Sprintf("  %d  %14s   %s\n", a.Index, prose.Sats(a.AmountSat), what))
		b.WriteString("      " + a.Address + "\n")
		// On its own line, because the label plus a bech32m address plus this
		// clause is 88 columns against a 78-column pane — and it stopped being a
		// rare line the day the app stopped building the transaction. Recognition
		// is now the ordinary way the change output is identified, so the sentence
		// that says how weak that claim is has to fit.
		if a.Recognised {
			b.WriteString("      recognised by key origin rather than by address\n")
		}
	}

	if v.ChangeSat > 0 {
		b.WriteString("\n")
		b.WriteString(prose.Table([]prose.Row{prose.Line("change", v.ChangeSat)}))
	}

	if len(v.Problems) > 0 {
		b.WriteString("\nWhat is wrong\n")
		for _, p := range v.Problems {
			head := p.Headline
			if p.Where != "" {
				head = p.Where + " — " + p.Headline
			}
			b.WriteString(prose.Wrap(head, "  - ", "    "))
			if p.Detail != "" {
				b.WriteString(prose.Wrap(p.Detail, "    ", "    "))
			}
		}
	}

	b.WriteString(v.Reported())

	if len(v.Unchecked) > 0 {
		b.WriteString("\nNot checked here\n")
		for _, u := range v.Unchecked {
			b.WriteString(prose.Bullet(u))
		}
	}
	return b.String()
}

// Reported renders the findings the verifier established and does not refuse
// over, or "" when there are none.
//
// Its own heading, and deliberately not "Not checked here": these were checked,
// and the answer is on the screen. It is separate from Report() because step 7's
// recheck prints its findings only on failure — a report that nobody prints is
// worse than the refusal it replaced, so the caller that stays quiet about
// problems still has something to print these with.
func (v *Verification) Reported() string {
	if len(v.Reports) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nReported, not refused\n")
	for _, r := range v.Reports {
		head := r.Headline
		if r.Where != "" {
			head = r.Where + " — " + r.Headline
		}
		b.WriteString(prose.Wrap(head, "  - ", "    "))
		if r.Detail != "" {
			b.WriteString(prose.Wrap(r.Detail, "    ", "    "))
		}
	}
	return b.String()
}

// Summary is the one line a log wants.
func (v *Verification) Summary() string {
	// The reported findings ride along on both branches. A log that recorded only
	// refusals would say "matches the plan" about a batch with no change output,
	// which is the one thing the demotion must not cost.
	var reported string
	if len(v.Reports) > 0 {
		codes := make([]string, 0, len(v.Reports))
		for _, r := range v.Reports {
			codes = append(codes, r.Code.String())
		}
		reported = fmt.Sprintf(" (reported, not refused: %s)", strings.Join(codes, "; "))
	}

	if v.OK() {
		return fmt.Sprintf("%s matches the plan: %d input(s), %d output(s), %d sat "+
			"fee at %.2f sat/vB%s", v.UnsignedTxID, len(v.Inputs), len(v.Outputs),
			v.FeeSat, v.FeeRate, reported)
	}
	codes := make([]string, 0, len(v.Problems))
	for _, p := range v.Problems {
		codes = append(codes, p.Code.String())
	}
	return fmt.Sprintf("%s does not match the plan: %s%s", v.UnsignedTxID,
		strings.Join(codes, "; "), reported)
}
