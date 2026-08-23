package bump_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/AusDavo/winthistle/internal/bitcoind"
	"github.com/AusDavo/winthistle/internal/bump"
	"github.com/AusDavo/winthistle/internal/combine"
	"github.com/AusDavo/winthistle/internal/journal"
	"github.com/AusDavo/winthistle/internal/lnd"
	"github.com/AusDavo/winthistle/internal/plan"
	"github.com/AusDavo/winthistle/internal/prose"
	"github.com/AusDavo/winthistle/internal/regtestenv"
	"github.com/AusDavo/winthistle/internal/rehearsal"
	"github.com/AusDavo/winthistle/internal/settle"
	"github.com/btcsuite/btcd/btcutil/psbt"
)

// These tests drive the whole bump against the live regtest node.
//
// The parent is a miner-to-cold payment at one sat/vB rather than a real batch,
// and that is the same choice internal/settle's CPFP test makes for the same
// reason: the batch's own funding transaction is broadcast in exactly one
// production place, publishing one costs cold coins and leaves open channels
// nothing in this build can close, and what a CPFP child needs from a parent is
// structurally simpler than a batch — an unconfirmed output the cold wallet
// owns, and a real size and fee to read back.
//
// What that means for the journal is that the run row is written by hand. Every
// gate the bump passes through is still the real one — BeginBump refuses a run
// that never reached the publish call, and the state machine is untouched — but
// the channels in that row are fictional, so nothing here proves anything about
// LND's pending list. The one thing the fixture therefore cannot test is the
// funding countdown, and the tests say so where it matters.
//
// What it does NOT prove: that the child is built against a *batch's* change
// output specifically. That difference is which outpoint it spends and nothing
// else.

func harnessCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// stalled builds a genuinely unconfirmed parent paying the cold wallet, and the
// journal row that says a run published it.
func stalled(t *testing.T, env *regtestenv.Env, amountSat int64) (
	*journal.Journal, string, string) {

	t.Helper()
	ctx := harnessCtx(t)

	addr, err := coldChangeAddress(ctx, env)
	if err != nil {
		t.Fatalf("asking the cold wallet for an address: %v", err)
	}

	// One sat/vB, so any real target needs a real child fee. As text rather than
	// a float: prose.BTC is the one way this codebase renders a satoshi as a BTC
	// amount, and Core parses it without loss.
	var sent struct {
		Complete bool   `json:"complete"`
		TxID     string `json:"txid"`
	}
	outputs := []map[string]any{{addr: prose.BTC(amountSat)}}
	if err := env.Miner.Call(ctx, "send",
		[]any{outputs, nil, "unset", 1.0}, &sent); err != nil {
		t.Fatalf("funding a stalled parent: %v", err)
	}
	if !sent.Complete || sent.TxID == "" {
		t.Fatalf("the miner did not complete the parent: %+v", sent)
	}
	// Confirm it on the way out so the harness does not accumulate a mempool.
	t.Cleanup(func() { env.Mine(t, 1) })

	// Wait for cold-watch to see it. The two wallets learn of the transaction
	// independently: the miner built it, and the cold wallet hears about it when
	// the node relays it into the mempool a moment later.
	awaitColdOutput(t, env, sent.TxID, addr)

	j, err := journal.Open(ctx, filepath.Join(t.TempDir(), "runs.db"))
	if err != nil {
		t.Fatalf("opening a journal: %v", err)
	}
	t.Cleanup(func() { j.Close() })

	runID := "bumpfixture-" + sent.TxID[:8]
	journalAPublishedRun(t, j, runID, sent.TxID)
	return j, runID, sent.TxID
}

// journalAPublishedRun writes the row a bump needs: a run that reached the
// publish call, carrying this txid.
func journalAPublishedRun(t *testing.T, j *journal.Journal, runID, txid string) {
	t.Helper()
	ctx := harnessCtx(t)

	id, err := lnd.NewPendingChanID()
	if err != nil {
		t.Fatal(err)
	}
	steps := []struct {
		what string
		do   func() error
	}{
		{"Begin", func() error {
			return j.Begin(ctx, runID, []journal.NewChannel{{
				PendingChanID: id,
				PeerPubkey:    "02" + fmt.Sprintf("%062d", 1),
				AmountSat:     250_000,
			}})
		}},
		{"MarkVerified", func() error { return j.MarkVerified(ctx, runID, id) }},
		{"RecordFinalizedTx", func() error {
			return j.RecordFinalizedTx(ctx, runID, txid, "00")
		}},
		{"MarkPending", func() error {
			return j.MarkPending(ctx, runID, id, lnd.ChannelPoint{TxID: txid, Index: 0})
		}},
		{"MarkPublishing", func() error { return j.MarkPublishing(ctx, runID) }},
		{"MarkPublished", func() error { return j.MarkPublished(ctx, runID) }},
	}
	for _, s := range steps {
		if err := s.do(); err != nil {
			t.Fatalf("journalling the fixture run (%s): %v", s.what, err)
		}
	}
}

func awaitColdOutput(t *testing.T, env *regtestenv.Env, txid, addr string) {
	t.Helper()
	ctx := harnessCtx(t)
	deadline := time.Now().Add(30 * time.Second)
	for {
		utxos, err := env.Cold.ListUnspent(ctx, 0, 0)
		if err != nil {
			t.Fatalf("listing the cold wallet's unconfirmed outputs: %v", err)
		}
		for _, u := range utxos {
			if u.TxID == txid && u.Address == addr {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the cold wallet never saw an unconfirmed output of %s", txid)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func coldChangeAddress(ctx context.Context, env *regtestenv.Env) (string, error) {
	var addr string
	err := env.Cold.Call(ctx, "getrawchangeaddress", nil, &addr)
	return addr, err
}

// deps wires the bump against the harness. Signers is the simulated cold
// wallet's two halves going through internal/combine, which is the same path a
// real device's partial takes.
func deps(t *testing.T, env *regtestenv.Env, j *journal.Journal, out *bytes.Buffer) bump.Deps {
	t.Helper()
	return bump.Deps{
		LND:       env.Alice.Lightning,
		Publisher: env.Alice.WalletKit,
		Node:      env.Node,
		Wallet:    env.Cold,
		Journal:   j,
		Signers:   coldSigners{t: t, env: env},
		Out:       out,
	}
}

// coldSigners is the harness's cold wallet as a bump.Signers.
type coldSigners struct {
	t   *testing.T
	env *regtestenv.Env
}

func (c coldSigners) Labels() []string { return regtestenv.ColdSigners() }

func (c coldSigners) Round(string) []rehearsal.Device {
	out := make([]rehearsal.Device, 0, 2)
	for _, label := range regtestenv.ColdSigners() {
		label := label
		out = append(out, rehearsal.Device{
			Label: label,
			Sign: func(_ context.Context, psbtB64 string) (combine.Part, error) {
				return c.env.SignPartial(c.t, label, psbtB64), nil
			},
		})
	}
	return out
}

// TestTheChildsArithmeticAgreesWithCoresOnARealStalledParent is the claim the
// whole command rests on, checked against a transaction that is genuinely
// sitting in a mempool.
//
// Two things have to agree, and they are arrived at independently.
// walletcreatefundedpsbt is given a *package* rate and charges the fee that
// lifts the whole unconfirmed ancestor package to it; plan.ChildFeeSat computes
// the same figure from the parent's size and fee, which is the expression
// internal/plan sizes the change output with at planning time. If those two ever
// stop agreeing, the change output this build promises is big enough to rescue a
// batch is sized against arithmetic Core does not use.
func TestTheChildsArithmeticAgreesWithCoresOnARealStalledParent(t *testing.T) {
	env := regtestenv.Start(t)
	ctx := harnessCtx(t)
	j, runID, parentTxID := stalled(t, env, 5_000_000)

	var out bytes.Buffer
	d := deps(t, env, j, &out)

	located, err := bump.Locate(ctx, d, runID)
	if err != nil {
		t.Fatalf("locating the parent: %v", err)
	}
	t.Logf("\n%s", located.Report())

	// Core's own mempool entry, asked for separately, is what the located figures
	// have to equal. Reading it twice by two routes is the point: if Locate ever
	// starts deriving these rather than reading them, this fails.
	entry, inMempool, err := env.Node.MempoolEntry(ctx, parentTxID)
	if err != nil || !inMempool {
		t.Fatalf("the parent is not in the mempool: %v", err)
	}
	if located.Parent.VsizeVB != entry.VsizeVB || located.Parent.FeeSat != entry.FeeSat {
		t.Errorf("the located parent is %d vB / %d sat and Core says %d vB / %d sat",
			located.Parent.VsizeVB, located.Parent.FeeSat, entry.VsizeVB, entry.FeeSat)
	}
	if entry.AncestorCount != 1 {
		t.Logf("note: this parent has %d unconfirmed ancestors, so the arithmetic "+
			"below is about the package rather than the transaction",
			entry.AncestorCount-1)
	}
	if located.Change.TxID != parentTxID {
		t.Fatalf("the change output found is %s:%d, not an output of the parent",
			located.Change.TxID, located.Change.Vout)
	}

	// The countdown cannot be tested from this fixture: the run's channels are
	// journalled by hand, so LND has never heard of them.
	if located.HasExpiry {
		t.Errorf("the fixture reported a funding countdown (%d blocks), which means "+
			"LND does know about these channels and this fixture is not what it "+
			"claims to be", located.ExpiryBlocks)
	}

	// Now the arithmetic, at three rates, the way internal/settle measured it.
	for _, target := range []float64{20, 50, 200} {
		child, err := settle.BuildChild(ctx, settle.ChildRequest{
			Wallet:         env.Cold,
			Parent:         located.Parent,
			TargetSatPerVB: target,
		})
		if err != nil {
			t.Fatalf("building a child at %g sat/vB: %v", target, err)
		}
		env.ReleaseLocksAtCleanup(t, env.Cold, child.Inputs)

		want := plan.ChildFeeSat(located.Parent.VsizeVB, located.Parent.FeeSat,
			target, child.VsizeVB)
		if child.FeeSat != want {
			t.Errorf("at %g sat/vB Core charged %d sat and plan.ChildFeeSat wanted "+
				"%d — a %d sat disagreement on a %d vB child over a %d vB parent",
				target, child.FeeSat, want, child.FeeSat-want, child.VsizeVB,
				located.Parent.VsizeVB)
			continue
		}
		t.Logf("%g sat/vB: Core and plan.ChildFeeSat both say %s for a %d vB child "+
			"(package %.2f sat/vB)", target, prose.Sats(child.FeeSat), child.VsizeVB,
			child.PackageRate)

		// And our verifier agrees with what Core built.
		v, err := bump.Verify(child.Raw, bump.Expectation{
			Chain:          "regtest",
			Parent:         located.Parent,
			TargetSatPerVB: target,
			PaysTo:         child.PaysTo,
			FeeSat:         child.FeeSat,
		})
		if err != nil {
			t.Fatalf("verifying the child at %g sat/vB: %v", target, err)
		}
		if !v.OK() {
			t.Errorf("the verifier refused a child Core built at %g sat/vB:\n%s",
				target, v.Report("what was built"))
		}
	}
}

// TestTheWholeBumpSignsAndPublishesAChild is the command end to end, and it does
// broadcast.
//
// It has to. "The child reaches the network on exactly one line, and only after
// it has been merged, finalized and verified in-app" is not a claim a dry run
// can make — the same reasoning internal/arm's publish test runs on. Unlike that
// one this leaves nothing behind: the child pays the cold wallet's own change
// back to the cold wallet, so the only cost is the fee, and the cleanup mines it
// away.
func TestTheWholeBumpSignsAndPublishesAChild(t *testing.T) {
	env := regtestenv.Start(t)
	ctx := harnessCtx(t)
	j, runID, parentTxID := stalled(t, env, 5_000_000)

	var out bytes.Buffer
	d := deps(t, env, j, &out)

	res, err := bump.Do(ctx, d, bump.Options{RunID: runID, TargetSatPerVB: 20})
	t.Logf("\n%s", out.String())
	if err != nil {
		t.Fatalf("the bump did not finish: %v", err)
	}
	if !res.Published {
		t.Fatal("the bump returned without error and without publishing")
	}

	// Both verification passes ran, and the second one measured rather than
	// estimated.
	if res.Verified == nil || !res.Verified.OK() {
		t.Error("the first verification pass did not run or did not pass")
	}
	if res.Rechecked == nil || !res.Rechecked.OK() {
		t.Fatal("the second verification pass did not run or did not pass")
	}
	if !res.Rechecked.Signed {
		t.Error("the recheck did not see a signed transaction, so the size it " +
			"reported was still an upper bound")
	}
	if res.Rechecked.VsizeVB > res.Child.VsizeVB {
		t.Errorf("the signed child is %d vB against a %d vB estimate. The estimate "+
			"uses upper bounds on every signature, so it must not be exceeded",
			res.Rechecked.VsizeVB, res.Child.VsizeVB)
	}
	if res.Rechecked.PackageRate < 20 {
		t.Errorf("the pair pays %.2f sat/vB against a target of 20",
			res.Rechecked.PackageRate)
	}

	// The child is really in the mempool, and so is the parent — a miner takes
	// the pair or neither, which is the whole mechanism.
	if !env.InMempool(t, res.Child.TxID) {
		t.Errorf("the child %s is not in the mempool", res.Child.TxID)
	}
	if !env.InMempool(t, parentTxID) {
		t.Errorf("the parent %s left the mempool", parentTxID)
	}

	// Core now sees the parent as having a descendant, and the package rate it
	// reports is the one the bump promised. This is the independent check: the
	// figure comes from Core's own ancestor accounting rather than from ours.
	entry, inMempool, err := env.Node.MempoolEntry(ctx, res.Child.TxID)
	if err != nil || !inMempool {
		t.Fatalf("Core has no mempool entry for the child: %v", err)
	}
	if entry.AncestorCount != 2 {
		t.Errorf("Core counts %d transactions in the child's ancestor package, "+
			"want 2", entry.AncestorCount)
	}
	coreRate := entry.AncestorRate()
	if math.Abs(coreRate-res.Rechecked.PackageRate) > 0.5 {
		t.Errorf("Core says the package pays %.2f sat/vB and this build says %.2f",
			coreRate, res.Rechecked.PackageRate)
	}
	t.Logf("Core's own ancestor accounting: %d vB, %s, %.2f sat/vB",
		entry.AncestorVsizeVB, prose.Sats(entry.AncestorFeeSat), coreRate)

	// The journal says published, and nothing is still locked.
	b, err := j.LoadBump(ctx, runID, res.Seq)
	if err != nil {
		t.Fatalf("LoadBump: %v", err)
	}
	if b.State != journal.BumpPublished {
		t.Errorf("the journal says the bump is %s", b.State)
	}
	if b.RawTx == "" {
		t.Error("the journal has no raw child, so there would be nothing to " +
			"re-broadcast")
	}
	if len(b.Signers) != 2 {
		t.Errorf("the journal recorded %d signers, want 2", len(b.Signers))
	}

	// Confirm the pair, so the harness is left clean.
	env.Mine(t, 1)
	if env.InMempool(t, res.Child.TxID) {
		t.Error("the child did not confirm")
	}
}

// TestASecondBumpNamesWhatIsHoldingTheChange is a diagnosis, not a mechanism.
//
// Core leaves locked outputs out of listunspent, so a coin an unfinished bump is
// holding looks exactly like a coin that has already been spent — and the two
// call for opposite actions. The journal is the only thing that can tell them
// apart, and this is the check that it does.
func TestASecondBumpNamesWhatIsHoldingTheChange(t *testing.T) {
	env := regtestenv.Start(t)
	ctx := harnessCtx(t)
	j, runID, _ := stalled(t, env, 5_000_000)

	var out bytes.Buffer
	d := deps(t, env, j, &out)

	located, err := bump.Locate(ctx, d, runID)
	if err != nil {
		t.Fatalf("locating the parent: %v", err)
	}

	// A bump that got as far as claiming the change and then stopped, which is
	// exactly what a crash between BeginBump and the signing round leaves.
	seq, err := j.BeginBump(ctx, runID, journal.BumpPlan{
		ParentTxID:     located.ParentTxID,
		ParentVsizeVB:  located.Parent.VsizeVB,
		ParentFeeSat:   located.Parent.FeeSat,
		Change:         located.Change.Outpoint(),
		ChangeSat:      located.Parent.ChangeSat,
		TargetSatPerVB: 20,
	})
	if err != nil {
		t.Fatalf("BeginBump: %v", err)
	}
	if _, err := env.Cold.LockForRun(ctx, []bitcoind.Outpoint{
		located.Change.Outpoint(),
	}); err != nil {
		t.Fatalf("locking the change output: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := j.AbandonBump(ctx, env.Cold, runID, seq); err != nil {
			t.Errorf("releasing the held lock: %v", err)
		}
	})

	if _, err = bump.Locate(ctx, d, runID); err == nil {
		t.Fatal("a second bump found a change output that is locked")
	}
	if !contains(err.Error(), "the journal is what tells them apart") {
		t.Errorf("the refusal does not name the held lock as the cause:\n%v", err)
	}
	if !contains(err.Error(), fmt.Sprintf("bump %d of this run", seq)) {
		t.Errorf("the refusal does not name which bump is holding it:\n%v", err)
	}
	t.Logf("refused, correctly: %v", err)

	// And once the lock is released the change is findable again, which is what
	// makes the advice in that message actionable rather than merely accurate.
	if _, err := j.AbandonBump(ctx, env.Cold, runID, seq); err != nil {
		t.Fatalf("AbandonBump: %v", err)
	}
	if _, err := bump.Locate(ctx, d, runID); err != nil {
		t.Errorf("after releasing the lock the change is still not findable: %v", err)
	}
}

// A run that never published has nothing in any mempool, and the journal is what
// knows. This is the gate, live.
func TestBumpingARunThatNeverPublishedIsRefused(t *testing.T) {
	env := regtestenv.Start(t)
	ctx := harnessCtx(t)

	j, err := journal.Open(ctx, filepath.Join(t.TempDir(), "runs.db"))
	if err != nil {
		t.Fatalf("opening a journal: %v", err)
	}
	t.Cleanup(func() { j.Close() })

	id, err := lnd.NewPendingChanID()
	if err != nil {
		t.Fatal(err)
	}
	const runID = "run-that-stopped-early"
	if err := j.Begin(ctx, runID, []journal.NewChannel{{
		PendingChanID: id, PeerPubkey: "02" + fmt.Sprintf("%062d", 1),
		AmountSat: 250_000,
	}}); err != nil {
		t.Fatalf("Begin: %v", err)
	}

	var out bytes.Buffer
	_, err = bump.Locate(ctx, deps(t, env, j, &out), runID)
	if !errors.Is(err, journal.ErrNotPublic) {
		t.Fatalf("wrong sentinel: %v", err)
	}
	t.Logf("refused, correctly: %v", err)
}

// TestCoreEnforcesTheRelayCeilingAndZeroLiftsIt is the source reading behind
// MaxChildFeeRateSatPerVB, checked against the node rather than taken on trust.
//
// The chain being asserted is that LND hands the child to btcwallet, which calls
// SendRawTransaction(tx, false), which rpcclient turns into maxfeerate: 0.1
// BTC/kvB. This test checks the far end of it: that Core really refuses a
// transaction above that rate at its default, and really accepts the same bytes
// when maxfeerate is zero. If Core's default ever changes, the constant and the
// advice built on it are both wrong, and this is what says so.
//
// Nothing is broadcast: testmempoolaccept validates without relaying, so the
// absurd fee in here is never paid.
func TestCoreEnforcesTheRelayCeilingAndZeroLiftsIt(t *testing.T) {
	env := regtestenv.Start(t)
	ctx := harnessCtx(t)

	// A one-in one-out spend of a cold-wallet coin that pays almost all of it as
	// fee: a small transaction at an enormous rate.
	utxos, err := env.Cold.ListUnspent(ctx, 1, math.MaxInt32)
	if err != nil || len(utxos) == 0 {
		t.Skipf("the cold wallet has no confirmed coins: %v", err)
	}
	coin := utxos[0]
	addr, err := coldChangeAddress(ctx, env)
	if err != nil {
		t.Fatalf("asking the cold wallet for an address: %v", err)
	}

	inputs := []map[string]any{{
		"txid": coin.TxID, "vout": coin.Vout,
		"sequence": plan.MaxNonReplaceableSequence,
	}}
	// Leave a dust-ish output; everything else is fee.
	outputs := []map[string]any{{addr: prose.BTC(1_000)}}
	var created struct {
		PSBT string `json:"psbt"`
	}
	if err := env.Cold.Call(ctx, "walletcreatefundedpsbt",
		[]any{inputs, outputs, 0, map[string]any{
			"add_inputs": false, "includeWatching": true, "replaceable": false,
			"fee_rate": 1, "minconf": 1,
		}, true}, &created); err != nil {
		t.Skipf("could not build an absurdly-priced transaction: %v", err)
	}

	// Rewrite the output down to almost nothing so the fee is the whole coin.
	raw, err := combine.ParseBase64(created.PSBT)
	if err != nil {
		t.Fatalf("Core returned a PSBT this build cannot read: %v", err)
	}
	raw = shrinkOutputs(t, raw)

	parts := env.SignWithColdWallet(t, base64Of(t, raw))
	merged, err := combine.Merge(raw, parts)
	if err != nil {
		t.Fatalf("merging: %v", err)
	}
	final, err := combine.Finalize(merged)
	if err != nil {
		t.Fatalf("finalizing: %v", err)
	}
	env.ReleaseLocksAtCleanup(t, env.Cold, []bitcoind.Outpoint{coin.Outpoint()})

	rate := float64(final.FeeSat) / float64(final.Vsize)
	if rate <= bump.MaxChildFeeRateSatPerVB {
		t.Skipf("the fixture only reached %.0f sat/vB, under the %d ceiling, so "+
			"there is nothing to test", rate, bump.MaxChildFeeRateSatPerVB)
	}
	t.Logf("a %d vB transaction paying %s: %.0f sat/vB", final.Vsize,
		prose.Sats(final.FeeSat), rate)

	hexTx := hexOf(final.RawTx)

	// The default: this is the ceiling LND's broadcast path applies, and it is
	// also the one our own pre-flight applies, because bitcoind.TestMempoolAccept
	// passes no maxfeerate either.
	allowed, reason := acceptWith(t, env, hexTx, nil)
	if allowed {
		t.Errorf("Core accepted a %.0f sat/vB transaction at its default "+
			"maxfeerate. MaxChildFeeRateSatPerVB (%d) and the advice built on it "+
			"are then both wrong", rate, bump.MaxChildFeeRateSatPerVB)
	} else {
		t.Logf("refused at the default maxfeerate, as expected: %s", reason)
	}

	// And zero lifts it, which is the escape the ceiling advice names.
	allowed, reason = acceptWith(t, env, hexTx, 0)
	if !allowed {
		t.Errorf("Core refused the same bytes with maxfeerate 0: %s\n"+
			"That is the way out this build tells operators to use, so it has to "+
			"work", reason)
	} else {
		t.Log("accepted with maxfeerate 0, which is the documented escape")
	}
}

// ---- helpers for the ceiling test ----

// acceptWith runs testmempoolaccept with an explicit maxfeerate, or with none.
//
// Deliberately not bitcoind.Client.TestMempoolAccept: that function passes no
// maxfeerate on purpose, because the app's pre-flight should apply the same
// ceiling the broadcast will. Widening it for a test would weaken that.
func acceptWith(t *testing.T, env *regtestenv.Env, hexTx string, maxFeeRate any) (bool, string) {
	t.Helper()
	ctx := harnessCtx(t)

	params := []any{[]string{hexTx}}
	if maxFeeRate != nil {
		params = append(params, maxFeeRate)
	}
	var out []struct {
		Allowed      bool   `json:"allowed"`
		RejectReason string `json:"reject-reason"`
	}
	if err := env.Node.Call(ctx, "testmempoolaccept", params, &out); err != nil {
		t.Fatalf("testmempoolaccept: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("testmempoolaccept answered about %d transactions", len(out))
	}
	return out[0].Allowed, out[0].RejectReason
}

func contains(haystack, needle string) bool {
	return bytes.Contains([]byte(haystack), []byte(needle))
}

// shrinkOutputs drops every output to the dust floor, turning the rest of the
// input value into fee. It is how the ceiling test reaches a rate no sane
// transaction would pay without spending anything to do it.
func shrinkOutputs(t *testing.T, raw []byte) []byte {
	t.Helper()
	packet, err := psbt.NewFromRawBytes(bytes.NewReader(raw), false)
	if err != nil {
		t.Fatalf("parsing the transaction to shrink: %v", err)
	}
	for _, out := range packet.UnsignedTx.TxOut {
		out.Value = plan.DustSat
	}
	var buf bytes.Buffer
	if err := packet.Serialize(&buf); err != nil {
		t.Fatalf("reserialising: %v", err)
	}
	return buf.Bytes()
}

func base64Of(t *testing.T, raw []byte) string {
	t.Helper()
	packet, err := psbt.NewFromRawBytes(bytes.NewReader(raw), false)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	b64, err := packet.B64Encode()
	if err != nil {
		t.Fatalf("base64-encoding: %v", err)
	}
	return b64
}

func hexOf(raw []byte) string { return hex.EncodeToString(raw) }
