package settle

import (
	"context"
	"testing"

	"github.com/AusDavo/winthistle/internal/lnd"
	"github.com/lightningnetwork/lnd/lnrpc"
	"google.golang.org/grpc"
)

// fakeLND answers the three calls settlement makes, so the one behaviour worth
// pinning without a node — a refusal that arrives inside a successful response —
// can be pinned exactly.
type fakeLND struct {
	open    map[string]bool
	active  map[string]bool
	pending map[string]int32

	// failWith is returned in PolicyUpdateResponse.FailedUpdates, with a nil
	// error, which is what LND does for a pending channel.
	failWith *lnrpc.FailedUpdate

	policyCalls int
}

func (f *fakeLND) PendingChannels(context.Context, *lnrpc.PendingChannelsRequest,
	...grpc.CallOption) (*lnrpc.PendingChannelsResponse, error) {

	resp := &lnrpc.PendingChannelsResponse{}
	for cp, expiry := range f.pending {
		resp.PendingOpenChannels = append(resp.PendingOpenChannels,
			&lnrpc.PendingChannelsResponse_PendingOpenChannel{
				Channel:             &lnrpc.PendingChannelsResponse_PendingChannel{ChannelPoint: cp},
				FundingExpiryBlocks: expiry,
			})
	}
	return resp, nil
}

func (f *fakeLND) ListChannels(context.Context, *lnrpc.ListChannelsRequest,
	...grpc.CallOption) (*lnrpc.ListChannelsResponse, error) {

	resp := &lnrpc.ListChannelsResponse{}
	for cp := range f.open {
		resp.Channels = append(resp.Channels, &lnrpc.Channel{
			ChannelPoint: cp, Active: f.active[cp],
		})
	}
	return resp, nil
}

func (f *fakeLND) UpdateChannelPolicy(context.Context, *lnrpc.PolicyUpdateRequest,
	...grpc.CallOption) (*lnrpc.PolicyUpdateResponse, error) {

	f.policyCalls++
	resp := &lnrpc.PolicyUpdateResponse{}
	if f.failWith != nil {
		resp.FailedUpdates = append(resp.FailedUpdates, f.failWith)
	}
	// Nil error, always. That is the point.
	return resp, nil
}

const testTxID = "0f0e0d0c0b0a09080706050403020100f0e0d0c0b0a090807060504030201000"

func testMember() Member {
	return Member{
		Channel:   lnd.ChannelPoint{TxID: testTxID, Index: 0},
		Peer:      "02cca6c5c966fcf61d121e3a70e03a1cd9eeeea024b26ea666ce974d43b242e636",
		AmountSat: 250_000,
		Policy: Policy{
			BaseFeeMsat:   0,
			FeeRatePPM:    250,
			TimeLockDelta: 80,
		},
	}
}

func failure(cp lnd.ChannelPoint, reason lnrpc.UpdateFailure, detail string) *lnrpc.FailedUpdate {
	return &lnrpc.FailedUpdate{
		Outpoint: &lnrpc.OutPoint{
			TxidStr:     cp.TxID,
			OutputIndex: cp.Index,
		},
		Reason:      reason,
		UpdateError: detail,
	}
}

// The finding this package exists for.
//
// UpdateChannelPolicy on a pending channel returns a NIL ERROR and puts the
// refusal in the response's failed_updates list — localchans.Manager.UpdatePolicy
// walks the graph, finds no edge, calls FetchChannel, sees IsPending and appends
// UPDATE_FAILURE_PENDING with "not yet confirmed". A caller that checked only
// err would record a policy it had not applied, and go on routing at LND's
// defaults believing otherwise.
func TestAPendingChannelIsRefusedInsideASuccessfulResponse(t *testing.T) {
	m := testMember()
	cli := &fakeLND{
		failWith: failure(m.Channel,
			lnrpc.UpdateFailure_UPDATE_FAILURE_PENDING, "not yet confirmed"),
	}

	out, err := ApplyPolicy(context.Background(), cli, m)
	if err != nil {
		t.Fatalf("LND returns nil here; ApplyPolicy must not invent an error: %v", err)
	}
	if out.Applied {
		t.Fatal("a pending channel was recorded as policied. LND said nil and put " +
			"the refusal in failed_updates; reading only the error is exactly the " +
			"mistake this test exists to catch")
	}
	if out.Reason != lnrpc.UpdateFailure_UPDATE_FAILURE_PENDING {
		t.Errorf("reason = %v", out.Reason)
	}
	if !out.Retryable() {
		t.Error("a pending channel becomes confirmed; the loop must keep asking")
	}
}

func TestAnAppliedPolicyIsAnEmptyFailureList(t *testing.T) {
	cli := &fakeLND{}
	out, err := ApplyPolicy(context.Background(), cli, testMember())
	if err != nil {
		t.Fatalf("ApplyPolicy: %v", err)
	}
	if !out.Applied {
		t.Fatalf("an empty failed_updates list is the only success there is: %v", out)
	}
}

// A failure about somebody else's channel is not this channel's failure. The
// request names one channel, so this cannot happen against LND today — but the
// response is a list, and reading its first element blindly would attribute
// another channel's refusal to this one.
func TestAFailureAboutAnotherChannelIsIgnored(t *testing.T) {
	other := lnd.ChannelPoint{TxID: testTxID, Index: 7}
	cli := &fakeLND{
		failWith: failure(other, lnrpc.UpdateFailure_UPDATE_FAILURE_PENDING, "not yet confirmed"),
	}
	out, err := ApplyPolicy(context.Background(), cli, testMember())
	if err != nil {
		t.Fatalf("ApplyPolicy: %v", err)
	}
	if !out.Applied {
		t.Fatal("another channel's refusal was attributed to this one")
	}
}

// An invalid policy is refused the same way on the first attempt and the
// hundredth. Retrying it forever would leave a channel routing at 1 ppm with a
// spinner beside it, so the loop has to stop and say so.
func TestAnInvalidPolicyIsNotRetryable(t *testing.T) {
	m := testMember()
	m.Policy.TimeLockDelta = 4 // below routing.MinCLTVDelta

	out, err := ApplyPolicy(context.Background(), &fakeLND{}, m)
	if err != nil {
		t.Fatalf("ApplyPolicy: %v", err)
	}
	if out.Applied {
		t.Fatal("LND would have refused this policy outright")
	}
	if out.Retryable() {
		t.Fatal("waiting cannot make a CLTV delta of 4 valid")
	}

	s := State{Policy: out}
	if !s.Stuck() {
		t.Fatal("a permanently invalid policy must show as stuck")
	}
}

func TestPolicyValidateMatchesLNDsBounds(t *testing.T) {
	ok := Policy{TimeLockDelta: MinTimeLockDelta}
	if err := ok.Validate(); err != nil {
		t.Errorf("LND's own minimum was refused: %v", err)
	}
	for name, p := range map[string]Policy{
		"delta below routing.MinCLTVDelta": {TimeLockDelta: MinTimeLockDelta - 1},
		"negative base fee":                {TimeLockDelta: 80, BaseFeeMsat: -1},
		"positive inbound base fee":        {TimeLockDelta: 80, InboundBaseFeeMsat: 1},
		"positive inbound rate":            {TimeLockDelta: 80, InboundFeeRatePPM: 1},
	} {
		if err := p.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// ExpectedDepth reproduces the NumRequiredConfs closure wired up in server.go:
// 6 for anything above MaxFundingAmount, otherwise 6*stake/MaxFundingAmount
// clamped into [3, 6]. It is a prediction about a peer running stock LND and the
// reports say so; this pins the arithmetic, not the claim.
func TestExpectedDepthReproducesLNDsDefaultPolicy(t *testing.T) {
	cases := map[int64]int64{
		20_000:               MinDepth, // LND's own MinChanFundingSize
		250_000:              MinDepth, // the harness fixtures
		MaxFundingAmount / 2: MinDepth, // 3 exactly, at the clamp
		MaxFundingAmount:     MaxDepth,
		MaxFundingAmount + 1: MaxDepth, // wumbo
		1_000_000_000:        MaxDepth,
		// 6*14,000,000/16,777,215 = 5.006, and the closure truncates.
		14_000_000: 5,
	}
	for amount, want := range cases {
		if got := ExpectedDepth(amount); got != want {
			t.Errorf("ExpectedDepth(%d) = %d, want %d", amount, got, want)
		}
	}
}

// Tick carries forward what a single pass cannot know, and stops asking once a
// channel is done. Both matter: the observed depth is the only authoritative
// reading of a peer's minimum_depth there is, and it is only readable in the one
// pass where the channel first appears open.
func TestTickRecordsTheDepthAChannelOpenedAtAndThenStops(t *testing.T) {
	m := testMember()
	cp := m.Channel.String()
	ctx := context.Background()

	cli := &fakeLND{
		open:    map[string]bool{},
		active:  map[string]bool{},
		pending: map[string]int32{cp: 2010},
		failWith: failure(m.Channel,
			lnrpc.UpdateFailure_UPDATE_FAILURE_PENDING, "not yet confirmed"),
	}
	chain := &fakeChain{confs: 1}
	opts := Options{Chain: chain, FundingTxID: testTxID}

	first, err := Tick(ctx, cli, []Member{m}, nil, opts)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if first.States[0].Settled() {
		t.Fatal("a pending channel is not settled")
	}
	if got := first.States[0].ExpiryBlocks; got != 2010 {
		t.Errorf("funding_expiry_blocks = %d, want 2010", got)
	}

	// The block that opens it. The channel appears in ListChannels at depth 3,
	// which is the peer's minimum_depth read from above.
	cli.open[cp] = true
	cli.active[cp] = true
	delete(cli.pending, cp)
	cli.failWith = nil
	chain.confs = 3

	second, err := Tick(ctx, cli, []Member{m}, first, opts)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	s := second.States[0]
	if !s.Settled() {
		t.Fatalf("still not settled: %v", s.Policy)
	}
	if s.ObservedDepth != 3 {
		t.Errorf("observed depth = %d, want 3", s.ObservedDepth)
	}
	if s.Attempts != 2 {
		t.Errorf("attempts = %d, want 2", s.Attempts)
	}

	// Deeper now, and the reading must not be revised: on a later pass the
	// transaction is simply older, and the depth says nothing about the peer.
	calls := cli.policyCalls
	chain.confs = 20
	third, err := Tick(ctx, cli, []Member{m}, second, opts)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if third.States[0].ObservedDepth != 3 {
		t.Errorf("observed depth was revised to %d", third.States[0].ObservedDepth)
	}
	if cli.policyCalls != calls {
		t.Error("the policy was applied again after it had landed")
	}
}

type fakeChain struct{ confs int64 }

func (f *fakeChain) Confirmations(context.Context, string) (int64, bool, error) {
	return f.confs, true, nil
}

// UPDATE_FAILURE_UNKNOWN is zero, and zero is also "no call has been made". If
// those two are conflated, an UNKNOWN refusal is retried forever: it is not
// retryable, but it also does not look like a failure, so the loop neither stops
// nor progresses. Refused is what separates them.
func TestAnUnknownRefusalStopsTheLoopRatherThanSpinning(t *testing.T) {
	m := testMember()
	cli := &fakeLND{
		failWith: failure(m.Channel, lnrpc.UpdateFailure_UPDATE_FAILURE_UNKNOWN, ""),
	}

	out, err := ApplyPolicy(context.Background(), cli, m)
	if err != nil {
		t.Fatalf("ApplyPolicy: %v", err)
	}
	if out.Applied || !out.Refused {
		t.Fatalf("an UNKNOWN failed_update is a refusal: %+v", out)
	}
	if out.Retryable() {
		t.Fatal("there is nothing identifiable to wait for, so waiting is not a plan")
	}
	if st := (State{Policy: out}); !st.Stuck() {
		t.Fatal("an unretryable refusal must show as stuck")
	}

	// And an outcome nobody has produced yet is neither applied nor refused, so
	// the loop knows to make the call.
	var never PolicyOutcome
	if never.Settled() || never.Refused {
		t.Fatalf("a zero outcome must read as not attempted: %+v", never)
	}
	if never.String() != "not attempted" {
		t.Errorf("zero outcome renders as %q", never.String())
	}
}
