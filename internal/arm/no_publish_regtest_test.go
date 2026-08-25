package arm

import (
	"context"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/AusDavo/winthistle/internal/abort"
	"github.com/AusDavo/winthistle/internal/coldwallet"
	"github.com/AusDavo/winthistle/internal/lnd"
	"github.com/AusDavo/winthistle/internal/regtestenv"
	"github.com/lightningnetwork/lnd/lnrpc"
)

// TestLNDItselfRefusesSkipFinalizeWithoutNoPublish, on a running node.
//
// This is the check the whole inversion leans on from the outside. Verify asserts
// no_publish on its own side before it sends anything — see
// TestVerifyRefusesAStreamThatDidNotSetNoPublish — but that assertion is only
// worth having if the combination it prevents is genuinely dangerous, and the
// reason to believe it is dangerous is that LND treats it as an error rather than
// as a configuration. So this test asks LND.
//
// The source says PsbtFundingVerify checks `skipFinalize &&
// psbtIntent.ShouldPublishFundingTX()` and returns "cannot set skip_finalize for
// channel that did not set no_publish" (lnwallet/wallet.go:764), before it
// advances the intent. Two things follow, and both are asserted here: the refusal
// happens, and it costs nothing — the intent is untouched, so the shim is still
// cancellable, which is why the cleanup below is a plain shim cancel.
//
// Deliberately not routed through Open. Open sets no_publish unconditionally and
// has no parameter that changes it, which is the property this file exists to
// protect; getting a stream without it means building the OpenChannel request
// here, in a test, where the hazard cannot escape into the package.
func TestLNDItselfRefusesSkipFinalizeWithoutNoPublish(t *testing.T) {
	env := regtestenv.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)

	// LND refuses to open a channel while its wallet is behind, and a cluster
	// left idle for a couple of hours is behind.
	env.Mine(t, 1)

	peers := env.Peers(t)
	if len(peers) < 1 {
		t.Skip("this test needs a peer, alice has none")
	}

	id, err := lnd.NewPendingChanID()
	if err != nil {
		t.Fatalf("generating a pending channel id: %v", err)
	}
	pubkey, err := hex.DecodeString(peers[0])
	if err != nil {
		t.Fatalf("peer %q is not hex: %v", peers[0], err)
	}

	streamCtx, hangUp := context.WithCancel(ctx)
	t.Cleanup(hangUp)

	recv, err := env.Alice.Lightning.OpenChannel(streamCtx, &lnrpc.OpenChannelRequest{
		NodePubkey:         pubkey,
		LocalFundingAmount: 250_000,
		FundingShim: &lnrpc.FundingShim{
			Shim: &lnrpc.FundingShim_PsbtShim{
				PsbtShim: &lnrpc.PsbtShim{
					PendingChanId: id.Bytes(),
					// The hazard, in a test and nowhere else. Open does not
					// offer this.
					NoPublish: false,
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("opening a funding stream: %v", err)
	}
	cancelShimAtCleanup(t, env, id)

	upd, err := recv.Recv()
	if err != nil {
		t.Fatalf("waiting for psbt_fund: %v", err)
	}
	fund := upd.GetPsbtFund()
	if fund == nil {
		t.Fatalf("expected psbt_fund, got %T", upd.GetUpdate())
	}

	// A transaction that pays exactly what LND asked for, so nothing about the
	// PSBT can be the reason for the refusal.
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
	built, err := coldwallet.Build(ctx, env.Cold, coldwallet.BuildRequest{
		Outputs: []coldwallet.Output{{
			Address: fund.GetFundingAddress(), AmountSat: fund.GetFundingAmount(),
		}},
		ChangeAddress:    change,
		FeeRateSatPerVB:  10.0,
		MinConfirmations: 1,
	})
	if err != nil {
		t.Fatalf("building the transaction: %v", err)
	}
	env.ReleaseLocksAtCleanup(t, env.Cold, built.Inputs)

	_, err = env.Alice.Lightning.FundingStateStep(ctx, &lnrpc.FundingTransitionMsg{
		Trigger: &lnrpc.FundingTransitionMsg_PsbtVerify{
			PsbtVerify: &lnrpc.FundingPsbtVerify{
				PendingChanId: id.Bytes(),
				FundedPsbt:    built.Raw,
				SkipFinalize:  true,
			},
		},
	})
	if err == nil {
		t.Fatal("LND accepted skip_finalize on a stream that did not set no_publish. " +
			"That is a request to arm a channel and broadcast the funding " +
			"transaction, which is I-1 breached from inside — and arm.Verify's own " +
			"assertion is now the only thing standing between this program and it")
	}
	if !strings.Contains(err.Error(), "skip_finalize") ||
		!strings.Contains(err.Error(), "no_publish") {
		t.Errorf("LND refused for some other reason, so this proves nothing about "+
			"the pair: %v", err)
	}
	t.Logf("LND refused: %v", err)

	// The refusal is checked before the intent is advanced, so the same stream is
	// still usable and the shim is still cancellable. That is what makes a batch
	// refused at step 5 re-armable rather than lost, and it is asserted here
	// rather than assumed because the whole cost model of step 5 rests on it.
	if _, err := env.Alice.Lightning.FundingStateStep(ctx, &lnrpc.FundingTransitionMsg{
		Trigger: &lnrpc.FundingTransitionMsg_PsbtVerify{
			PsbtVerify: &lnrpc.FundingPsbtVerify{
				PendingChanId: id.Bytes(),
				FundedPsbt:    built.Raw,
			},
		},
	}); err != nil {
		t.Errorf("the refused stream is unusable afterwards, so the check is not "+
			"free after all: %v", err)
	}
}

// cancelShimAtCleanup releases the reservation this test's stream is holding.
//
// A shim cancel and nothing else: the point of the test is that psbt_verify was
// refused before LND advanced the intent, so there is no channel to abandon.
func cancelShimAtCleanup(t *testing.T, env *regtestenv.Env, id lnd.PendingChanID) {
	t.Helper()
	t.Cleanup(func() {
		ctx, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		if err := abort.CancelShim(ctx, env.Alice.Lightning, id); err != nil {
			t.Errorf("cancelling the shim this test left open: %v", err)
		}
	})
}
