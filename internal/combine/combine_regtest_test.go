package combine_test

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/AusDavo/winthistle/internal/abort"
	"github.com/AusDavo/winthistle/internal/combine"
	"github.com/AusDavo/winthistle/internal/plan"
	"github.com/AusDavo/winthistle/internal/prose"
	"github.com/AusDavo/winthistle/internal/regtestenv"
	"github.com/AusDavo/winthistle/internal/regtestenv/coldwallet"
)

// fixtureChannelSat matches internal/abort's and internal/plan's, so a peer that
// accepts one fixture's channel accepts the others'.
const fixtureChannelSat = 250_000

// fixtureFeeRate is what these fixtures target, in sat/vB.
const fixtureFeeRate = 10.0

func harnessCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// armed is a real batch, held at the point where the signers have it: n funding
// streams open, one transaction built by the watch-only cold wallet, verified by
// the plan and by every stream.
type armed struct {
	env     *regtestenv.Env
	streams []*regtestenv.Stream
	plan    *plan.Plan
	built   coldwallet.Built
}

// setUpBatch drives steps 2 through 5 for n channels and leaves the streams open.
//
// Nothing here finalizes, so every stream is still a shim and cancelling it costs
// the peers nothing they will not forget on their own. That is what makes it
// affordable to run this fixture repeatedly against the same harness.
func setUpBatch(t *testing.T, n int) *armed {
	t.Helper()
	env := regtestenv.Start(t)
	ctx := harnessCtx(t)

	peers := env.Peers(t)
	if len(peers) < n {
		t.Skipf("this test needs %d peers, alice has %d", n, len(peers))
	}

	streams := make([]*regtestenv.Stream, 0, n)
	for _, p := range peers[:n] {
		streams = append(streams, env.OpenShimStream(t, p, fixtureChannelSat))
	}
	t.Cleanup(func() {
		c, done := context.WithTimeout(context.Background(), 60*time.Second)
		defer done()
		for _, s := range streams {
			if err := abort.CancelShim(c, env.Alice.Lightning, s.PendingChanID); err != nil {
				t.Errorf("cancelling shim %s: %v", s.PendingChanID, err)
			}
		}
	})

	// Directed mode's own coin policy: judge every coin, then lock the ones LND
	// would refuse so Core's selection cannot reach them.
	coins, err := coldwallet.SelectCoins(ctx, env.Cold, 1)
	if err != nil {
		t.Fatalf("selecting the cold wallet's coins: %v", err)
	}
	fenced, err := coldwallet.FenceOff(ctx, env.Cold, coins)
	if err != nil {
		t.Fatalf("fencing off the coins the batch may not spend: %v", err)
	}
	env.ReleaseLocksAtCleanup(t, env.Cold, fenced)

	change, err := coldwallet.ChangeAddress(ctx, env.Cold)
	if err != nil {
		t.Fatalf("asking the cold wallet for a change address: %v", err)
	}

	outputs := make([]coldwallet.Output, 0, n)
	p := &plan.Plan{
		Chain:  "regtest",
		Change: plan.Change{Address: change},
		Inputs: plan.Inputs{MinConfirmations: 1},
	}
	for _, s := range streams {
		outputs = append(outputs, coldwallet.Output{
			Address: s.FundingAddress, AmountSat: s.FundingAmount,
		})
		p.Channels = append(p.Channels, plan.Channel{
			Peer:          s.PeerPubkey,
			PendingChanID: s.PendingChanID.String(),
			Address:       s.FundingAddress,
			AmountSat:     s.FundingAmount,
		})
	}

	built, err := coldwallet.Build(ctx, env.Cold, coldwallet.BuildRequest{
		Outputs:          outputs,
		ChangeAddress:    change,
		FeeRateSatPerVB:  fixtureFeeRate,
		MinConfirmations: 1,
	})
	if err != nil {
		t.Fatalf("building the batch transaction: %v", err)
	}
	env.ReleaseLocksAtCleanup(t, env.Cold, built.Inputs)

	v, err := p.Verify(built.Raw)
	if err != nil {
		t.Fatalf("verifying the transaction Core built: %v", err)
	}
	if !v.OK() {
		t.Fatalf("the verifier refused the transaction Core built to the plan:\n%s",
			v.Report())
	}
	if v.UnsignedTxID != built.TxID {
		t.Fatalf("the verifier hashed %s, Core says %s", v.UnsignedTxID, built.TxID)
	}

	// And LND, on the same bytes, for every stream — before anything is signed.
	for _, s := range streams {
		if err := env.TryVerify(t, s, built.PSBT); err != nil {
			t.Fatalf("psbt_verify for %s: %v", s.PendingChanID, err)
		}
	}
	t.Logf("%d streams verified %s at %.2f sat/vB, %d vB", len(streams), built.TxID,
		v.FeeRate, v.Size.Vsize)

	return &armed{env: env, streams: streams, plan: p, built: built}
}

// TestTheAppCombinesTheColdWalletsPartialsAndNothingElseCould is I-2 against the
// real fixture, and it is the point of this whole package.
//
// regtest/cold-wallet.py's cold1 and cold2 each hold one key of a 2-of-2. Neither
// completes the transaction; the app combines them. `make -C regtest verify`
// already proves the fixture behaves that way in Python — what this proves is
// that the combining step has moved into the app, so no external party is ever in
// a position to publish.
func TestTheAppCombinesTheColdWalletsPartialsAndNothingElseCould(t *testing.T) {
	b := setUpBatch(t, 2)
	env := b.env

	parts := env.SignWithColdWallet(t, b.built.PSBT)

	// Each half alone is a dead end, and it has to be checked through the app's
	// own finalizer rather than through Core's completeness flag: "Core said not
	// complete" and "this build cannot finalize it" are different claims, and the
	// second is the one I-2 rests on.
	for _, alone := range parts {
		merged, err := combine.Merge(b.built.Raw, []combine.Part{alone})
		if err != nil {
			t.Fatalf("%s alone did not merge: %v", alone.Label, err)
		}
		if _, err := combine.Finalize(merged); err == nil {
			t.Fatalf("%s alone produced a broadcastable transaction", alone.Label)
		} else {
			t.Logf("%s alone: %v", alone.Label, err)
		}
	}

	final, v, err := combine.Complete(b.plan, b.built.Raw, parts)
	if err != nil {
		t.Fatalf("combining the cold wallet's partials: %v", err)
	}
	if !v.OK() {
		t.Fatalf("the plan refused the transaction the merge produced:\n%s", v.Report())
	}

	// I-3: the transaction LND committed to at psbt_verify is the transaction that
	// came out.
	if final.TxID != b.built.TxID {
		t.Fatalf("txid moved: %s, was %s", final.TxID, b.built.TxID)
	}

	rawHex := hex.EncodeToString(final.RawTx)
	if ok, why := env.AcceptsToMempool(t, rawHex); !ok {
		t.Fatalf("testmempoolaccept refused the transaction this build assembled: %s\n"+
			"Every witness executed against its own script locally, so a refusal "+
			"here is node policy rather than a bad signature — and knowing which is "+
			"the point of running both.", why)
	}

	// testmempoolaccept validates without relaying. Prove that.
	if env.InMempool(t, final.TxID) {
		t.Fatalf("%s is in the mempool. Nothing in this test publishes, and the "+
			"batch is not even armed yet", final.TxID)
	}

	t.Logf("2-of-2 combined in-app: %s, %d vB, %s fee, signers %v",
		final.TxID, final.Vsize, prose.Sats(final.FeeSat), final.Signers)
}

// TestOurFinalizerAgreesWithCoresByteForByte answers the open question in
// docs/design.html — "does Core's finalizepsbt handle every signer's output for
// the specific multisig descriptor in use, or is a fallback finalizer needed?" —
// from the other direction.
//
// The app does not call Core's finalizer and does not need to: internal/combine
// merges the partials and btcd's MaybeFinalizeAll assembles the witness, which is
// then executed against each input's script before anything else sees it. So the
// question stops being load-bearing.
//
// This test is the corroboration rather than the answer. Core's combinepsbt plus
// finalizepsbt, on the same two partials, produces the same transaction to the
// byte. That is a strong signal for a wsh(sortedmulti(2,…)) spend, because the
// witness stack order is derived from the witness script by both implementations
// and the signatures themselves are deterministic. If a future descriptor ever
// makes the two disagree, this is where it shows — and the app's own answer is
// still the one that ships.
func TestOurFinalizerAgreesWithCoresByteForByte(t *testing.T) {
	b := setUpBatch(t, 1)
	env := b.env
	ctx := harnessCtx(t)

	parts := env.SignWithColdWallet(t, b.built.PSBT)

	ours, _, err := combine.Complete(b.plan, b.built.Raw, parts)
	if err != nil {
		t.Fatalf("combining in-app: %v", err)
	}

	// Core's route, for comparison only. Note what it involves: after
	// combinepsbt the node holds a PSBT that finalizepsbt can complete, which is
	// precisely the position I-2 says no external party should be in. It is
	// tolerable here because this test never publishes and asserts the mempool
	// stays empty.
	b64 := make([]string, 0, len(parts))
	for _, p := range parts {
		b64 = append(b64, base64.StdEncoding.EncodeToString(p.PSBT))
	}
	var combined string
	if err := env.Node.Call(ctx, "combinepsbt", []any{b64}, &combined); err != nil {
		t.Fatalf("combinepsbt: %v", err)
	}
	var finalized struct {
		Hex      string `json:"hex"`
		Complete bool   `json:"complete"`
	}
	if err := env.Node.Call(ctx, "finalizepsbt", []any{combined}, &finalized); err != nil {
		t.Fatalf("finalizepsbt: %v", err)
	}
	if !finalized.Complete {
		t.Fatalf("Core's finalizepsbt could not complete a 2-of-2 it has both " +
			"signatures for, which is the failure the design's open question was " +
			"about. The app does not depend on this, and now there is a record of it")
	}

	if got := hex.EncodeToString(ours.RawTx); got != finalized.Hex {
		t.Errorf("this build and Core finalized the same partials differently.\n"+
			"  ours: %s\n  core: %s\nThe app's answer is the one that ships; this is "+
			"worth understanding before the next descriptor shape.", got, finalized.Hex)
	} else {
		t.Logf("identical to Core's combinepsbt + finalizepsbt (%d bytes), and the "+
			"app needed neither", len(ours.RawTx))
	}

	if env.InMempool(t, ours.TxID) {
		t.Fatalf("%s reached the mempool", ours.TxID)
	}
}

// TestADeviceThatReSignsAModifiedTransactionIsNamed.
//
// The unit tests cover the refusal; this covers it with a real signer's output,
// where the packet also carries the derivations and witness script Core attaches
// and there is more for a sloppy merge to get wrong.
func TestADeviceThatReSignsAModifiedTransactionIsNamed(t *testing.T) {
	b := setUpBatch(t, 1)
	env := b.env
	ctx := harnessCtx(t)

	first := env.SignPartial(t, regtestenv.Cold1, b.built.PSBT)

	// A second transaction, built from the same wallet a moment later, so it is
	// plausible rather than obviously hostile: the same outputs, different coins.
	other, err := coldwallet.Build(ctx, env.Cold, coldwallet.BuildRequest{
		Outputs: []coldwallet.Output{{
			Address: b.streams[0].FundingAddress, AmountSat: b.streams[0].FundingAmount,
		}},
		ChangeAddress:    b.plan.Change.Address,
		FeeRateSatPerVB:  fixtureFeeRate + 5,
		MinConfirmations: 1,
	})
	if err != nil {
		t.Skipf("the cold wallet could not fund a second transaction: %v", err)
	}
	env.ReleaseLocksAtCleanup(t, env.Cold, other.Inputs)
	if other.TxID == b.built.TxID {
		t.Skip("Core built the same transaction twice, so there is nothing to detect")
	}
	second := env.SignPartial(t, regtestenv.Cold2, other.PSBT)

	_, err = combine.Merge(b.built.Raw, []combine.Part{first, second})
	if err == nil {
		t.Fatal("two signatures over two different transactions were combined")
	}
	var de *combine.DeviceError
	if !errors.As(err, &de) || de.Label != regtestenv.Cold2 {
		t.Fatalf("the refusal does not name cold2: %v", err)
	}
	t.Logf("refused, naming the device: %v", err)
}
