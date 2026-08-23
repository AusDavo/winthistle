package fees_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/AusDavo/winthistle/internal/fees"
	"github.com/AusDavo/winthistle/internal/regtestenv"
)

// TestRegtestHasNoFeeEstimateAndTheFloorCarriesIt is the case every regtest run
// and every fresh node is in, and it is worth pinning against a real Core rather
// than a fake one.
//
// estimatesmartfee does not fail. It succeeds, returns no feerate field at all,
// and puts "Insufficient data or no feerate found" in an errors array. A client
// that treated the absent field as a rate would build the batch at zero sat/vB;
// one that treated the errors array as an RPC failure would refuse to build a
// batch on a node that is working perfectly well.
//
// Note what the harness's -fallbackfee does NOT do here: it is a wallet setting,
// consulted by Core's own coin selection, and estimatesmartfee never looks at it.
func TestRegtestHasNoFeeEstimateAndTheFloorCarriesIt(t *testing.T) {
	env := regtestenv.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	r, err := fees.Estimate(ctx, env.Node, fees.Request{FloorSatPerVB: 10})
	if err != nil {
		t.Fatalf("asking Core for a fee rate: %v", err)
	}
	t.Logf("%s", r.Summary())
	t.Logf("\n%s", r.Report())

	if r.Estimated() {
		t.Skipf("this regtest node has a fee estimate (%g sat/vB), which means it "+
			"has block history this test assumed it would not", r.EstimateSatPerVB)
	}
	if !strings.Contains(r.CoreSaid, "Insufficient data") {
		t.Errorf("Core's own reason was lost: %q", r.CoreSaid)
	}
	if r.Source != fees.FromConfiguredFloor {
		t.Errorf("source = %v, want the configured floor", r.Source)
	}
	if r.SatPerVB != 10 {
		t.Errorf("rate = %g sat/vB, want the floor of 10", r.SatPerVB)
	}

	// The relay floor is read from the same node and is a real number: Core's
	// default minrelaytxfee of 0.00001 BTC/kvB is 1 sat/vB.
	if r.RelayFloorSatPerVB <= 0 {
		t.Errorf("relay floor = %g sat/vB; Core always has one", r.RelayFloorSatPerVB)
	}
}

// With no floor configured there is no honest answer, and the refusal has to be
// an error rather than a default. I-4 is the reason: a rate chosen badly cannot
// be corrected by replacing the transaction.
func TestOnRegtestWithNoFloorThereIsNoRate(t *testing.T) {
	env := regtestenv.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	r, err := fees.Estimate(ctx, env.Node, fees.Request{})
	if err == nil {
		t.Fatalf("a rate of %g sat/vB was produced with nothing behind it", r.SatPerVB)
	}
	t.Logf("refused, correctly: %v", err)
}

// The fee rate the plan is verified against has to be the one the transaction
// was built at, and the CPFP target has to be derived from it — the change
// output is sized against that target, and on a node with no estimate the rate
// is a floor rather than a market figure.
func TestTheRateFlowsIntoThePlansFee(t *testing.T) {
	env := regtestenv.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	r, err := fees.Estimate(ctx, env.Node, fees.Request{
		FloorSatPerVB: 10,
		CPFPMultiple:  4,
	})
	if err != nil {
		t.Fatalf("asking Core for a fee rate: %v", err)
	}
	f := r.Fee()
	if f.TargetSatPerVB != r.SatPerVB {
		t.Errorf("the plan's target is %g and the rate is %g", f.TargetSatPerVB, r.SatPerVB)
	}
	if f.CPFPTargetSatPerVB != r.SatPerVB*4 {
		t.Errorf("CPFP target = %g, want %g", f.CPFPTargetSatPerVB, r.SatPerVB*4)
	}
	if f.Low() >= f.TargetSatPerVB || f.High() <= f.TargetSatPerVB {
		t.Errorf("the tolerance band does not bracket the target: %g..%g around %g",
			f.Low(), f.High(), f.TargetSatPerVB)
	}
}
