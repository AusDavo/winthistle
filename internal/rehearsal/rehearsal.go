// Package rehearsal is Phase 0's dress rehearsal, and the measurement the armed
// window's abort gate compares against.
//
// A decoy PSBT — the same coins, the same fee rate, the same number of outputs,
// all of them paying back into the cold wallet — is pushed through every signer
// for real, combined and finalized in-app, offered to testmempoolaccept, and
// thrown away. Nothing is broadcast and nothing changes hands. What comes out is
// a number: how long a full signing round actually takes with these devices, in
// this room, tonight.
//
// # Why that number is the gate
//
// The peers' ten minutes start at arm.Open and end whether or not the operator
// is ready. Everything inside that window except the signing round has a
// duration this build controls; the signing round has a duration the devices and
// the operator control, and until it has been measured, arming is a bet.
//
// docs/design.html sets the gate at 5:00 — limits.abort_after_signing_seconds —
// and it is a hard refusal rather than a warning: a rehearsal slower than that
// means the batch will not be armed at all. Note what running out of time
// actually costs, though. Because nothing is published until the I-1 gate opens,
// a lapsed window strands no channels and spends no fees; it costs one more
// signing round. The clock is an inconvenience, not a hazard, and the gate is
// there so the inconvenience is met before the cold wallet comes out rather than
// at 9:30 with three peers waiting.
//
// # What the rehearsal cannot prove
//
// It opens no funding stream, which is exactly what makes it free and exactly
// why it cannot vouch for LND's funding flow. Signers, descriptors and fee
// arithmetic are what it proves. Steps 2 onward are untested by it; that is what
// the mainnet cold probe is for.
//
// # The decoy is dangerous in one specific way
//
// By the end of Run there is, briefly, a fully signed transaction spending the
// cold wallet's coins. It pays them straight back to the cold wallet, so
// broadcasting it would lose nothing — but it would spend the very inputs the
// real batch is about to use, and the batch would then fail at build time or,
// worse, at psbt_verify with the cold wallet already out. So the bytes do not
// leave this package: Measurement carries the decoy's txid, size and fee and no
// transaction at all, the way arm.Armed keeps its raw transaction unexported.
// Core's coin locks are released before Run returns, on every path.
package rehearsal

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/AusDavo/winthistle/internal/bitcoind"
	"github.com/AusDavo/winthistle/internal/coldwallet"
	"github.com/AusDavo/winthistle/internal/combine"
)

// DefaultAbortAfterSigning is docs/design.html's limits.abort_after_signing_seconds.
//
// Five minutes against the peers' ten. The other half is not slack: it is the
// build, n psbt_verify calls, the merge, the finalize, n psbt_finalize calls
// each waiting for its own chan_pending, and the channel backup export — plus
// whatever the operator does between them.
const DefaultAbortAfterSigning = 5 * time.Minute

// PeerWindow is the budget the gate is carved out of: the peer's own
// reservation timeout. Stated here so the reports can show what the measurement
// is being judged against rather than only whether it passed.
const PeerWindow = 10 * time.Minute

// Signer collects one partial signature from one device.
//
// A func rather than an interface because the transports differ so much — SD
// card, animated QR, a Core wallet in the test harness — and every one of them
// is "hand over a base64 PSBT, get one back". The returned Part must carry only
// that device's own partial signature: internal/combine refuses a packet that
// arrives already finalized, because a finalized input is a complete witness and
// that device therefore held a broadcastable transaction (I-2).
type Signer func(ctx context.Context, psbtB64 string) (combine.Part, error)

// Device is one cold-storage signer and the way to reach it.
type Device struct {
	// Label is what the operator calls it. It is what the journal records and
	// what a refusal names, and it never has to be unique to a key.
	Label string
	Sign  Signer
}

// Request is the rehearsal's shape.
//
// It has to match the real batch's, because the point of the measurement is
// that it predicts the real signing round. The two things that decide how long a
// hardware device takes are the number of inputs it must sign and the number of
// outputs it must show the operator, so both are mirrored rather than
// approximated.
type Request struct {
	// Wallet is the watch-only cold wallet, and Node is Core without a wallet
	// scope, for testmempoolaccept.
	Wallet *bitcoind.Client
	Node   *bitcoind.Client

	// MirrorSat are the real batch's output amounts, in order: every funding
	// output and the reserve top-up. Each becomes a decoy output of the same
	// size paying a fresh address of the cold wallet's own, so coin selection
	// and the output count both come out the same.
	MirrorSat []int64

	// FeeRateSatPerVB is the batch's rate, from internal/fees.
	FeeRateSatPerVB float64

	// MinConfirmations is the batch's input floor.
	MinConfirmations int

	// Devices are the signers, in the order an operator would visit them. There
	// must be exactly as many as the descriptor requires: btcd's finalizer wants
	// exactly m signatures, not more, so asking a third device on a 2-of-3 "just
	// in case" breaks the transaction. internal/combine catches that and says so.
	Devices []Device

	// Limit is the abort gate. Zero means DefaultAbortAfterSigning.
	Limit time.Duration
}

func (r Request) limit() time.Duration {
	if r.Limit <= 0 {
		return DefaultAbortAfterSigning
	}
	return r.Limit
}

// Round is one device's turn.
type Round struct {
	Label   string
	Started time.Time
	Elapsed time.Duration

	// Err is that device's failure. A rehearsal that finds one has done its job:
	// a signer that produces malformed witnesses is one of the two failures the
	// phase exists to catch, and finding it here costs nothing.
	Err error
}

// Decoy is what the rehearsal built, minus the thing it must not hand out.
//
// No raw transaction and no PSBT. See the package comment: the finalized decoy
// spends the coins the real batch is about to spend, so the one thing this type
// must not be able to do is let anybody publish it.
type Decoy struct {
	TxID   string
	Inputs []bitcoind.Outpoint
	FeeSat int64

	// VsizeVB is exact rather than estimated: it is measured on the finalized
	// transaction, with every witness present. It is zero if the rehearsal did
	// not get that far.
	VsizeVB int64

	// Outputs is how many the decoy mirrored, and OutputSat their total. Both
	// are the batch's, which is the point: the number of outputs is what each
	// device has to display and confirm, and that is most of what a signing
	// round costs.
	Outputs   int
	OutputSat int64
}

// Measurement is the rehearsal's answer.
type Measurement struct {
	Decoy  Decoy
	Rounds []Round

	// Signing is the number the gate compares: from the moment the first device
	// was handed the PSBT to the moment the last partial came back. It excludes
	// building the decoy and combining the result, because in the real window
	// the build happens before the clock matters and the combine is this
	// process's own arithmetic.
	Signing time.Duration

	// Total is the whole rehearsal, for the operator's sense of the evening.
	Total time.Duration

	// Accepted is testmempoolaccept's verdict, and RejectReason is Core's own
	// words when it says no. testmempoolaccept validates without relaying, which
	// is the only pre-flight there is and specifically not a broadcast.
	Accepted     bool
	RejectReason string

	Limit time.Duration
}

// Passes reports whether the batch may be armed.
func (m *Measurement) Passes() bool {
	return m != nil && m.Accepted && m.failures() == 0 && m.Signing <= m.Limit
}

// Headroom is what would be left of a peer's ten minutes after a signing round
// of this length. Negative means the round alone outlasts the window.
func (m *Measurement) Headroom() time.Duration { return PeerWindow - m.Signing }

// Slowest names the device that took longest, which is the one to do something
// about.
func (m *Measurement) Slowest() (Round, bool) {
	var (
		worst Round
		found bool
	)
	for _, r := range m.Rounds {
		if !found || r.Elapsed > worst.Elapsed {
			worst, found = r, true
		}
	}
	return worst, found
}

func (m *Measurement) failures() int {
	n := 0
	for _, r := range m.Rounds {
		if r.Err != nil {
			n++
		}
	}
	return n
}

var (
	// ErrTooSlow means the signing round outran the abort gate.
	ErrTooSlow = errors.New("the signing round is slower than the abort gate allows")

	// ErrDeviceFailed means at least one signer did not return a usable partial.
	ErrDeviceFailed = errors.New("a signer did not return a usable partial signature")

	// ErrDecoyRefused means the mempool would not have accepted the rehearsal's
	// own transaction, so the signatures or the fee are wrong.
	ErrDecoyRefused = errors.New("testmempoolaccept refused the rehearsal transaction")

	// ErrTxIDMoved means the decoy's txid changed under signing, which is I-3's
	// failure rehearsed: on the real batch that is n channels lost.
	ErrTxIDMoved = errors.New("the transaction's txid moved while it was being signed")
)

// Gate is the check Phase 1 makes before arming.
//
// Separate from Run so that a measurement taken an hour ago can still be judged,
// and so the refusal to arm is one call at the top of the armed window rather
// than something Run happened to have decided.
func Gate(m *Measurement) error {
	switch {
	case m == nil:
		return fmt.Errorf("no rehearsal has been run, so the abort gate has nothing " +
			"to compare against. Phase 0's measurement is what makes the ten-minute " +
			"window a known quantity rather than a bet")
	case m.failures() > 0:
		return fmt.Errorf("%w: %d of %d device(s) failed", ErrDeviceFailed,
			m.failures(), len(m.Rounds))
	case !m.Accepted:
		return fmt.Errorf("%w: %s", ErrDecoyRefused, m.RejectReason)
	case m.Signing > m.Limit:
		return fmt.Errorf("%w: the round took %s and the gate is %s",
			ErrTooSlow, m.Signing.Round(time.Second), m.Limit.Round(time.Second))
	}
	return nil
}

// Run performs the dress rehearsal.
//
// It returns a Measurement even when the rehearsal went badly, because a failed
// rehearsal is the most useful thing this phase produces: it names the device,
// or the fee, or the refusal, at the only moment when acting on it is free. Gate
// is what turns the measurement into a verdict.
//
// Core's coin locks are released before it returns, on every path. The decoy's
// build locks the very coins the batch is about to use, and a rehearsal that
// left them held would have the real build fail with "insufficient funds" on a
// wallet that is not short of anything.
func Run(ctx context.Context, req Request) (*Measurement, error) {
	switch {
	case req.Wallet == nil || req.Node == nil:
		return nil, fmt.Errorf("the rehearsal needs the cold wallet and the node")
	case len(req.MirrorSat) == 0:
		return nil, fmt.Errorf("nothing to mirror: the rehearsal builds a decoy of " +
			"the same shape as the batch, so it needs the batch's output amounts")
	case len(req.Devices) == 0:
		return nil, fmt.Errorf("no signers: a rehearsal with nobody to sign measures " +
			"nothing")
	case req.FeeRateSatPerVB <= 0:
		return nil, fmt.Errorf("a fee rate of %g sat/vB is not a fee rate",
			req.FeeRateSatPerVB)
	}

	started := time.Now()
	m := &Measurement{Limit: req.limit()}

	built, release, err := buildDecoy(ctx, req)
	if err != nil {
		return nil, err
	}
	// Before anything else can fail. The locks are the one thing this function
	// leaves behind that would hurt the real batch.
	defer release()

	m.Decoy = Decoy{
		TxID:      built.TxID,
		Inputs:    built.Inputs,
		FeeSat:    built.FeeSat,
		Outputs:   len(req.MirrorSat),
		OutputSat: sum(req.MirrorSat),
	}

	// The measured round. Sequential, because that is how an operator visits
	// devices, and because two devices signing at once is not the thing the
	// window has to accommodate.
	signingStart := time.Now()
	parts := make([]combine.Part, 0, len(req.Devices))
	for _, d := range req.Devices {
		r := Round{Label: d.Label, Started: time.Now()}
		part, err := d.Sign(ctx, built.PSBT)
		r.Elapsed = time.Since(r.Started)
		if err != nil {
			r.Err = err
			m.Rounds = append(m.Rounds, r)
			continue
		}
		if part.Label == "" {
			part.Label = d.Label
		}
		parts = append(parts, part)
		m.Rounds = append(m.Rounds, r)
	}
	m.Signing = time.Since(signingStart)

	if m.failures() > 0 {
		m.Total = time.Since(started)
		return m, nil
	}

	merged, err := combine.Merge(built.Raw, parts)
	if err != nil {
		m.Total = time.Since(started)
		return m, fmt.Errorf("combining the rehearsal's partials: %w", err)
	}
	final, err := combine.Finalize(merged)
	if err != nil {
		m.Total = time.Since(started)
		return m, fmt.Errorf("finalizing the rehearsal transaction in-app: %w", err)
	}
	if final.TxID != built.TxID {
		m.Total = time.Since(started)
		return m, fmt.Errorf("%w: %s became %s. On the real batch that is every "+
			"channel in it lost, because LND commits to the funding outpoint at "+
			"psbt_verify", ErrTxIDMoved, built.TxID, final.TxID)
	}
	m.Decoy.VsizeVB = final.Vsize

	accepted, why, err := acceptable(ctx, req.Node, final.RawTx)
	if err != nil {
		m.Total = time.Since(started)
		return m, err
	}
	m.Accepted, m.RejectReason = accepted, why
	m.Total = time.Since(started)
	return m, nil
}

// buildDecoy creates the throwaway transaction and returns the release for the
// coins Core locked while building it.
func buildDecoy(ctx context.Context, req Request) (coldwallet.Built, func(), error) {
	outputs := make([]coldwallet.Output, 0, len(req.MirrorSat))
	for i, amount := range req.MirrorSat {
		if amount <= 0 {
			return coldwallet.Built{}, nil, fmt.Errorf("output %d of the batch is %d sat, "+
				"which is not an amount to mirror", i+1, amount)
		}
		// A fresh receive address per output, not one address reused: Core
		// collapses two outputs paying the same address into one, which would
		// give the devices a different transaction from the one the batch will
		// have.
		addr, err := freshAddress(ctx, req.Wallet)
		if err != nil {
			return coldwallet.Built{}, nil, err
		}
		outputs = append(outputs, coldwallet.Output{Address: addr, AmountSat: amount})
	}

	change, err := coldwallet.ChangeAddress(ctx, req.Wallet)
	if err != nil {
		return coldwallet.Built{}, nil, err
	}

	built, err := coldwallet.Build(ctx, req.Wallet, coldwallet.BuildRequest{
		Outputs:          outputs,
		ChangeAddress:    change,
		FeeRateSatPerVB:  req.FeeRateSatPerVB,
		MinConfirmations: req.MinConfirmations,
	})
	if err != nil {
		return coldwallet.Built{}, nil, fmt.Errorf("building the rehearsal's decoy "+
			"transaction: %w\nThis is the same builder the batch uses, so a failure "+
			"here is a failure the batch would have had", err)
	}

	release := func() {
		// Its own context: the caller's may already have been cancelled, and
		// leaving the batch's coins locked is worse than any reason for that.
		c, done := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer done()
		_, _ = req.Wallet.ReleaseLocks(c, built.Inputs)
	}
	return built, release, nil
}

// freshAddress asks the watch-only cold wallet for one of its own receive
// addresses.
//
// The decoy pays back into cold storage rather than anywhere else, which is what
// makes it harmless if it ever did escape — and what makes the operator's
// devices show a screenful of their own addresses, which is itself a check.
func freshAddress(ctx context.Context, wallet *bitcoind.Client) (string, error) {
	var addr string
	if err := wallet.Call(ctx, "getnewaddress", nil, &addr); err != nil {
		return "", fmt.Errorf("asking the cold wallet for an address to rehearse "+
			"against: %w", err)
	}
	if addr == "" {
		return "", fmt.Errorf("the cold wallet returned an empty address")
	}
	return addr, nil
}

// acceptable runs testmempoolaccept, which validates without relaying.
func acceptable(ctx context.Context, node *bitcoind.Client, rawTx []byte) (bool, string, error) {
	ok, reason, _, err := node.TestMempoolAccept(ctx, hex.EncodeToString(rawTx))
	if err != nil {
		return false, "", fmt.Errorf("offering the rehearsal transaction to %w", err)
	}
	return ok, reason, nil
}

func sum(v []int64) int64 {
	var total int64
	for _, n := range v {
		total += n
	}
	return total
}
