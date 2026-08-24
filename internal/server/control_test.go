package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubLauncher is a launcher that does nothing but let a test watch what it was
// given. What matters about it is the context: every assertion in this file
// about decision 2 is an assertion about the context that arrived here.
type stubLauncher struct {
	batch   string
	wallet  string
	refusal error

	mu      sync.Mutex
	started int
	setups  int
	bumps   int
	bumpReq BumpRequest
	ctx     context.Context
	req     StartRequest

	// block, when non-nil, is what Start waits on. Closing it, or cancelling the
	// run's context, is what lets Start return.
	block chan struct{}
	err   error

	// The journal, as the two read-only screens see it. unfinished is what the
	// list renders, ids are the runs in it, and journalled maps one run id to its
	// own screen. journalErr is a journal that cannot be read at all.
	unfinished string
	ids        []string
	journalled map[string]string
	journalErr error

	// progress is what the attach screen's state block renders, per run id. A
	// run absent from it has journalled nothing, which is ordinary.
	progress map[string]string
}

func (l *stubLauncher) Batch() string  { return l.batch }
func (l *stubLauncher) Wallet() string { return l.wallet }

// StartSetup is the setup screen's seam, stubbed the way Start is: it says one
// thing into the transcript and then blocks on the same channel, so a test can
// hold a setup open and watch what the registry and the screens do about it.
func (l *stubLauncher) StartSetup(ctx context.Context, r *Run) error {
	l.mu.Lock()
	l.setups++
	l.ctx = ctx
	block := l.block
	l.mu.Unlock()

	r.Write([]byte("Reading the descriptors " + l.wallet + " holds\n"))
	if block == nil {
		return l.err
	}
	select {
	case <-block:
	case <-ctx.Done():
		return ctx.Err()
	}
	return l.err
}

func (l *stubLauncher) Start(ctx context.Context, r *Run, req StartRequest) error {
	l.mu.Lock()
	l.started++
	l.ctx, l.req = ctx, req
	block := l.block
	l.mu.Unlock()

	r.Write([]byte("Phase 0\n"))
	if block == nil {
		return l.err
	}
	select {
	case <-block:
	case <-ctx.Done():
		return ctx.Err()
	}
	return l.err
}

// StartBump is the bump screen's seam, stubbed like the other two.
func (l *stubLauncher) StartBump(ctx context.Context, r *Run, req BumpRequest) error {
	l.mu.Lock()
	l.bumps++
	l.ctx, l.bumpReq = ctx, req
	block := l.block
	l.mu.Unlock()

	fmt.Fprintf(r, "The batch being accelerated: run %s\n", req.RunID)
	if block == nil {
		return l.err
	}
	select {
	case <-block:
	case <-ctx.Done():
		return ctx.Err()
	}
	return l.err
}

func (l *stubLauncher) AbortRefusal(context.Context, string) error { return l.refusal }

func (l *stubLauncher) Unfinished(context.Context) (string, []string, error) {
	if l.journalErr != nil {
		return "", nil, l.journalErr
	}
	return l.unfinished, l.ids, nil
}

func (l *stubLauncher) Progress(_ context.Context, id string) (string, error) {
	if l.journalErr != nil {
		return "", l.journalErr
	}
	text, ok := l.progress[id]
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrNoJournalledRun, id)
	}
	return text, nil
}

func (l *stubLauncher) Journalled(_ context.Context, id string) (string, error) {
	if l.journalErr != nil {
		return "", l.journalErr
	}
	text, ok := l.journalled[id]
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrNoJournalledRun, id)
	}
	return text, nil
}

func (l *stubLauncher) runCtx(t *testing.T) context.Context {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		l.mu.Lock()
		ctx := l.ctx
		l.mu.Unlock()
		if ctx != nil {
			return ctx
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the launcher was never started")
	return nil
}

func launching(t *testing.T, l *stubLauncher) *Server {
	t.Helper()
	s := testServer(t)
	s.opts.Launcher = l
	return s
}

// TestTheRunsContextIsNotTheRequests is decision 2's whole point, as an
// assertion rather than a comment.
//
// The request's context is cancelled here — which is what a closed tab, a
// reload, a laptop lid and a Wi-Fi blip all look like from inside a handler —
// and the run's context is required to be untouched by it. Aborting a
// partially-armed batch on that evidence is worse than anything abort protects
// against: it tears down n peers' reservations and abandons channels that had
// reached chan_pending, and the ceremony is done again from the beginning.
func TestTheRunsContextIsNotTheRequests(t *testing.T) {
	l := &stubLauncher{batch: "1 channel", block: make(chan struct{})}
	s := launching(t, l)

	reqCtx, closeTab := context.WithCancel(context.Background())
	r := post(t, s, "/runs", url.Values{}).WithContext(reqCtx)
	if w := serveIt(s, r); w.Code != http.StatusSeeOther {
		t.Fatalf("starting a run got %d:\n%s", w.Code, w.Body.String())
	}

	runCtx := l.runCtx(t)
	closeTab()

	// Long enough that a cancellation propagated through a derived context would
	// have arrived.
	time.Sleep(50 * time.Millisecond)
	select {
	case <-runCtx.Done():
		t.Fatal("the run's context was cancelled when the request's was.\n" +
			"  That is decision 2 broken: a closed tab, a reload, a laptop lid and a " +
			"Wi-Fi blip are indistinguishable from each other, and none of them is " +
			"evidence about a batch with peers holding reservations.")
	default:
	}
	close(l.block)
}

// TestASecondConcurrentRunIsRefused.
//
// One journal, one cold wallet, one armed window. The expensive collision is not
// the journal — SQLite serialises that — it is the coins: the second run's dress
// rehearsal builds a decoy over the same inputs the first run is about to spend,
// so it would either lose coin selection or take the inputs out from under a
// batch that is already armed, with the cold wallet out and n peers waiting.
func TestASecondConcurrentRunIsRefused(t *testing.T) {
	l := &stubLauncher{batch: "1 channel", block: make(chan struct{})}
	s := launching(t, l)

	first := serveIt(s, post(t, s, "/runs", url.Values{}))
	if first.Code != http.StatusSeeOther {
		t.Fatalf("the first run got %d", first.Code)
	}
	l.runCtx(t)

	second := serveIt(s, post(t, s, "/runs", url.Values{}))
	if second.Code != http.StatusConflict {
		t.Fatalf("the second concurrent run got %d, want 409", second.Code)
	}
	got := pre(t, second.Body.String())
	if !strings.Contains(got, "one run at a time") {
		t.Errorf("the refusal does not say what the rule is:\n%s", got)
	}
	if !strings.Contains(got, "dress rehearsal") {
		t.Errorf("the refusal does not say why the rule is what it is:\n%s", got)
	}
	if l.started != 1 {
		t.Errorf("the launcher was started %d times", l.started)
	}

	// And the index does not offer the control while one is going, so the
	// operator is not taught to press a button that refuses.
	index := serveIt(s, get(t, s, "/")).Body.String()
	if strings.Contains(index, `action="/runs"`) {
		t.Error("the overview still offers the start control while a run is going")
	}

	// Once it is over, the control comes back.
	close(l.block)
	deadline := time.Now().Add(2 * time.Second)
	for s.Runs.Live() != nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !strings.Contains(serveIt(s, get(t, s, "/")).Body.String(), `action="/runs"`) {
		t.Error("the start control did not come back after the run finished")
	}
}

// TestTheAbortControlCancelsTheRun. Decision 2 makes this mandatory rather than
// optional: if a closed tab does not abort, the operator needs a thing that does.
func TestTheAbortControlCancelsTheRun(t *testing.T) {
	l := &stubLauncher{batch: "1 channel", block: make(chan struct{})}
	s := launching(t, l)
	serveIt(s, post(t, s, "/runs", url.Values{}))
	runCtx := l.runCtx(t)

	live := s.Runs.Live()
	if live == nil {
		t.Fatal("no run is live")
	}

	// The screen in front of it says what stopping costs, and what it will not
	// do. There is no JavaScript in this UI to put a dialog up with.
	warning := pre(t, serveIt(s, get(t, s, "/runs/"+live.ID+"/abort")).Body.String())
	for _, want := range []string{
		"What it costs is the ceremony",
		"already public",
		"force-close path",
	} {
		if !strings.Contains(warning, want) {
			t.Errorf("the abort screen does not say %q:\n%s", want, warning)
		}
	}

	w := serveIt(s, post(t, s, "/runs/"+live.ID+"/abort", url.Values{}))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("the abort got %d, want 303:\n%s", w.Code, w.Body.String())
	}
	select {
	case <-runCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the abort control did not cancel the run's context")
	}
	if !live.Aborting() {
		t.Error("the run does not know it was asked to stop, so the screen will " +
			"invite the operator to press it again")
	}
}

// TestTheAbortControlIsRefusedForARunThatMayBePublished is the guard HANDOFF
// asked for by name.
//
// journal.Run.AbortTarget refuses a run in publishing or published with
// ErrMayBePublished, and run.RecoverOne refuses the same run for the same
// reason: abandoning a pending channel whose funding transaction later confirms
// strands its funds with no force-close path, which is the worst outcome this
// design has. The control has to refuse it too, and it asks rather than deciding
// — this package cannot import internal/journal, so it could not hold a second
// copy of the rule even if somebody tried.
func TestTheAbortControlIsRefusedForARunThatMayBePublished(t *testing.T) {
	refusal := errors.New("run 20260824-1 is publishing and funds 3 channel(s) on " +
		"abc123: the run reached the publish call — it must not be aborted")
	l := &stubLauncher{batch: "1 channel", block: make(chan struct{}), refusal: refusal}
	s := launching(t, l)
	serveIt(s, post(t, s, "/runs", url.Values{}))
	runCtx := l.runCtx(t)
	live := s.Runs.Live()

	// The screen refuses, and it does not render a form to press.
	screenBody := serveIt(s, get(t, s, "/runs/"+live.ID+"/abort"))
	if screenBody.Code != http.StatusConflict {
		t.Fatalf("the abort screen got %d, want 409", screenBody.Code)
	}
	if strings.Contains(screenBody.Body.String(), "<form") {
		t.Error("the abort screen offered a form for a run that may be published")
	}
	got := pre(t, screenBody.Body.String())
	for _, want := range []string{
		"must not be aborted",
		"force-close path",
		"winthistle bump",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the refusal does not say %q:\n%s", want, got)
		}
	}

	// And the POST refuses too, rather than trusting that the screen was read.
	// A form kept open from before the publish is exactly how this arrives.
	if w := serveIt(s, post(t, s, "/runs/"+live.ID+"/abort", url.Values{})); w.Code != http.StatusConflict {
		t.Fatalf("the abort POST got %d, want 409", w.Code)
	}
	select {
	case <-runCtx.Done():
		t.Fatal("the run was cancelled despite the refusal")
	default:
	}
	close(l.block)
}

// TestTheStartControlIsAbsentWithoutABatch. Absent rather than disabled: a
// control that is offered and then refused teaches an operator to press it
// twice.
func TestTheStartControlIsAbsentWithoutABatch(t *testing.T) {
	s := testServer(t) // no launcher at all
	body := serveIt(s, get(t, s, "/")).Body.String()
	if strings.Contains(body, `action="/runs"`) {
		t.Error("the overview offers a start control with no batch behind it")
	}
	if !strings.Contains(pre(t, body), "winthistle") {
		t.Error("the overview did not render")
	}

	w := serveIt(s, post(t, s, "/runs", url.Values{}))
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("starting a run with no batch got %d, want 501", w.Code)
	}
	if got := pre(t, w.Body.String()); !strings.Contains(got, "serve --batch") {
		t.Errorf("the refusal does not say how to fix it:\n%s", got)
	}
}

// TestTheOperatorsChoicesReachTheRun: the two flags on the form are the two the
// command line has, and --stop-before-publish is the cold probe.
func TestTheOperatorsChoicesReachTheRun(t *testing.T) {
	l := &stubLauncher{batch: "1 channel"}
	s := launching(t, l)

	if w := serveIt(s, post(t, s, "/runs", url.Values{
		"probe": {"1"}, "stop-before-publish": {"1"},
	})); w.Code != http.StatusSeeOther {
		t.Fatalf("starting a run got %d", w.Code)
	}
	l.runCtx(t)

	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.req.Probe || !l.req.StopBeforePublish {
		t.Errorf("the run was started with %+v", l.req)
	}
}
