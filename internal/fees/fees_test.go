package fees

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AusDavo/winthistle/internal/bitcoind"
)

// fakeCore answers the two RPCs the fee source makes.
//
// A real HTTP server rather than an interface, because half of what is being
// tested is the shape of Core's JSON: estimatesmartfee *succeeds* and reports
// "Insufficient data or no feerate found" in an errors array with no feerate
// field at all, and a client that only checked for an RPC error would read that
// as a rate of zero.
type fakeCore struct {
	estimate map[string]any
	mempool  map[string]any
	asked    []string
}

func (f *fakeCore) start(t *testing.T) *bitcoind.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
			Params []any  `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decoding the request: %v", err)
			return
		}
		f.asked = append(f.asked, req.Method)

		var result any
		switch req.Method {
		case "estimatesmartfee":
			result = f.estimate
		case "getmempoolinfo":
			result = f.mempool
		default:
			t.Errorf("the fee source made an unexpected call: %s", req.Method)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"result": result, "error": nil})
	}))
	t.Cleanup(srv.Close)

	c, err := bitcoind.New(bitcoind.Config{
		Address: strings.TrimPrefix(srv.URL, "http://"),
		User:    "u", Pass: "p",
	})
	if err != nil {
		t.Fatalf("building the client: %v", err)
	}
	return c
}

// The floors, in Core's own units: 0.00001 BTC/kvB is 1 sat/vB.
func floors(minFee, relay float64) map[string]any {
	return map[string]any{
		"mempoolminfee": minFee, "minrelaytxfee": relay, "size": 12,
	}
}

func TestCoresEstimateIsUsedWhenItHasOne(t *testing.T) {
	core := (&fakeCore{
		// 0.00012345 BTC/kvB = 12.345 sat/vB, which rounds to 12.35.
		estimate: map[string]any{"feerate": 0.00012345, "blocks": 6},
		mempool:  floors(0.00001, 0.00001),
	}).start(t)

	r, err := Estimate(context.Background(), core, Request{FloorSatPerVB: 1})
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if r.Source != FromEstimate {
		t.Fatalf("source = %v, want %v", r.Source, FromEstimate)
	}
	if r.SatPerVB != 12.35 {
		t.Errorf("rate = %g sat/vB, want 12.35", r.SatPerVB)
	}
	if r.Mode != Conservative {
		t.Errorf("mode = %q; CONSERVATIVE is the default because I-4 leaves no way "+
			"to correct an underpaying transaction except a CPFP child", r.Mode)
	}
	// The plan's CPFP target has to come from the rate that was actually chosen.
	if got := r.Fee().CPFPTargetSatPerVB; got != 12.35*3 {
		t.Errorf("CPFP target = %g, want %g", got, 12.35*3)
	}
}

// Regtest is the ordinary case, not the odd one: estimatesmartfee has no block
// history to work from and says so, in a successful response.
func TestNoEstimateFallsBackToTheConfiguredFloor(t *testing.T) {
	core := (&fakeCore{
		estimate: map[string]any{
			"errors": []string{"Insufficient data or no feerate found"},
			"blocks": 6,
		},
		mempool: floors(0.00001, 0.00001),
	}).start(t)

	r, err := Estimate(context.Background(), core, Request{FloorSatPerVB: 10})
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if r.Estimated() {
		t.Fatal("Core gave no rate; the client must not read one out of an absent field")
	}
	if r.Source != FromConfiguredFloor || r.SatPerVB != 10 {
		t.Fatalf("rate = %g from %v, want 10 from the configured floor", r.SatPerVB, r.Source)
	}
	if !strings.Contains(r.CoreSaid, "Insufficient data") {
		t.Errorf("Core's own reason was dropped: %q", r.CoreSaid)
	}
	if !strings.Contains(r.Report(), "Insufficient data") {
		t.Error("the report must say the rate is a number somebody chose")
	}
}

// The one case where there is no honest answer. A guess here is not a small
// error: I-4 forbids replacing the funding transaction, so a rate chosen badly
// costs a CPFP child and another cold-wallet session.
func TestNoEstimateAndNoFloorIsAnError(t *testing.T) {
	core := (&fakeCore{
		estimate: map[string]any{
			"errors": []string{"Insufficient data or no feerate found"},
			"blocks": 6,
		},
		mempool: floors(0.00001, 0.00001),
	}).start(t)

	_, err := Estimate(context.Background(), core, Request{})
	if err == nil {
		t.Fatal("a rate was produced with nothing to base it on")
	}
	if !strings.Contains(err.Error(), "fee_floor") {
		t.Errorf("the error must name the setting that fixes it: %v", err)
	}
}

// A congested mempool raises mempoolminfee above the static relay floor, and a
// transaction below it is evicted rather than merely slow.
func TestTheRelayFloorWinsWhenItIsHighest(t *testing.T) {
	core := (&fakeCore{
		estimate: map[string]any{"feerate": 0.00000500, "blocks": 6}, // 0.5 sat/vB
		mempool:  floors(0.00002, 0.00001),                           // 2 sat/vB
	}).start(t)

	r, err := Estimate(context.Background(), core, Request{FloorSatPerVB: 1})
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if r.Source != FromRelayFloor || r.SatPerVB != 2 {
		t.Fatalf("rate = %g from %v, want 2 from the relay floor", r.SatPerVB, r.Source)
	}
	if !strings.Contains(r.Report(), "not slow, it is absent") {
		t.Error("the report must say what being below the relay floor means")
	}
}

func TestCoresAnsweredTargetIsReported(t *testing.T) {
	core := (&fakeCore{
		// Asked about 2, answered about 6. Core answers for the nearest target
		// it has data for.
		estimate: map[string]any{"feerate": 0.0001, "blocks": 6},
		mempool:  floors(0.00001, 0.00001),
	}).start(t)

	r, err := Estimate(context.Background(), core, Request{TargetBlocks: 2, FloorSatPerVB: 1})
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if r.TargetBlocks != 2 || r.AnsweredForBlocks != 6 {
		t.Fatalf("asked %d, answered %d", r.TargetBlocks, r.AnsweredForBlocks)
	}
	if !strings.Contains(r.Report(), "Asked about 2 blocks") {
		t.Error("the report must say the rate is for a longer wait than was asked for")
	}
}

func TestANegativeFloorIsRefused(t *testing.T) {
	core := (&fakeCore{
		estimate: map[string]any{"feerate": 0.0001, "blocks": 6},
		mempool:  floors(0.00001, 0.00001),
	}).start(t)

	if _, err := Estimate(context.Background(), core, Request{FloorSatPerVB: -1}); err == nil {
		t.Fatal("a negative floor was accepted")
	}
}

// bitcoind's own unit conversion, pinned separately because it is the one place
// a factor of a thousand could hide.
func TestSatPerVBConversion(t *testing.T) {
	cases := map[float64]float64{
		0.00001: 1,   // Core's default relay floor
		0.0001:  10,  //
		0.001:   100, //
		0:       0,   // no estimate
	}
	for btcPerKvB, want := range cases {
		e := bitcoind.FeeEstimate{FeeRateBTCPerKvB: btcPerKvB}
		if got := e.SatPerVB(); got != want {
			t.Errorf("%g BTC/kvB = %g sat/vB, want %g", btcPerKvB, got, want)
		}
	}
}

func TestErrorsFromCoreAreNotSwallowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"result": nil,
			"error":  map[string]any{"code": -32601, "message": "Method not found"},
		})
	}))
	defer srv.Close()

	c, err := bitcoind.New(bitcoind.Config{
		Address: strings.TrimPrefix(srv.URL, "http://"), User: "u", Pass: "p",
	})
	if err != nil {
		t.Fatalf("building the client: %v", err)
	}
	if _, err := Estimate(context.Background(), c, Request{FloorSatPerVB: 10}); err == nil {
		t.Fatal("an RPC failure must not fall through to the floor: a node that " +
			"cannot be asked is a different situation from a node with nothing to say")
	}
}
