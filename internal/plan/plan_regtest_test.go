package plan_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/AusDavo/winthistle/internal/abort"
	"github.com/AusDavo/winthistle/internal/coldwallet"
	"github.com/AusDavo/winthistle/internal/plan"
	"github.com/AusDavo/winthistle/internal/prose"
	"github.com/AusDavo/winthistle/internal/regtestenv"
	"github.com/AusDavo/winthistle/internal/reserve"
)

// fixtureChannelSat matches internal/abort's, so a peer that accepts one
// fixture's channel accepts the other's.
const fixtureChannelSat = 250_000

// feeRate is what the fixtures target, in sat/vB.
const feeRate = 10.0

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// cancelStreams takes the batch apart. Nothing here ever finalizes, so every
// stream is still a shim and cancelling costs the peers nothing they will not
// forget on their own.
func cancelStreams(t *testing.T, env *regtestenv.Env, streams []*regtestenv.Stream) {
	t.Cleanup(func() {
		ctx, done := context.WithTimeout(context.Background(), 60*time.Second)
		defer done()
		for _, s := range streams {
			if err := abort.CancelShim(ctx, env.Alice.Lightning, s.PendingChanID); err != nil {
				t.Errorf("cancelling shim %s: %v", s.PendingChanID, err)
			}
		}
	})
}

// changeAddress asks the cold wallet for one of its own change addresses.
//
// Directed mode can do this, which is why the plan names an exact change script
// here rather than recognising one by key origin. Assisted mode cannot, and
// plan.Recognition is what covers that case.
func changeAddress(t *testing.T, env *regtestenv.Env) string {
	t.Helper()
	addr, err := coldwallet.ChangeAddress(testCtx(t), env.Cold)
	if err != nil {
		t.Fatalf("asking the cold wallet for a change address: %v", err)
	}
	return addr
}

// buildBatch is step 4, directed mode: one unsigned PSBT with an output for
// every stream, plus whatever else the plan names, funded and change-derived by
// the watch-only cold wallet.
//
// It goes through coldwallet.Build rather than calling walletcreatefundedpsbt
// itself, so these tests exercise the app's own builder — including the options
// that are not negotiable there (replaceable:false, lockUnspents, bip32derivs).
// The outputs are the fixture's business, which is what lets a test ask for a
// batch paying a stranger or a funding output one satoshi short.
func buildBatch(t *testing.T, env *regtestenv.Env, outputs []coldwallet.Output,
	change string) coldwallet.Built {

	t.Helper()

	built, err := coldwallet.Build(testCtx(t), env.Cold, coldwallet.BuildRequest{
		Outputs:          outputs,
		ChangeAddress:    change,
		FeeRateSatPerVB:  feeRate,
		MinConfirmations: 1,
	})
	if err != nil {
		t.Fatalf("building the batch transaction: %v", err)
	}

	t.Cleanup(func() {
		ctx, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		if _, err := env.Cold.ReleaseLocks(ctx, built.Inputs); err != nil {
			t.Errorf("releasing the cold wallet's coin locks: %v", err)
		}
	})
	return built
}

// batchPlan assembles the plan for a set of streams.
func batchPlan(t *testing.T, streams []*regtestenv.Stream, topUp string,
	topUpSat int64, change string) *plan.Plan {

	t.Helper()
	p := &plan.Plan{
		Chain:  "regtest",
		Change: plan.Change{Address: change},
		Fee:    plan.Fee{TargetSatPerVB: feeRate},
		Inputs: plan.Inputs{MinConfirmations: 1},
	}
	for _, s := range streams {
		p.Channels = append(p.Channels, plan.Channel{
			Peer:          s.PeerPubkey,
			PendingChanID: s.PendingChanID.String(),
			Address:       s.FundingAddress,
			AmountSat:     s.FundingAmount,
		})
	}
	if topUpSat > 0 {
		p.TopUp = &plan.TopUp{Address: topUp, AmountSat: topUpSat, Shortfall: topUpSat}
	}
	return p
}

func fundingOutputs(streams []*regtestenv.Stream) []coldwallet.Output {
	out := make([]coldwallet.Output, 0, len(streams))
	for _, s := range streams {
		out = append(out, coldwallet.Output{
			Address: s.FundingAddress, AmountSat: s.FundingAmount,
		})
	}
	return out
}

// TestTheVerifierAndLNDAgreeOnARealBatch runs the plan and the verifier over the
// same transaction LND is about to be shown, and then shows it to LND.
//
// The point is not that both say yes. It is that both say yes to the *same*
// transaction, so a clean verification is a prediction of step 5 rather than a
// separate opinion about it.
func TestTheVerifierAndLNDAgreeOnARealBatch(t *testing.T) {
	env := regtestenv.Start(t)
	ctx := testCtx(t)

	peers := env.Peers(t)
	streams := make([]*regtestenv.Stream, 0, len(peers))
	for _, p := range peers {
		streams = append(streams, env.OpenShimStream(t, p, fixtureChannelSat))
	}
	cancelStreams(t, env, streams)

	topUp, err := plan.TopUpAddress(ctx, env.Alice.Lightning)
	if err != nil {
		t.Fatalf("minting a reserve top-up address: %v", err)
	}
	const topUpSat = 50_000
	change := changeAddress(t, env)

	outputs := append(fundingOutputs(streams),
		coldwallet.Output{Address: topUp, AmountSat: topUpSat})
	b := buildBatch(t, env, outputs, change)

	p := batchPlan(t, streams, topUp, topUpSat, change)
	t.Logf("\n%s", p.Document())

	v, err := p.VerifyBase64(b.PSBT)
	if err != nil {
		t.Fatalf("verifying: %v", err)
	}
	if !v.OK() {
		t.Fatalf("the verifier refused the transaction Core built to the plan:\n%s",
			v.Report())
	}
	t.Logf("\n%s", v.Report())

	// I-3's anchor has to be the transaction everyone else is talking about.
	if v.UnsignedTxID != b.TxID {
		t.Fatalf("the verifier hashed %s, Core says %s", v.UnsignedTxID, b.TxID)
	}
	if len(v.Outputs) != len(streams)+2 {
		t.Errorf("attributed %d outputs, expected %d funding plus top-up plus change",
			len(v.Outputs), len(streams)+2)
	}
	for _, a := range v.Outputs {
		if !a.Named {
			t.Errorf("output %d (%s) was not attributed", a.Index, a.Address)
		}
	}

	// And now LND, on the same bytes.
	for _, s := range streams {
		if err := env.TryVerify(t, s, b.PSBT); err != nil {
			t.Fatalf("psbt_verify refused a transaction the plan accepted, for %s: %v\n"+
				"That is the disagreement this test exists to catch.", s.PendingChanID, err)
		}
	}
}

// TestLNDAcceptsAnOutputTheVerifierRefuses is the whole argument for this
// package, run against a live node.
//
// PsbtIntent.Verify locates its own funding output with psbt.TxOutsEqual and
// requires only that the input sum exceed the total output sum. It never asserts
// that its output is the only one — which is what lets n channels share one
// transaction, and what leaves an unnamed output entirely unpoliced. So a
// returned PSBT can pay a stranger and still satisfy every psbt_verify in the
// batch.
func TestLNDAcceptsAnOutputTheVerifierRefuses(t *testing.T) {
	env := regtestenv.Start(t)
	ctx := testCtx(t)

	peers := env.Peers(t)
	streams := make([]*regtestenv.Stream, 0, len(peers))
	for _, p := range peers {
		streams = append(streams, env.OpenShimStream(t, p, fixtureChannelSat))
	}
	cancelStreams(t, env, streams)

	topUp, err := plan.TopUpAddress(ctx, env.Alice.Lightning)
	if err != nil {
		t.Fatalf("minting a reserve top-up address: %v", err)
	}
	const topUpSat = 50_000
	change := changeAddress(t, env)

	// The stranger: an address of the miner wallet, standing in for whoever a
	// compromised or merely wrong builder would pay.
	var stranger string
	if err := env.Miner.Call(ctx, "getnewaddress", nil, &stranger); err != nil {
		t.Fatalf("minting the stranger's address: %v", err)
	}
	const strangerSat = 400_000

	outputs := append(fundingOutputs(streams),
		coldwallet.Output{Address: topUp, AmountSat: topUpSat},
		coldwallet.Output{Address: stranger, AmountSat: strangerSat})
	b := buildBatch(t, env, outputs, change)

	p := batchPlan(t, streams, topUp, topUpSat, change)
	v, err := p.VerifyBase64(b.PSBT)
	if err != nil {
		t.Fatalf("verifying: %v", err)
	}
	if v.OK() {
		t.Fatalf("the verifier accepted a transaction paying %s to %s:\n%s",
			prose.Sats(strangerSat), stranger, v.Report())
	}
	var sawUnnamed bool
	for _, prob := range v.Problems {
		if prob.Code == plan.UnnamedOutput {
			sawUnnamed = true
		}
	}
	if !sawUnnamed {
		t.Fatalf("the stranger's output was not the objection: %s", v.Summary())
	}
	t.Logf("\n%s", v.Report())

	// LND, on the same bytes, for every channel in the batch.
	for _, s := range streams {
		if err := env.TryVerify(t, s, b.PSBT); err != nil {
			t.Fatalf("psbt_verify refused %s for a reason of its own (%v), which "+
				"would make this test prove nothing. Check the fixture.",
				s.PendingChanID, err)
		}
	}
	t.Logf("all %d psbt_verify calls accepted a transaction paying %s to an "+
		"address nobody named. LND is not the check here — this package is.",
		len(streams), prose.Sats(strangerSat))
}

// TestTheReserveTopUpCountsAtVerify proves the design's remedy against the live
// node, and proves it works with the p2tr address the design asks for.
//
// CheckReservedValue credits transaction outputs paying an address the node's
// wallet owns, so a top-up inside the batch counts at step 5 with no second
// transaction and no wait. The alternative reading — that a top-up has to
// confirm first — would make the remedy useless inside a ten-minute window.
func TestTheReserveTopUpCountsAtVerify(t *testing.T) {
	env := regtestenv.Start(t)
	ctx := testCtx(t)

	// Take the node's own wallet out of the picture, exactly as
	// internal/reserve's tests do.
	leased, _ := env.LeaseAllUnspent(t)
	t.Logf("leased %s of alice's own coins, so the anchor reserve cannot be met "+
		"from the wallet", prose.Sats(leased))

	finding, err := reserve.Check(ctx, env.Alice.WalletKit, reserve.Batch{Public: 1})
	if err != nil {
		t.Fatalf("reserve pre-flight: %v", err)
	}
	if finding.Verdict() != reserve.WouldBeRefused {
		t.Skipf("alice can still meet the reserve (%s), so this test cannot set up "+
			"the refusal it needs", finding.Summary())
	}

	peers := env.Peers(t)
	if len(peers) < 2 {
		t.Skipf("this test needs two peers, alice has %d", len(peers))
	}
	change := changeAddress(t, env)

	// Without the top-up: psbt_verify must refuse, for a reason that has nothing
	// to do with the batch.
	bare := env.OpenShimStream(t, peers[0], fixtureChannelSat)
	cancelStreams(t, env, []*regtestenv.Stream{bare})
	bareTx := buildBatch(t, env, fundingOutputs([]*regtestenv.Stream{bare}), change)

	barePlan := batchPlan(t, []*regtestenv.Stream{bare}, "", 0, change)
	if v, err := barePlan.VerifyBase64(bareTx.PSBT); err != nil {
		t.Fatalf("verifying the bare batch: %v", err)
	} else if !v.OK() {
		t.Fatalf("the verifier refused a well-formed batch:\n%s", v.Report())
	}
	err = env.TryVerify(t, bare, bareTx.PSBT)
	if err == nil {
		t.Fatalf("psbt_verify accepted a batch with the reserve unmet, so this test " +
			"is not reproducing the condition it needs")
	}
	t.Logf("without a top-up, LND says: %v", err)

	// With one: same shape, plus the output internal/reserve says is needed. The
	// amount is the shortfall exactly — CheckReservedValue refuses only when the
	// balance is strictly below the reserve, so exactly enough is enough, and a
	// test that overshot would not prove that.
	top, err := plan.ReserveTopUp(ctx, env.Alice.Lightning, finding)
	if err != nil {
		t.Fatalf("building the reserve top-up: %v", err)
	}
	if top == nil {
		t.Fatal("the pre-flight said the batch would be refused and then asked for " +
			"no top-up")
	}
	topUp, topUpSat := top.Address, top.AmountSat

	toppedStream := env.OpenShimStream(t, peers[1], fixtureChannelSat)
	cancelStreams(t, env, []*regtestenv.Stream{toppedStream})
	outputs := append(fundingOutputs([]*regtestenv.Stream{toppedStream}),
		coldwallet.Output{Address: topUp, AmountSat: topUpSat})
	toppedTx := buildBatch(t, env, outputs, change)

	toppedPlan := batchPlan(t, []*regtestenv.Stream{toppedStream}, topUp, topUpSat, change)
	v, err := toppedPlan.VerifyBase64(toppedTx.PSBT)
	if err != nil {
		t.Fatalf("verifying the topped-up batch: %v", err)
	}
	if !v.OK() {
		t.Fatalf("the verifier refused the topped-up batch:\n%s", v.Report())
	}
	if err := env.TryVerify(t, toppedStream, toppedTx.PSBT); err != nil {
		t.Fatalf("psbt_verify still refused the batch with a %s top-up to %s: %v\n"+
			"The design's remedy for a reserve shortfall depends on this working.",
			prose.Sats(topUpSat), topUp, err)
	}
	t.Logf("a %s top-up paying %s — a p2tr address the node minted — cleared the "+
		"reserve at psbt_verify, with nothing confirmed and no second transaction",
		prose.Sats(topUpSat), topUp)
}

// TestTheVerifierNamesTheChannelAnAmountBelongsTo. A finding the operator cannot
// act on is a finding that costs another signing round.
func TestTheVerifierNamesTheChannelAnAmountBelongsTo(t *testing.T) {
	env := regtestenv.Start(t)
	ctx := testCtx(t)

	peers := env.Peers(t)
	s := env.OpenShimStream(t, peers[0], fixtureChannelSat)
	cancelStreams(t, env, []*regtestenv.Stream{s})

	topUp, err := plan.TopUpAddress(ctx, env.Alice.Lightning)
	if err != nil {
		t.Fatalf("minting a reserve top-up address: %v", err)
	}
	change := changeAddress(t, env)

	// Core builds a transaction paying one satoshi less than LND asked for.
	outputs := []coldwallet.Output{
		{Address: s.FundingAddress, AmountSat: s.FundingAmount - 1},
		{Address: topUp, AmountSat: 50_000},
	}
	b := buildBatch(t, env, outputs, change)

	p := batchPlan(t, []*regtestenv.Stream{s}, topUp, 50_000, change)
	v, err := p.VerifyBase64(b.PSBT)
	if err != nil {
		t.Fatalf("verifying: %v", err)
	}
	if v.OK() {
		t.Fatal("a funding output one satoshi short was accepted; LND compares its " +
			"own output with psbt.TxOutsEqual, which compares the value")
	}
	var named bool
	for _, prob := range v.Problems {
		if prob.Code == plan.WrongAmount && strings.Contains(prob.Headline, "channel 1") &&
			strings.Contains(prob.Headline, s.PeerPubkey[:16]) {
			named = true
		}
	}
	if !named {
		t.Errorf("the wrong-amount finding does not name the channel: %s", v.Summary())
	}

	// LND agrees, which is the case where our check is merely earlier and
	// clearer rather than the only one.
	if err := env.TryVerify(t, s, b.PSBT); err == nil {
		t.Error("psbt_verify accepted a funding output one satoshi short")
	} else {
		t.Logf("LND agrees, in its own words: %v", err)
	}
}
