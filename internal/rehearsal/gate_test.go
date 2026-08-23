package rehearsal

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func measured(signing time.Duration, rounds ...Round) *Measurement {
	m := &Measurement{
		Signing:  signing,
		Limit:    DefaultAbortAfterSigning,
		Accepted: true,
		Rounds:   rounds,
		Decoy:    Decoy{TxID: "decoy", Outputs: 3, OutputSat: 750_000, FeeSat: 2_000, VsizeVB: 400},
	}
	return m
}

func ok(label string, d time.Duration) Round { return Round{Label: label, Elapsed: d} }

func TestGateRefusesToArmWhenTheRoundOutranTheLimit(t *testing.T) {
	m := measured(6*time.Minute, ok("cold1", 3*time.Minute), ok("cold2", 3*time.Minute))
	err := Gate(m)
	if err == nil {
		t.Fatal("a six-minute round passed a five-minute gate")
	}
	if !errors.Is(err, ErrTooSlow) {
		t.Fatalf("wrong sentinel: %v", err)
	}
	if m.Passes() {
		t.Fatal("Passes and Gate disagree")
	}
	// The headroom is what the gate is protecting, and it has to be visible in
	// the report rather than only implied by the verdict.
	if m.Headroom() != PeerWindow-6*time.Minute {
		t.Errorf("headroom = %s", m.Headroom())
	}
}

func TestGateClearsAComfortableRound(t *testing.T) {
	m := measured(90*time.Second, ok("cold1", 40*time.Second), ok("cold2", 50*time.Second))
	if err := Gate(m); err != nil {
		t.Fatalf("Gate: %v", err)
	}
	if !m.Passes() {
		t.Fatal("Passes and Gate disagree")
	}
	worst, found := m.Slowest()
	if !found || worst.Label != "cold2" {
		t.Errorf("slowest = %+v, want cold2", worst)
	}
}

// A device that fails is the more important of the two things this phase
// catches, and it must not be masked by a fast round.
func TestGateRefusesADeviceThatFailed(t *testing.T) {
	m := measured(20*time.Second,
		ok("cold1", 10*time.Second),
		Round{Label: "cold2", Elapsed: 10 * time.Second, Err: errors.New("malformed witness")},
	)
	err := Gate(m)
	if !errors.Is(err, ErrDeviceFailed) {
		t.Fatalf("wrong sentinel: %v", err)
	}
	if m.Passes() {
		t.Fatal("a failed signer passed the gate")
	}
}

func TestGateRefusesARejectedDecoy(t *testing.T) {
	m := measured(20*time.Second, ok("cold1", 10*time.Second))
	m.Accepted, m.RejectReason = false, "min relay fee not met"
	if err := Gate(m); !errors.Is(err, ErrDecoyRefused) {
		t.Fatalf("wrong sentinel: %v", err)
	}
}

// There is no default measurement. Arming without one is arming on a guess about
// how long the devices take, and that guess is what the whole phase exists to
// replace.
func TestGateRefusesWhenNoRehearsalWasRun(t *testing.T) {
	if err := Gate(nil); err == nil {
		t.Fatal("a batch was allowed to arm with no measurement behind it")
	}
}

func TestTheGateMatchesTheDesignsConfigBlock(t *testing.T) {
	// docs/design.html: limits.abort_after_signing_seconds = 300.
	if DefaultAbortAfterSigning != 300*time.Second {
		t.Fatalf("DefaultAbortAfterSigning = %s, want 300s", DefaultAbortAfterSigning)
	}
	// And it is half of what it is carved out of, which is the point of it.
	if PeerWindow != 10*time.Minute {
		t.Fatalf("PeerWindow = %s, want 10m", PeerWindow)
	}
}

// The rehearsal report is read in the same pane as the reserve and plan reports.
// The risk here is the decoy's txid: 64 characters that cannot be wrapped.
func TestTheRehearsalReportStaysInThePane(t *testing.T) {
	m := measured(4*time.Minute+13*time.Second,
		ok("coldcard-upstairs", 2*time.Minute),
		ok("bitbox-in-the-safe", 2*time.Minute+13*time.Second))
	m.Decoy.TxID = "f232ecfce9421a454b8993652c894a3eb9c356e0cf9326b5e47d518f3e12005f"
	m.Total = 5 * time.Minute

	for i, line := range strings.Split(m.Report(), "\n") {
		if n := len([]rune(line)); n > 78 {
			t.Errorf("line %d is %d columns, over the 78-column pane:\n%s", i+1, n, line)
		}
	}
}

// A signing round measured in milliseconds must not print as "0s": the report is
// how an operator decides whether their setup is fast enough, and a rounding
// step that erases the measurement is worse than no measurement.
func TestASubSecondRoundIsNotRoundedAway(t *testing.T) {
	m := measured(43*time.Millisecond, ok("cold1", 20*time.Millisecond))
	m.Total = 60 * time.Millisecond
	if got := m.Report(); strings.Contains(got, "signing round        0s") {
		t.Errorf("the measurement was rounded away:\n%s", got)
	}
	if !strings.Contains(m.Report(), "43ms") {
		t.Errorf("the measured round is not in the report:\n%s", m.Report())
	}
}
