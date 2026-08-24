package server

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/AusDavo/winthistle/internal/prose"
)

// This file is every route that changes something. There are three, and until
// this slice there were none.
//
// # The first unsafe methods in this repository
//
// The guard already refuses an unsafe method that cannot say where it came from
// — no Origin and no Sec-Fetch-Site: same-origin is a refusal, not a
// fallthrough — so a POST added here lands behind a check that was written
// before there was anything to check. What is new is everything else about a
// POST: it can start something, it can answer something, and it can stop
// something.
//
// What it cannot do is publish. Two locks, neither of them a convention:
// Method.CallSites pins WalletKit.PublishTransaction at two production call
// sites and internal/methods' test type-checks the whole module and fails on a
// third; and internal/server may not import internal/arm, internal/bump or
// internal/journal, which imports_test.go enforces. So no handler in this file
// can name an *arm.Armed, a *bump.Signed, or the table a setup answer is
// recorded in. What the server may do is start a run and answer its questions.
//
// # The run's context is not the request's
//
// startRun derives the run's context from the server's, and the only thing that
// cancels it is Run.Abort. A request context ends when the response ends, and
// decision 2 is that a response ending means nothing at all: a tab closing is
// indistinguishable from a reload, a laptop lid and a Wi-Fi blip, and a
// partially-armed batch is the last thing that should be taken apart on that
// evidence.
//
// r.Context() appears exactly once in this package, in the doctor screen, and
// TestOnlyTheDoctorScreenReadsTheRequestContext holds it there. A pre-flight has
// nothing to unwind; a run has peers holding reservations.

// maxAnswer bounds a posted form. The signed PSBT of a large batch is the only
// big thing an operator ever pastes in, and a megabyte is far more than one:
// bitcoind's own relay rules would refuse the transaction long before its packet
// reached this size.
const maxAnswer = 1 << 20

// abortCheckTimeout bounds the journal read behind the abort control. It is a
// local SQLite query, and if it cannot be answered the control is refused rather
// than offered — the question being asked is whether a transaction may already
// be public, and an unanswered version of that question is a no.
const abortCheckTimeout = 5 * time.Second

// startRun is the POST that gives Registry its first production caller.
func (s *Server) startRun(w http.ResponseWriter, r *http.Request) {
	if s.opts.Launcher == nil || s.opts.Launcher.Batch() == "" {
		s.refuseScreen(w, http.StatusNotImplemented, "no batch", noBatch())
		return
	}
	if err := r.ParseForm(); err != nil {
		s.refuseScreen(w, http.StatusBadRequest, "run not started",
			prose.Para("The form could not be read: "+err.Error()))
		return
	}

	req := StartRequest{
		Probe:             r.PostForm.Get("probe") != "",
		StopBeforePublish: r.PostForm.Get("stop-before-publish") != "",
	}

	id, err := NewRunID()
	if err != nil {
		s.refuseScreen(w, http.StatusInternalServerError, "run not started",
			prose.Para(err.Error()))
		return
	}

	// Decision 2, in one line. The run's context is the server's with a cancel
	// of its own; nothing derived from this request reaches it, so nothing that
	// happens to this connection can end it.
	ctx, cancel := context.WithCancel(s.base())

	run, err := s.Runs.Start(id, KindBatch, "", cancel)
	if err != nil {
		cancel()
		code := http.StatusInternalServerError
		if errors.Is(err, ErrRunInFlight) {
			code = http.StatusConflict
		}
		s.refuseScreen(w, code, "run not started", alreadyRunning(err, s.Runs.Live()))
		return
	}

	go func() {
		defer cancel()
		run.Finish(s.opts.Launcher.Start(ctx, run, req))
	}()

	http.Redirect(w, r, "/runs/"+id, http.StatusSeeOther)
}

// answer hands one reply to the run that is waiting for it.
//
// The question id is posted back and checked, so a browser showing an older
// screen — the back button, a second tab, a double submit — cannot answer the
// question that replaced the one it was showing. That matters most for
// abort.Confirmation, which is a func per channel precisely so it cannot be set
// once and reused: each rejection asks again, with that channel's name and LND's
// own words in it, and an answer carries the id of the question it was given.
func (s *Server) answer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	run := s.Runs.Get(id)
	if run == nil {
		s.noSuchRun(w, id)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxAnswer)
	a, why := readAnswer(r)
	if why != "" {
		s.refuseRunScreen(w, http.StatusBadRequest, id, prose.Para(why))
		return
	}

	// Whatever came back, verbatim. A choice this question did not offer arrives
	// as no choice at all, and the seam's own adapter decides what that means:
	// nothing here fabricates a verdict out of a malformed form.
	err := run.Reply(r.PostForm.Get("question"), a)
	if errors.Is(err, ErrStaleQuestion) {
		s.refuseRunScreen(w, http.StatusConflict, id, staleAnswer())
		return
	}
	http.Redirect(w, r, "/runs/"+id, http.StatusSeeOther)
}

// refuseRunScreen is a refusal about a run, carrying the way back to it.
//
// Three refusals named the run screen and none of them could reach it: the nav
// carries overview, doctor and recover, so "go back to the run" was an
// instruction with nothing behind it — on pages an operator lands on precisely
// when they have lost their place, after a back button, a second tab or a form
// this handler would not take. Found by rendering the upload leg's refusal; the
// two older ones had it too.
func (s *Server) refuseRunScreen(w http.ResponseWriter, code int, id, text string) {
	w.WriteHeader(code)
	body := screen("run "+id, "", text)
	body += fmt.Sprintf("<p><a href=\"%s\">back to run %s</a></p>\n",
		runPath(id), html.EscapeString(id))
	serve(w, body)
}

// readAnswer turns the posted form into an Answer, or returns the sentence to
// refuse it with.
//
// Two encodings arrive here and the split is deliberate. A question that asks
// for a packet back renders a multipart form, because it carries a file input;
// the three that ask only for a decision stay urlencoded, so the parsing path
// under the blunt-abandon confirmation — the one prompt in this product where a
// human authorises something that could lose funds if the premise were wrong —
// is the path it always had.
//
// Nothing an operator uploads reaches the disk. http.MaxBytesReader has already
// capped the body at maxAnswer, and ParseMultipartForm is given the same number
// as its in-memory budget, so the body cannot exceed the budget and no part can
// spill to a temp file. That is arithmetic rather than a policy: raise one of the
// two and a signed PSBT starts being written to /tmp.
func readAnswer(r *http.Request) (Answer, string) {
	const unreadable = "That answer could not be read: "
	const retry = ". Nothing was recorded and the run is still asking — reload " +
		"the run screen and answer again."

	ct := r.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "multipart/form-data") {
		if err := r.ParseForm(); err != nil {
			return Answer{}, unreadable + err.Error() + retry
		}
		return Answer{
			Choice: r.PostForm.Get("choice"),
			Text:   strings.TrimSpace(r.PostForm.Get("reply")),
		}, ""
	}

	if err := r.ParseMultipartForm(maxAnswer); err != nil {
		return Answer{}, unreadable + err.Error() + retry
	}
	a := Answer{
		Choice: r.PostForm.Get("choice"),
		Text:   strings.TrimSpace(r.PostForm.Get("reply")),
	}

	file, header, err := r.FormFile("upload")
	switch {
	case errors.Is(err, http.ErrMissingFile):
		// Nothing chosen, which is the ordinary case: the operator pasted.
		return a, ""
	case err != nil:
		return Answer{}, unreadable + err.Error() + retry
	}
	defer file.Close()

	body, err := io.ReadAll(file)
	if err != nil {
		return Answer{}, "That file could not be read: " + err.Error() + retry
	}
	if len(body) == 0 {
		return Answer{}, "That file is empty, so there is no packet in it" + retry
	}
	if a.Text != "" {
		// Two packets, and choosing between them here would be a second place a
		// verdict is decided. The operator did two things and only they know
		// which one they meant.
		return Answer{}, "That form came back with a packet pasted and a file " +
			"chosen as well, and those are two different packets. Choosing " +
			"between them here would decide something only you can: nothing was " +
			"recorded, and the run is still asking. Open the run screen again and " +
			"give it one of them — paste the base64, or pick the file, not both."
	}
	a.Upload = body
	if header != nil {
		a.UploadName = header.Filename
	}
	return a, ""
}

// abortScreen is the confirmation in front of the abort control.
//
// A GET, and a screen rather than a dialog, because there is no JavaScript in
// this UI to put a dialog up with and because what abort costs deserves a
// paragraph rather than an "are you sure". It is also where the refusal lives: a
// run that reached the publish call is not offered the control at all.
func (s *Server) abortScreen(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	run := s.Runs.Get(id)
	if run == nil {
		s.noSuchRun(w, id)
		return
	}

	if run.Kind != KindBatch {
		s.refuseRunScreen(w, http.StatusNotFound, id, notABatch(run))
		return
	}

	if why := s.abortRefusal(id); why != nil {
		w.WriteHeader(http.StatusConflict)
		serve(w, screen("stop run "+id, "", abortRefused(id, why)))
		return
	}

	if _, finished, _ := run.State(); finished {
		serve(w, screen("stop run "+id, "", prose.Para(fmt.Sprintf(
			"Run %s has already stopped. There is nothing to cancel. If it left "+
				"anything behind, `winthistle recover %s` is the screen that says "+
				"what, and takes it apart.", id, id))))
		return
	}

	var b strings.Builder
	b.WriteString(screen("stop run "+id, "", abortWarning(id, run.Aborting())))
	if !run.Aborting() {
		fmt.Fprintf(&b, `
<form method="post" action="/runs/%s/abort">
<button type="submit">Stop the run and take it apart</button>
</form>
`, html.EscapeString(id))
	}
	fmt.Fprintf(&b, "<p><a href=\"/runs/%s\">back to the run</a></p>\n",
		html.EscapeString(id))
	serve(w, b.String())
}

// abortRun is the control itself: cancel the run's context, and let the run
// unwind through the abort path it already has.
//
// It does not tear anything down here, and that is the point. internal/run's
// armed window already answers a failure the only way it can be answered —
// cancel the shims, abandon what reached pending, release Core's locks, through
// journal.Recover, which is the same code `winthistle recover` runs. A second
// teardown written in a handler would be a second abort path, and the one thing
// worse than an abort that fails is two of them disagreeing.
func (s *Server) abortRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	run := s.Runs.Get(id)
	if run == nil {
		s.noSuchRun(w, id)
		return
	}
	if run.Kind != KindBatch {
		s.refuseRunScreen(w, http.StatusNotFound, id, notABatch(run))
		return
	}
	if why := s.abortRefusal(id); why != nil {
		w.WriteHeader(http.StatusConflict)
		serve(w, screen("stop run "+id, "", abortRefused(id, why)))
		return
	}
	run.Abort()
	http.Redirect(w, r, "/runs/"+id, http.StatusSeeOther)
}

// abortRefusal asks the journal, through the launcher, whether this run may be
// aborted at all.
//
// The answer is not kept here and not recomputed here. journal.Run.AbortTarget
// refuses a run in publishing or published with ErrMayBePublished, and
// run.RecoverOne refuses the same run for the same reason; this control has to
// refuse it too, and the only way to be sure it refuses the same set is to ask
// the same question. internal/server cannot import internal/journal — the import
// ban — so asking is the only route available, which is the point of the ban.
//
// No launcher means no run was started from here, so there is nothing to check.
func (s *Server) abortRefusal(id string) error {
	if s.opts.Launcher == nil {
		return nil
	}
	// Not the request's context. See the file comment: a browser that gives up
	// mid-check must not turn a refusal into a permission.
	ctx, cancel := context.WithTimeout(s.base(), abortCheckTimeout)
	defer cancel()
	return s.opts.Launcher.AbortRefusal(ctx, id)
}

// questionForm renders the controls for whatever the run is waiting on.
//
// The prompt itself is not here: it is copy, so it goes into the same <pre> the
// transcript does, through screen(), which is what keeps decision 3's oracle
// over it. What is here is the form — the buttons, and the two fields a signing
// round needs — and a form is chrome in the same way the nav is.
func questionForm(runID string, q *Question) string {
	var b strings.Builder
	// multipart only when there is a packet to send back, so the three
	// decision-only questions post exactly what they always posted. See
	// readAnswer: the blunt-abandon confirmation keeps its parsing path.
	enc := ""
	if q.Reply != "" {
		enc = ` enctype="multipart/form-data"`
	}
	fmt.Fprintf(&b, "\n<form method=\"post\" action=\"/runs/%s/answer\"%s>\n",
		html.EscapeString(runID), enc)
	fmt.Fprintf(&b, "<input type=\"hidden\" name=\"question\" value=\"%s\">\n",
		html.EscapeString(q.ID))

	if q.Payload != "" {
		label := q.PayloadLabel
		if label == "" {
			label = "the packet to sign"
		}
		// The link is on the label's line, above the field, and both halves of
		// that are a render defect that was found by looking at the page.
		//
		// Below the field it landed between the payload and "paste what cold1
		// gave back", equidistant from each, and read as "download it instead of
		// pasting" — which is backwards: the file replaces the *copy*, not the
		// reply. And "instead" on its own named nothing, so it is "or" now. The
		// two ways of taking the packet are one choice and they belong on one
		// line, before the blob rather than after it, because which way to take
		// it is decided before it is read.
		fmt.Fprintf(&b, "<p><label for=\"payload\">%s</label> — "+
			"<a download href=\"%s\">or download it as a .psbt file</a></p>\n",
			html.EscapeString(label), payloadPath(runID, q))
		fmt.Fprintf(&b, "<textarea id=\"payload\" rows=\"6\" readonly>%s</textarea>\n",
			html.EscapeString(q.Payload))
	}
	if q.Reply != "" {
		fmt.Fprintf(&b, "<p><label for=\"reply\">%s</label></p>\n",
			html.EscapeString(q.Reply))
		b.WriteString("<textarea id=\"reply\" name=\"reply\" rows=\"6\"></textarea>\n")
		// The return leg's other half. "or" rather than "and": readAnswer
		// refuses a form carrying both, because two packets is the operator
		// having done two things and this is not the place to pick one.
		b.WriteString("<p><label for=\"upload\">or choose the signed .psbt file " +
			"the wallet wrote — binary or base64, either is read</label></p>\n")
		b.WriteString("<input id=\"upload\" name=\"upload\" type=\"file\" " +
			"accept=\".psbt,application/octet-stream,text/plain\">\n")
	}
	for _, c := range q.Choices {
		fmt.Fprintf(&b, "<button type=\"submit\" name=\"choice\" value=\"%s\">%s</button>\n",
			html.EscapeString(c.Value), html.EscapeString(c.Label))
	}
	b.WriteString("</form>\n")
	return b.String()
}

// startForm is the control on the overview screen.
func startForm() string {
	return `
<form method="post" action="/runs">
<p><label><input type="checkbox" name="probe" value="1"> shim-probe every peer
first — costs each accepted peer a pending-channel slot for about eleven minutes,
and this run then waits that out before arming</label></p>
<p><label><input type="checkbox" name="stop-before-publish" value="1"> withhold
step 9 — run the whole production path and do not publish. This is the cold
probe</label></p>
<button type="submit">Open this batch</button>
</form>
`
}

// refuseScreen is a refusal the operator will read, as a screen rather than the
// plain sentence guard.go hands a stranger.
func (s *Server) refuseScreen(w http.ResponseWriter, code int, title, text string) {
	w.WriteHeader(code)
	serve(w, screen(title, "", text))
}

// noSuchRun is /runs/{id} for an id this process is not driving, and it is
// decision 2 on that URL: it does not fall back to the journal.
//
// A fallback would make one URL mean two things — one with a live transcript, a
// pending question and an abort control, one with none of those — and the
// difference between them is the difference between a run that is happening and
// a record of one that stopped. That is precisely what an operator reads wrong
// under pressure. So this is a 404 that says where the other thing is, and the
// journal has its own path.
func (s *Server) noSuchRun(w http.ResponseWriter, id string) {
	w.WriteHeader(http.StatusNotFound)
	var b strings.Builder
	b.WriteString(screen("run", "", prose.Para(fmt.Sprintf(
		"There is no run called %q in this process, so there is nothing here to "+
			"attach to, answer or stop. A run is in the registry only while the "+
			"winthistle that started it is still running.", id))+"\n"+
		prose.Para("A run from an earlier process is in the journal instead, "+
			"which is a separate screen because it is a separate thing: a journal "+
			"row is a record read off disk, with no transcript, no question and no "+
			"control that stops anything. If this run is in there, it is at "+
			"/recover/"+id+".")))
	fmt.Fprintf(&b, "<p><a href=\"%s\">look for %s in the journal</a></p>\n",
		recoverPath(id), html.EscapeString(id))
	serve(w, b.String())
}

func noBatch() string {
	return prose.Para("There is no batch to open. This server was started " +
		"without one, and a run needs peers and amounts before it needs anything " +
		"else — start winthistle again with `serve --batch FILE`, or open the " +
		"batch from the command line with `winthistle run --batch FILE`. " +
		"`winthistle example-batch` prints one to start from.")
}

// alreadyRunning is the second concurrent run, refused.
//
// The middle paragraph used to be unconditional, and it was a false statement in
// operator copy the moment the registry grew a second Kind: two *batches* collide
// over coin selection, and a setup or a bump colliding with a batch does not. So
// the collision that could actually have happened is the one described, and
// whatIsGoing names the run in its own vocabulary.
func alreadyRunning(err error, live *Run) string {
	var b strings.Builder
	b.WriteString(prose.Para("Nothing was started: " + err.Error() + "."))
	b.WriteString("\n")

	if live != nil && live.Kind != KindBatch {
		b.WriteString(prose.Para(whatIsGoing(live) + ". One at a time, on " +
			"purpose: there is one journal and one cold wallet, and the operator a " +
			"batch would ask to fetch m devices is the same operator that is " +
			"waiting on."))
		b.WriteString("\n")
		b.WriteString(prose.Para(fmt.Sprintf("It is at /runs/%s. Nothing about it "+
			"is armed and nothing is at risk in it — when it is done, this batch "+
			"can be started here.", live.ID)))
		return b.String()
	}

	b.WriteString(prose.Para("One at a time, on purpose. There is one journal, " +
		"one cold wallet and one armed window, and the place two runs collide is " +
		"the expensive one: the second run's dress rehearsal builds a decoy over " +
		"the same coins the first run is about to spend. It would either lose coin " +
		"selection or take the inputs out from under a batch that is already " +
		"armed, with the cold wallet out and the peers waiting."))
	if live != nil {
		b.WriteString("\n")
		b.WriteString(prose.Para(fmt.Sprintf("Run %s is the one that is going. "+
			"Attach to it, or stop it there — closing a tab does not stop anything.",
			live.ID)))
	}
	return b.String()
}

// notABatch is /runs/{id}/abort for a run that is not a batch.
//
// The URL is reachable by hand and by a bookmark from an earlier run, and the
// screen behind it describes cancelling shims, abandoning pending channels and
// releasing coin locks — three things a setup has none of. So it refuses rather
// than renders, and it says what the way out actually is, which is the same
// sentence the run screen carries above the form.
func notABatch(r *Run) string {
	var b strings.Builder
	fmt.Fprintf(&b, "run %s cannot be stopped here\n\n", r.ID)
	b.WriteString(prose.Para("This control cancels a batch, and run " + r.ID +
		" is not one: it is a setup of the cold wallet " + r.About + ". The screen " +
		"behind this link would have offered to cancel funding shims, abandon " +
		"channels that reached pending and release Core's coin locks, and a setup " +
		"has none of those — it has read two RPCs and it is waiting on a person."))
	b.WriteString("\n")
	b.WriteString(prose.Para("Nothing about a setup needs stopping. What is " +
		"outstanding is a question, and every way of not answering it — the third " +
		"button, closing the tab, letting the window pass — records nothing at " +
		"all. The link below goes back to it."))
	return b.String()
}

func staleAnswer() string {
	return prose.Para("That answer is to a question this run is no longer " +
		"asking. Nothing was recorded and nothing was decided by it — a reload, a " +
		"back button or a second tab is the usual cause. The link below goes back " +
		"to the run: if it is still waiting on something, the question it is " +
		"waiting on is the one on that screen.")
}

func abortWarning(id string, already bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "stop run %s\n\n", id)

	if already {
		b.WriteString(prose.Para("This run has already been asked to stop, and it " +
			"is unwinding: cancelling the shims, abandoning what reached pending, " +
			"releasing Core's coin locks. Watch the run screen — it says what came " +
			"apart, and pressing this again would not make it faster."))
		return b.String()
	}

	b.WriteString(prose.Para("This cancels the run, and the run takes itself " +
		"apart on the way out: every funding shim cancelled, every channel that " +
		"reached pending abandoned, every coin lock Core is holding released. It " +
		"is the same thing Ctrl-C does in the terminal winthistle is serving from, " +
		"and it goes through the same code `winthistle recover` runs."))
	b.WriteString("\n")
	b.WriteString(prose.Para("What it costs is the ceremony. Nothing has been " +
		"published — that is what the armed window is defined by — so no coins are " +
		"at risk and there is no channel yet to lose. What has to be done again is " +
		"the cold-storage signing round, the peers' cooperation, and the " +
		"ten-minute windows."))
	b.WriteString("\n")
	b.WriteString(prose.Para("What it will not do is stop a transaction that is " +
		"already public. A run that reached the publish call cannot be aborted at " +
		"all, and this page refuses rather than pretending: abandoning a pending " +
		"channel whose funding transaction later confirms strands its funds with " +
		"no force-close path. `winthistle bump` is the way out of a batch that is " +
		"public and confirming too slowly."))
	return b.String()
}

func abortRefused(id string, why error) string {
	var b strings.Builder
	fmt.Fprintf(&b, "run %s cannot be stopped\n\n", id)
	b.WriteString(prose.Para(why.Error()))
	b.WriteString("\n")
	b.WriteString(prose.Para("This is the one refusal in the recovery path that " +
		"is not about tidiness. An unconfirmed funding transaction never becomes " +
		"safe to abandon on its own: its inputs stay unspent, so it stays valid " +
		"indefinitely, and eviction from a mempool does not invalidate it. A " +
		"channel abandoned now, whose funding transaction confirms afterwards, is " +
		"a channel whose funds are stranded with no force-close path."))
	b.WriteString("\n")
	b.WriteString(prose.Para("So the exit is forward rather than back. Let it " +
		"confirm, and close the channels normally if you no longer want them. If " +
		"it is confirming too slowly, `winthistle bump " + id + "` builds a CPFP " +
		"child of the batch — never a replacement of it, which would change every " +
		"funding outpoint and destroy the batch."))
	return b.String()
}
