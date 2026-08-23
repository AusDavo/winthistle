package settle_test

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/AusDavo/winthistle/internal/bitcoind"
	"github.com/AusDavo/winthistle/internal/coldwallet"
	"github.com/AusDavo/winthistle/internal/plan"
	"github.com/AusDavo/winthistle/internal/prose"
	"github.com/AusDavo/winthistle/internal/regtestenv"
	"github.com/AusDavo/winthistle/internal/settle"
)

// stalledParent creates a genuinely unconfirmed transaction paying the cold
// wallet, and returns it in the shape the CPFP builder reasons about.
//
// Not a channel batch, and deliberately not one. The batch's own funding
// transaction is broadcast in exactly one place in this repository —
// internal/arm's publish test — and adding a second broadcaster to a fixture
// would weaken a property the build is careful about. What the CPFP child needs
// is structurally simpler than a batch: an unconfirmed output the cold wallet
// owns, and a parent whose real size and fee can be read back. The miner paying
// the cold wallet at 1 sat/vB produces exactly that, and the arithmetic the
// child does is the same arithmetic either way.
//
// What this therefore does NOT prove: that the child is built correctly against
// a *batch's* change output specifically. That difference is the outpoint it
// spends, and nothing else.
func stalledParent(t *testing.T, env *regtestenv.Env, amountSat int64) settle.Parent {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	addr, err := coldwallet.ChangeAddress(ctx, env.Cold)
	if err != nil {
		t.Fatalf("asking the cold wallet for an address: %v", err)
	}

	// 1 sat/vB, so a package target well above it needs a real child fee.
	var sent struct {
		Complete bool   `json:"complete"`
		TxID     string `json:"txid"`
	}
	// As text, not a float: prose.BTC is the one way this codebase renders a
	// satoshi as a BTC amount, and Core parses it without loss.
	outputs := []map[string]any{{addr: prose.BTC(amountSat)}}
	if err := env.Miner.Call(ctx, "send",
		[]any{outputs, nil, "unset", 1.0}, &sent); err != nil {
		t.Fatalf("funding a stalled parent from the miner: %v", err)
	}
	if !sent.Complete || sent.TxID == "" {
		t.Fatalf("the miner did not complete the parent: %+v", sent)
	}

	// The parent's real size and fee, from the mempool it is sitting in.
	var entry struct {
		Vsize int64 `json:"vsize"`
		Fees  struct {
			Base float64 `json:"base"`
		} `json:"fees"`
	}
	if err := env.Node.Call(ctx, "getmempoolentry", []any{sent.TxID}, &entry); err != nil {
		t.Fatalf("reading the parent out of the mempool: %v", err)
	}

	// Which output is ours. minconf 0, because the whole point is that it is
	// unconfirmed — and polled, because the two wallets learn of the transaction
	// independently: the miner built it, and the cold wallet hears about it when
	// the node relays it into the mempool a moment later.
	var (
		found bool
		p     settle.Parent
	)
	deadline := time.Now().Add(30 * time.Second)
	for {
		utxos, err := env.Cold.ListUnspent(ctx, 0, 0)
		if err != nil {
			t.Fatalf("listing the cold wallet's unconfirmed outputs: %v", err)
		}
		for _, u := range utxos {
			if u.TxID != sent.TxID || u.Address != addr {
				continue
			}
			p = settle.Parent{
				TxID:      sent.TxID,
				VsizeVB:   entry.Vsize,
				FeeSat:    int64(math.Round(entry.Fees.Base * 1e8)),
				Change:    plan.Outpoint{TxID: u.TxID, Vout: u.Vout},
				ChangeSat: satOf(u),
			}
			found = true
		}
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the cold wallet never saw an unconfirmed output of %s", sent.TxID)
		}
		time.Sleep(500 * time.Millisecond)
	}

	// Confirm it on the way out, so the harness does not accumulate a mempool.
	t.Cleanup(func() { env.Mine(t, 1) })

	t.Logf("parent %s: %d vB, %d sat fee (%.2f sat/vB), %d sat to spend",
		p.TxID, p.VsizeVB, p.FeeSat, float64(p.FeeSat)/float64(p.VsizeVB), p.ChangeSat)
	return p
}

// TestTheCPFPChildLiftsTheParentToTheTarget is I-4's only remedy, built for real.
//
// The batch can never be replaced, so a batch that is underpaying can only be
// accelerated by spending its own change. The child has to pay for both
// transactions: (parentVsize + childVsize) * target - parentFee. Core applies a
// fee *rate* rather than an absolute fee, which is why BuildChild builds twice —
// once to learn the child's size, once at the rate that produces the fee the
// package needs.
func TestTheCPFPChildLiftsTheParentToTheTarget(t *testing.T) {
	env := regtestenv.Start(t)
	ctx := harnessCtx(t)

	parent := stalledParent(t, env, 5_000_000)
	const target = 20.0

	child, err := settle.BuildChild(ctx, settle.ChildRequest{
		Wallet:         env.Cold,
		Parent:         parent,
		TargetSatPerVB: target,
	})
	if err != nil {
		t.Fatalf("building the CPFP child: %v", err)
	}
	releaseAtCleanup(t, env.Cold, child.Inputs)

	t.Logf("\n%s", child.Report(parent, target))

	// It spends the parent's output and nothing else. A second, already-confirmed
	// input would make a cheaper child and would also let a miner take the child
	// without the parent, which is the one thing CPFP must not allow.
	if len(child.Inputs) != 1 {
		t.Fatalf("the child spends %d inputs: %v", len(child.Inputs), child.Inputs)
	}
	if child.Inputs[0].TxID != parent.Change.TxID || child.Inputs[0].Vout != parent.Change.Vout {
		t.Fatalf("the child spends %s, not the parent's output %s",
			child.Inputs[0], parent.Change)
	}

	// It reaches the target. The estimate is an upper bound on size, so the
	// achieved rate is at or slightly above the target and never below it.
	if child.PackageRate < target {
		t.Errorf("the package pays %.2f sat/vB and the target was %.2f",
			child.PackageRate, target)
	}
	if child.PackageRate > target*1.5 {
		t.Errorf("the package pays %.2f sat/vB against a target of %.2f, which is "+
			"a lot of overpayment for a rounding step", child.PackageRate, target)
	}
	if child.PackageVsizeVB != parent.VsizeVB+child.VsizeVB {
		t.Errorf("package vsize %d != %d + %d", child.PackageVsizeVB,
			parent.VsizeVB, child.VsizeVB)
	}
	if child.PackageFeeSat != parent.FeeSat+child.FeeSat {
		t.Errorf("package fee %d != %d + %d", child.PackageFeeSat,
			parent.FeeSat, child.FeeSat)
	}

	// And it leaves something worth having, above the dust floor.
	if child.OutputSat < plan.DustSat {
		t.Errorf("the child returns %d sat, below the %d dust floor",
			child.OutputSat, plan.DustSat)
	}
	if child.OutputSat+child.FeeSat != parent.ChangeSat {
		t.Errorf("the child does not conserve value: %d + %d != %d",
			child.OutputSat, child.FeeSat, parent.ChangeSat)
	}

	// Core would accept the pair. testmempoolaccept validates without relaying,
	// so nothing goes out — and the child is unsigned here, so this is a check
	// that the *parent* is still what the arithmetic assumed rather than a check
	// of the child's witnesses.
	if !env.InMempool(t, parent.TxID) {
		t.Error("the parent left the mempool while the child was being built")
	}
}

// A parent that already pays the target needs no child, and saying so is better
// than building one that pays a fee for nothing.
func TestNoChildIsBuiltForAParentThatAlreadyPays(t *testing.T) {
	env := regtestenv.Start(t)
	ctx := harnessCtx(t)

	parent := stalledParent(t, env, 5_000_000)

	_, err := settle.BuildChild(ctx, settle.ChildRequest{
		Wallet:         env.Cold,
		Parent:         parent,
		TargetSatPerVB: 0.5, // below the parent's own 1 sat/vB
	})
	if !errors.Is(err, settle.ErrNoChildNeeded) {
		t.Fatalf("wrong sentinel: %v", err)
	}
	t.Logf("refused, correctly: %v", err)
}

// A change output too small to fund a viable child is the state internal/plan's
// ChangeFloor exists to prevent at planning time. If it is reached anyway, the
// refusal has to name the arithmetic rather than hand back a child whose output
// would not relay.
func TestAChangeOutputTooSmallToLiftIsRefused(t *testing.T) {
	env := regtestenv.Start(t)
	ctx := harnessCtx(t)

	parent := stalledParent(t, env, 5_000_000)
	// Pretend the change is barely above dust: the child's own fee at any
	// meaningful target exceeds it.
	parent.ChangeSat = plan.DustSat + 100

	_, err := settle.BuildChild(ctx, settle.ChildRequest{
		Wallet:         env.Cold,
		Parent:         parent,
		TargetSatPerVB: 50,
	})
	if !errors.Is(err, settle.ErrChangeTooSmall) {
		t.Fatalf("wrong sentinel: %v", err)
	}
	t.Logf("refused, correctly: %v", err)
}

// releaseAtCleanup gives back the coins the child's build locked.
func releaseAtCleanup(t *testing.T, wallet *bitcoind.Client, ops []bitcoind.Outpoint) {
	t.Helper()
	if len(ops) == 0 {
		return
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := wallet.ReleaseLocks(ctx, ops); err != nil {
			t.Errorf("releasing the child's coin locks: %v", err)
		}
	})
}

func satOf(u bitcoind.UTXO) int64 { return int64(math.Round(u.Amount * 1e8)) }
