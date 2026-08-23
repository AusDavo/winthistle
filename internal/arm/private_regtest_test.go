package arm_test

import (
	"errors"
	"testing"

	"github.com/AusDavo/winthistle/internal/abort"
	"github.com/AusDavo/winthistle/internal/arm"
	"github.com/AusDavo/winthistle/internal/plan"
	"github.com/AusDavo/winthistle/internal/regtestenv"
	"github.com/AusDavo/winthistle/internal/reserve"
)

// TestAPrivateMemberIsInvisibleToTheAnchorReserve exercises the private path
// through both arm and reserve, which was written and unexercised.
//
// It matters twice over, and both times it is the same source reading:
// enforceNewReservedValue returns before it counts anything when the channel is
// unannounced, and CurrentNumAnchorChans skips private channels when it counts.
// So a private member is not judged at psbt_verify and does not raise the figure
// the node has to hold afterwards — and passing n rather than the announced
// count would have the operator top their node up to satisfy a check that is not
// going to run.
//
// Nothing here is published. Three streams open, the counts are asserted, and
// the batch is taken apart again.
func TestAPrivateMemberIsInvisibleToTheAnchorReserve(t *testing.T) {
	env := regtestenv.Start(t)
	ctx := harnessCtx(t)

	peers := env.Peers(t)
	if len(peers) < 3 {
		t.Skipf("this test needs 3 peers, alice has %d", len(peers))
	}

	chans := []arm.Channel{
		{Peer: peers[0], AmountSat: fixtureChannelSat},
		{Peer: peers[1], AmountSat: fixtureChannelSat, Private: true},
		{Peer: peers[2], AmountSat: fixtureChannelSat},
	}

	// Phase 0 counts the batch this way, before any stream exists.
	batch := arm.BatchOf(chans)
	if batch.Public != 2 || batch.Private != 1 {
		t.Fatalf("BatchOf counted %d public and %d private, want 2 and 1",
			batch.Public, batch.Private)
	}

	finding, err := reserve.Check(ctx, env.Alice.WalletKit, batch)
	if err != nil {
		t.Fatalf("the anchor-reserve pre-flight: %v", err)
	}
	t.Logf("reserve pre-flight: %s", finding.Summary())
	t.Logf("\n%s", finding.Report())

	// The reserve is worked out for two channels, not three.
	if finding.Batch.Public != 2 {
		t.Errorf("the finding is about %d public channels", finding.Batch.Public)
	}
	twoMore, err := reserve.Check(ctx, env.Alice.WalletKit, reserve.Batch{Public: 2})
	if err != nil {
		t.Fatalf("re-checking: %v", err)
	}
	if finding.AfterBatch != twoMore.AfterBatch {
		t.Errorf("a batch of 2 public + 1 private needs %d after the batch and a "+
			"batch of 2 public needs %d; the private member is being counted",
			finding.AfterBatch, twoMore.AfterBatch)
	}
	if finding.AfterBatch == reserve.MaxReserve {
		// RequiredReserve is 10,000 sat per public anchor channel capped at
		// 100,000, and this node has been through enough tests to be at the cap.
		// The comparison above is then true for both the right reason and the
		// wrong one, so say so rather than let it read as a strong result.
		t.Logf("this node is at the %d sat reserve cap, so the figures would "+
			"agree for any batch size; the counts below are the real assertion",
			reserve.MaxReserve)
	}

	streams, err := arm.Open(ctx, env.Alice.Lightning, "regtest", chans)
	t.Cleanup(func() {
		if streams == nil {
			return
		}
		streams.Close()
		for _, id := range streams.PendingChanIDs() {
			if err := abort.CancelShim(ctx, env.Alice.Lightning, id); err != nil &&
				!errors.Is(err, abort.ErrNoShim) {
				t.Errorf("cancelling %s: %v", id, err)
			}
		}
	})
	if err != nil {
		t.Fatalf("opening the batch's funding streams: %v", err)
	}

	// The flag survived the round trip through LND, and the streams count the
	// same way the plan did.
	if got := streams.PublicCount(); got != 2 {
		t.Errorf("PublicCount = %d, want 2", got)
	}
	if got := streams.Batch(); got != batch {
		t.Errorf("the streams are %+v and the reserve was checked for %+v", got, batch)
	}
	if err := finding.StillApplies(streams.Batch()); err != nil {
		t.Errorf("the finding does not describe the batch that opened: %v", err)
	}
	private := 0
	for _, st := range streams.All {
		if st.Private {
			private++
		}
	}
	if private != 1 {
		t.Errorf("%d of the open streams are private, want 1", private)
	}
}

// An all-private batch is never judged, so topping the node up for it would be
// paying a fee to satisfy a check that does not run.
func TestAnAllPrivateBatchNeedsNoReserveTopUp(t *testing.T) {
	env := regtestenv.Start(t)
	ctx := harnessCtx(t)

	finding, err := reserve.Check(ctx, env.Alice.WalletKit, reserve.Batch{Private: 3})
	if err != nil {
		t.Fatalf("the anchor-reserve pre-flight: %v", err)
	}
	if finding.Verdict() != reserve.NotApplicable {
		t.Fatalf("verdict = %v, want not applicable", finding.Verdict())
	}
	if finding.Blocking() {
		t.Error("a check that does not run cannot block")
	}
	t.Logf("\n%s", finding.Report())

	// And no address is minted for it: TopUpAddress would be a call to LND for
	// an output nobody is going to build.
	topUp, err := plan.ReserveTopUp(ctx, env.Alice.Lightning, finding)
	if err != nil {
		t.Fatalf("ReserveTopUp: %v", err)
	}
	if topUp != nil {
		t.Fatalf("a top-up of %d sat was built for an all-private batch",
			topUp.AmountSat)
	}
}
