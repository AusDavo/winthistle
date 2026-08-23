package peers_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/AusDavo/winthistle/internal/arm"
	"github.com/AusDavo/winthistle/internal/peers"
	"github.com/AusDavo/winthistle/internal/regtestenv"
)

// fixtureChannelSat matches the other packages' fixtures, so a peer that accepts
// one accepts the others.
const fixtureChannelSat = 250_000

// generator is a valid secp256k1 point that is nobody: G itself. It is the
// cleanest way to ask about a peer that is well formed and does not exist.
const generator = "0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798"

func ctxFor(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// TestThePeerPreFlightReadsTheLocalGraph covers the two free tiers against a
// live node: the pubkey check, and everything GetNodeInfo can say.
//
// Nothing here opens a funding stream, so nothing here starts a clock. That is
// what makes it Phase 0 in the strict sense, and it is why it can be run as
// often as the operator likes.
func TestThePeerPreFlightReadsTheLocalGraph(t *testing.T) {
	env := regtestenv.Start(t)
	ctx := ctxFor(t)

	pubkeys := env.Peers(t)
	wants := make([]peers.Want, 0, len(pubkeys))
	for _, p := range pubkeys {
		wants = append(wants, peers.Want{Pubkey: p, AmountSat: fixtureChannelSat})
	}

	facts, err := peers.Check(ctx, env.Alice.Lightning, wants)
	if err != nil {
		t.Fatalf("the peer pre-flight: %v", err)
	}
	if len(facts) != len(wants) {
		t.Fatalf("checked %d peers, got %d findings", len(wants), len(facts))
	}

	for _, f := range facts {
		t.Logf("%s", f.Summary())
		if !f.KeyOK {
			t.Errorf("a harness peer's key was refused: %s", f.KeyProblem)
		}
		if f.Connection != peers.AlreadyConnected {
			t.Errorf("%s: connection = %s, want already connected — bootstrap.sh "+
				"connects alice to every peer", f.Want.Pubkey[:16], f.Connection)
		}
		if !f.Usable() {
			t.Errorf("%s is not usable: %s", f.Want.Pubkey[:16], f.ConnectDetail)
		}
		if !f.InGraph {
			// Not a hard failure: a node with no announced channels is genuinely
			// absent from the graph, and the report says what that does and does
			// not mean.
			t.Logf("%s is not in alice's gossip graph", f.Want.Pubkey[:16])
			continue
		}
		if f.Alias == "" {
			t.Errorf("%s has no alias in the graph", f.Want.Pubkey[:16])
		}
		t.Logf("\n%s", f.Report())
	}
}

// A key that is well formed and belongs to nothing: the graph has never heard of
// it and the address does not answer.
//
// Both branches matter to Phase 0. An unknown node is not an error — a new node
// looks exactly like this — but an unreachable one cannot be armed against, and
// the difference has to survive into the report rather than being flattened into
// a failure.
func TestAnUnknownPeerIsReportedRatherThanFailed(t *testing.T) {
	env := regtestenv.Start(t)
	ctx := ctxFor(t)

	facts, err := peers.Check(ctx, env.Alice.Lightning, []peers.Want{{
		Pubkey:    generator,
		Host:      "127.0.0.1:1",
		AmountSat: fixtureChannelSat,
	}})
	if err != nil {
		t.Fatalf("the pre-flight failed rather than reporting: %v", err)
	}
	f := facts[0]

	if !f.KeyOK {
		t.Fatalf("the generator point is a valid pubkey: %s", f.KeyProblem)
	}
	if f.Connection != peers.Unreachable {
		t.Errorf("connection = %s, want unreachable", f.Connection)
	}
	if f.InGraph {
		t.Error("alice's graph should not know the generator point")
	}
	if f.Usable() {
		t.Error("an unreachable peer must not be reported as usable")
	}
	t.Logf("\n%s", f.Report())
}

// TestAnAcceptedProbeIsAuthoritativeAndCostsAPeerSlot is the shim probe, and the
// finding that changes the flow.
//
// The probe works: accept_channel arrives, LND names a funding output, and that
// is the only authoritative answer about what a peer will take. What it costs is
// the part docs/design.html gets wrong by calling the probe "free". The peer has
// created a reservation, and shim_cancel does not release it —
// CancelFundingIntent deletes an entry in our own wallet's map and sends the peer
// nothing at all. Only the peer's own zombie sweeper takes it back, after ten
// minutes with up to a minute of slack.
//
// So a Phase 0 that probes all n peers and then immediately arms collides with
// its own probes against any peer running LND's default of one pending channel.
// ReadyToArm is the gate, and this asserts that it holds.
func TestAnAcceptedProbeIsAuthoritativeAndCostsAPeerSlot(t *testing.T) {
	env := regtestenv.Start(t)
	ctx := ctxFor(t)

	pubkey := env.Peers(t)[0]
	want := peers.Want{Pubkey: pubkey, AmountSat: fixtureChannelSat}

	probe, err := peers.Do(ctx, env.Alice.Lightning, "regtest", want)
	if err != nil {
		t.Fatalf("probing %s: %v", pubkey[:16], err)
	}
	t.Logf("%s", probe.Summary())

	if !probe.Accepted {
		t.Fatalf("the harness peers accept %d sat: %s", fixtureChannelSat,
			probe.Rejection.Raw)
	}
	if probe.FundingAddress == "" {
		t.Error("an accepted probe means LND named a funding output")
	}
	if !probe.Cancelled || probe.CancelErr != nil {
		t.Errorf("the probe's own shim was left standing: %v", probe.CancelErr)
	}

	// The cost, asserted rather than described.
	if probe.HoldsUntil.IsZero() {
		t.Fatal("an accepted probe leaves the peer holding a reservation, and the " +
			"probe has to say until when")
	}
	if got := probe.HoldsUntil.Sub(probe.Answered); got != peers.HoldUpperBound {
		t.Errorf("hold = %s, want %s", got, peers.HoldUpperBound)
	}
	if err := peers.ReadyToArm([]peers.Probe{probe}, time.Now()); err == nil {
		t.Fatal("the gate did not hold immediately after an accepted probe")
	} else if !errors.Is(err, peers.ErrPeersStillHolding) {
		t.Errorf("wrong sentinel: %v", err)
	}
	t.Logf("\n%s", probe.Report(time.Now()))

	// And the gate is a judgement about the peer's limit, not a lock: this
	// harness runs --maxpendingchannels=200, so the same peer accepts another
	// stream straight away. A peer running LND's default of one would not, and
	// the operator would meet that at step 2 with the cold wallet out.
	second, err := peers.Do(ctx, env.Alice.Lightning, "regtest", want)
	if err != nil {
		t.Fatalf("re-probing %s: %v", pubkey[:16], err)
	}
	if !second.Accepted {
		t.Fatalf("the harness peer refused a second reservation: %s\n"+
			"That is what a peer running the LND default of one would do, and it "+
			"is exactly the collision ReadyToArm exists to prevent",
			second.Rejection.Peer)
	}
	if !second.Cancelled {
		t.Errorf("the second probe's shim was left standing: %v", second.CancelErr)
	}
}

// A probe against a peer that cannot be reached asks the peer nothing, so it
// costs nothing and must not hold the gate.
func TestAProbeAgainstAnUnreachablePeerCostsNothing(t *testing.T) {
	env := regtestenv.Start(t)
	ctx := ctxFor(t)

	probe, err := peers.Do(ctx, env.Alice.Lightning, "regtest", peers.Want{
		Pubkey: generator, AmountSat: fixtureChannelSat,
	})
	if err != nil {
		t.Fatalf("the probe failed rather than reporting: %v", err)
	}
	if probe.Accepted {
		t.Fatal("a peer alice is not connected to accepted a channel")
	}
	if !probe.HoldsUntil.IsZero() {
		t.Error("a probe that never reached a peer must not hold the gate")
	}
	if err := peers.ReadyToArm([]peers.Probe{probe}, time.Now()); err != nil {
		t.Errorf("a refused probe held the gate: %v", err)
	}
	t.Logf("refusal: kind=%v raw=%q", probe.Rejection.Kind, probe.Rejection.Raw)
	t.Logf("\n%s", probe.Report(time.Now()))
}

// The probe is arm.Open with the answer thrown away, and that is worth pinning
// as a fact about the types rather than a claim in a comment: both take the same
// client and the same description of a channel.
func TestTheProbeAndTheArmAskTheSameQuestion(t *testing.T) {
	var _ func(context.Context, arm.Client, string, peers.Want) (peers.Probe, error) = peers.Do
	var _ func(context.Context, arm.Client, string, []arm.Channel) (*arm.Streams, error) = arm.Open

	w := peers.Want{Pubkey: "aa", AmountSat: 1, Private: true}
	c := w.Channel()
	if c.Peer != w.Pubkey || c.AmountSat != w.AmountSat || c.Private != w.Private {
		t.Fatalf("Want.Channel lost something: %+v from %+v", c, w)
	}
}
