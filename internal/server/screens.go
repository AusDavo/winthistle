package server

import (
	"context"
	"errors"
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

// In the order an operator moves through them: what this is, whether anything is
// wrong, the cold wallet's commissioning, then the three reports about the batch
// and the node it would be opened against, then the journal.
//
// Every one of them is here rather than reachable only from another page. Copy
// that names a screen the nav cannot reach has been found four times in this UI,
// and the cure each time was a link — so a screen that ships gets its entry in
// the same commit.
var nav = []link{
	{"/", "overview"},
	{"/doctor", "doctor"},
	{"/setup", "setup"},
	{"/peers", "peers"},
	{"/fees", "fees"},
	{"/reserve", "reserve"},
	{"/recover", "recover"},
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
form { margin: 1.5rem 0 0; max-width: %dch; }
label { display: inline-block; }
textarea { display: block; width: 100%%; box-sizing: border-box; font: inherit;
  white-space: pre-wrap; overflow-wrap: break-word; margin: .25rem 0 1rem; }
/* Block, like the textarea it is the alternative to. Inline, the file picker
   flowed into the button row and split the two choices across two lines, so
   "This is the signed packet" and "cold1 cannot sign" stopped reading as the
   pair they are. Found by rendering it. */
input[type=file] { display: block; font: inherit; margin: .25rem 0 1.25rem; }
button { font: inherit; padding: .4rem .9rem; margin: 0 .5rem .5rem 0; }
h2 { font: inherit; font-weight: 600; margin: 2rem 0 .5rem; }
</style>
</head><body>
<nav>`, html.EscapeString(title), prose.PaneWidth, prose.PaneWidth, prose.PaneWidth)

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

	// What a run started from here would open, and the control that starts it.
	// Both are absent rather than disabled when there is no batch: a control
	// that is offered and then refused teaches an operator to press it twice.
	b.WriteString(s.startSection())

	// The run list is the only place a link is generated from state, and it is
	// what decision 2 buys: a tab that comes back finds the run it left, because
	// the run is in the registry rather than in the connection it lost.
	runs := s.Runs.List()
	b.WriteString("\n<h2>Runs</h2>\n")
	if len(runs) == 0 {
		b.WriteString("<pre>" + html.EscapeString(prose.Para(
			"Nothing has run in this process yet. A run started from the command "+
				"line with `winthistle run --batch FILE` is not in this list either: "+
				"this list is the registry, which is per process, and the journal is "+
				"what outlives one. The journal is on the recover screen.")) +
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
			// What it is, as well as how it went. Two rows that differ only by id
			// are two rows an operator cannot tell apart, and a setup and a batch
			// are not remotely the same thing to attach to.
			fmt.Fprintf(&b, "<li><a href=\"/runs/%s\">%s</a> — %s, started %s, %s</li>\n",
				html.EscapeString(run.ID), html.EscapeString(run.ID),
				html.EscapeString(what(run)), run.Started.Format(time.RFC3339),
				html.EscapeString(state))
		}
		b.WriteString("</ul>\n")
	}
	serve(w, b.String())
}

// startSection is the batch and the control that opens it.
//
// The batch summary is the launcher's text rather than this package's: the
// launcher is the thing holding the batch file, and a second renderer of the
// same amounts is a second thing to keep in step with the plan document.
// progressBlock is the journal's account of the run, or nothing.
//
// Nothing, rather than an error, in the two cases that are not faults: no
// launcher at all — a server started without one cannot have run this — and a
// run that has journalled nothing, whose streams have not opened. Neither is
// worth a paragraph on a screen whose transcript already says where the run is.
//
// A journal that could not be *read*, though, is said out loud. The receipt
// count is the live reading of I-1, and a screen that quietly omitted it would
// look like a run with no channels rather than a question nobody could answer.
// The context is the server's and not the request's, which decision 2's guard
// enforces rather than trusts: a response ends on a reload, a closed tab and a
// laptop lid, and none of those is a reason to abandon a journal read. It is
// bounded the way the other journal reads are — a screen that hung would be a
// screen an operator cannot get the receipt count out of.
func (s *Server) progressBlock(id string) string {
	if s.opts.Launcher == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(s.base(), journalReadTimeout)
	defer cancel()

	text, err := s.opts.Launcher.Progress(ctx, id)
	switch {
	case errors.Is(err, ErrNoJournalledRun):
		return ""
	case err != nil:
		return prose.Para(fmt.Sprintf("The run journal could not be read, so how "+
			"many channels have their receipt is not known from here: %v\n"+
			"That is a question about this run's state and not about the run — the "+
			"transcript below is still what the run itself said.", err))
	}
	return text
}

func (s *Server) startSection() string {
	var b strings.Builder
	b.WriteString("\n<h2>The batch</h2>\n")

	if s.opts.Launcher == nil || s.opts.Launcher.Batch() == "" {
		return b.String() + "<pre>" + html.EscapeString(noBatch()) + "</pre>\n"
	}
	b.WriteString("<pre>" + html.EscapeString(s.opts.Launcher.Batch()) + "</pre>\n")

	if live := s.Runs.Live(); live != nil {
		// Kind-aware, because the sentence underneath is about coin selection and
		// only two batches collide over that. A setup reads descriptors and derives
		// addresses; describing it as a decoy over these coins would be a false
		// statement in operator copy on the first screen anybody sees.
		b.WriteString("<pre>" + html.EscapeString(nothingToStart(live)) + "</pre>\n")
		return b.String()
	}
	b.WriteString(startForm())
	return b.String()
}

// whatIsGoing is one clause naming the live run, in the vocabulary of whichever
// of the three things it is.
//
// It exists because the sentence that used to be written wherever it is now
// called — the second run's dress rehearsal building a decoy over the same coins
// — is true of a batch and false of the other two. Four screens were saying it,
// and a refusal that explains a collision that could not have happened is a false
// statement in operator copy on the one page somebody reads while they are
// already lost.
func whatIsGoing(live *Run) string {
	switch live.Kind {
	case KindSetup:
		return fmt.Sprintf("A setup of the cold wallet %s is going in this "+
			"process right now, as run %s", live.About, live.ID)
	case KindBump:
		return fmt.Sprintf("A CPFP child of run %s is being built in this "+
			"process right now, as run %s", live.About, live.ID)
	default:
		return fmt.Sprintf("Run %s is opening the batch right now", live.ID)
	}
}

// what is one noun phrase for a run, for the list on the overview.
//
// Short on purpose: it sits inside a line that already carries an id, a
// timestamp and a state.
func what(r *Run) string {
	switch r.Kind {
	case KindSetup:
		return "a setup of " + r.About
	case KindBump:
		return "a CPFP child of run " + r.About
	default:
		return "the batch"
	}
}

// nothingToStart is why the batch's control is absent, in the vocabulary of
// whatever is actually going.
func nothingToStart(live *Run) string {
	if live.Kind != KindBatch {
		return prose.Para(whatIsGoing(live) + ", so there is nothing to start " +
			"here yet. One at a time, because there is one journal and one cold " +
			"wallet — and a batch and the thing that is going both want the same " +
			"devices out of the same safe. Nothing about it is armed, and no batch " +
			"is at risk in it.")
	}
	return prose.Para(fmt.Sprintf("Run %s is going, so there is nothing to "+
		"start. One at a time: there is one journal, one cold wallet and one "+
		"armed window, and a second run's dress rehearsal would build a decoy "+
		"over the coins this one is about to spend.", live.ID))
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

// doctor is the read-only screen, and the only handler in this package that has
// no clock, no signer and no state: it reads, it renders, and the one thing it
// creates is the journal file, because a journal that does not exist yet is not a
// fault. Nothing here can arm, publish or abort.
//
// It is also the one place a request context is used, and the only one.
// r.Context() is passed to the pre-flight, so a browser that gives up stops the
// checks — right for a read-only screen and exactly what must never happen to a
// run. The difference is not the transport: a pre-flight has nothing to unwind
// and a run has peers holding reservations against outpoints.
// TestOnlyTheDoctorScreenReadsTheRequestContext is what keeps it here.
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
//
// Deliberately no auto-refresh. A <meta http-equiv="refresh"> would work — the
// CSP forbids fetching, not reloading — and it would be wrong here, because the
// thing the operator is most often doing on this screen is pasting a signed PSBT
// into a textarea, and a page that reloads under them loses it. Reloading is the
// operator's, and the copy says so.
func (s *Server) attach(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	run := s.Runs.Get(id)
	if run == nil {
		s.noSuchRun(w, id)
		return
	}

	transcript, finished, err := run.State()
	pending := run.Pending()

	var text strings.Builder
	fmt.Fprintf(&text, "%s\n\nStarted %s.\n", heading(run),
		run.Started.Format(time.RFC3339))
	switch {
	case finished && err != nil:
		text.WriteString("\n" + prose.Para("It stopped: "+err.Error()))
	case finished:
		text.WriteString("\n" + prose.Para("It finished."))
	case run.Aborting():
		text.WriteString("\n" + prose.Para("It has been asked to stop and is "+
			"unwinding: cancelling the shims, abandoning what reached pending, "+
			"releasing Core's coin locks. Reload to see how far it has got."))
	case pending != nil:
		text.WriteString("\n" + waitingOn(run, pending))
	default:
		text.WriteString("\n" + prose.Para("It is still going. Reload to see more; "+
			"closing this tab does not stop it."))
	}

	// The state block, then the transcript, then the question.
	//
	// State first because it is the thing being watched: the transcript grows
	// without bound and pushes its own top off the screen, so a receipt count at
	// the end of it is a receipt count nobody finds. The order is deliberate the
	// other way round from the transcript's own — what the run is *waiting on*
	// still goes last, next to the form that answers it.
	//
	// A batch only, because the receipt count is a batch's. A setup opens no
	// channel and a bump's channels are already funded, so there is no number
	// there for I-1 to turn on — and asking for one would be asking the journal
	// about an id it has never had.
	if run.Kind == KindBatch {
		text.WriteString("\n" + s.progressBlock(id))
	}
	// Above the transcript, with the rest of what this screen knows about the run
	// rather than below a transcript that grows without bound. It answers "what
	// can I do here", which is a question asked on arrival.
	if !finished && run.Kind != KindBatch {
		text.WriteString("\n" + wayOut(run))
	}
	text.WriteString("\n" + transcript)
	if pending != nil {
		text.WriteString("\n" + pending.Prompt)
	}

	// One <pre> for everything that is copy — the transcript and the question
	// both go through screen(), which is what keeps decision 3's oracle over
	// strings this repository did not choose — and then the controls.
	var b strings.Builder
	b.WriteString(screen(title(run), "", text.String()))
	if pending != nil {
		b.WriteString(questionForm(run.ID, pending))
	}
	if !finished && run.Kind == KindBatch {
		// The abort control decision 2 makes mandatory: if a closed tab does not
		// stop a run, something has to, and it is a link to a screen that says
		// what stopping costs rather than a button that does it on one click.
		//
		// A batch only. What that control cancels is an armed window with n shims,
		// n pending channels and Core's coin locks behind it, and the screen behind
		// it says so in three paragraphs; a setup and a bump have none of those and
		// each has its own way out, which wayOut states above instead. Offering
		// this on them would be offering a teardown of nothing, described as a
		// teardown of a batch.
		fmt.Fprintf(&b, "<p><a href=\"/runs/%s/abort\">stop this run</a></p>\n",
			html.EscapeString(run.ID))
	}
	serve(w, b.String())
}

// heading is the first line of a run's screen, in the vocabulary of whichever of
// the three things it is.
//
// A batch keeps "run %s" exactly, because that id is the journal's and every
// other screen in this UI already calls it that. A setup says what it is about as
// well, because its id is this process's alone and nothing else on the page
// carries the wallet's name until the question arrives.
//
// It leads with the id rather than with the description, and that is a render
// fix rather than a preference: webrun's own prompt opens with "The cold wallet's
// setup — <wallet>", so a heading phrased the same way put the identical line on
// the screen twice with a paragraph between them.
func heading(r *Run) string {
	switch r.Kind {
	case KindSetup:
		return fmt.Sprintf("run %s — a setup of the cold wallet %s", r.ID, r.About)
	case KindBump:
		return fmt.Sprintf("run %s — a CPFP child of run %s", r.ID, r.About)
	default:
		return "run " + r.ID
	}
}

// title is the browser tab's, which is a different job from heading's: it has one
// line and no room to explain, and an operator with three tabs open is choosing
// between them by the first few words.
func title(r *Run) string {
	switch r.Kind {
	case KindSetup:
		return "setup " + r.ID
	case KindBump:
		return "bump of run " + r.About
	default:
		return "run " + r.ID
	}
}

// waitingOn is the clock sentence over a pending question, and there are two
// clocks rather than one.
//
// A batch's is the signing gate — limits.abort_after_signing_seconds, the number
// rehearsal.Gate measures a round against — and letting it pass costs one more
// signing round. A setup's is not a gate at all: nothing is armed, no peer is
// waiting, and letting it pass records nothing, which is the same verdict its
// third button gives. A bump's is a budget rather than a gate too, and its
// expiry has a consequence neither of the others has — the coin lock on the
// batch's change output goes back.
//
// Calling any of the other two a signing gate would name a mechanism that is not
// running, and the bump one said it over the question about whether to build a
// child at all, which signs nothing. Found by looking at the page.
//
// A batch has one question that is not a signing round either, and it said the
// signing-gate sentence for several slices: the blunt-abandon confirmation, asked
// during a teardown. Nothing is being signed then — the round is over and the
// batch is being taken apart — and letting it pass does not cost one more signing
// round, it costs an abort that has to be finished by hand, which is a different
// thing to be told while authorising i_know_what_i_am_doing. What separates them
// is Question.Reply: a signing question asks for a packet back and a decision
// does not, which is the same distinction questionForm already draws to decide
// the form's enctype. Found by looking at the page, on the run that mixed its
// transports.
func waitingOn(r *Run, q *Question) string {
	left := q.Deadline.Sub(q.Asked).Round(time.Second)
	if r.Kind == KindBatch && q.Reply == "" {
		return prose.Para(fmt.Sprintf("It is waiting on you. Answer below, before "+
			"%s, which is %s from when it was asked. That is not a signing gate — "+
			"the signing round is over and this batch is being taken apart. Letting "+
			"it pass declines the escalation, which leaves an abort to be finished "+
			"by hand rather than anything that was at risk.",
			q.Deadline.Format(time.TimeOnly), left))
	}
	switch r.Kind {
	case KindSetup:
		return prose.Para(fmt.Sprintf("It is waiting on you. Answer below, before "+
			"%s: this question is held open for %s, and that is not a gate on "+
			"anything — nothing is armed and no peer knows this is happening. "+
			"Letting it pass records nothing at all, which is exactly what the "+
			"third button records.",
			q.Deadline.Format(time.TimeOnly), left))
	case KindBump:
		// One sentence for both of a bump's questions — whether to build the
		// child, and then each device's signature over it. Neither of them is the
		// gate this UI means everywhere else: the batch is already public, no peer
		// is waiting, and what expiry costs is the same in both cases.
		return prose.Para(fmt.Sprintf("It is waiting on you. Answer below, before "+
			"%s, which is %s from when it was asked. Letting it pass costs only "+
			"this attempt: no child is signed, and the coin lock Core may be "+
			"holding on the batch's change output is released. The batch is "+
			"untouched either way, and `winthistle bump` can be run again.",
			q.Deadline.Format(time.TimeOnly), left))
	}
	return prose.Para(fmt.Sprintf("It is waiting on you. Answer below, before %s "+
		"— that is the %s signing gate, not a timeout of this page's, and letting "+
		"it pass costs one more signing round rather than anything that was at "+
		"risk.", q.Deadline.Format(time.TimeOnly), left))
}

// wayOut is what to do instead of the abort control, for the two kinds that do
// not have one.
//
// Written rather than omitted, because decision 2's own argument applies to every
// kind: a closing tab stops nothing, so the operator needs to be told what does.
// The difference is that here the answer is a button already on the screen rather
// than a control that cancels a context.
//
// It says only what this screen knows and webrun's prompt does not. The prompt
// already explains, at length, that the third button records nothing and that the
// addresses are there tomorrow; repeating either was a duplication a browser
// showed and no test would have.
func wayOut(r *Run) string {
	if r.Kind == KindBump {
		// What only this screen knows, and no more. The state line above already
		// says what expiry costs and the signing prompt says it again; three
		// copies of one sentence on one page is a duplication a browser shows.
		return prose.Para("There is no control on this screen that stops a bump, " +
			"and the clean way out is one of the answers rather than a control: a " +
			"device that cannot sign fails the round. A batch run has a stop control " +
			"because it has funding shims open and channels that reached pending; " +
			"this batch's channels are funded and its funding transaction is public, " +
			"so there is nothing here to unwind. If it is interrupted some other " +
			"way — Ctrl-C, or nobody here when the window passes — `winthistle bump " +
			"--abandon " + r.About + "` hands back a coin lock left behind.")
	}
	return prose.Para("There is no control on this screen that stops a setup, and " +
		"there is nothing for one to stop. A batch run has that control because it " +
		"has funding shims open, channels that reached pending and coin locks Core " +
		"is holding; this has two read-only RPCs and a question. Every way of not " +
		"answering it ends the same: nothing recorded.")
}
