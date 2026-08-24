package journal_test

import (
	"context"
	"errors"
	"testing"

	"github.com/AusDavo/winthistle/internal/journal"
	"github.com/AusDavo/winthistle/internal/lnd"
)

// A receipt naming a channel this run never opened must not be recorded.
//
// The reason is I-1 and not tidiness. Arming is "every channel in this batch
// reached chan_pending", so anything that can add a pending channel from outside
// the batch is a way to make n of n arrive without n channels being recoverable.
func TestAReceiptForAChannelNotInTheBatchIsRefused(t *testing.T) {
	ctx := context.Background()
	j := open(t)
	mine := ids(t, 3)
	if err := j.Begin(ctx, "r1", batch(t, mine)); err != nil {
		t.Fatalf("Begin: %v", err)
	}

	stranger := ids(t, 1)[0]
	err := j.MarkPending(ctx, "r1", stranger,
		lnd.ChannelPoint{TxID: fixtureTxID, Index: 7})
	if err == nil {
		t.Fatal("a receipt for a channel outside the batch was accepted")
	}
	if !errors.Is(err, journal.ErrNoRun) {
		t.Errorf("error is %v, want it to wrap ErrNoRun so a caller can tell "+
			"this apart from a database failure", err)
	}

	run, err := j.Load(ctx, "r1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(run.Channels) != 3 {
		t.Errorf("the run has %d channels, want the 3 it opened: a refused "+
			"receipt must not add one", len(run.Channels))
	}
	if run.State == journal.StateArmed {
		t.Fatal("the run is armed. A receipt from outside the batch moved the I-1 gate")
	}
}

// The arming gate counts channels, not receipts. Two receipts for one channel
// must not stand in for the second channel's.
//
// This is the test for the implementation that was not chosen: a counter
// incremented per receipt would read three receipts as three channels and call a
// two-channel-short batch armed, which is the exact failure I-1 exists to make
// impossible.
func TestDuplicateReceiptsCannotForgeAnArmedBatch(t *testing.T) {
	ctx := context.Background()
	j := open(t)
	mine := ids(t, 3)
	if err := j.Begin(ctx, "r1", batch(t, mine)); err != nil {
		t.Fatalf("Begin: %v", err)
	}

	// One channel, three times over.
	for i := range 3 {
		if err := j.MarkPending(ctx, "r1", mine[0],
			lnd.ChannelPoint{TxID: fixtureTxID, Index: 0}); err != nil {
			t.Fatalf("MarkPending %d: %v", i, err)
		}
	}

	run, err := j.Load(ctx, "r1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if run.State == journal.StateArmed {
		t.Fatal("three receipts for one channel armed a three-channel batch. " +
			"Two peers hold no commitment signature and the publish gate is open")
	}

	pending := 0
	for _, c := range run.Channels {
		if c.State == journal.ChanPending {
			pending++
		}
	}
	if pending != 1 {
		t.Errorf("%d channels read as pending, want 1", pending)
	}

	// And the gate itself, asked directly.
	if err := j.MarkPublishing(ctx, "r1"); !errors.Is(err, journal.ErrNotArmed) {
		t.Errorf("MarkPublishing gave %v, want ErrNotArmed", err)
	}
}

// A second receipt for the same channel naming a *different* outpoint is not a
// duplicate — it is a disagreement about which outpoint the channel is at.
//
// The journalled outpoint is what an abort abandons, so overwriting it silently
// would point the teardown at the wrong channel. There is no legitimate route
// here: LND commits to the funding outpoint at psbt_verify (I-3).
func TestAReceiptCannotMoveAChannelsOutpoint(t *testing.T) {
	ctx := context.Background()
	j := open(t)
	mine := ids(t, 2)
	if err := j.Begin(ctx, "r1", batch(t, mine)); err != nil {
		t.Fatalf("Begin: %v", err)
	}

	first := lnd.ChannelPoint{TxID: fixtureTxID, Index: 0}
	if err := j.MarkPending(ctx, "r1", mine[0], first); err != nil {
		t.Fatalf("MarkPending: %v", err)
	}

	// The same receipt again is not a conflict, and must stay harmless: LND is
	// allowed to be told something twice.
	if err := j.MarkPending(ctx, "r1", mine[0], first); err != nil {
		t.Fatalf("repeating the identical receipt was refused: %v", err)
	}

	const otherTxID = "1111111111111111111111111111111111111111111111111111111111111111"
	err := j.MarkPending(ctx, "r1", mine[0],
		lnd.ChannelPoint{TxID: otherTxID, Index: 1})
	if !errors.Is(err, journal.ErrOutpointMoved) {
		t.Errorf("second receipt at a different outpoint gave %v, want "+
			"ErrOutpointMoved. That outpoint is what an abort abandons", err)
	}

	run, err := j.Load(ctx, "r1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, c := range run.Channels {
		if c.PendingChanID != mine[0] {
			continue
		}
		if c.Outpoint != first {
			t.Errorf("the journalled outpoint is now %v, want the first one %v",
				c.Outpoint, first)
		}
	}
}
