// Package peers is Phase 0's peer pre-flight: everything that can be learned
// about a peer before a clock starts, and the one thing that cannot.
//
// # Three tiers, and only the third is authoritative
//
//  1. The pubkey itself. Thirty-three bytes, compressed, a real point on
//     secp256k1. Local, instant, and it catches the whole family of typos and
//     truncations.
//  2. LND's own gossip graph, through GetNodeInfo. An alias, addresses, the
//     peer's channel count and total capacity, and — the useful part — the
//     capacities of the channels it already has. There is no gossip field for
//     "minimum channel size", so the smallest and median of those capacities are
//     an empirical proxy and nothing stronger. No third party is asked: CLAUDE.md
//     forbids a Lightning explorer, and the graph is already on the node.
//  3. accept_channel. The peer's minimum, its reserve, its HTLC limits and its
//     accepted commitment type are enforced conversationally and are not
//     published anywhere. The only way to know them is to ask, and asking means
//     opening a funding stream.
//
// # The probe is not free, and the design says it is
//
// docs/design.html calls the shim probe "free and abortable". It is free of fees
// and free of risk — nothing is signed, nothing is broadcast, and shim_cancel
// still works — but it is not free of the peer's patience, and that turns out to
// matter more.
//
// Reading handleFundingOpen at v0.19.3-beta: before the peer creates anything it
// counts its live reservations for us plus its pending channels with no thaw
// height, and refuses with ErrMaxPendingChannels if that count is already at
// --maxpendingchannels. LND's default for that flag is 1. Then, if it accepts,
// it creates a reservation and sends accept_channel.
//
// Two consequences, in opposite directions:
//
//   - A probe the peer REFUSES costs nothing. Every limit check — max chan size,
//     min chan size, the channel acceptor — runs before InitChannelReservation,
//     so a refused probe leaves no reservation behind and the next attempt is
//     unencumbered. The cheap answer is the one that costs nothing to get.
//   - A probe the peer ACCEPTS costs one of that peer's pending-channel slots,
//     and shim_cancel does not give it back. CancelFundingIntent
//     (lnwallet/wallet.go) deletes an entry in our own wallet's map and sends the
//     peer nothing at all. The peer's own zombie sweeper releases it, after
//     DefaultReservationTimeout with up to DefaultZombieSweeperInterval of
//     slack — ten minutes plus one.
//
// So a Phase 0 that probes all n peers and then immediately arms is a Phase 0
// that collides with its own probes: against a peer running LND's default of
// one pending channel, step 2 is refused with "Number of pending channels exceed
// maximum", on the clock, for a reason we caused ourselves a minute earlier.
//
// # What that means for the flow
//
// The probe starts a clock. Phase 0 was defined as the phase with no clock, so
// the probe does not belong inside an ordinary run:
//
//   - Do not probe as part of arming. A successful probe *is* step 2 with the
//     answer thrown away — same RPC, same reservation, same ten minutes. If the
//     batch is ready to arm, arm it: arm.Open asks the same question and keeps
//     the answer.
//   - Probe as a separate, earlier act, when a peer's minimum is genuinely
//     unknown and the graph proxy is not good enough. Then wait out HoldsUntil
//     before arming. ReadyToArm is the gate.
//   - A refused probe is free, so re-probing downwards to find a peer's minimum
//     costs nothing at all. It is only the accepted one that has to be the last.
//
// All source citations are against lnd v0.19.3-beta.
package peers

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/AusDavo/winthistle/internal/arm"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/lnrpc"
	"google.golang.org/grpc"
)

// The peer's clock, from LND's own constants.
//
// chanfunding.DefaultReservationTimeout is how long a reservation may sit
// unchanged; lncfg.DefaultZombieSweeperInterval is how often the sweeper looks.
// Neither is adjustable in a release build — lncfg/dev.go returns the constant
// and only a dev-tagged build reads the flags — so the hold is bounded by their
// sum, and TestWhoOwnsTheTenMinuteClock measured 10m41s against bob.
//
// Stated here rather than imported because they describe the *peer's* build, not
// ours, and a peer that is not LND is bound by neither.
const (
	ReservationTimeout = 10 * time.Minute
	SweeperInterval    = 1 * time.Minute

	// HoldUpperBound is how long to assume a peer holds a reservation we opened
	// and then cancelled.
	HoldUpperBound = ReservationTimeout + SweeperInterval
)

// Client is the read-and-connect slice of LND this pre-flight uses.
//
// Narrow for the reason internal/reserve's and internal/plan's are: none of
// these three can move a coin, open a stream or publish anything, and the type
// is where that is said. The probe takes a different client — arm.Client — and
// that separation is the point, because the probe is the part that costs
// something.
type Client interface {
	ConnectPeer(ctx context.Context, in *lnrpc.ConnectPeerRequest,
		opts ...grpc.CallOption) (*lnrpc.ConnectPeerResponse, error)

	ListPeers(ctx context.Context, in *lnrpc.ListPeersRequest,
		opts ...grpc.CallOption) (*lnrpc.ListPeersResponse, error)

	GetNodeInfo(ctx context.Context, in *lnrpc.NodeInfoRequest,
		opts ...grpc.CallOption) (*lnrpc.NodeInfo, error)

	PendingChannels(ctx context.Context, in *lnrpc.PendingChannelsRequest,
		opts ...grpc.CallOption) (*lnrpc.PendingChannelsResponse, error)
}

// Want is one peer the batch means to open to.
type Want struct {
	// Pubkey is the peer's identity key, hex-encoded and compressed.
	Pubkey string

	// Host is "host:port", used only if we are not already connected. It may be
	// empty when the peer is connected or when the graph carries an address.
	Host string

	// AmountSat is the capacity this batch would ask for, which is what the
	// graph comparison and the probe are about.
	AmountSat int64

	// Private makes the channel unannounced. Carried through so that the
	// reserve batch and the arm channel can be built from the same list.
	Private bool
}

// Channel renders this want as the armed window's channel.
func (w Want) Channel() arm.Channel {
	return arm.Channel{Peer: w.Pubkey, AmountSat: w.AmountSat, Private: w.Private}
}

// Connection is what happened when we tried to reach the peer.
type Connection int

const (
	// NotTried: no host was given and the peer was not already connected.
	NotTried Connection = iota

	// AlreadyConnected: LND already had a live connection.
	AlreadyConnected

	// Connected: ConnectPeer succeeded.
	Connected

	// Unreachable: ConnectPeer failed.
	Unreachable
)

func (c Connection) String() string {
	switch c {
	case NotTried:
		return "not tried"
	case AlreadyConnected:
		return "already connected"
	case Connected:
		return "connected"
	case Unreachable:
		return "unreachable"
	default:
		return "unknown"
	}
}

// PendingOpen is a channel this node already has pending open with a peer the
// batch means to open to.
//
// It is here because of what the peer does with it. handleFundingOpen counts its
// live reservations for us *plus* its pending channels with no thaw height, and
// refuses with ErrMaxPendingChannels once that count is at
// --maxpendingchannels, whose default is 1. So one channel already pending with
// a peer is, against a default peer, the whole of its budget for us — and the
// batch's step 2 is refused on the clock, with the cold wallet out, for a reason
// that was sitting in local state the entire time.
type PendingOpen struct {
	// ChannelPoint is the funding outpoint, as LND prints it.
	ChannelPoint string

	CapacitySat int64

	// Ours is whether this node initiated the channel. Not a filter: the peer
	// counts a pending channel against its limit whoever opened it. It is
	// carried because it changes what the operator should do about it — one we
	// opened may be an earlier run of this tool that did not finish.
	Ours bool

	Private bool
}

// Facts is everything the local graph and one connection attempt can say about
// a peer. None of it is authoritative about what the peer will accept.
type Facts struct {
	Want Want

	// KeyOK is whether the pubkey is a real compressed point on secp256k1, and
	// KeyProblem says why not.
	KeyOK      bool
	KeyProblem string

	Connection    Connection
	ConnectDetail string

	// InGraph is whether LND's own gossip graph has heard of this node at all.
	// A peer absent from the graph is not necessarily a bad peer — a brand new
	// node, or one with only private channels, looks exactly like this — but it
	// is a peer nothing below this line can be said about.
	InGraph bool
	Alias   string

	NumChannels      uint32
	TotalCapacitySat int64

	// SmallestSat and MedianSat are the capacities of the channels this node
	// already has, and they are the whole of the graph-derived proxy for what it
	// will accept. There is no gossip field for a minimum channel size.
	SmallestSat int64
	MedianSat   int64

	Addresses  []string
	LastUpdate time.Time

	// Pending are the channels this node already has pending open with this
	// peer, before the batch adds any. Free to read and, unlike everything else
	// about what a peer will accept, not a proxy for anything — it is a fact
	// about our own node. See HasCompetingOpen for what it is evidence of.
	Pending []PendingOpen
}

// Usable reports whether this peer can be armed against at all: the key parses
// and LND is connected to it.
func (f Facts) Usable() bool {
	return f.KeyOK && (f.Connection == Connected || f.Connection == AlreadyConnected)
}

// BelowSmallest reports whether the batch's amount is under the smallest channel
// this peer already has. That is a hint and never a verdict — a peer's smallest
// existing channel is evidence about its policy, not a statement of it.
func (f Facts) BelowSmallest() bool {
	return f.InGraph && f.SmallestSat > 0 && f.Want.AmountSat < f.SmallestSat
}

// HasCompetingOpen reports whether this node already has a channel pending open
// with this peer.
//
// A warning and never a refusal, for the same reason BelowSmallest is: the
// peer's --maxpendingchannels is its own local configuration and is published
// nowhere, so a peer that allows several will take this batch quite happily and
// a hard gate here would refuse a batch that works.
//
// It has a blind spot worth stating, because it points the wrong way. This reads
// *our* view of what is pending, and AbandonChannel is local-only — a channel
// this node abandoned is gone from here while the peer still counts it, until
// 2016 blocks pass from its funding height. So an empty answer is weaker than a
// non-empty one: this can tell you that a slot is taken and cannot tell you that
// one is free.
func (f Facts) HasCompetingOpen() bool { return len(f.Pending) > 0 }

// Check runs the free tiers for every peer: validate the key, connect if we are
// not already, and read the local graph.
//
// It opens no funding stream, so it starts no clock and costs nothing to run
// again. Errors that belong to one peer are recorded on that peer's Facts rather
// than returned, because a batch with one unreachable peer is a batch the
// operator should be shown whole.
func Check(ctx context.Context, cli Client, wants []Want) ([]Facts, error) {
	if len(wants) == 0 {
		return nil, fmt.Errorf("no peers to check")
	}

	connected, err := connectedSet(ctx, cli)
	if err != nil {
		return nil, err
	}
	pending, err := pendingByPeer(ctx, cli)
	if err != nil {
		return nil, err
	}

	out := make([]Facts, 0, len(wants))
	for _, w := range wants {
		out = append(out, check(ctx, cli, w, connected, pending))
	}
	return out, nil
}

func check(ctx context.Context, cli Client, w Want, connected map[string]bool,
	pending map[string][]PendingOpen) Facts {

	f := Facts{Want: w}

	if err := ValidatePubkey(w.Pubkey); err != nil {
		f.KeyProblem = err.Error()
		return f
	}
	f.KeyOK = true
	// Before the connection attempt and before the graph, because both of those
	// have early returns and this fact is available for a peer that is neither
	// reachable nor in the graph. It is about our node, not theirs.
	f.Pending = pending[strings.ToLower(w.Pubkey)]

	switch {
	case connected[strings.ToLower(w.Pubkey)]:
		f.Connection = AlreadyConnected
	case w.Host == "":
		f.Connection = NotTried
		f.ConnectDetail = "no host given, and LND is not already connected"
	default:
		_, err := cli.ConnectPeer(ctx, &lnrpc.ConnectPeerRequest{
			Addr: &lnrpc.LightningAddress{Pubkey: w.Pubkey, Host: w.Host},
			// Not perm: a permanent connection makes LND keep retrying this peer
			// forever, which is a lasting change to the node's behaviour made by
			// a pre-flight. Phase 0 must be repeatable and must leave nothing.
			Perm: false,
		})
		switch {
		case err == nil:
			f.Connection = Connected
		case isAlreadyConnected(err):
			// A race with the ListPeers snapshot, or another process. Either way
			// the end state is the one we wanted.
			f.Connection = AlreadyConnected
		default:
			f.Connection = Unreachable
			f.ConnectDetail = err.Error()
		}
	}

	info, err := cli.GetNodeInfo(ctx, &lnrpc.NodeInfoRequest{
		PubKey:          w.Pubkey,
		IncludeChannels: true,
	})
	if err != nil {
		// "unable to find node" is the ordinary answer for a node the graph has
		// not heard of, and it is not a failure of this pre-flight.
		f.InGraph = false
		return f
	}
	f.InGraph = true
	f.Alias = info.GetNode().GetAlias()
	f.NumChannels = info.GetNumChannels()
	f.TotalCapacitySat = info.GetTotalCapacity()
	if t := info.GetNode().GetLastUpdate(); t > 0 {
		f.LastUpdate = time.Unix(int64(t), 0).UTC()
	}
	for _, a := range info.GetNode().GetAddresses() {
		f.Addresses = append(f.Addresses, a.GetAddr())
	}
	f.SmallestSat, f.MedianSat = capacityStats(info.GetChannels())
	return f
}

// capacityStats reduces the peer's existing channels to the two figures that
// say anything about its policy.
func capacityStats(edges []*lnrpc.ChannelEdge) (smallest, median int64) {
	if len(edges) == 0 {
		return 0, 0
	}
	caps := make([]int64, 0, len(edges))
	for _, e := range edges {
		if c := e.GetCapacity(); c > 0 {
			caps = append(caps, c)
		}
	}
	if len(caps) == 0 {
		return 0, 0
	}
	sort.Slice(caps, func(i, j int) bool { return caps[i] < caps[j] })
	return caps[0], caps[len(caps)/2]
}

// connectedSet reads which peers LND currently has a connection to.
//
// Asked once for the whole batch rather than per peer, and asked before
// connecting rather than after, so that "already connected" is a fact about the
// node as the operator found it.
func connectedSet(ctx context.Context, cli Client) (map[string]bool, error) {
	resp, err := cli.ListPeers(ctx, &lnrpc.ListPeersRequest{})
	if err != nil {
		return nil, fmt.Errorf("listing this node's peers: %w", err)
	}
	out := make(map[string]bool, len(resp.GetPeers()))
	for _, p := range resp.GetPeers() {
		out[strings.ToLower(p.GetPubKey())] = true
	}
	return out, nil
}

// pendingByPeer groups this node's pending opens by the peer they are with.
//
// Asked once for the whole batch, like connectedSet, and for the same reason: n
// identical answers to one question is n chances for them to disagree.
//
// Only pending_open_channels are counted. A channel that is pending *close* is
// on its way out and does not hold an open slot, and one that is force-closing
// is not pending open either — the same narrowing abort.isPendingOpen makes, for
// the same reason.
func pendingByPeer(ctx context.Context, cli Client) (map[string][]PendingOpen, error) {
	resp, err := cli.PendingChannels(ctx, &lnrpc.PendingChannelsRequest{})
	if err != nil {
		return nil, fmt.Errorf("listing this node's pending channels: %w", err)
	}
	out := map[string][]PendingOpen{}
	for _, p := range resp.GetPendingOpenChannels() {
		ch := p.GetChannel()
		key := strings.ToLower(ch.GetRemoteNodePub())
		out[key] = append(out[key], PendingOpen{
			ChannelPoint: ch.GetChannelPoint(),
			CapacitySat:  ch.GetCapacity(),
			Ours:         ch.GetInitiator() == lnrpc.Initiator_INITIATOR_LOCAL,
			Private:      ch.GetPrivate(),
		})
	}
	return out, nil
}

// ValidatePubkey checks a peer key without asking anybody.
//
// Not merely a length check: btcec.ParsePubKey rejects a well-formed 33 bytes
// that is not on the curve, which is the residue left after hex and length have
// both passed. It is the cheapest check in the whole tool and it catches the
// mistake — a transposed or truncated key — that would otherwise surface as an
// unreachable peer with no explanation.
func ValidatePubkey(s string) error {
	if s == "" {
		return errors.New("no pubkey given")
	}
	raw, err := hex.DecodeString(s)
	if err != nil {
		return fmt.Errorf("not hex: %w", err)
	}
	if len(raw) != 33 {
		return fmt.Errorf("%d bytes, want a 33-byte compressed pubkey", len(raw))
	}
	if raw[0] != 0x02 && raw[0] != 0x03 {
		return fmt.Errorf("starts with %#02x, so it is not a compressed pubkey", raw[0])
	}
	if _, err := btcec.ParsePubKey(raw); err != nil {
		return fmt.Errorf("is not a point on secp256k1: %w", err)
	}
	return nil
}

// isAlreadyConnected recognises LND's refusal to connect twice.
//
// Matched on text because rpcserver.go returns a plain fmt.Errorf here with no
// code to key off — the same situation as abort.ErrNoShim, and handled the same
// way.
func isAlreadyConnected(err error) bool {
	return strings.Contains(err.Error(), "already connected to peer")
}
