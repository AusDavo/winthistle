// Package settle is Phase 2: from the single publish to active-and-policied.
//
// The transaction is public, every channel in it is already recoverable by
// force-close, and nothing here can lose money. What it can lose is fees, time
// and — right at the end of a long stall — a channel, so the three things it
// watches are the fee-policy race, the confirmation, and LND's funding horizon.
//
// # The fee-policy race
//
// A new channel sits at LND's defaults from the moment it goes active: 1000 msat
// base and 1 ppm (chainreg.DefaultBitcoinBaseFeeMSat and DefaultBitcoinFeeRate),
// with an 80-block CLTV delta. On a large channel that is close to free routing
// for whoever notices first. The policy was chosen per peer back in Phase 0, so
// closing the race needs no human — only a loop that keeps trying.
//
// # Why it polls unconditionally
//
// docs/design.html leaves open whether UpdateChannelPolicy accepts a channel
// point that is pending but not yet active, and answers it by not asking: poll,
// and the question stops mattering. Reading v0.21.2-beta, the answer turns out to
// be worse than either option the question offered, and the design's instinct was
// right for a reason it did not know.
//
// UpdateChannelPolicy on a pending channel does not fail. localchans.Manager
// .UpdatePolicy walks the graph's outgoing edges, does not find one, then calls
// FetchChannel, sees IsPending, and appends a FailedUpdate with reason
// UPDATE_FAILURE_PENDING and the text "not yet confirmed". The RPC then returns
// **nil error** and a PolicyUpdateResponse carrying that item. A caller that
// checked only err would record a policy it had not applied, and would go on
// routing at 1 ppm believing otherwise.
//
// So this package treats a non-empty FailedUpdates as a failure, always, and
// keeps polling. Applied is only ever set when LND came back with no failures at
// all.
//
// # One member's refusal is not the batch's
//
// Members share a funding transaction and nothing else. So a member whose policy
// cannot be applied is recorded and the loop carries on for everyone else, and
// Settle returns one ErrStuck at the end naming all of them. It used to return
// on the first, which the first live mainnet batch punished exactly as it
// deserved: one channel of five confirmed early, was refused with LND's
// catch-all while its peer was briefly offline, and took the watch on four
// channels that had not yet opened down with it. Each of those went live at
// LND's defaults with nothing left running to notice.
//
// Which refusals are worth waiting through is the other half of that, and the
// answer is not readable off the enum. INVALID_PARAMETER is a refusal against
// bounds this channel negotiated when it opened, so it is terminal at once;
// PENDING and NOT_FOUND name what they are
// waiting for; and everything else — UNKNOWN, INTERNAL_ERR, whatever a later
// version adds — is LND declining to say, which is retried for RetryWindow and
// then reported. See PolicyOutcome.Retryable, Terminal and Unexplained.
//
// # minimum_depth is readable, in one window
//
// Phase 0 in docs/design.html says to check each peer's minimum_depth and show
// "usable after k confirmations" up front, and step 9 observes it. As the
// initiator, both are available — which is not what this package said for its
// first several revisions, and the correction is issue #47.
//
// The peer states min_depth in accept_channel. funderProcessAcceptChannel takes
// it, floors a zero at 1 for anything that is not zero-conf, and stores it as
// OpenChannel.NumConfsRequired (funding/manager.go:2129-2142). PendingChannels
// then reports that figure straight back: confirmations_until_active
// (lightning.proto:2812) is filled from calcRemainingConfs, whose entire
// unconfirmed case is "return uint32(pendingChan.NumConfsRequired)"
// (rpcserver.go:4015-4019).
//
// The window is the whole of the constraint. calcRemainingConfs returns the
// peer's figure only while OpenChannel.ConfirmationHeight is zero; once the
// funding transaction confirms, the same field becomes a countdown to the
// target height and shrinks every block. So the reading is taken once, while
// the channel is pending and unconfirmed, and never revised — a reading taken
// afterwards would be smaller than the peer's number and would look like a peer
// that had asked for less. State.PeerDepth carries it, and confirmation_height
// (field 8 on the same message) is what says which of the two meanings the
// field currently has.
//
// Three figures, then, in descending order of what they establish:
//
//   - The peer's own. Exact, and available from the moment the channel is
//     pending — which is before the transaction is even published, so it is
//     early enough to plan with. Its one hedge is the floor: a peer that sent 0
//     is stored as 1, so this is what the channel will wait for rather than what
//     the peer wrote on the wire.
//   - The observed depth. When a channel first appears in ListChannels, the
//     number of confirmations it had at that moment is an upper bound on the
//     same number — the loop polls, so the channel may have been open for part
//     of a block interval before it was seen. It needs a source of confirmation
//     counts, which no production run has, so it is the harness's cross-check on
//     the reading above rather than a second source for it.
//   - LND's own default policy, which a peer running stock LND will be using:
//     between 1 and 6, scaled linearly by capacity against MaxFundingAmount, and
//     6 for anything wumbo. ExpectedDepth computes it, it is a prediction, and
//     it is what stands in for a channel whose funding transaction had already
//     confirmed before this loop first asked.
//
// # The horizon past the end
//
// A funding transaction that never confirms is not a stalemate. After
// lncfg.DefaultMaxWaitNumBlocksFundingConf = 2016 blocks from its broadcast
// height, the *responder* gives up: waitForFundingWithTimeout only starts
// waitForTimeout when !ch.IsInitiator, so the peer closes its side as
// FundingCanceled while our node — the initiator — waits forever. If the
// transaction then confirms, we hold an open channel the peer has forgotten.
//
// LND publishes the countdown: PendingChannels reports funding_expiry_blocks,
// computed as 2016 + broadcastHeight - currentHeight, and its own proto says a
// negative value means the responder has very likely cancelled. That is the
// number this package watches, and CPFP out of the change output is the only
// remedy — never a replacement, which is I-4.
//
// All source citations are against lnd v0.21.2-beta.
package settle

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/AusDavo/winthistle/internal/lnd"
	"github.com/AusDavo/winthistle/internal/policy"
	"github.com/lightningnetwork/lnd/lnrpc"
	"google.golang.org/grpc"
)

// LND's own figures, for predicting what a stock peer will do and for saying
// what a channel is worth before the policy lands.
const (
	// ForgetHorizonBlocks is lncfg.DefaultMaxWaitNumBlocksFundingConf. Not
	// adjustable in a release build: lncfg/dev.go returns the constant, and the
	// flag is read only by lncfg/dev_integration.go, which is built under the
	// `integration` tag. Not the `dev` tag, which LND also has and which drives
	// something else.
	ForgetHorizonBlocks = 2016

	// MinDepth and MaxDepth bound LND's default NumRequiredConfs, and
	// MaxFundingAmount is what it scales against — funding.MaxBtcFundingAmount,
	// 2^24 - 1. They are lnwallet.minRequiredConfs and lnwallet.maxRequiredConfs,
	// both unexported, in lnwallet/confscale.go.
	MinDepth         = 1
	MaxDepth         = 6
	MaxFundingAmount = int64(1<<24) - 1

	// LND's default forwarding policy, which is what a new channel routes at
	// until this package replaces it, and the bounds UpdateChannelPolicy will
	// accept. Re-exported from internal/policy, which owns the type: Phase 0
	// chooses the policy, the plan document shows it, and this package applies
	// it, so the value lives where all three can see it.
	DefaultBaseFeeMsat   = policy.DefaultBaseFeeMsat
	DefaultFeeRatePPM    = policy.DefaultFeeRatePPM
	DefaultTimeLockDelta = policy.DefaultTimeLockDelta

	MinTimeLockDelta = policy.MinTimeLockDelta
	MaxTimeLockDelta = policy.MaxTimeLockDelta
)

// Client is the slice of LND settlement uses.
//
// Three methods, and only one of them changes anything. Nothing here can open a
// stream, move a coin or broadcast: by Phase 2 the transaction is already out,
// and the remaining work is watching and one policy call per channel.
type Client interface {
	PendingChannels(ctx context.Context, in *lnrpc.PendingChannelsRequest,
		opts ...grpc.CallOption) (*lnrpc.PendingChannelsResponse, error)

	ListChannels(ctx context.Context, in *lnrpc.ListChannelsRequest,
		opts ...grpc.CallOption) (*lnrpc.ListChannelsResponse, error)

	UpdateChannelPolicy(ctx context.Context, in *lnrpc.PolicyUpdateRequest,
		opts ...grpc.CallOption) (*lnrpc.PolicyUpdateResponse, error)
}

// Chain turns "not open yet" into a number of confirmations.
//
// Nothing in the application fills it, and nothing may. Item 5 removed Bitcoin
// Core from this build entirely, and the no-third-party rule forbids the obvious
// substitute for the same reason it forbids a fee API: a block explorer asked
// how deep this transaction is has been handed the transaction. So on every
// production path this is nil, no depth is reported, and depthNote says so as a
// design fact rather than as a fault.
//
// What fills it is the harness. internal/regtestenv keeps a Core — the same way
// it keeps the simulated cold wallet, as the stand-in item 5 left inside the
// test environment rather than in the application — and
// TestTheSettlementPassPoliciesAChannelAndLearnsItsDepth uses it to watch a live
// channel open one block at a time. What that buys is State.ObservedDepth, and
// since issue #47 its job has changed: it is no longer the only reading of a
// peer's minimum_depth an initiator can get, it is the harness's independent
// cross-check on the reading LND hands over directly. Two numbers from two
// sources agreeing on a running node is better evidence than either alone, which
// is why the field stays.
//
// The doc comment this replaces justified the field by "assisted mode has no
// Core", and assisted mode dissolved with I-2. Stale twice over, which is what
// issue #21 was really about: a seam nobody fills grows copy nobody checks.
//
// A production depth reading does not come through here, and does not need to.
// The peer's own figure is on PendingChannels, which this package already calls
// — see the package comment. What this seam would report is something else: how
// deep the funding transaction is right now, which is a count of blocks and
// would be a new call site, a registry entry and a decision, not a field
// somebody fills in.
type Chain interface {
	Confirmations(ctx context.Context, txid string) (confs int64, present bool, err error)
}

// Policy is one channel's intended forwarding policy, chosen in Phase 0.
//
// An alias rather than a definition: internal/policy owns the type so that the
// plan document can show the policy beside the amount it belongs to. This
// package is where it is applied, and where LND's behaviour around applying it
// is documented — see the package comment.
type Policy = policy.Policy

// request renders a policy as LND's own.
func request(p Policy, cp lnd.ChannelPoint) *lnrpc.PolicyUpdateRequest {
	req := &lnrpc.PolicyUpdateRequest{
		Scope:         &lnrpc.PolicyUpdateRequest_ChanPoint{ChanPoint: cp.RPC()},
		BaseFeeMsat:   p.BaseFeeMsat,
		TimeLockDelta: p.TimeLockDelta,
		MaxHtlcMsat:   p.MaxHTLCMsat,
		// FeeRatePpm rather than FeeRate: they are mutually exclusive — LND
		// refuses a request carrying both — and the ppm form is the protocol's
		// own fixed point. FeeRate is a float that LND multiplies by a million
		// and rounds, which is a rounding step for nothing.
		FeeRatePpm: p.FeeRatePPM,
	}
	if p.MinHTLCMsat != nil {
		req.MinHtlcMsat = uint64(*p.MinHTLCMsat)
		req.MinHtlcMsatSpecified = true
	}
	if p.InboundBaseFeeMsat != 0 || p.InboundFeeRatePPM != 0 {
		req.InboundFee = &lnrpc.InboundFee{
			BaseFeeMsat: p.InboundBaseFeeMsat,
			FeeRatePpm:  p.InboundFeeRatePPM,
		}
	}
	return req
}

// Member is one channel of the published batch.
type Member struct {
	Channel   lnd.ChannelPoint
	Peer      string
	AmountSat int64
	Private   bool
	Policy    Policy
}

// ExpectedDepth predicts how many confirmations a peer running stock LND will
// want before it considers this channel open.
//
// A prediction, and the report says so. It reproduces lnwallet.ScaleNumConfs,
// which the NumRequiredConfs closure in server.go:1608 calls through
// lnwallet.FundingConfsForAmounts: 6 for anything above MaxFundingAmount, and
// otherwise 6 * stake / MaxFundingAmount clamped into [MinDepth, MaxDepth]. A
// peer that set --bitcoin.defaultchanconfs, or that runs a channel acceptor, or
// that is not LND at all, is bound by none of it.
func ExpectedDepth(amountSat int64) int64 {
	if amountSat > MaxFundingAmount {
		return MaxDepth
	}
	// The closure works in millisatoshis on both sides of the ratio, so the
	// factors cancel and this is the same arithmetic in satoshis.
	conf := int64(MaxDepth) * amountSat / MaxFundingAmount
	if conf < MinDepth {
		return MinDepth
	}
	if conf > MaxDepth {
		return MaxDepth
	}
	return conf
}

// PolicyOutcome is what one UpdateChannelPolicy call actually did.
//
// Applied is the only success. LND's own failure channel for this call is a list
// inside a successful response, not an error, so a caller that read err alone
// would see every one of these as a success.
type PolicyOutcome struct {
	Applied bool

	// Refused is whether LND named this channel in failed_updates.
	//
	// Separate from "Reason is non-zero", because zero is
	// UPDATE_FAILURE_UNKNOWN — a reason LND can genuinely return. Without this
	// field an UNKNOWN refusal would be indistinguishable from a call that has
	// not been made yet, and the settlement loop would retry it until the
	// operator gave up.
	Refused bool

	// Reason is LND's enum when it refused, and Detail its own text.
	Reason lnrpc.UpdateFailure
	Detail string
}

// Retryable reports whether waiting could change the answer.
//
// Everything except INVALID_PARAMETER. PENDING and NOT_FOUND name what they are
// waiting for — a confirmation and a graph edge — and waiting is the whole plan
// for both.
//
// UNKNOWN used to be excluded, on the reading that LND declining to say leaves
// nothing identifiable to wait for. The node falsified that on mainnet on
// 2026-08-26: a channel that had just opened, with its peer briefly offline, was
// refused with UNKNOWN, and the identical update applied cleanly by hand a few
// minutes later. **UNKNOWN is LND's catch-all, not a verdict on the policy.**
// The old reading was right about one thing — retrying it forever is not a plan
// either — and that half now lives in Unexplained and RetryWindow, which bound
// the retry instead of refusing to make it.
func (o PolicyOutcome) Retryable() bool { return o.Refused && !o.Terminal() }

// Terminal reports whether the refusal will arrive again on the hundredth
// attempt exactly as it did on the first.
//
// One reason qualifies, and only one — but not for the reason this comment gave
// for a year. INVALID_PARAMETER is not LND checking the figures ahead of the
// channel. Both sites that emit it are updateEdge failing
// (routing/localchans/manager.go:122 and :273), and updateEdge fetches the
// channel and measures the HTLC bounds against its *negotiated*
// LocalChanCfg — MinHTLC and ChannelStateBounds.MaxPendingAmount (:448-467,
// :478). It is entirely about the channel.
//
// The conclusion survives on the better argument: negotiated bounds are fixed
// when the channel opens and nothing later changes them, so a min or max HTLC
// this channel will not carry is one it will still not carry on the hundredth
// attempt.
//
// The figures LND does check ahead of any channel — the CLTV delta and the
// inbound fees — produce no failed_updates entry at all. They fail the whole
// UpdateChannelPolicy call as a gRPC error, which is why policy.Validate refuses
// them before a batch is armed rather than waiting to read them here.
//
// Not settled: updateEdge's first statement is FetchChannel, and a channel that
// went away between the graph lookup and that fetch would land here as
// INVALID_PARAMETER for a transient reason. Whether that ordering is reachable
// has not been established, and this build does not claim either way.
func (o PolicyOutcome) Terminal() bool {
	return o.Refused &&
		o.Reason == lnrpc.UpdateFailure_UPDATE_FAILURE_INVALID_PARAMETER
}

// Unexplained reports whether LND refused without naming something to wait for.
//
// UNKNOWN, INTERNAL_ERR, and any reason a later LND grows that this build does
// not recognise. Retrying these is right — the mainnet refusal above is one of
// them — and retrying them forever is not, because nothing in this class will
// ever announce that it has changed its mind. So the loop bounds its own
// patience: RetryWindow is the bound, Tick applies it, and
// State.RetriesExhausted carries the verdict.
//
// An unrecognised reason is deliberately in this class rather than in Terminal.
// Treating a value we cannot interpret as a statement about the policy is
// exactly the mistake UNKNOWN was, and a bounded retry costs minutes at worst.
func (o PolicyOutcome) Unexplained() bool {
	if !o.Refused || o.Terminal() {
		return false
	}
	switch o.Reason {
	case lnrpc.UpdateFailure_UPDATE_FAILURE_PENDING,
		lnrpc.UpdateFailure_UPDATE_FAILURE_NOT_FOUND:
		return false
	default:
		return true
	}
}

func (o PolicyOutcome) String() string {
	switch {
	case o.Applied:
		return "applied"
	case !o.Refused:
		return "not attempted"
	}
	name := strings.ToLower(strings.TrimPrefix(o.Reason.String(), "UPDATE_FAILURE_"))
	if o.Detail == "" {
		return name
	}
	return fmt.Sprintf("%s (%s)", name, o.Detail)
}

// ApplyPolicy sets one channel's forwarding policy and reports what LND did.
//
// The whole of this function that is not obvious is the FailedUpdates loop, and
// it is the reason the function exists rather than the call being made inline.
// See the package comment: a pending channel is refused inside a successful
// response.
func ApplyPolicy(ctx context.Context, cli Client, m Member) (PolicyOutcome, error) {
	// Our own refusal, reported under LND's enum for it. Validate covers exactly
	// the figures LND checks in the RPC handler ahead of any channel — the CLTV
	// delta and the inbound fees — where a refusal is a gRPC error for the whole
	// call and never a failed_updates entry, so there is no LND reason code to
	// carry it. INVALID_PARAMETER is the right classification anyway: it is
	// terminal, which is what the caller needs, and repeating the call cannot
	// change it.
	if err := m.Policy.Validate(); err != nil {
		return PolicyOutcome{
			Refused: true,
			Reason:  lnrpc.UpdateFailure_UPDATE_FAILURE_INVALID_PARAMETER,
			Detail:  err.Error(),
		}, nil
	}

	resp, err := cli.UpdateChannelPolicy(ctx, request(m.Policy, m.Channel))
	if err != nil {
		return PolicyOutcome{}, fmt.Errorf("setting the policy on the channel to %s "+
			"(%s): %w", short(m.Peer), m.Channel, err)
	}

	want := m.Channel.String()
	for _, f := range resp.GetFailedUpdates() {
		// LND renders the outpoint itself; compare on the rendered form rather
		// than reassembling it, and treat an unattributed failure as ours,
		// because the request named exactly one channel.
		if op := f.GetOutpoint(); op != nil && outpointString(op) != want {
			continue
		}
		return PolicyOutcome{
			Refused: true, Reason: f.GetReason(), Detail: f.GetUpdateError(),
		}, nil
	}
	return PolicyOutcome{Applied: true}, nil
}

// outpointString renders lnrpc.OutPoint the way LND prints a channel point.
func outpointString(op *lnrpc.OutPoint) string {
	if s := op.GetTxidStr(); s != "" {
		return fmt.Sprintf("%s:%d", s, op.GetOutputIndex())
	}
	return ""
}

// State is where one member of the batch has got to.
type State struct {
	Member Member

	// Open is whether LND lists the channel as open at all. Active is
	// ListChannels' own active field, which LND computes as peerOnline &&
	// link.EligibleToForward() — so what it establishes is narrower than "the
	// peer is up": this node's own link is not eligible to forward, and a peer
	// that is offline is only one of the ways that happens. The others are our
	// own link still coming up after a restart, and a link that has been taken
	// out of service.
	//
	// Only Open gates the policy. Active is reported because a channel whose
	// link cannot forward routes nothing, and that is worth seeing rather than
	// debugging.
	Open   bool
	Active bool

	// Confs is how deep the funding transaction is, according to Options.Chain.
	//
	// -1 on every production run, and that is the rule rather than the exception:
	// the application sets no Chain and cannot. Only the harness ever sees a
	// number here. Reports must read -1 as "this build does not count blocks",
	// never as "the count is unavailable just now".
	Confs int64

	// The three figures for the peer's minimum_depth, in the order the package
	// comment ranks them.
	//
	// PeerDepth is the peer's own, read once off PendingChannels'
	// confirmations_until_active while the funding transaction was still
	// unconfirmed. Zero means it was never in that window when this loop
	// looked — a channel whose transaction had already confirmed before the
	// first pass offers no reading, and there is no second chance at it.
	//
	// ExpectedDepth is the prediction from LND's default policy, and it is set
	// on every pass because it is arithmetic on the amount.
	//
	// ObservedDepth is the depth the channel was at the moment it first appeared
	// open, which is an upper bound on the same number the loop's own polling
	// makes loose. Since it is read off Confs it stays zero on every run the
	// harness is not driving.
	PeerDepth     int64
	ExpectedDepth int64
	ObservedDepth int64

	// ExpiryBlocks is PendingChannels' funding_expiry_blocks: how many blocks
	// remain before the responder gives up. Meaningful only while pending, and
	// negative means the peer has very likely cancelled already.
	ExpiryBlocks int32
	StillPending bool

	Policy   PolicyOutcome
	Attempts int

	// RefusedSince is when the current unexplained refusal was first seen, and
	// RetriesExhausted whether RetryWindow has run out since. Both are set by
	// Tick, which is the only thing here holding a clock, so that Stuck stays a
	// question about a Result rather than about the moment it is asked.
	//
	// Zero unless the outcome is Unexplained. A PENDING refusal is not on a
	// clock of this kind — it is on the funding horizon's, which is measured in
	// blocks and reported separately — and a terminal one needs no clock at all.
	RefusedSince     time.Time
	RetriesExhausted bool

	// RefusedFor is that silence's length as of this pass, so the report can
	// name what actually happened rather than the ceiling it was measured
	// against. A member is found exhausted one poll past the window, never
	// exactly on it.
	RefusedFor time.Duration
}

// Settled reports whether there is nothing left to do for this channel.
func (s State) Settled() bool { return s.Open && s.Policy.Applied }

// Stuck reports whether this channel needs a human: either LND refused the
// policy itself, or it refused without saying why for longer than RetryWindow.
//
// A stuck member does not stop the settlement. It is recorded, the loop carries
// on for every other member, and Settle names all of them in one ErrStuck at the
// end. That is the worse half of issue #6: on the first live batch a single
// transient refusal abandoned the watch on four channels that had not opened
// yet, so each went live at LND's defaults with nothing left running to notice.
func (s State) Stuck() bool {
	return s.Policy.Terminal() || (s.Policy.Unexplained() && s.RetriesExhausted)
}

// Result is the whole settlement, member by member.
type Result struct {
	States []State

	// Height is the chain height at the last poll, when Core was available.
	Height int64

	// Elapsed is how long the settlement has been running.
	Elapsed time.Duration

	// RetryWindow is the patience this pass ran with, carried so that the report
	// names the figure it actually used and not the default. Zero in a Result
	// built by hand, and retryWindow falls back to the constant.
	RetryWindow time.Duration
}

func (r *Result) retryWindow() time.Duration {
	if r.RetryWindow <= 0 {
		return RetryWindow
	}
	return r.RetryWindow
}

// Done reports whether every member is open and policied.
func (r *Result) Done() bool {
	for _, s := range r.States {
		if !s.Settled() {
			return false
		}
	}
	return len(r.States) > 0
}

// Stalled reports the members still waiting on a confirmation.
func (r *Result) Stalled() []State {
	var out []State
	for _, s := range r.States {
		if !s.Open {
			out = append(out, s)
		}
	}
	return out
}

// Stuck lists the members whose policy will not be applied by any more polling.
//
// In Result order, which is by channel point: the report and the error both name
// every one of them, and naming them in a stable order is what makes two runs
// comparable.
func (r *Result) Stuck() []State {
	var out []State
	for _, s := range r.States {
		if s.Stuck() {
			out = append(out, s)
		}
	}
	return out
}

// finished reports whether another pass could achieve anything: every member is
// either open and policied, or stuck.
//
// Not Done, and not "any member is stuck". Done is the success, and stopping at
// the first stuck member is the defect — a batch was only ever as settlable as
// its unluckiest channel.
func (r *Result) finished() bool {
	for _, s := range r.States {
		if !s.Settled() && !s.Stuck() {
			return false
		}
	}
	return len(r.States) > 0
}

// stuckErr is the one error that names every member the loop could not police,
// or nil when there is none.
func (r *Result) stuckErr() error {
	stuck := r.Stuck()
	if len(stuck) == 0 {
		return nil
	}
	parts := make([]string, 0, len(stuck))
	for _, s := range stuck {
		parts = append(parts, fmt.Sprintf("%s (%s) — %s",
			short(s.Member.Peer), s.Member.Channel, s.Policy))
	}
	return fmt.Errorf("%w: %d of %d — %s", ErrStuck, len(stuck), len(r.States),
		strings.Join(parts, "; "))
}

// NearestExpiry is the smallest funding_expiry_blocks across the members still
// pending, and whether there was one to report.
func (r *Result) NearestExpiry() (int32, bool) {
	var (
		nearest int32
		found   bool
	)
	for _, s := range r.States {
		if !s.StillPending {
			continue
		}
		if !found || s.ExpiryBlocks < nearest {
			nearest, found = s.ExpiryBlocks, true
		}
	}
	return nearest, found
}

// Options tunes the settlement loop.
type Options struct {
	// Interval is how often to poll. Zero means DefaultInterval.
	Interval time.Duration

	// Chain is a source of confirmation counts, and the application never sets
	// it — see the Chain type. Nil on every production path; the harness fills
	// it with regtest's Core.
	Chain Chain

	// FundingTxID is the batch's transaction, and it is only ever used to ask
	// Chain how deep it is. It is the same txid every member's channel point
	// carries, and run.settlePhase passes it so that a Chain wired in for a test
	// has something to ask about.
	FundingTxID string

	// RetryWindow overrides the default patience with an unexplained refusal.
	// Zero means RetryWindow, and production passes zero.
	RetryWindow time.Duration
}

// DefaultInterval is how often the settlement loop asks.
//
// Ten seconds. Nothing here is urgent — the channels are already recoverable and
// the transaction is already public — and the events being waited for are a
// block and a graph update. A tighter loop would only ask LND the same question
// more often.
const DefaultInterval = 10 * time.Second

func (o Options) interval() time.Duration {
	if o.Interval <= 0 {
		return DefaultInterval
	}
	return o.Interval
}

// RetryWindow is how long an unexplained refusal is retried before the member is
// called stuck.
//
// Ten minutes, measured from the first refusal of that kind and not from the
// start of the run: a channel that sat pending for two days and then hit UNKNOWN
// gets the whole window. Long enough for a peer to reconnect and for the graph
// to catch up — the mainnet refusal that prompted this was a peer offline for a
// few minutes — and a third of run.DefaultSettleFor, so a member that is never
// going to take its policy is named inside the default window rather than at the
// end of it.
//
// In time rather than in attempts, because a count of attempts only means
// minutes at one particular Interval, and Interval belongs to the caller. A
// caller that drives Tick four times as often should not get a quarter of the
// patience.
const RetryWindow = 10 * time.Minute

func (o Options) retryWindow() time.Duration {
	if o.RetryWindow <= 0 {
		return RetryWindow
	}
	return o.RetryWindow
}

// Tick runs one pass: read LND's view, apply the policy to anything newly open,
// and report where every member stands.
//
// Separated from Settle so that one pass can be tested, and so that a UI can
// drive the loop itself at whatever rate it refreshes at.
//
// prev may be nil on the first pass. It carries forward the four things a
// single pass cannot know: how many attempts have been made, how long an
// unexplained refusal has been going on, the peer's own minimum_depth as it was
// read while the channel was pending and unconfirmed, and the depth at which the
// channel was first seen open. The last two are each readable in one window
// only, and a later pass would report a different number for the same field.
func Tick(ctx context.Context, cli Client, members []Member, prev *Result,
	opts Options) (*Result, error) {

	now := time.Now()

	open, active, err := openChannels(ctx, cli)
	if err != nil {
		return nil, err
	}
	pending, err := pendingChannels(ctx, cli)
	if err != nil {
		return nil, err
	}

	confs := int64(-1)
	if opts.Chain != nil && opts.FundingTxID != "" {
		n, present, err := opts.Chain.Confirmations(ctx, opts.FundingTxID)
		if err != nil {
			return nil, err
		}
		if present {
			confs = n
		}
	}

	before := map[string]State{}
	if prev != nil {
		for _, s := range prev.States {
			before[s.Member.Channel.String()] = s
		}
	}

	out := &Result{RetryWindow: opts.retryWindow()}
	for _, m := range members {
		key := m.Channel.String()
		s := State{
			Member:        m,
			Confs:         confs,
			ExpectedDepth: ExpectedDepth(m.AmountSat),
		}
		if was, ok := before[key]; ok {
			s.Attempts = was.Attempts
			s.PeerDepth = was.PeerDepth
			s.ObservedDepth = was.ObservedDepth
			s.Policy = was.Policy
			s.RefusedSince, s.RetriesExhausted = was.RefusedSince, was.RetriesExhausted
			s.RefusedFor = was.RefusedFor
		}

		s.Open = open[key]
		s.Active = active[key]
		if p, ok := pending[key]; ok {
			s.StillPending, s.ExpiryBlocks = true, p.expiryBlocks

			// The peer's own minimum_depth, taken once and only inside the one
			// window where confirmations_until_active means it: before the
			// funding transaction confirms, calcRemainingConfs returns
			// NumConfsRequired verbatim, and afterwards the same field is a
			// countdown that shrinks every block. See the package comment.
			if s.PeerDepth == 0 && !p.confirmed && p.depth > 0 {
				s.PeerDepth = int64(p.depth)
			}
		}

		// The moment a channel first appears open, the depth it is at is an
		// upper bound on the peer's minimum_depth, read from above rather than
		// taken from LND's record of what the peer asked for. Recorded once and
		// never revised: on a later pass the transaction is deeper and the
		// reading is worthless.
		if s.Open && s.ObservedDepth == 0 && confs > 0 {
			s.ObservedDepth = confs
		}

		// Nothing another call can do: it landed, or it is stuck. Stuck rather
		// than "not retryable", because the retry on an unexplained refusal is
		// bounded by time and the bound is checked below, so this is where a
		// member that has run out of patience stops costing an RPC a pass.
		if s.Policy.Applied || s.Stuck() {
			out.States = append(out.States, s)
			continue
		}

		// Unconditionally, whatever LND says about the channel's state. That is
		// the design's deliberate defusing of the pending-but-inactive question,
		// and it costs one RPC per pass per unfinished channel.
		outcome, err := ApplyPolicy(ctx, cli, m)
		if err != nil {
			return nil, err
		}
		s.Attempts++
		s.Policy = outcome

		// The clock on an unexplained refusal, which is the only kind that is
		// retried on a clock at all. It starts at the first one, survives a
		// change of reason inside the class — UNKNOWN and INTERNAL_ERR are the
		// same silence — and is cleared by anything that names what it is
		// waiting for, so a channel refused with UNKNOWN and then with PENDING
		// starts again from zero if the silence comes back.
		switch {
		case !outcome.Unexplained():
			s.RefusedSince, s.RetriesExhausted = time.Time{}, false
			s.RefusedFor = 0
		case s.RefusedSince.IsZero():
			s.RefusedSince = now
		default:
			s.RefusedFor = now.Sub(s.RefusedSince)
			// Only after an attempt, never instead of one: a window of zero
			// still buys the member the retry it is entitled to.
			s.RetriesExhausted = s.RefusedFor >= opts.retryWindow()
		}

		out.States = append(out.States, s)
	}

	sort.SliceStable(out.States, func(i, j int) bool {
		return out.States[i].Member.Channel.String() < out.States[j].Member.Channel.String()
	})
	return out, nil
}

// ErrStuck means the loop finished with at least one member still on LND's
// default policy.
//
// It is returned once, at the end, wrapping a list of every member it applies to
// — not on the first one, and not instead of settling the rest. The text says
// "not applied to every channel" rather than "the settlement stopped", because
// under this loop it did not stop.
var ErrStuck = errors.New("the policy was not applied to every channel")

// Settle runs the loop until every member is either open and policied or stuck,
// or the context ends.
//
// A stuck member does not end it. The loop keeps going for everyone else and the
// stuck ones come back in one ErrStuck at the end, naming all of them: they have
// nothing to do with each other, and on the first live batch four channels that
// had not yet opened lost their watcher to a fifth channel's transient refusal.
//
// It returns the last Result on every path, including the failing ones: a
// partially settled batch is exactly the thing the operator has to be shown, and
// an error with no state attached would send them to the node to find out what
// happened.
func Settle(ctx context.Context, cli Client, members []Member, opts Options) (*Result, error) {
	if len(members) == 0 {
		return nil, fmt.Errorf("no channels to settle")
	}
	for _, m := range members {
		if m.Channel.TxID == "" {
			return nil, fmt.Errorf("the channel to %s has no funding outpoint, so "+
				"there is nothing to watch or to police", short(m.Peer))
		}
	}

	started := time.Now()
	ticker := time.NewTicker(opts.interval())
	defer ticker.Stop()

	var last *Result
	for {
		res, err := Tick(ctx, cli, members, last, opts)
		if err != nil {
			return last, err
		}
		res.Elapsed = time.Since(started)
		last = res

		// Every member is settled or stuck, so another pass would ask LND the
		// same questions and get the same answers. stuckErr is nil when the
		// batch simply finished, which is the ordinary success.
		if res.finished() {
			return res, res.stuckErr()
		}

		select {
		case <-ctx.Done():
			// Not an error in itself. A settlement that runs out of context has
			// left the channels exactly where they were: open or pending, and
			// policied or not, all of it visible in the Result and all of it
			// resumable by calling this again.
			return last, ctx.Err()
		case <-ticker.C:
		}
	}
}

// openChannels reads which of our channels LND considers open, and which of
// those have their peer online.
func openChannels(ctx context.Context, cli Client) (open, active map[string]bool, err error) {
	resp, err := cli.ListChannels(ctx, &lnrpc.ListChannelsRequest{})
	if err != nil {
		return nil, nil, fmt.Errorf("listing this node's open channels: %w", err)
	}
	open = map[string]bool{}
	active = map[string]bool{}
	for _, ch := range resp.GetChannels() {
		open[ch.GetChannelPoint()] = true
		active[ch.GetChannelPoint()] = ch.GetActive()
	}
	return open, active, nil
}

// pendingOpen is what one PendingChannels entry is worth to this package.
type pendingOpen struct {
	// expiryBlocks is funding_expiry_blocks: how far this node's own count of
	// the funding horizon has left to run.
	expiryBlocks int32

	// depth is confirmations_until_active, which is the peer's minimum_depth
	// only while confirmed is false. See the package comment for why the same
	// field means two different things either side of the first confirmation.
	depth uint32

	// confirmed is whether LND has recorded a confirmation height for the
	// funding transaction, which is the condition calcRemainingConfs branches
	// on. Zero-conf channels never appear in this list at all, so a pending
	// channel that is unconfirmed always has a real figure to give.
	confirmed bool
}

// pendingChannels reads the funding horizon and the peer's stated depth for
// every pending open.
func pendingChannels(ctx context.Context, cli Client) (map[string]pendingOpen, error) {
	resp, err := cli.PendingChannels(ctx, &lnrpc.PendingChannelsRequest{})
	if err != nil {
		return nil, fmt.Errorf("listing this node's pending channels: %w", err)
	}
	out := map[string]pendingOpen{}
	for _, p := range resp.GetPendingOpenChannels() {
		out[p.GetChannel().GetChannelPoint()] = pendingOpen{
			expiryBlocks: p.GetFundingExpiryBlocks(),
			depth:        p.GetConfirmationsUntilActive(),
			confirmed:    p.GetConfirmationHeight() != 0,
		}
	}
	return out, nil
}

func short(pubkey string) string {
	if len(pubkey) <= 16 {
		return pubkey
	}
	return pubkey[:16] + "…"
}
