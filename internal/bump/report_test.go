package bump_test

import (
	"strings"
	"testing"

	"github.com/AusDavo/winthistle/internal/bitcoind"
	"github.com/AusDavo/winthistle/internal/bump"
	"github.com/AusDavo/winthistle/internal/plan"
	"github.com/AusDavo/winthistle/internal/prose"
	"github.com/AusDavo/winthistle/internal/settle"
)

const reportTxID = "5c1e9a3f7b2d8046a1c3e5f70981b2d4c6e8f0a2b4c6d8e0f2a4b6c8d0e2f401"

func located(expiry int32, hasExpiry bool, entry bitcoind.MempoolEntry) *bump.Located {
	return &bump.Located{
		RunID:      "20260823-120000-abc123",
		ParentTxID: reportTxID,
		Parent: settle.Parent{
			TxID:      reportTxID,
			VsizeVB:   7_007,
			FeeSat:    7_007,
			Change:    plan.Outpoint{TxID: reportTxID, Vout: 3},
			ChangeSat: 400_000,
		},
		Entry:        entry,
		ExpiryBlocks: expiry,
		HasExpiry:    hasExpiry,
	}
}

func plainEntry() bitcoind.MempoolEntry {
	return bitcoind.MempoolEntry{
		VsizeVB: 7_007, FeeSat: 7_007,
		AncestorCount: 1, AncestorVsizeVB: 7_007, AncestorFeeSat: 7_007,
		DescendantCount: 1,
	}
}

// TestThePastHorizonScreenSaysTheChannelsAreGoneAndTheChildIsStillWorthIt is the
// one place in this design where a signing round races something that does not
// wait, so the copy has to hold two things at once: the channels are lost, and
// the transaction is still worth confirming.
//
// Getting either half wrong is a real cost. Saying only "the horizon has passed"
// reads as "do not bother", and the coins are then stuck in a transaction I-4
// forbids replacing. Saying only "build the child" hides that the operator is
// about to own channels their peers have forgotten.
func TestThePastHorizonScreenSaysTheChannelsAreGoneAndTheChildIsStillWorthIt(t *testing.T) {
	got := located(-7, true, plainEntry()).Report()

	mustSay(t, got, "the channel responder has very likely cancelled the funding")
	mustSay(t, got, "A child is still worth building")
	mustSay(t, got, "What expired is the peers' patience, not this transaction")
	mustSay(t, got, "confirming it is the only way they ever become spendable again")
	mustSay(t, got, "Expect to force-close whatever opens")
	mustSay(t, got, "Never replace the transaction instead")
}

// An urgent horizon has to say what the clock is, and that the signing round is
// racing it — including in wall clock, because that is the unit an operator
// deciding whether to fetch two devices from two places is working in.
func TestTheUrgentHorizonScreenPricesTheSigningRound(t *testing.T) {
	got := located(100, true, plainEntry()).Report()

	mustSay(t, got, "roughly a day")
	mustSay(t, got, "the clock the signing round below is racing")
	mustSay(t, got, "16 hours")
	mustSay(t, got, "Nothing here enforces it")
}

// A distant horizon must not manufacture urgency. This is the ordinary case —
// somebody bumping because fees moved, not because a deadline is close — and the
// copy says which.
func TestADistantHorizonDoesNotManufactureUrgency(t *testing.T) {
	got := located(1500, true, plainEntry()).Report()

	mustSay(t, got, "a long way off")
	mustSay(t, got, "about the fee market rather than about a deadline")
	mustNotSay(t, got, "Act now")
	mustNotSay(t, got, "roughly a day")
}

// The report names where its figures came from, because the answer is not
// obvious and the wrong answer is invisible: a fee summed from the inputs could
// disagree with Core's and nothing would say so.
func TestTheReportSaysWhoseFiguresTheseAre(t *testing.T) {
	got := located(1500, true, plainEntry()).Report()

	mustSay(t, got, "Both of those figures are Core's")
	mustSay(t, got, "out of its own mempool entry")
	mustSay(t, got, "The change output is the only output of a batch the cold "+
		"wallet can see")
}

// A parent with unconfirmed ancestors of its own is the case where the obvious
// arithmetic is wrong, so the report has to say that the figures above it are
// the ancestor figures and show the transaction's own.
func TestAParentWithUnconfirmedAncestorsSaysSo(t *testing.T) {
	entry := plainEntry()
	entry.AncestorCount = 3
	entry.AncestorVsizeVB = 9_500
	entry.AncestorFeeSat = 9_500

	l := located(1500, true, entry)
	l.Parent.VsizeVB = entry.AncestorVsizeVB
	l.Parent.FeeSat = entry.AncestorFeeSat

	got := l.Report()
	mustSay(t, got, "the *ancestor* figures, and that is deliberate")
	mustSay(t, got, "not the bottom of its own package")
	mustSay(t, got, "lifts the whole unconfirmed ancestor package")
}

// With no pending channel there is no countdown, and inventing one would be
// worse than saying so.
func TestNoPendingChannelMeansNoCountdown(t *testing.T) {
	got := located(0, false, plainEntry()).Report()
	mustSay(t, got, "no funding countdown to report")
	mustNotSay(t, got, "blocks before")
}

// TestTheCeilingAdviceNamesBothWaysOut is the finding that changes what this
// command can promise, so the copy has to be usable rather than merely accurate.
func TestTheCeilingAdviceNamesBothWaysOut(t *testing.T) {
	f := newFixture(t, 250)
	vsize := f.childVsize(t)
	fee := f.feeFor(t, vsize)
	f.exp.Parent.ChangeSat = fee + 100_000

	raw, exp := f.build(t, childSpec{outputSat: 100_000})
	exp.Parent.ChangeSat = f.exp.Parent.ChangeSat
	exp.FeeSat = fee
	raw = withInputValue(t, raw, f.exp.Parent.ChangeSat, f.w.pkScript)

	v, err := bump.Verify(raw, exp)
	if err != nil {
		t.Fatalf("verifying: %v", err)
	}
	got := v.Report("what was built")

	mustSay(t, got, "Nothing is wrong with the child")
	mustSay(t, got, "a valid transaction that this node will not relay")
	mustSay(t, got, "maxfeerate fixed at 0.10 BTC/kvB")
	mustSay(t, got, "Ask for a smaller lift")
	mustSay(t, got, "sendrawtransaction <hex> 0")
	mustSay(t, got, "not a way around anything")
	mustSay(t, got, "gives up is LND's wallet-level rebroadcaster")
}

// The two verification passes have to be distinguishable on the page, because
// only one of them is a measurement — and an operator reading "150 vB" needs to
// know whether that is a bound or a fact.
func TestTheTwoVerificationPassesSayWhichIsMeasured(t *testing.T) {
	f := newFixture(t, 20)
	raw, exp, vsize := f.correct(t)

	v, err := bump.Verify(raw, exp)
	if err != nil {
		t.Fatalf("verifying: %v", err)
	}
	built := v.Report("what was built")
	mustSay(t, built, "an upper bound")
	mustSay(t, built, "so the rates above are floors rather than estimates")

	final := signChild(t, f, raw)
	view, err := final.View()
	if err != nil {
		t.Fatalf("View: %v", err)
	}
	rv, err := bump.Recheck(view, exp, vsize)
	if err != nil {
		t.Fatalf("rechecking: %v", err)
	}
	back := rv.Report("what came back")
	mustSay(t, back, "measured")
	mustSay(t, back, "the rates are the rates this transaction will actually pay")
}

// Every screen this command prints is read in the same pane as the reserve, plan
// and settlement reports, so it wraps to the same column. An overrun is a hazard
// rather than a blemish: the figures stop lining up with the prose.
func TestTheBumpScreensStayInThePane(t *testing.T) {
	f := newFixture(t, 20)
	raw, exp, vsize := f.correct(t)
	v, err := bump.Verify(raw, exp)
	if err != nil {
		t.Fatalf("verifying: %v", err)
	}
	final := signChild(t, f, raw)
	view, err := final.View()
	if err != nil {
		t.Fatalf("View: %v", err)
	}
	rv, err := bump.Recheck(view, exp, vsize)
	if err != nil {
		t.Fatalf("rechecking: %v", err)
	}

	// A refused child too, since the problem list is the widest thing here.
	bad, badExp := f.build(t, childSpec{outputSat: plan.DustSat - 1})
	refused, err := bump.Verify(bad, badExp)
	if err != nil {
		t.Fatalf("verifying the bad child: %v", err)
	}

	screens := map[string]string{
		"past horizon":     located(-7, true, plainEntry()).Report(),
		"urgent horizon":   located(100, true, plainEntry()).Report(),
		"warned horizon":   located(300, true, plainEntry()).Report(),
		"distant horizon":  located(1500, true, plainEntry()).Report(),
		"no horizon":       located(0, false, plainEntry()).Report(),
		"with ancestors":   ancestorReport(),
		"verified built":   v.Report("what was built"),
		"verified back":    rv.Report("what came back"),
		"verified refused": refused.Report("what was built"),
	}
	for name, text := range screens {
		for i, line := range strings.Split(text, "\n") {
			if n := len([]rune(line)); n > prose.PaneWidth {
				t.Errorf("%s line %d is %d columns, over the %d-column pane:\n%s",
					name, i+1, n, prose.PaneWidth, line)
			}
		}
	}
}

func ancestorReport() string {
	entry := plainEntry()
	entry.AncestorCount = 3
	entry.AncestorVsizeVB = 9_500
	entry.AncestorFeeSat = 9_500
	l := located(1500, true, entry)
	l.Parent.VsizeVB = entry.AncestorVsizeVB
	l.Parent.FeeSat = entry.AncestorFeeSat
	return l.Report()
}

// flat collapses whitespace, so a test can assert about the words rather than
// about where the wrap fell.
func flat(s string) string { return strings.Join(strings.Fields(s), " ") }

func mustSay(t *testing.T, got, want string) {
	t.Helper()
	if !strings.Contains(flat(got), flat(want)) {
		t.Errorf("the screen does not say %q:\n%s", want, got)
	}
}

func mustNotSay(t *testing.T, got, unwanted string) {
	t.Helper()
	if strings.Contains(flat(got), flat(unwanted)) {
		t.Errorf("the screen says %q and should not:\n%s", unwanted, got)
	}
}
