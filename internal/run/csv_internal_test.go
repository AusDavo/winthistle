package run

import "testing"

// The CSV's bytes, exactly, because every failure this format guards against is
// a failure of one character.
//
// Sparrow reads the amount column in whatever unit the operator's preference is
// set to and cannot be told which we meant, so a regression here is not a
// cosmetic one: a sat integer where a BTC decimal belongs loads 10^8 too large
// and says nothing. A grouping separator in the amount either converges on the
// same wrong value (quoted) or shifts the columns (unquoted). An unquoted label
// containing a comma shifts them too. None of those look wrong on screen.
//
// So this asserts the whole file rather than a property of it. There is one
// deliberately awkward row per hazard: an alias with a comma in it, an alias with
// a quote in it, an amount that is not a round number of BTC, and an amount that
// is a whole one.
func TestTheRecipientsCSVIsExactlyThis(t *testing.T) {
	got := string(recipientsCSV([]Recipient{
		{
			Label:     "channel 1  ACINQ, Inc.",
			Address:   "bcrt1qh0st4ln4l0mmc4ck7wkw2s6a3lstwph4h4uu2xuprvnyfnk3ntvsw6chnx",
			AmountSat: 250_000,
		},
		{
			Label:     `channel 2  the "best" node`,
			Address:   "bcrt1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsxqqqqqqqqqqqqqqqhxwmz3",
			AmountSat: 1_234_567,
		},
		{
			Label:     "anchor reserve",
			Address:   "bcrt1q9d4ywgfnd8h43da5tpcxcn6ajv590cg6d3tg6axemvljvt2k76zs0eyayr",
			AmountSat: 100_000_000,
		},
	}))

	want := "address,amount_btc,label\n" +
		"bcrt1qh0st4ln4l0mmc4ck7wkw2s6a3lstwph4h4uu2xuprvnyfnk3ntvsw6chnx," +
		"0.00250000,\"channel 1  ACINQ, Inc.\"\n" +
		"bcrt1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsxqqqqqqqqqqqqqqqhxwmz3," +
		"0.01234567,\"channel 2  the \"\"best\"\" node\"\n" +
		"bcrt1q9d4ywgfnd8h43da5tpcxcn6ajv590cg6d3tg6axemvljvt2k76zs0eyayr," +
		"1.00000000,\"anchor reserve\"\n"

	if got != want {
		t.Errorf("the recipients CSV has changed.\n got:\n%s\nwant:\n%s", got, want)
	}
}

// TestTheAmountColumnIsBTCAndNeverSats, at the edges where an eight-place
// decimal is easiest to get wrong.
//
// These pin the format, not the arithmetic. float64(sat)/1e8 with %.8f was tried
// against every case below and agrees with all of them — int64 sats are inside
// the 53-bit mantissa, so it would take a supply cap several orders of magnitude
// higher to separate the two. The integer form is here because it is exact
// without that argument having to be made, and these cases are the ones where a
// wrong format string — a missing zero pad, an %e for the large one, a truncated
// remainder — shows up.
func TestTheAmountColumnIsBTCAndNeverSats(t *testing.T) {
	for _, c := range []struct {
		sat  int64
		want string
	}{
		{0, "0.00000000"},
		{1, "0.00000001"},
		{99_999_999, "0.99999999"},
		{100_000_000, "1.00000000"},
		{100_000_001, "1.00000001"},
		{250_000, "0.00250000"},
		{16_777_217, "0.16777217"},
		{2_100_000_000_000_000, "21000000.00000000"},
	} {
		if got := btcAmount(c.sat); got != c.want {
			t.Errorf("btcAmount(%d) is %q, want %q", c.sat, got, c.want)
		}
	}
}

// TestNoRowEverCarriesAGroupingSeparatorOrABareLineBreak.
//
// The grouping separator is the one the step-4 table prints and this file must
// not: 250,000 unquoted shifts the columns, and quoted it is stripped to the same
// wrong value rather than refused. The line break is the alias LND's gossip could
// hand us — a row that spans two lines is a row Sparrow reads as two.
func TestNoRowEverCarriesAGroupingSeparatorOrABareLineBreak(t *testing.T) {
	got := string(recipientsCSV([]Recipient{{
		Label:     "channel 1  two\nlines\rand a carriage return",
		Address:   "bcrt1qexample",
		AmountSat: 250_000,
	}}))
	want := "address,amount_btc,label\n" +
		"bcrt1qexample,0.00250000,\"channel 1  two lines and a carriage return\"\n"
	if got != want {
		t.Errorf("got:\n%q\nwant:\n%q", got, want)
	}
}
