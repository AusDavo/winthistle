package server

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/AusDavo/winthistle/internal/prose"
)

// The recovery screens: the runs the journal has left unfinished, and one of
// them at a time.
//
// This is the screen an operator reads on a node that is down. It needs nothing
// but the journal — no LND, no Core, no run in the registry — which is why it is
// the one to build before the rest, and why nothing in this file dials anything.
// prose.RecoveryList and prose.Recovery already promise that ("it is the same
// screen on a node that is down") and a handler that quietly asked LND for one
// figure would break the promise from underneath them.
//
// # A journalled run is not a live run, and this screen must not let those blur
//
// Registry holds the runs this process is driving. The journal holds runs that
// stopped, possibly from a process that is no longer here. They overlap: a run
// started from this UI writes journal rows while it goes, so for its whole life
// it is in both, and an operator who reads a journal row as "this is running"
// will wait for something that is not happening.
//
// Three things keep them apart, and all three are deliberate:
//
//   - Two screens, two paths, two vocabularies. The registry's runs are on the
//     overview under "Runs" and at /runs/{id}. The journal's are here, under
//     /recover, and every heading on this screen says journal.
//   - Every rendering of a journal row is prefixed by what this process knows
//     about it. journalNote and journalledNote below are the only copy in this
//     file that is not prose's, and they exist for exactly this: to say, above
//     the row, whether the run it describes is going right now.
//   - The empty list is covered too, and that is the case worth naming. A run
//     that started a moment ago has not written its row yet, so the journal can
//     be empty while a run is going — and "No unfinished runs. Nothing to
//     recover." over a live batch is precisely the half-truth that produces a
//     second incident. journalNote says so when it happens.
//
// # This screen offers no abort, and that is a decision
//
// run.RecoverOne would be the fourth unsafe method in this repository and the
// first that abandons channels on a run this process never started. It asks
// abort.Confirmation once per channel — a func per channel, deliberately, so
// that a yes about one channel cannot authorise another — and a browser answers
// those through server.Run.Ask, which needs a Run. A journalled run has none.
//
// Inventing one is the move to refuse. A synthetic registry entry for a run this
// process is not driving would put a journal row on /runs/{id} with a question
// and an abort control on it, which is the blur the whole screen is built to
// prevent, at the one moment the operator is least able to afford it.
//
// So this ships the listing and refuses the abort, and points at `winthistle
// recover ID`, which already exists, already asks per channel, and is already
// tested on both shapes of a bad night. There is no POST in this file and no
// POST route under /recover — the mux's method patterns make one a 405 rather
// than a handler somebody forgot to write.
//
// What it does still ask is whether a run may be aborted *at all*, because that
// decides whether to point at that command or warn against it. The answer comes
// from journal.Run.AbortTarget through Launcher.AbortRefusal — the same function
// run.RecoverOne refuses on, and the same one the live abort control asks. This
// package cannot hold a second copy of that rule: it may not import
// internal/journal.
//
// # The context is the server's, not the request's
//
// Both handlers take their context from Server.base with a timeout, which is
// what the abort control's journal read does. TestOnlyTheDoctorScreenReadsThe-
// RequestContext requires it, and the reason it is right rather than merely
// required is the same one: a browser that gives up mid-read must not be able to
// change what a screen says. On the abort control that would turn a refusal into
// a permission. Here it would turn "the journal has these three runs" into "the
// journal could not be read", which is the same class of mistake with a smaller
// blast radius, and there is no reason to prefer it.

// journalReadTimeout bounds the journal reads behind these screens.
//
// Local SQLite, so the ordinary case is milliseconds. The case it is sized for
// is a contended journal — another winthistle holding the write lock, against
// journal.Open's own 5-second busy timeout — where a few queries can queue.
// Generous enough that a slow answer is still an answer, and short enough that
// a browser is told something rather than left holding a socket open.
//
// Not abortCheckTimeout, which is the same shape and a different question. That
// one bounds a refusal, where an unanswered question is a no; this one bounds a
// screen, where an unanswered question is an error page.
const journalReadTimeout = 30 * time.Second

// recoverList is the runs that stopped, and the CPFP children left half-done.
func (s *Server) recoverList(w http.ResponseWriter, r *http.Request) {
	if s.opts.Launcher == nil {
		s.refuseScreen(w, http.StatusNotImplemented, "the run journal", noJournal())
		return
	}

	ctx, cancel := context.WithTimeout(s.base(), journalReadTimeout)
	defer cancel()

	text, ids, err := s.opts.Launcher.Unfinished(ctx)
	if err != nil {
		s.refuseScreen(w, http.StatusInternalServerError, "the run journal",
			journalUnreadable(err))
		return
	}

	var b strings.Builder
	b.WriteString(screen("the run journal", "/recover",
		journalNote(s.Runs.Live(), ids)+"\n"+text))

	// The links, after the copy the oracle covers. Chrome, like the nav: the ids
	// are already in the text above, and these only save the operator retyping
	// one.
	if len(ids) > 0 {
		b.WriteString("\n<ul>\n")
		for _, id := range ids {
			fmt.Fprintf(&b, "<li><a href=\"%s\">%s</a></li>\n",
				recoverPath(id), html.EscapeString(id))
		}
		b.WriteString("</ul>\n")
	}
	serve(w, b.String())
}

// recoverOne is one journalled run: what it is, and what an abort would do.
func (s *Server) recoverOne(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.opts.Launcher == nil {
		s.refuseScreen(w, http.StatusNotImplemented, "the run journal", noJournal())
		return
	}

	ctx, cancel := context.WithTimeout(s.base(), journalReadTimeout)
	defer cancel()

	text, err := s.opts.Launcher.Journalled(ctx, id)
	if errors.Is(err, ErrNoJournalledRun) {
		w.WriteHeader(http.StatusNotFound)
		serve(w, screen("the run journal", "/recover", noSuchJournalledRun(id)))
		return
	}
	if err != nil {
		s.refuseScreen(w, http.StatusInternalServerError, "the run journal",
			journalUnreadable(err))
		return
	}

	// Asked, not remembered. Whether this run reached the publish call is
	// journal.Run.AbortTarget's answer, and this screen needs it to decide
	// whether to name the command that takes a run apart or to warn against it.
	//
	// This is the journal's second open for one page, and it stays that way. The
	// obvious tidy — one launcher method returning the text and the refusal
	// together — would put "may this be aborted" and "what does it look like" in
	// one answer, and the first of those has to keep coming from the same
	// function run.RecoverOne refuses on rather than from something rendered
	// alongside it. Two short reads of a local SQLite file is the cheaper half of
	// that trade.
	refusal := s.abortRefusal(id)

	var b strings.Builder
	b.WriteString(screen("run "+id+" in the journal", "/recover",
		journalledNote(id, s.Runs.Get(id))+"\n"+text+"\n"+readOnlyNote(id, refusal)))
	b.WriteString("<p><a href=\"/recover\">back to the journal</a></p>\n")
	serve(w, b.String())
}

// recoverPath is one run's journal screen, escaped for both the path and the
// attribute it goes in.
//
// A run id comes off the URL here rather than out of NewRunID, so it is not this
// package's string to trust: a '/' or a quote in it would otherwise land in a
// path segment or an attribute that meant something else.
func recoverPath(id string) string {
	return html.EscapeString("/recover/" + url.PathEscape(id))
}

// journalNote is what this process knows about the list underneath it.
//
// The one piece of copy on this screen that is not prose's, and it is here
// because prose cannot write it: prose.RecoveryList is shared with `winthistle
// recover`, which has no registry and nothing running. Whether a row is also a
// live run is a fact about this process, so it is stated by this process, above
// the rows rather than inside them.
func journalNote(live *Run, ids []string) string {
	var b strings.Builder
	b.WriteString("the run journal\n\n")

	if live == nil {
		b.WriteString(prose.Para("This is the journal on disk, which is a " +
			"different thing from the runs this winthistle is driving. Nothing is " +
			"running in this process, so everything below is a record of a run " +
			"that stopped — possibly from a process that is no longer here. None " +
			"of it is happening now."))
		return b.String()
	}

	// A live run that is not a batch will never be in this list, and saying it
	// has "not written a row yet" would imply one is coming. A setup writes a
	// setups row keyed by the wallet and no run row at all, so the honest thing
	// is to say the list below is complete and name the other thing that is going.
	if live.Kind != KindBatch {
		b.WriteString(prose.Para(whatIsGoing(live) + ", and it will never appear " +
			"in the list below: a setup records its answer against the wallet " +
			"rather than as a run, so it has no run row to be unfinished. What is " +
			"below is the runs, and it is complete."))
		b.WriteString("\n")
		b.WriteString(prose.Para(fmt.Sprintf("That setup is at /runs/%s. Nothing "+
			"about it is armed, and nothing below is happening.", live.ID)))
		return b.String()
	}

	if inList(live.ID, ids) {
		b.WriteString(prose.Para(fmt.Sprintf(
			"Run %s is going in this process right now, and it is in the list "+
				"below because the journal has its row too. That row is what it had "+
				"written to disk when this page was rendered: a snapshot, not a view "+
				"of the run. The run itself is at /runs/%s, which is the screen with "+
				"its transcript, the question it may be waiting on, and the control "+
				"that stops it.", live.ID, live.ID)))
		return b.String()
	}

	// The dangerous case. journal.Begin has not written this run's row yet, so
	// the list below can say "No unfinished runs. Nothing to recover." while a
	// batch is being armed — which is the half-truth this whole screen exists
	// not to tell.
	b.WriteString(prose.Para(fmt.Sprintf(
		"Run %s is going in this process right now, and it is not in the list "+
			"below: it has not written a row to this journal yet. So an empty list "+
			"here does not mean this node is clean. The run itself is at /runs/%s, "+
			"which is the screen with its transcript, the question it may be "+
			"waiting on, and the control that stops it.", live.ID, live.ID)))
	return b.String()
}

// journalledNote is the same distinction, for one run.
func journalledNote(id string, live *Run) string {
	if live != nil {
		return prose.Para(fmt.Sprintf(
			"Run %s is going in this process right now. What follows is its journal "+
				"row read off disk, which is always a little behind the run itself: "+
				"/runs/%s is the live screen, with the transcript, the question it "+
				"may be waiting on, and the control that stops it.", id, id))
	}
	return prose.Para(fmt.Sprintf(
		"This is run %s as the journal has it, and nothing in this process is "+
			"driving it — so nothing below is going to change while you read it. If "+
			"another winthistle is driving this run, this page cannot tell: two "+
			"winthistles against one journal is what `winthistle doctor` reports.",
		id))
}

// readOnlyNote is what this screen will not do, and who does it instead.
//
// It is the fourth unsafe method, declined. See the file comment: an abort of a
// journalled run asks per channel, a browser answers through a Run, and a
// journalled run has none — so the honest shape is a screen that reads and a
// command that acts, rather than a control that half-works.
func readOnlyNote(id string, refusal error) string {
	var b strings.Builder

	b.WriteString(prose.Para("This screen is read-only. Nothing here abandons a " +
		"channel, cancels a shim or releases a coin lock."))
	b.WriteString("\n")

	if refusal == nil {
		b.WriteString(prose.Para(fmt.Sprintf(
			"`winthistle recover %s`, in the terminal, is what takes it apart. It "+
				"asks about each channel separately — one question per channel, with "+
				"LND's own refusal quoted in it — because an answer given once about "+
				"one channel must not authorise the next one.", id)))
		return b.String()
	}

	b.WriteString(prose.Para("And this run must not be taken apart at all: " +
		refusal.Error()))
	b.WriteString("\n")
	b.WriteString(prose.Para(fmt.Sprintf(
		"That refusal is the journal's rather than this page's. This UI does not "+
			"keep its own copy of the rule — it asks the same question `winthistle "+
			"recover %s` asks, and gets the same answer, so the two cannot come "+
			"apart. Running that command would refuse in the same words.", id)))
	return b.String()
}

func noSuchJournalledRun(id string) string {
	var b strings.Builder
	b.WriteString("the run journal\n\n")
	b.WriteString(prose.Para(fmt.Sprintf(
		"The journal has no run called %q. A run that finished cleanly or was "+
			"aborted cleanly is not on this screen either — it is still in the "+
			"journal, and this screen lists only what was left unfinished.", id)))
	return b.String()
}

func noJournal() string {
	return prose.Para("This server cannot read the run journal. It was started " +
		"without the launcher that owns it, which is the shape a test drives and " +
		"not one `winthistle serve` produces — `winthistle recover` reads the " +
		"same journal from the terminal and needs nothing from this process.")
}

func journalUnreadable(err error) string {
	var b strings.Builder
	b.WriteString("the run journal\n\n")
	b.WriteString(prose.Para("The journal could not be read: " + err.Error()))
	b.WriteString("\n")
	b.WriteString(prose.Para("So this page is not saying there is nothing to " +
		"recover. It is saying it does not know, which is a different thing and " +
		"the one worth acting on: `winthistle recover` from the terminal reads " +
		"the same file and will say why in more detail."))
	return b.String()
}

func inList(id string, ids []string) bool {
	for _, other := range ids {
		if other == id {
			return true
		}
	}
	return false
}
