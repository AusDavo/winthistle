package prose

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/AusDavo/winthistle/internal/abort"
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

// channelWithID is channel() for a test that needs the ids to differ, which is
// anything reading a journal.Run and an abort.Report against each other:
// alreadyGoneSplit keys one on the other.
func channelWithID(id lnd.PendingChanID, state journal.ChannelState) journal.Channel {
	c := channel(state, false)
	c.PendingChanID = id
	return c
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

// The mirror of the one above, and the reason it exists.
//
// Before the inversion a run in signing had verified every stream and reached
// chan_pending on none of them, so this screen said the abort was free and
// shim_cancel was the whole of it. Now signing is the state *after* the gate:
// every channel is at chan_pending, so the abort is n abandons with LND's blunt
// flag on each. The old copy would have told an operator a teardown costs nothing
// at the moment it costs each peer a pending-channel slot for 2016 blocks, which
// is the kind of wrong that gets acted on.
func TestTheSigningRecoveryScreenDoesNotCallTheAbortFree(t *testing.T) {
	got := Recovery(run(journal.StateSigning,
		channel(journal.ChanPending, true),
		channel(journal.ChanPending, true)), time.Now())

	mustContain(t, got, "Abandon")
	mustContain(t, got, "2016 blocks")
	// The two facts that make the state legible: the gate closed before anything
	// was asked of a wallet, and nothing went out.
	mustContain(t, got, "already recoverable")
	mustContain(t, got, "Nothing was broadcast")
	mustNotContain(t, got, "cancellable for free")
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
		Cancelled: []abort.ShimOutcome{{AlreadyGone: true}},
		Failures:  []error{errors.New("cancelling shim aabb: rpc error")},
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
// #27's claim one function over, which that issue's sweep did not reach.
//
// The success path said n channels "are still pending on the other side" off
// rep.Abandoned, which establishes only that this node abandoned them. Found by
// the audit run after this slice's copy was written, because the sweep grepped
// the wording failureLine used. It shows the mechanism now, the way
// recoveryPlan's own paragraph does.
func TestTheCleanOutcomeShowsWhyThePeersAreNotClean(t *testing.T) {
	rep := &abort.Report{Abandoned: []abort.AbandonOutcome{
		{Channel: lnd.ChannelPoint{TxID: fakeTxID, Index: 0}},
		{Channel: lnd.ChannelPoint{TxID: fakeTxID, Index: 1}},
	}}
	got := flat(RecoveryOutcome(run(journal.StateAborting), rep, nil))

	if regexp.MustCompile(`still pending on the other side`).MatchString(got) {
		t.Errorf("the outcome screen states the peers' side bare, where only "+
			"this node's abandons were read:\n%s", got)
	}
	mustContain(t, got, "2 channels were abandoned here")
	mustContain(t, got, "an abandon tells the peer nothing at all")
	mustContain(t, got, "keeps its side pending")
	// Clock B still in blocks, which the style rule requires of recovery copy.
	mustContain(t, got, "until 2016 blocks pass from the funding height")
}

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

// A failure line may say what was read and not what it implies.
//
// #27. internal/abort looked the channel up in *this node's* PendingChannels and
// the abandon did not run, so "it is still pending here" is exactly that — and
// the arm went on to say "and on the peer", which nothing here asked. The
// inference is sound, which is what made it a copy decision, and the paragraph
// in recoveryPlan makes the same claim with its working shown and clock B named
// in blocks. That paragraph is on a different screen, so this line has to be
// self-contained: it says the mechanism instead of the mechanism's conclusion.
func TestTheBluntRefusalSaysWhatWasReadAndNotWhatItImplies(t *testing.T) {
	rep := &abort.Report{Failures: []error{errWrap(abort.ErrBluntNotConfirmed)}}
	got := flat(RecoveryOutcome(run(journal.StateAborting), rep, errors.New("x")))

	// The old claim, in the shape it was made. "pending" survives in the
	// replacement, so the assertion is on the peer clause and not on the word.
	if regexp.MustCompile(`pending here and on the peer`).MatchString(got) {
		t.Errorf("the failure line asserts the channel is pending on the peer, "+
			"where only this node's PendingChannels was read:\n%s", got)
	}
	// What was read, named as the thing that was read.
	mustContain(t, got, "This node still has it pending, which is what was read")
	// And the mechanism, so the reader can carry the inference themselves.
	mustContain(t, got, "an abandon tells the peer nothing either way")
	mustContain(t, got, "changed nothing on the peer's side")
	// The other arms are unchanged: ErrNotPending's copy was checked in the
	// same sweep and is sound.
	notPending := flat(RecoveryOutcome(run(journal.StateAborting),
		&abort.Report{Failures: []error{errWrap(abort.ErrNotPending)}},
		errors.New("x")))
	mustContain(t, notPending, "That is the refusal working")
}

// The safe-to-call-twice list may not credit a step this abort does not have.
//
// #32's third item, and #21's class: copy crediting a deleted dependency. Item 5
// removed Bitcoin Core and every coin lock the app took, and the list still
// offered Core's lock release as one of its three reasons. abort.Run's own doc
// names two, and two is what the screen says now.
func TestTheSafeToCallTwiceListDoesNotCreditCore(t *testing.T) {
	got := flat(RecoveryOutcome(run(journal.StateAborting),
		&abort.Report{Failures: []error{errWrap(abort.ErrNoShim)}},
		errors.New("x")))
	if regexp.MustCompile(`(?i)core`).MatchString(got) {
		t.Errorf("the outcome screen credits Bitcoin Core, which item 5 removed "+
			"from this application along with every coin lock it took:\n%s", got)
	}
	mustContain(t, got, "Both steps in it are written to be safe to call twice")
	mustContain(t, got, "an already-abandoned channel is not an error in LND")
}

func TestRecoveryListSaysWhenSomethingMustNotBeTouched(t *testing.T) {
	runs := []*journal.Run{
		run(journal.StateArming, channel(journal.ChanShimRegistered, false)),
		run(journal.StatePublishing, channel(journal.ChanPending, true)),
	}
	got := RecoveryList(runs, time.Now())
	mustContain(t, got, "2 unfinished runs")
	// The journal knows a run is neither published nor aborted. It does not know
	// whether one is being driven right now — an arming run is in this list for
	// its whole life — so the screen must not assert that they stopped.
	mustContain(t, got, "Unless something is driving one right now, they stopped")
	// A capital in the middle of a sentence. theyThey returned "They" because
	// that clause used to open the paragraph, and moving it left "…right now,
	// They stopped…" on the screen — which a passing test for the new clause did
	// not notice and a browser did.
	if strings.Contains(got, ", They ") || strings.Contains(got, ", It ") {
		t.Errorf("a sentence restarts mid-clause with a capital:\n%s", got)
	}
	if strings.Contains(strings.Join(strings.Fields(got), " "),
		"journal. They stopped") {
		t.Errorf("the list asserts every run stopped, which it cannot know:\n%s", got)
	}
	mustContain(t, got, "must not be aborted")
	// It names which one, and it does not point at a section that is not there.
	// "see below" was what it said, and there is no below: the run's own screen
	// is where the explanation lives, on both front doors.
	mustContain(t, got, "1 of these reached the publish call: run-1")
	if strings.Contains(got, "see below") {
		t.Errorf("the list points at a section it does not have:\n%s", got)
	}

	if got := RecoveryList(nil, time.Now()); !strings.Contains(got, "Nothing to recover") {
		t.Errorf("empty list: %q", got)
	}
}

// No screen may point at a section it does not have.
//
// Two did. The list said an unabortable run was explained "see below" and
// nothing below explained it — that paragraph was the last thing on the screen.
// The abort plan said the blunt-flag prompt was described "see below" and it is
// described in BluntConfirmation, which is a different screen shown only when
// the abort actually runs. Both were survivable in a terminal, where the next
// command's output follows; both read as broken copy the moment a browser
// rendered them as a page that ends.
//
// The rule, because "see below" is legitimate where there is a below —
// recoveryPlan's "Not free — see below" is followed by the paragraphs that price
// it: a screen may point below itself only if at least two more paragraphs
// follow. One is a closing remark, and a promise answered by a closing remark is
// the shape both defects had.
func TestNoScreenPointsBelowItself(t *testing.T) {
	screens := map[string]string{
		"list": RecoveryList([]*journal.Run{
			run(journal.StatePublishing, channel(journal.ChanPending, true)),
		}, time.Now()),
		"armed":        Recovery(run(journal.StateArmed, channel(journal.ChanPending, true)), time.Now()),
		"half-aborted": Recovery(halfAborted(2), time.Now()),
		"published": Recovery(run(journal.StatePublished,
			channel(journal.ChanPending, true)), time.Now()),
	}
	for name, text := range screens {
		for _, phrase := range []string{"see below", "See below"} {
			i := strings.Index(text, phrase)
			if i < 0 {
				continue
			}
			after := 0
			for _, para := range strings.Split(text[i:], "\n\n") {
				if strings.TrimSpace(para) != "" {
					after++
				}
			}
			// The paragraph containing the phrase counts as one of them.
			if after < 3 {
				t.Errorf("%s says %q and then ends within %d paragraph(s). What it "+
					"points at has to be on the same screen:\n%s",
					name, phrase, after-1, text)
			}
		}
	}
}

// The columns have to line up, because the second line of a row is read as
// belonging to the first.
//
// The id field was fourteen wide and NewRunID produces twenty-two, so two things
// were wrong at once and only a browser showed either. Rows with ids of
// different lengths did not line up with each other, because the short ones were
// padded to fourteen and the long ones ran past it. And every row's detail line
// sat at column seventeen, which was inside the id above it rather than under
// anything — a column that lines up with nothing reads as a rendering fault.
func TestTheListColumnsLineUp(t *testing.T) {
	runs := []*journal.Run{
		run(journal.StateArming, channel(journal.ChanShimRegistered, false)),
		halfAborted(2),
	}
	// Deliberately different lengths. `winthistle run --id` takes whatever it is
	// given, so a journal holds both shapes, and equal-length ids would let a
	// fixed-width field pass this test for the wrong reason.
	runs[0].ID = "probe"
	runs[0].TxID = ""

	text := RecoveryList(runs, time.Now())
	lines := strings.Split(text, "\n")

	var states []int
	for _, line := range lines {
		for _, state := range []string{"arming", "aborting"} {
			if i := strings.Index(line, state); i > 0 {
				states = append(states, i)
			}
		}
	}
	if len(states) != 2 {
		t.Fatalf("expected two state columns, found %d in:\n%s", len(states), text)
	}
	if states[0] != states[1] {
		t.Errorf("the state column is at %d on one row and %d on the next, so two "+
			"ids of different lengths do not line up:\n%s",
			states[0], states[1], text)
	}

	// And the detail lines sit before the ids rather than inside them.
	for i, line := range lines {
		trimmed := strings.TrimLeft(line, " ")
		if trimmed == "" || !strings.HasPrefix(line, " ") {
			continue
		}
		indent := len(line) - len(trimmed)
		if strings.Contains(line, "arming") || strings.Contains(line, "aborting") {
			continue // a row's own line
		}
		if indent > 2 && indent < states[0] {
			// Between the start of the id and the start of the state: in the
			// middle of the id above it.
			if indent >= len("  ")+len(runs[0].ID) {
				t.Errorf("line %d is indented %d, which lands inside the id above "+
					"it rather than under a column:\n%s", i+1, indent, text)
			}
		}
	}

	// And one hand-picked id does not pad every other row out with it.
	runs[0].ID = strings.Repeat("x", 90)
	for i, line := range strings.Split(RecoveryList(runs, time.Now()), "\n") {
		if strings.Contains(line, "aborting") && len([]rune(line)) > PaneWidth {
			t.Errorf("one long id pushed an unrelated row to %d columns (line %d):\n%s",
				len([]rune(line)), i+1, line)
		}
	}
}

// A count and the state it counts must never end up on opposite sides of a line
// break. "12" alone at the end of one line and "abandoned" at the start of the
// next is a count of twelve of something unnamed followed by a state with no
// number, on the one line whose whole job is to say which channels are on which
// side of the abort.
func TestTheBreakdownNeverBreaksACountAwayFromItsState(t *testing.T) {
	got := RecoveryList([]*journal.Run{halfAborted(12)}, time.Now())
	for i, line := range strings.Split(got, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if last := fields[len(fields)-1]; last == "12" || last == "12," {
			t.Errorf("line %d ends with a bare count, so its state is on the next "+
				"line:\n%s", i+1, got)
		}
	}
}

// The zero value of State is not a verdict, and the two screens that render it
// must not let a blank read as "this run never got anywhere".
//
// It should be unreachable — runs.state is NOT NULL and every writer sets it —
// which is exactly why it is worth pinning: an unreachable case renders whatever
// the last person assumed, and the default branch here used to produce a
// sentence with its subject missing.
func TestABlankStateIsNotRenderedAsAVerdict(t *testing.T) {
	r := run("", channel(journal.ChanPending, true))

	list := RecoveryList([]*journal.Run{r}, time.Now())
	mustContain(t, list, "(no state)")

	one := Recovery(r, time.Now())
	mustContain(t, one, "(no state)")
	mustContain(t, one, "no state at all for this run")
	if strings.Contains(one, "has this run as .") {
		t.Errorf("the blank state rendered as a sentence with its subject "+
			"missing:\n%s", one)
	}
	// And it must still price the abort off the channels, which are the only
	// fact on the screen when the run's own state says nothing.
	mustContain(t, one, "Abandon 1 channel")
}

// The three states a live run holds must not be rendered as a run that stopped.
//
// #30 items 1 and 2. arming is written when the streams open, signing before the
// wallet is asked and aborting before the abort's first call — deliberately, so
// that a crash leaves artifacts — so all three are written by a run that is
// still going, including this process's own teardown, which prints this very
// screen through run.recoverRun. The copy said "streams were open", "had gone
// out to be signed" and "an abort of this run was started and did not finish".
//
// Keyed on the shape of the old claim rather than on a word, because the
// replacements still contain most of the words: "did not finish" is the claim,
// "in progress, or one was interrupted partway" is the hedge, and a bare grep
// for "abort" matches both.
func TestNoStateThatALiveRunHoldsIsRenderedAsARunThatStopped(t *testing.T) {
	for _, tc := range []struct {
		state journal.State
		chans []journal.Channel
		// asserted is the old bare claim, in the shape it was made.
		asserted *regexp.Regexp
		// hedged is what must be there instead.
		hedged string
	}{
		{
			state:    journal.StateArming,
			chans:    []journal.Channel{channel(journal.ChanShimRegistered, false)},
			asserted: regexp.MustCompile(`streams were open`),
			hedged:   "Funding streams are open",
		},
		{
			state:    journal.StateSigning,
			chans:    []journal.Channel{channel(journal.ChanPending, true)},
			asserted: regexp.MustCompile(`had gone out to be signed`),
			hedged:   "is out with the signing wallet",
		},
		{
			state:    journal.StateAborting,
			chans:    []journal.Channel{channel(journal.ChanPending, true)},
			asserted: regexp.MustCompile(`abort of this run was started and did not finish`),
			hedged:   "in progress, or one was interrupted partway",
		},
	} {
		t.Run(string(tc.state), func(t *testing.T) {
			got := flat(Recovery(run(tc.state, tc.chans...), time.Now()))
			if tc.asserted.MatchString(got) {
				t.Errorf("the %s screen asserts %q, which the journal cannot "+
					"establish — that state is written before the work it names:\n%s",
					tc.state, tc.asserted, got)
			}
			mustContain(t, got, tc.hedged)
		})
	}
}

// And the hedge has to say why, not just soften the verb.
//
// The reason is the same on all three and it is the only thing that makes the
// hedge readable rather than evasive: the journal records what a run wrote, not
// whether it is still writing. prose.RecoveryList says it in those words for the
// list; this is the one-run screen saying it for the state it was asked about.
func TestTheHedgedStateSaysWhatTheJournalEstablished(t *testing.T) {
	for _, state := range []journal.State{
		journal.StateArming, journal.StateSigning, journal.StateAborting,
	} {
		got := flat(Recovery(run(state, channel(journal.ChanPending, true)), time.Now()))
		mustContain(t, got, "written")
		if !strings.Contains(got, "right now") {
			t.Errorf("the %s screen hedges without saying that a run in this "+
				"state may be going right now:\n%s", state, got)
		}
	}
}

// #32 item 2, and the finding there is that the screen was already right and the
// constant's doc was the silent half. journal.StateSigning is written before
// sign() is called — it is run.armWindow's next statement — and FileWallet.Signed
// runs an os.Remove over both watched paths before it prints the prompt naming
// them, so a failure there is a wallet that was never asked at all. #30 put that
// in the renderer; this pins that it stays there, because the constant's doc now
// points at this sentence rather than repeating it.
func TestTheSigningScreenSaysTheWalletHasNotBeenAsked(t *testing.T) {
	got := flat(Recovery(run(journal.StateSigning,
		channel(journal.ChanPending, true)), time.Now()))
	mustContain(t, got, "written before the wallet is asked for anything")
}

// withSigners is a run carrying signer rows, which no test in this package built
// until now — so every sentence signerNote writes about a signer had been
// rendered by nothing, including its widths. #24 found the same gap in doctor's
// journal check.
func withSigners(state journal.State, states ...journal.SignerState) *journal.Run {
	r := run(state, channel(journal.ChanPending, true))
	for i, st := range states {
		r.Signers = append(r.Signers, journal.Signer{
			Label: fmt.Sprintf("wallet-%d", i),
			State: st,
		})
	}
	return r
}

// The signer note may not say what the run was doing, and it may not say a
// wallet refused.
//
// Two halves of #30, one function. The zero branch said "No signer had been
// asked for anything when this stopped" — the first half is establishable,
// because run.sign writes the awaiting row before it calls Signed, and the
// second is a claim about a run that may be going right now. And "%d declined"
// says a wallet said no, which nothing in this build can observe: PR #31 stopped
// sign() writing the value, so every row this will ever see was written by the
// defect that fix removed.
func TestTheSignerNoteSaysOnlyWhatTheJournalHolds(t *testing.T) {
	t.Run("no rows", func(t *testing.T) {
		got := flat(Recovery(run(journal.StateArming,
			channel(journal.ChanShimRegistered, false)), time.Now()))
		if regexp.MustCompile(`asked for anything when this stopped`).MatchString(got) {
			t.Errorf("the signer note says the run stopped, which it cannot "+
				"know:\n%s", got)
		}
		mustContain(t, got, "No signer row was written for this run")
		mustContain(t, got, "step 7 writes one before it asks a wallet")
	})

	t.Run("declined", func(t *testing.T) {
		got := flat(Recovery(withSigners(journal.StateSigning,
			journal.SignerDeclined), time.Now()))
		// The count stays — the row is on disk and dropping the arm would put it
		// in the unknown bucket — and it must not stand alone as a verdict.
		mustContain(t, got, "1 marked declined")
		mustContain(t, got, "never meant a wallet said no")
		mustContain(t, got, "whenever the signing step failed at all")
		// The four things the old step 7 actually had in hand when it wrote it.
		mustContain(t, got, "Ctrl-C")
		mustContain(t, got, "a txid that had moved")
	})

	t.Run("every state adds up", func(t *testing.T) {
		got := flat(Recovery(withSigners(journal.StateSigning,
			journal.SignerSigned, journal.SignerPartial, journal.SignerAwaiting,
			journal.SignerDeclined, journal.SignerState("from-the-future")), time.Now()))
		for _, want := range []string{
			"1 signed", "1 returned a partial signature", "1 still awaited",
			"1 marked declined", "1 in a state this build does not recognise",
		} {
			mustContain(t, got, want)
		}
	})
}

// #32 item 1's screen half. "It is not a failure" was asserted over every shim
// that came back already gone, on the one path where it is a channel the
// operator must go and look at with clock B running.
//
// abort.ErrNoShim establishes an absence and nothing else — LND holds no funding
// intent under that id. What decides what the absence means is the journal's own
// row, which is why this screen splits the shims rather than hedging all of
// them: on the unverified side the run never reached psbt_verify, so LND never
// created a channel; on the verified side psbt_verify completed the funding flow
// and a channel that reached chan_pending consumed its own intent on the way.
func TestAShimGoneOverAVerifiedChannelIsNotWavedAway(t *testing.T) {
	verified := lnd.PendingChanID{1}
	unverified := lnd.PendingChanID{2}
	got := RecoveryOutcome(
		run(journal.StateAborting,
			channelWithID(verified, journal.ChanVerified),
			channelWithID(unverified, journal.ChanShimRegistered)),
		&abort.Report{Cancelled: []abort.ShimOutcome{
			{ID: verified, AlreadyGone: true},
			{ID: unverified, AlreadyGone: true},
		}}, nil)

	// The claims that were made and could not be: two unobserved causes, a
	// verdict over the whole set, and "nothing is left" on a screen reporting a
	// channel that is.
	mustNotContain(t, got, "It is not a failure")
	mustNotContain(t, got, "expected result of a second abort")
	mustNotContain(t, got, "timed the reservation out first")
	mustNotContain(t, got, "Nothing of this run is left on this node")

	// What was read, on both sides.
	mustContain(t, got, "LND holds no funding intent under")
	mustContain(t, got, "nothing left to take apart")
	mustContain(t, got, "this run had verified, and that is what to go and look at")
	mustContain(t, got, "consumed its own intent")

	// The next move, and the peer's key to match it against.
	mustContain(t, got, "lncli pendingchannels")
	mustContain(t, got, fakeKey)
	mustContain(t, got, "2016 blocks")
	mustContain(t, got, "stays marked as aborting")
}

// The other side on its own: with nothing verified there is nothing standing,
// and the screen must still be able to say so. A hedge that fires on every
// already-gone shim would be the same defect facing the other way.
func TestAShimGoneOverAnUnverifiedChannelStillSaysNothingIsLeft(t *testing.T) {
	id := lnd.PendingChanID{2}
	got := RecoveryOutcome(
		run(journal.StateAborting, channelWithID(id, journal.ChanShimRegistered)),
		&abort.Report{Cancelled: []abort.ShimOutcome{{ID: id, AlreadyGone: true}}},
		nil)

	mustContain(t, got, "Nothing of this run is left on this node")
	mustContain(t, got, "nothing left to take apart")
	mustNotContain(t, got, "go and look at")
	mustNotContain(t, got, "lncli pendingchannels")
}

// A shim the run has no row for is not the benign case. The zero ChannelState is
// not ChanShimRegistered, and the branch it falls to has to be the one that
// tells the operator to go and look — an unknown must not be the thing that gets
// waved away. [[go-zero-value-passes-an-assertion]] is this the other way up.
func TestAShimGoneForAChannelTheRunDoesNotKnowIsNotWavedAway(t *testing.T) {
	got := RecoveryOutcome(run(journal.StateAborting),
		&abort.Report{Cancelled: []abort.ShimOutcome{
			{ID: lnd.PendingChanID{9}, AlreadyGone: true},
		}}, nil)

	mustContain(t, got, "go and look at")
	mustNotContain(t, got, "Nothing of this run is left on this node")
}

// A heading with no bullets under it reads as a rendering fault, and Recovery
// has already said nothing is standing two lines above. Reachable for any run
// whose channels are all in terminal states, which ChanShimGone makes ordinary.
func TestARunWithNothingLeftPrintsNoAbortPlan(t *testing.T) {
	got := Recovery(run(journal.StateAborted,
		channel(journal.ChanShimGone, false)), time.Now())

	mustContain(t, got, "Nothing of this run is still standing in LND")
	mustNotContain(t, got, "An abort of this run would")
	// And the state itself now has a sentence rather than the default's bare
	// restatement of the value.
	mustContain(t, got, "left nothing behind")
	mustNotContain(t, got, "The journal has this run as aborted.")
}

// halfAborted is the shape of a bad night: an abort that ran partway, so the
// same run carries channels on both sides of it and in every state at once,
// ChanShimGone included since #32 added it.
//
// It is the widest row RecoveryList can produce, and it is not a hypothetical —
// RecoveryOutcome tells the operator to expect exactly this shape and to run the
// recovery again.
func halfAborted(perState int) *journal.Run {
	r := run(journal.StateAborting)
	r.ID = "20260824-193012-9f3a1c"
	for _, st := range []journal.ChannelState{
		journal.ChanShimRegistered, journal.ChanVerified, journal.ChanPending,
		journal.ChanAbandoned, journal.ChanCancelled, journal.ChanShimGone,
	} {
		for i := 0; i < perState; i++ {
			r.Channels = append(r.Channels, channel(st, st == journal.ChanPending))
		}
	}
	return r
}

// Every recovery screen is read in the same pane as the reserve and plan
// reports, so it wraps to the same column. A screen that overruns is a hazard
// rather than a blemish: the figures stop lining up with the prose.
//
// RecoveryList was missing from this map until it got a route in the web UI, and
// it was ten columns over the pane when it was added: a half-aborted run carries
// channels in all six states at once and the counts went out on one unwrapped
// line. Same defect internal/plan shipped, for the same reason — the screen
// nobody measured. Every screen this file renders is in here now, including the
// list's empty and no-channel shapes.
func TestTheRecoveryScreensStayInThePane(t *testing.T) {
	screens := map[string]string{
		"arming": Recovery(run(journal.StateArming, channel(journal.ChanShimRegistered, false)), time.Now()),
		"armed":  Recovery(run(journal.StateArmed, channel(journal.ChanPending, true)), time.Now()),
		// Post-gate, and the longest of the pre-publish paragraphs, which is why
		// it is measured rather than assumed to fit alongside the other two.
		"signing": Recovery(run(journal.StateSigning, channel(journal.ChanPending, true)), time.Now()),
		"published": Recovery(run(journal.StatePublished,
			channel(journal.ChanPending, true)), time.Now()),
		"half-aborted": Recovery(halfAborted(12), time.Now()),
		"blunt": BluntConfirmation(abort.BluntRequest{
			Channel:   lnd.ChannelPoint{TxID: fakeTxID, Index: 0},
			Rejection: "not externally funded or not pending",
		}),
		"list": RecoveryList([]*journal.Run{
			run(journal.StateArming, channel(journal.ChanShimRegistered, false)),
			halfAborted(12),
			run(journal.StatePublishing, channel(journal.ChanPending, true)),
		}, time.Now()),
		"list, one channel each": RecoveryList([]*journal.Run{halfAborted(1)}, time.Now()),
		"empty list":             RecoveryList(nil, time.Now()),
		"list of a run with no channels": RecoveryList(
			[]*journal.Run{run(journal.StateArming)}, time.Now()),
		// RecoveryOutcome was missing from this map, and it is the screen whose
		// width is least under this file's control: every failureLine ends with
		// LND's or abort's own error text, appended to a bullet. Added when #27
		// lengthened one of those bullets.
		"outcome, clean": RecoveryOutcome(run(journal.StateAborting), &abort.Report{
			Abandoned: []abort.AbandonOutcome{{
				Channel:   lnd.ChannelPoint{TxID: fakeTxID, Index: 0},
				UsedBlunt: true,
			}},
			Cancelled: []abort.ShimOutcome{{ID: lnd.PendingChanID{1, 2, 3}}},
		}, nil),
		// signerNote's own sentences, which nothing measured before: the counts
		// line at its widest, and the declined paragraph under it.
		"signing, every signer state": Recovery(withSigners(journal.StateSigning,
			journal.SignerSigned, journal.SignerPartial, journal.SignerAwaiting,
			journal.SignerDeclined, journal.SignerState("from-the-future")), time.Now()),
		// #32 item 1's screen half: the widest of the already-gone paragraphs,
		// with a full 66-character pubkey on a bullet under it.
		"outcome, a shim gone over a verified channel": RecoveryOutcome(
			run(journal.StateAborting,
				channelWithID(lnd.PendingChanID{1}, journal.ChanVerified),
				channelWithID(lnd.PendingChanID{2}, journal.ChanShimRegistered)),
			&abort.Report{Cancelled: []abort.ShimOutcome{
				{ID: lnd.PendingChanID{1}, AlreadyGone: true},
				{ID: lnd.PendingChanID{2}, AlreadyGone: true},
			}}, nil),
		"outcome, partial": RecoveryOutcome(run(journal.StateAborting), &abort.Report{
			Failures: []error{
				errWrap(abort.ErrBluntNotConfirmed),
				errWrap(abort.ErrNotPending),
				errWrap(abort.ErrNoShim),
			},
		}, errors.New("the abort did not complete")),
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
