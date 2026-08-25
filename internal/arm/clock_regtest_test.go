package arm_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/AusDavo/winthistle/internal/abort"
	"github.com/AusDavo/winthistle/internal/regtestenv"
)

// slowEnv gates the one test in this repository that takes longer than a coffee.
const slowEnv = "WINTHISTLE_SLOW"

// TestWhoOwnsTheTenMinuteClock answers the open question in docs/design.html:
// "Exactly when does each peer's ten-minute clock start — at OpenChannel, or at
// the peer's accept_channel?"
//
// The answer from LND's source at v0.21.2-beta, which this test then measures:
//
//   - Our own node never starts a clock at all. pruneZombieReservations skips
//     PSBT reservations outright — "We don't want to expire PSBT funding
//     reservations. These reservations are always initiated by us and the remote
//     peer is likely going to cancel them after some idle time anyway." So the
//     only clock in the picture is the peer's.
//   - On the peer, the clock is resCtx.lastUpdated, and handleFundingOpen sets it
//     with `defer resCtx.updateTimestamp()` — at the end of handling
//     open_channel, which is the same message handler that sends accept_channel.
//     The two candidate answers are therefore a few microseconds apart on the
//     peer's side of the wire, and one round trip apart on ours.
//   - It fires when the peer's zombie sweeper next ticks and finds
//     time.Since(lastUpdated) > ReservationTimeout. Those are
//     DefaultZombieSweeperInterval = 1 minute and DefaultReservationTimeout = 10
//     minutes, neither adjustable in a release build — lncfg/dev.go returns the
//     constant, and only a `dev`-tagged build reads the flags.
//
// So: answerable on regtest, but not in the form the question was asked. The
// magnitude, the granularity and *whose* clock it is are all observable, and this
// test observes them. Which of OpenChannel and accept_channel starts it is not
// distinguishable here and would not be on mainnet either, because the difference
// is one network round trip against a ten-minute budget. The useful consequence
// for the countdown is the one this test establishes: start it at our OpenChannel
// call, treat 10:00 as an upper bound with up to a minute of sweep granularity
// past it, and remember that none of it is guaranteed for a peer that is not LND.
//
// It takes eleven minutes of wall clock, so it is skipped unless WINTHISTLE_SLOW
// is set. `make test` is not the place for it; a question worth answering once is.
func TestWhoOwnsTheTenMinuteClock(t *testing.T) {
	if os.Getenv(slowEnv) == "" {
		t.Skipf("takes ~11 minutes of wall clock; set %s=1 to run it", slowEnv)
	}

	env := regtestenv.Start(t)
	peers := env.Peers(t)

	// One stream, opened and then left alone. Nothing is built, nothing is
	// verified, nothing is signed — the whole point is that the reservation goes
	// stale from inactivity.
	opened := time.Now()
	s := env.OpenShimStream(t, peers[0], fixtureChannelSat)
	t.Logf("psbt_fund arrived %s after the OpenChannel call, naming %s",
		time.Since(opened).Round(time.Millisecond), s.FundingAddress)

	// Twelve minutes: ten for the timeout, one for the sweeper's granularity, one
	// spare. A test that waited exactly ten would report "no failure" for a
	// timeout that fired at 10:30.
	elapsed, err := env.AwaitStreamFailure(t, s, 12*time.Minute)
	if err == nil {
		t.Fatalf("the stream was still alive after %s. Either the peer's "+
			"reservation timeout is not %s any more, or its sweeper is not running "+
			"— both would change what the countdown should say",
			elapsed.Round(time.Second), 10*time.Minute)
	}
	// Measured 10m41s against bob on 2026-08-23 at lnd v0.19.3-beta, and 10m14s
	// against carol on 2026-08-26 at v0.21.2-beta: ten minutes of
	// DefaultReservationTimeout plus the sweeper's up-to-a-minute granularity.
	// LND's own wording for it is worth knowing, because it is what an operator
	// will see — "remote canceled funding, possibly timed out", which is
	// chanfunding.ErrRemoteCanceled wrapped around the peer's error. Note that the
	// peer reports it as an internal error rather than as a timeout; the "possibly"
	// is ours.
	t.Logf("the peer gave up %s after our OpenChannel call: %v",
		elapsed.Round(time.Second), err)

	// The window is a floor and a ceiling, and both matter. Too early and the
	// design's five-minute abort gate is not the safety margin it claims; too late
	// and the countdown is pessimistic in a way that costs signing rounds.
	switch {
	case elapsed < 9*time.Minute:
		t.Errorf("the peer gave up after %s, well inside the ten minutes the phase "+
			"model budgets for", elapsed.Round(time.Second))
	case elapsed > 11*time.Minute+30*time.Second:
		t.Errorf("the peer held on for %s. Later than the countdown assumes is not "+
			"dangerous, but it means the displayed number is not the one that matters",
			elapsed.Round(time.Second))
	}

	// And what it cost: nothing. Nothing was published and nothing was signed, so
	// the window lapsing costs one more signing round rather than a channel — the
	// phase model's central claim. A fresh context, because harnessCtx's is long
	// dead by now, which is itself the shape of the problem: everything in the
	// armed window has to outlive the window.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	switch err := abort.CancelShim(ctx, env.Alice.Lightning, s.PendingChanID); {
	case err == nil:
		t.Log("the shim was still ours to cancel afterwards, so re-arming is free")
	case errors.Is(err, abort.ErrNoShim):
		t.Log("LND had already dropped the funding intent when the peer's error " +
			"arrived, so there was nothing left to cancel — also free")
	default:
		t.Errorf("cancelling the lapsed shim: %v", err)
	}
}
