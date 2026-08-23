// Package fees is the batch's fee rate, and the only place it can come from.
//
// Core's estimatesmartfee, never a fee API. That is CLAUDE.md's rule and the
// reason is in the rule: a fee API is handed the size of what is being built and
// the moment it is being built, which together are most of what this tool exists
// not to leak. Core is already on the machine, already has the mempool, and asks
// nobody.
//
// # Why the answer is a range rather than a number
//
// Three figures decide the rate, and the operator should see all three:
//
//   - Core's estimate for the chosen confirmation target, which is what the
//     batch would ordinarily pay;
//   - the node's relay floor — the higher of mempoolminfee and minrelaytxfee —
//     below which the transaction will not propagate at all;
//   - a configured floor, which exists because estimatesmartfee is entitled to
//     answer "insufficient data" and does so on every regtest node, on a freshly
//     synced one, and on any node that has been offline for a while.
//
// The rate is the largest of the three, and Source says which one won. Nothing
// here invents a number: a node with no estimate and no configured floor is an
// error, not a guess, because I-4 means a rate chosen badly cannot be corrected
// by replacing the transaction.
//
// # Why CONSERVATIVE
//
// Core's two modes differ in how willing they are to believe a recent fall in
// fees. ECONOMICAL will; CONSERVATIVE will not. For an ordinary payment the
// economical mode is right, because a payment that lags can be replaced. This
// transaction cannot be replaced — I-4 — so the only remedy for an underpaying
// batch is a CPFP child out of the change output, which costs another
// cold-wallet signing round. Paying a little more up front is the cheaper of
// those two errors, so CONSERVATIVE is the default and the mode is recorded in
// the report rather than assumed.
package fees

import (
	"context"
	"fmt"
	"math"

	"github.com/AusDavo/winthistle/internal/bitcoind"
	"github.com/AusDavo/winthistle/internal/plan"
)

// Core's own mode names, spelled here so a caller does not have to know them.
const (
	Conservative = "CONSERVATIVE"
	Economical   = "ECONOMICAL"
)

// DefaultTargetBlocks is the confirmation target asked of Core.
//
// Six rather than one or two. The batch is not urgent — nothing is at risk while
// it sits unconfirmed, since the coins stay ours and every channel is already
// recoverable — and a target of one buys minutes at a large premium. What the
// batch cannot tolerate is being so far under the market that it never confirms
// at all, and that is what the floors are for rather than the target.
const DefaultTargetBlocks = 6

// Source is which of the three figures set the rate.
type Source int

const (
	// FromEstimate: Core's estimatesmartfee answered and its rate was the
	// largest of the three.
	FromEstimate Source = iota

	// FromConfiguredFloor: the configured floor was higher than Core's estimate,
	// or Core had no estimate to give.
	FromConfiguredFloor

	// FromRelayFloor: the node's own relay floor was the highest, which means
	// anything less would not have propagated.
	FromRelayFloor
)

func (s Source) String() string {
	switch s {
	case FromEstimate:
		return "Core's estimate"
	case FromConfiguredFloor:
		return "the configured floor"
	case FromRelayFloor:
		return "the node's relay floor"
	default:
		return "unknown"
	}
}

// Request is what to ask Core.
type Request struct {
	// TargetBlocks is the confirmation target. Zero means DefaultTargetBlocks.
	TargetBlocks int

	// Mode is Core's estimate mode. Empty means Conservative — see the package
	// comment for why that is not the usual choice.
	Mode string

	// FloorSatPerVB is the operator's own floor, from winthistle.toml. It is what
	// stands in when Core cannot estimate, and Estimate refuses to produce a rate
	// without one in that case rather than choosing a number nobody agreed to.
	FloorSatPerVB float64

	// CPFPMultiple is how much headroom the change output must leave, as a
	// multiple of the rate. Zero means plan.DefaultCPFPMultiple.
	CPFPMultiple float64

	// Tolerance is how far the returned transaction's rate may stray from this
	// one before internal/plan objects. Zero means plan.DefaultFeeTolerance.
	Tolerance float64
}

func (r Request) targetBlocks() int {
	if r.TargetBlocks <= 0 {
		return DefaultTargetBlocks
	}
	return r.TargetBlocks
}

func (r Request) mode() string {
	if r.Mode == "" {
		return Conservative
	}
	return r.Mode
}

// Rate is the fee rate the batch will be built at, and everything that went into
// choosing it.
type Rate struct {
	// SatPerVB is the rate. It is the largest of the three figures below.
	SatPerVB float64

	Source Source

	// TargetBlocks is what was asked for and AnsweredForBlocks is what Core
	// answered about, which can be larger.
	TargetBlocks      int
	AnsweredForBlocks int
	Mode              string

	// EstimateSatPerVB is zero when Core could not estimate, and CoreSaid is
	// then Core's own reason.
	EstimateSatPerVB float64
	CoreSaid         string

	// RelayFloorSatPerVB is the higher of mempoolminfee and minrelaytxfee, and
	// ConfiguredFloorSatPerVB is the operator's.
	RelayFloorSatPerVB      float64
	ConfiguredFloorSatPerVB float64

	// MempoolTxs is how many transactions the node's mempool holds, which is the
	// one number that says whether an estimate is being made in a busy market or
	// an empty one.
	MempoolTxs int64

	cpfpMultiple float64
	tolerance    float64
}

// Fee is the plan's view of this rate.
//
// The CPFP target is carried through rather than left to the plan's default,
// because the change output has to be sized against the rate that was actually
// chosen — and on a node with no estimate, that rate is a floor rather than a
// market figure.
func (r Rate) Fee() plan.Fee {
	f := plan.Fee{
		TargetSatPerVB: r.SatPerVB,
		Tolerance:      r.tolerance,
	}
	multiple := r.cpfpMultiple
	if multiple <= 0 {
		multiple = plan.DefaultCPFPMultiple
	}
	f.CPFPTargetSatPerVB = r.SatPerVB * multiple
	return f
}

// Estimated reports whether Core had an opinion at all. A batch built on a node
// with no estimate is not wrong, but the operator should know it is being
// charged a floor rather than a market rate.
func (r Rate) Estimated() bool { return r.EstimateSatPerVB > 0 }

// Estimate asks Core for the batch's fee rate.
//
// It is repeatable and free, and it opens nothing: this belongs to Phase 0 in
// the strict sense — no stream, no clock, no cost to running it again a minute
// later when the operator asks what the number would be now.
func Estimate(ctx context.Context, node *bitcoind.Client, req Request) (Rate, error) {
	if req.FloorSatPerVB < 0 {
		return Rate{}, fmt.Errorf("a fee floor of %g sat/vB is not a floor", req.FloorSatPerVB)
	}

	est, err := node.EstimateSmartFee(ctx, req.targetBlocks(), req.mode())
	if err != nil {
		return Rate{}, err
	}
	info, err := node.GetMempoolInfo(ctx)
	if err != nil {
		return Rate{}, err
	}

	r := Rate{
		TargetBlocks:            req.targetBlocks(),
		AnsweredForBlocks:       est.Blocks,
		Mode:                    req.mode(),
		EstimateSatPerVB:        round2(est.SatPerVB()),
		CoreSaid:                est.Why(),
		RelayFloorSatPerVB:      info.FloorSatPerVB(),
		ConfiguredFloorSatPerVB: req.FloorSatPerVB,
		MempoolTxs:              info.Size,
		cpfpMultiple:            req.CPFPMultiple,
		tolerance:               req.Tolerance,
	}

	// Core cannot estimate and nobody named a floor. There is no number here
	// that would be honest, so there is no number.
	if !est.Answered() && req.FloorSatPerVB <= 0 {
		return r, fmt.Errorf("Core has no fee estimate for %d blocks (%s) and no "+
			"fee floor is configured, so there is no rate to build the batch at.\n"+
			"Set limits.fee_floor_sat_per_vb in winthistle.toml, or point this at a "+
			"node with enough block history to estimate. This build will not guess: "+
			"I-4 forbids replacing the funding transaction, so a rate chosen badly "+
			"can only be corrected by a CPFP child and another cold-wallet session.",
			r.TargetBlocks, r.CoreSaid)
	}

	r.SatPerVB, r.Source = r.EstimateSatPerVB, FromEstimate
	if r.ConfiguredFloorSatPerVB > r.SatPerVB {
		r.SatPerVB, r.Source = r.ConfiguredFloorSatPerVB, FromConfiguredFloor
	}
	if r.RelayFloorSatPerVB > r.SatPerVB {
		r.SatPerVB, r.Source = r.RelayFloorSatPerVB, FromRelayFloor
	}
	return r, nil
}

// round2 keeps a rate to two decimal places. Core's estimates carry eight
// decimal places of BTC, which is far more precision than a sat/vB figure has,
// and an unrounded rate makes every report unreadable for no gain.
func round2(v float64) float64 { return math.Round(v*100) / 100 }
