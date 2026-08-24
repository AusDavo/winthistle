package server

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestTheBumpScreenFitsThePane, measured on the page for the reason the setup
// screen's is: the composition is what has overflowed, not the paragraphs.
func TestTheBumpScreenFitsThePane(t *testing.T) {
	s, _ := setupServer(t)

	w := serveIt(s, get(t, s, "/bump/20260824-1930"))
	if w.Code != http.StatusOK {
		t.Fatalf("the bump screen got %d:\n%s", w.Code, w.Body.String())
	}
	fitsThePane(t, "/bump/{id}", pre(t, w.Body.String()))

	s.Runs.add("20260824-1930")
	w = serveIt(s, get(t, s, "/bump/20260824-1930"))
	fitsThePane(t, "/bump/{id} while a batch is going", pre(t, w.Body.String()))
}

// TestStartingABumpGivesApproveItsCaller.
//
// bump.Approve was the last of the four callback seams with no caller. This is
// the assertion that it has one, and that what it is handed says which batch it
// is about — a bump takes a run id rather than a txid because the journal is what
// says a transaction was published, what its change output was, and whether an
// earlier child is already holding a coin lock.
func TestStartingABumpGivesApproveItsCaller(t *testing.T) {
	s, l := setupServer(t)
	l.block = make(chan struct{})
	defer close(l.block)

	w := serveIt(s, post(t, s, "/bump/20260824-1930", url.Values{
		"target": {"12.5"}, "build-only": {"1"},
	}))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("starting a bump got %d:\n%s", w.Code, w.Body.String())
	}
	l.runCtx(t)

	l.mu.Lock()
	n, req := l.bumps, l.bumpReq
	l.mu.Unlock()
	if n != 1 {
		t.Fatalf("StartBump was called %d times, want 1", n)
	}
	if req.RunID != "20260824-1930" {
		t.Errorf("the bump is about run %q, not the one in the path", req.RunID)
	}
	if req.TargetSatPerVB != 12.5 {
		t.Errorf("the target came through as %v, want 12.5", req.TargetSatPerVB)
	}
	if !req.BuildOnly {
		t.Error("the dry-run box was ticked and did not reach the launcher")
	}

	live := s.Runs.Live()
	if live == nil {
		t.Fatal("the bump is not in the registry, so no tab can come back to it")
	}
	if live.Kind != KindBump {
		t.Errorf("the bump went into the registry as kind %d, not KindBump", live.Kind)
	}
	if live.About != "20260824-1930" {
		t.Errorf("the run does not name the batch it accelerates: %q", live.About)
	}
	if live.ID == "20260824-1930" {
		t.Error("the bump took the batch's own run id as its registry key.\n" +
			"  That would put it on /runs/20260824-1930, which is the URL that " +
			"means the batch is going in this process, and make the journal screens " +
			"say a run is live when what is live is a second cold-wallet session " +
			"about a transaction that is already public.")
	}
}

// TestABumpsContextIsNotTheRequests. Decision 2, and here it is a cold-wallet
// signing round with the same devices a batch uses.
func TestABumpsContextIsNotTheRequests(t *testing.T) {
	s, l := setupServer(t)
	l.block = make(chan struct{})
	defer close(l.block)

	reqCtx, closeTab := context.WithCancel(context.Background())
	r := post(t, s, "/bump/20260824-1930", url.Values{}).WithContext(reqCtx)
	if w := serveIt(s, r); w.Code != http.StatusSeeOther {
		t.Fatalf("starting a bump got %d", w.Code)
	}

	bumpCtx := l.runCtx(t)
	closeTab()
	time.Sleep(50 * time.Millisecond)
	select {
	case <-bumpCtx.Done():
		t.Fatal("closing the tab ended the bump's context, which would abandon a " +
			"signing round mid-way and leave the coin lock behind")
	default:
	}
}

// TestAnUnreadableTargetIsRefusedRatherThanTreatedAsEmpty.
//
// Empty means ask Core, through the same estimator the batch used. A typo means a
// typo. Those two are opposite instructions and collapsing them would silently
// substitute a guess for the one figure that decides what a bump costs — in the
// place where I-4 means the batch gets no second attempt.
func TestAnUnreadableTargetIsRefusedRatherThanTreatedAsEmpty(t *testing.T) {
	for _, raw := range []string{"twelve", "12.5.1", "-3", "0", "1e", " "} {
		if raw == " " {
			// A field the operator tabbed through is empty, not a typo.
			if v, why := parseTarget(raw); why != "" || v != 0 {
				t.Errorf("whitespace was refused as a target: %q → %v, %q", raw, v, why)
			}
			continue
		}
		v, why := parseTarget(raw)
		if why == "" {
			t.Errorf("%q was accepted as a target rate and came through as %v",
				raw, v)
		}
	}
	if v, why := parseTarget("12.5"); why != "" || v != 12.5 {
		t.Errorf("a good target was refused: %v, %q", v, why)
	}
	if v, why := parseTarget(""); why != "" || v != 0 {
		t.Errorf("an empty target was refused: %v, %q", v, why)
	}
}

// TestABadTargetStartsNothing is the same property one layer up: the refusal is a
// screen, and no run is in the registry behind it.
func TestABadTargetStartsNothing(t *testing.T) {
	s, l := setupServer(t)

	w := serveIt(s, post(t, s, "/bump/20260824-1930", url.Values{
		"target": {"twelve"},
	}))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("an unreadable target got %d, want 400", w.Code)
	}
	mustSay(t, pre(t, w.Body.String()), "Nothing was built")
	if live := s.Runs.Live(); live != nil {
		t.Fatalf("run %s was started behind the refusal", live.ID)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.bumps != 0 {
		t.Errorf("StartBump was called %d times for a form that was refused", l.bumps)
	}
}

// TestABumpIsNotOfferedTheBatchsAbortControl, and the refusal is not the setup's.
//
// A bump's way out is inside the round: answering that a device cannot sign fails
// it, which releases the coin lock. Cancelling the context is not that, and the
// screen behind /runs/{id}/abort describes abandoning pending channels — which is
// the one move that can strand funds on a batch whose funding transaction is
// already public.
func TestABumpIsNotOfferedTheBatchsAbortControl(t *testing.T) {
	s, _ := setupServer(t)
	sess := s.Runs.addKind("a-bump", KindBump, "20260824-1930")

	body := serveIt(s, get(t, s, "/runs/"+sess.ID)).Body.String()
	if strings.Contains(body, "/abort") {
		t.Error("the bump's run screen links to the batch's abort control")
	}
	mustSay(t, pre(t, body),
		"There is no control on this screen that stops a bump",
		"winthistle bump --abandon 20260824-1930")

	w := serveIt(s, get(t, s, "/runs/"+sess.ID+"/abort"))
	if w.Code != http.StatusNotFound {
		t.Fatalf("the abort screen for a bump got %d, want 404", w.Code)
	}
	got := pre(t, w.Body.String())
	mustSay(t, got, "it is a CPFP child of run 20260824-1930")
	if says(got, "it is a setup of the cold wallet") {
		t.Errorf("a bump was refused with the setup's words:\n%s", got)
	}
}

// TestTheJournalScreenOffersABumpOnlyForARunItRefusesToAbort.
//
// The two conditions are the same condition. journal.Run.AbortTarget refuses a
// run in publishing or published, and that is exactly when a bump is possible: a
// batch that never reached the publish call has no parent in any mempool and
// nothing to accelerate. So the link is offered on the refusal and withheld
// otherwise, rather than offered always and dead half the time.
func TestTheJournalScreenOffersABumpOnlyForARunItRefusesToAbort(t *testing.T) {
	s, l := journalling(t)

	if body := serveIt(s, get(t, s, "/recover/20260824-1930")).Body.String(); strings.Contains(body, "/bump/") {
		t.Errorf("a run that can still be aborted was offered a bump:\n%s", body)
	}

	l.refusal = errors.New("run 20260824-1930 is publishing and funds 3 " +
		"channel(s): it may already be public — check whether it is in the mempool " +
		"or a block before touching anything")
	body := serveIt(s, get(t, s, "/recover/20260824-1930")).Body.String()
	if !strings.Contains(body, `href="/bump/20260824-1930"`) {
		t.Errorf("a published run was not offered the one exit it has:\n%s", body)
	}
	mustSay(t, pre(t, body), "a child, never a replacement of it")
}

// TestTheJournalScreenDoesNotSayNothingIsHappeningOverARowThatIs.
//
// A bump's rows go under the id of the batch it is accelerating, so the run it is
// about is in the list below — and is being written to while the page is read.
// "Nothing below is happening" is exactly the sentence this screen exists not to
// say, and a live bump is the one case where it would have been said over a
// changing row.
func TestTheJournalScreenDoesNotSayNothingIsHappeningOverARowThatIs(t *testing.T) {
	s, _ := journalling(t)
	s.Runs.addKind("a-bump", KindBump, "20260824-1930")

	got := pre(t, serveIt(s, get(t, s, "/recover")).Body.String())
	if says(got, "Nothing below is happening") {
		t.Errorf("the journal screen says nothing below is happening while a "+
			"bump is writing rows under one of those runs:\n%s", got)
	}
	mustSay(t, got,
		"run 20260824-1930 is in the list below, and its rows are being added to",
		"a bump builds a child rather than a replacement")

	// And a bump of a run the journal does not list is worth saying out loud
	// rather than passing over: a bump needs a batch that was actually published.
	s2, l2 := journalling(t)
	l2.ids = nil
	s2.Runs.addKind("a-bump", KindBump, "20260824-1930")
	got = pre(t, serveIt(s2, get(t, s2, "/recover")).Body.String())
	mustSay(t, got, "Run 20260824-1930 is not in the list below")
}

// TestTheBumpScreenIsAbsentRatherThanOfferedWithoutALauncher.
func TestTheBumpScreenIsAbsentRatherThanOfferedWithoutALauncher(t *testing.T) {
	s := testServer(t)
	for _, r := range []*http.Request{
		get(t, s, "/bump/20260824-1930"),
		post(t, s, "/bump/20260824-1930", url.Values{}),
	} {
		w := serveIt(s, r)
		if w.Code != http.StatusNotImplemented {
			t.Fatalf("%s /bump with no launcher got %d, want 501", r.Method, w.Code)
		}
		mustSay(t, pre(t, w.Body.String()), "winthistle bump RUN")
	}
}

// TestTheBumpScreenCarriesTheWayBack is the dead-end defect, which has now been
// found three times: copy that names a screen the nav cannot reach.
//
// The nav has overview, doctor, setup and recover on it. A bump screen is under
// none of them, and it is reached from /recover/{id}, so it has to carry the link
// back there itself.
func TestTheBumpScreenCarriesTheWayBack(t *testing.T) {
	s, _ := setupServer(t)
	body := serveIt(s, get(t, s, "/bump/20260824-1930")).Body.String()
	if !strings.Contains(body, `href="/recover/20260824-1930"`) {
		t.Errorf("the bump screen has no way back to the run it is about:\n%s", body)
	}
}
