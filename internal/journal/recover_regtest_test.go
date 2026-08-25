package journal_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/AusDavo/winthistle/internal/journal"
	"github.com/AusDavo/winthistle/internal/lnd"
	"github.com/AusDavo/winthistle/internal/regtestenv"
	"github.com/lightningnetwork/lnd/lnrpc"
)

const fixtureChannelSat = 250_000

// The whole point of the journal, against a real node: a run that stopped in the
// middle of the window — one channel armed, one stream never verified, coin
// locks behind both — is recoverable from its row alone, by a process that has
// been restarted and holds nothing in memory.
//
// The half-armed shape is made differently after the inversion, and the
// difference is worth reading. It used to be produced by finalizing one stream
// and not the other: psbt_verify committed an outpoint and stopped, so a verified
// stream was one that had not started a channel. psbt_verify now carries
// skip_finalize, which completes LND's funding flow — so verifying a stream *is*
// starting its channel, and the way to leave one stranded is to not verify it at
// all. The journal's split does not move: pending is abandoned, everything short
// of it is cancelled.
//
// The journal is closed and reopened before recovery on purpose. Everything the
// recovery needs has to have been on disk *before* the step it describes, and
// re-reading it from the file is the only honest way to check that.
func TestRecoverAbortsACrashedRunFromItsJournalRow(t *testing.T) {
	env := regtestenv.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	peers := env.Peers(t)
	if len(peers) < 2 {
		t.Skipf("need 2 peers, alice has %d — run: make -C regtest reset", len(peers))
	}

	path := filepath.Join(t.TempDir(), "runs.db")
	j, err := journal.Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	const runID = "regtest-crashed-run"

	// Step 2: both streams open, both handles journalled before anything else
	// happens. These pending channel ids are the only thing that can cancel
	// these streams, so they go to disk first.
	armedStream := env.OpenShimStream(t, peers[0], fixtureChannelSat)
	strandedStream := env.OpenShimStream(t, peers[1], fixtureChannelSat)
	streams := []*regtestenv.Stream{armedStream, strandedStream}

	err = j.Begin(ctx, runID, []journal.NewChannel{
		{PendingChanID: armedStream.PendingChanID, PeerPubkey: armedStream.PeerPubkey,
			AmountSat: armedStream.FundingAmount},
		{PendingChanID: strandedStream.PendingChanID, PeerPubkey: strandedStream.PeerPubkey,
			AmountSat: strandedStream.FundingAmount},
	})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	// Step 4: one transaction for both channels, and the coins Core locked for
	// it recorded as soon as they are known.
	funded := env.BuildFundingPSBT(t, env.Cold, streams, 5)
	t.Cleanup(func() {
		c, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		_, _ = env.Cold.ReleaseLocks(c, funded.Inputs)
	})
	if err := j.RecordLocks(ctx, runID, funded.Inputs); err != nil {
		t.Fatalf("RecordLocks: %v", err)
	}

	// The txid goes on disk before the first psbt_verify, because that call does
	// not pause LND's funding flow — it completes it, and from there a peer may be
	// storing a commitment signature against an outpoint of this transaction.
	txid := funded.TxID
	if err := j.RecordPinnedTxID(ctx, runID, txid); err != nil {
		t.Fatalf("RecordPinnedTxID: %v", err)
	}

	// Step 5, for one stream only, and then the crash. The other stream never
	// sees the transaction at all, which is what leaves it cancellable.
	env.VerifySkippingFinalize(t, armedStream, funded.Base64)
	if err := j.MarkVerified(ctx, runID, armedStream.PendingChanID); err != nil {
		t.Fatalf("MarkVerified: %v", err)
	}
	if got := load(t, j, runID).State; got != journal.StateArming {
		t.Fatalf("run is %s with one channel of two verified, want %s",
			got, journal.StateArming)
	}

	// Step 6 for that stream: its receipt arrives on its own, over a transaction
	// nobody has signed. The gate stays shut — one of two is exactly the state
	// that must not be publishable.
	cp := env.Receipt(t, armedStream)
	if err := j.MarkPending(ctx, runID, armedStream.PendingChanID, cp); err != nil {
		t.Fatalf("MarkPending: %v", err)
	}
	if env.InMempool(t, txid) {
		t.Fatalf("funding tx %s reached the mempool — I-1 is broken", txid)
	}
	if err := j.MarkPublishing(ctx, runID); err == nil {
		t.Fatal("the journal allowed a publish with one channel still unverified")
	}
	if err := j.MarkSigning(ctx, runID); err == nil {
		t.Fatal("the journal let a half-armed batch go out to be signed. Nothing " +
			"is asked of a wallet until every channel is already recoverable")
	}
	if r := load(t, j, runID); r.RawTx != "" {
		t.Fatalf("the journal holds a raw transaction for a run that never signed "+
			"anything: %q", r.RawTx)
	}
	if err := j.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// A restarted process, holding nothing but the file.
	reopened, err := journal.Open(ctx, path)
	if err != nil {
		t.Fatalf("reopening the journal: %v", err)
	}
	defer reopened.Close()

	unfinished, err := reopened.Unfinished(ctx)
	if err != nil {
		t.Fatalf("Unfinished: %v", err)
	}
	if len(unfinished) != 1 || unfinished[0].ID != runID {
		t.Fatalf("want the crashed run listed for recovery, got %+v", unfinished)
	}
	target, err := unfinished[0].AbortTarget()
	if err != nil {
		t.Fatalf("AbortTarget: %v", err)
	}
	if len(target.Channels) != 1 || target.Channels[0] != cp {
		t.Fatalf("want %s to abandon, got %v", cp, target.Channels)
	}
	if len(target.Shims) != 1 || target.Shims[0] != strandedStream.PendingChanID {
		t.Fatalf("want %s to cancel, got %v", strandedStream.PendingChanID, target.Shims)
	}
	if len(target.Locks) != len(funded.Inputs) {
		t.Fatalf("want %d coin locks to free, got %d", len(funded.Inputs), len(target.Locks))
	}

	if !pendingOpen(t, env, cp) {
		t.Fatalf("%s should still be pending before the recovery", cp)
	}

	// The recovery itself. The blunt flag is the standard route here, not an
	// edge case — rpcserver.go infers "shim funded" from ThawHeight > 0 and a
	// plain PSBT open sets none — so a confirmation has to be on offer.
	rep, err := reopened.Recover(ctx, env.Alice.Lightning, env.Cold, runID, alwaysConfirm)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if !rep.Clean() {
		t.Fatalf("recovery left failures: %v", rep.Failures)
	}
	if len(rep.Abandoned) != 1 || len(rep.Cancelled) != 1 {
		t.Fatalf("report is %+v", rep)
	}
	if len(rep.LocksFreed) != len(funded.Inputs) {
		t.Fatalf("freed %d of %d coin locks", len(rep.LocksFreed), len(funded.Inputs))
	}
	if pendingOpen(t, env, cp) {
		t.Fatalf("%s is still pending after the recovery", cp)
	}
	if env.InMempool(t, txid) {
		t.Fatalf("funding tx %s was broadcast at some point — nothing here may publish", txid)
	}

	r := load(t, reopened, runID)
	if r.State != journal.StateAborted {
		t.Fatalf("run is %s after a clean recovery, want %s", r.State, journal.StateAborted)
	}
	if len(mustUnfinished(t, reopened)) != 0 {
		t.Error("a settled run is still listed for recovery")
	}

	// And again, because a recovery can itself be interrupted and restarted.
	rep2, err := reopened.Recover(ctx, env.Alice.Lightning, env.Cold, runID, alwaysConfirm)
	if err != nil {
		t.Fatalf("second Recover should be a no-op, got: %v", err)
	}
	if len(rep2.Abandoned) != 0 || len(rep2.Cancelled) != 0 || len(rep2.LocksFreed) != 0 {
		t.Fatalf("second Recover redid work that was already done: %+v", rep2)
	}
}

func pendingOpen(t *testing.T, env *regtestenv.Env, cp lnd.ChannelPoint) bool {
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
