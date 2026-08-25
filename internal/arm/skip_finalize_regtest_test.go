package arm

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/AusDavo/winthistle/internal/abort"
	"github.com/AusDavo/winthistle/internal/lnd"
	"github.com/AusDavo/winthistle/internal/plan"
	"github.com/AusDavo/winthistle/internal/regtestenv"
	"github.com/AusDavo/winthistle/internal/regtestenv/coldwallet"
	"github.com/AusDavo/winthistle/internal/reserve"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/lightningnetwork/lnd/lnrpc"
)

// TestSkipFinalizeReachesChanPendingWithNothingSigned settles the claim the
// replan rests on.
//
// CLAUDE.md's rejected list said skip_finalize "skips the step that produces the
// chan_pending gate I-1 depends on", and marked it non-negotiable. The source
// says otherwise. In lnwallet/chanfunding/psbt_assembler.go at v0.19.3-beta,
// PsbtIntent.Verify ends:
//
//	if !i.shouldPublish && skipFinalize {
//	        i.FinalTX = packet.UnsignedTx
//	        i.State = PsbtFinalized
//	        i.signalPsbtReady.Do(func() { close(i.PsbtReady) })
//
// and funding/manager.go reads that channel with "nil error means the flow
// continues normally now". So with no_publish set — which I-1 requires on every
// channel anyway — skip_finalize does not skip the gate. It skips handing LND a
// *signed* transaction, which lnwallet/wallet.go only stores at all when
// ShouldPublishFundingTX() is true. CompileFundingTx still runs and still "sets
// the actual funding outpoint in stone", because every input is segwit and a
// witness cannot move the txid.
//
// What this test proves is the consequence: n channels reach chan_pending over
// an UNSIGNED transaction, so every channel in the batch is recoverable by
// force-close before any device has been asked for a signature. That inverts the
// order the whole design was built around — the peers' ten-minute window covers
// building an unsigned transaction rather than a cold-storage signing round, and
// a wallet holding a broadcastable copy afterwards has nothing left to front-run.
//
// Nothing here signs anything. That is the assertion. There is no combine, no
// psbt_finalize and no publish, and the mempool is checked at the end because the
// transaction has no witnesses for anyone to have put it there with.
func TestSkipFinalizeReachesChanPendingWithNothingSigned(t *testing.T) {
	env := regtestenv.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)

	// LND refuses to open a channel while its wallet is behind, and a cluster
	// left idle for a couple of hours is behind. One block and the wait for it.
	env.Mine(t, 1)

	const n = 2
	peers := env.Peers(t)
	if len(peers) < n {
		t.Skipf("this test needs %d peers, alice has %d", n, len(peers))
	}
	chans := make([]Channel, 0, n)
	for _, p := range peers[:n] {
		chans = append(chans, Channel{Peer: p, AmountSat: 250_000})
	}

	// Phase 0's last act, and the reason it is here rather than skipped: LND runs
	// enforceNewReservedValue inside psbt_verify, over its own hot wallet. A batch
	// that does not top the node up is refused at step 5 for a reason that has
	// nothing to do with skip_finalize, which would read as a disproof.
	finding, err := reserve.Check(ctx, env.Alice.WalletKit, reserve.Batch{Public: n})
	if err != nil {
		t.Fatalf("the anchor-reserve pre-flight: %v", err)
	}
	t.Logf("reserve pre-flight: %s", finding.Summary())
	topUp, err := plan.ReserveTopUp(ctx, env.Alice.Lightning, finding)
	if err != nil {
		t.Fatalf("building the reserve top-up: %v", err)
	}

	// What the cleanup will have to take apart, filled in as the test creates it.
	// A run that fails before its receipts arrive leaves shims; one that gets them
	// leaves pending channels; either way it leaves Core's coin locks.
	leftBehind := abort.Target{}
	abandonAtCleanup(t, env, &leftBehind)

	streams, err := Open(ctx, env.Alice.Lightning, "regtest", chans)
	if err != nil {
		t.Fatalf("opening the batch's funding streams: %v", err)
	}
	t.Cleanup(streams.Close)
	leftBehind.Shims = streams.PendingChanIDs()

	// An ordinary unsigned batch transaction. coldwallet.Build is the builder
	// this repository already has, and after the replan it is the harness playing
	// Sparrow's part; the point of the test is that its output is never signed.
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

	outputs := fundingOutputsIn(streams)
	if topUp != nil {
		outputs = append(outputs, coldwallet.Output{
			Address: topUp.Address, AmountSat: topUp.AmountSat,
		})
	}
	built, err := coldwallet.Build(ctx, env.Cold, coldwallet.BuildRequest{
		Outputs:          outputs,
		ChangeAddress:    change,
		FeeRateSatPerVB:  10.0,
		MinConfirmations: 1,
	})
	if err != nil {
		t.Fatalf("building the batch transaction: %v", err)
	}
	env.ReleaseLocksAtCleanup(t, env.Cold, built.Inputs)
	t.Logf("unsigned batch transaction %s, %d channels, %d input(s) locked",
		built.TxID, n, len(built.Inputs))

	// The outpoints LND should commit to, resolved from the funding addresses in
	// the transaction we are about to show it. Checking the receipt against these
	// is what makes chan_pending evidence rather than a message.
	packet, err := psbt.NewFromRawBytes(bytes.NewReader(built.Raw), false)
	if err != nil {
		t.Fatalf("parsing the packet Core built: %v", err)
	}
	want, err := streams.outpoints(packet.UnsignedTx, built.TxID)
	if err != nil {
		t.Fatalf("resolving the batch's funding outputs: %v", err)
	}

	// psbt_verify with skip_finalize, and nothing after it. No psbt_finalize, no
	// signing round, no combine.
	for _, st := range streams.All {
		if _, err := env.Alice.Lightning.FundingStateStep(ctx, &lnrpc.FundingTransitionMsg{
			Trigger: &lnrpc.FundingTransitionMsg_PsbtVerify{
				PsbtVerify: &lnrpc.FundingPsbtVerify{
					PendingChanId: st.PendingChanID.Bytes(),
					FundedPsbt:    built.Raw,
					SkipFinalize:  true,
				},
			},
		}); err != nil {
			t.Fatalf("psbt_verify(skip_finalize) for the channel to %s (%s): %v\n"+
				"If LND refuses skip_finalize outright this is the disproof, and "+
				"docs/replan-2026-08.md is void", short(st.Peer), st.PendingChanID, err)
		}
		t.Logf("psbt_verify(skip_finalize) accepted for %s", short(st.Peer))
	}

	// The receipt, read off the same stream arm.Finalize reads it from.
	var got []lnd.ChannelPoint
	for _, st := range streams.All {
		upd, err := st.recv.Recv()
		if err != nil {
			t.Fatalf("waiting for chan_pending for the channel to %s: %v",
				short(st.Peer), err)
		}
		pending := upd.GetChanPending()
		if pending == nil {
			t.Fatalf("expected chan_pending for the channel to %s, got %T",
				short(st.Peer), upd.GetUpdate())
		}
		cp, err := lnd.ChannelPointFromPending(pending.GetTxid(), pending.GetOutputIndex())
		if err != nil {
			t.Fatalf("reading the funding outpoint out of chan_pending for %s: %v",
				st.PendingChanID, err)
		}
		if cp != want[st.PendingChanID] {
			t.Fatalf("chan_pending for %s says %s, and this transaction pays %s at %s",
				st.PendingChanID, cp, st.FundingAddress, want[st.PendingChanID])
		}
		got = append(got, cp)

		// The channel is in LND's channel database now, so it is abandoned rather
		// than cancelled. Recorded before the next assertion, because from here a
		// failure leaves a pending channel behind either way.
		leftBehind.Channels = append(leftBehind.Channels, cp)
		leftBehind.Shims = remove(leftBehind.Shims, st.PendingChanID)

		t.Logf("chan_pending for %s at %s, with nothing signed", short(st.Peer), cp)
	}
	if len(got) != n {
		t.Fatalf("%d receipts for %d channels", len(got), n)
	}

	// The gate is open and LND agrees: both channels are pending opens, which is
	// the same fact chan_pending reports arriving by a route that does not depend
	// on the stream still being alive.
	assertPendingOpens(ctx, t, env, got)

	// I-1's other half. There is nothing to broadcast: this transaction has no
	// witnesses, and no path in this test could have published it if it had.
	if env.InMempool(t, built.TxID) {
		t.Fatalf("the unsigned batch transaction %s reached the mempool", built.TxID)
	}
	t.Logf("%d of %d channels recoverable by force-close, nothing signed, "+
		"mempool clear", n, n)
}

// assertPendingOpens asks LND directly whether it holds these as pending opens.
func assertPendingOpens(ctx context.Context, t *testing.T, env *regtestenv.Env,
	want []lnd.ChannelPoint) {

	t.Helper()
	resp, err := env.Alice.Lightning.PendingChannels(ctx, &lnrpc.PendingChannelsRequest{})
	if err != nil {
		t.Fatalf("asking LND for its pending channels: %v", err)
	}
	open := map[string]bool{}
	for _, p := range resp.GetPendingOpenChannels() {
		open[p.GetChannel().GetChannelPoint()] = true
	}
	for _, cp := range want {
		if !open[cp.String()] {
			t.Errorf("LND does not list %s as a pending open, so the receipt is "+
				"not backed by a channel in its database", cp)
		}
	}
}

// abandonAtCleanup takes apart whatever the test left in LND and in Core.
//
// Deliberately not through Journal.Recover, and it stays that way now that the
// journal does describe this shape. This test exists to be independent of
// internal/arm and internal/journal: it drives FundingStateStep directly so that
// it keeps proving the claim the replan rests on even if both of those packages
// are wrong. A teardown routed through the journal would put one of them back in
// the path. arm_regtest_test.go is where the app's own version is exercised.
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

// remove drops one pending channel id from a slice, so a stream that reached
// chan_pending stops being listed as a cancellable shim.
func remove(ids []lnd.PendingChanID, id lnd.PendingChanID) []lnd.PendingChanID {
	out := ids[:0]
	for _, have := range ids {
		if have != id {
			out = append(out, have)
		}
	}
	return out
}

// fundingOutputs is the harness reading the recipients off the open streams, the
// way an operator reads them off the terminal at step 4.
func fundingOutputsIn(streams *Streams) []coldwallet.Output {
	addrs := make([]string, 0, len(streams.All))
	amounts := make([]int64, 0, len(streams.All))
	for _, st := range streams.All {
		addrs = append(addrs, st.FundingAddress)
		amounts = append(amounts, st.FundingAmount)
	}
	return coldwallet.FundingOutputsOf(addrs, amounts)
}
