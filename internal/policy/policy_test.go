package policy

import (
	"strings"
	"testing"
)

// TestLNDsDefaultsAreRecognisedAsSuch.
//
// A channel with no policy is not neutral: it is these three numbers, from the
// moment it goes active, and on a large channel 1 ppm is close to free routing
// for whoever notices first. So the plan document has to be able to say "that
// is what LND would have used anyway" rather than showing three figures and
// leaving the operator to recognise them.
func TestLNDsDefaultsAreRecognisedAsSuch(t *testing.T) {
	lnd := Policy{
		BaseFeeMsat:   DefaultBaseFeeMsat,
		FeeRatePPM:    DefaultFeeRatePPM,
		TimeLockDelta: DefaultTimeLockDelta,
	}
	if !lnd.IsLNDDefault() {
		t.Error("LND's own defaults are not recognised as LND's own defaults")
	}
	chosen := lnd
	chosen.FeeRatePPM = 250
	if chosen.IsLNDDefault() {
		t.Error("a chosen policy is being reported as LND's default")
	}
	// A max HTLC is a policy decision even when the fees match.
	withMax := lnd
	withMax.MaxHTLCMsat = 1_000_000
	if withMax.IsLNDDefault() {
		t.Error("a max-HTLC limit is being ignored when deciding what is default")
	}
}

// TestValidateRefusesWhatLNDRefuses pins that Validate enforces the bounds, not
// that the bounds are LND's — every case here is keyed on this package's own
// constant, so it would pass against any value. What the constants are is
// TestTranscribedConstantsMatchLND's question, and it reads LND to answer it.
func TestValidateRefusesWhatLNDRefuses(t *testing.T) {
	ok := Policy{TimeLockDelta: MinTimeLockDelta}
	if err := ok.Validate(); err != nil {
		t.Errorf("routing.MinCLTVDelta itself was refused: %v", err)
	}

	bad := map[string]Policy{
		"a delta below routing.MinCLTVDelta": {TimeLockDelta: MinTimeLockDelta - 1},
		"a delta above routing.MaxCLTVDelta": {TimeLockDelta: MaxTimeLockDelta + 1},
		"a negative base fee": {
			TimeLockDelta: MinTimeLockDelta, BaseFeeMsat: -1,
		},
		// LND refuses a positive inbound fee outright rather than clamping it,
		// unless the node runs with --accept-positive-inbound-fees.
		"a positive inbound fee": {
			TimeLockDelta: MinTimeLockDelta, InboundFeeRatePPM: 1,
		},
	}
	for name, p := range bad {
		if err := p.Validate(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// TestSummarySaysTheDeltaToo. The CLTV delta is the one figure here that can be
// refused outright, and the operator reviewing the plan is the last person who
// can change it for free.
func TestSummarySaysTheDeltaToo(t *testing.T) {
	min := int64(1000)
	p := Policy{
		BaseFeeMsat: 0, FeeRatePPM: 250, TimeLockDelta: 144,
		MinHTLCMsat: &min, MaxHTLCMsat: 990_000_000,
	}
	s := p.Summary()
	for _, want := range []string{"0 msat base", "250 ppm", "CLTV delta 144",
		"min HTLC 1000 msat", "max HTLC 990000000 msat"} {

		if !strings.Contains(s, want) {
			t.Errorf("the summary does not say %q: %s", want, s)
		}
	}
}
