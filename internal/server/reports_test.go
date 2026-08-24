package server

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

// reporting is a server whose three pre-flight reports answer, with text long
// enough to tell one from another on a page.
func reporting(t *testing.T) (*Server, *stubLauncher) {
	t.Helper()
	l := &stubLauncher{
		batch:  "3 channels, 15 000 000 sat",
		wallet: "winthistle-cold",
		peers: "alice\n03aa\n\n  connection    already connected\n" +
			"  channels      4\n",
		fees: "Fee rate: 12.00 sat/vB, from Core's estimate.\n\n" +
			"  Core's estimate  12.00 sat/vB (economical, 6 blocks)\n",
		reserve: "This node's own wallet clears the anchor reserve.\n\n" +
			"  LND will require at verify   10 000 sat\n",
	}
	return launching(t, l), l
}

// The three paths, and the text each one is supposed to be carrying.
var reportScreens = []struct {
	path string
	want func(*stubLauncher) string
}{
	{"/peers", func(l *stubLauncher) string { return l.peers }},
	{"/fees", func(l *stubLauncher) string { return l.fees }},
	{"/reserve", func(l *stubLauncher) string { return l.reserve }},
}

// TestTheReportsAreServedVerbatim is decision 3 on the three newest screens.
//
// It is the same property TestTheTextIsTheOracle fixes over screen(), asserted
// through the handlers so that a screen which built its own version of a
// Report() — rewrapped, summarised, or merely reordered — fails here rather than
// drifting quietly from the text `winthistle run` prints for the same check.
func TestTheReportsAreServedVerbatim(t *testing.T) {
	s, l := reporting(t)

	for _, screen := range reportScreens {
		w := serveIt(s, get(t, s, screen.path))
		if w.Code != http.StatusOK {
			t.Fatalf("%s got %d:\n%s", screen.path, w.Code, w.Body.String())
		}
		if got := pre(t, w.Body.String()); !strings.Contains(got, screen.want(l)) {
			t.Errorf("%s did not serve the check's own text verbatim.\n got: %q\n"+
				"want it to contain: %q", screen.path, got, screen.want(l))
		}
	}
}

// TestTheReportScreensFitThePane, measured on the served page rather than on the
// copy functions.
//
// The composition is what overflows — a note, then a heading, then a report — and
// three screens in a row have shipped over the pane in this package because
// nobody measured the whole page. The report itself is exempt from being blamed
// here only in the sense that it is measured in its own package: the stub's text
// stands in for it, so what this measures is this package's contribution.
func TestTheReportScreensFitThePane(t *testing.T) {
	s, _ := reporting(t)

	for _, screen := range reportScreens {
		fitsThePane(t, screen.path, pre(t, serveIt(s, get(t, s, screen.path)).Body.String()))
	}

	// And with a batch going, which adds a paragraph to every one of them.
	s.Runs.addKind("20260824-193012-9f3a1c", KindBatch, "")
	for _, screen := range reportScreens {
		fitsThePane(t, screen.path+" while a batch is going",
			pre(t, serveIt(s, get(t, s, screen.path)).Body.String()))
	}
}

// TestAReportScreenIsKindAwareAboutALiveRun is server.Run.Kind's trap, on the
// three screens that inherit it.
//
// Almost every sentence one of these screens could say about a batch is false
// about a setup: a setup opens no channel, connects to no peer, spends nothing
// and pays no fee. So the note is written per kind, and the case that has to hold
// is the negative one — a setup must not put a paragraph about pending channels
// or an unrevisable fee rate on a screen it has nothing to do with.
func TestAReportScreenIsKindAwareAboutALiveRun(t *testing.T) {
	batch := &Run{ID: "20260824-193012-9f3a1c"}
	bump := &Run{ID: "20260824-194500-aabbcc", Kind: KindBump, About: "20260824-1930"}
	setup := &Run{ID: "20260824-195000-ddeeff", Kind: KindSetup, About: "winthistle-cold"}

	mustSay(t, peersNote(batch), "Run 20260824-193012-9f3a1c is opening the batch",
		"including its own")
	mustSay(t, peersNote(bump), "they are the batch being accelerated")
	mustSay(t, feesNote(batch), "there is no RBF on a funding transaction (I-4)")
	mustSay(t, feesNote(bump), "The child's own rate is not this number")
	mustSay(t, reserveNote(batch), "The reading that stays true throughout is the "+
		"available balance")
	mustSay(t, reserveNote(bump), "a batch that would come after them")

	for what, note := range map[string]string{
		"the peers":          peersNote(setup),
		"the fee rate":       feesNote(setup),
		"the anchor reserve": reserveNote(setup),
	} {
		if note != "" {
			t.Errorf("%s says something about a live setup, which opens no channel, "+
				"pays no fee and touches this node's wallet not at all:\n%s", what, note)
		}
	}
	for what, note := range map[string]string{
		"the peers":          peersNote(nil),
		"the fee rate":       feesNote(nil),
		"the anchor reserve": reserveNote(nil),
	} {
		if note != "" {
			t.Errorf("%s says something about a run when none is going:\n%s", what, note)
		}
	}
}

// TestTheReportsNeverConnectToAPeer is the one leg of these three checks that
// would change the node.
//
// peers.Check dials a peer whose Want carries a host, and `winthistle doctor`
// strips the hosts unless it is given --connect. There is no --connect on a
// screen, so internal/webrun strips them unconditionally — and this is the
// sentence that says a screen must never grow one.
func TestTheReportsNeverConnectToAPeer(t *testing.T) {
	s, _ := reporting(t)

	body := serveIt(s, get(t, s, "/peers")).Body.String()
	if strings.Contains(body, "<form") || strings.Contains(body, "<button") {
		t.Errorf("/peers carries a control; these screens read and nothing else:\n%s",
			body)
	}
	for _, path := range []string{"/peers", "/fees", "/reserve"} {
		r := post(t, s, path, nil)
		if w := serveIt(s, r); w.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s got %d, want 405 — the report screens are read-only",
				path, w.Code)
		}
	}
}

// TestThePeerReportRefusesWithoutABatchAndTheOtherTwoDoNot is the asymmetry
// worth a test, because it is the one that could be smoothed away by accident.
//
// The peers are the batch's, so with no batch there is nobody to look up. The fee
// rate is the market's and the reserve is this node's own wallet's, so both
// answer — and the reserve says which of the two questions it answered, because
// its report is written about "this batch" and with no batch that phrase names a
// channel nobody asked for.
func TestThePeerReportRefusesWithoutABatchAndTheOtherTwoDoNot(t *testing.T) {
	l := &stubLauncher{
		fees:    "Fee rate: 12.00 sat/vB, from Core's estimate.\n",
		reserve: "This node's own wallet clears the anchor reserve.\n",
	}
	s := launching(t, l)

	w := serveIt(s, get(t, s, "/peers"))
	if w.Code != http.StatusNotImplemented {
		t.Errorf("/peers with no batch got %d, want 501", w.Code)
	}
	mustSay(t, pre(t, w.Body.String()),
		"There is no batch, and this report is about one",
		"Nothing was asked of the node",
		"`serve --batch FILE`")

	for _, path := range []string{"/fees", "/reserve"} {
		if w := serveIt(s, get(t, s, path)); w.Code != http.StatusOK {
			t.Errorf("%s with no batch got %d, want 200 — it does not need one",
				path, w.Code)
		}
	}
	mustSay(t, pre(t, serveIt(s, get(t, s, "/reserve")).Body.String()),
		"for one announced channel rather than for anything you have asked for",
		"will not clear it for several")
}

// TestAReportThatCouldNotBeMadeSaysSoAndPointsSomewhere.
//
// The zero value of a report is an empty one, and an empty report on a screen
// headed "the anchor reserve" reads as "nothing to worry about" — the same class
// of mistake as an empty journal listing claiming a clean node. It has to say the
// check was not made, and it has to point at a screen the nav can reach: copy
// that names one it cannot has been found four times in this UI.
func TestAReportThatCouldNotBeMadeSaysSoAndPointsSomewhere(t *testing.T) {
	s, l := reporting(t)
	l.reportErr = errors.New("connecting to LND at 127.0.0.1:10009: " +
		"connection refused")

	for _, path := range []string{"/peers", "/fees", "/reserve"} {
		w := serveIt(s, get(t, s, path))
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s with the node down got %d, want 503", path, w.Code)
		}
		got := pre(t, w.Body.String())
		mustSay(t, got,
			"This check could not be made",
			"connection refused",
			"Nothing was changed by the attempt",
			"The doctor screen runs this same check")
		if !navReaches(t, w.Body.String(), "/doctor") {
			t.Errorf("%s points at the doctor screen and the nav does not carry it:"+
				"\n%s", path, w.Body.String())
		}
	}
}

// TestTheNavCarriesEveryReportScreen is the dead-end guard, mechanised.
//
// A screen reachable only from another page is how "see the peer report" became
// copy naming somewhere an operator could not get to, four times. Every path in
// the nav has to answer, and every screen this file adds has to be in the nav.
func TestTheNavCarriesEveryReportScreen(t *testing.T) {
	s, _ := reporting(t)

	for _, path := range []string{"/peers", "/fees", "/reserve"} {
		if !navReaches(t, serveIt(s, get(t, s, "/")).Body.String(), path) {
			t.Errorf("the nav does not carry %s, so nothing links to it", path)
		}
	}
	// And every nav entry answers, which is the other half: an entry pointing at
	// a route that does not exist is a 404 in the chrome of every page.
	for _, l := range nav {
		if w := serveIt(s, get(t, s, l.Path)); w.Code == http.StatusNotFound {
			t.Errorf("the nav carries %s and the router has no such route", l.Path)
		}
	}
}

// navReaches reports whether the page's nav links to a path.
func navReaches(t *testing.T, body, path string) bool {
	t.Helper()
	_, rest, ok := strings.Cut(body, "<nav>")
	if !ok {
		t.Fatalf("no <nav> in the page:\n%s", body)
	}
	inner, _, ok := strings.Cut(rest, "</nav>")
	if !ok {
		t.Fatalf("unterminated <nav> in the page:\n%s", body)
	}
	return strings.Contains(inner, `href="`+path+`"`)
}
