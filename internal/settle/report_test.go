package settle

import (
	"strings"
	"testing"
	"time"

	"github.com/AusDavo/winthistle/internal/prose"
	"github.com/lightningnetwork/lnd/lnrpc"
)

// TestTheReportsFitThePane holds settlement to the same column as every other
// screen in this build.
//
// plan, doctor and prose each had one of these and settle did not, while it
// renders more operator-facing prose than any of them. It was over: State.line
// hand-rolls a three-column row with no width check at all, and the third column
// is LND's own refusal text, which is unbounded. Measured at 93 columns against
// a 78-column pane on a real refusal — "time lock delta of 4 is too small" — off
// TestTheReportAndTheErrorNameEveryStuckMember's own logged report.
//
// The states below are built by hand rather than driven through Tick, because
// what is under test is the rendering of every branch and several of them do not
// co-occur on any single run: a batch cannot be both finished and past its
// horizon. Driving them would test the loop again and cover less.
func TestTheReportsFitThePane(t *testing.T) {
	for name, r := range paneCases() {
		t.Run(name, func(t *testing.T) {
			report := r.Report()
			t.Logf("\n%s", report)
			assertInPane(t, report)
			assertInPane(t, r.Summary())
		})
	}
}

// paneCases is one Result per branch of Report, each carrying the widest copy
// that branch can be given.
func paneCases() map[string]*Result {
	// LND's own text, at the length it actually arrives at. The refusal detail
	// is not this program's string to shorten: it is the node's explanation of
	// what it would not do, and it is pasted into the row verbatim.
	long := PolicyOutcome{
		Refused: true,
		Reason:  lnrpc.UpdateFailure_UPDATE_FAILURE_INVALID_PARAMETER,
		Detail: "max htlc size of 1000000000 mSAT is above max pending amount " +
			"of 247500000 mSAT",
	}
	unexplained := PolicyOutcome{
		Refused: true,
		Reason:  lnrpc.UpdateFailure_UPDATE_FAILURE_UNKNOWN,
		Detail:  "could not update policies",
	}

	return map[string]*Result{
		// #26. A row whose link is not forwarding, so linkNote renders and is
		// measured — the note is the only place with room to say what the field
		// actually establishes.
		"an open channel whose link is not forwarding": {
			Elapsed:     12 * time.Second,
			RetryWindow: RetryWindow,
			States: []State{{
				Member: paneMember(0),
				Open:   true,
				Active: false,
				Confs:  -1,
				Policy: PolicyOutcome{
					Refused: true,
					Reason:  lnrpc.UpdateFailure_UPDATE_FAILURE_PENDING,
					Detail:  "not yet confirmed",
				},
			}},
		},
		"a stuck member with LND's own refusal in the row": {
			Elapsed:     94 * time.Second,
			RetryWindow: RetryWindow,
			States: []State{
				{
					Member: paneMember(0),
					Open:   true,
					Confs:  -1,
					Policy: long,
				},
				{
					Member:           paneMember(1),
					Open:             true,
					Active:           true,
					Confs:            -1,
					Policy:           unexplained,
					Attempts:         41,
					RefusedSince:     time.Now().Add(-11 * time.Minute),
					RefusedFor:       11 * time.Minute,
					RetriesExhausted: true,
				},
			},
		},
		// The production shape since issue #47: nothing open yet, the peer's own
		// minimum_depth read off PendingChannels, no confirmation count because
		// there is no Bitcoin node to give one, and the horizon a long way off.
		"pending, showing the peer's own minimum_depth": {
			Elapsed:     11 * time.Second,
			RetryWindow: RetryWindow,
			States: []State{{
				Member:        paneMember(0),
				Confs:         -1,
				PeerDepth:     3,
				ExpectedDepth: 6,
				ExpiryBlocks:  2010,
				StillPending:  true,
				Policy: PolicyOutcome{
					Refused: true,
					Reason:  lnrpc.UpdateFailure_UPDATE_FAILURE_PENDING,
					Detail:  "channel is not yet confirmed",
				},
				Attempts: 2,
			}},
		},
		// The window missed: the funding transaction had already confirmed when
		// this loop first asked, so there is no reading and the prediction
		// stands in.
		"pending with no depth reading": {
			Elapsed:     11 * time.Second,
			RetryWindow: RetryWindow,
			States: []State{{
				Member:        paneMember(0),
				Confs:         -1,
				ExpectedDepth: 6,
				ExpiryBlocks:  2010,
				StillPending:  true,
				Policy: PolicyOutcome{
					Refused: true,
					Reason:  lnrpc.UpdateFailure_UPDATE_FAILURE_PENDING,
					Detail:  "channel is not yet confirmed",
				},
				Attempts: 2,
			}},
		},
		"pending with a depth reading, and everything in the detail row": {
			Elapsed:     11 * time.Second,
			RetryWindow: RetryWindow,
			States: []State{{
				Member:        paneMember(0),
				Confs:         2,
				ExpectedDepth: 6,
				ObservedDepth: 3,
				ExpiryBlocks:  2010,
				StillPending:  true,
				Policy: PolicyOutcome{
					Refused: true,
					Reason:  lnrpc.UpdateFailure_UPDATE_FAILURE_NOT_FOUND,
					Detail:  "channel is not found in the graph database",
				},
				Attempts: 17,
			}},
		},
		"a day from the horizon":      paneHorizon(HorizonUrgent - 1),
		"three days from the horizon": paneHorizon(HorizonWarn - 1),
		"a long way from the horizon": paneHorizon(2010),
		// Issue #20's branch: the count has run out. The one an operator reads
		// under the most pressure, and the one that used to tell them their
		// channel was already dead.
		"past the horizon": paneHorizon(-3),
		"finished, with what the batch learned": {
			Elapsed:     4 * time.Minute,
			RetryWindow: RetryWindow,
			States: []State{
				{
					Member:        paneMember(0),
					Open:          true,
					Active:        true,
					Confs:         9,
					PeerDepth:     3,
					ExpectedDepth: 6,
					ObservedDepth: 3,
					Policy:        PolicyOutcome{Applied: true},
				},
				{
					Member:        paneMember(1),
					Open:          true,
					Confs:         9,
					ExpectedDepth: 6,
					ObservedDepth: 6,
					Policy:        PolicyOutcome{Applied: true},
				},
			},
		},
		"nothing to settle": {},
	}
}

func paneHorizon(blocks int32) *Result {
	return &Result{
		Elapsed:     3 * time.Hour,
		RetryWindow: RetryWindow,
		States: []State{{
			Member:        paneMember(0),
			Confs:         -1,
			ExpectedDepth: 6,
			ExpiryBlocks:  blocks,
			StillPending:  true,
			Policy: PolicyOutcome{
				Refused: true,
				Reason:  lnrpc.UpdateFailure_UPDATE_FAILURE_PENDING,
				Detail:  "channel is not yet confirmed",
			},
			Attempts: 640,
		}},
	}
}

// paneMember reuses settle_test.go's members, so the rows here are the widths
// the rest of the package's fixtures already produce.
func paneMember(i int) Member {
	return memberAt(uint32(i), [...]string{peerSilent, peerInvalid, peerFine}[i])
}

// assertInPane is the check itself.
//
// Runes, not bytes: this copy is full of em dashes, and a byte count reports a
// line three columns wider than it renders — which is worse than no check,
// because it is the kind of wrongness that gets fixed by widening the pane.
//
// One exemption, and it is the same one doctor makes for a pasteable command:
// the channel point on its own line. It is 66 characters, it is what the
// operator's next command has to be given verbatim, and policyNote wraps nothing
// around it on purpose — a wrapped outpoint is a transcription error waiting to
// happen. Everything the operator *reads* is held to the pane.
func assertInPane(t *testing.T, report string) {
	t.Helper()
	for i, line := range strings.Split(report, "\n") {
		if strings.HasPrefix(line, "  - ") && isChannelPoint(strings.TrimPrefix(line, "  - ")) {
			continue
		}
		if n := len([]rune(line)); n > prose.PaneWidth {
			t.Errorf("line %d is %d columns, past the %d-column pane:\n%s",
				i+1, n, prose.PaneWidth, line)
		}
	}
}

// isChannelPoint recognises the one exempt shape rather than exempting every
// bullet, so that an over-wide instruction cannot hide behind the exemption.
func isChannelPoint(s string) bool {
	txid, index, ok := strings.Cut(s, ":")
	if !ok || len(txid) != 64 || index == "" {
		return false
	}
	return strings.IndexFunc(txid+index, func(r rune) bool {
		return !strings.ContainsRune("0123456789abcdef", r)
	}) < 0
}

// TestThePastHorizonReportDoesNotSayWhatThePeerDid is issue #20.
//
// The count has run out. What is established is our own arithmetic against our
// own default; what is not established is that the peer did anything at all,
// because nothing in this build has heard from it and no RPC reports a peer's
// horizon to the initiator. The report used to quote LND's proto hedge and
// override it in the same sentence, and then print ForgetHorizonBlocks as the
// number of blocks the peer waited.
//
// The remedy is unchanged and is asserted here too: it is right whether or not
// the peer has forgotten, which is what made this copy-only.
func TestThePastHorizonReportDoesNotSayWhatThePeerDid(t *testing.T) {
	report := paneHorizon(-3).Report()
	t.Logf("\n%s", report)
	rep := flat(report)

	for _, want := range []string{
		"very likely",              // LND's hedge, kept as LND's
		"Very likely is as far as", // and not overridden
		"funding_expiry_blocks",    // whose count it is
		"--maxwaitnumblocksfundingconf gives up somewhere else",
		// The remedy, which does not depend on any of the above.
		"expect to force-close",
		"Never replace the transaction",
	} {
		if !strings.Contains(rep, want) {
			t.Errorf("the report does not say %q:\n%s", want, rep)
		}
	}

	// The claims themselves. Each of these asserted something about the peer
	// off a number this node computed on its own.
	for _, gone := range []string{
		"and that is what has happened",
		"the peer waited",
		"a channel the peer has forgotten",
	} {
		if strings.Contains(rep, gone) {
			t.Errorf("the report still tells the operator what the peer did, which "+
				"nothing here has observed: %q\n%s", gone, rep)
		}
	}
}

// TestTheDepthNoteSaysTheDesignRatherThanAFault is issue #21.
//
// The branch under test is the confirmation count, and Confs is -1 on every
// production run because the application sets no Chain and cannot. The copy
// named a Bitcoin Core this build has not dialled since item 5 — which reads as
// something to go and fix — and then offered a second cause, "or it has not seen
// the transaction", that nothing here asked anything about.
//
// The prediction paragraph is asserted on the same report, because that is the
// other half: State.line prints "expect ~6" on this branch, and the hedge that
// explains the number used to live on a branch that never fires.
func TestTheDepthNoteSaysTheDesignRatherThanAFault(t *testing.T) {
	report := paneCases()["pending with no depth reading"].Report()
	t.Logf("\n%s", report)
	rep := flat(report)

	for _, want := range []string{
		"expect ~6",
		"showing an expected depth instead, which is a prediction",
		"binds a peer running stock LND and nobody else",
		"this build dials no Bitcoin node",
		"not a fault to go and fix",
		"The channel leaving LND's pending list is the authoritative signal",
	} {
		if !strings.Contains(rep, want) {
			t.Errorf("the report does not say %q:\n%s", want, report)
		}
	}
	for _, gone := range []string{
		"Core is not connected",
		"has not seen the transaction",
		"without Core there is no way",
	} {
		if strings.Contains(rep, gone) {
			t.Errorf("the report still says %q, about a component this build "+
				"removed and a question it never asked:\n%s", gone, report)
		}
	}

	// And with a confirmation count — which only the harness can produce — the
	// prediction is still explained and the design paragraph is not printed.
	withDepth := flat(paneCases()["pending with a depth reading, and everything in the detail row"].Report())
	if !strings.Contains(withDepth, "which is a prediction") {
		t.Error("the prediction is unexplained when a depth count is present")
	}
	if strings.Contains(withDepth, "dials no Bitcoin node") {
		t.Error("a report carrying a confirmation count still says there is " +
			"nothing here that can count blocks")
	}
}

// TestThePeersOwnMinimumDepthIsNotShownAsAPrediction is issue #47.
//
// Five printed sentences said an initiator cannot read a peer's minimum_depth,
// and it can. The rot is not that the number was missing — it is that the number
// on screen was a guess described in the language of a fact, or the reverse. So
// what this pins is the *attribution*: a row showing the peer's own figure says
// the peer asked for it, a row showing LND's default policy says it expects it,
// and the note under them explains whichever ones the rows above actually
// carried.
//
// Both shapes appear in one batch — the reading is only available while the
// funding transaction is unconfirmed, so a channel that confirmed before the
// first poll has the prediction and nothing else — and the report has to be
// right about a batch that contains both.
func TestThePeersOwnMinimumDepthIsNotShownAsAPrediction(t *testing.T) {
	read := paneCases()["pending, showing the peer's own minimum_depth"]
	report := read.Report()
	t.Logf("\n%s", report)
	rep := flat(report)

	for _, want := range []string{
		"peer asked for 3 confirmations",
		"showing the depth its peer asked for, which is a reading and not a guess",
		"reports it back as confirmations_until_active",
		// The one hedge on the figure, which is LND's floor and not the peer's.
		"a peer that sent zero is stored as one",
	} {
		if !strings.Contains(rep, want) {
			t.Errorf("the report does not say %q:\n%s", want, report)
		}
	}
	// The prediction is not shown at all, and its note must not fire either: the
	// row carries the peer's own number and calling that a prediction is the
	// same defect one field over.
	for _, gone := range []string{
		"expect ~",
		"which is a prediction",
		"exposes it over no RPC",
		"ChannelAcceptResponse",
	} {
		if strings.Contains(rep, gone) {
			t.Errorf("the report describes a reading as a prediction: %q\n%s",
				gone, report)
		}
	}

	// A batch with one of each. Both rows keep their own attribution and both
	// notes render, because the operator is looking at two different numbers.
	mixed := &Result{
		Elapsed:     read.Elapsed,
		RetryWindow: read.RetryWindow,
		States: []State{
			read.States[0],
			paneCases()["pending with no depth reading"].States[0],
		},
	}
	both := flat(mixed.Report())
	t.Logf("\n%s", mixed.Report())
	for _, want := range []string{
		"peer asked for 3 confirmations",
		"expect ~6",
		"1 channel is showing the depth its peer asked for",
		"1 channel is showing an expected depth instead",
	} {
		if !strings.Contains(both, want) {
			t.Errorf("a mixed batch does not say %q:\n%s", want, mixed.Report())
		}
	}
}

// TestTheBatchLessonSurvivesWithoutAChain is the other half of issue #47.
//
// depthLesson never printed on a production run. Its only row came from
// ObservedDepth, which is read off Confs, and no production run has a Chain to
// read — so the section that says "what this batch learned" taught a real
// operator nothing, on every batch they have ever run. The peer's own figure
// needs no Chain, so it now does.
func TestTheBatchLessonSurvivesWithoutAChain(t *testing.T) {
	finished := paneCases()["finished, with what the batch learned"]
	// The production shape: no Chain, so no confirmation counts and no observed
	// depths anywhere in the batch.
	for i := range finished.States {
		finished.States[i].Confs = -1
		finished.States[i].ObservedDepth = 0
	}
	report := finished.Report()
	t.Logf("\n%s", report)
	rep := flat(report)

	if !strings.Contains(rep, "What this batch learned") {
		t.Fatalf("nothing was learned on a run with no Chain:\n%s", report)
	}
	for _, want := range []string{
		"asked for 3 confirmations (predicted 6)",
		"is the peer's own figure, read off PendingChannels",
	} {
		if !strings.Contains(rep, want) {
			t.Errorf("the lesson does not say %q:\n%s", want, report)
		}
	}
	// The peer with no reading contributes no row: a batch that learned nothing
	// about one peer must not print a line implying it did. Measured on the
	// lesson alone, because every peer has a row in the status list above it.
	_, lesson, _ := strings.Cut(rep, "What this batch learned:")
	if strings.Contains(lesson, short(paneMember(1).Peer)) {
		t.Errorf("a peer whose minimum_depth was never read has a row in the "+
			"lesson:\n%s", report)
	}
	if strings.Contains(lesson, "opened at") {
		t.Errorf("a run with no Chain reports a depth it never observed:\n%s",
			report)
	}
}

// flat collapses the wrapping so an assertion can be keyed on a sentence rather
// than on where prose.Wrap happened to break it. Without it these checks pass
// and fail on the column the copy was written to, which is not what any of them
// are about.
func flat(report string) string { return strings.Join(strings.Fields(report), " ") }
