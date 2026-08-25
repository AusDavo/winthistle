// Package policy is one channel's intended forwarding policy: what Phase 0
// chooses, what the plan document shows, and what Phase 2 applies.
//
// It is its own package because those are three packages. internal/settle
// applies the policy and knows what LND does with it; internal/plan renders
// the batch plan and has to show the policy beside the amount, because the two
// are one decision — where the money goes, and what it will charge to route.
// internal/settle imported internal/plan for the CPFP arithmetic when it built
// the child, so the type could not live in either of them without a cycle. That
// import is gone with the child; a shared type between two packages that both
// render it is still the right shape, and moving it now would be churn.
//
// # Why the plan document is the right place to review it
//
// A new channel routes at LND's defaults from the moment it goes active:
// DefaultBaseFeeMsat and DefaultFeeRatePPM below, 1000 msat and 1 ppm. On a
// large channel that is close to free routing for whoever notices first, and
// the window is open from "active" until Phase 2's loop wins the race. Nothing
// about that is visible in the amounts, so a plan that showed only the amounts
// would have the operator approve half of the decision.
//
// All source citations are against lnd v0.21.2-beta.
package policy

import "fmt"

const (
	// LND's own defaults, which are what a new channel routes at until Phase 2
	// replaces them: chainreg.DefaultBitcoinBaseFeeMSat,
	// chainreg.DefaultBitcoinFeeRate and chainreg.DefaultBitcoinTimeLockDelta.
	DefaultBaseFeeMsat   = 1000
	DefaultFeeRatePPM    = 1
	DefaultTimeLockDelta = 80

	// MinTimeLockDelta and MaxTimeLockDelta are what UpdateChannelPolicy will
	// accept: routing.MinCLTVDelta and routing.MaxCLTVDelta.
	MinTimeLockDelta = 18
	MaxTimeLockDelta = 65535
)

// Policy is one channel's intended forwarding policy, chosen in Phase 0.
type Policy struct {
	BaseFeeMsat   int64
	FeeRatePPM    uint32
	TimeLockDelta uint32

	// MinHTLCMsat is applied only when set: LND distinguishes "not specified"
	// from zero with its own flag, and specifying zero is a different policy
	// from leaving the existing minimum alone.
	MinHTLCMsat *int64

	// MaxHTLCMsat of zero leaves LND's own maximum in place.
	MaxHTLCMsat uint64

	// InboundBaseFeeMsat and InboundFeeRatePPM must be zero or negative unless
	// the node runs with --accept-positive-inbound-fees; LND refuses a positive
	// value outright rather than clamping it.
	InboundBaseFeeMsat int32
	InboundFeeRatePPM  int32
}

// Validate refuses a policy LND would refuse, before a channel is waiting on it.
//
// Cheap and worth doing early: an out-of-range CLTV delta produces the same
// refusal on the hundredth poll as on the first, and a settlement loop that
// retries a permanently invalid policy forever is a channel routing at 1 ppm
// with a green tick beside it.
func (p Policy) Validate() error {
	switch {
	case p.TimeLockDelta < MinTimeLockDelta:
		return fmt.Errorf("a CLTV delta of %d is below LND's minimum of %d",
			p.TimeLockDelta, MinTimeLockDelta)
	case p.TimeLockDelta > MaxTimeLockDelta:
		return fmt.Errorf("a CLTV delta of %d is above LND's maximum of %d",
			p.TimeLockDelta, MaxTimeLockDelta)
	case p.BaseFeeMsat < 0:
		return fmt.Errorf("a base fee of %d msat is not a fee", p.BaseFeeMsat)
	case p.InboundBaseFeeMsat > 0 || p.InboundFeeRatePPM > 0:
		return fmt.Errorf("inbound fees must be zero or negative unless the node " +
			"runs with --accept-positive-inbound-fees, and LND refuses a positive " +
			"value rather than clamping it")
	}
	return nil
}

// Summary is the one line the plan document puts beside a channel's amount.
//
// It states the CLTV delta as well as the fees, because a delta is the one
// figure here that can be refused outright — see Validate — and the operator
// reviewing the plan is the last person who can change it for free.
func (p Policy) Summary() string {
	s := fmt.Sprintf("%d msat base + %d ppm, CLTV delta %d",
		p.BaseFeeMsat, p.FeeRatePPM, p.TimeLockDelta)
	if p.MinHTLCMsat != nil {
		s += fmt.Sprintf(", min HTLC %d msat", *p.MinHTLCMsat)
	}
	if p.MaxHTLCMsat != 0 {
		s += fmt.Sprintf(", max HTLC %d msat", p.MaxHTLCMsat)
	}
	if p.InboundBaseFeeMsat != 0 || p.InboundFeeRatePPM != 0 {
		s += fmt.Sprintf(", inbound %d msat + %d ppm",
			p.InboundBaseFeeMsat, p.InboundFeeRatePPM)
	}
	return s
}

// IsLNDDefault reports whether this policy is the one a channel would route at
// if nothing were applied at all.
//
// Worth saying out loud in the plan document rather than leaving the operator
// to compare three numbers: a policy that matches LND's defaults means Phase 2
// has nothing to win, and the fee-drain hazard the policy pass exists for is
// being accepted rather than addressed.
func (p Policy) IsLNDDefault() bool {
	return p.BaseFeeMsat == DefaultBaseFeeMsat &&
		p.FeeRatePPM == DefaultFeeRatePPM &&
		p.TimeLockDelta == DefaultTimeLockDelta &&
		p.MinHTLCMsat == nil && p.MaxHTLCMsat == 0 &&
		p.InboundBaseFeeMsat == 0 && p.InboundFeeRatePPM == 0
}
