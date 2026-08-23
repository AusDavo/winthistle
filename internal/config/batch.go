package config

import (
	"fmt"
	"strings"

	"github.com/AusDavo/winthistle/internal/arm"
	"github.com/AusDavo/winthistle/internal/peers"
	"github.com/AusDavo/winthistle/internal/policy"
)

// Batch is the file that says where the money goes.
//
// It is separate from winthistle.toml because it changes every time and that
// one does not, and because it is the artifact worth reviewing: peers, amounts,
// and what each channel will charge to route. The plan document Phase 0 emits
// is this file plus the things only LND and Core can supply — the funding
// addresses, the coin set, the fee rate and the reserve top-up — and the
// verifier then holds the returned transaction to it.
//
// # Why the policy is in here
//
// Because it is the same decision. A channel's capacity and its forwarding
// policy are chosen together or the second one is not chosen at all: LND opens
// every channel at 1000 msat base and 1 ppm, so a batch file that named only
// the amounts would be a batch file that silently chose free routing. The
// [policy] block is the default and each [[channel]] may override any of its
// keys, so the common case is three lines at the top and nothing per channel.
type Batch struct {
	Path string

	// Default is [policy], which every channel starts from.
	Default policy.Policy

	Channels []Channel
}

// Channel is one member of the batch.
type Channel struct {
	// Peer is the peer's identity key, hex-encoded and compressed.
	Peer string

	// Host is "host:port", used only if LND is not already connected. The graph
	// often carries an address, so it is optional.
	Host string

	AmountSat int64

	// Private makes the channel unannounced. It matters past privacy: an
	// unannounced channel is invisible to LND's anchor-reserve check twice over
	// — see internal/reserve — and it will not route for anyone.
	Private bool

	// Policy is the default with this channel's overrides applied.
	Policy policy.Policy
}

var batchSections = map[string]bool{"policy": true, "channel": true}

// LoadBatch reads a batch file.
func LoadBatch(path string) (*Batch, error) {
	doc, err := parseFile(path)
	if err != nil {
		return nil, err
	}
	b := &Batch{Path: path}
	var errs []string
	fail := func(err error) {
		if err != nil {
			errs = append(errs, err.Error())
		}
	}

	b.Default, err = readPolicy(path, doc.section("policy"), PolicyDefaults())
	fail(err)

	for _, t := range doc.array("channel") {
		var ch Channel
		ch.Peer, err = t.str(path, "peer", "")
		fail(err)
		ch.Host, err = t.str(path, "host", "")
		fail(err)
		ch.AmountSat, err = t.integer(path, "amount_sat", 0)
		fail(err)
		ch.Private, err = t.boolean(path, "private", false)
		fail(err)
		ch.Policy, err = readPolicy(path, t, b.Default)
		fail(err)

		if err := ch.check(path, t.line); err != nil {
			errs = append(errs, err.Error())
		}
		b.Channels = append(b.Channels, ch)
	}

	if len(b.Channels) == 0 {
		errs = append(errs, fmt.Sprintf("%s has no [[channel]] in it, so there is "+
			"no batch to open", path))
	}
	seen := map[string]int{}
	for i, ch := range b.Channels {
		if prev, dup := seen[ch.Peer]; dup && ch.Peer != "" {
			errs = append(errs, fmt.Sprintf("channels %d and %d both open to %s. "+
				"Two channels to one peer in one batch is legal, and it is also the "+
				"shape a copy-paste error takes — say so deliberately by running two "+
				"batches, or delete one", prev+1, i+1, short(ch.Peer)))
		}
		seen[ch.Peer] = i
	}

	errs = append(errs, doc.unknown(batchSections)...)
	if len(errs) > 0 {
		return nil, &Invalid{Path: path, Problems: errs}
	}
	return b, nil
}

// readPolicy reads the policy keys out of a table, starting from a default.
//
// The same keys are read from [policy] and from each [[channel]], which is what
// makes an override an override rather than a second vocabulary.
func readPolicy(path string, t *table, def policy.Policy) (policy.Policy, error) {
	p := def
	var errs []string
	fail := func(err error) {
		if err != nil {
			errs = append(errs, err.Error())
		}
	}

	base, err := t.integer(path, "base_fee_msat", p.BaseFeeMsat)
	fail(err)
	p.BaseFeeMsat = base

	rate, err := t.integer(path, "fee_rate_ppm", int64(p.FeeRatePPM))
	fail(err)
	p.FeeRatePPM = uint32(rate)

	delta, err := t.integer(path, "time_lock_delta", int64(p.TimeLockDelta))
	fail(err)
	p.TimeLockDelta = uint32(delta)

	if t.has("min_htlc_msat") {
		min, err := t.integer(path, "min_htlc_msat", 0)
		fail(err)
		// A pointer because LND distinguishes "not specified" from zero with its
		// own flag, and specifying zero is a different policy from leaving the
		// existing minimum alone.
		p.MinHTLCMsat = &min
	}
	max, err := t.integer(path, "max_htlc_msat", int64(p.MaxHTLCMsat))
	fail(err)
	p.MaxHTLCMsat = uint64(max)

	inBase, err := t.integer(path, "inbound_base_fee_msat", int64(p.InboundBaseFeeMsat))
	fail(err)
	p.InboundBaseFeeMsat = int32(inBase)

	inRate, err := t.integer(path, "inbound_fee_rate_ppm", int64(p.InboundFeeRatePPM))
	fail(err)
	p.InboundFeeRatePPM = int32(inRate)

	if len(errs) > 0 {
		return p, fmt.Errorf("%s", strings.Join(errs, "\n  - "))
	}
	// Refused here rather than at Phase 2. UpdateChannelPolicy reports an
	// invalid parameter inside a *successful* response, and by then the
	// transaction is public and the channel routes at 1 ppm until a human
	// notices — see internal/settle.
	if err := p.Validate(); err != nil {
		return p, err
	}
	return p, nil
}

func (ch Channel) check(path string, line int) error {
	if err := peers.ValidatePubkey(ch.Peer); err != nil {
		return fmt.Errorf("%s: peer: %w", where(path, line), err)
	}
	if ch.AmountSat <= 0 {
		return fmt.Errorf("%s: amount_sat is %d, which is not an amount",
			where(path, line), ch.AmountSat)
	}
	return nil
}

// Wants is the peer pre-flight's view of this batch.
func (b *Batch) Wants() []peers.Want {
	out := make([]peers.Want, 0, len(b.Channels))
	for _, ch := range b.Channels {
		out = append(out, peers.Want{
			Pubkey: ch.Peer, Host: ch.Host,
			AmountSat: ch.AmountSat, Private: ch.Private,
		})
	}
	return out
}

// ArmChannels is the armed window's view of this batch.
//
// The policy is copied per channel rather than pointed at the slice element,
// because the pointer outlives this loop: arm.Channel carries it into
// arm.Stream, into the plan document, and into Phase 2's settle.Member.
func (b *Batch) ArmChannels() []arm.Channel {
	out := make([]arm.Channel, 0, len(b.Channels))
	for _, ch := range b.Channels {
		p := ch.Policy
		out = append(out, arm.Channel{
			Peer: ch.Peer, AmountSat: ch.AmountSat, Private: ch.Private, Policy: &p,
		})
	}
	return out
}

// TotalSat is what the channels come to, before fee, change and any top-up.
func (b *Batch) TotalSat() int64 {
	var n int64
	for _, ch := range b.Channels {
		n += ch.AmountSat
	}
	return n
}

// AmountsSat are the channel capacities in order, which is what the dress
// rehearsal mirrors so a decoy costs the devices what the batch will.
func (b *Batch) AmountsSat() []int64 {
	out := make([]int64, 0, len(b.Channels))
	for _, ch := range b.Channels {
		out = append(out, ch.AmountSat)
	}
	return out
}

func short(pubkey string) string {
	if len(pubkey) <= 12 {
		return pubkey
	}
	return pubkey[:8] + "…"
}

// ExampleBatch is what `winthistle doctor` prints when asked for one.
const ExampleBatch = `# Every channel starts from this policy and may override any key of it.
# These are LND's own defaults, which is what a channel routes at if nothing
# is applied — 1000 msat base and 1 ppm is close to free on a large channel,
# so this block is worth an argument rather than a copy.
[policy]
base_fee_msat   = 1000
fee_rate_ppm    = 1
time_lock_delta = 80

[[channel]]
peer       = "02aaaa...33 bytes of compressed pubkey, hex..."
host       = "203.0.113.10:9735"    # only used if we are not connected already
amount_sat = 5_000_000
fee_rate_ppm = 250                  # this channel only

[[channel]]
peer       = "03bbbb..."
amount_sat = 2_000_000
private    = true
`
