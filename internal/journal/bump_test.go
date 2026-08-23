package journal_test

import (
	"context"
	"errors"
	"testing"

	"github.com/AusDavo/winthistle/internal/bitcoind"
	"github.com/AusDavo/winthistle/internal/journal"
	"github.com/AusDavo/winthistle/internal/lnd"
)

// The parent of every bump in here: a regtest txid, so nothing can be mistaken
// for mainnet state.
const parentTxID = "3f2a9c5e1b7d4086a1c3e5f70981b2d4c6e8f0a2b4c6d8e0f2a4b6c8d0e2f406"

// published drives a run all the way to StatePublished, which is the only state
// a bump may be started against.
func published(t *testing.T, j *journal.Journal, runID string) {
	t.Helper()
	ctx := context.Background()
	chanIDs := ids(t, 2)

	if err := j.Begin(ctx, runID, batch(t, chanIDs)); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	for _, id := range chanIDs {
		if err := j.MarkVerified(ctx, runID, id); err != nil {
			t.Fatalf("MarkVerified: %v", err)
		}
	}
	if err := j.RecordFinalizedTx(ctx, runID, parentTxID, "00"); err != nil {
		t.Fatalf("RecordFinalizedTx: %v", err)
	}
	for i, id := range chanIDs {
		cp := lnd.ChannelPoint{TxID: parentTxID, Index: uint32(i)}
		if err := j.MarkPending(ctx, runID, id, cp); err != nil {
			t.Fatalf("MarkPending: %v", err)
		}
	}
	if err := j.MarkPublishing(ctx, runID); err != nil {
		t.Fatalf("MarkPublishing: %v", err)
	}
	if err := j.MarkPublished(ctx, runID); err != nil {
		t.Fatalf("MarkPublished: %v", err)
	}
}

func samplePlan() journal.BumpPlan {
	return journal.BumpPlan{
		ParentTxID:     parentTxID,
		ParentVsizeVB:  7_007,
		ParentFeeSat:   7_007,
		Change:         bitcoind.Outpoint{TxID: parentTxID, Vout: 3},
		ChangeSat:      400_000,
		TargetSatPerVB: 20,
	}
}

// TestABumpWalksThroughToPublished is the healthy sequence, with the states the
// journal derives for itself checked at each step.
func TestABumpWalksThroughToPublished(t *testing.T) {
	ctx := context.Background()
	j := open(t)
	const runID = "run-with-a-child"
	published(t, j, runID)

	seq, err := j.BeginBump(ctx, runID, samplePlan())
	if err != nil {
		t.Fatalf("BeginBump: %v", err)
	}
	if seq != 1 {
		t.Errorf("the first bump of a run is %d, want 1", seq)
	}

	b, err := j.LoadBump(ctx, runID, seq)
	if err != nil {
		t.Fatalf("LoadBump: %v", err)
	}
	if b.State != journal.BumpBuilding {
		t.Errorf("a new bump is %s, want %s", b.State, journal.BumpBuilding)
	}
	// The change outpoint is claimed before Core is asked to lock it, which is
	// the whole reason the row exists this early: a crash inside
	// walletcreatefundedpsbt has to leave a lock this journal admits to.
	if len(b.Locks) != 1 || b.Locks[0].Outpoint != samplePlan().Change {
		t.Fatalf("a new bump claims %v; it should claim the change outpoint",
			b.Locks)
	}
	if b.Locks[0].Released {
		t.Error("the change outpoint is journalled as already released")
	}

	// Publishing is refused until the child is signed, and refused with the
	// sentinel a caller can match on.
	if err := j.MarkBumpPublishing(ctx, runID, seq); !errors.Is(err, journal.ErrBumpNotSigned) {
		t.Fatalf("publishing an unsigned child: %v", err)
	}

	const childTxID = "c1c1c1c1b7d4086a1c3e5f70981b2d4c6e8f0a2b4c6d8e0f2a4b6c8d0e2f4060"
	if err := j.RecordBumpChild(ctx, runID, seq, childTxID, 136_133,
		[]bitcoind.Outpoint{samplePlan().Change}); err != nil {
		t.Fatalf("RecordBumpChild: %v", err)
	}
	if b, _ = j.LoadBump(ctx, runID, seq); b.State != journal.BumpSigning {
		t.Errorf("after the build the bump is %s, want %s", b.State, journal.BumpSigning)
	}

	// Still refused: built is not signed.
	if err := j.MarkBumpPublishing(ctx, runID, seq); !errors.Is(err, journal.ErrBumpNotSigned) {
		t.Fatalf("publishing a built-but-unsigned child: %v", err)
	}

	for _, label := range []string{"cold1", "cold2"} {
		if err := j.RecordBumpSigner(ctx, runID, seq, label,
			journal.SignerPartial); err != nil {
			t.Fatalf("RecordBumpSigner(%s): %v", label, err)
		}
	}
	if err := j.RecordBumpRawTx(ctx, runID, seq, childTxID, "deadbeef"); err != nil {
		t.Fatalf("RecordBumpRawTx: %v", err)
	}
	if err := j.MarkBumpPublishing(ctx, runID, seq); err != nil {
		t.Fatalf("MarkBumpPublishing: %v", err)
	}
	if err := j.MarkBumpPublished(ctx, runID, seq); err != nil {
		t.Fatalf("MarkBumpPublished: %v", err)
	}

	b, err = j.LoadBump(ctx, runID, seq)
	if err != nil {
		t.Fatalf("LoadBump: %v", err)
	}
	if b.State != journal.BumpPublished || !b.Finished() || !b.MayBePublic() {
		t.Errorf("a published bump: state %s, finished %v, may be public %v",
			b.State, b.Finished(), b.MayBePublic())
	}
	if len(b.Signers) != 2 {
		t.Errorf("the bump records %d signers, want 2", len(b.Signers))
	}
	if b.RawTx == "" || b.ChildTxID != childTxID {
		t.Errorf("the finalized child did not round-trip: %q / %q", b.ChildTxID, b.RawTx)
	}

	// A published bump is finished, so it drops off the unfinished list — which
	// is what stops doctor re-reporting it forever.
	unfinished, err := j.UnfinishedBumps(ctx)
	if err != nil {
		t.Fatalf("UnfinishedBumps: %v", err)
	}
	if len(unfinished) != 0 {
		t.Errorf("a published bump is still listed as unfinished: %+v", unfinished)
	}
}

// TestABumpIsRefusedForARunThatWasNeverPublished is the gate.
//
// A CPFP child accelerates a transaction that is already in a mempool. A child
// of a transaction nobody has would spend an output that does not exist, so it
// could never confirm — and the journal is the component that knows which runs
// reached the publish call, the same way it is the one that knows whether every
// chan_pending arrived.
func TestABumpIsRefusedForARunThatWasNeverPublished(t *testing.T) {
	ctx := context.Background()
	j := open(t)
	const runID = "run-that-never-went-out"

	if err := j.Begin(ctx, runID, batch(t, ids(t, 2))); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	_, err := j.BeginBump(ctx, runID, samplePlan())
	if !errors.Is(err, journal.ErrNotPublic) {
		t.Fatalf("wrong sentinel for an unpublished run: %v", err)
	}
	t.Logf("refused, correctly: %v", err)
}

// A child of the wrong transaction accelerates nothing, and the journal is the
// only place the parent's identity is recorded.
func TestABumpIsRefusedForTheWrongParent(t *testing.T) {
	ctx := context.Background()
	j := open(t)
	const runID = "run-with-a-known-txid"
	published(t, j, runID)

	p := samplePlan()
	p.ParentTxID = "0000000000000000000000000000000000000000000000000000000000000001"
	if _, err := j.BeginBump(ctx, runID, p); err == nil {
		t.Fatal("a bump against the wrong parent was accepted")
	} else {
		t.Logf("refused, correctly: %v", err)
	}
}

// TestGivingUpOnABumpReleasesItsLockAndNothingElse is a bump's whole teardown.
//
// One list where a run has three: no shim to cancel and no channel to abandon,
// because a child talks to no peer and creates nothing. What is left is the coin
// lock, and the point of releasing it is that Core leaves locked outputs out of
// listunspent — so a lock nobody will ever use looks exactly like a spent coin.
func TestGivingUpOnABumpReleasesItsLockAndNothingElse(t *testing.T) {
	ctx := context.Background()
	j := open(t)
	const runID = "run-with-an-abandoned-child"
	published(t, j, runID)

	seq, err := j.BeginBump(ctx, runID, samplePlan())
	if err != nil {
		t.Fatalf("BeginBump: %v", err)
	}

	b, _ := j.LoadBump(ctx, runID, seq)
	target := b.AbortTarget()
	if len(target.Channels) != 0 || len(target.Shims) != 0 {
		t.Errorf("a bump's abort target names channels or shims: %+v", target)
	}
	if len(target.Locks) != 1 {
		t.Fatalf("a bump's abort target names %d locks, want 1", len(target.Locks))
	}

	core := &fakeCore{}
	rep, err := j.AbandonBump(ctx, core, runID, seq)
	if err != nil {
		t.Fatalf("AbandonBump: %v", err)
	}
	if len(rep.LocksFreed) != 1 {
		t.Errorf("freed %d locks, want 1", len(rep.LocksFreed))
	}
	if core.calls != 1 {
		t.Errorf("Core was asked %d times, want 1", core.calls)
	}

	b, err = j.LoadBump(ctx, runID, seq)
	if err != nil {
		t.Fatalf("LoadBump: %v", err)
	}
	if b.State != journal.BumpAbandoned {
		t.Errorf("after giving up the bump is %s, want %s", b.State, journal.BumpAbandoned)
	}
	if len(b.AbortTarget().Locks) != 0 {
		t.Error("the released lock is still being offered, so every later listing " +
			"would re-offer it forever")
	}

	// Safe to call twice, like everything else on this path.
	if _, err := j.AbandonBump(ctx, core, runID, seq); err != nil {
		t.Fatalf("a second AbandonBump: %v", err)
	}
}

// TestGivingUpOnAPublishedChildDoesNotClaimItWasCalledOff is the honesty check.
//
// Releasing a coin lock takes nothing back. If the child's bytes are out, they
// are out, and a journal that recorded "abandoned" would be making a claim about
// the network that this call cannot make.
func TestGivingUpOnAPublishedChildDoesNotClaimItWasCalledOff(t *testing.T) {
	ctx := context.Background()
	j := open(t)
	const runID = "run-whose-child-went-out"
	published(t, j, runID)

	seq, err := j.BeginBump(ctx, runID, samplePlan())
	if err != nil {
		t.Fatalf("BeginBump: %v", err)
	}
	const childTxID = "c2c2c2c2b7d4086a1c3e5f70981b2d4c6e8f0a2b4c6d8e0f2a4b6c8d0e2f4060"
	if err := j.RecordBumpChild(ctx, runID, seq, childTxID, 1000, nil); err != nil {
		t.Fatalf("RecordBumpChild: %v", err)
	}
	if err := j.RecordBumpRawTx(ctx, runID, seq, childTxID, "deadbeef"); err != nil {
		t.Fatalf("RecordBumpRawTx: %v", err)
	}
	if err := j.MarkBumpPublishing(ctx, runID, seq); err != nil {
		t.Fatalf("MarkBumpPublishing: %v", err)
	}

	if _, err := j.AbandonBump(ctx, &fakeCore{}, runID, seq); err != nil {
		t.Fatalf("AbandonBump: %v", err)
	}
	b, err := j.LoadBump(ctx, runID, seq)
	if err != nil {
		t.Fatalf("LoadBump: %v", err)
	}
	if b.State != journal.BumpPublishing {
		t.Errorf("giving up on a published child moved it to %s. It must keep the "+
			"state that says its bytes may be in a mempool", b.State)
	}
	if !b.MayBePublic() {
		t.Error("a child that reached the publish call no longer reports that it " +
			"may be public")
	}
}

// A finalized child whose txid differs from the one that was built is the
// child's own version of I-3, and it is refused rather than overwritten.
func TestAChildWhoseTxidMovedIsRefused(t *testing.T) {
	ctx := context.Background()
	j := open(t)
	const runID = "run-with-a-moved-child"
	published(t, j, runID)

	seq, err := j.BeginBump(ctx, runID, samplePlan())
	if err != nil {
		t.Fatalf("BeginBump: %v", err)
	}
	const built = "c3c3c3c3b7d4086a1c3e5f70981b2d4c6e8f0a2b4c6d8e0f2a4b6c8d0e2f4060"
	const finalized = "c4c4c4c4b7d4086a1c3e5f70981b2d4c6e8f0a2b4c6d8e0f2a4b6c8d0e2f4060"
	if err := j.RecordBumpChild(ctx, runID, seq, built, 1000, nil); err != nil {
		t.Fatalf("RecordBumpChild: %v", err)
	}
	if err := j.RecordBumpRawTx(ctx, runID, seq, finalized, "deadbeef"); err == nil {
		t.Fatal("a child whose txid moved during signing was accepted")
	} else {
		t.Logf("refused, correctly: %v", err)
	}
}

// Two children of one run number themselves, which is what makes a second lift
// representable at all — and what lets the recovery screen tell them apart.
func TestASecondBumpOfOneRunIsNumberedSeparately(t *testing.T) {
	ctx := context.Background()
	j := open(t)
	const runID = "run-with-two-children"
	published(t, j, runID)

	first, err := j.BeginBump(ctx, runID, samplePlan())
	if err != nil {
		t.Fatalf("first BeginBump: %v", err)
	}
	if _, err := j.AbandonBump(ctx, &fakeCore{}, runID, first); err != nil {
		t.Fatalf("AbandonBump: %v", err)
	}
	second, err := j.BeginBump(ctx, runID, samplePlan())
	if err != nil {
		t.Fatalf("second BeginBump: %v", err)
	}
	if second != first+1 {
		t.Errorf("the second bump is %d and the first was %d", second, first)
	}

	all, err := j.Bumps(ctx, runID)
	if err != nil {
		t.Fatalf("Bumps: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("the run has %d children, want 2", len(all))
	}

	// Only the live one is unfinished, which is what doctor and the recovery
	// screen list.
	unfinished, err := j.UnfinishedBumps(ctx)
	if err != nil {
		t.Fatalf("UnfinishedBumps: %v", err)
	}
	if len(unfinished) != 1 || unfinished[0].Seq != second {
		t.Errorf("unfinished bumps: %+v", unfinished)
	}
}

// A bump against a run the journal has never heard of is ErrNoRun, and a bump
// the journal has never heard of is ErrNoBump. Both matter to a caller that has
// to tell "nothing to do" from "something went wrong".
func TestTheBumpSentinels(t *testing.T) {
	ctx := context.Background()
	j := open(t)

	if _, err := j.BeginBump(ctx, "no-such-run", samplePlan()); !errors.Is(err, journal.ErrNoRun) {
		t.Errorf("BeginBump on a missing run: %v", err)
	}

	const runID = "run-without-children"
	published(t, j, runID)
	if _, err := j.LoadBump(ctx, runID, 1); !errors.Is(err, journal.ErrNoBump) {
		t.Errorf("LoadBump on a missing bump: %v", err)
	}
	if err := j.MarkBumpPublishing(ctx, runID, 1); !errors.Is(err, journal.ErrNoBump) {
		t.Errorf("MarkBumpPublishing on a missing bump: %v", err)
	}
	if err := j.RecordBumpSigner(ctx, runID, 1, "cold1",
		journal.SignerAwaiting); !errors.Is(err, journal.ErrNoBump) {
		t.Errorf("RecordBumpSigner on a missing bump: %v", err)
	}
}
