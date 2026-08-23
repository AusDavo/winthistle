package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// awaitPending polls until the run is asking something. Polled rather than
// signalled on purpose: the production reader is a browser reloading a page, and
// a channel to wait on here would be a mechanism the screens do not have.
func awaitPending(t *testing.T, r *Run) *Question {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if q := r.Pending(); q != nil {
			return q
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the run never asked anything")
	return nil
}

// post is a form submission shaped exactly the way a browser shapes one.
//
// The headers are not a plausible reconstruction; they are what Chrome 152 was
// measured sending on a same-origin top-level form POST to this UI: an *opaque*
// Origin, and Sec-Fetch-Site: same-origin. Getting this wrong is how a guard bug
// that made every form in the UI unusable passed the whole suite — the tests all
// chose `Origin: http://127.0.0.1:7420`, which no browser sends here.
//
// If you are tempted to "fix" a failing test by putting a real Origin back, that
// is the test telling you the guard has stopped accepting browsers.
func post(t *testing.T, s *Server, path string, form url.Values) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	r.Host = "127.0.0.1:7420"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", opaqueOrigin)
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.Header.Set("Sec-Fetch-Mode", "navigate")
	r.AddCookie(&http.Cookie{Name: cookieName, Value: s.token})
	return r
}

// TestAQuestionWithNoClockIsRefused.
//
// The bound on a seam has to be limits.abort_after_signing_seconds, computed by
// whoever read the configuration. This package cannot compute it — it has no
// configuration of the clocks and deliberately never reads rehearsal.PeerWindow
// — so a question that arrives without a deadline is a programming error and is
// refused rather than waited on. A question with no clock would block run.Do
// until the process died.
func TestAQuestionWithNoClockIsRefused(t *testing.T) {
	r := NewRegistry()
	run, err := r.Start("run-1", func() {})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := run.Ask(context.Background(), Question{Prompt: "well?"}); !errors.Is(err, ErrNoClock) {
		t.Fatalf("a question with no deadline returned %v, want ErrNoClock", err)
	}
}

// TestAnUnansweredQuestionExpiresRatherThanHanging.
//
// A browser can stop answering, and a closed tab is not detectable — decision 2
// — so the only thing that can end the wait is the clock. What must not happen
// is run.Do blocked forever on a channel nobody will write to, with n peers
// holding reservations.
func TestAnUnansweredQuestionExpiresRatherThanHanging(t *testing.T) {
	run, _ := NewRegistry().Start("run-1", func() {})

	start := time.Now()
	_, err := run.Ask(context.Background(), Question{
		Prompt:   "well?",
		Deadline: time.Now().Add(30 * time.Millisecond),
	})
	if !errors.Is(err, ErrUnanswered) {
		t.Fatalf("an abandoned question returned %v, want ErrUnanswered", err)
	}
	if time.Since(start) > time.Second {
		t.Errorf("it waited %s, which is not the deadline it was given", time.Since(start))
	}
	if run.Pending() != nil {
		t.Error("the question is still pending after it expired, so the next one " +
			"would be refused as a second question")
	}
}

// TestADeadlineAlreadyPastIsToldTheRoundIsOver.
//
// m devices in a round share one deadline, so the second device can legitimately
// be asked after the budget is gone. That is not a race to lose; it is a fact to
// report.
func TestADeadlineAlreadyPastIsToldTheRoundIsOver(t *testing.T) {
	run, _ := NewRegistry().Start("run-1", func() {})
	_, err := run.Ask(context.Background(), Question{
		Prompt:   "well?",
		Deadline: time.Now().Add(-time.Minute),
	})
	if !errors.Is(err, ErrUnanswered) {
		t.Fatalf("a question whose round is over returned %v, want ErrUnanswered", err)
	}
}

// TestACancelledRunStopsAsking. The abort control cancels the run's context, and
// a question open at that moment has to come back rather than hold the teardown.
func TestACancelledRunStopsAsking(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	run, _ := NewRegistry().Start("run-1", cancel)

	go func() {
		awaitPending(t, run)
		run.Abort()
	}()

	_, err := run.Ask(ctx, Question{
		Prompt:   "well?",
		Deadline: time.Now().Add(time.Minute),
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled run's question returned %v, want context.Canceled", err)
	}
}

// TestTheQuestionIsServedAndTheAnswerReachesTheRun is the seam end to end,
// through the socket rather than through the channel.
//
// It is also decision 3 over a question: the prompt is copy, so it goes into the
// same <pre> the transcript does, and the oracle covers it. The form is chrome.
func TestTheQuestionIsServedAndTheAnswerReachesTheRun(t *testing.T) {
	s := testServer(t)
	run := s.Runs.add("run-1")
	run.Write([]byte("Phase 0 — the dress rehearsal\n"))

	type result struct {
		a   Answer
		err error
	}
	done := make(chan result, 1)
	go func() {
		a, err := run.Ask(context.Background(), Question{
			Prompt:       "cold1: sign this & hand it back. Don't finalize.",
			Payload:      "cHNidP8BAAA=",
			PayloadLabel: "the packet to take to cold1",
			Reply:        "paste what cold1 gave back",
			Choices: []Choice{
				{Value: "signed", Label: "This is the signed packet"},
				{Value: "cannot", Label: "cold1 cannot sign"},
			},
			Deadline: time.Now().Add(time.Minute),
		})
		done <- result{a, err}
	}()

	q := awaitPending(t, run)

	body := serveIt(s, get(t, s, "/runs/run-1")).Body.String()
	if got := pre(t, body); !strings.Contains(got, "Don't finalize.") {
		t.Errorf("the prompt is not in the page verbatim:\n%s", got)
	}
	if strings.Contains(body, "Don't finalize.") {
		t.Error("the prompt reached the page unescaped, so the chokepoint was bypassed")
	}
	for _, want := range []string{
		`action="/runs/run-1/answer"`,
		`name="question" value="` + q.ID + `"`,
		`name="choice" value="signed"`,
		`name="reply"`,
		"cHNidP8BAAA=",
		`href="/runs/run-1/abort"`, // the control decision 2 makes mandatory
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the run screen is missing %q:\n%s", want, body)
		}
	}

	w := serveIt(s, post(t, s, "/runs/run-1/answer", url.Values{
		"question": {q.ID},
		"choice":   {"signed"},
		"reply":    {"  cHNidP8BAAA=signed  "},
	}))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("answering got %d, want 303:\n%s", w.Code, w.Body.String())
	}

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("Ask returned %v", got.err)
		}
		if got.a.Choice != "signed" {
			t.Errorf("the choice arrived as %q", got.a.Choice)
		}
		// Trimmed, because a textarea collects whatever the paste brought with it
		// and a leading newline is not part of a PSBT.
		if got.a.Text != "cHNidP8BAAA=signed" {
			t.Errorf("the packet arrived as %q", got.a.Text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the answer never reached the run")
	}
}

// TestAStaleAnswerIsRefusedRatherThanApplied.
//
// The back button, a second tab, a double submit. An answer carries the id of
// the question it was given, and the run has moved on. This is the layer under
// abort.Confirmation's per-channel shape: a "yes" about one channel must not be
// spendable on the next one.
func TestAStaleAnswerIsRefusedRatherThanApplied(t *testing.T) {
	s := testServer(t)
	run := s.Runs.add("run-1")

	// One question, asked and answered.
	first := make(chan Answer, 1)
	go func() {
		a, _ := run.Ask(context.Background(), Question{
			Prompt:   "channel one?",
			Choices:  []Choice{{Value: "yes", Label: "yes"}},
			Deadline: time.Now().Add(time.Minute),
		})
		first <- a
	}()
	q1 := awaitPending(t, run)
	serveIt(s, post(t, s, "/runs/run-1/answer", url.Values{
		"question": {q1.ID}, "choice": {"yes"},
	}))
	<-first

	// A second question, about a different channel. The old id must not answer it.
	second := make(chan Answer, 1)
	go func() {
		a, _ := run.Ask(context.Background(), Question{
			Prompt:   "channel two?",
			Choices:  []Choice{{Value: "yes", Label: "yes"}},
			Deadline: time.Now().Add(time.Minute),
		})
		second <- a
	}()
	q2 := awaitPending(t, run)
	if q2.ID == q1.ID {
		t.Fatalf("both questions have id %q, so an answer to one answers the other",
			q1.ID)
	}

	w := serveIt(s, post(t, s, "/runs/run-1/answer", url.Values{
		"question": {q1.ID}, "choice": {"yes"},
	}))
	if w.Code != http.StatusConflict {
		t.Fatalf("a stale answer got %d, want 409", w.Code)
	}
	if got := pre(t, w.Body.String()); !strings.Contains(got, "no longer") {
		t.Errorf("the refusal does not say what happened:\n%s", got)
	}

	select {
	case a := <-second:
		t.Fatalf("the stale answer was applied to the second question as %+v", a)
	case <-time.After(50 * time.Millisecond):
	}

	// And the current id still works, so the refusal is about staleness rather
	// than about the run having stopped listening.
	serveIt(s, post(t, s, "/runs/run-1/answer", url.Values{
		"question": {q2.ID}, "choice": {"yes"},
	}))
	select {
	case <-second:
	case <-time.After(2 * time.Second):
		t.Fatal("the current question could not be answered either")
	}
}

// TestAnUnofferedChoiceReachesTheSeamVerbatim.
//
// Choices are the buttons, not a filter. A form that comes back with something
// this question did not offer is handed to the seam exactly as it arrived, and
// the seam decides — which is why every adapter in internal/webrun matches the
// affirmative explicitly rather than testing for the negative. A server that
// silently rewrote the answer would be a second place a verdict is decided.
func TestAnUnofferedChoiceReachesTheSeamVerbatim(t *testing.T) {
	s := testServer(t)
	run := s.Runs.add("run-1")

	got := make(chan Answer, 1)
	go func() {
		a, _ := run.Ask(context.Background(), Question{
			Prompt:   "did they match?",
			Choices:  []Choice{{Value: "matched", Label: "yes"}, {Value: "differed", Label: "no"}},
			Deadline: time.Now().Add(time.Minute),
		})
		got <- a
	}()
	q := awaitPending(t, run)

	serveIt(s, post(t, s, "/runs/run-1/answer", url.Values{
		"question": {q.ID}, "choice": {"probably"},
	}))

	select {
	case a := <-got:
		if a.Choice != "probably" {
			t.Fatalf("the choice was rewritten to %q; the seam has to see what the "+
				"form actually said", a.Choice)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the answer never arrived")
	}
}
