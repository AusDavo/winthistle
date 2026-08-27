package prose

import "strings"

import "testing"

func TestSats(t *testing.T) {
	cases := map[int64]string{
		0:           "0 sat",
		1:           "1 sat",
		999:         "999 sat",
		1_000:       "1,000 sat",
		10_000:      "10,000 sat",
		100_000:     "100,000 sat",
		1_234_567:   "1,234,567 sat",
		-10_000:     "-10,000 sat",
		500_000_000: "500,000,000 sat",
	}
	for in, want := range cases {
		if got := Sats(in); got != want {
			t.Errorf("Sats(%d) = %q, want %q", in, got, want)
		}
	}
}

// TestBTC. Core parses these, so a rounding error here is a wrong amount in a
// funding output rather than a cosmetic defect.
func TestBTC(t *testing.T) {
	cases := map[int64]string{
		0:               "0.00000000",
		1:               "0.00000001",
		100_000_000:     "1.00000000",
		2_100_000_000:   "21.00000000",
		1_234_567:       "0.01234567",
		-1:              "-0.00000001",
		500_000_000_000: "5000.00000000",
	}
	for in, want := range cases {
		if got := BTC(in); got != want {
			t.Errorf("BTC(%d) = %q, want %q", in, got, want)
		}
	}
}

// TestWrapKeepsWholeWords. A greedy wrapper that splits a txid or an address in
// half produces copy an operator cannot compare against a device screen, which
// is the one job the round-trip check has.
func TestWrapKeepsWholeWords(t *testing.T) {
	long := strings.Repeat("bcrt1qexample ", 12)
	out := Wrap(long, "  - ", "    ")
	for i, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if n := len([]rune(line)); n > ProseWidth {
			t.Errorf("line %d is %d columns: %q", i+1, n, line)
		}
		for _, w := range strings.Fields(line) {
			if w != "-" && w != "bcrt1qexample" {
				t.Errorf("line %d split a word: %q", i+1, w)
			}
		}
	}
}

// TestTableStaysInThePane. A note long enough to overrun goes under its row
// rather than off the side.
func TestTableStaysInThePane(t *testing.T) {
	out := Table([]Row{
		{"required at verify", 30_000, "a note long enough that it cannot possibly " +
			"share a line with the figure it is annotating"},
		{"unlocked and available", 1_000_000, "short"},
	})
	for i, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if n := len([]rune(line)); n > PaneWidth {
			t.Errorf("line %d is %d columns: %q", i+1, n, line)
		}
	}
	if !strings.Contains(out, "  30,000 sat") {
		t.Errorf("amounts are not right-aligned against the widest:\n%s", out)
	}
}

// stages is every Stage StoppingHere declares, keyed by the identifier rather
// than by the number, so a stage that is renamed or removed fails to compile
// instead of going quiet.
var stages = map[string]Stage{
	"StageNothingAsked": StageNothingAsked,
	"StageStreamsOpen":  StageStreamsOpen,
	"StageArmed":        StageArmed,
	"StagePublished":    StagePublished,
}

// TestEveryStageSaysSomethingDifferent.
//
// The line is the one place a run says what stopping costs, and the whole
// design is that its answer changes at each boundary. Two stages rendering the
// same sentence would be a boundary that says nothing, which is the defect this
// asserts against rather than a cosmetic repeat.
func TestEveryStageSaysSomethingDifferent(t *testing.T) {
	seen := map[string]string{}
	for name, s := range stages {
		got := StoppingHere(s, 2)
		if strings.TrimSpace(got) == "" {
			t.Errorf("%s renders nothing", name)
			continue
		}
		if !strings.Contains(got, "If you stop here:") {
			t.Errorf("%s does not open with the standing lead-in:\n%s", name, got)
		}
		if prev, dup := seen[got]; dup {
			t.Errorf("%s renders the same line as %s, so the boundary between "+
				"them tells the operator nothing:\n%s", name, prev, got)
		}
		seen[got] = name
	}
}

// TestNoStageLineRepeatsAnLNDDefault.
//
// StockLNDNote attributes the eleven minutes and the 2016 blocks once per
// screen, on the screens that name them. StoppingHere prints on four more
// screens, so a figure that leaked into it would owe an attribution on each —
// and the rule exists to stop noise, not to license it.
func TestNoStageLineRepeatsAnLNDDefault(t *testing.T) {
	for name, s := range stages {
		got := StoppingHere(s, 2)
		for _, figure := range []string{"2016", "eleven minutes", "ten minutes"} {
			if strings.Contains(got, figure) {
				t.Errorf("%s names %q, which is an LND default and needs "+
					"StockLNDNote beside it:\n%s", name, figure, got)
			}
		}
	}
}

// TestAnUnknownStageSaysSoRatherThanNothing.
//
// Go's zero value is a valid Stage, but nothing stops a caller reaching this
// with a value no case handles. A switch that fell through to "" would render
// as a screen with no standing line — indistinguishable from a screen where
// stopping is free, which is the reading that could cost a batch. The default
// has to be loud, and it may not assert anything about the batch, because at
// that point it knows nothing about the batch.
func TestAnUnknownStageSaysSoRatherThanNothing(t *testing.T) {
	got := StoppingHere(Stage(99), 2)
	if strings.TrimSpace(got) == "" {
		t.Fatal("an unknown Stage renders nothing, which reads as a screen " +
			"where stopping costs nothing")
	}
	if !strings.Contains(got, "cannot say") {
		t.Errorf("an unknown Stage does not say that it cannot say:\n%s", got)
	}
	for _, claim := range []string{"recoverable", "broadcast", "abandoned"} {
		if strings.Contains(got, claim) {
			t.Errorf("the unknown-Stage line claims %q about a batch it knows "+
				"nothing about:\n%s", claim, got)
		}
	}
}

// TestTheStageLineCountsGrammatically. One channel is, two channels are.
//
// Compared against the unwrapped line, because Para wraps to ProseWidth and a
// phrase this test is about can have a newline through the middle of it. The
// grammar is the assertion; where the wrap lands is not.
func TestTheStageLineCountsGrammatically(t *testing.T) {
	cases := []struct {
		stage Stage
		n     int
		want  string
	}{
		{StageArmed, 1, "1 channel is already recoverable"},
		{StageArmed, 2, "2 channels are already recoverable"},
		{StageStreamsOpen, 1, "cancels the 1 shim and"},
		{StageStreamsOpen, 3, "cancels the 3 shims and"},
	}
	for _, c := range cases {
		got := unwrapped(StoppingHere(c.stage, c.n))
		if !strings.Contains(got, c.want) {
			t.Errorf("StoppingHere(%d, %d) does not say %q:\n%s",
				c.stage, c.n, c.want, got)
		}
	}
}

// unwrapped collapses a wrapped paragraph back to one line.
func unwrapped(s string) string { return strings.Join(strings.Fields(s), " ") }
