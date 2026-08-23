package peers

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/AusDavo/winthistle/internal/abort"
	"github.com/AusDavo/winthistle/internal/arm"
)

// Kind classifies a peer's refusal.
//
// The classification exists because the operator's next move differs for each,
// and because two of these are self-inflicted: TooManyPending is very often our
// own earlier probe, and Internal means the peer decided not to tell us.
type Kind int

const (
	// Unknown: the peer refused with something this build does not recognise.
	// The raw text is kept and shown; a peer that is not LND can say anything.
	Unknown Kind = iota

	// TooSmall: below the peer's min chan size, and the peer named the figure.
	TooSmall

	// TooLarge: above the peer's max chan size, and the peer named the figure.
	TooLarge

	// TooManyPending: the peer is already holding as many pending channels from
	// us as it will. lnwire.ErrMaxPendingChannels.
	TooManyPending

	// Internal: LND's generic refusal. failFundingFlow only forwards the real
	// text for ReservationError, FundingError and ChanAcceptError; everything
	// else becomes "funding failed due to internal error", so the reason exists
	// but stayed on the peer's machine.
	Internal

	// NotConnected: our own node could not reach the peer, so the question was
	// never asked. Costs nothing and is not the peer's answer.
	NotConnected
)

func (k Kind) String() string {
	switch k {
	case TooSmall:
		return "below the peer's minimum"
	case TooLarge:
		return "above the peer's maximum"
	case TooManyPending:
		return "the peer is holding too many pending channels from us"
	case Internal:
		return "the peer refused without saying why"
	case NotConnected:
		return "not connected"
	default:
		return "refused, reason unrecognised"
	}
}

// Rejection is a peer's refusal, unwrapped.
type Rejection struct {
	// Raw is exactly what LND handed us, kept because a peer that is not LND
	// can refuse in words this build has never seen.
	Raw string

	// Peer is what the peer itself said, with LND's two layers of prefix
	// removed. See Unwrap for why that matters.
	Peer string

	Kind Kind

	// MinChanSat and MaxChanSat are the peer's own figures when it named them.
	MinChanSat int64
	MaxChanSat int64
}

// Probe is one shim probe and what it cost.
type Probe struct {
	Pubkey    string
	AmountSat int64

	// Accepted means accept_channel arrived and LND got as far as naming a
	// funding address. That is the authoritative "this peer will take this
	// channel at this size".
	Accepted bool

	// FundingAddress is what LND asked to be paid, when accepted. It is thrown
	// away with the shim; it is here only so the report can show that the peer
	// really did answer.
	FundingAddress string

	// Rejection is the peer's answer when it said no.
	Rejection Rejection

	Started  time.Time
	Answered time.Time

	// HoldsUntil is when this peer's reservation can be assumed gone.
	//
	// Zero for a refused probe, which costs nothing: every limit check in
	// handleFundingOpen runs before InitChannelReservation, so a refusal leaves
	// no reservation behind. Set for an accepted one, because shim_cancel is
	// local — CancelFundingIntent deletes our own map entry and sends the peer
	// nothing — so only the peer's zombie sweeper releases it.
	HoldsUntil time.Time

	// Cancelled is whether the shim was taken down on our side. It is always
	// attempted; CancelErr says what happened if it failed.
	Cancelled bool
	CancelErr error
}

// Held reports whether the peer is still assumed to be holding a reservation
// from this probe.
func (p Probe) Held(now time.Time) bool {
	return !p.HoldsUntil.IsZero() && now.Before(p.HoldsUntil)
}

// Remaining is how long until this peer is clear, or zero.
func (p Probe) Remaining(now time.Time) time.Duration {
	if !p.Held(now) {
		return 0
	}
	return p.HoldsUntil.Sub(now)
}

// ErrProbeIncomplete means the stream neither produced psbt_fund nor failed, so
// the probe learned nothing.
var ErrProbeIncomplete = errors.New("the funding stream neither answered nor failed")

// Do runs one shim probe: open a funding stream to the peer at that size, read
// whichever of accept_channel or a refusal comes back, and cancel the shim.
//
// This is arm.Open with the answer thrown away. It is the same RPC, the same
// no_publish shim and the same reservation, which is exactly why a batch that is
// ready to arm should arm rather than probe: arm.Open asks the same question and
// keeps the answer. See the package comment.
//
// The shim is cancelled whatever happened, including on the accepted path, and
// especially on it: an uncancelled intent is one this node keeps a coin lock and
// a map entry for. Cancelling does not release the *peer's* reservation, which
// is what HoldsUntil is about.
func Do(ctx context.Context, cli arm.Client, chain string, w Want) (Probe, error) {
	if err := ValidatePubkey(w.Pubkey); err != nil {
		return Probe{}, fmt.Errorf("peer %q: %w", w.Pubkey, err)
	}

	p := Probe{Pubkey: w.Pubkey, AmountSat: w.AmountSat, Started: time.Now()}

	streams, err := arm.Open(ctx, cli, chain, []arm.Channel{w.Channel()})
	p.Answered = time.Now()

	if err != nil {
		// arm.Open hands back the streams that did open alongside the error, and
		// for a one-channel probe there are none — but cancel whatever is there
		// rather than assuming, because the cost of assuming is an orphaned
		// intent nothing on disk knows about.
		p.cancelAll(ctx, cli, streams)
		p.Rejection = Unwrap(err)
		return p, nil
	}
	if len(streams.All) != 1 {
		p.cancelAll(ctx, cli, streams)
		return p, fmt.Errorf("probing %s: %w", short(w.Pubkey), ErrProbeIncomplete)
	}

	st := streams.All[0]
	p.Accepted = true
	p.FundingAddress = st.FundingAddress
	// The peer sent accept_channel, so it created a reservation, and only its own
	// sweeper will take that back.
	p.HoldsUntil = p.Answered.Add(HoldUpperBound)
	p.cancelAll(ctx, cli, streams)
	return p, nil
}

// cancelAll takes down every shim the probe opened, and hangs up the streams.
func (p *Probe) cancelAll(ctx context.Context, cli arm.Client, streams *arm.Streams) {
	if streams == nil {
		return
	}
	defer streams.Close()
	for _, st := range streams.All {
		err := abort.CancelShim(ctx, cli, st.PendingChanID)
		switch {
		case err == nil, errors.Is(err, abort.ErrNoShim):
			p.Cancelled = true
		default:
			p.CancelErr = err
		}
	}
}

// DoAll probes several peers in turn.
//
// Sequentially, and that is not laziness: each probe that succeeds starts a
// clock on its peer, and running them in parallel would only make the clocks
// start closer together. What matters is the latest of them, which ReadyToArm
// reports.
//
// A probe that errors outright — the RPC could not be made — stops the run,
// because that is a fault in this node rather than an answer about a peer.
func DoAll(ctx context.Context, cli arm.Client, chain string, wants []Want) ([]Probe, error) {
	out := make([]Probe, 0, len(wants))
	for _, w := range wants {
		p, err := Do(ctx, cli, chain, w)
		if err != nil {
			return out, err
		}
		out = append(out, p)
	}
	return out, nil
}

// ErrPeersStillHolding means at least one probed peer is still inside the window
// in which it is assumed to be holding the reservation that probe created.
var ErrPeersStillHolding = errors.New("a probed peer is still holding its reservation")

// ReadyToArm is the gate between probing and arming.
//
// Against a peer running LND's default --maxpendingchannels of 1, arming inside
// this window is refused with "Number of pending channels exceed maximum" — on
// the clock, for a reason we created ourselves. The peer's limit is not
// observable from here, so this refuses on the conservative reading rather than
// on the likely one.
//
// It is a gate the operator can decline: a peer known to allow several pending
// channels is not blocked by this, and the report says so rather than pretending
// the wait is a law.
func ReadyToArm(probes []Probe, now time.Time) error {
	var (
		waiting []string
		longest time.Duration
	)
	for _, p := range probes {
		if !p.Held(now) {
			continue
		}
		waiting = append(waiting, fmt.Sprintf("%s (%s)", short(p.Pubkey),
			roundSeconds(p.Remaining(now))))
		if d := p.Remaining(now); d > longest {
			longest = d
		}
	}
	if len(waiting) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s. Arming now risks \"Number of pending channels "+
		"exceed maximum\" from %s running LND's default of one. Wait %s, or arm "+
		"anyway if you know these peers allow more: %w",
		ErrPeersStillHolding, strings.Join(waiting, ", "),
		anyPeer(len(waiting)), roundSeconds(longest), ErrPeersStillHolding)
}

// ---------------------------------------------------------------------------
// Reading the refusal
// ---------------------------------------------------------------------------

// LND's two layers of prefix on a peer's funding error.
//
// The first is the misleading one. handleErrorMsg wraps *every* peer error in
// chanfunding.ErrRemoteCanceled when the reservation is a PSBT one — "if this
// was a PSBT funding flow, the remote likely timed out because we waited too
// long" — so a peer that refused instantly because the channel was too small
// arrives claiming it possibly timed out. It did not. Stripping this is the
// whole reason Unwrap exists.
const (
	remoteCanceled = "remote canceled funding, possibly timed out: "
	genericPeerErr = "funding failed due to internal error"
)

var (
	// "received funding error from <66 hex chars>: "
	fromPeer = regexp.MustCompile(`received funding error from [0-9a-fA-F]{66}: `)

	// lnwallet.ErrChanTooSmall / ErrChanTooLarge, which render their amounts
	// through btcutil.Amount.String() — BTC with trailing zeros trimmed.
	tooSmall = regexp.MustCompile(
		`chan size of ([0-9.]+) BTC is below min chan size of ([0-9.]+) BTC`)
	tooLarge = regexp.MustCompile(
		`chan size of ([0-9.]+) BTC exceeds maximum chan size of ([0-9.]+) BTC`)
)

// Unwrap turns the error a funding stream failed with into a Rejection.
//
// The peer's own words survive the trip — failFundingFlow forwards the real text
// for lnwallet.ReservationError, lnwire.FundingError and
// chanacceptor.ChanAcceptError — but they arrive under two prefixes, one of
// which asserts a timeout that did not happen. Reporting the raw string would
// tell an operator their peer timed out when in fact it named its minimum in the
// same sentence.
func Unwrap(err error) Rejection {
	if err == nil {
		return Rejection{}
	}
	r := Rejection{Raw: err.Error()}

	peer := r.Raw
	if i := strings.Index(peer, remoteCanceled); i >= 0 {
		peer = peer[i+len(remoteCanceled):]
	}
	if loc := fromPeer.FindStringIndex(peer); loc != nil {
		peer = peer[loc[1]:]
	}
	r.Peer = strings.TrimSpace(peer)

	switch {
	case tooSmall.MatchString(r.Peer):
		m := tooSmall.FindStringSubmatch(r.Peer)
		r.Kind = TooSmall
		r.MinChanSat = satFromBTCString(m[2])
	case tooLarge.MatchString(r.Peer):
		m := tooLarge.FindStringSubmatch(r.Peer)
		r.Kind = TooLarge
		r.MaxChanSat = satFromBTCString(m[2])
	case strings.Contains(r.Peer, "Number of pending channels exceed maximum"):
		r.Kind = TooManyPending
	case strings.Contains(r.Peer, genericPeerErr):
		r.Kind = Internal
	case notConnected(r.Raw):
		// The question never reached a peer, so there is no peer sentence to
		// report. Clearing it is what stops the report attributing our own
		// node's error to somebody else's policy.
		r.Kind, r.Peer = NotConnected, ""
	default:
		r.Kind = Unknown
	}
	return r
}

// notConnected recognises the refusals that come from our own node because the
// peer is not reachable.
//
// "peer <pubkey> is not online" is rpcserver.go's wording when OpenChannel is
// asked about a peer with no live connection, and it arrives with the pubkey
// spliced into the middle — so the match is on the tail, not on a fixed phrase.
func notConnected(raw string) bool {
	for _, phrase := range []string{
		"is not online",
		"unable to connect to",
		"not connected to peer",
	} {
		if strings.Contains(raw, phrase) {
			return true
		}
	}
	return false
}

// satFromBTCString parses "0.0002" into satoshis without a float.
//
// btcutil.Amount.String() renders BTC with trailing zeros trimmed, so the string
// can carry anywhere from zero to eight decimal places. Parsing it as a float
// and multiplying would be the ordinary mistake; this is a peer's stated minimum
// and it goes straight into a comparison against a channel amount.
func satFromBTCString(s string) int64 {
	whole, frac, _ := strings.Cut(s, ".")
	if len(frac) > 8 {
		frac = frac[:8]
	}
	frac += strings.Repeat("0", 8-len(frac))
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0
	}
	f, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0
	}
	return w*1e8 + f
}

func short(pubkey string) string {
	if len(pubkey) <= 16 {
		return pubkey
	}
	return pubkey[:16] + "…"
}

func anyPeer(n int) string {
	if n == 1 {
		return "a peer"
	}
	return "peers"
}

// roundSeconds keeps a countdown readable: whole seconds, never a nanosecond
// tail in a sentence an operator is reading while deciding whether to wait.
func roundSeconds(d time.Duration) time.Duration { return d.Round(time.Second) }
