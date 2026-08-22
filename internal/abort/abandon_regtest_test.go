package abort_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/AusDavo/winthistle/internal/abort"
	"github.com/AusDavo/winthistle/internal/lnd"
	"github.com/AusDavo/winthistle/internal/regtestenv"
	"github.com/lightningnetwork/lnd/lnrpc"
)

// armOneChannel drives steps 2-7 for a single channel and returns its funding
// outpoint, having first asserted the property everything else rests on: that
// chan_pending arrived and the transaction did not.
func armOneChannel(t *testing.T, env *regtestenv.Env) (lnd.ChannelPoint, *regtestenv.Stream) {
	t.Helper()

	s := env.OpenShimStream(t, env.Peers(t)[0], fixtureChannelSat)
	funded := env.BuildFundingPSBT(t, env.Miner, []*regtestenv.Stream{s}, 5)
	t.Cleanup(func() {
		c, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		_, _ = env.Miner.ReleaseLocks(c, funded.Inputs)
	})

	env.Verify(t, s, funded.Base64)
	rawTx, txid := env.SignWithMiner(t, funded.Base64)
	cp := env.Finalize(t, s, rawTx)

	// I-1, observed rather than assumed. chan_pending is emitted only after
	// CompleteReservation has stored the peer's commitment signature, so the
	// channel is force-closeable — and no_publish cleared the bit that gates
	// the broadcast, so the transaction is still ours alone.
	if env.InMempool(t, txid) {
		t.Fatalf("funding tx %s reached the mempool despite no_publish — I-1 is broken", txid)
	}
	if cp.TxID != txid {
		t.Fatalf("chan_pending outpoint %s does not match the funding tx %s", cp, txid)
	}
	return cp, s
}

func isPendingOpen(t *testing.T, env *regtestenv.Env, cp lnd.ChannelPoint) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := env.Alice.Lightning.PendingChannels(ctx, &lnrpc.PendingChannelsRequest{})
	if err != nil {
		t.Fatalf("PendingChannels: %v", err)
	}
	for _, p := range resp.GetPendingOpenChannels() {
		if p.GetChannel().GetChannelPoint() == cp.String() {
			return true
		}
	}
	return false
}

func TestAbandonPendingShimFundedChannel(t *testing.T) {
	env := regtestenv.Start(t)
	ctx := testCtx(t)

	cp, _ := armOneChannel(t, env)
	if !isPendingOpen(t, env, cp) {
		t.Fatalf("%s did not appear among pending opens", cp)
	}

	// First with no authorisation for the blunt flag. Two outcomes are correct:
	// the safe flag is accepted, or it is declined and we stop. Silently
	// escalating is the one thing that must not happen.
	outcome, err := abort.AbandonPending(ctx, env.Alice.Lightning, cp, nil)
	switch {
	case err == nil:
		if outcome.UsedBlunt {
			t.Fatal("the blunt flag was used with no confirmation")
		}
		t.Logf("pending_funding_shim_only accepted the channel outright")

	case errors.Is(err, abort.ErrBluntNotConfirmed):
		// Expected on a plain PSBT open: rpcserver.go infers "shim funded" from
		// ThawHeight > 0, and we set no thaw height, so the safe flag declines.
		t.Logf("safe flag declined as the design predicts; escalation refused: %v", err)

		if !isPendingOpen(t, env, cp) {
			t.Fatal("the channel was removed even though the abort refused to escalate")
		}

		var asked int
		confirm := func(_ context.Context, req abort.BluntRequest) (bool, error) {
			asked++
			if req.Channel != cp {
				t.Errorf("confirmation asked about %s, expected %s", req.Channel, cp)
			}
			if req.Rejection == "" {
				t.Error("confirmation carried no rejection text")
			}
			return true, nil
		}
		outcome, err = abort.AbandonPending(ctx, env.Alice.Lightning, cp, confirm)
		if err != nil {
			t.Fatalf("abandon with confirmation: %v", err)
		}
		if asked != 1 {
			t.Fatalf("confirmation was asked %d times, want 1", asked)
		}
		if !outcome.UsedBlunt {
			t.Fatal("outcome does not record that the blunt flag was used")
		}

	default:
		t.Fatalf("unexpected error: %v", err)
	}

	if isPendingOpen(t, env, cp) {
		t.Fatalf("%s is still pending after being abandoned", cp)
	}
}

// A confirmation that says yes must not be enough to abandon a channel that is
// not pending. This is the hazard the safe flag exists for and the one the blunt
// flag reopens: a confirmed channel removed this way is a channel whose funds
// have no force-close path left.
func TestAbandonRefusesAConfirmedChannel(t *testing.T) {
	env := regtestenv.Start(t)
	ctx := testCtx(t)

	// A normal open, which broadcasts immediately and carries no thaw height —
	// so the safe flag will decline it for the same reason it declines ours, and
	// only the pending check separates the two cases.
	peers := env.Peers(t)
	peer := peers[len(peers)-1]
	cp := env.OpenAndConfirmPlainChannel(t, peer, fixtureChannelSat*4)

	if isPendingOpen(t, env, cp) {
		t.Fatalf("%s is still pending; the test needs it confirmed", cp)
	}

	confirm := func(context.Context, abort.BluntRequest) (bool, error) {
		t.Error("confirmation was offered for a channel that is not pending")
		return true, nil
	}
	_, err := abort.AbandonPending(ctx, env.Alice.Lightning, cp, confirm)
	if !errors.Is(err, abort.ErrNotPending) {
		t.Fatalf("want ErrNotPending, got %v", err)
	}

	if !env.HasOpenChannel(t, cp) {
		t.Fatalf("%s was removed despite the refusal", cp)
	}
}

// The whole abort, over the state a mid-window failure actually leaves: one
// channel that reached chan_pending, one stream that never finalized, and the
// coin locks behind both.
func TestRunAbortsAPartiallyArmedBatch(t *testing.T) {
	env := regtestenv.Start(t)
	ctx := testCtx(t)

	armed, _ := armOneChannel(t, env)
	unfinished := env.OpenShimStream(t, env.Peers(t)[0], fixtureChannelSat)
	funded := env.BuildFundingPSBT(t, env.Cold, []*regtestenv.Stream{unfinished}, 5)

	target := abort.Target{
		Channels: []lnd.ChannelPoint{armed},
		Shims:    []lnd.PendingChanID{unfinished.PendingChanID},
		Locks:    funded.Inputs,
	}

	alwaysConfirm := func(context.Context, abort.BluntRequest) (bool, error) { return true, nil }
	rep, err := abort.Run(ctx, env.Alice.Lightning, env.Cold, target, alwaysConfirm)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !rep.Clean() {
		t.Fatalf("abort left failures: %v", rep.Failures)
	}
	if len(rep.Abandoned) != 1 || len(rep.Cancelled) != 1 {
		t.Fatalf("report incomplete: %+v", rep)
	}
	if len(rep.LocksFreed) != len(funded.Inputs) {
		t.Fatalf("freed %d of %d coin locks", len(rep.LocksFreed), len(funded.Inputs))
	}
	if isPendingOpen(t, env, armed) {
		t.Fatalf("%s is still pending after the abort", armed)
	}

	// Running the same abort again must be quiet, not an error: this is what a
	// resumed recovery after a crash looks like.
	rep2, err := abort.Run(ctx, env.Alice.Lightning, env.Cold, target, alwaysConfirm)
	if err != nil {
		t.Fatalf("second Run should be a no-op, got: %v", err)
	}
	if len(rep2.Cancelled) != 1 || !rep2.Cancelled[0].AlreadyGone {
		t.Fatalf("second Run did not report the shim as already gone: %+v", rep2.Cancelled)
	}
	if len(rep2.LocksFreed) != 0 {
		t.Fatalf("second Run freed %v, but nothing was locked", rep2.LocksFreed)
	}
}
