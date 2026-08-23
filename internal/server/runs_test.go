package server

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

// TestAClosingTabDoesNotTouchTheRun is decision 2, stated as a test.
//
// The request context is what a closing tab cancels, and this is the assertion
// that nothing hangs off it: a run written to, attached to, abandoned mid-render
// and attached to again is the same run with the same transcript. There is no
// per-connection state, so there is nothing for a lost connection to end.
//
// What this cannot test is the absence of a future mistake — a handler that
// plumbs r.Context() into run.Do would pass every test here and abort a batch on
// a laptop lid. The guard against that is the shape: run.Do is started from a
// goroutine with a context that is not the request's, and Registry is the only
// place a run is reachable from. That is written down in this package's comment
// and in HANDOFF.md, because it is the most dangerous part of the change.
func TestAClosingTabDoesNotTouchTheRun(t *testing.T) {
	s := testServer(t)
	run := s.Runs.Add("run-1")
	run.Write([]byte("Phase 0\n"))

	// First tab attaches.
	w := serveIt(s, get(t, s, "/runs/run-1"))
	if w.Code != http.StatusOK {
		t.Fatalf("the first attach got %d", w.Code)
	}
	if !strings.Contains(pre(t, w.Body.String()), "Phase 0") {
		t.Fatal("the first attach did not show the transcript")
	}

	// That tab goes away — a reload, a lid, a blip. The run keeps talking.
	run.Write([]byte("Phase 1: armed\n"))

	// A second tab, with the same token, attaches to the same run and gets the
	// whole transcript from the beginning rather than what it did not miss.
	got := pre(t, serveIt(s, get(t, s, "/runs/run-1")).Body.String())
	for _, want := range []string{"Phase 0", "Phase 1: armed", "still going"} {
		if !strings.Contains(got, want) {
			t.Errorf("the second attach is missing %q:\n%s", want, got)
		}
	}
}

// TestTheTokenIsJoinablePerRunRatherThanPerTab.
//
// This is the consequence decision 2 has to build for. If the token were per
// tab, a reconnecting browser would be a new session and the live run would be
// unreachable from it — which would make "a closing tab does not abort" a
// promise with no way to collect on it. One token per server start, and a run
// addressed by its journal id, is what makes the reconnect an attach.
func TestTheTokenIsJoinablePerRunRatherThanPerTab(t *testing.T) {
	s := testServer(t)
	s.Runs.Add("run-a").Write([]byte("a\n"))
	s.Runs.Add("run-b").Write([]byte("b\n"))

	// Two different "tabs" are only two requests carrying the same token.
	for _, id := range []string{"run-a", "run-b", "run-a"} {
		w := serveIt(s, get(t, s, "/runs/"+id))
		if w.Code != http.StatusOK {
			t.Fatalf("attaching to %s got %d", id, w.Code)
		}
	}

	// And the index lists them, so a tab that came back with no URL can find
	// them again.
	body := serveIt(s, get(t, s, "/")).Body.String()
	for _, id := range []string{"run-a", "run-b"} {
		if !strings.Contains(body, `href="/runs/`+id+`"`) {
			t.Errorf("the index does not link %s:\n%s", id, body)
		}
	}
}

// TestAFinishedRunSaysSoAndKeepsItsTranscript. A run that stopped is the case an
// operator is most likely to be reading, and a transcript with no verdict on it
// is the screen that gets misread.
func TestAFinishedRunSaysSoAndKeepsItsTranscript(t *testing.T) {
	s := testServer(t)
	run := s.Runs.Add("run-x")
	run.Write([]byte("Phase 0\n"))
	run.Finish(errors.New("peer 2 of 3 refused: capacity below its minimum"))

	got := pre(t, serveIt(s, get(t, s, "/runs/run-x")).Body.String())
	if !strings.Contains(got, "It stopped: peer 2 of 3 refused") {
		t.Errorf("the run screen does not say what stopped it:\n%s", got)
	}
	if !strings.Contains(got, "Phase 0") {
		t.Errorf("the transcript was dropped when the run ended:\n%s", got)
	}
}

// TestARunFromAnEarlierProcessIsNotHere says the thing an operator will
// otherwise conclude for themselves and get wrong. The registry is per process;
// the journal is what outlives one, and `winthistle recover` is what reads it.
func TestARunFromAnEarlierProcessIsNotHere(t *testing.T) {
	s := testServer(t)
	w := serveIt(s, get(t, s, "/runs/from-yesterday"))
	if w.Code != http.StatusNotFound {
		t.Fatalf("an unknown run got %d, want 404", w.Code)
	}
	if got := pre(t, w.Body.String()); !strings.Contains(got, "winthistle recover") {
		t.Errorf("the refusal does not say where the run actually is:\n%s", got)
	}
}

// TestRunsAreListedNewestFirst: a run started a moment ago is what the operator
// came back for, and one from ten minutes ago is history.
func TestRunsAreListedNewestFirst(t *testing.T) {
	s := testServer(t)
	first := s.Runs.Add("first")
	second := s.Runs.Add("second")
	third := s.Runs.Add("third")
	// Add stamps time.Now(), and three calls in a row can land on the same
	// instant on a coarse clock. Spread them so the order is the ordering rather
	// than the map.
	first.Started = second.Started.Add(-2 * 1e9)
	third.Started = second.Started.Add(2 * 1e9)

	list := s.Runs.List()
	if len(list) != 3 {
		t.Fatalf("List returned %d runs", len(list))
	}
	for i, want := range []string{"third", "second", "first"} {
		if list[i].ID != want {
			t.Errorf("List()[%d] = %s, want %s", i, list[i].ID, want)
		}
	}
}
