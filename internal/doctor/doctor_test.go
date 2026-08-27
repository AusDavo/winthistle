package doctor

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/AusDavo/winthistle/internal/config"
	"github.com/AusDavo/winthistle/internal/journal"
	"github.com/AusDavo/winthistle/internal/lnd"
	"github.com/AusDavo/winthistle/internal/prose"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// journalWithRuns is n runs that reached StateArming and nothing further, in a
// database of their own. That is exactly the row a live batch writes: Begin
// records StateArming before anything is shown to a wallet, so nothing here can
// be distinguished from a batch being armed in another terminal right now — and
// that indistinguishability is the whole subject of these two tests.
//
// No node, no harness. checkJournal takes the journal as a parameter and this
// package is internal, so the check can be called directly.
func journalWithRuns(t *testing.T, n int) (*journal.Journal, *config.Config) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "runs.db")
	j, err := journal.Open(ctx, path)
	if err != nil {
		t.Fatalf("opening the journal: %v", err)
	}
	t.Cleanup(func() { j.Close() })

	for i := range n {
		id, err := lnd.NewPendingChanID()
		if err != nil {
			t.Fatalf("NewPendingChanID: %v", err)
		}
		runID := fmt.Sprintf("20260827-04344%d-9a8f28", i)
		err = j.Begin(ctx, runID, []journal.NewChannel{{
			PendingChanID: id,
			PeerPubkey:    fmt.Sprintf("02%062x", i+1),
			AmountSat:     2_000_000,
		}})
		if err != nil {
			t.Fatalf("Begin %s: %v", runID, err)
		}
	}
	return j, &config.Config{Journal: config.Journal{Path: path}}
}

// theOldClaim matches the sentence this check used to print: a count, the word
// "run" or "runs", and "stopped" asserted straight off it. The replacement still
// contains the word "stopped", inside a hedge, so an assertion on that word
// alone would be an assertion about nothing.
var theOldClaim = regexp.MustCompile(`\d+ runs? stopped`)

// TestAnUnfinishedRunIsNotReportedAsStopped.
//
// journal.Unfinished is `state NOT IN (published, aborted)`, and a run is in
// that set from the moment its streams open. So the report may say the journal
// never saw these runs finish, and may not say they stopped: a batch being armed
// in another terminal writes the identical row, and the journal carries no
// heartbeat that could tell the two apart. prose.RecoveryList already reads this
// query correctly; this is the same hedge on the other caller.
func TestAnUnfinishedRunIsNotReportedAsStopped(t *testing.T) {
	j, cfg := journalWithRuns(t, 3)

	r := &Report{}
	checkJournal(context.Background(), r, cfg, j, nil)

	// Flattened: the report wraps to the pane, so a Contains against a sentence
	// written as one line fails on the column rather than on the claim.
	flat := strings.Join(strings.Fields(r.Report()), " ")

	if theOldClaim.MatchString(flat) {
		t.Errorf("the report asserts these runs stopped, which the journal "+
			"cannot establish:\n%s", r.Report())
	}
	if !strings.Contains(flat, "unless something is driving one right now") &&
		!strings.Contains(flat, "Unless something is driving one right now") {
		t.Errorf("the report does not hedge a live run:\n%s", r.Report())
	}
	if !strings.Contains(flat, "neither published nor aborted") {
		t.Errorf("the report does not say what the journal established:\n%s",
			r.Report())
	}

	// The hedge must not have cost the list. An operator reading this needs the
	// ids, because the next command takes one.
	for _, run := range mustRuns(t, j) {
		if !strings.Contains(flat, run.ID) {
			t.Errorf("run %s is not in the report:\n%s", run.ID, r.Report())
		}
	}
}

func mustRuns(t *testing.T, j *journal.Journal) []*journal.Run {
	t.Helper()
	runs, err := j.Unfinished(context.Background())
	if err != nil {
		t.Fatalf("Unfinished: %v", err)
	}
	if len(runs) == 0 {
		t.Fatal("the fixture wrote no unfinished runs")
	}
	return runs
}

// TestTheTwoRefusalsThatLookAlike.
//
// Both carry bakery's plain "permission denied" text and they mean opposite
// things: InvalidArgument is CheckMacaroonPermissions answering about the
// macaroon in the request, and an untyped error with the same text is LND's
// interceptor refusing *this* call. Matching on the code alone would confuse
// them; matching on the text alone would too.
func TestTheTwoRefusalsThatLookAlike(t *testing.T) {
	cases := map[string]struct {
		err  error
		want bool
	}{
		"the interceptor refusing us": {
			// bakery.ErrPermissionDenied is errgo.New("permission denied") with
			// no gRPC status attached, so it arrives as codes.Unknown.
			err:  status.Error(codes.Unknown, "permission denied"),
			want: true,
		},
		"the answer about somebody else's macaroon": {
			err:  status.Error(codes.InvalidArgument, "permission denied"),
			want: false,
		},
		"a typed refusal, if LND ever starts sending one": {
			err:  status.Error(codes.PermissionDenied, "nope"),
			want: true,
		},
		"the node being down": {
			err:  status.Error(codes.Unavailable, "connection refused"),
			want: false,
		},
		"a plain error": {
			err:  errors.New("permission denied"),
			want: true,
		},
		"nothing at all": {err: nil, want: false},
	}
	for name, tc := range cases {
		if got := refusedOverMacaroon(tc.err); got != tc.want {
			t.Errorf("%s: refusedOverMacaroon = %v, want %v (%v)",
				name, got, tc.want, tc.err)
		}
	}
}

// TestTheReportStaysInThePane. These reports are read in a terminal beside
// something else, often while deciding whether to bring a cold wallet out.
func TestTheReportStaysInThePane(t *testing.T) {
	r := &Report{}

	ok := r.add(Check{Name: "LND"})
	ok.say("bitcoin regtest, lnd 0.21.2-beta, " +
		"02c6047f9441ed7d6d3045406e95c07cd85c778e4b8cef3ca7abac09b95c709ee5")

	bad := r.add(Check{Name: "the macaroon"})
	bad.fail("this credential can also do 3 things the tool promises it cannot, " +
		"which is what admin.macaroon looks like from here and is the whole " +
		"reason the never-list is checked against the file rather than against " +
		"this build's intentions")
	bad.say("    send coins on-chain — /lnrpc.Lightning/SendCoins")
	bad.fix("winthistle print-macaroon-command --save-to /home/someone/.lnd/" +
		"winthistle.macaroon | sh")

	// A real checkJournal report, rather than a hand-built stand-in of one. Every
	// other check in here is assembled by this test out of r.add and say, so the
	// sentences the checks themselves write were width-checked by nothing — and
	// this one's is three clauses over run-id rows that are already 40-odd
	// columns before the state is appended.
	j, cfg := journalWithRuns(t, 3)
	checkJournal(context.Background(), r, cfg, j, nil)

	for i, line := range strings.Split(r.Report(), "\n") {
		// A command is exempt, and deliberately: it has to be pasteable, and a
		// shell command cannot be wrapped without changing it. Everything the
		// operator *reads* is held to the pane; the one line they *paste* is not.
		if strings.HasPrefix(line, "  $ ") {
			continue
		}
		// Runes, not bytes. This copy is full of em dashes, and a byte count
		// reports a line as three columns wider than it renders — which is worse
		// than no check, because it is the kind of wrongness that gets fixed by
		// widening the pane. Every other pane test in the repository counts
		// runes; this one did not.
		if n := len([]rune(line)); n > prose.PaneWidth {
			t.Errorf("line %d is %d columns, past the %d-column pane:\n%s",
				i+1, n, prose.PaneWidth, line)
		}
	}
	if r.OK() {
		t.Error("a report with a failure in it says it is OK")
	}
	if !strings.Contains(r.Report(), "Not ready") {
		t.Error("a failing report does not open by saying so")
	}
}

// TestAWarningIsNotAFailure. A warning never stops a run; if something should
// stop a run it is a Fail, and the difference is the whole vocabulary here.
func TestAWarningIsNotAFailure(t *testing.T) {
	r := &Report{}
	c := r.add(Check{Name: "the coins"})
	c.warn("2 coins excluded")
	if !r.OK() {
		t.Error("a warning made the report say the setup is unusable")
	}
	// And a warning after a failure must not downgrade it.
	c.fail("nothing to fund a batch with")
	c.warn("also this")
	if r.Checks[0].Status != Fail {
		t.Errorf("a warning downgraded a failure to %s", r.Checks[0].Status)
	}
}
