package prose

import (
	"strings"
	"testing"
	"time"

	"github.com/AusDavo/winthistle/internal/journal"
	"github.com/AusDavo/winthistle/internal/lnd"
)

// live builds a run whose streams opened `since` ago.
func live(state journal.State, since time.Duration, chans ...journal.Channel) *journal.Run {
	return &journal.Run{
		ID:        "run-live",
		State:     state,
		TxID:      fakeTxID,
		CreatedAt: time.Now().Add(-since),
		UpdatedAt: time.Now().Add(-time.Second),
		Channels:  chans,
	}
}

func chanAt(state journal.ChannelState, index uint32, withOutpoint bool) journal.Channel {
	c := journal.Channel{
		PendingChanID: lnd.PendingChanID{1, 2, 3},
		PeerPubkey:    fakeKey,
		AmountSat:     250_000,
		State:         state,
	}
	if withOutpoint {
		c.Outpoint = lnd.ChannelPoint{TxID: fakeTxID, Index: index}
	}
	return c
}

// The receipt count is the live reading of I-1, so it counts channels in the
// pending state — the same rows MarkPending counts — and not lines of transcript
// or anything else that could be n while the channels are not.
func TestTheReceiptCountIsTwoOfThreeUntilTheThirdArrives(t *testing.T) {
	r := live(journal.StateArming, 3*time.Minute,
		chanAt(journal.ChanPending, 0, true),
		chanAt(journal.ChanPending, 1, true),
		chanAt(journal.ChanVerified, 0, false))

	got := Progress(r, time.Now(), 10*time.Minute)
	mustContain(t, got, "receipts      2 of 3")
	mustContain(t, got, "The funding transaction stays here until all 3 channels "+
		"have their receipt")
	if strings.Contains(got, "the gate is open") {
		t.Error("a two-of-three batch was described as through the gate")
	}
}

// HELD is the one line on this screen an operator looks at when nervous, so it
// has to stop saying HELD at exactly the write that lands before the publish
// call — not a request later.
func TestHeldStopsSayingHeldTheMomentTheJournalIsUnsure(t *testing.T) {
	both := []journal.Channel{
		chanAt(journal.ChanPending, 0, true), chanAt(journal.ChanPending, 1, true),
	}
	for _, tc := range []struct {
		state journal.State
		want  string
		gone  string
	}{
		{journal.StateArming, "HELD", "MAY BE PUBLIC"},
		{journal.StateSigning, "HELD", "MAY BE PUBLIC"},
		{journal.StateArmed, "HELD", "MAY BE PUBLIC"},
		{journal.StatePublishing, "MAY BE PUBLIC", "HELD"},
		{journal.StatePublished, "PUBLISHED", "HELD"},
	} {
		got := Progress(live(tc.state, time.Minute, both...), time.Now(), 10*time.Minute)
		if !strings.Contains(got, "funding tx    "+tc.want) {
			t.Errorf("in %s the funding line does not say %q:\n%s",
				tc.state, tc.want, got)
		}
		if strings.Contains(got, "funding tx    "+tc.gone) {
			t.Errorf("in %s the funding line still says %q", tc.state, tc.gone)
		}
	}
}

// A run in publishing must carry the reason nothing will abort it, because this
// is the screen somebody is reading while deciding whether to intervene.
func TestThePublishingStateSaysWhyNothingWillTearItDown(t *testing.T) {
	got := Progress(live(journal.StatePublishing, time.Minute,
		chanAt(journal.ChanPending, 0, true)), time.Now(), 10*time.Minute)
	mustContain(t, got, "these bytes may be in a mempool")
	mustContain(t, got, "strands its funds with no force-close path")
}

// The countdown is the peers' reservation clock, and a reservation that became a
// channel is not on it any more. Showing one on a fully armed batch would be
// telling somebody to hurry for no reason.
func TestTheCountdownDisappearsWhenNothingIsWaitingOnAPeer(t *testing.T) {
	waiting := Progress(live(journal.StateArming, time.Minute,
		chanAt(journal.ChanPending, 0, true),
		chanAt(journal.ChanShimRegistered, 0, false)), time.Now(), 10*time.Minute)
	if !strings.Contains(waiting, "peer window") {
		t.Error("a batch with a channel still awaiting its receipt shows no window")
	}

	armed := Progress(live(journal.StateArmed, time.Minute,
		chanAt(journal.ChanPending, 0, true),
		chanAt(journal.ChanPending, 1, true)), time.Now(), 10*time.Minute)
	if strings.Contains(armed, "peer window") {
		t.Error("a fully armed batch is counting down a reservation that has " +
			"already become a channel:\n" + armed)
	}
}

// Past the window the screen says so rather than showing a negative, and it says
// what it costs — which is a signing round, not any coins.
func TestALapsedWindowSaysWhatItCost(t *testing.T) {
	got := Progress(live(journal.StateArming, 12*time.Minute,
		chanAt(journal.ChanVerified, 0, false)), time.Now(), 10*time.Minute)
	mustContain(t, got, "may have lapsed")
	mustContain(t, got, "one more signing round")
	if strings.Contains(got, "-") && strings.Contains(got, "about -") {
		t.Error("a negative countdown reached the screen")
	}
}

// During a teardown the channels move from pending to abandoned. The receipt
// count must not read as though receipts were being taken back: nothing about
// what those peers signed has changed.
func TestATeardownDoesNotWindTheReceiptCountBackwards(t *testing.T) {
	got := Progress(live(journal.StateAborting, 5*time.Minute,
		chanAt(journal.ChanAbandoned, 0, true),
		chanAt(journal.ChanPending, 1, true)), time.Now(), 10*time.Minute)
	mustContain(t, got, "receipts      2 of 2")
	mustContain(t, got, "Nothing was broadcast")
}

// A run whose streams have not opened yet has no channels, and that is a state
// to describe rather than an empty table.
func TestARunWithNoChannelsYetSaysSo(t *testing.T) {
	got := Progress(live(journal.StateArming, time.Second), time.Now(), 10*time.Minute)
	mustContain(t, got, "funding streams have not opened")
	mustContain(t, got, "receipts      0 of 0")
}

// The same pane discipline the recovery screens are held to. This screen is the
// one that renders during the armed window, so an overrun lands at the worst
// moment there is.
func TestTheLiveScreenStaysInThePane(t *testing.T) {
	long := make([]journal.Channel, 0, 12)
	for i := range 12 {
		long = append(long, chanAt(journal.ChanPending, uint32(i), true))
	}
	screens := map[string]string{
		"arming, one waiting": Progress(live(journal.StateArming, 4*time.Minute,
			chanAt(journal.ChanPending, 0, true),
			chanAt(journal.ChanVerified, 0, false)), time.Now(), 10*time.Minute),
		"armed":      Progress(live(journal.StateArmed, time.Minute, long...), time.Now(), 10*time.Minute),
		"publishing": Progress(live(journal.StatePublishing, time.Minute, long...), time.Now(), 10*time.Minute),
		"published":  Progress(live(journal.StatePublished, time.Minute, long...), time.Now(), 10*time.Minute),
		"lapsed": Progress(live(journal.StateArming, 30*time.Minute,
			chanAt(journal.ChanShimRegistered, 0, false)), time.Now(), 10*time.Minute),
		"no channels": Progress(live(journal.StateArming, time.Second), time.Now(), 10*time.Minute),
		"nil":         Progress(nil, time.Now(), 10*time.Minute),
	}
	for name, text := range screens {
		for i, line := range strings.Split(text, "\n") {
			if n := len([]rune(line)); n > PaneWidth {
				t.Errorf("%s line %d is %d columns, over the %d-column pane:\n%s",
					name, i+1, n, PaneWidth, line)
			}
		}
	}
}
