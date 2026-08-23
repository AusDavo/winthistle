package settle

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/AusDavo/winthistle/internal/bitcoind"
	"github.com/AusDavo/winthistle/internal/coldwallet"
	"github.com/AusDavo/winthistle/internal/plan"
	"github.com/AusDavo/winthistle/internal/prose"
)

// RateTolerance is how far below the package target the achieved rate may sit
// before the child is refused.
//
// It exists because the two sizes being compared are not measured the same way.
// plan.SizeOf is an upper bound — 73-byte signatures, the largest DER can be —
// while Core sizes with its own dummy signatures, so the rate this package
// reports is a floor on the rate the transaction will actually pay. Two per cent
// covers that gap and nothing else: a genuine failure to bump misses by orders
// of magnitude, not by a percent.
const RateTolerance = 0.02

// Parent is the published batch, as the child has to reason about it.
//
// Every figure here comes from the verification the batch already passed: the
// vsize is exact once the transaction is finalized, and the change outpoint is
// the one internal/plan attributed. Nothing is re-derived.
type Parent struct {
	TxID string

	// VsizeVB and FeeSat are the parent's own, and the pair is what decides how
	// much the child has to pay.
	VsizeVB int64
	FeeSat  int64

	// Change is the outpoint the child spends and ChangeSat what it is worth.
	// I-4 is why this exists at all: a batch that cannot be replaced can only be
	// accelerated by spending its own change, so a batch with no change output
	// is a batch with no remedy, and internal/plan refuses one.
	Change    plan.Outpoint
	ChangeSat int64
}

// Rate is the parent's own fee rate.
func (p Parent) Rate() float64 {
	if p.VsizeVB <= 0 {
		return 0
	}
	return float64(p.FeeSat) / float64(p.VsizeVB)
}

// ChildRequest is what to build.
type ChildRequest struct {
	// Wallet is the watch-only cold wallet, which owns the change output.
	Wallet *bitcoind.Client

	Parent Parent

	// TargetSatPerVB is the rate to lift the parent and child to *together*.
	// Not the child's own rate: the point of a CPFP child is that a miner values
	// the pair, so the figure that matters is the package's.
	TargetSatPerVB float64
}

// Child is the acceleration transaction, unsigned.
//
// Unsigned, and that is the whole shape of it: the change output belongs to cold
// storage, so a child spending it costs another signing round. There is no way
// around that and no attempt to pretend otherwise — the alternative would be a
// hot key able to spend the batch's change, which is the thing the cold wallet
// exists to avoid.
type Child struct {
	// PSBT is base64 and Raw the same packet in bytes, for the signers.
	PSBT string
	Raw  []byte

	TxID string

	// VsizeVB is this build's own estimate, using the same upper bounds
	// internal/plan's verifier uses. It is therefore a ceiling, which makes
	// PackageRate a floor.
	VsizeVB int64
	FeeSat  int64

	// RequiredFeeSat is what this package's own arithmetic says the child has to
	// pay, computed with plan.ChildFeeSat. Core arrives at its figure
	// independently; keeping both is what makes the agreement checkable.
	RequiredFeeSat int64

	// PaysTo is the cold-wallet address the child sends the remainder to, and
	// OutputSat what is left after the fee.
	PaysTo    string
	OutputSat int64

	// The package arithmetic, so the report can show its working.
	PackageVsizeVB int64
	PackageFeeSat  int64
	PackageRate    float64

	// Inputs are the coins Core locked while building this. They are the child's
	// alone — the parent's change, and nothing else — but they are still locked,
	// and a caller that abandons the child has to release them.
	Inputs []bitcoind.Outpoint
}

var (
	// ErrNoChildNeeded means the parent already pays the target rate on its own.
	ErrNoChildNeeded = errors.New("the batch already pays the target rate")

	// ErrChangeTooSmall means the change output cannot fund a child that reaches
	// the target. internal/plan's ChangeFloor exists to make this unreachable at
	// planning time; reaching it means the fee market moved further than
	// DefaultCPFPMultiple allowed for.
	ErrChangeTooSmall = errors.New("the change output is too small to lift the batch")

	// ErrNotBumped means Core built a child that does not lift the package,
	// which is what happens if the parent is no longer unconfirmed.
	ErrNotBumped = errors.New("the child does not lift the parent to the target")
)

// BuildChild builds the CPFP child that accelerates a stalled batch.
//
// Never a replacement. I-4 is not a preference here: replacing the funding
// transaction changes every outpoint in it, and every peer holds a commitment
// signature against the old ones. There is no code path in this repository that
// bumps the parent, and this is the function that exists instead.
//
// # Core does the package arithmetic, and it is ours to check
//
// walletcreatefundedpsbt's fee_rate is not the child's own rate when the child
// spends an unconfirmed input. Core accounts for the unconfirmed ancestors and
// charges the fee that lifts the whole package to the rate asked for:
//
//	fee = ceil(rate * (parentVsize + childVsize)) - parentFee
//
// which is, term for term, plan.ChildFeeSat — the same expression internal/plan
// builds ChangeFloor out of when it decides whether a batch's change output is
// large enough to rescue it. Measured against Core 29 at 50, 200 and 600 sat/vB
// on a 7,007 vB parent paying 1 sat/vB: the two agree exactly.
//
// So this asks Core for the package rate and then checks the answer with its own
// arithmetic, which is the relationship internal/plan already has with
// psbt_verify. A disagreement is a refusal rather than a warning, because the
// only reason to build this transaction is to reach a particular rate.
//
// The child spends exactly one input — the parent's change — and pays exactly
// one output, back to the cold wallet. add_inputs is off: pulling in a second,
// already-confirmed coin would make a cheaper child, and it would also mean the
// acceleration no longer depends on the parent, which is the one property CPFP
// has to have.
func BuildChild(ctx context.Context, req ChildRequest) (*Child, error) {
	switch {
	case req.Wallet == nil:
		return nil, fmt.Errorf("no cold wallet to build the child from")
	case req.Parent.TxID == "":
		return nil, fmt.Errorf("the parent has no txid")
	case req.Parent.Change.TxID == "":
		return nil, fmt.Errorf("the parent has no change outpoint, so there is " +
			"nothing to spend. I-4 forbids replacing it, so a batch in that state " +
			"can only be waited out")
	case req.Parent.VsizeVB <= 0 || req.Parent.FeeSat <= 0:
		return nil, fmt.Errorf("the parent's size (%d vB) and fee (%d sat) are what "+
			"decide the child's, and one of them is missing",
			req.Parent.VsizeVB, req.Parent.FeeSat)
	case req.Parent.ChangeSat <= 0:
		return nil, fmt.Errorf("the parent's change output is %d sat", req.Parent.ChangeSat)
	case req.TargetSatPerVB <= 0:
		return nil, fmt.Errorf("a package target of %g sat/vB is not a rate",
			req.TargetSatPerVB)
	}

	if req.Parent.Rate() >= req.TargetSatPerVB {
		return nil, fmt.Errorf("%w: it pays %.2f sat/vB and the target is %.2f",
			ErrNoChildNeeded, req.Parent.Rate(), req.TargetSatPerVB)
	}

	addr, err := coldwallet.ChangeAddress(ctx, req.Wallet)
	if err != nil {
		return nil, err
	}

	child, err := buildChildAt(ctx, req, addr, req.TargetSatPerVB)
	if err != nil {
		if amountTooSmall(err) {
			return nil, tooSmallToLift(ctx, req, addr, err)
		}
		return nil, err
	}

	child.RequiredFeeSat = plan.ChildFeeSat(req.Parent.VsizeVB, req.Parent.FeeSat,
		req.TargetSatPerVB, child.VsizeVB)
	child.PackageVsizeVB = req.Parent.VsizeVB + child.VsizeVB
	child.PackageFeeSat = req.Parent.FeeSat + child.FeeSat
	child.PackageRate = float64(child.PackageFeeSat) / float64(child.PackageVsizeVB)

	if child.PackageRate < req.TargetSatPerVB*(1-RateTolerance) {
		return nil, fmt.Errorf("%w: the pair pays %.2f sat/vB and the target is "+
			"%.2f. Core charged %s and this build's own arithmetic wanted %s.\n"+
			"The usual cause is that %s is no longer unconfirmed — Core only bumps "+
			"ancestors that are still in the mempool, and a confirmed parent needs "+
			"no child",
			ErrNotBumped, child.PackageRate, req.TargetSatPerVB,
			prose.Sats(child.FeeSat), prose.Sats(child.RequiredFeeSat), req.Parent.TxID)
	}
	if child.OutputSat < plan.DustSat {
		return nil, fmt.Errorf("%w: the child would return %s, below the %s dust "+
			"floor, so its output would not relay", ErrChangeTooSmall,
			prose.Sats(child.OutputSat), prose.Sats(plan.DustSat))
	}
	return child, nil
}

// tooSmallToLift turns Core's refusal into the arithmetic behind it.
//
// Core says only "The transaction amount is too small to pay the fee", which
// names neither the shortfall nor the rate the change could actually reach —
// and both are what the operator needs, because under I-4 the child is the only
// remedy there is and "a smaller lift" is the remaining option.
//
// The child's size cannot be read off a built transaction here, because there is
// no built transaction. It is computed instead, from the change output's own
// scripts: one input spending that script, one output paying it back. That is
// plan.ChildVsize, the same estimate the verifier used at planning time when it
// decided this change output was big enough — so the two figures are arrived at
// the same way, and comparing them is meaningful.
func tooSmallToLift(ctx context.Context, req ChildRequest, addr string, cause error) error {
	childVsize, err := changeChildVsize(ctx, req)
	if err != nil {
		return fmt.Errorf("%w: %v (and the child's size could not be worked out "+
			"either: %v)", ErrChangeTooSmall, cause, err)
	}

	need := plan.ChildFeeSat(req.Parent.VsizeVB, req.Parent.FeeSat,
		req.TargetSatPerVB, childVsize)
	// What the change *could* pay for: the rate reached if the child's fee were
	// the whole change output less the dust it has to leave behind.
	affordable := float64(req.Parent.FeeSat+req.Parent.ChangeSat-plan.DustSat) /
		float64(req.Parent.VsizeVB+childVsize)

	// A smaller lift is worth offering only if it is actually a lift. When the
	// change is near dust the best reachable rate is the parent's own, and
	// saying "try 1.01 sat/vB" would be advice to pay a fee for nothing.
	best := fmt.Sprintf("The most this change can reach is about %.2f sat/vB, "+
		"which is worth doing.", affordable)
	if affordable <= req.Parent.Rate() {
		best = fmt.Sprintf("This change output cannot lift the batch at all: the "+
			"most it reaches is about %.2f sat/vB and the batch already pays "+
			"%.2f. There is no acceleration available, and I-4 rules out the "+
			"alternative.", affordable, req.Parent.Rate())
	}

	return fmt.Errorf("%w: lifting %s and a %d vB child to %.2f sat/vB needs %s, "+
		"and the change output is %s.\n%s\ninternal/plan sizes the change output "+
		"so that this cannot happen at %gx the batch's own rate; reaching it means "+
		"the fee market moved further than that",
		ErrChangeTooSmall, req.Parent.TxID, childVsize, req.TargetSatPerVB,
		prose.Sats(need), prose.Sats(req.Parent.ChangeSat), best,
		plan.DefaultCPFPMultiple)
}

// changeChildVsize sizes the child from the change output's own scripts.
//
// listunspent is where the witness script comes from: the watch-only wallet
// derived the change address, so it knows the script that redeems it. Without
// that script a P2WSH spend cannot be sized at all — which is itself the right
// answer, because a change output this build cannot size is one it could not
// have promised a CPFP child for.
func changeChildVsize(ctx context.Context, req ChildRequest) (int64, error) {
	utxos, err := req.Wallet.ListUnspent(ctx, 0, math.MaxInt32)
	if err != nil {
		return 0, fmt.Errorf("listing the cold wallet's outputs: %w", err)
	}
	for _, u := range utxos {
		if u.TxID != req.Parent.Change.TxID || u.Vout != req.Parent.Change.Vout {
			continue
		}
		script, err := hex.DecodeString(u.ScriptPubKey)
		if err != nil {
			return 0, fmt.Errorf("the change output's script is not hex: %w", err)
		}
		witness, err := hex.DecodeString(u.WitnessScript)
		if err != nil {
			return 0, fmt.Errorf("the change output's witness script is not hex: %w", err)
		}
		return plan.ChildVsize(script, witness)
	}
	return 0, fmt.Errorf("the cold wallet does not hold %s", req.Parent.Change)
}

// amountTooSmall recognises Core's refusal to subtract a fee larger than the
// output it is subtracting from.
func amountTooSmall(err error) bool {
	return strings.Contains(err.Error(), "amount is too small to pay the fee") ||
		strings.Contains(err.Error(), "too small to send after the fee")
}

// buildChildAt builds a one-in one-out child at a given package rate.
func buildChildAt(ctx context.Context, req ChildRequest, addr string, rate float64) (
	*Child, error) {

	inputs := []map[string]any{{
		"txid": req.Parent.Change.TxID,
		"vout": req.Parent.Change.Vout,
		// I-4's habit rather than I-4 itself: replacing the *child* would not
		// touch a funding outpoint, so it would be safe. It is disabled anyway,
		// because internal/plan refuses any input below this sequence and one
		// verifier for both transactions is worth more than the option to bump.
		"sequence": plan.MaxNonReplaceableSequence,
	}}
	outputs := []map[string]any{{addr: prose.BTC(req.Parent.ChangeSat)}}

	opts := map[string]any{
		// The package rate, not the child's own — see BuildChild.
		"fee_rate": rate,
		// The whole point: without this the child would have to pay its fee out
		// of coins that are not there, since the only input is the change and
		// the output already claims all of it.
		"subtractFeeFromOutputs": []int{0},
		// The change output is unconfirmed by definition — that is why a child
		// is being built at all.
		"minconf": 0,
		// And it may be "unsafe" by Core's reckoning, which is a heuristic about
		// provenance rather than about validity: an unconfirmed output is safe
		// only if every input of the transaction that created it belongs to this
		// wallet. That holds for a batch the cold wallet funded, and does not
		// hold in general. The input here is named explicitly — there is no coin
		// selection to go wrong — so the heuristic has nothing to add, and
		// without this a batch whose parent Core distrusts could not be
		// accelerated at all. Under I-4 that would leave it with no remedy.
		"include_unsafe": true,
		// Off, and this is the load-bearing one. A second confirmed input would
		// make a cheaper child and would also let a miner take the child without
		// the parent, which is the one thing CPFP must not allow.
		"add_inputs":      false,
		"lockUnspents":    true,
		"replaceable":     false,
		"includeWatching": true,
	}

	var created struct {
		PSBT string  `json:"psbt"`
		Fee  float64 `json:"fee"`
	}
	// The trailing true is bip32derivs: the key-origin information is what lets
	// the cold wallet's devices recognise their own key, here as in the batch.
	err := req.Wallet.Call(ctx, "walletcreatefundedpsbt",
		[]any{inputs, outputs, 0, opts, true}, &created)
	if err != nil {
		return nil, fmt.Errorf("building the CPFP child at %.2f sat/vB: %w", rate, err)
	}

	raw, err := base64.StdEncoding.DecodeString(created.PSBT)
	if err != nil {
		return nil, fmt.Errorf("Core returned a child PSBT that is not base64: %w", err)
	}
	size, err := plan.SizeOf(raw)
	if err != nil {
		return nil, fmt.Errorf("sizing the CPFP child: %w", err)
	}

	spent, txid, err := describeChild(ctx, req.Wallet, created.PSBT)
	if err != nil {
		return nil, err
	}
	want := bitcoind.Outpoint{TxID: req.Parent.Change.TxID, Vout: req.Parent.Change.Vout}
	if len(spent) != 1 || spent[0] != want {
		return nil, fmt.Errorf("the child spends %v, not the parent's change output "+
			"%s. A child that does not spend the parent accelerates nothing",
			spent, req.Parent.Change)
	}

	feeSat := int64(math.Round(created.Fee * 1e8))
	return &Child{
		PSBT:      created.PSBT,
		Raw:       raw,
		TxID:      txid,
		VsizeVB:   size.Vsize,
		FeeSat:    feeSat,
		PaysTo:    addr,
		OutputSat: req.Parent.ChangeSat - feeSat,
		Inputs:    spent,
	}, nil
}

// describeChild reads the child's inputs and txid back from Core.
func describeChild(ctx context.Context, wallet *bitcoind.Client, psbtB64 string) (
	[]bitcoind.Outpoint, string, error) {

	var decoded struct {
		Tx struct {
			TxID string `json:"txid"`
			Vin  []struct {
				TxID string `json:"txid"`
				Vout uint32 `json:"vout"`
			} `json:"vin"`
		} `json:"tx"`
	}
	if err := wallet.Call(ctx, "decodepsbt", []any{psbtB64}, &decoded); err != nil {
		return nil, "", fmt.Errorf("reading back the CPFP child: %w", err)
	}
	out := make([]bitcoind.Outpoint, 0, len(decoded.Tx.Vin))
	for _, in := range decoded.Tx.Vin {
		out = append(out, bitcoind.Outpoint{TxID: in.TxID, Vout: in.Vout})
	}
	return out, decoded.Tx.TxID, nil
}
