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
// # minimum_depth is not readable, and the design assumes it is
//
// Phase 0 in docs/design.html says to check each peer's minimum_depth and show
// "usable after k confirmations" up front. As the *initiator* there is no way to
// do that. The peer states min_depth in accept_channel; LND stores it as
// OpenChannel.NumConfsRequired and exposes it over no RPC — min_accept_depth
// appears in lnrpc only on ChannelAcceptResponse, which is the responder's side
// of somebody else's channel.
//
// Three things stand in, in descending order of certainty:
//
//   - The observed depth. When a channel first appears in ListChannels, the
//     number of confirmations it had at that moment is the peer's minimum_depth,
//     from above. It is the authoritative figure, it arrives too late to plan
//     with, and it is exactly the right thing to record for next time.
//   - LND's own default policy, which a peer running stock LND will be using:
//     between 3 and 6, scaled linearly by capacity against MaxFundingAmount, and
//     6 for anything wumbo. ExpectedDepth computes it, and it is a prediction.
//   - Nothing else. There is no gossip field and no third party may be asked.
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
	// adjustable in a release build: lncfg/dev.go returns the constant and only
	// a dev-tagged build reads the flag.
	ForgetHorizonBlocks = 2016

	// MinDepth and MaxDepth bound LND's default NumRequiredConfs, and
	// MaxFundingAmount is what it scales against — funding.MaxBtcFundingAmount,
	// 2^24 - 1.
	MinDepth         = 3
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

// Chain is the optional Core connection that turns "not open yet" into a number
// of confirmations.
//
// Optional because assisted mode has no Core. Without it the settlement still
// works — LND moving a channel out of pending_open_channels is the authoritative
// signal and needs nobody's help — but the operator sees "not yet" rather than
// "2 of an expected 3", and the peer's real minimum_depth cannot be learned.
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
// A prediction, and the report says so. It reproduces the NumRequiredConfs
// closure wired up in server.go: 6 for anything above MaxFundingAmount, and
// otherwise 6 * stake / MaxFundingAmount clamped into [3, 6]. A peer that set
// --bitcoin.defaultchanconfs, or that runs a channel acceptor, or that is not
// LND at all, is bound by none of it.
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
// PENDING and NOT_FOUND both will: the first becomes false when the funding
// transaction confirms, and the second when the channel's edge reaches the graph
// database. INVALID_PARAMETER will not — the policy itself is wrong, and
// retrying it every ten seconds until the operator gives up is worse than
// stopping and saying so. Nor will UNKNOWN, which is LND declining to say: there
// is nothing identifiable to wait for, so waiting is not a plan.
func (o PolicyOutcome) Retryable() bool {
	if !o.Refused {
		return false
	}
	switch o.Reason {
	case lnrpc.UpdateFailure_UPDATE_FAILURE_PENDING,
		lnrpc.UpdateFailure_UPDATE_FAILURE_NOT_FOUND,
		lnrpc.UpdateFailure_UPDATE_FAILURE_INTERNAL_ERR:
		return true
	default:
		return false
	}
}

// Settled reports whether this outcome needs nothing further.
func (o PolicyOutcome) Settled() bool { return o.Applied || (o.Refused && !o.Retryable()) }

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

	// Open is whether LND lists the channel as open at all, and Active whether
	// the peer is currently connected. Only Open gates the policy; Active is
	// reported because an open channel with an offline peer routes nothing, and
	// that is worth seeing rather than debugging.
	Open   bool
	Active bool

	// Confs is how deep the funding transaction is, from Core. -1 means Core was
	// not available or has never seen the transaction.
	Confs int64

	// ExpectedDepth is the prediction; ObservedDepth is the depth at the moment
	// the channel first appeared open, which is the peer's real minimum_depth
	// from above. Zero until that happens, and it stays zero without Core.
	ExpectedDepth int64
	ObservedDepth int64

	// ExpiryBlocks is PendingChannels' funding_expiry_blocks: how many blocks
	// remain before the responder gives up. Meaningful only while pending, and
	// negative means the peer has very likely cancelled already.
	ExpiryBlocks int32
	StillPending bool

	Policy   PolicyOutcome
	Attempts int
}

// Settled reports whether there is nothing left to do for this channel.
func (s State) Settled() bool { return s.Open && s.Policy.Applied }

// Stuck reports whether this channel needs a human: the policy was refused for a
// reason that waiting will not fix.
func (s State) Stuck() bool { return s.Policy.Refused && !s.Policy.Retryable() }

// Result is the whole settlement, member by member.
type Result struct {
	States []State

	// Height is the chain height at the last poll, when Core was available.
	Height int64

	// Elapsed is how long the settlement has been running.
	Elapsed time.Duration
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

	// Chain is Core, optionally. Without it depth is not reported and the peers'
	// real minimum_depth cannot be learned.
	Chain Chain

	// FundingTxID is the batch's transaction, needed only to ask Core how deep
	// it is. It is the same txid every member's channel point carries.
	FundingTxID string
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

// Tick runs one pass: read LND's view, apply the policy to anything newly open,
// and report where every member stands.
//
// Separated from Settle so that one pass can be tested, and so that a UI can
// drive the loop itself at whatever rate it refreshes at.
//
// prev may be nil on the first pass. It carries forward the two things a single
// pass cannot know: how many attempts have been made, and the depth at which a
// channel was first seen open, which is the only authoritative reading of the
// peer's minimum_depth there is.
func Tick(ctx context.Context, cli Client, members []Member, prev *Result,
	opts Options) (*Result, error) {

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

	out := &Result{}
	for _, m := range members {
		key := m.Channel.String()
		s := State{
			Member:        m,
			Confs:         confs,
			ExpectedDepth: ExpectedDepth(m.AmountSat),
		}
		if was, ok := before[key]; ok {
			s.Attempts = was.Attempts
			s.ObservedDepth = was.ObservedDepth
			s.Policy = was.Policy
		}

		s.Open = open[key]
		s.Active = active[key]
		if p, ok := pending[key]; ok {
			s.StillPending, s.ExpiryBlocks = true, p
		}

		// The moment a channel first appears open, the depth it is at is the
		// peer's minimum_depth from above. Recorded once and never revised: on a
		// later pass the transaction is deeper and the reading is worthless.
		if s.Open && s.ObservedDepth == 0 && confs > 0 {
			s.ObservedDepth = confs
		}

		if s.Policy.Settled() {
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
		out.States = append(out.States, s)
	}

	sort.SliceStable(out.States, func(i, j int) bool {
		return out.States[i].Member.Channel.String() < out.States[j].Member.Channel.String()
	})
	return out, nil
}

// ErrStuck means a member's policy was refused for a reason that waiting will
// not change.
var ErrStuck = errors.New("a channel's policy was refused for a reason polling cannot fix")

// Settle runs the loop until every member is open and policied, the context ends,
// or a policy is refused in a way that will not improve.
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

		if res.Done() {
			return res, nil
		}
		for _, s := range res.States {
			if s.Stuck() {
				return res, fmt.Errorf("%w: the channel to %s — %s",
					ErrStuck, short(s.Member.Peer), s.Policy)
			}
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

// pendingChannels reads funding_expiry_blocks for every pending open.
func pendingChannels(ctx context.Context, cli Client) (map[string]int32, error) {
	resp, err := cli.PendingChannels(ctx, &lnrpc.PendingChannelsRequest{})
	if err != nil {
		return nil, fmt.Errorf("listing this node's pending channels: %w", err)
	}
	out := map[string]int32{}
	for _, p := range resp.GetPendingOpenChannels() {
		out[p.GetChannel().GetChannelPoint()] = p.GetFundingExpiryBlocks()
	}
	return out, nil
}

func short(pubkey string) string {
	if len(pubkey) <= 16 {
		return pubkey
	}
	return pubkey[:16] + "…"
}
