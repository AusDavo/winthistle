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
