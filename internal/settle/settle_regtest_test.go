package settle_test

import (
	"context"
	"testing"
	"time"

	"github.com/AusDavo/winthistle/internal/abort"
	"github.com/AusDavo/winthistle/internal/lnd"
	"github.com/AusDavo/winthistle/internal/regtestenv"
	"github.com/AusDavo/winthistle/internal/settle"
	"github.com/lightningnetwork/lnd/lnrpc"
)

const (
	fixtureChannelSat = 250_000
	fixtureFeeRate    = 10.0
)

func harnessCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

func intendedPolicy() settle.Policy {
	return settle.Policy{
		// Deliberately not LND's defaults, so "applied" is distinguishable from
		// "never touched": LND opens at 1000 msat base and 1 ppm.
		BaseFeeMsat:   0,
		FeeRatePPM:    250,
		TimeLockDelta: 144,
		MaxHTLCMsat:   uint64(fixtureChannelSat) * 1000 * 99 / 100,
	}
}

// armOnePendingChannel drives steps 2 to 7 for a single channel and stops there:
// LND holds a pending channel, the peer holds its commitment signature, and
// nothing has been broadcast.
//
// It is the state Phase 2 has to be able to describe without acting on, and it
// is the only way to get a pending channel that will stay pending.
func armOnePendingChannel(t *testing.T, env *regtestenv.Env) (lnd.ChannelPoint, string) {
	t.Helper()

	peer := env.Peers(t)[0]
	stream := env.OpenShimStream(t, peer, fixtureChannelSat)
	funded := env.BuildFundingPSBT(t, env.Cold, []*regtestenv.Stream{stream}, fixtureFeeRate)
	env.ReleaseLocksAtCleanup(t, env.Cold, funded.Inputs)

	env.Verify(t, stream, funded.Base64)
	rawTx, _ := env.SignAndCombine(t, funded)
	cp := env.Finalize(t, stream, rawTx)

	// Never published. The abort path is how this ends.
	if env.InMempool(t, cp.TxID) {
		t.Fatalf("%s reached the mempool; this fixture must not broadcast", cp.TxID)
	}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_, err := abort.AbandonPending(ctx, env.Alice.Lightning, cp,
			func(context.Context, abort.BluntRequest) (bool, error) { return true, nil })
		if err != nil {
			t.Errorf("cleaning up %s: %v", cp, err)
		}
	})
	return cp, peer
}

// TestUpdateChannelPolicyRefusesAPendingChannelInsideASuccessOK is the finding
// this package is built around, observed against a live node.
//
// docs/design.html leaves open whether UpdateChannelPolicy accepts a pending
// channel point, and defuses the question by polling. The answer is worse than
// either option the question offered: the call does not fail. It returns a nil
// error and puts the refusal in PolicyUpdateResponse.failed_updates, with reason
// UPDATE_FAILURE_PENDING and the text "not yet confirmed".
//
// So a caller that checked only the error would record a policy it had not
// applied, and go on routing at LND's defaults believing otherwise. Polling is
// still the right answer; reading failed_updates is what makes the polling work.
func TestUpdateChannelPolicyRefusesAPendingChannelInsideASuccessOK(t *testing.T) {
	env := regtestenv.Start(t)
	ctx := harnessCtx(t)

	cp, peer := armOnePendingChannel(t, env)
	m := settle.Member{
		Channel: cp, Peer: peer, AmountSat: fixtureChannelSat, Policy: intendedPolicy(),
	}

	// The raw call first, so the finding is about LND rather than about us.
	resp, err := env.Alice.Lightning.UpdateChannelPolicy(ctx, &lnrpc.PolicyUpdateRequest{
		Scope:         &lnrpc.PolicyUpdateRequest_ChanPoint{ChanPoint: cp.RPC()},
		BaseFeeMsat:   0,
		FeeRatePpm:    250,
		TimeLockDelta: 144,
	})
	if err != nil {
		t.Fatalf("UpdateChannelPolicy on a pending channel returned an error, which "+
			"is not what v0.21.2-beta does — the finding may have changed: %v", err)
	}
	if len(resp.GetFailedUpdates()) == 0 {
		t.Fatal("LND reported no failure for a pending channel. If that is now " +
			"true, UpdateChannelPolicy accepts a pending channel point and the " +
			"design's open question has a different answer")
	}
	f := resp.GetFailedUpdates()[0]
	t.Logf("LND: err=nil, failed_updates[0] = %s (%q) for %s:%d",
		f.GetReason(), f.GetUpdateError(),
		f.GetOutpoint().GetTxidStr(), f.GetOutpoint().GetOutputIndex())

	if f.GetReason() != lnrpc.UpdateFailure_UPDATE_FAILURE_PENDING {
		t.Errorf("reason = %v, want UPDATE_FAILURE_PENDING", f.GetReason())
	}

	// And ours reads it correctly.
	out, err := settle.ApplyPolicy(ctx, env.Alice.Lightning, m)
	if err != nil {
		t.Fatalf("ApplyPolicy: %v", err)
	}
	if out.Applied {
		t.Fatal("ApplyPolicy reported a policy it had not applied")
	}
	if !out.Retryable() {
		t.Error("a pending channel becomes confirmed; the loop must keep asking")
	}
}

// TestTheSettlementPassPoliciesAChannelAndLearnsItsDepth is Phase 2 end to end,
// one block at a time.
//
// The channel is opened the ordinary way — LND funds and broadcasts it — because
// what is under test is settlement, not arming, and because a batch member's
// funding transaction only confirms if the batch is published, which is the one
// thing the test suite does in exactly one other place.
//
// The depth it opens at is the point. A peer states its minimum_depth in
// accept_channel and LND exposes it over no RPC, so the depth at which the
// channel first appears in ListChannels is the only authoritative reading an
// initiator can get. Mining a block at a time is what makes that reading
// meaningful.
func TestTheSettlementPassPoliciesAChannelAndLearnsItsDepth(t *testing.T) {
	env := regtestenv.Start(t)
	ctx := harnessCtx(t)

	peer := env.Peers(t)[0]
	cp := env.OpenPlainChannel(t, peer, fixtureChannelSat)
	t.Logf("opened %s to %s", cp, peer[:16])

	members := []settle.Member{{
		Channel: cp, Peer: peer, AmountSat: fixtureChannelSat, Policy: intendedPolicy(),
	}}
	opts := settle.Options{
		Interval:    time.Second,
		Chain:       env.Node,
		FundingTxID: cp.TxID,
	}

	var res *settle.Result
	for block := 0; block <= settle.MaxDepth+2; block++ {
		var err error
		res, err = settle.Tick(ctx, env.Alice.Lightning, members, res, opts)
		if err != nil {
			t.Fatalf("Tick: %v", err)
		}
		if res.Done() {
			break
		}
		env.Mine(t, 1)
		// LND has to see the block before the next tick means anything.
		time.Sleep(1500 * time.Millisecond)
	}

	t.Logf("%s", res.Summary())
	t.Logf("\n%s", res.Report())

	if !res.Done() {
		t.Fatalf("the channel was not settled within %d blocks:\n%s",
			settle.MaxDepth+2, res.Report())
	}
	s := res.States[0]
	if !s.Policy.Applied {
		t.Fatalf("policy not applied: %s", s.Policy)
	}
	if s.ObservedDepth <= 0 {
		t.Fatal("the depth the channel opened at was not recorded, and it is the " +
			"only authoritative reading of the peer's minimum_depth available")
	}
	if s.ObservedDepth != s.ExpectedDepth {
		// Not a failure: the prediction is LND's default policy and the peer is
		// entitled to a different one. Worth seeing when it happens.
		t.Logf("this peer opened at %d confirmations; LND's default policy for a "+
			"channel of %d sat predicts %d", s.ObservedDepth, fixtureChannelSat,
			s.ExpectedDepth)
	}

	// And the policy really is on the channel, read back from LND rather than
	// believed from the response.
	assertPolicyOnChain(t, env, cp, intendedPolicy())
}

// assertPolicyOnChain reads the channel's own edge back out of the graph.
//
// It mines first, and that is not impatience. A policy applied through
// UpdateChannelPolicy lands in the graph database and in the switch immediately,
// but GetNodeInfo's channel list is the *announced* graph, and BOLT 7 puts the
// channel announcement six confirmations after the funding transaction. So the
// settlement pass is finished well before the policy is readable back this way,
// which is itself worth knowing: the read-back is corroboration, not the gate.
func assertPolicyOnChain(t *testing.T, env *regtestenv.Env, cp lnd.ChannelPoint,
	want settle.Policy) {

	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	env.Mine(t, 6)

	info, err := env.Alice.Lightning.GetInfo(ctx, &lnrpc.GetInfoRequest{})
	if err != nil {
		t.Fatalf("GetInfo: %v", err)
	}
	self := info.GetIdentityPubkey()

	resp, err := env.Alice.Lightning.ListChannels(ctx, &lnrpc.ListChannelsRequest{})
	if err != nil {
		t.Fatalf("ListChannels: %v", err)
	}
	var chanID uint64
	for _, ch := range resp.GetChannels() {
		if ch.GetChannelPoint() == cp.String() {
			chanID = ch.GetChanId()
		}
	}
	if chanID == 0 {
		t.Fatalf("%s is not in alice's open channels", cp)
	}

	deadline := time.Now().Add(2 * time.Minute)
	for {
		node, err := env.Alice.Lightning.GetNodeInfo(ctx, &lnrpc.NodeInfoRequest{
			PubKey: self, IncludeChannels: true,
		})
		if err != nil {
			t.Fatalf("GetNodeInfo: %v", err)
		}
		for _, e := range node.GetChannels() {
			if e.GetChannelId() != chanID {
				continue
			}
			ours := e.GetNode1Policy()
			if e.GetNode2Pub() == self {
				ours = e.GetNode2Policy()
			}
			if ours == nil {
				t.Fatal("the graph has the channel but no policy of ours on it")
			}
			if ours.GetFeeRateMilliMsat() != int64(want.FeeRatePPM) {
				t.Errorf("fee rate = %d ppm, want %d — LND's default is %d",
					ours.GetFeeRateMilliMsat(), want.FeeRatePPM, settle.DefaultFeeRatePPM)
			}
			if ours.GetTimeLockDelta() != want.TimeLockDelta {
				t.Errorf("CLTV delta = %d, want %d — LND's default is %d",
					ours.GetTimeLockDelta(), want.TimeLockDelta, settle.DefaultTimeLockDelta)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("channel %d never appeared on alice's own node in the "+
				"announced graph", chanID)
		}
		time.Sleep(2 * time.Second)
	}
}

// TestTheFundingHorizonIsReachedByMining is the hazard past the end of Phase 2,
// and on regtest it is a few seconds of mining rather than two weeks.
//
// After lncfg.DefaultMaxWaitNumBlocksFundingConf = 2016 blocks from the
// broadcast height, the *responder* gives up: waitForFundingWithTimeout starts
// waitForTimeout only when !ch.IsInitiator. Our node never does. So the end state
// is asymmetric — the peer has closed its side as FundingCanceled and we are
// still waiting — and if the funding transaction then confirms we hold a channel
// the peer has forgotten.
//
// PendingChannels publishes the countdown as funding_expiry_blocks, computed as
// 2016 + broadcastHeight - currentHeight, and LND's own proto says a negative
// value means the responder has very likely cancelled. Both halves are asserted
// here.
//
// Side effect worth knowing: mining past the horizon is also the cheapest way to
// clear the pending channels that aborted batches leave on the peers. HANDOFF
// said `make harness` was the only cure; it is not, it is just the only one that
// does not advance the chain by two weeks.
func TestTheFundingHorizonIsReachedByMining(t *testing.T) {
	env := regtestenv.Start(t)
	ctx := harnessCtx(t)

	cp, peer := armOnePendingChannel(t, env)
	name := env.PeerNamed(t, peer)
	peerNode := env.PeerNode(t, name)

	before := expiryBlocks(t, ctx, env.Alice.Lightning, cp)
	if before <= 0 {
		t.Fatalf("funding_expiry_blocks is %d before any mining, so this channel is "+
			"already past the horizon", before)
	}
	t.Logf("alice: %d blocks to the horizon; %s holds it pending: %v",
		before, name, isPendingOn(t, ctx, peerNode.Lightning, cp))

	if !isPendingOn(t, ctx, peerNode.Lightning, cp) {
		t.Fatalf("%s does not hold %s pending, so there is nothing to time out", name, cp)
	}

	// Nine or ten seconds of mining on this machine.
	started := time.Now()
	env.Mine(t, settle.ForgetHorizonBlocks)
	t.Logf("mined %d blocks in %s", settle.ForgetHorizonBlocks,
		time.Since(started).Round(time.Millisecond))

	// Exactly at the boundary, and the boundary is inclusive on both sides.
	// funding_expiry_blocks is maxFundingHeight - currentHeight, so it reaches
	// zero on the block the responder gives up on: waitForTimeout fires on
	// `epoch.Height >= maxHeight`. Zero already means gone, not "one to go".
	at := expiryBlocks(t, ctx, env.Alice.Lightning, cp)
	if at != 0 {
		t.Fatalf("funding_expiry_blocks is %d after exactly %d blocks, want 0",
			at, settle.ForgetHorizonBlocks)
	}
	t.Logf("alice: funding_expiry_blocks is 0 — the horizon, to the block")

	env.Mine(t, 1)
	after := expiryBlocks(t, ctx, env.Alice.Lightning, cp)
	if after >= 0 {
		t.Fatalf("funding_expiry_blocks is %d one block past the horizon; LND's "+
			"own proto says a negative value is what says the responder has very "+
			"likely cancelled", after)
	}
	t.Logf("alice: %d blocks past the horizon, and still holding it pending", after)

	// The peer gives up; we do not. The timeout fires on a block notification,
	// so give it a moment after the last block.
	deadline := time.Now().Add(90 * time.Second)
	for isPendingOn(t, ctx, peerNode.Lightning, cp) {
		if time.Now().After(deadline) {
			t.Fatalf("%s still holds %s pending 90s past the horizon", name, cp)
		}
		time.Sleep(2 * time.Second)
	}
	t.Logf("%s has forgotten %s", name, cp)

	if !isPendingOn(t, ctx, env.Alice.Lightning, cp) {
		t.Fatal("alice gave up on the channel too. waitForFundingWithTimeout only " +
			"arms the timeout for the responder, so the initiator is supposed to " +
			"wait forever — if that has changed, the hazard this test describes " +
			"has changed with it")
	}

	// And the settlement pass says so, in the register the operator needs.
	res, err := settle.Tick(ctx, env.Alice.Lightning, []settle.Member{{
		Channel: cp, Peer: peer, AmountSat: fixtureChannelSat, Policy: intendedPolicy(),
	}}, nil, settle.Options{})
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	t.Logf("\n%s", res.Report())
}

func expiryBlocks(t *testing.T, ctx context.Context, cli lnrpc.LightningClient,
	cp lnd.ChannelPoint) int32 {

	t.Helper()
	resp, err := cli.PendingChannels(ctx, &lnrpc.PendingChannelsRequest{})
	if err != nil {
		t.Fatalf("PendingChannels: %v", err)
	}
	for _, p := range resp.GetPendingOpenChannels() {
		if p.GetChannel().GetChannelPoint() == cp.String() {
			return p.GetFundingExpiryBlocks()
		}
	}
	t.Fatalf("%s is not a pending open channel", cp)
	return 0
}

func isPendingOn(t *testing.T, ctx context.Context, cli lnrpc.LightningClient,
	cp lnd.ChannelPoint) bool {

	t.Helper()
	resp, err := cli.PendingChannels(ctx, &lnrpc.PendingChannelsRequest{})
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
