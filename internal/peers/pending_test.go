package peers

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
	"google.golang.org/grpc"
)

// A second batch peer, so attribution can be tested rather than assumed.
const otherPeerKey = "0294a2a5e2a1f6b0e1cd1e1e57e6b0c9d4ba1c2f4c9b0e2a1d3f5a7b9c1e3d5f7a"

// fakeLND is the narrow slice peers.Check uses, and nothing else. Written here
// rather than driven against the harness because every fact under test is one
// this package derives from an RPC answer: what the node said is the input.
type fakeLND struct {
	connected []string
	pending   []*lnrpc.PendingChannelsResponse_PendingOpenChannel
	closing   []*lnrpc.PendingChannelsResponse_WaitingCloseChannel
	inGraph   map[string]bool

	pendingCalls int
}

func (f *fakeLND) ConnectPeer(context.Context, *lnrpc.ConnectPeerRequest,
	...grpc.CallOption) (*lnrpc.ConnectPeerResponse, error) {
	return &lnrpc.ConnectPeerResponse{}, nil
}

func (f *fakeLND) ListPeers(context.Context, *lnrpc.ListPeersRequest,
	...grpc.CallOption) (*lnrpc.ListPeersResponse, error) {
	resp := &lnrpc.ListPeersResponse{}
	for _, k := range f.connected {
		resp.Peers = append(resp.Peers, &lnrpc.Peer{PubKey: k})
	}
	return resp, nil
}

func (f *fakeLND) GetNodeInfo(_ context.Context, in *lnrpc.NodeInfoRequest,
	_ ...grpc.CallOption) (*lnrpc.NodeInfo, error) {
	if !f.inGraph[in.GetPubKey()] {
		return nil, errors.New("unable to find node")
	}
	return &lnrpc.NodeInfo{
		Node:        &lnrpc.LightningNode{Alias: "peer-" + in.GetPubKey()[:4]},
		NumChannels: 2,
	}, nil
}

func (f *fakeLND) PendingChannels(context.Context, *lnrpc.PendingChannelsRequest,
	...grpc.CallOption) (*lnrpc.PendingChannelsResponse, error) {
	f.pendingCalls++
	return &lnrpc.PendingChannelsResponse{
		PendingOpenChannels:  f.pending,
		WaitingCloseChannels: f.closing,
	}, nil
}

func pendingOpen(peer, cp string, capSat int64,
	who lnrpc.Initiator) *lnrpc.PendingChannelsResponse_PendingOpenChannel {

	return &lnrpc.PendingChannelsResponse_PendingOpenChannel{
		Channel: &lnrpc.PendingChannelsResponse_PendingChannel{
			RemoteNodePub: peer,
			ChannelPoint:  cp,
			Capacity:      capSat,
			Initiator:     who,
		},
	}
}

// A channel already pending with a batch peer is the thing this check exists to
// find, and finding it must not stop the run: --maxpendingchannels is the peer's
// own configuration and is published nowhere, so a peer that allows several will
// take the batch quite happily.
func TestAChannelAlreadyPendingWithABatchPeerIsReportedAndNotFatal(t *testing.T) {
	cli := &fakeLND{
		connected: []string{testPeerKey},
		inGraph:   map[string]bool{testPeerKey: true},
		pending: []*lnrpc.PendingChannelsResponse_PendingOpenChannel{
			pendingOpen(testPeerKey, "aa:0", 250_000, lnrpc.Initiator_INITIATOR_LOCAL),
		},
	}

	facts, err := Check(context.Background(), cli, []Want{
		{Pubkey: testPeerKey, AmountSat: 1_000_000},
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	f := facts[0]

	if !f.HasCompetingOpen() {
		t.Fatal("a channel pending with this very peer was not reported")
	}
	if len(f.Pending) != 1 {
		t.Fatalf("got %d pending channels, want 1", len(f.Pending))
	}
	if got := f.Pending[0].ChannelPoint; got != "aa:0" {
		t.Errorf("channel point %q, want aa:0", got)
	}
	if !f.Pending[0].Ours {
		t.Error("INITIATOR_LOCAL did not come back as ours, so the report would " +
			"blame the peer for a channel this node opened")
	}
	if !f.Usable() {
		t.Fatal("a competing pending open made the peer unusable, which would " +
			"refuse a batch that a peer allowing several pending channels takes. " +
			"It is a warning: the peer's --maxpendingchannels is published nowhere")
	}
	if !strings.Contains(f.Report(), "aa:0") {
		t.Error("the report does not name the outpoint, so the operator cannot act on it")
	}
}

// The whole value of the check is attribution. A pending channel with somebody
// else says nothing about this peer's remaining room.
func TestPendingChannelsAreAttributedToTheRightPeer(t *testing.T) {
	cli := &fakeLND{
		connected: []string{testPeerKey, otherPeerKey},
		inGraph:   map[string]bool{testPeerKey: true, otherPeerKey: true},
		pending: []*lnrpc.PendingChannelsResponse_PendingOpenChannel{
			pendingOpen(otherPeerKey, "bb:1", 500_000, lnrpc.Initiator_INITIATOR_REMOTE),
		},
	}

	facts, err := Check(context.Background(), cli, []Want{
		{Pubkey: testPeerKey, AmountSat: 1_000_000},
		{Pubkey: otherPeerKey, AmountSat: 1_000_000},
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}

	if facts[0].HasCompetingOpen() {
		t.Error("a channel pending with a different peer was charged to this one")
	}
	if !facts[1].HasCompetingOpen() {
		t.Fatal("the peer that does have one pending was reported as clear")
	}
	if facts[1].Pending[0].Ours {
		t.Error("INITIATOR_REMOTE came back as ours")
	}

	// Asked once for the whole batch, like the connected set: n identical
	// answers to one question is n chances for them to disagree.
	if cli.pendingCalls != 1 {
		t.Errorf("PendingChannels called %d times for a 2-peer batch, want 1",
			cli.pendingCalls)
	}
}

// Only pending *open* holds the slot step 2 needs. A channel on its way out does
// not, and counting one would refuse a batch for a channel that is leaving.
func TestOnlyPendingOpensCount(t *testing.T) {
	cli := &fakeLND{
		connected: []string{testPeerKey},
		inGraph:   map[string]bool{testPeerKey: true},
		closing: []*lnrpc.PendingChannelsResponse_WaitingCloseChannel{{
			Channel: &lnrpc.PendingChannelsResponse_PendingChannel{
				RemoteNodePub: testPeerKey, ChannelPoint: "cc:2",
			},
		}},
	}

	facts, err := Check(context.Background(), cli, []Want{
		{Pubkey: testPeerKey, AmountSat: 1_000_000},
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if facts[0].HasCompetingOpen() {
		t.Error("a channel waiting to close was counted as holding an open slot")
	}
}

// The fact is about our own node, so it survives a peer the gossip graph has
// never heard of — which is the case where the operator has least else to go on.
func TestACompetingOpenIsReportedForAPeerTheGraphDoesNotKnow(t *testing.T) {
	cli := &fakeLND{
		connected: []string{testPeerKey},
		inGraph:   map[string]bool{}, // GetNodeInfo returns "unable to find node"
		pending: []*lnrpc.PendingChannelsResponse_PendingOpenChannel{
			pendingOpen(testPeerKey, "dd:3", 100_000, lnrpc.Initiator_INITIATOR_LOCAL),
		},
	}

	facts, err := Check(context.Background(), cli, []Want{
		{Pubkey: testPeerKey, AmountSat: 1_000_000},
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	f := facts[0]

	if f.InGraph {
		t.Fatal("the fake said the graph does not know this peer")
	}
	if !f.HasCompetingOpen() {
		t.Fatal("the pending channel was dropped on the graph's early return. It " +
			"is a fact about this node, not about the peer, so it is exactly the " +
			"thing still knowable here")
	}
	if !strings.Contains(f.Report(), "dd:3") {
		t.Error("the report for an unknown peer does not name the pending outpoint")
	}
}
