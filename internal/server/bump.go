package server

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/AusDavo/winthistle/internal/prose"
)

// The bump screen, and the last of the four callback seams to get a POST.
//
// bump.Approve has been built, unit tested and unreachable since the seams
// landed. With this it is called, and there is no written-but-uncalled code left
// in this UI.
//
// The screen itself is thin, and deliberately: everything an operator needs to
// decide with — the parent's fee rate out of Core's mempool entry, what the lift
// costs, the child's arithmetic and the verifier's two passes over it — is
// produced by the bump itself and arrives in the transcript. So this page is the
// two knobs and the sentence about what a bump is; the decision is asked on the
// run screen, after the numbers exist.
//
// # It hangs off the journal screen rather than living under it
//
// /recover is read-only and there is no POST under it, which recover.go argues
// at length: an abort of a journalled run asks abort.Confirmation once per
// channel, and a browser answers those through a Run that a journalled run does
// not have. A bump is not that. It is a new thing this process drives, with a
// Run of its own and a transcript of its own, so it gets its own path — and
// /recover/{id} links to it, which keeps that screen's method patterns exactly as
// they were.
//
// The link appears there only for a run the journal refuses to abort, and that
// is not a coincidence: journal.Run.AbortTarget refuses a run in publishing or
// published, which is the same condition that makes a bump possible at all. A
// batch that never reached the publish call has no parent in any mempool and
// nothing to accelerate.
//
// # What it cannot do
//
// Replace the parent. I-4, and it is not this package's promise to keep — there
// is no code path in this repository that replaces a funding transaction, and
// this one could not name a *bump.Signed if it wanted to. What reaches the
// network is bump.Publish, behind the same import ban that keeps arm.Publish
// away from a handler.

// bumpScreen is what a bump would do, and the control that starts it.
func (s *Server) bumpScreen(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.opts.Launcher == nil {
		s.refuseScreen(w, http.StatusNotImplemented, "accelerate run "+id, noBump())
		return
	}

	live := s.Runs.Live()
	var b strings.Builder
	b.WriteString(screen("accelerate run "+id, "", bumpIntro(id)+"\n"+bumpBusy(live)))
	if live == nil {
		b.WriteString(bumpForm(id))
	}
	fmt.Fprintf(&b, "<p><a href=\"%s\">back to run %s in the journal</a></p>\n",
		recoverPath(id), html.EscapeString(id))
	serve(w, b.String())
}

// startBump is the POST that gives bump.Approve its first caller.
func (s *Server) startBump(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.opts.Launcher == nil {
		s.refuseScreen(w, http.StatusNotImplemented, "accelerate run "+id, noBump())
		return
	}
	if err := r.ParseForm(); err != nil {
		s.refuseScreen(w, http.StatusBadRequest, "no child was built",
			prose.Para("The form could not be read: "+err.Error()))
		return
	}

	target, why := parseTarget(r.PostForm.Get("target"))
	if why != "" {
		s.refuseScreen(w, http.StatusBadRequest, "no child was built",
			prose.Para(why))
		return
	}
	req := BumpRequest{
		RunID:          id,
		TargetSatPerVB: target,
		BuildOnly:      r.PostForm.Get("build-only") != "",
	}

	sessionID, err := NewRunID()
	if err != nil {
		s.refuseScreen(w, http.StatusInternalServerError, "no child was built",
			prose.Para(err.Error()))
		return
	}

	// Decision 2. The round in here is a cold-wallet round with the same devices
	// a batch uses, and a reload must not end one.
	ctx, cancel := context.WithCancel(s.base())

	sess, err := s.Runs.Start(sessionID, KindBump, id, cancel)
	if err != nil {
		cancel()
		s.refuseScreen(w, http.StatusConflict, "no child was built",
			bumpRefused(err, s.Runs.Live()))
		return
	}

	go func() {
		defer cancel()
		sess.Finish(s.opts.Launcher.StartBump(ctx, sess, req))
	}()

	http.Redirect(w, r, runPath(sessionID), http.StatusSeeOther)
}

// parseTarget reads the one number on the form, or says why it will not.
//
// Empty is legal and is not a default: it means ask Core, through the same
// estimator the batch used. A number that cannot be read is refused rather than
// treated as empty, because those two mean opposite things — one is "you choose"
// and the other is a typo in the only figure that decides what this costs.
func parseTarget(raw string) (float64, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, ""
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, "That target rate could not be read as a number: " +
			strconv.Quote(raw) + ". Nothing was built. Leave the field empty to " +
			"ask Core instead — a bump is the one place where guessing is worse " +
			"than asking, because I-4 means the batch gets no second attempt."
	}
	if v <= 0 {
		return 0, "A target rate has to be above zero, and " + raw + " is not. " +
			"Nothing was built. Leave the field empty to ask Core through the same " +
			"estimator the batch used."
	}
	return v, ""
}

func bumpForm(runID string) string {
	return fmt.Sprintf(`
<form method="post" action="%s">
<p><label for="target">the sat/vB to lift the batch and its child to together —
not the child's own rate. Leave it empty to ask Core, through the same estimator
the batch used</label></p>
<input id="target" name="target" type="text" inputmode="decimal" size="12">
<p><label><input type="checkbox" name="build-only" value="1"> build and verify
the child and ask no device for anything. The arithmetic is the part that can be
wrong, and checking it costs nothing</label></p>
<button type="submit">Build the child</button>
</form>
`, bumpPath(runID))
}

// bumpPath is one run's bump screen, escaped for both the path and the attribute
// it goes in. Same reason as recoverPath: the id came off a URL.
func bumpPath(runID string) string {
	return html.EscapeString("/bump/" + url.PathEscape(runID))
}

func bumpIntro(runID string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "accelerate run %s\n\n", runID)
	b.WriteString(prose.Para("A batch that went out too cheap cannot be replaced. " +
		"Every peer holds a commitment signature against its outpoints, so " +
		"replacing the funding transaction would move all of them and destroy the " +
		"batch — that is I-4, and there is no code path in this build that does " +
		"it. What there is instead is this: a child transaction that spends the " +
		"batch's change output and pays enough fee to lift the pair of them " +
		"together."))
	b.WriteString("\n")
	b.WriteString(prose.Para("It is a second cold-wallet session, and that is the " +
		"whole cost of it. The change output belongs to cold storage like any " +
		"other output, so accelerating the batch needs every device in turn, each " +
		"returning a partial signature. There is no shortcut that does not amount " +
		"to a hot key able to spend the batch's change."))
	b.WriteString("\n")
	b.WriteString(prose.Para("Nothing after this point can lose the batch. Its " +
		"funding transaction is already public, every channel in it reached " +
		"pending before that, and no channel depends on the change output. A " +
		"child that is never signed leaves the batch exactly as it is."))
	b.WriteString("\n")
	b.WriteString(prose.Para("Pressing the button below does not touch a device. " +
		"It finds the parent in Core's mempool — its real size and fee, and its " +
		"ancestors', which is the figure the arithmetic needs — prices the lift, " +
		"builds the child, verifies it twice, and shows the working. Only then " +
		"does it ask, and the question is on the run screen with the numbers " +
		"above it."))
	b.WriteString("\n")
	b.WriteString(prose.Para("Two things it will refuse, and both are cheap to " +
		"find out. If the parent is not in a mempool, a child of it would be an " +
		"orphan and it says so rather than pretending. If the parent already pays " +
		"the target rate, there is no child to build."))
	return b.String()
}

// bumpBusy is why the control is absent, or nothing.
func bumpBusy(live *Run) string {
	if live == nil {
		return ""
	}
	return prose.Para(fmt.Sprintf("%s, so there is nothing to start here yet — "+
		"one at a time, because there is one journal and one cold wallet. It is "+
		"at /runs/%s, which is the screen with its transcript and whatever it is "+
		"waiting on.", whatIsGoing(live), live.ID))
}

func bumpRefused(err error, live *Run) string {
	var b strings.Builder
	b.WriteString("no child was built\n\n")
	b.WriteString(prose.Para("Nothing was started: " + err.Error() + "."))
	if live != nil {
		b.WriteString("\n")
		b.WriteString(bumpBusy(live))
	}
	b.WriteString("\n")
	b.WriteString(prose.Para("The batch is untouched by that. Its funding " +
		"transaction is where it was, no child of it has been built, and no coin " +
		"lock was taken."))
	return b.String()
}

func noBump() string {
	var b strings.Builder
	b.WriteString("accelerate a batch\n\n")
	b.WriteString(prose.Para("This server cannot build a CPFP child: it was " +
		"started without the launcher that owns the connections and the journal, " +
		"which is the shape a test drives and not one `winthistle serve` " +
		"produces."))
	b.WriteString("\n")
	b.WriteString(prose.Para("`winthistle bump RUN` in the terminal does the same " +
		"thing and needs nothing from this process. `winthistle recover` lists " +
		"the runs it will take."))
	return b.String()
}
