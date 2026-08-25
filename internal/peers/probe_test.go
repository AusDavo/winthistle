package peers

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// The strings below are assembled the way LND assembles them at v0.21.2-beta,
// and that assembly is the thing under test.
//
// The peer's own words go out through failFundingFlow, which forwards the real
// text only for lnwallet.ReservationError, lnwire.FundingError and
// chanacceptor.ChanAcceptError. Our node's handleErrorMsg then wraps them twice:
// once with "received funding error from <pubkey>", and once — for a PSBT
// reservation, which every one of ours is — with chanfunding.ErrRemoteCanceled.
//
// The amounts render through btcutil.Amount.String(), which formats BTC with
// strconv.FormatFloat(v, 'f', -8, 64) and therefore trims trailing zeros: LND's
// own MinChanFundingSize of 20,000 sat prints as "0.0002 BTC", not
// "0.00020000 BTC". Getting that wrong is how a parser silently reports a
// minimum of zero.
const testPeerKey = "02cca6c5c966fcf61d121e3a70e03a1cd9eeeea024b26ea666ce974d43b242e636"

func wrapped(peerSaid string) error {
	return errors.New(remoteCanceled +
		"received funding error from " + testPeerKey + ": " + peerSaid)
}

func TestUnwrapReadsThePeersOwnRefusal(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		kind    Kind
		minSat  int64
		maxSat  int64
		peerHas string
	}{
		{
			name:    "too small names the peer's minimum",
			err:     wrapped("chan size of 0.00001 BTC is below min chan size of 0.0002 BTC"),
			kind:    TooSmall,
			minSat:  20_000,
			peerHas: "chan size of 0.00001 BTC is below min chan size of 0.0002 BTC",
		},
		{
			name:    "too large names the peer's maximum",
			err:     wrapped("chan size of 20 BTC exceeds maximum chan size of 0.16777215 BTC"),
			kind:    TooLarge,
			maxSat:  16_777_215,
			peerHas: "chan size of 20 BTC exceeds maximum chan size of 0.16777215 BTC",
		},
		{
			name:    "the pending-channel limit",
			err:     wrapped("Number of pending channels exceed maximum"),
			kind:    TooManyPending,
			peerHas: "Number of pending channels exceed maximum",
		},
		{
			name:    "LND's generic refusal keeps the reason on the peer",
			err:     wrapped("funding failed due to internal error"),
			kind:    Internal,
			peerHas: "funding failed due to internal error",
		},
		{
			name:    "a peer that is not LND can say anything",
			err:     wrapped("we only open channels on tuesdays"),
			kind:    Unknown,
			peerHas: "we only open channels on tuesdays",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := Unwrap(tc.err)
			if r.Kind != tc.kind {
				t.Errorf("kind = %v, want %v", r.Kind, tc.kind)
			}
			if r.MinChanSat != tc.minSat {
				t.Errorf("min = %d, want %d", r.MinChanSat, tc.minSat)
			}
			if r.MaxChanSat != tc.maxSat {
				t.Errorf("max = %d, want %d", r.MaxChanSat, tc.maxSat)
			}
			// The whole point of unwrapping: what is reported to the operator must
			// be the peer's sentence, not LND's claim that it timed out.
			if r.Peer != tc.peerHas {
				t.Errorf("peer said %q, want %q", r.Peer, tc.peerHas)
			}
			if r.Raw == "" {
				t.Error("the raw error must be kept: a peer that is not LND can " +
					"refuse in words this build has never seen")
			}
		})
	}
}

// A refusal that arrives under ErrRemoteCanceled is not evidence of a timeout,
// and the report must not repeat LND's claim that it is. This pins the specific
// case that would mislead an operator most: an instant refusal, presented as a
// ten-minute lapse.
func TestUnwrapDoesNotBelieveTheTimeoutClaim(t *testing.T) {
	r := Unwrap(wrapped("chan size of 0.00001 BTC is below min chan size of 0.0002 BTC"))
	if r.Kind != TooSmall {
		t.Fatalf("kind = %v", r.Kind)
	}
	if got := r.Peer; got == r.Raw {
		t.Fatal("the prefix was not stripped, so the operator would be told the " +
			"peer possibly timed out when in fact it named its minimum")
	}
	for _, unwanted := range []string{"timed out", "remote canceled"} {
		if strings.Contains(r.Peer, unwanted) {
			t.Errorf("the peer's own words still contain %q: %q", unwanted, r.Peer)
		}
	}
}

func TestSatFromBTCString(t *testing.T) {
	cases := map[string]int64{
		"0.0002":     20_000,
		"0.00000001": 1,
		"1":          100_000_000,
		"20":         2_000_000_000,
		"0.16777215": 16_777_215,
		"0":          0,
	}
	for in, want := range cases {
		if got := satFromBTCString(in); got != want {
			t.Errorf("satFromBTCString(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestValidatePubkey(t *testing.T) {
	if err := ValidatePubkey(testPeerKey); err != nil {
		t.Fatalf("a real compressed key was refused: %v", err)
	}

	bad := map[string]string{
		"empty":        "",
		"not hex":      "zz" + testPeerKey[2:],
		"too short":    testPeerKey[:64],
		"uncompressed": "04" + testPeerKey[2:],
		// x = 2^256 - 1, which is above secp256k1's field prime
		// (2^256 - 2^32 - 977), so it is not a coordinate at all. This is the
		// residue the length and hex checks both let through, and it is the
		// shape a transposed key most often takes.
		"not on the curve": "02" + strings.Repeat("ff", 32),
	}
	for name, key := range bad {
		if err := ValidatePubkey(key); err == nil {
			t.Errorf("%s: accepted %q", name, key)
		}
	}
}

// ReadyToArm is the gate between probing and arming, and it exists because a
// successful probe consumes one of the peer's pending-channel slots for the
// length of its own reservation timeout. A refused probe consumes nothing, so it
// must not hold the gate.
func TestReadyToArmWaitsOnlyForAcceptedProbes(t *testing.T) {
	now := time.Now()

	refused := Probe{Pubkey: testPeerKey, Rejection: Rejection{Kind: TooSmall}}
	if err := ReadyToArm([]Probe{refused}, now); err != nil {
		t.Fatalf("a refused probe held the gate: %v\n"+
			"Every limit check in handleFundingOpen runs before "+
			"InitChannelReservation, so a refusal leaves no reservation behind", err)
	}

	accepted := Probe{
		Pubkey:     testPeerKey,
		Accepted:   true,
		Answered:   now,
		HoldsUntil: now.Add(HoldUpperBound),
	}
	err := ReadyToArm([]Probe{accepted}, now)
	if err == nil {
		t.Fatal("an accepted probe did not hold the gate: arming inside its window " +
			"risks the peer's own pending-channel limit")
	}
	if !errors.Is(err, ErrPeersStillHolding) {
		t.Errorf("wrong sentinel: %v", err)
	}

	// And it clears on its own, without anything being asked of the peer.
	if err := ReadyToArm([]Probe{accepted}, now.Add(HoldUpperBound+time.Second)); err != nil {
		t.Errorf("the gate did not clear after the hold: %v", err)
	}
}

func TestHoldUpperBoundMatchesLNDsOwnConstants(t *testing.T) {
	// chanfunding.DefaultReservationTimeout plus lncfg.DefaultZombieSweeperInterval.
	// TestWhoOwnsTheTenMinuteClock measured 10m41s at v0.19.3-beta and 10m14s at
	// v0.21.2-beta, both inside
	// this bound and outside the ten minutes alone.
	if HoldUpperBound != 11*time.Minute {
		t.Fatalf("HoldUpperBound = %s, want 11m", HoldUpperBound)
	}
}

// Every peer report is read in the same pane as the reserve and plan reports, so
// it wraps to the same column. The refusal text is the risk here: it carries a
// 66-character pubkey spliced into the middle of a sentence by LND, and an
// unwrapped copy of it runs off the screen.
func TestThePeerReportsStayInThePane(t *testing.T) {
	long := wrapped("chan size of 0.00001 BTC is below min chan size of 0.0002 BTC")
	notReachable := errors.New("opening the batch's funding streams (0 already " +
		"open, and cancellable): channel 1 of 1: waiting for psbt_fund from " +
		testPeerKey[:16] + "…: rpc error: code = Unknown desc = peer " +
		testPeerKey + " is not online")

	now := time.Now()
	screens := map[string]string{
		"accepted": Probe{
			Pubkey: testPeerKey, AmountSat: 250_000, Accepted: true, Cancelled: true,
			FundingAddress: "bcrt1q8c2xd7272ef26jpajcgwagguaa3yqg4z7d3hcft6u6pqszzmlf2q09kv76",
			Answered:       now, HoldsUntil: now.Add(HoldUpperBound),
		}.Report(now),
		"too small": Probe{
			Pubkey: testPeerKey, AmountSat: 10_000, Rejection: Unwrap(long),
		}.Report(now),
		"unreachable": Probe{
			Pubkey: testPeerKey, AmountSat: 250_000, Rejection: Unwrap(notReachable),
		}.Report(now),
		"facts": Facts{
			Want: Want{Pubkey: testPeerKey, AmountSat: 250_000}, KeyOK: true,
			Connection: AlreadyConnected, InGraph: true, Alias: "bob",
			NumChannels: 4, TotalCapacitySat: 4_000_000,
			SmallestSat: 250_000, MedianSat: 1_000_000,
		}.Report(),
	}

	for name, text := range screens {
		for i, line := range strings.Split(text, "\n") {
			if n := len([]rune(line)); n > 78 {
				t.Errorf("%s line %d is %d columns, over the 78-column pane:\n%s",
					name, i+1, n, line)
			}
		}
	}
}
