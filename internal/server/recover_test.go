package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AusDavo/winthistle/internal/prose"
)

// says is strings.Contains over copy that has been wrapped.
//
// Every screen in this repository is wrapped to a fixed column before it is
// served, so a phrase an operator reads as one sentence is several lines in the
// string. Collapsing the whitespace on both sides is what lets a test assert on
// the sentence rather than on where prose.Wrap happened to break it.
func says(text, phrase string) bool {
	return strings.Contains(strings.Join(strings.Fields(text), " "),
		strings.Join(strings.Fields(phrase), " "))
}

func mustSay(t *testing.T, text string, phrases ...string) {
	t.Helper()
	for _, phrase := range phrases {
		if !says(text, phrase) {
			t.Errorf("the screen does not say %q:\n%s", phrase, text)
		}
	}
}

// journalling is a server whose journal has one unfinished run in it.
func journalling(t *testing.T) (*Server, *stubLauncher) {
	t.Helper()
	l := &stubLauncher{
		unfinished: "1 unfinished run in the journal. It stopped somewhere it " +
			"should not have.\n\n  20260824-1930  armed       9h0m0s ago\n" +
			"                 3 pending\n",
		ids: []string{"20260824-1930"},
		journalled: map[string]string{
			"20260824-1930": "Run 20260824-1930 — armed, last touched 9h0m0s ago.\n\n" +
				"An abort of this run would:\n  - Abandon 3 channels.\n",
		},
	}
	return launching(t, l), l
}

// TestTheJournalScreenSaysNothingBelowItIsHappening is decision 1, on the screen
// where nothing is running.
//
// The registry and the journal overlap and an operator who reads a journal row
// as "this is running" waits for something that is not happening. So every
// rendering of a journal row is prefixed by what this process knows, and the
// no-run case is the one that has to say so out loud rather than by omission.
func TestTheJournalScreenSaysNothingBelowItIsHappening(t *testing.T) {
	s, _ := journalling(t)

	w := serveIt(s, get(t, s, "/recover"))
	if w.Code != http.StatusOK {
		t.Fatalf("the journal screen got %d", w.Code)
	}
	got := pre(t, w.Body.String())
	mustSay(t, got,
		"the run journal",
		"Nothing is running in this process",
		"None of it is happening now",
		"1 unfinished run in the journal")
	if !strings.Contains(w.Body.String(), `href="/recover/20260824-1930"`) {
		t.Errorf("the journal screen does not link its rows:\n%s", w.Body.String())
	}
}

// TestALiveRunIsNamedAboveItsJournalRow is the overlap, stated.
//
// A run started from this UI is in both places for its whole life, and its
// journal row is always a little behind it. Saying which row that is, and where
// the live screen for it is, is what stops the two blurring.
func TestALiveRunIsNamedAboveItsJournalRow(t *testing.T) {
	s, _ := journalling(t)
	s.Runs.add("20260824-1930")

	got := pre(t, serveIt(s, get(t, s, "/recover")).Body.String())
	mustSay(t, got,
		"Run 20260824-1930 is going in this process right now",
		"a snapshot, not a view of the run",
		"/runs/20260824-1930")
	if says(got, "Nothing is running in this process") {
		t.Errorf("the journal screen says nothing is running while a run is:\n%s", got)
	}
}

// TestAnEmptyJournalDoesNotClaimACleanNodeWhileARunIsGoing is the case worth
// building the whole framing for.
//
// journal.Begin has not written a row when a run has only just started, so
// prose.RecoveryList renders "No unfinished runs. Nothing to recover." over a
// batch that is being armed. That sentence is true about the journal and false
// about the node, and it is exactly the half-truth that produces a second
// incident — the same failure mode as listing runs without listing the CPFP
// children.
func TestAnEmptyJournalDoesNotClaimACleanNodeWhileARunIsGoing(t *testing.T) {
	l := &stubLauncher{unfinished: prose.Para("No unfinished runs. Nothing to recover.")}
	s := launching(t, l)
	s.Runs.add("20260824-1935")

	mustSay(t, pre(t, serveIt(s, get(t, s, "/recover")).Body.String()),
		"Run 20260824-1935 is going in this process right now",
		"it is not in the list below",
		"has not written a row to this journal yet",
		"does not mean this node is clean")
}

// TestTheJournalScreenOffersNoAbort is decision 3, and it is mechanical rather
// than a promise in the copy.
//
// run.RecoverOne would be the fourth unsafe method and the first that abandons
// channels on a run this process never started. It asks abort.Confirmation once
// per channel, a browser answers those through a Run, and a journalled run has
// none — so this ships the listing and points at the command that does the rest.
// There is no POST route under /recover, which is what makes a control somebody
// adds later a 405 rather than a silent handler.
func TestTheJournalScreenOffersNoAbort(t *testing.T) {
	s, _ := journalling(t)

	for _, path := range []string{"/recover", "/recover/20260824-1930"} {
		body := serveIt(s, get(t, s, path)).Body.String()
		if strings.Contains(body, "<form") || strings.Contains(body, "<button") {
			t.Errorf("%s carries a control:\n%s", path, body)
		}
	}

	// The routes themselves. A POST is a 405 from the mux rather than anything
	// this package had to remember to refuse.
	for _, path := range []string{"/recover", "/recover/20260824-1930"} {
		r := httptest.NewRequest(http.MethodPost, path, nil)
		r.Host = "127.0.0.1:7420"
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		r.AddCookie(&http.Cookie{Name: cookieName, Value: s.token})
		if w := serveIt(s, r); w.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s got %d, want 405 — the journal screens are read-only",
				path, w.Code)
		}
	}

	mustSay(t, pre(t, serveIt(s, get(t, s, "/recover/20260824-1930")).Body.String()),
		"This screen is read-only",
		"`winthistle recover 20260824-1930`",
		"one question per channel")
}

// TestTheJournalScreenAsksTheJournalWhetherARunMayBeAbortedAtAll is decision 3's
// other half, and it is the same guard the live abort control has.
//
// This screen never aborts anything, so the question is not "may I act" but
// "should I point at the command that would". The answer must come from
// journal.Run.AbortTarget through Launcher.AbortRefusal — the same function
// run.RecoverOne refuses on — rather than from a copy of that rule kept here.
// The import ban is what makes asking the only route: this package cannot name
// journal.ErrMayBePublished.
func TestTheJournalScreenAsksTheJournalWhetherARunMayBeAbortedAtAll(t *testing.T) {
	s, l := journalling(t)
	l.refusal = errors.New("run 20260824-1930 is publishing and funds 3 " +
		"channel(s): it may already be public")

	got := pre(t, serveIt(s, get(t, s, "/recover/20260824-1930")).Body.String())
	mustSay(t, got,
		"must not be taken apart at all",
		"it may already be public",
		"That refusal is the journal's rather than this page's")
	if says(got, "is what takes it apart") {
		t.Errorf("the screen pointed at the command as though it would work on a "+
			"run the journal refuses:\n%s", got)
	}
}

// TestAJournalThatCannotBeReadDoesNotSayThereIsNothingToRecover.
//
// The zero value of a listing is an empty one, and an empty listing on this
// screen reads as "your node is clean". A journal that could not be opened has
// to say it does not know, which is a different fact and the one worth acting
// on.
func TestAJournalThatCannotBeReadDoesNotSayThereIsNothingToRecover(t *testing.T) {
	l := &stubLauncher{journalErr: errors.New("runs.db: permission denied")}
	s := launching(t, l)

	for _, path := range []string{"/recover", "/recover/anything"} {
		w := serveIt(s, get(t, s, path))
		if w.Code != http.StatusInternalServerError {
			t.Errorf("%s with an unreadable journal got %d, want 500", path, w.Code)
		}
		got := pre(t, w.Body.String())
		mustSay(t, got, "could not be read", "permission denied", "it does not know")
		if says(got, "Nothing to recover") {
			t.Errorf("%s claimed there was nothing to recover:\n%s", path, got)
		}
	}
}

// TestAJournalledRunThatIsNotThereIs404. A run that finished or was aborted
// cleanly is not unfinished, so it is not on the list — and saying that is the
// difference between "no such run" and "this screen only shows some of them".
func TestAJournalledRunThatIsNotThereIs404(t *testing.T) {
	s, _ := journalling(t)

	w := serveIt(s, get(t, s, "/recover/never-happened"))
	if w.Code != http.StatusNotFound {
		t.Fatalf("an unknown journalled run got %d, want 404", w.Code)
	}
	mustSay(t, pre(t, w.Body.String()), "no run called", "aborted cleanly")
}

// TestTheJournalScreensGoThroughTheChokepoint. Everything on these screens is
// read off disk and none of it was chosen by this repository — a run id, LND's
// error text, a peer pubkey — so decision 3's escaping applies here as much as
// to a transcript.
func TestTheJournalScreensGoThroughTheChokepoint(t *testing.T) {
	l := &stubLauncher{
		unfinished: "1 unfinished run: <script>alert(1)</script>\n",
		ids:        []string{`a"b<c`},
		journalled: map[string]string{`a"b<c`: "Run <b>a</b>, last touched 1s ago.\n"},
	}
	s := launching(t, l)

	body := serveIt(s, get(t, s, "/recover")).Body.String()
	if strings.Contains(body, "<script>") {
		t.Errorf("the listing reached the page unescaped:\n%s", body)
	}
	if !strings.Contains(pre(t, body), "<script>alert(1)</script>") {
		t.Errorf("the listing did not survive the page verbatim:\n%s", pre(t, body))
	}
	// The link is built from an id off the journal, so it is escaped for the
	// path and for the attribute it sits in.
	if strings.Contains(body, `href="/recover/a"b<c"`) {
		t.Errorf("a run id broke out of its href:\n%s", body)
	}

	one := serveIt(s, get(t, s, "/recover/"+`a"b<c`)).Body.String()
	if strings.Contains(one, "<b>a</b>") {
		t.Errorf("the run screen reached the page unescaped:\n%s", one)
	}
}

// TestTheServersOwnCopyFitsThePane is the check this package did not have.
//
// Every report package in this repository measures its screens against the pane,
// except that internal/plan did not and shipped a 79-column line in the plan
// verification — the one document an operator approves before the cold wallet
// comes out. This package writes operator copy too: the overview, the refusals,
// the abort warning, and now the two paragraphs that keep a journal row from
// being read as a live run. It is measured here for the same reason.
//
// Runes, not bytes. A byte count reads every em dash as three columns, which is
// worse than no check because it gets fixed by widening the pane.
// aQuestion is a pending question with the clock a screen renders, and nothing
// else: waitingOn reads only Asked and Deadline.
func aQuestion() *Question {
	asked := time.Date(2026, 8, 24, 19, 30, 12, 0, time.UTC)
	return &Question{Asked: asked, Deadline: asked.Add(5 * time.Minute)}
}

func TestTheServersOwnCopyFitsThePane(t *testing.T) {
	live := &Run{ID: "20260824-193012-9f3a1c"}
	refusal := errors.New("run 20260824-193012-9f3a1c is publishing and funds 3 " +
		"channel(s) on 0f0e0d0c0b0a09080706050403020100f0e0d0c0b0a090807060504030201000: " +
		"it may already be public — check whether it is in the mempool or a block " +
		"before touching anything")

	screens := map[string]string{
		"overview":            overview("/home/someone/.config/winthistle/winthistle.toml"),
		"no batch":            noBatch(),
		"stale answer":        staleAnswer(),
		"already running":     alreadyRunning(ErrRunInFlight, live),
		"abort warning":       abortWarning(live.ID, false),
		"abort warning again": abortWarning(live.ID, true),
		"abort refused":       abortRefused(live.ID, refusal),
		"unwinding":           unwinding(live.ID),
		"still unwinding":     stillUnwinding(live.ID),

		"setup intro":   setupIntro("winthistle-cold"),
		"setup busy":    setupBusy(&Run{ID: "20260824-193012-9f3a1c", Kind: KindSetup, About: "winthistle-cold"}),
		"setup refused": setupRefused(ErrRunInFlight, live),
		"no setup":      noSetup(),
		"not a batch":   notABatch(&Run{ID: "20260824-193012-9f3a1c", Kind: KindSetup, About: "winthistle-cold"}),
		"the way out, setup": wayOut(
			&Run{ID: live.ID, Kind: KindSetup, About: "winthistle-cold"}),
		"the way out, bump": wayOut(
			&Run{ID: live.ID, Kind: KindBump, About: "20260824-1930"}),
		"not a batch, bump": notABatch(
			&Run{ID: live.ID, Kind: KindBump, About: "20260824-1930"}),
		"heading, bump": heading(&Run{ID: live.ID, Kind: KindBump, About: "20260824-1930"}),
		"nothing to start, bump": nothingToStart(
			&Run{ID: live.ID, Kind: KindBump, About: "20260824-1930"}),
		"bump intro":              bumpIntro("20260824-1930"),
		"bump busy":               bumpBusy(&Run{ID: live.ID, Kind: KindBump, About: "20260824-1930"}),
		"bump refused":            bumpRefused(ErrRunInFlight, live),
		"no bump":                 noBump(),
		"waiting, batch":          waitingOn(live, aQuestion()),
		"waiting, setup":          waitingOn(&Run{ID: live.ID, Kind: KindSetup, About: "winthistle-cold"}, aQuestion()),
		"nothing to start, batch": nothingToStart(live),
		"nothing to start, setup": nothingToStart(
			&Run{ID: live.ID, Kind: KindSetup, About: "winthistle-cold"}),
		"heading, batch": heading(live),
		"heading, setup": heading(&Run{ID: live.ID, Kind: KindSetup, About: "winthistle-cold"}),

		"journal note, nothing running": journalNote(nil, nil),
		"journal note, run listed": journalNote(live,
			[]string{live.ID, "20260824-1930"}),
		"journal note, run not listed": journalNote(live, nil),
		"journal note, bump running": journalNote(
			&Run{ID: live.ID, Kind: KindBump, About: "20260824-1930"},
			[]string{"20260824-1930"}),
		"journal note, bump of a run not listed": journalNote(
			&Run{ID: live.ID, Kind: KindBump, About: "20260824-1930"}, nil),
		"journal note, setup running": journalNote(
			&Run{ID: live.ID, Kind: KindSetup, About: "winthistle-cold"}, nil),
		"journalled note, live":     journalledNote(live.ID, live),
		"journalled note, not live": journalledNote(live.ID, nil),
		"read-only note":            readOnlyNote(live.ID, nil),
		"read-only note, refused":   readOnlyNote(live.ID, refusal),
		"no such journalled run":    noSuchJournalledRun(live.ID),
		"no journal":                noJournal(),
		"journal unreadable":        journalUnreadable(refusal),

		"peers, always":         peersAlways(),
		"fees, always":          feesAlways(),
		"reserve, always":       reserveAlways(),
		"peers note, batch":     peersNote(live),
		"peers note, bump":      peersNote(&Run{ID: live.ID, Kind: KindBump, About: "20260824-1930"}),
		"peers note, setup":     peersNote(&Run{ID: live.ID, Kind: KindSetup, About: "winthistle-cold"}),
		"peers note, nothing":   peersNote(nil),
		"fees note, batch":      feesNote(live),
		"fees note, bump":       feesNote(&Run{ID: live.ID, Kind: KindBump, About: "20260824-1930"}),
		"fees note, setup":      feesNote(&Run{ID: live.ID, Kind: KindSetup, About: "winthistle-cold"}),
		"reserve note, batch":   reserveNote(live),
		"reserve note, bump":    reserveNote(&Run{ID: live.ID, Kind: KindBump, About: "20260824-1930"}),
		"reserve note, setup":   reserveNote(&Run{ID: live.ID, Kind: KindSetup, About: "winthistle-cold"}),
		"one announced channel": hypotheticalChannel(),
		"no batch for a report": noBatchFor("the peers"),
		"report failed":         reportFailed("the fee rate", refusal),
		"no reports":            noReports("the anchor reserve"),
	}
	for name, text := range screens {
		for i, line := range strings.Split(text, "\n") {
			if n := len([]rune(line)); n > prose.PaneWidth {
				t.Errorf("%s line %d is %d columns, over the %d-column pane:\n%s",
					name, i+1, n, prose.PaneWidth, line)
			}
		}
	}
}
