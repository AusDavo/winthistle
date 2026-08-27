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
		Detail: "time lock delta of 4 is too small, minimum supported is 18 and " +
			"the maximum is 2016",
	}
	unexplained := PolicyOutcome{
		Refused: true,
		Reason:  lnrpc.UpdateFailure_UPDATE_FAILURE_UNKNOWN,
		Detail:  "could not update policies",
	}

	return map[string]*Result{
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
		// The production shape: nothing open yet, no depth reported because
		// there is no Bitcoin node to report one, and the horizon still a long
		// way off.
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

// flat collapses the wrapping so an assertion can be keyed on a sentence rather
// than on where prose.Wrap happened to break it. Without it these checks pass
// and fail on the column the copy was written to, which is not what any of them
// are about.
func flat(report string) string { return strings.Join(strings.Fields(report), " ") }
