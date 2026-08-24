package webrun_test

import (
	"context"
	"encoding/base64"
	"errors"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/AusDavo/winthistle/internal/config"
	"github.com/AusDavo/winthistle/internal/journal"
	"github.com/AusDavo/winthistle/internal/prose"
	"github.com/AusDavo/winthistle/internal/regtestenv"
	"github.com/AusDavo/winthistle/internal/server"
	"github.com/AusDavo/winthistle/internal/webrun"
)

// fixtureChannelSat matches the other packages' fixtures, so a peer that accepts
// one accepts the others.
const fixtureChannelSat = 250_000

// TestABrowserDrivenColdProbeRunsTheRealPathAndWithholdsStepNine.
//
// The same cold probe internal/run's regtest test drives, with every one of the
// four seams answered through the HTTP handler instead of through a Go func. It
// is the test that says the seams work rather than that they compile:
//
//   - two full signing rounds go out as base64 in a Question and come back as
//     base64 in a form post, through internal/combine and internal/arm's
//     verifier untouched. Six devices' worth of transport code is not being
//     exercised here — one field out, one field back, which is the browser
//     transport at its minimum — but the packets are real, the cold wallet is
//     the harness's real 2-of-2, and each half returns only its own partial.
//   - the run is started by a POST, which is this repository's first unsafe
//     method, behind the guard that already refused one that could not say where
//     it came from.
//   - the teardown's blunt-abandon confirmation is asked per channel and
//     answered per channel, which is the whole reason abort.Confirmation is a
//     func rather than a bool.
//
// And it withholds step 9, so nothing here reaches a mempool.
func TestABrowserDrivenColdProbeRunsTheRealPathAndWithholdsStepNine(t *testing.T) {
	env := regtestenv.Start(t)
	peers := env.Peers(t)
	if len(peers) < 2 {
		t.Skipf("this test needs 2 peers, alice has %d", len(peers))
	}
	dir := t.TempDir()

	cfg, err := config.Load(env.ConfigFile(t, dir))
	if err != nil {
		t.Fatalf("the harness config file: %v", err)
	}
	batch, err := config.LoadBatch(regtestenv.BatchFile(t, dir, peers[:2], fixtureChannelSat))
	if err != nil {
		t.Fatalf("the harness batch file: %v", err)
	}

	s, err := server.New(cfg, server.Options{Launcher: webrun.New(cfg, batch)})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}

	// The overview offers the batch it would open, before anything is started.
	if body := serve(t, s, get(t, s, "/")); !strings.Contains(body, `action="/runs"`) {
		t.Fatal("the overview does not offer the control that starts a run")
	}

	w := do(t, s, form(t, s, "/runs", url.Values{"stop-before-publish": {"1"}}))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("POST /runs got %d:\n%s", w.Code, w.Body.String())
	}
	id := strings.TrimPrefix(w.Header().Get("Location"), "/runs/")
	if id == "" {
		t.Fatal("the run started without a location to attach to")
	}
	run := s.Runs.Get(id)
	if run == nil {
		t.Fatalf("run %s is not in the registry, which is the one thing the POST "+
			"had to do", id)
	}

	// Drive it. Every answer goes through the handler, so what is being tested is
	// the route rather than the adapter.
	signed, confirmed, answered := 0, 0, map[string]bool{}
	deadline := time.Now().Add(6 * time.Minute)

	for time.Now().Before(deadline) {
		if _, finished, _ := run.State(); finished {
			break
		}
		q := run.Pending()
		if q == nil {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if answered[q.ID] {
			time.Sleep(10 * time.Millisecond)
			continue
		}

		// The first question also has to be *on the page*, with a form aimed at
		// the right place and the prompt inside the <pre> the oracle covers.
		if len(answered) == 0 {
			body := serve(t, s, get(t, s, "/runs/"+id))
			for _, want := range []string{
				`action="/runs/` + id + `/answer"`,
				`name="question" value="` + q.ID + `"`,
				`name="reply"`,
				`href="/runs/` + id + `/abort"`,
			} {
				if !strings.Contains(body, want) {
					t.Fatalf("the run screen is missing %q:\n%s", want, body)
				}
			}
			// The prompt, not the rehearsal's report: the report is printed when
			// the round is over, and the first question is the first device of
			// that round. What has to be on the screen is the question, inside
			// the <pre> decision 3's oracle covers.
			if got := preOf(t, body); !strings.Contains(got, "The dress rehearsal") {
				t.Errorf("the question is not on the screen:\n%s", got)
			}
		}

		values := url.Values{"question": {q.ID}}
		switch {
		case offers(q, webrun.ChoiceSigned):
			label := device(t, q.Prompt)
			part := env.SignPartial(t, label, q.Payload)
			values.Set("choice", webrun.ChoiceSigned)
			values.Set("reply", base64.StdEncoding.EncodeToString(part.PSBT))
			signed++
		case offers(q, webrun.ChoiceYes):
			// The blunt abandon. LND's safe flag infers "shim funded" from
			// ThawHeight > 0 and a plain PSBT open sets none, so this is the
			// standard route rather than an edge case.
			values.Set("choice", webrun.ChoiceYes)
			confirmed++
		default:
			t.Fatalf("a question with no answerable choice:\n%s", q.Prompt)
		}

		if w := do(t, s, form(t, s, "/runs/"+id+"/answer", values)); w.Code != http.StatusSeeOther {
			t.Fatalf("answering %s got %d:\n%s", q.ID, w.Code, w.Body.String())
		}
		answered[q.ID] = true
	}

	transcript, finished, runErr := run.State()
	t.Logf("\n%s", transcript)
	if !finished {
		t.Fatalf("the run was still going after 6 minutes; %d signatures and %d "+
			"confirmations went in", signed, confirmed)
	}
	if runErr != nil {
		t.Fatalf("the browser-driven cold probe: %v", runErr)
	}

	// Two rounds of two devices: the dress rehearsal and the batch. If the
	// rehearsal had been skipped this would be two, and the gate would be
	// measuring nothing.
	if signed != 4 {
		t.Errorf("%d packets were signed through the page, expected 4 — two rounds "+
			"of two devices", signed)
	}
	if confirmed == 0 {
		t.Error("the teardown never asked for a blunt abandon, so the per-channel " +
			"confirmation seam was not exercised")
	}

	for _, want := range []string{
		"psbt_verify: all 2 channels",
		"Step 9 was not made",
		"Taking the batch apart",
	} {
		if !strings.Contains(transcript, want) {
			t.Errorf("the transcript does not contain %q", want)
		}
	}

	// Every question that was answered left a record, so a reload after the
	// ceremony shows what the operator agreed to rather than nothing at all.
	for _, want := range []string{
		"> The dress rehearsal", "> The signing round", "> Abandon this channel?",
		"This is the signed packet", "i_know_what_i_am_doing",
	} {
		if !strings.Contains(transcript, want) {
			t.Errorf("the transcript keeps no record of %q", want)
		}
	}

	assertFitsThePane(t, transcript)

	// The journal is the record, and it is the only thing that can say the batch
	// reached the I-1 gate.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	j, err := journal.Open(ctx, cfg.Server.Journal)
	if err != nil {
		t.Fatalf("opening the journal: %v", err)
	}
	defer j.Close()

	jr, err := j.Load(ctx, id)
	if err != nil {
		t.Fatalf("reading run %s back: %v", id, err)
	}
	if jr.State != journal.StateAborted {
		t.Errorf("the run ended in %s, not %s", jr.State, journal.StateAborted)
	}
	if jr.TxID == "" || jr.RawTx == "" {
		t.Error("the journal does not hold the finalized transaction")
	}
	if env.InMempool(t, jr.TxID) {
		t.Fatalf("%s reached the mempool. The whole claim of this mode is that "+
			"step 9 is the one call it does not make", jr.TxID)
	}

	// And every device's partial is recorded, which is what a recovery screen
	// reads to say how far the signing round got.
	for _, sg := range jr.Signers {
		if sg.State != journal.SignerPartial {
			t.Errorf("signer %s is journalled as %s", sg.Label, sg.State)
		}
	}
	if len(jr.Signers) != 2 {
		t.Errorf("the journal recorded %d signers for the batch round", len(jr.Signers))
	}

	// The run is over, so the registry offers the control again — and the abort
	// screen says there is nothing left to cancel rather than offering a button.
	if s.Runs.Live() != nil {
		t.Error("the registry still calls the run live")
	}
	if body := serve(t, s, get(t, s, "/runs/"+id+"/abort")); strings.Contains(body, "<form") {
		t.Error("the abort screen offers a control for a run that has stopped")
	}
}

// TestTheSecondRunIsRefusedWhileTheFirstIsGoing, against the live harness rather
// than a stub: the collision this refusal prevents is a collision over coins, and
// the cold wallet is what has them.
func TestTheSecondRunIsRefusedWhileTheFirstIsGoing(t *testing.T) {
	env := regtestenv.Start(t)
	peers := env.Peers(t)
	if len(peers) < 1 {
		t.Skip("this test needs a peer")
	}
	dir := t.TempDir()

	cfg, err := config.Load(env.ConfigFile(t, dir))
	if err != nil {
		t.Fatalf("the harness config file: %v", err)
	}
	batch, err := config.LoadBatch(regtestenv.BatchFile(t, dir, peers[:1], fixtureChannelSat))
	if err != nil {
		t.Fatalf("the harness batch file: %v", err)
	}

	s, err := server.New(cfg, server.Options{Launcher: webrun.New(cfg, batch)})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}

	first := do(t, s, form(t, s, "/runs", url.Values{"stop-before-publish": {"1"}}))
	if first.Code != http.StatusSeeOther {
		t.Fatalf("the first run got %d:\n%s", first.Code, first.Body.String())
	}
	id := strings.TrimPrefix(first.Header().Get("Location"), "/runs/")
	run := s.Runs.Get(id)

	// Wait until the run is genuinely under way — the first question is a
	// rehearsal signature, which means Core has already been asked for coins.
	waitForQuestion(t, run)

	second := do(t, s, form(t, s, "/runs", url.Values{}))
	if second.Code != http.StatusConflict {
		t.Fatalf("the second concurrent run got %d, want 409:\n%s",
			second.Code, second.Body.String())
	}
	if got := preOf(t, second.Body.String()); !strings.Contains(got, "dress rehearsal") {
		t.Errorf("the refusal does not say why the rule is what it is:\n%s", got)
	}

	// Stop the first one through the control, and let it unwind. Nothing has been
	// published — this run never got past its rehearsal — so there is nothing the
	// abort could strand.
	if w := do(t, s, form(t, s, "/runs/"+id+"/abort", url.Values{})); w.Code != http.StatusSeeOther {
		t.Fatalf("the abort got %d:\n%s", w.Code, w.Body.String())
	}
	deadline := time.Now().Add(2 * time.Minute)
	for s.Runs.Live() != nil && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	transcript, finished, runErr := run.State()
	t.Logf("\n%s", transcript)
	if !finished {
		t.Fatal("the abort control did not stop the run")
	}
	if runErr == nil {
		t.Error("a cancelled run reported success")
	}
	if !errors.Is(runErr, context.Canceled) {
		t.Logf("it stopped with %v, which is a cancellation reaching the run by "+
			"some other name", runErr)
	}
}

// offers reports whether a question has this choice on it.
func offers(q *server.Question, value string) bool {
	for _, c := range q.Choices {
		if c.Value == value {
			return true
		}
	}
	return false
}

// device is which half of the simulated cold wallet a question is about. The
// prompt names it, because a refusal that does not name the device is a hunt.
func device(t *testing.T, prompt string) string {
	t.Helper()
	for _, label := range regtestenv.ColdSigners() {
		if strings.Contains(prompt, label) {
			return label
		}
	}
	t.Fatalf("no signing device is named in:\n%s", prompt)
	return ""
}

func waitForQuestion(t *testing.T, r *server.Run) *server.Question {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		if q := r.Pending(); q != nil {
			return q
		}
		if _, finished, err := r.State(); finished {
			t.Fatalf("the run finished before asking anything: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the run never asked anything")
	return nil
}

// get and form are requests that have already got past the guard. The bind in
// the harness config is 127.0.0.1:7420, which is the authority the guard
// computed, and an unsafe method has to say where it came from.
func get(t *testing.T, s *server.Server, path string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Host = "127.0.0.1:7420"
	r.Header.Set("Authorization", "Bearer "+s.Token())
	return r
}

// form is a POST shaped the way a browser shapes one: an opaque Origin and
// Sec-Fetch-Site: same-origin, which is what Chrome was measured sending. See
// internal/server's post helper for why the exact headers matter.
func form(t *testing.T, s *server.Server, path string, values url.Values) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(values.Encode()))
	r.Host = "127.0.0.1:7420"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "null")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.Header.Set("Authorization", "Bearer "+s.Token())
	return r
}

func do(t *testing.T, s *server.Server, r *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func serve(t *testing.T, s *server.Server, r *http.Request) string {
	t.Helper()
	w := do(t, s, r)
	if w.Code != http.StatusOK {
		t.Fatalf("%s %s got %d:\n%s", r.Method, r.URL, w.Code, w.Body.String())
	}
	return w.Body.String()
}

// preOf is the oracle's reader, from outside the package.
func preOf(t *testing.T, body string) string {
	t.Helper()
	_, rest, ok := strings.Cut(body, "<pre>")
	if !ok {
		t.Fatalf("no <pre> in the page:\n%s", body)
	}
	inner, _, ok := strings.Cut(rest, "</pre>")
	if !ok {
		t.Fatalf("unterminated <pre> in the page:\n%s", body)
	}
	return html.UnescapeString(inner)
}

// assertFitsThePane measures a whole run's transcript against prose.PaneWidth.
//
// This is the guard that was missing, and it is the reason two lines went out
// unwrapped: every *report* package measures its own screens, and nothing measured
// the composition — the strings internal/run writes between them. One of those
// carried LND's verbatim errors and reached 251 characters, at the moment an
// operator is reading hardest.
//
// A whole real transcript, from a real run against real peers, is the only thing
// that covers them: they are one Fprintf each, on a path no unit test walks.
// Runes rather than bytes, because this copy is full of em dashes and a byte count
// would report a line as three columns wider than it renders.
//
// The one exemption is internal/doctor's: a line an operator *pastes* cannot be
// wrapped without changing it. Nothing a run writes is pasteable today, and the
// exemption is here so that adding one is a deliberate act rather than a failing
// test somebody widens the pane to silence.
func assertFitsThePane(t *testing.T, transcript string) {
	t.Helper()
	if strings.TrimSpace(transcript) == "" {
		t.Fatal("the transcript is empty, so this measures nothing")
	}
	for i, line := range strings.Split(transcript, "\n") {
		if strings.HasPrefix(line, "  $ ") {
			continue
		}
		if n := len([]rune(line)); n > prose.PaneWidth {
			t.Errorf("transcript line %d is %d runes, past the %d-column pane. "+
				"Everything internal/run writes between the reports has to be "+
				"wrapped the way the reports are:\n%s", i+1, n, prose.PaneWidth, line)
		}
	}
}
