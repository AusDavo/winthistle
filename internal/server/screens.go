package server

import (
	"fmt"
	"html"
	"net/http"
	"strings"
	"time"

	"github.com/AusDavo/winthistle/internal/doctor"
	"github.com/AusDavo/winthistle/internal/prose"
)

// DoctorOptions is internal/doctor's, so the doctor screen can be about the
// batch the operator means to open rather than a hypothetical one.
type DoctorOptions = doctor.Options

// screen is the one place a Report() string becomes a page, and it is the reason
// decision 3 has a guard rather than a promise.
//
// Every operator-facing string in this UI goes through here and comes out inside
// a <pre>, escaped and otherwise untouched. TestTheTextIsTheOracle unescapes the
// <pre> out of a served page and requires the original back byte for byte. So
// when a screen is later re-rendered as HTML — the recovery screen will be, it
// is the highest-stakes copy in the product and it deserves better than a
// terminal in a browser — that test does not get deleted. The HTML has to be
// built from this same string, and the text stays the oracle: prose's overrun
// tests cover the text, so the copy cannot drift into a second rendering that
// nothing measures.
func screen(title string, here string, text string) string {
	return page(title, here, "<pre>"+html.EscapeString(text)+"</pre>")
}

// link is one entry in the nav. Navigation is chrome rather than copy, so it is
// the one thing on a page that is not inside the <pre>.
type link struct {
	Path  string
	Label string
}

var nav = []link{
	{"/", "overview"},
	{"/doctor", "doctor"},
}

// page is the shell: one <style> block, no script, no fetched asset.
//
// The CSP in guard.go allows nothing but inline styles, so this is not a
// convention that could slip — a stylesheet link or a script tag added later
// does not load. The column is prose.PaneWidth, which is the width every
// Report() in this repository is written and tested against.
func page(title, here, body string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<!doctype html>
<html lang="en"><head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="referrer" content="no-referrer">
<title>%s — winthistle</title>
<style>
:root { color-scheme: light dark; }
body { margin: 0; padding: 1.5rem 1.5rem 4rem;
  font: 400 14px/1.45 ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; }
nav { margin: 0 0 1.5rem; padding: 0 0 .75rem; border-bottom: 1px solid;
  border-color: color-mix(in srgb, currentColor 25%%, transparent); }
nav a { margin-right: 1.25rem; }
nav a[aria-current] { font-weight: 600; text-decoration: none; }
pre { margin: 0; white-space: pre-wrap; overflow-wrap: break-word;
  max-width: %dch; }
ul { padding-left: 1.25rem; max-width: %dch; }
</style>
</head><body>
<nav>`, html.EscapeString(title), prose.PaneWidth, prose.PaneWidth)

	for _, l := range nav {
		current := ""
		if l.Path == here {
			current = ` aria-current="page"`
		}
		fmt.Fprintf(&b, `<a href="%s"%s>%s</a>`, l.Path, current, l.Label)
	}
	b.WriteString("</nav>\n")
	b.WriteString(body)
	b.WriteString("\n</body></html>\n")
	return b.String()
}

// serve writes a rendered page.
func serve(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, body)
}

// index says what this is and what is running.
func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	var b strings.Builder
	b.WriteString(screen("overview", "/", overview(s.cfg.Path)))

	// The run list is the only place a link is generated from state, and it is
	// what decision 2 buys: a tab that comes back finds the run it left, because
	// the run is in the registry rather than in the connection it lost.
	runs := s.Runs.List()
	b.WriteString("\n<h2>Runs</h2>\n")
	if len(runs) == 0 {
		b.WriteString("<pre>" + html.EscapeString(prose.Para(
			"Nothing is running. Starting a run from here is the next slice; "+
				"until then `winthistle run --batch FILE` is how a batch is "+
				"opened, and this page is where it will be attached to.")) +
			"</pre>\n")
	} else {
		b.WriteString("<ul>\n")
		for _, run := range runs {
			_, finished, err := run.State()
			state := "running"
			switch {
			case finished && err != nil:
				state = "stopped: " + err.Error()
			case finished:
				state = "finished"
			}
			fmt.Fprintf(&b, "<li><a href=\"/runs/%s\">%s</a> — started %s, %s</li>\n",
				html.EscapeString(run.ID), html.EscapeString(run.ID),
				run.Started.Format(time.RFC3339), html.EscapeString(state))
		}
		b.WriteString("</ul>\n")
	}
	serve(w, b.String())
}

func overview(cfgPath string) string {
	var b strings.Builder
	b.WriteString("winthistle\n\n")
	b.WriteString(prose.Para("Batch-open Lightning channels in one on-chain " +
		"transaction, funded from cold storage. This UI is local: it is bound to " +
		"the address it printed at startup, it holds a token that is generated " +
		"fresh on every start, and it sends no CORS headers and accepts no " +
		"cross-origin request."))
	b.WriteString("\n")
	b.WriteString(prose.Para("Reading " + cfgPath + "."))
	b.WriteString("\n")
	b.WriteString(prose.Para("Closing this tab does not stop anything. A tab " +
		"closing is indistinguishable from a reload, a laptop lid or a Wi-Fi " +
		"blip, and a partially-armed batch is the last thing that should be taken " +
		"apart on that evidence. What ends a run is the clock — the signing gate, " +
		"and the ten minutes each peer gives its own reservation — or Ctrl-C in " +
		"the terminal winthistle is running in, which cancels the run and unwinds " +
		"it through the abort path. Come back to this page and the run will be " +
		"here."))
	return b.String()
}

// doctor is the one screen this slice serves end to end.
//
// Chosen because it has no clock, no signer and no state: it reads, it renders,
// and the one thing it creates is the journal file, because a journal that does
// not exist yet is not a fault. Nothing here can arm, publish or abort.
//
// r.Context() is passed to the pre-flight, so a browser that gives up stops the
// checks. That is right for a read-only screen and it is exactly what must never
// happen to a run — see Registry. The difference is not the transport, it is
// that a pre-flight has nothing to unwind and a run has peers holding
// reservations.
func (s *Server) doctor(w http.ResponseWriter, r *http.Request) {
	// One pre-flight at a time. Two would open the run journal twice and report
	// the same failure in two places, and a reloaded slow page is how that
	// happens.
	s.doctorMu.Lock()
	defer s.doctorMu.Unlock()

	report := doctor.Run(r.Context(), s.cfg, s.opts.Doctor)
	serve(w, screen("doctor", "/doctor", report.Report()))
}

// attach renders a run from the beginning.
//
// Every time, rather than from a cursor: an attach that resumed where some
// earlier connection stopped would be a subscription, and a subscription is a
// thing a tab owns. This is a view.
func (s *Server) attach(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	run := s.Runs.Get(id)
	if run == nil {
		w.WriteHeader(http.StatusNotFound)
		serve(w, screen("run", "", prose.Para(fmt.Sprintf(
			"There is no run called %q in this process. A run is in the registry "+
				"only while the winthistle that started it is still running; a run "+
				"from an earlier process is in the journal instead, and "+
				"`winthistle recover` is what reads that.", id))))
		return
	}

	transcript, finished, err := run.State()
	var b strings.Builder
	fmt.Fprintf(&b, "run %s\n\nStarted %s.\n", run.ID, run.Started.Format(time.RFC3339))
	switch {
	case finished && err != nil:
		b.WriteString("\n" + prose.Para("It stopped: "+err.Error()))
	case finished:
		b.WriteString("\n" + prose.Para("It finished."))
	default:
		b.WriteString("\n" + prose.Para("It is still going. Reload to see more; "+
			"closing this tab does not stop it."))
	}
	b.WriteString("\n" + transcript)
	serve(w, screen("run "+run.ID, "", b.String()))
}
