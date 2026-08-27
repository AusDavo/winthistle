package abort_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/AusDavo/winthistle/internal/abort"
	"github.com/AusDavo/winthistle/internal/bitcoind"
	"github.com/AusDavo/winthistle/internal/lnd"
	"github.com/AusDavo/winthistle/internal/regtestenv"
)

const fixtureChannelSat = 250_000

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestCancelShimReleasesAnUnfundedStream(t *testing.T) {
	env := regtestenv.Start(t)
	ctx := testCtx(t)

	s := env.OpenShimStream(t, env.Peers(t)[0], fixtureChannelSat)
	if s.FundingAddress == "" {
		t.Fatal("psbt_fund carried no funding address")
	}
	if s.FundingAmount != fixtureChannelSat {
		t.Fatalf("lnd wants %d sat, we asked for %d", s.FundingAmount, fixtureChannelSat)
	}

	if err := abort.CancelShim(ctx, env.Alice.Lightning, s.PendingChanID); err != nil {
		t.Fatalf("CancelShim: %v", err)
	}

	// Safe to call twice: the second cancel finds nothing, which is the end
	// state an abort wants and must not be reported as a failure.
	err := abort.CancelShim(ctx, env.Alice.Lightning, s.PendingChanID)
	if !errors.Is(err, abort.ErrNoShim) {
		t.Fatalf("second cancel: want ErrNoShim, got %v", err)
	}
}

func TestCancelShimOnAnUnknownIDIsDistinguishable(t *testing.T) {
	env := regtestenv.Start(t)
	ctx := testCtx(t)

	id, err := lnd.NewPendingChanID()
	if err != nil {
		t.Fatal(err)
	}
	if err := abort.CancelShim(ctx, env.Alice.Lightning, id); !errors.Is(err, abort.ErrNoShim) {
		t.Fatalf("want ErrNoShim for an id lnd never saw, got %v", err)
	}
}

// Answers an open question from docs/design.html: "Does shim_cancel still
// succeed after a successful psbt_verify? The whole re-arm story depends on it."
//
// It does. The intent lives in LightningWallet.fundingIntents and psbt_verify
// only attaches a verified packet to it, so cancelling still finds and clears
// it. Re-arming after a lapsed ten-minute window is therefore free: cancel every
// shim, reopen the streams for fresh addresses, re-issue the same transaction.
func TestCancelShimAfterVerify(t *testing.T) {
	env := regtestenv.Start(t)
	ctx := testCtx(t)

	s := env.OpenShimStream(t, env.Peers(t)[0], fixtureChannelSat)

	// The watch-only cold wallet, exactly as production funds it. Nothing here
	// signs anything — psbt_verify takes the unsigned transaction.
	funded := env.BuildFundingPSBT(t, env.Cold, []*regtestenv.Stream{s}, 5)
	t.Cleanup(func() {
		c, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		if _, err := env.Cold.ReleaseLocks(c, funded.Inputs); err != nil {
			t.Errorf("releasing coin locks: %v", err)
		}
	})

	env.Verify(t, s, funded.Base64)

	if err := abort.CancelShim(ctx, env.Alice.Lightning, s.PendingChanID); err != nil {
		t.Fatalf("shim_cancel after psbt_verify: %v — the re-arm path depends on this", err)
	}
}

// walletcreatefundedpsbt with lockUnspents set is what actually creates the coin
// locks a run has to clean up, so check that the two halves meet: the inputs the
// fixture reports are the ones Core is holding.
func TestFundingPSBTLocksItsInputs(t *testing.T) {
	env := regtestenv.Start(t)
	ctx := testCtx(t)

	s := env.OpenShimStream(t, env.Peers(t)[0], fixtureChannelSat)
	funded := env.BuildFundingPSBT(t, env.Cold, []*regtestenv.Stream{s}, 5)
	t.Cleanup(func() {
		c, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		_, _ = env.Cold.ReleaseLocks(c, funded.Inputs)
		_ = abort.CancelShim(c, env.Alice.Lightning, s.PendingChanID)
	})

	if len(funded.Inputs) == 0 {
		t.Fatal("the funding psbt reported no inputs")
	}
	locked, err := env.Cold.ListLocks(ctx)
	if err != nil {
		t.Fatalf("ListLocks: %v", err)
	}
	held := make(map[bitcoind.Outpoint]bool, len(locked))
	for _, l := range locked {
		held[l] = true
	}
	for _, in := range funded.Inputs {
		if !held[in] {
			t.Errorf("input %s was not locked by Core", in)
		}
	}

	freed, err := env.Cold.ReleaseLocks(ctx, funded.Inputs)
	if err != nil {
		t.Fatalf("ReleaseLocks: %v", err)
	}
	if len(freed) != len(funded.Inputs) {
		t.Fatalf("freed %d of %d inputs", len(freed), len(funded.Inputs))
	}
}

// TestAShimSurvivesItsStreamBeingHungUp answers the question #42 named as unread:
// whether a cancelled funding stream tears down the reservation LND created for
// it.
//
// It does not. rpcServer.OpenChannel's update loop returns on the first
// updateStream.Send failure with a bare `return err` — nothing on that path
// cancels the reservation — and the intent lives in
// LightningWallet.fundingIntents until something asks for it to go. So the
// pending channel id really is the only handle, and a code path that drops one
// after LND has registered the intent leaves a shim nothing can reach.
//
// Which is not the same as saying every failed open leaks one. A refusal that
// arrives as an error *from LND* has already cleaned itself up: a peer's
// lnwire.Error reaches Manager.handleErrorMsg, which calls cancelReservationCtx,
// which calls ChannelReservation.Cancel, which is what deletes the intent
// (lnwallet/wallet.go:1488). The leak is confined to a client-side hang-up over
// a reservation LND is still happy with.
//
// The wait is not decoration. Without it a shim cleaned up asynchronously when
// the stream died would still be in the map when the cancel arrived, and the
// test would pass while proving nothing.
func TestAShimSurvivesItsStreamBeingHungUp(t *testing.T) {
	env := regtestenv.Start(t)
	ctx := testCtx(t)

	s := env.OpenShimStream(t, env.Peers(t)[0], fixtureChannelSat)

	// Hang up the client's end. Stream.Close does not cancel the shim, which is
	// the whole point of the distinction it documents.
	s.Close()
	time.Sleep(2 * time.Second)

	if err := abort.CancelShim(ctx, env.Alice.Lightning, s.PendingChanID); err != nil {
		t.Fatalf("cancelling the shim of a hung-up stream: %v.\n"+
			"If this is ErrNoShim, LND does tear the reservation down with the "+
			"stream, and arm.open discarding the pending channel id on a "+
			"post-psbt_fund failure costs nothing. Say so where that id is "+
			"dropped", err)
	}
	t.Log("the shim outlived its stream: the pending channel id is the only " +
		"handle that could have released it")
}
