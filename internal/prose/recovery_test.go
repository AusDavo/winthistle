package prose

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/AusDavo/winthistle/internal/abort"
	"github.com/AusDavo/winthistle/internal/bitcoind"
	"github.com/AusDavo/winthistle/internal/journal"
	"github.com/AusDavo/winthistle/internal/lnd"
)

const (
	fakeTxID = "0f0e0d0c0b0a09080706050403020100f0e0d0c0b0a090807060504030201000"
	fakeKey  = "02cca6c5c966fcf61d121e3a70e03a1cd9eeeea024b26ea666ce974d43b242e636"
)

func run(state journal.State, chans ...journal.Channel) *journal.Run {
	return &journal.Run{
		ID:        "run-1",
		State:     state,
		TxID:      fakeTxID,
		RawTx:     "0200",
		UpdatedAt: time.Now().Add(-90 * time.Second),
		Channels:  chans,
	}
}

func channel(state journal.ChannelState, withOutpoint bool) journal.Channel {
	c := journal.Channel{
		PendingChanID: lnd.PendingChanID{1, 2, 3},
		PeerPubkey:    fakeKey,
		AmountSat:     250_000,
		State:         state,
	}
	if withOutpoint {
		c.Outpoint = lnd.ChannelPoint{TxID: fakeTxID, Index: 1}
	}
	return c
}

// The screen for a run that reached the publish call is the one place in the
// product where the correct action is to do nothing yet, and the reason has to
// survive being skim-read at two in the morning.
func TestThePublishedRecoveryScreenRefusesAndSaysWhy(t *testing.T) {
	for _, state := range []journal.State{journal.StatePublishing, journal.StatePublished} {
		got := Recovery(run(state, channel(journal.ChanPending, true)), time.Now())

		mustContain(t, got, "will not be aborted")
		mustContain(t, got, "no force-close path")
		mustContain(t, got, fakeTxID)
		// The remedy, not just the refusal.
		mustContain(t, got, "re-broadcast")
		// And the thing that must never be done, said outright rather than
		// left to be inferred from the refusal.
		mustContain(t, got, "do not build a replacement")
		mustContain(t, got, "changes every funding outpoint")
	}
}

// An armed run is the gate working, not a fault, and the screen has to say so —
// otherwise the operator reads a stopped run as a broken one and reaches for
// something.
func TestTheArmedRecoveryScreenExplainsThatNothingWentOut(t *testing.T) {
	got := Recovery(run(journal.StateArmed,
		channel(journal.ChanPending, true),
		channel(journal.ChanPending, true)), time.Now())

	mustContain(t, got, "already recoverable by force-close")
	mustContain(t, got, "the publish never happened")
	// The cost of aborting, stated rather than implied.
	mustContain(t, got, "2016 blocks")
	mustContain(t, got, "Nothing was broadcast, so nothing was spent")
}

// A run whose streams never finalized costs nothing to take apart, and saying so
// is what stops an operator agonising over a decision that has no downside.
func TestTheArmingRecoveryScreenSaysTheAbortIsFree(t *testing.T) {
	got := Recovery(run(journal.StateArming,
		channel(journal.ChanShimRegistered, false)), time.Now())

	mustContain(t, got, "cancellable for free")
	mustContain(t, got, "Cancel 1 funding shim")
	mustNotContain(t, got, "Abandon")
}

// The blunt-flag prompt is the only place a human authorises something that
// could lose funds if the premise were wrong. It has to say what the premise is
// and what already checked it.
func TestTheBluntConfirmationExplainsWhatIsBeingAuthorised(t *testing.T) {
	got := BluntConfirmation(abort.BluntRequest{
		Channel:   lnd.ChannelPoint{TxID: fakeTxID, Index: 0},
		Rejection: "channel abc is not externally funded or not pending",
	})

	// LND's own words, not a paraphrase.
	mustContain(t, got, "is not externally funded or not pending")
	// Why the refusal is expected rather than alarming.
	mustContain(t, got, "thaw height")
	// What the fallback gives up.
	mustContain(t, got, "would remove a confirmed channel just as readily")
	// And what has already been checked on the operator's behalf.
	mustContain(t, got, "PendingChannels")
}

func TestTheOutcomeScreenReportsAPartialAbortHonestly(t *testing.T) {
	rep := &abort.Report{
		Abandoned: []abort.AbandonOutcome{{
			Channel:   lnd.ChannelPoint{TxID: fakeTxID, Index: 0},
			UsedBlunt: true,
		}},
		Cancelled:  []abort.ShimOutcome{{AlreadyGone: true}},
		LocksFreed: []bitcoind.Outpoint{{TxID: fakeTxID, Vout: 3}},
		Failures:   []error{errors.New("cancelling shim aabb: rpc error")},
	}
	got := RecoveryOutcome(run(journal.StateAborting), rep, errors.New("abort did not fully complete"))

	mustContain(t, got, "did not fully complete")
	// What did work must not have to be discovered again.
	mustContain(t, got, "none of it has to be done again")
	mustContain(t, got, "safe to call twice")
	mustContain(t, got, "rpc error")
}

// A clean abort still leaves the peers holding their side, and a screen that
// said "clean" without saying that would be producing the next incident.
func TestACleanAbortStillWarnsAboutThePeers(t *testing.T) {
	rep := &abort.Report{
		Abandoned: []abort.AbandonOutcome{{
			Channel: lnd.ChannelPoint{TxID: fakeTxID, Index: 0},
		}},
	}
	got := RecoveryOutcome(run(journal.StateAborted), rep, nil)

	mustContain(t, got, "Nothing of this run is left on this node")
	mustContain(t, got, "Your peers are not clean")
	mustContain(t, got, "2016 blocks")
}

// A failure that is the refusal working must not read like a bug.
func TestTheOutcomeScreenNamesTheRefusalThatProtectedYou(t *testing.T) {
	rep := &abort.Report{Failures: []error{
		errors.New("wrapped: " + abort.ErrNotPending.Error()),
	}}
	// errors.Is needs the sentinel, not its text.
	rep.Failures = []error{errWrap(abort.ErrNotPending)}

	got := RecoveryOutcome(run(journal.StateAborting), rep, errors.New("x"))
	mustContain(t, got, "That is the refusal working")
	mustContain(t, got, "stranded")
}

func TestRecoveryListSaysWhenSomethingMustNotBeTouched(t *testing.T) {
	runs := []*journal.Run{
		run(journal.StateArming, channel(journal.ChanShimRegistered, false)),
		run(journal.StatePublishing, channel(journal.ChanPending, true)),
	}
	got := RecoveryList(runs, time.Now())
	mustContain(t, got, "2 unfinished runs")
	mustContain(t, got, "must not be aborted")

	if got := RecoveryList(nil, time.Now()); !strings.Contains(got, "Nothing to recover") {
		t.Errorf("empty list: %q", got)
	}
}

// Every recovery screen is read in the same pane as the reserve and plan
// reports, so it wraps to the same column. A screen that overruns is a hazard
// rather than a blemish: the figures stop lining up with the prose.
func TestTheRecoveryScreensStayInThePane(t *testing.T) {
	screens := map[string]string{
		"arming": Recovery(run(journal.StateArming, channel(journal.ChanShimRegistered, false)), time.Now()),
		"armed":  Recovery(run(journal.StateArmed, channel(journal.ChanPending, true)), time.Now()),
		"published": Recovery(run(journal.StatePublished,
			channel(journal.ChanPending, true)), time.Now()),
		"blunt": BluntConfirmation(abort.BluntRequest{
			Channel:   lnd.ChannelPoint{TxID: fakeTxID, Index: 0},
			Rejection: "not externally funded or not pending",
		}),
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

func errWrap(err error) error { return wrapped{err} }

type wrapped struct{ err error }

func (w wrapped) Error() string { return "cancelling: " + w.err.Error() }
func (w wrapped) Unwrap() error { return w.err }

// The copy is wrapped to a fixed column, so a phrase the operator reads as one
// sentence is not one line. Collapsing whitespace before matching is what lets a
// test assert about the words rather than about where the wrap fell.
func flat(s string) string { return strings.Join(strings.Fields(s), " ") }

func mustContain(t *testing.T, got, want string) {
	t.Helper()
	if !strings.Contains(flat(got), flat(want)) {
		t.Errorf("the screen does not say %q:\n%s", want, got)
	}
}

func mustNotContain(t *testing.T, got, unwanted string) {
	t.Helper()
	if strings.Contains(flat(got), flat(unwanted)) {
		t.Errorf("the screen says %q and should not:\n%s", unwanted, got)
	}
}
