package reserve_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/AusDavo/winthistle/internal/abort"
	"github.com/AusDavo/winthistle/internal/regtestenv"
	"github.com/AusDavo/winthistle/internal/reserve"
)

const fixtureChannelSat = 250_000

// lndsRefusal is the error text psbt_verify returns when the node's own wallet is
// short. From lnwallet/wallet.go:
//
//	ErrReservedValueInvalidated = errors.New("reserved wallet balance " +
//	        "invalidated: transaction would leave insufficient funds for " +
//	        "fee bumping anchor channel closings (see debug log for details)")
//
// Matched on text because it arrives over gRPC as a message with no code behind
// it. If LND rewords it this test fails, which is the right direction: the copy
// in report.go quotes it, and a quote that no longer matches is worse than none.
const lndsRefusal = "reserved wallet balance invalidated"

// TestCheckPredictsWhatVerifyDoes is the whole claim of this package: the
// pre-flight's verdict is the verdict psbt_verify will reach, and it is available
// before anyone is asked for a signature.
//
// It is proved in both directions against a live node — refused when the node's
// own wallet is empty, accepted when it is not — because a check that only ever
// says "fine" would also pass a one-directional test.
func TestCheckPredictsWhatVerifyDoes(t *testing.T) {
	env := regtestenv.Start(t)
	ctx := testCtx(t)
	peer := env.Peers(t)[0]

	// A funded node first, so the test knows the fixture is sound before it
	// starts breaking things.
	before, err := reserve.Check(ctx, env.Alice.WalletKit, reserve.Batch{Public: 1})
	if err != nil {
		t.Fatalf("Check on a healthy node: %v", err)
	}
	if before.Verdict() != reserve.Clear {
		t.Fatalf("the harness node does not clear its own reserve before the "+
			"test starts, so nothing below proves anything:\n%s", before.Report())
	}

	// Lease every coin alice has. This is the finding reproduced: LND's balance
	// for this check comes from ListUnspentWitness, which skips leased outpoints,
	// so a node holding 5 BTC in five leased UTXOs is a node with nothing.
	leased, release := env.LeaseAllUnspent(t)
	if leased <= 0 {
		t.Fatal("leased nothing — alice's wallet was already empty, so the " +
			"refusal below would not be evidence of anything")
	}

	short, err := reserve.Check(ctx, env.Alice.WalletKit, reserve.Batch{Public: 1})
	if err != nil {
		t.Fatalf("Check on a leased-out node: %v", err)
	}
	if short.Verdict() != reserve.WouldBeRefused {
		t.Fatalf("with %d sat leased and nothing unlocked, Check said %s:\n%s",
			leased, short.Verdict(), short.Report())
	}
	if short.Available != 0 {
		t.Errorf("Available = %d with every coin leased, want 0", short.Available)
	}
	if short.Leased != leased {
		t.Errorf("Leased = %d, want the %d we leased", short.Leased, leased)
	}

	// Now find out whether LND agrees. Everything up to psbt_verify is free and
	// cancellable, so this costs nothing but the streams it opens.
	refusal := tryVerifyOneChannel(t, env, peer)
	if refusal == nil {
		t.Fatal("psbt_verify succeeded on a node with no unlocked coins — the " +
			"pre-flight would be blocking a batch that would have worked")
	}
	if !strings.Contains(refusal.Error(), lndsRefusal) {
		t.Fatalf("psbt_verify failed, but not over the reserve: %v", refusal)
	}
	t.Logf("lnd refused as predicted: %v", refusal)

	// Give the coins back and check the prediction flips with the node.
	release()

	after, err := reserve.Check(ctx, env.Alice.WalletKit, reserve.Batch{Public: 1})
	if err != nil {
		t.Fatalf("Check after releasing the leases: %v", err)
	}
	if after.Verdict() != reserve.Clear {
		t.Fatalf("the leases were released but Check still says %s:\n%s",
			after.Verdict(), after.Report())
	}

	if err := tryVerifyOneChannel(t, env, peer); err != nil {
		t.Fatalf("Check said clear but psbt_verify refused: %v", err)
	}
}

// TestCheckAgreesWithLNDsOwnArithmetic: the figures the report quotes have to be
// LND's, not a reimplementation of them. RequiredReserve is the only place they
// can come from, so check the shape of what it returns rather than trusting a
// constant.
func TestCheckAgreesWithLNDsOwnArithmetic(t *testing.T) {
	env := regtestenv.Start(t)
	ctx := testCtx(t)

	one, err := reserve.Check(ctx, env.Alice.WalletKit, reserve.Batch{Public: 1})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	three, err := reserve.Check(ctx, env.Alice.WalletKit, reserve.Batch{Public: 3})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}

	// The verify figure does not depend on the size of the batch: at verify time
	// none of the batch is in the channel database, so it is always
	// CurrentNumAnchorChans + 1.
	if one.AtVerify != three.AtVerify {
		t.Errorf("AtVerify moved with the batch size (%d for 1, %d for 3) — the "+
			"count at verify is existing + 1 regardless of n",
			one.AtVerify, three.AtVerify)
	}
	if one.AtVerify != one.NowRequired+reserve.PerAnchorChannel &&
		one.AtVerify != reserve.MaxReserve {

		t.Errorf("AtVerify = %d with NowRequired = %d: expected one more channel's "+
			"worth (%d), or the cap (%d)",
			one.AtVerify, one.NowRequired, reserve.PerAnchorChannel, reserve.MaxReserve)
	}

	// The post-batch figure does grow with n, until it hits the cap.
	if three.AfterBatch < one.AfterBatch {
		t.Errorf("AfterBatch fell as the batch grew: %d for 3, %d for 1",
			three.AfterBatch, one.AfterBatch)
	}
	if three.AfterBatch > reserve.MaxReserve {
		t.Errorf("AfterBatch = %d, above LND's cap of %d",
			three.AfterBatch, reserve.MaxReserve)
	}
}

// TestAnAllPrivateBatchIsNotJudgedAtAll. enforceNewReservedValue returns before
// it counts anything when the channel is unannounced, so a node that cannot
// clear the reserve can still open private channels. Worth having as a test
// rather than a comment: it is the difference between a blocked run and a
// working one.
func TestAnAllPrivateBatchIsNotJudgedAtAll(t *testing.T) {
	env := regtestenv.Start(t)
	ctx := testCtx(t)

	leased, _ := env.LeaseAllUnspent(t)
	if leased <= 0 {
		t.Fatal("leased nothing — alice's wallet was already empty")
	}

	f, err := reserve.Check(ctx, env.Alice.WalletKit, reserve.Batch{Private: 3})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if f.Verdict() != reserve.NotApplicable {
		t.Errorf("an all-private batch was judged %s:\n%s", f.Verdict(), f.Report())
	}
	if f.Blocking() {
		t.Error("an all-private batch was blocked over the anchor reserve")
	}
}

// tryVerifyOneChannel walks steps 2 to 5 for a single channel and reports what
// psbt_verify said, then takes the stream back down.
//
// Nothing is finalized, so nothing reaches chan_pending and no peer spends one of
// its pending-channel slots — see HANDOFF.md, finding 6, for why that matters to
// a suite that runs repeatedly.
func tryVerifyOneChannel(t *testing.T, env *regtestenv.Env, peer string) error {
	t.Helper()

	s := env.OpenShimStream(t, peer, fixtureChannelSat)
	funded := env.BuildFundingPSBT(t, env.Cold, []*regtestenv.Stream{s}, 5)

	t.Cleanup(func() {
		c, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		if _, err := env.Cold.ReleaseLocks(c, funded.Inputs); err != nil {
			t.Errorf("releasing coin locks: %v", err)
		}
		if err := abort.CancelShim(c, env.Alice.Lightning, s.PendingChanID); err != nil {
			t.Errorf("cancelling the shim: %v", err)
		}
	})

	return env.TryVerify(t, s, funded.Base64)
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	return ctx
}
