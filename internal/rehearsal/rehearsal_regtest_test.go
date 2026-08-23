package rehearsal_test

import (
	"context"
	"testing"
	"time"

	"github.com/AusDavo/winthistle/internal/bitcoind"
	"github.com/AusDavo/winthistle/internal/coldwallet"
	"github.com/AusDavo/winthistle/internal/combine"
	"github.com/AusDavo/winthistle/internal/regtestenv"
	"github.com/AusDavo/winthistle/internal/rehearsal"
)

// The batch this rehearsal is standing in for: three channels at the size every
// other package's fixtures use, at the rate a regtest node's floor produces.
var (
	mirror  = []int64{250_000, 250_000, 250_000}
	feeRate = 10.0
)

func devices(t *testing.T, env *regtestenv.Env) []rehearsal.Device {
	t.Helper()
	var out []rehearsal.Device
	for _, label := range regtestenv.ColdSigners() {
		label := label
		out = append(out, rehearsal.Device{
			Label: label,
			Sign: func(_ context.Context, psbtB64 string) (combine.Part, error) {
				// SignPartial fails the test if this half completes the PSBT
				// alone, which is the property I-2 rests on.
				return env.SignPartial(t, label, psbtB64), nil
			},
		})
	}
	return out
}

// fenceOffLegacy keeps Core's coin selection off anything LND would refuse, the
// way the real Phase 0 does before it builds anything.
func fenceOffLegacy(t *testing.T, wallet *bitcoind.Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	coins, err := coldwallet.SelectCoins(ctx, wallet, 1)
	if err != nil {
		t.Fatalf("selecting the cold wallet's coins: %v", err)
	}
	locked, err := coldwallet.FenceOff(ctx, wallet, coins)
	if err != nil {
		t.Fatalf("fencing off the coins the batch may not spend: %v", err)
	}
	if len(locked) == 0 {
		return
	}
	t.Cleanup(func() {
		c, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		if _, err := wallet.ReleaseLocks(c, locked); err != nil {
			t.Errorf("releasing the fence: %v", err)
		}
	})
}

// TestTheDressRehearsalMeasuresARealSigningRound is Phase 0's gate on Phase 1,
// run for real: a decoy of the batch's exact shape, pushed through both halves
// of the simulated cold wallet, combined and finalized in-app, offered to
// testmempoolaccept, and thrown away.
//
// Nothing is broadcast. testmempoolaccept validates without relaying, and the
// finalized transaction never leaves the package — Measurement carries the
// decoy's txid, size and fee and no transaction at all, deliberately, because
// the decoy spends the very coins the real batch is about to use.
func TestTheDressRehearsalMeasuresARealSigningRound(t *testing.T) {
	env := regtestenv.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	fenceOffLegacy(t, env.Cold)

	m, err := rehearsal.Run(ctx, rehearsal.Request{
		Wallet:           env.Cold,
		Node:             env.Node,
		MirrorSat:        mirror,
		FeeRateSatPerVB:  feeRate,
		MinConfirmations: 1,
		Devices:          devices(t, env),
	})
	if err != nil {
		t.Fatalf("the dress rehearsal: %v", err)
	}
	t.Logf("%s", m.Summary())
	t.Logf("\n%s", m.Report())

	if err := rehearsal.Gate(m); err != nil {
		t.Fatalf("the abort gate refused a harness signing round: %v", err)
	}
	if !m.Accepted {
		t.Fatalf("testmempoolaccept refused the rehearsal: %s", m.RejectReason)
	}
	if len(m.Rounds) != len(regtestenv.ColdSigners()) {
		t.Fatalf("measured %d rounds for %d signers", len(m.Rounds),
			len(regtestenv.ColdSigners()))
	}
	for _, r := range m.Rounds {
		if r.Err != nil {
			t.Errorf("%s failed: %v", r.Label, r.Err)
		}
		if r.Elapsed <= 0 {
			t.Errorf("%s was measured at %s", r.Label, r.Elapsed)
		}
	}

	// The decoy mirrored the batch, which is what makes the measurement a
	// prediction rather than a stopwatch reading: the number of outputs is what
	// each device has to display, and the number of inputs is what it has to
	// sign.
	if m.Decoy.Outputs != len(mirror) {
		t.Errorf("mirrored %d outputs, the batch has %d", m.Decoy.Outputs, len(mirror))
	}
	if len(m.Decoy.Inputs) == 0 {
		t.Error("the decoy spent nothing")
	}
	if m.Decoy.VsizeVB <= 0 {
		t.Error("the decoy's size is measured on the finalized transaction and " +
			"should be exact")
	}

	// The signing round is measured on its own, not on the whole rehearsal.
	// Building and combining happen off the peers' clock and must not count
	// against the gate.
	if m.Signing > m.Total {
		t.Errorf("the signing round (%s) outran the whole rehearsal (%s)",
			m.Signing, m.Total)
	}

	// And the coins are back. A rehearsal that left Core's locks in place would
	// have the real build fail with "insufficient funds" on a wallet that is not
	// short of anything.
	locked, err := env.Cold.ListLocks(ctx)
	if err != nil {
		t.Fatalf("listing the cold wallet's locks: %v", err)
	}
	held := map[bitcoind.Outpoint]bool{}
	for _, l := range locked {
		held[l] = true
	}
	for _, in := range m.Decoy.Inputs {
		if held[in] {
			t.Errorf("%s is still locked after the rehearsal", in)
		}
	}
}

// A device that will not sign is one of the two failures Phase 0 exists to
// catch, and it must come back as a measurement rather than as an exception:
// the operator needs to be told which device, and the batch must not be armed.
func TestARehearsalReportsTheDeviceThatFailed(t *testing.T) {
	env := regtestenv.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	fenceOffLegacy(t, env.Cold)

	all := devices(t, env)
	broken := append([]rehearsal.Device{}, all[0])
	broken = append(broken, rehearsal.Device{
		Label: "cold2-unplugged",
		Sign: func(context.Context, string) (combine.Part, error) {
			return combine.Part{}, context.DeadlineExceeded
		},
	})

	m, err := rehearsal.Run(ctx, rehearsal.Request{
		Wallet:           env.Cold,
		Node:             env.Node,
		MirrorSat:        mirror,
		FeeRateSatPerVB:  feeRate,
		MinConfirmations: 1,
		Devices:          broken,
	})
	if err != nil {
		t.Fatalf("Run must report a failed device, not fail: %v", err)
	}
	if m.Passes() {
		t.Fatal("a rehearsal with a dead signer passed")
	}
	if err := rehearsal.Gate(m); err == nil {
		t.Fatal("the gate let a batch arm with a dead signer behind it")
	} else {
		t.Logf("the gate refused, correctly: %v", err)
	}
	t.Logf("\n%s", m.Report())

	// Still cleaned up: a failed rehearsal must not leave the batch's coins
	// locked either.
	locked, err := env.Cold.ListLocks(ctx)
	if err != nil {
		t.Fatalf("listing the cold wallet's locks: %v", err)
	}
	for _, l := range locked {
		for _, in := range m.Decoy.Inputs {
			if l == in {
				t.Errorf("%s is still locked after a failed rehearsal", in)
			}
		}
	}
}
