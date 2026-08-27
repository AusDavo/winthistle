// Package reserve is the pre-flight for LND's anchor reserve.
//
// It exists because psbt_verify — step 5 of the sequence, run with the cold
// wallet already out and the ten-minute windows already ticking — can refuse a
// batch for a reason that has nothing to do with the batch.
//
// LightningWallet.PsbtFundingVerify does not stop when PsbtIntent.Verify
// returns. It goes on to call enforceNewReservedValue, which calls
// CheckReservedValue, which sums ListUnspentWitnessFromDefaultAccount — the
// node's *own* on-chain coins, unlocked ones only — and compares the total
// against RequiredReserve(public anchor channels + 1): 10,000 sat each, capped
// at 100,000. If the node is short, verify fails with
//
//	reserved wallet balance invalidated: transaction would leave insufficient
//	funds for fee bumping anchor channel closings (see debug log for details)
//
// which names neither the node's own wallet, nor the reserve figure, nor the
// fact that the cold wallet is fine. The actual cause only appears in LND's
// debug log, on the node, as "Reserved value=… above final walletbalance=…".
//
// So the check runs here instead, before anyone is asked for a signature.
//
// # Why the batch does not count towards it
//
// CheckReservedValue deducts the transaction's inputs that belong to the node's
// wallet and credits its outputs that pay into the node's wallet. A Winthistle
// batch spends cold-storage coins and pays cold-storage change, so neither
// applies: the figure LND compares is exactly the node's own unlocked balance,
// unchanged by the batch. That is what makes this pre-flight an exact prediction
// of step 5 rather than an estimate.
//
// It also means the design's remedy works for a reason worth knowing: a top-up
// output inside the batch, paying an address of the node's own, *is* credited by
// CheckReservedValue, so it counts at verify time without waiting for a block.
//
// # Two figures, and the batch moves between them
//
// The count CurrentNumAnchorChans reads is existing channels + 1 at the FIRST
// verify, and it can be larger at every verify after that. This used to be
// written the other way round — every verify sees the same pre-batch count,
// because CompleteReservation runs after psbt_finalize — and both halves of that
// are wrong in this build. There is no psbt_finalize here: a skip_finalize
// psbt_verify is what completes LND's funding flow, so CompleteReservation runs
// during arm.Verify, and its SyncPending (lnwallet/wallet.go:2534) writes the
// channel into the database before chan_pending is emitted. Nothing orders one
// channel's SyncPending against another channel's psbt_verify.
//
// Measured rather than reasoned about, at n = 3, by
// TestALaterVerifyCountsAnEarlierChannelInTheBatch: with channel 1 at
// chan_pending, RequiredReserve(additional=1) — the call behind AtVerify — went
// from 10,000 to 20,000 sat while the batch was half-verified. Back to back with
// no wait, the verify loop won the race against the peer's funding_signed round
// trip on that harness; that is a latency measurement and not a property to rest
// a pre-flight on.
//
// So the worst case at verify is existing + n, which is AfterBatch — the same
// figure the node needs once the batch is published, and below which LND declines
// further on-chain spends and public channel opens and has less on hand to
// fee-bump a force-close than it has decided there should be. AtVerify is the
// floor, AfterBatch is the ceiling, and a batch has to clear the ceiling.
//
// What clears it is plan.ReserveTopUp, which aims at the larger of the two
// figures, and the fact that CheckReservedValue credits an output paying into the
// node's own wallet: a top-up inside the batch counts at every verify, including
// the last.
//
// All source citations are against lnd v0.21.2-beta.
package reserve

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"google.golang.org/grpc"
)

// LND's own reserve constants, from lnwallet/wallet.go:
//
//	AnchorChanReservedValue    = btcutil.Amount(10_000)
//	MaxAnchorChanReservedValue = 10 * AnchorChanReservedValue
//
// Used only to explain a figure to the operator. Every number this package
// *decides* on comes from WalletKit.RequiredReserve, because the channel count
// behind it is CurrentNumAnchorChans's business — it counts open, pending-open
// and waiting-close public anchor channels, and pending-close ones out of the
// historical bucket — and that is not reproducible from outside the node.
const (
	PerAnchorChannel = 10_000
	MaxReserve       = 10 * PerAnchorChannel
)

// defaultAccount is the account CheckReservedValue looks at, and only that one:
// ListUnspentWitnessFromDefaultAccount passes lnwallet.DefaultAccountName.
const defaultAccount = "default"

// Client is the slice of LND this pre-flight needs.
//
// Three read-only WalletKit calls, and narrow on purpose: a pre-flight that runs
// before the operator has agreed to anything must not be able to move a coin,
// lease one, or open a stream, and the type says so.
type Client interface {
	ListUnspent(context.Context, *walletrpc.ListUnspentRequest,
		...grpc.CallOption) (*walletrpc.ListUnspentResponse, error)
	ListLeases(context.Context, *walletrpc.ListLeasesRequest,
		...grpc.CallOption) (*walletrpc.ListLeasesResponse, error)
	RequiredReserve(context.Context, *walletrpc.RequiredReserveRequest,
		...grpc.CallOption) (*walletrpc.RequiredReserveResponse, error)
}

// Batch is what the run will open, split the way LND's check splits it.
//
// Only public channels matter to the reserve. enforceNewReservedValue returns
// early for an unannounced channel — `if !isPublic || fundingIntent.LocalFundingAmt() == 0`
// — and CurrentNumAnchorChans skips private channels when counting. Private
// members are carried here so the report can say they were considered and why
// they do not count.
type Batch struct {
	Public  int
	Private int
}

// Total is the size of the batch.
func (b Batch) Total() int { return b.Public + b.Private }

// Verdict is what the pre-flight decided.
type Verdict int

const (
	// Clear: the node's wallet covers the reserve at verify and after the batch.
	Clear Verdict = iota

	// ShortAfterBatch: the first verify clears, but the wallet is under the
	// figure the batch's last verify could see, and under the one the node needs
	// once every member is pending. Not blocking, because the plan pays the
	// difference as a top-up output inside the batch; without one it is a refusal
	// waiting to happen partway through clock A.
	ShortAfterBatch

	// WouldBeRefused: psbt_verify will fail. Nothing about the batch can fix
	// this, and finding out at step 5 costs a cold-wallet session.
	WouldBeRefused

	// NotApplicable: every channel in the batch is private, so the check LND
	// runs at verify does not run at all.
	NotApplicable
)

func (v Verdict) String() string {
	switch v {
	case Clear:
		return "clear"
	case ShortAfterBatch:
		return "short after the batch"
	case WouldBeRefused:
		return "would be refused at verify"
	case NotApplicable:
		return "not applicable"
	default:
		return "unknown"
	}
}

// Finding is everything the pre-flight learned, in satoshis.
type Finding struct {
	Batch Batch

	// Available is the figure CheckReservedValue will compare: the node's own
	// unspent witness outputs in the default account, unlocked, at zero
	// confirmations. Read through WalletKit.ListUnspent, which wraps the same
	// ListUnspentWitness call with the same arguments.
	Available int64

	// Leased is what the wallet holds but cannot count. Not part of any
	// decision — it is the difference between "the hot wallet is empty" and "the
	// hot wallet is full and every coin in it is committed elsewhere", which is
	// the half of the diagnosis LND's error omits.
	Leased int64

	// NowRequired is RequiredReserve(additional_public_channels = 0): what the
	// node needs for the channels it already has.
	NowRequired int64

	// AtVerify is RequiredReserve(additional = 1) — the figure the FIRST
	// psbt_verify in the batch uses, and the floor for the rest. A later verify
	// can see a larger one, because an earlier channel may already be in the
	// channel database; see the package doc. Meaningful only when
	// Batch.Public > 0.
	AtVerify int64

	// AfterBatch is RequiredReserve(additional = Batch.Public): what the node
	// will need once the batch is published and all of them are pending — and
	// also the ceiling at verify, since the nth verify can see existing + n.
	AfterBatch int64
}

// Verdict reads the finding.
func (f Finding) Verdict() Verdict {
	switch {
	case f.Batch.Public == 0:
		return NotApplicable
	case f.Available < f.AtVerify:
		return WouldBeRefused
	case f.Available < f.AfterBatch:
		return ShortAfterBatch
	default:
		return Clear
	}
}

// Blocking reports whether the run must not proceed to signing.
func (f Finding) Blocking() bool { return f.Verdict() == WouldBeRefused }

// ShortfallAtVerify is what has to arrive before the batch's first verify will
// pass. It is a floor, not the figure to aim at — see ShortfallAfterBatch.
func (f Finding) ShortfallAtVerify() int64 { return shortfall(f.AtVerify, f.Available) }

// ShortfallAfterBatch is what has to arrive before the node is back at the
// reserve it wants with the whole batch pending — and it is also what every
// verify in the batch is safe against, which is why plan.ReserveTopUp aims here.
func (f Finding) ShortfallAfterBatch() int64 { return shortfall(f.AfterBatch, f.Available) }

func shortfall(need, have int64) int64 {
	if need > have {
		return need - have
	}
	return 0
}

// Check runs the pre-flight.
//
// It returns an error only when LND could not be asked. A shortfall is not an
// error here: it is a finding, with a verdict and a report, because the caller's
// job is to show it to a human and the human's next move differs for each
// verdict.
func Check(ctx context.Context, cli Client, b Batch) (Finding, error) {
	if b.Public < 0 || b.Private < 0 {
		return Finding{}, fmt.Errorf("a batch cannot have %d public and %d private channels",
			b.Public, b.Private)
	}
	if b.Total() == 0 {
		return Finding{}, fmt.Errorf("a batch with no channels in it is not a batch")
	}

	f := Finding{Batch: b}

	available, err := unlockedBalance(ctx, cli)
	if err != nil {
		return Finding{}, err
	}
	f.Available = available

	leased, err := leasedBalance(ctx, cli)
	if err != nil {
		return Finding{}, err
	}
	f.Leased = leased

	// Three figures, one call each, memoised because additional=1 and
	// additional=Public coincide for the common single-channel batch.
	reserves := map[int]int64{}
	required := func(additional int) (int64, error) {
		if v, ok := reserves[additional]; ok {
			return v, nil
		}
		resp, err := cli.RequiredReserve(ctx, &walletrpc.RequiredReserveRequest{
			AdditionalPublicChannels: uint32(additional),
		})
		if err != nil {
			return 0, fmt.Errorf("asking LND for the reserve with %d more public "+
				"channel(s): %w", additional, err)
		}
		reserves[additional] = resp.GetRequiredReserve()
		return reserves[additional], nil
	}

	if f.NowRequired, err = required(0); err != nil {
		return Finding{}, err
	}
	if f.AtVerify, err = required(1); err != nil {
		return Finding{}, err
	}
	if f.AfterBatch, err = required(b.Public); err != nil {
		return Finding{}, err
	}
	return f, nil
}

// unlockedBalance sums exactly what CheckReservedValue will sum.
//
// min_confs 0 is not a shortcut: CheckReservedValue calls
// ListUnspentWitnessFromDefaultAccount(0, math.MaxInt32), and btcwallet's
// ListUnspent counts a credit with zero confirmations. A top-up therefore counts
// from the moment it is in the mempool, which is worth telling the operator.
//
// Locked coins are absent for the same reason in both places: btcwallet's
// ListUnspent skips an outpoint the wallet has leased.
func unlockedBalance(ctx context.Context, cli Client) (int64, error) {
	resp, err := cli.ListUnspent(ctx, &walletrpc.ListUnspentRequest{
		MinConfs: 0,
		MaxConfs: math.MaxInt32,
		Account:  defaultAccount,
	})
	if err != nil {
		return 0, fmt.Errorf("listing the node's own unspent outputs: %w", err)
	}
	var total int64
	for _, u := range resp.GetUtxos() {
		total += u.GetAmountSat()
	}
	return total, nil
}

// leasedBalance sums what the wallet is holding unspendable.
//
// Wallet-wide rather than per-account: ListLeases has no account filter. It is
// only ever reported, never compared, so the imprecision costs nothing.
func leasedBalance(ctx context.Context, cli Client) (int64, error) {
	resp, err := cli.ListLeases(ctx, &walletrpc.ListLeasesRequest{})
	if err != nil {
		return 0, fmt.Errorf("listing the node's coin leases: %w", err)
	}
	var total int64
	for _, l := range resp.GetLockedUtxos() {
		total += int64(l.GetValue())
	}
	return total, nil
}

// ErrBatchChanged means the batch that opened is not the batch this finding was
// made about.
var ErrBatchChanged = errors.New("the reserve was checked for a different batch")

// StillApplies reports whether this finding describes the batch b.
//
// The pre-flight runs in Phase 0, before any funding stream exists, against the
// channel list the operator approved. By the time the streams are open that list
// could have changed — a peer dropped, a channel flipped to private, a batch
// re-planned — and a finding is about a particular count of announced channels
// and nothing else. Two figures in it move with that count: AtVerify is
// meaningless if Public was zero and is not now, and AfterBatch is simply the
// wrong number for a different n.
//
// This costs no RPC. It is the cheapest possible check that the answer on the
// screen is an answer about the batch on the screen, and the armed window is
// where a stale one would first do damage.
func (f Finding) StillApplies(b Batch) error {
	if f.Batch == b {
		return nil
	}
	return fmt.Errorf("%w: checked %d announced and %d private, opening %d and %d. "+
		"Re-run the pre-flight — AtVerify and AfterBatch are both figures about a "+
		"particular count of announced channels",
		ErrBatchChanged, f.Batch.Public, f.Batch.Private, b.Public, b.Private)
}
