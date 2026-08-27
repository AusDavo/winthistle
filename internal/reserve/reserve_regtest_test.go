package reserve_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/AusDavo/winthistle/internal/abort"
	"github.com/AusDavo/winthistle/internal/lnd"
	"github.com/AusDavo/winthistle/internal/prose"
	"github.com/AusDavo/winthistle/internal/regtestenv"
	"github.com/AusDavo/winthistle/internal/reserve"
	"github.com/lightningnetwork/lnd/lnrpc"
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

	// The verify figure does not depend on the size of the batch, by construction:
	// it is RequiredReserve(additional=1) whatever n is. That is a statement about
	// the call, not about the batch — a later verify in the same batch can be
	// judged against a larger figure, which is
	// TestALaterVerifyCountsAnEarlierChannelInTheBatch's subject.
	if one.AtVerify != three.AtVerify {
		t.Errorf("AtVerify moved with the batch size (%d for 1, %d for 3) — it is "+
			"RequiredReserve(additional=1) regardless of n",
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
// its pending-channel slots — see HANDOFF.md's "the peers do not forget an
// aborted batch", for why that matters to a suite that runs repeatedly.
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

// TestALaterVerifyCountsAnEarlierChannelInTheBatch is #51's measurement.
//
// This package's doc used to conclude that "every verify in a batch sees the same
// pre-batch count", and it reasoned from an ordering the inversion deleted:
// CompleteReservation ran "after psbt_finalize", and there is no psbt_finalize in
// this build. Under skip_finalize the funding flow completes from psbt_verify, so
// the question is live and it is not a reading — nothing in LND orders one
// channel's CompleteReservation against another channel's psbt_verify, and
// whether the count has grown by the time a later verify runs is a matter of
// timing. So it is measured here.
//
// It is arranged to be deterministic rather than raced. The first channel's
// chan_pending is read before the second and third are verified, and chan_pending
// arrives strictly after CompleteReservation's SyncPending (lnwallet/wallet.go:2534)
// has written the channel into the database — so at the second verify the count
// CurrentNumAnchorChans reads has definitely grown. The natural, back-to-back
// case is measured too, between the second verify and the third, and only logged:
// that one is a race and an assertion on it would be a flake.
//
// What it asserts: the count does grow, mid-batch, before a later verify — and
// on a node under LND's 100,000-sat reserve cap, so does the figure. Finding.AtVerify
// is RequiredReserve(additional=1) read in Phase 0, and after channel 1 is
// pending the same call returns one channel's worth more. That is the pre-flight's
// own number going stale inside clock A, which is the concrete form of the
// finding.
//
// What it does not assert, because this test cannot establish it: that the worst
// case is survivable. It is, but for a reason that lives in the source rather
// than here — plan.ReserveTopUp aims at the larger of the two figures, and
// CheckReservedValue credits a top-up output inside the batch, so the wallet
// clears existing + n at every verify. On a capped node the figure cannot move at
// all, so the three verifies passing below is not evidence about the arithmetic.
// It is only evidence that nothing else refused them.
func TestALaterVerifyCountsAnEarlierChannelInTheBatch(t *testing.T) {
	env := regtestenv.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)

	// LND refuses to open a channel while its wallet is behind, and a cluster
	// left idle for a couple of hours is behind.
	env.Mine(t, 1)

	const n = 3
	all := env.Peers(t)
	if len(all) < n {
		t.Skipf("this test needs %d peers, alice has %d", n, len(all))
	}

	// The pre-flight has to say clear before anything below is evidence: a verify
	// refused because alice's own wallet is short would look like a verify refused
	// because the count moved, and they are different findings.
	before, err := reserve.Check(ctx, env.Alice.WalletKit, reserve.Batch{Public: n})
	if err != nil {
		t.Fatalf("the anchor-reserve pre-flight: %v", err)
	}
	if before.Verdict() != reserve.Clear {
		t.Fatalf("the harness node does not clear its own reserve, so a refused "+
			"verify below would not be evidence of anything:\n%s", before.Report())
	}
	capped := before.AtVerify == reserve.MaxReserve
	if capped {
		t.Logf("alice is at LND's %d-sat reserve cap, so the count moving cannot "+
			"move the figure on this harness. The count itself is still measured, "+
			"but the figure going stale is not — run this against a node with "+
			"fewer than ten announced anchor channels for that half",
			reserve.MaxReserve)
	}

	streams := make([]*regtestenv.Stream, 0, n)
	for _, p := range all[:n] {
		streams = append(streams, env.OpenShimStream(t, p, fixtureChannelSat))
	}

	funded := env.BuildFundingPSBT(t, env.Cold, streams, 5)
	env.ReleaseLocksAtCleanup(t, env.Cold, funded.Inputs)
	t.Logf("unsigned batch transaction %s, %d channels", funded.TxID, n)

	// Whatever reaches chan_pending has to be abandoned, and each abandon costs
	// that peer one pending-channel slot for LND's ~2016-block forget horizon.
	left := abort.Target{}
	for _, s := range streams {
		left.Shims = append(left.Shims, s.PendingChanID)
	}
	abandonAtCleanup(t, env, &left)

	baseline := pendingOpenCount(ctx, t, env)
	t.Logf("pending opens before the first verify: %d", baseline)

	// Channel 1, and its receipt before anything else is verified. This is the
	// deliberate part: chan_pending is emitted after SyncPending, so reading it
	// here removes the race from what follows.
	env.VerifySkippingFinalize(t, streams[0], funded.Base64)
	first := env.Receipt(t, streams[0])
	left.Channels = append(left.Channels, first)
	left.Shims = withoutShim(left.Shims, streams[0].PendingChanID)
	t.Logf("channel 1 to %s is at chan_pending at %s", short(streams[0].PeerPubkey), first)

	grown := pendingOpenCount(ctx, t, env)
	if grown != baseline+1 {
		t.Fatalf("after one chan_pending LND lists %d pending opens, not %d. "+
			"This test's whole subject is the channel database growing mid-batch, "+
			"and PendingChannels is how it is observed", grown, baseline+1)
	}
	t.Logf("pending opens before the second verify: %d — the count "+
		"CurrentNumAnchorChans reads has grown by one, inside the batch, before "+
		"channel 2 is verified", grown)

	// And the figure with it, on a node with room to move. This is the same
	// RequiredReserve(additional=1) call that filled Finding.AtVerify in Phase 0,
	// asked again mid-batch: what it returns now is what channel 2's verify will
	// be judged against, and it is not what the pre-flight reported.
	mid, err := reserve.Check(ctx, env.Alice.WalletKit, reserve.Batch{Public: n})
	if err != nil {
		t.Fatalf("re-running the pre-flight mid-batch: %v", err)
	}
	switch {
	case capped:
		t.Logf("the figure at verify is still %s: alice is at the cap and it has "+
			"nowhere to go", prose.Sats(mid.AtVerify))
	case mid.AtVerify != before.AtVerify+reserve.PerAnchorChannel:
		t.Errorf("the figure at verify went from %d to %d after one channel of the "+
			"batch reached chan_pending; expected one channel's worth more (%d). "+
			"Either the count did not reach CurrentNumAnchorChans or RequiredReserve "+
			"no longer reads it", before.AtVerify, mid.AtVerify, reserve.PerAnchorChannel)
	default:
		t.Logf("the figure at verify moved from %s to %s while the batch was "+
			"half-verified: Finding.AtVerify is the figure the FIRST verify uses, "+
			"and channel 2's is larger", prose.Sats(before.AtVerify),
			prose.Sats(mid.AtVerify))
	}

	// Channel 2, into a database that is already one larger. If the count at
	// verify were still the pre-batch figure, this is where it would show.
	env.VerifySkippingFinalize(t, streams[1], funded.Base64)

	// The natural case, measured and not asserted: channel 3's verify goes out
	// immediately after channel 2's returned, which is arm.Verify's own pace
	// minus its journal write. Whether channel 2 is in the database by now is a
	// race against the peer's funding_signed round trip, and this is the number
	// that says how much margin the app has.
	between := pendingOpenCount(ctx, t, env)
	if between > grown {
		t.Logf("channel 2 was in the database %d channel(s) later, back-to-back, "+
			"with no wait: the natural loop loses this race too", between-grown)
	} else {
		t.Logf("channel 2 was not in the database yet when channel 3 was verified, " +
			"so back-to-back the loop wins the race on this harness. That is a " +
			"measurement of regtest latency and not a property to rely on")
	}

	env.VerifySkippingFinalize(t, streams[2], funded.Base64)

	// Every later verify succeeded — VerifySkippingFinalize fails the test
	// otherwise — and every channel arms.
	for i, s := range streams[1:] {
		cp := env.Receipt(t, s)
		left.Channels = append(left.Channels, cp)
		left.Shims = withoutShim(left.Shims, s.PendingChanID)
		t.Logf("channel %d to %s is at chan_pending at %s", i+2, short(s.PeerPubkey), cp)
	}

	final := pendingOpenCount(ctx, t, env)
	if final != baseline+n {
		t.Fatalf("%d pending opens after a batch of %d, expected %d",
			final, n, baseline+n)
	}

	// I-1's other half, kept here because the transaction was never signed.
	if env.InMempool(t, funded.TxID) {
		t.Fatalf("the unsigned batch transaction %s reached the mempool", funded.TxID)
	}
	t.Logf("%d of %d verified against a channel database that grew under them, "+
		"nothing signed, mempool clear", n, n)
}

// pendingOpenCount is how many channels LND holds as pending opens.
//
// It stands in for CurrentNumAnchorChans, which is not reachable over the RPC
// interface. FetchPendingChannels filters the same FetchAllChannels that
// CurrentNumAnchorChans counts, so a channel listed here is a channel counted
// there — and on this harness nothing but the test under way adds one.
func pendingOpenCount(ctx context.Context, t *testing.T, env *regtestenv.Env) int {
	t.Helper()
	resp, err := env.Alice.Lightning.PendingChannels(ctx, &lnrpc.PendingChannelsRequest{})
	if err != nil {
		t.Fatalf("asking LND for its pending channels: %v", err)
	}
	return len(resp.GetPendingOpenChannels())
}

// abandonAtCleanup takes apart whatever reached chan_pending, and cancels the
// shims that did not.
func abandonAtCleanup(t *testing.T, env *regtestenv.Env, target *abort.Target) {
	t.Helper()
	t.Cleanup(func() {
		ctx, done := context.WithTimeout(context.Background(), 2*time.Minute)
		defer done()

		blunt := abort.Confirmation(func(_ context.Context, req abort.BluntRequest) (bool, error) {
			t.Logf("authorising the blunt abandon of %s (lnd said: %s)",
				req.Channel, req.Rejection)
			return true, nil
		})
		rep, err := abort.Run(ctx, env.Alice.Lightning, *target, blunt)
		if err != nil {
			t.Errorf("aborting what the test left behind: %v", err)
		}
		if rep != nil {
			t.Logf("cleanup: abandoned %d, cancelled %d",
				len(rep.Abandoned), len(rep.Cancelled))
		}
	})
}

// withoutShim drops one pending channel id, so a stream that reached
// chan_pending stops being listed as a cancellable shim.
func withoutShim(ids []lnd.PendingChanID, id lnd.PendingChanID) []lnd.PendingChanID {
	out := ids[:0]
	for _, have := range ids {
		if have != id {
			out = append(out, have)
		}
	}
	return out
}

// short is the eight-character peer label the app's own copy uses.
func short(pubkey string) string {
	if len(pubkey) <= 8 {
		return pubkey
	}
	return pubkey[:8]
}
