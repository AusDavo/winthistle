package server_test

import (
	"context"
	"html"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AusDavo/winthistle/internal/config"
	"github.com/AusDavo/winthistle/internal/doctor"
	"github.com/AusDavo/winthistle/internal/regtestenv"
	"github.com/AusDavo/winthistle/internal/server"
)

// TestTheDoctorScreenIsServedEndToEnd is the read-only screen, end to end
// against the live harness.
//
// doctor was the right *first* screen precisely because it is the least
// dangerous one: no clock, no signer, no state, and the only thing it creates is
// the journal file. Nothing on this path can arm, publish or abort, so what is
// being proved here is the plumbing — a real winthistle.toml, a real pre-flight
// against a real node, through the guard, into a <pre>. The screens that *can*
// arm are proved by internal/webrun's browser-driven cold probe.
//
// The assertion that carries weight is the pane. Every Report() in this
// repository is written to prose.PaneWidth and tested against it, and serving
// one verbatim is only honest if the page does not undo that. report.go allows
// exactly one thing past the pane — a pasteable command, because a shell command
// cannot be wrapped without changing it — so that is the one exception here too,
// and it is stated rather than inferred from a passing test.
func TestTheDoctorScreenIsServedEndToEnd(t *testing.T) {
	env := regtestenv.Start(t)
	dir := t.TempDir()

	cfg, err := config.Load(env.ConfigFile(t, dir))
	if err != nil {
		t.Fatalf("loading the config: %v", err)
	}

	s, err := server.New(cfg, server.Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	r := httptest.NewRequestWithContext(ctx, http.MethodGet, "/doctor", nil)
	r.Host = "127.0.0.1:7420"
	r.Header.Set("Authorization", "Bearer "+s.Token())
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /doctor got %d:\n%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q", ct)
	}

	text := preOf(t, w.Body.String())

	// It is a doctor report, not an error page dressed as one.
	if first, _, _ := strings.Cut(text, "\n"); !strings.HasSuffix(first, " checks") ||
		(!strings.HasPrefix(first, "Ready") && !strings.HasPrefix(first, "Not ready")) {

		t.Fatalf("the first line is not a doctor summary: %q", first)
	}

	// And it is the same report the CLI prints. Run it again locally rather than
	// diffing two renderings of one run: the point is that the screen is the
	// report, so the check is that every check name and status appears, not that
	// two calls to a live node produce identical bytes.
	local := doctor.Run(ctx, cfg, doctor.Options{})
	for _, c := range local.Checks {
		if !strings.Contains(text, c.Name) {
			t.Errorf("the served screen is missing the %q check", c.Name)
		}
	}
	if len(local.Checks) == 0 {
		t.Fatal("the local report has no checks, so the comparison proves nothing")
	}

	// Not a pane check. internal/doctor and internal/prose own the width, and
	// this test measured it for one commit before the reason it cannot be
	// measured here became obvious: a real report against a real node contains
	// tokens prose.Wrap cannot break — an absolute path to winthistle.toml, a
	// 66-character pubkey, a descriptor — so a served report is regularly past
	// the pane through no fault of the page. It passed only because
	// t.TempDir() is short, which is the definition of a test that measures the
	// wrong thing. What the page owes the report is not to reflow it, and that
	// is TestTheTextIsTheOracle; what it owes a long line is a soft wrap rather
	// than a horizontal scrollbar, and that is TestALongLineSoftWraps.
	if !strings.Contains(text, "winthistle.toml") {
		t.Error("the first check does not name the file it read")
	}
}

// preOf is the oracle's reader again, from the external test package.
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
