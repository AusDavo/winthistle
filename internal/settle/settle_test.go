package settle

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

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

	// failFor overrides failWith for one channel point, so a batch can have one
	// member refused while the rest are applied. A present-but-nil entry means
	// that member succeeds. Issue #6 is entirely about members not sharing a
	// fate, so the fake has to be able to give them different ones.
	failFor map[string]*lnrpc.FailedUpdate

	// answer, if set, is called before each policy call with the channel point
	// and the number of calls made against it so far, so a test can make LND
	// change its mind. That is the other half of issue #6: a refusal that a
	// later attempt does not repeat.
	answer func(f *fakeLND, cp string, nth int)

	policyCalls int
	callsFor    map[string]int
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

func (f *fakeLND) UpdateChannelPolicy(_ context.Context, in *lnrpc.PolicyUpdateRequest,
	_ ...grpc.CallOption) (*lnrpc.PolicyUpdateResponse, error) {

	f.policyCalls++
	cp := fmt.Sprintf("%s:%d", in.GetChanPoint().GetFundingTxidStr(),
		in.GetChanPoint().GetOutputIndex())
	if f.callsFor == nil {
		f.callsFor = map[string]int{}
	}
	f.callsFor[cp]++
	if f.answer != nil {
		f.answer(f, cp, f.callsFor[cp])
	}

	fail := f.failWith
	if v, ok := f.failFor[cp]; ok {
		fail = v
	}
	resp := &lnrpc.PolicyUpdateResponse{}
	if fail != nil {
		resp.FailedUpdates = append(resp.FailedUpdates, fail)
	}
	// Nil error, always. That is the point.
	return resp, nil
}

// opens marks a channel open with its peer online, which is what makes a member
// Settled once its policy lands.
func (f *fakeLND) opens(cp string) {
	if f.open == nil {
		f.open, f.active = map[string]bool{}, map[string]bool{}
	}
	f.open[cp], f.active[cp] = true, true
	delete(f.pending, cp)
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

// ExpectedDepth reproduces lnwallet.ScaleNumConfs: 6 for anything above
// MaxFundingAmount, otherwise 6*stake/MaxFundingAmount clamped into
// [MinDepth, MaxDepth]. It is a prediction about a peer running stock LND and
// the reports say so; this pins the arithmetic, not the claim. That the two
// bounds are LND's own is TestTranscribedConstantsMatchLND's to say.
func TestExpectedDepthReproducesLNDsDefaultPolicy(t *testing.T) {
	cases := map[int64]int64{
		20_000:  MinDepth, // LND's own MinChanFundingSize, far under the clamp
		250_000: MinDepth, // the harness fixtures
		// 6*8,388,607/16,777,215 = 2.99, and the closure truncates.
		MaxFundingAmount / 2: 2,
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

// UPDATE_FAILURE_UNKNOWN is zero, and zero is also "no call has been made", so
// "a refusal happened" needs its own boolean. Without Refused an UNKNOWN refusal
// is indistinguishable from a call nobody has made yet, and the loop would
// neither retry it nor report it.
//
// The classification around it changed with issue #6 and this is where the
// change is pinned: UNKNOWN is retryable, it is not a verdict on the policy, and
// on its own — before any window has run out — it is not stuck either.
func TestAnUnknownRefusalIsARefusalAndIsRetryable(t *testing.T) {
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
	if !out.Retryable() {
		t.Fatal("UNKNOWN is LND's catch-all, not a verdict: a channel that had " +
			"just opened with its peer offline landed here on mainnet, and the " +
			"identical update applied by hand minutes later")
	}
	if out.Terminal() {
		t.Fatal("only INVALID_PARAMETER is a refusal against bounds that cannot " +
			"change")
	}
	if !out.Unexplained() {
		t.Fatal("UNKNOWN names nothing to wait for, so the retry has to be bounded")
	}
	if st := (State{Policy: out}); st.Stuck() {
		t.Fatal("stuck on the first refusal is the bug: the bound is the window, " +
			"not the enum")
	}
	if st := (State{Policy: out, RetriesExhausted: true}); !st.Stuck() {
		t.Fatal("once the window has run out there is nothing left to retry")
	}

	// And an outcome nobody has produced yet is neither applied nor refused, so
	// the loop knows to make the call.
	var never PolicyOutcome
	if never.Refused || never.Retryable() || never.Terminal() || never.Unexplained() {
		t.Fatalf("a zero outcome must read as not attempted: %+v", never)
	}
	if never.String() != "not attempted" {
		t.Errorf("zero outcome renders as %q", never.String())
	}
}

// The four tests below are issue #6, which the first live mainnet batch found.
// One channel of five confirmed early, its policy was refused with
// UPDATE_FAILURE_UNKNOWN while its peer was briefly offline, and Phase 2 stopped
// — abandoning the watch on four channels that had not even opened yet, so each
// of them went live at LND's defaults with nothing left running to notice.

// memberAt is one member of a batch: the same funding transaction, one output
// index each, which is what a real batch looks like.
func memberAt(index uint32, peer string) Member {
	m := testMember()
	m.Channel = lnd.ChannelPoint{TxID: testTxID, Index: index}
	m.Peer = peer
	return m
}

const (
	// The peer from the live batch, and two stand-ins. Sixteen distinct leading
	// characters each, because that is all short() keeps and the reports have to
	// be readable.
	peerSilent  = "03d6749842cabfbf" + "0000000000000000000000000000000000000000000000000a"
	peerInvalid = "02aa11bb22cc33dd" + "0000000000000000000000000000000000000000000000000b"
	peerFine    = "02cca6c5c966fcf6" + "0000000000000000000000000000000000000000000000000c"
)

func settleCtx(t *testing.T) context.Context {
	t.Helper()
	// A bound on the whole test rather than on the settlement: every fake answer
	// here is immediate, so anything that takes seconds is a loop that is not
	// terminating, and that should fail rather than hang.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func stateFor(t *testing.T, r *Result, cp string) State {
	t.Helper()
	for _, s := range r.States {
		if s.Member.Channel.String() == cp {
			return s
		}
	}
	t.Fatalf("%s is not in the result", cp)
	return State{}
}

// An UNKNOWN refusal followed by a success is applied, not abandoned.
//
// This is the refusal that stopped the live batch. UNKNOWN is LND's catch-all:
// the channel had just opened with its peer offline, and the identical update
// applied cleanly a few minutes later. So the loop has to try again.
func TestAnUnknownRefusalFollowedByASuccessIsApplied(t *testing.T) {
	m := testMember()
	cp := m.Channel.String()

	cli := &fakeLND{
		open:   map[string]bool{cp: true},
		active: map[string]bool{cp: false}, // the peer is offline, which is the case
		failFor: map[string]*lnrpc.FailedUpdate{
			cp: failure(m.Channel, lnrpc.UpdateFailure_UPDATE_FAILURE_UNKNOWN,
				"could not update policies"),
		},
	}
	// The peer comes back between one attempt and the next, and LND stops
	// refusing. Nothing announced it; the only way to find out was to ask again.
	cli.answer = func(f *fakeLND, cp string, nth int) {
		if nth >= 2 {
			f.failFor[cp] = nil
			f.active[cp] = true
		}
	}

	res, err := Settle(settleCtx(t), cli, []Member{m},
		Options{Interval: time.Millisecond})
	if err != nil {
		t.Fatalf("a transient refusal ended the settlement: %v", err)
	}
	if !res.Done() {
		t.Fatalf("not settled:\n%s", res.Report())
	}
	s := res.States[0]
	if !s.Policy.Applied {
		t.Fatalf("policy = %s, and the second attempt was accepted", s.Policy)
	}
	if s.Attempts != 2 {
		t.Errorf("attempts = %d, want 2: one refusal and one success", s.Attempts)
	}
}

// A terminally stuck member does not stop the loop for the others, and the
// others still get their policy.
//
// The worse half of the issue, and independent of the first: even where a
// channel's policy genuinely cannot be applied, the other members have nothing
// to do with it. Here the stuck member runs out of patience on its second
// attempt, while the other two are still pending and do not accept the policy
// until their fourth — so if the loop stops for the stuck one, they never get
// it, which is exactly what happened on mainnet.
func TestAStuckMemberDoesNotStopTheLoopForTheOthers(t *testing.T) {
	silent := memberAt(1, peerSilent)
	first, second := memberAt(0, peerFine), memberAt(2, peerInvalid)
	members := []Member{first, silent, second}

	stuck := silent.Channel.String()
	cli := &fakeLND{
		pending: map[string]int32{
			first.Channel.String():  2000,
			second.Channel.String(): 2000,
		},
		failFor: map[string]*lnrpc.FailedUpdate{
			stuck: failure(silent.Channel,
				lnrpc.UpdateFailure_UPDATE_FAILURE_UNKNOWN, "could not update policies"),
			first.Channel.String(): failure(first.Channel,
				lnrpc.UpdateFailure_UPDATE_FAILURE_PENDING, "not yet confirmed"),
			second.Channel.String(): failure(second.Channel,
				lnrpc.UpdateFailure_UPDATE_FAILURE_PENDING, "not yet confirmed"),
		},
	}
	// The two pending channels confirm on the fourth pass, well after the stuck
	// one has been given up on.
	cli.answer = func(f *fakeLND, cp string, nth int) {
		if cp == stuck || nth < 4 {
			return
		}
		f.failFor[cp] = nil
		f.opens(cp)
	}

	res, err := Settle(settleCtx(t), cli, members, Options{
		Interval: time.Millisecond,
		// A window this short is exhausted on the second attempt, which is the
		// earliest it may ever be: one retry is the minimum the fix promises.
		RetryWindow: time.Nanosecond,
	})
	if !errors.Is(err, ErrStuck) {
		t.Fatalf("err = %v, want ErrStuck", err)
	}

	for _, m := range []Member{first, second} {
		s := stateFor(t, res, m.Channel.String())
		if !s.Policy.Applied {
			t.Errorf("%s: policy = %s. A channel that had nothing to do with the "+
				"stuck one lost its watcher, which is the defect",
				short(s.Member.Peer), s.Policy)
		}
		if !s.Settled() {
			t.Errorf("%s: not settled:\n%s", short(s.Member.Peer), res.Report())
		}
		if got := cli.callsFor[m.Channel.String()]; got < 4 {
			t.Errorf("%s: %d policy calls, and it does not accept one until the "+
				"fourth", short(s.Member.Peer), got)
		}
	}

	s := stateFor(t, res, stuck)
	if !s.Stuck() {
		t.Fatalf("the silent member is not stuck: %s", s.Policy)
	}
	if got := cli.callsFor[stuck]; got != 2 {
		t.Errorf("%d policy calls on the stuck member, want 2: one refusal, one "+
			"retry, and then left alone rather than asked every pass forever", got)
	}
	if n := len(res.Stuck()); n != 1 {
		t.Errorf("%d stuck members, want 1", n)
	}
}

// INVALID_PARAMETER is still terminal for that member on the first refusal.
//
// The bound on an unexplained refusal must not soften this one. LND checks the
// CLTV delta and the inbound fees against its own bounds before it looks at the
// channel, so the answer on the hundredth attempt is the answer on the first,
// and retrying it is nothing but a slower way to report it.
func TestAnInvalidParameterIsTerminalOnTheFirstRefusal(t *testing.T) {
	bad, good := memberAt(0, peerInvalid), memberAt(1, peerFine)
	badCP, goodCP := bad.Channel.String(), good.Channel.String()

	cli := &fakeLND{
		open:   map[string]bool{badCP: true, goodCP: true},
		active: map[string]bool{badCP: true, goodCP: true},
		failFor: map[string]*lnrpc.FailedUpdate{
			// LND's own refusal, not internal/policy's pre-check: the pre-check
			// never reaches the node, and this test is about the reason arriving
			// from it. The detail is updateEdge's, because updateEdge failing is
			// the only thing that emits this reason — a CLTV delta out of bounds
			// fails the whole call as a gRPC error and never appears here.
			badCP: failure(bad.Channel,
				lnrpc.UpdateFailure_UPDATE_FAILURE_INVALID_PARAMETER,
				"min htlc amount of 1000 mSAT is below min htlc parameter of "+
					"20000 mSAT"),
		},
	}

	res, err := Settle(settleCtx(t), cli, []Member{bad, good},
		Options{Interval: time.Millisecond})
	if !errors.Is(err, ErrStuck) {
		t.Fatalf("err = %v, want ErrStuck", err)
	}

	s := stateFor(t, res, badCP)
	if !s.Stuck() {
		t.Fatal("an invalid policy is stuck on the first refusal, with no window")
	}
	if s.Policy.Retryable() || s.Policy.Unexplained() {
		t.Errorf("INVALID_PARAMETER must be neither retryable nor unexplained: %+v",
			s.Policy)
	}
	if s.Attempts != 1 || cli.callsFor[badCP] != 1 {
		t.Errorf("attempts = %d, calls = %d, want 1 and 1: the bounds this "+
			"refusal measures against were negotiated when the channel opened "+
			"and nothing later moves them", s.Attempts, cli.callsFor[badCP])
	}
	if !s.RefusedSince.IsZero() || s.RetriesExhausted {
		t.Errorf("a terminal refusal was put on the retry clock: %+v", s)
	}
	if !stateFor(t, res, goodCP).Settled() {
		t.Errorf("the other channel did not settle:\n%s", res.Report())
	}

	// And the screen says why it will not change, in terms of the mechanism that
	// actually produces it. Both emitting sites are updateEdge failing against
	// the channel's negotiated LocalChanCfg bounds; the figures LND checks ahead
	// of any channel never reach failed_updates at all.
	rep := res.Report()
	// Unwrapped, because prose.Para breaks the sentence at the pane width and a
	// literal would then depend on where it happened to break.
	flat := strings.Join(strings.Fields(rep), " ")
	if !strings.Contains(flat, "negotiated when it opened") {
		t.Errorf("the report does not say what the bounds are:\n%s", rep)
	}
	if strings.Contains(flat, "before it looks at the channel") {
		t.Errorf("the report still has the mechanism inverted:\n%s", rep)
	}
}

// The end-of-run report names every terminally failed member, not just the
// first — and so does the error.
//
// Two members fail for opposite reasons and one is fine. Both failures have to
// be named, with the channel point each one needs to be fixed by hand, because
// the operator's next move is one updatechanpolicy per stuck channel.
func TestTheReportAndTheErrorNameEveryStuckMember(t *testing.T) {
	silent := memberAt(0, peerSilent)
	invalid := memberAt(1, peerInvalid)
	fine := memberAt(2, peerFine)

	cli := &fakeLND{
		open: map[string]bool{
			silent.Channel.String():  true,
			invalid.Channel.String(): true,
			fine.Channel.String():    true,
		},
		active: map[string]bool{fine.Channel.String(): true},
		failFor: map[string]*lnrpc.FailedUpdate{
			silent.Channel.String(): failure(silent.Channel,
				lnrpc.UpdateFailure_UPDATE_FAILURE_UNKNOWN, "could not update policies"),
			invalid.Channel.String(): failure(invalid.Channel,
				lnrpc.UpdateFailure_UPDATE_FAILURE_INVALID_PARAMETER,
				"time lock delta of 4 is too small"),
		},
	}

	res, err := Settle(settleCtx(t), cli, []Member{silent, invalid, fine},
		Options{Interval: time.Millisecond, RetryWindow: time.Nanosecond})
	if !errors.Is(err, ErrStuck) {
		t.Fatalf("err = %v, want ErrStuck", err)
	}

	for _, want := range []string{
		short(peerSilent), silent.Channel.String(),
		short(peerInvalid), invalid.Channel.String(),
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name %s:\n%v", want, err)
		}
	}
	if strings.Contains(err.Error(), short(peerFine)) {
		t.Errorf("the error names a channel that settled:\n%v", err)
	}

	rep := res.Report()
	t.Logf("\n%s", rep)
	for _, want := range []string{
		short(peerSilent), silent.Channel.String(),
		short(peerInvalid), invalid.Channel.String(),
		"catch-all",  // the silent one is not a verdict on the policy
		"CLTV delta", // the invalid one is
		"failed_updates",
	} {
		if !strings.Contains(rep, want) {
			t.Errorf("the report does not mention %q:\n%s", want, rep)
		}
	}

	// The copy that asserted what it could not know, and was false about a valid
	// policy on mainnet. It applies to INVALID_PARAMETER and to nothing else, so
	// it may not be said about a batch's stuck members in general.
	if strings.Contains(rep, "That is the policy itself, not the channel") {
		t.Error("the report still tells the operator their policy is at fault for " +
			"a refusal LND declined to explain")
	}
}

// And while the retry is still running, the report says so rather than either
// claiming success or reporting a failure.
//
// The state an operator is most likely to actually see, since the window is ten
// minutes and the interval is ten seconds. It is also the one place the new copy
// has five substitutions in it, which is where a %!s(int=1) hides.
func TestARefusalStillBeingRetriedIsReportedAsSuch(t *testing.T) {
	m := memberAt(0, peerSilent)
	cp := m.Channel.String()
	cli := &fakeLND{
		open:   map[string]bool{cp: true},
		active: map[string]bool{cp: false},
		failFor: map[string]*lnrpc.FailedUpdate{
			cp: failure(m.Channel, lnrpc.UpdateFailure_UPDATE_FAILURE_UNKNOWN,
				"could not update policies"),
		},
	}

	res, err := Tick(settleCtx(t), cli, []Member{m}, nil, Options{})
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	s := res.States[0]
	if s.Stuck() {
		t.Fatal("stuck on the first refusal, with ten minutes of window unspent")
	}
	if s.RefusedSince.IsZero() {
		t.Fatal("the clock on the silence never started, so it can never run out")
	}
	if res.finished() {
		t.Fatal("a member still being retried is not finished")
	}

	rep := res.Report()
	t.Logf("\n%s", rep)
	for _, want := range []string{"without a reason given", "10m0s", "catch-all"} {
		if !strings.Contains(rep, want) {
			t.Errorf("the report does not mention %q:\n%s", want, rep)
		}
	}
	if strings.Contains(rep, "%!") {
		t.Errorf("a format string is wrong:\n%s", rep)
	}
	if strings.Contains(rep, "by hand") {
		t.Errorf("the operator is being sent to the node while the loop is still "+
			"working:\n%s", rep)
	}
}
