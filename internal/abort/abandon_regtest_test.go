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
	"github.com/lightningnetwork/lnd/lnrpc"
)

// armedBatch is what one arming leaves behind: a receipt per channel, the
// streams that produced them, and the one transaction that is still ours alone.
type armedBatch struct {
	Channels []lnd.ChannelPoint
	Streams  []*regtestenv.Stream
	Locks    []bitcoind.Outpoint
	RawTx    string
	TxID     string
}

// target is the batch as an abort would see it after a total failure: every
// channel armed, so every channel has to be abandoned rather than cancelled.
func (b armedBatch) target() abort.Target {
	return abort.Target{Channels: b.Channels, Locks: b.Locks}
}

// armBatch drives steps 2-7 for a whole batch: one funding stream per peer, ONE
// unsigned transaction carrying every funding output plus change, psbt_verify
// against every stream, then psbt_finalize on all of them.
//
// It asserts I-1 as it goes, which is the property the rest of the design rests
// on: every channel reaches chan_pending, and the transaction that funds them
// all is still in nobody's mempool afterwards.
//
// LND tolerates the extra outputs by construction. PsbtIntent.Verify looks for
// its own funding output with psbt.TxOutsEqual and only requires that the input
// sum exceed the *total* output sum — it never asserts that its output is the
// only one. So n streams can each verify the same n-output transaction, and each
// one commits to the same unsigned TXID (I-3).
func armBatch(t *testing.T, env *regtestenv.Env, peers []string) armedBatch {
	t.Helper()

	streams := make([]*regtestenv.Stream, 0, len(peers))
	for _, p := range peers {
		streams = append(streams, env.OpenShimStream(t, p, fixtureChannelSat))
	}

	// One transaction for the whole batch, funded by the watch-only cold wallet
	// exactly as production does, and signed by that wallet's two halves on the
	// way back — see SignAndCombine. No party but this process ever holds a
	// complete transaction, which is I-2, and the fixture is the real path.
	funded := env.BuildFundingPSBT(t, env.Cold, streams, 5)
	t.Cleanup(func() {
		c, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		_, _ = env.Cold.ReleaseLocks(c, funded.Inputs)
	})

	// I-4 wants a change output we control, so a CPFP child stays viable when
	// the transaction cannot be replaced. Core reports -1 when it added none.
	if funded.ChangeI < 0 {
		t.Fatalf("the batch transaction has no change output — I-4 leaves no CPFP handle")
	}

	// Every stream verifies before any of them finalizes. That is the order the
	// design specifies, and it is not merely tidy: PsbtFundingVerify re-runs
	// LND's reserved-value check, and the reserve grows with the number of
	// anchor channels the wallet already has pending, so verifying all n first
	// keeps that requirement identical for every member of the batch.
	for _, s := range streams {
		env.Verify(t, s, funded.Base64)
	}

	rawTx, txid := env.SignAndCombine(t, funded)
	assertNotReplaceable(t, env, rawTx)

	b := armedBatch{
		Streams: streams,
		Locks:   funded.Inputs,
		RawTx:   rawTx,
		TxID:    txid,
	}
	for _, s := range streams {
		cp := env.Finalize(t, s, rawTx)

		// I-1, observed rather than assumed, and re-checked after every single
		// finalize. chan_pending is emitted only after CompleteReservation has
		// stored the peer's commitment signature, so the channel is
		// force-closeable — and no_publish cleared the bit that gates the
		// broadcast, so the transaction is still ours alone. The last member of
		// the batch is the one that matters: it is exactly where LND's own docs
		// would have had us publish.
		if env.InMempool(t, txid) {
			t.Fatalf("funding tx %s reached the mempool after finalizing %s "+
				"despite no_publish — I-1 is broken", txid, s.PendingChanID)
		}
		if cp.TxID != txid {
			t.Fatalf("chan_pending outpoint %s does not match the funding tx %s", cp, txid)
		}
		b.Channels = append(b.Channels, cp)
	}

	// n receipts, n distinct outputs of the one transaction.
	if len(b.Channels) != len(streams) {
		t.Fatalf("armed %d channels, expected %d", len(b.Channels), len(streams))
	}
	seen := make(map[uint32]bool, len(b.Channels))
	for _, cp := range b.Channels {
		if seen[cp.Index] {
			t.Fatalf("two channels claim output %d of %s", cp.Index, cp.TxID)
		}
		seen[cp.Index] = true
	}
	return b
}

// assertNotReplaceable checks I-4 on the transaction that actually exists.
//
// Replacing the funding transaction would move every outpoint in the batch and
// destroy every channel on it, so replaceability is disabled at construction —
// BuildFundingPSBT passes replaceable:false. This confirms Core honoured it: BIP
// 125 opts in via any input with nSequence below 0xfffffffe.
func assertNotReplaceable(t *testing.T, env *regtestenv.Env, rawTxHex string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var decoded struct {
		Vin []struct {
			Sequence uint32 `json:"sequence"`
		} `json:"vin"`
	}
	if err := env.Node.Call(ctx, "decoderawtransaction", []any{rawTxHex}, &decoded); err != nil {
		t.Fatalf("decoderawtransaction: %v", err)
	}
	if len(decoded.Vin) == 0 {
		t.Fatal("the funding transaction has no inputs")
	}
	for i, in := range decoded.Vin {
		if in.Sequence < 0xfffffffe {
			t.Fatalf("input %d signals replaceability (nSequence %#x) — I-4 forbids RBF",
				i, in.Sequence)
		}
	}
}

// armOneChannel is the single-channel case of armBatch, kept because most of the
// abort tests only need one armed channel to take apart.
func armOneChannel(t *testing.T, env *regtestenv.Env) (lnd.ChannelPoint, *regtestenv.Stream) {
	t.Helper()

	b := armBatch(t, env, env.Peers(t)[:1])
	return b.Channels[0], b.Streams[0]
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

// The assertion the whole design rests on, at n > 1.
//
// Three peers, three funding streams, one unsigned transaction carrying all
// three funding outputs plus change. Every stream verifies that transaction and
// every stream finalizes it, and the transaction still reaches no mempool. That
// is I-1: publish only once every channel is already recoverable, and the app is
// the only party that can publish at all.
//
// It is worth being precise about what fails if this is wrong. LND's own docs
// say to set no_publish on all but the last channel, which would have the last
// psbt_finalize broadcast for us — at a moment when the earlier channels are
// pending but this one has not yet stored its peer's commitment signature. The
// mempool check after *every* finalize, including the last, is what separates
// this design from that one.
func TestBatchArmsEveryChannelBeforeAnythingIsPublished(t *testing.T) {
	env := regtestenv.Start(t)
	ctx := testCtx(t)

	peers := env.Peers(t)
	if len(peers) < 3 {
		t.Skipf("need 3 peers for a batch, alice has %d — run: make -C regtest reset", len(peers))
	}
	peers = peers[:3]

	b := armBatch(t, env, peers)
	t.Logf("armed %d channels on %s", len(b.Channels), b.TxID)

	// Every channel is recoverable: LND is watching each outpoint, and each has
	// its peer's commitment signature stored. Nothing has been broadcast.
	for _, cp := range b.Channels {
		if !isPendingOpen(t, env, cp) {
			t.Errorf("%s reached chan_pending but is not among pending opens", cp)
		}
	}
	if env.InMempool(t, b.TxID) {
		t.Fatalf("funding tx %s is in the mempool with the whole batch armed — I-1 is broken", b.TxID)
	}

	// And the gate closes again: a batch this far along is still entirely
	// recoverable, because we hold the only copy of the transaction. Tearing it
	// down is the same abort as for one channel, n times over — and it is what
	// the mainnet cold probe will terminate through.
	alwaysConfirm := func(context.Context, abort.BluntRequest) (bool, error) { return true, nil }
	rep, err := abort.Run(ctx, env.Alice.Lightning, env.Cold, b.target(), alwaysConfirm)
	if err != nil {
		t.Fatalf("aborting the armed batch: %v", err)
	}
	if !rep.Clean() {
		t.Fatalf("abort left failures: %v", rep.Failures)
	}
	if len(rep.Abandoned) != len(b.Channels) {
		t.Fatalf("abandoned %d of %d channels", len(rep.Abandoned), len(b.Channels))
	}
	for _, cp := range b.Channels {
		if isPendingOpen(t, env, cp) {
			t.Errorf("%s is still pending after the abort", cp)
		}
	}
	if env.InMempool(t, b.TxID) {
		t.Fatalf("funding tx %s was broadcast at some point — nothing here may publish", b.TxID)
	}
}
