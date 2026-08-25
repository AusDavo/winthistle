package arm_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/AusDavo/winthistle/internal/abort"
	"github.com/AusDavo/winthistle/internal/lnd"
	"github.com/AusDavo/winthistle/internal/regtestenv"
)

// How long after our OpenChannel call a peer's reservation is certainly gone.
//
// TestWhoOwnsTheTenMinuteClock measured 10m41s at v0.19.3-beta and 10m14s at
// v0.21.2-beta: ten minutes of
// DefaultReservationTimeout plus up to a minute of DefaultZombieSweeperInterval
// granularity. Neither is adjustable in a release build.
const peerWindowCertainlyOver = 11*time.Minute + 30*time.Second

// staggerGap is how long after the lapsing stream the surviving one opens.
//
// It has to be long enough that the first peer is swept well before the second
// is anywhere near its own window, and short enough that the second is still
// comfortably inside it when the batch finalizes. Six minutes leaves the
// survivor about five minutes of margin at the moment it matters.
const staggerGap = 6 * time.Minute

// One peer's funding window expires while the rest of the batch is still good,
// and the batch is finalized anyway — one receipt in hand, one refusal.
//
// This is the scenario the triage listed as "funding timeout expires with one
// receipt outstanding", and driving it needs a stagger rather than a wait. Every
// peer's clock starts at its own accept_channel, so a batch opened together
// lapses together: waiting eleven minutes on a two-channel batch produces two
// dead reservations and no receipt at all. Opening the doomed stream six minutes
// before the survivor is what makes exactly one of them expire.
//
// What it establishes that the stub tests in mid_batch_test.go cannot:
//
//   - psbt_finalize against a swept reservation is refused by our *own* node,
//     synchronously, rather than hanging. LND drops the funding intent when the
//     peer's cancellation arrives, so there is nothing left to finalize against.
//   - the refusal arrives at psbt_finalize and not at psbt_verify, because
//     psbt_verify is local and touches no reservation timer — the batch verifies
//     clean minutes before one of its members is gone.
//   - and the channel that did arm is genuinely armed. A peer disappearing from
//     the batch does not take the others' commitment signatures with it. The
//     teardown is what proves it rather than the receipt: AbandonPending refuses
//     a channel LND does not hold as a pending open, so one abandon reported
//     clean is LND agreeing the channel is in its channel database.
//
// Around eleven minutes of wall clock — measured 10m13s and 10m39s on two runs,
// the sweeper's granularity — so it is gated the same way the clock test is.
// `make test` is not the place for it. It holds two of alice's funding streams
// and the cold wallet's coin locks throughout, so nothing else harness-backed
// may run alongside it.
func TestOnePeersWindowExpiringLeavesTheRestOfTheBatchArmed(t *testing.T) {
	if os.Getenv(slowEnv) == "" {
		t.Skipf("takes ~11 minutes of wall clock; set %s=1 to run it", slowEnv)
	}

	env := regtestenv.Start(t)
	peers := env.Peers(t)
	if len(peers) < 2 {
		t.Skipf("this test needs 2 peers, alice has %d — run: make -C regtest reset", len(peers))
	}

	// One block first, and it is not decoration. LND calls itself unsynced when
	// its best header is more than two hours old — btcwallet's IsSynced, "if the
	// timestamp on the best header is more than 2 hours in the past" — and
	// refuses OpenChannel with "channels cannot be created before the wallet is
	// fully synced". A test that spans twelve minutes on an idle regtest can
	// cross that boundary between its first stream and its second, and the first
	// run of this one did exactly that. Mining once buys two hours of headroom.
	// Nothing is in the mempool yet, so this confirms nothing but itself.
	env.Mine(t, 1)

	// The doomed one first, so its clock is the one that runs out.
	opened := time.Now()
	lapsing := env.OpenShimStream(t, peers[0], fixtureChannelSat)
	t.Logf("stream to %s open at t+0, funding %s", peers[0][:16], lapsing.FundingAddress)

	t.Logf("waiting %s before opening the second stream", staggerGap)
	time.Sleep(staggerGap)

	surviving := env.OpenShimStream(t, peers[1], fixtureChannelSat)
	t.Logf("stream to %s open at %s, funding %s", peers[1][:16],
		time.Since(opened).Round(time.Second), surviving.FundingAddress)

	// One transaction paying both, built and verified while both reservations are
	// still alive. psbt_verify is local — it validates the packet against the
	// reservation and touches no timer on either side of the wire — so this
	// passing now says nothing about whether it would pass later.
	streams := []*regtestenv.Stream{lapsing, surviving}
	funded := env.BuildFundingPSBT(t, env.Cold, streams, fixtureFeeRate)
	env.ReleaseLocksAtCleanup(t, env.Cold, funded.Inputs)
	for _, s := range streams {
		env.Verify(t, s, funded.Base64)
	}
	rawTxHex, txID := env.SignAndCombine(t, funded)
	t.Logf("both streams verified %s and the batch is signed: %s",
		time.Since(opened).Round(time.Second), txID)

	// Now watch the first peer give up, rather than sleeping past it and assuming.
	// AwaitStreamFailure is only safe before psbt_finalize, which is why this
	// happens here and not after the batch is armed.
	wait := time.Until(opened.Add(peerWindowCertainlyOver))
	elapsed, err := env.AwaitStreamFailure(t, lapsing, wait+2*time.Minute)
	if err == nil {
		t.Fatalf("the first stream was still alive %s after it opened. Either the "+
			"peer's reservation timeout moved or its sweeper is not running — and "+
			"this test is then measuring nothing",
			time.Since(opened).Round(time.Second))
	}
	// AwaitStreamFailure measures from its own call, not from the stream opening,
	// which is the same thing in TestWhoOwnsTheTenMinuteClock and is six minutes
	// out here. Report both, and check the one that means something.
	gaveUp := time.Since(opened)
	t.Logf("the first peer gave up %s after its OpenChannel call (%s after we "+
		"started watching): %v", gaveUp.Round(time.Second),
		elapsed.Round(time.Second), err)
	if gaveUp < 9*time.Minute {
		t.Errorf("the first peer gave up %s after opening, well inside the ten "+
			"minutes the phase model budgets for. This test is then not the "+
			"scenario it claims: something other than the reservation timeout "+
			"ended that stream", gaveUp.Round(time.Second))
	}

	// The survivor's window is what the rest of this depends on, so say where it
	// stands rather than discovering it in a failure.
	left := staggerGap + peerWindowCertainlyOver - time.Since(opened)
	t.Logf("the second stream has roughly %s of its own window left",
		left.Round(time.Second))
	if left < time.Minute {
		t.Fatalf("no margin left on the surviving stream (%s) — the stagger is "+
			"too small for this cluster", left.Round(time.Second))
	}

	// The receipt that did arrive.
	cp := env.Finalize(t, surviving, rawTxHex)
	t.Logf("the surviving channel armed at %s", cp)

	// And the one that did not. The refusal is our own node's, synchronous, and
	// specific: LND dropped the funding intent when the peer's cancellation
	// arrived, so there is nothing for psbt_finalize to act on. Measured against
	// alice on 2026-08-25 — "no funding intent found for pendingChannelID(…)",
	// which is chanfunding's own wording and names the channel, so it is safe to
	// put in front of an operator as it stands.
	err = env.TryFinalize(t, lapsing, rawTxHex)
	if err == nil {
		t.Fatal("psbt_finalize succeeded against a reservation the peer had already " +
			"swept. If LND accepted it, this batch has a channel whose peer is not " +
			"holding a commitment signature — and the receipt would be a lie")
	}
	t.Logf("psbt_finalize for the lapsed stream: %v", err)

	// I-1, in the state that matters most: one of n armed and nothing published.
	// A batch this shape is exactly what must never reach the network — publishing
	// it funds a channel that no peer is holding a commitment signature for.
	if env.InMempool(t, txID) {
		t.Fatalf("the funding transaction %s is in the mempool with one of two "+
			"channels armed — I-1 is broken", txID)
	}

	// The teardown, which is the whole cost of the scenario: one abandon and one
	// shim cancel that may already be gone.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	rep, err := abort.Run(ctx, env.Alice.Lightning, abort.Target{
		Channels: []lnd.ChannelPoint{cp},
		Shims:    []lnd.PendingChanID{lapsing.PendingChanID},
	}, func(context.Context, abort.BluntRequest) (bool, error) { return true, nil })
	if err != nil {
		t.Fatalf("tearing the half-armed batch down: %v", err)
	}
	if !rep.Clean() {
		t.Fatalf("the abort left failures: %v", rep.Failures)
	}
	if len(rep.Abandoned) != 1 {
		t.Errorf("abandoned %d channels, want the 1 that armed", len(rep.Abandoned))
	}
	if env.InMempool(t, txID) {
		t.Fatalf("%s reached a mempool at some point — nothing here may publish", txID)
	}
}
