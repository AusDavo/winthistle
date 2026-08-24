package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/AusDavo/winthistle/internal/prose"
)

// The setup screen: the one question this program cannot answer for itself, and
// the route that finally gives setup.Ask a caller.
//
// internal/webrun.Ask has been built, unit tested and unreachable since the
// callback seams landed. This file is what ends that, and it is deliberately the
// thinnest thing that can: a screen that says what the comparison is, a POST
// that starts the resume path, and then the attach screen — the same one a batch
// uses — where the addresses appear in the transcript and the three-way question
// appears as a form.
//
// # It is the resume path, and that is a decision rather than a subset
//
// `winthistle setup --descriptors FILE` installs. This screen never does, and
// there is no field on it that could. A descriptor file needs a path; a path
// posted from a browser is a browser choosing which file this process opens and
// imports; and an import blocks for the whole rescan, which is minutes to hours
// on mainnet and would hold the one-at-a-time slot for all of it.
//
// None of that is what a browser is good for. What it is good for is the
// question — `coldwallet.Read` and `DeriveCheck`, which have no side effect at
// all — and that is the half of the command built to be asked twice. So the
// install stays in the terminal, where the operator names their own file, and
// this screen is the part they come back to with a hardware wallet in hand.
//
// # The guard behind the question is not this file's, and nothing here adds to it
//
// Four layers keep NotAnswered out of the setups table: the form is three-way
// with a button whose wording is the answer, internal/webrun's adapter maps
// every other outcome to NotAnswered, setup.Do returns before RecordSetup on
// NotAnswered and is the only writer of that table, and this package cannot
// import internal/journal at all. This file adds a route. It does not add a
// fifth layer, and it must not: a check here would be a second place a verdict
// is decided, which is the thing all four layers exist to prevent.
//
// # No abort control, and the reason is that it already has a better one
//
// A batch run's abort control is mandatory because a closing tab does not stop a
// run and a partially-armed batch has n shims, n pending channels and Core's
// coin locks behind it. A setup has none of that. It has read two RPCs and it is
// waiting on a person, and the way out is on the screen already: "I have not
// compared them yet" records nothing, which is the same verdict letting the
// window lapse produces. Cancelling the context would be a third way to reach
// the same nothing, spelled as though it undid something.

// setupScreen is what a setup would ask about, and the control that asks it.
func (s *Server) setupScreen(w http.ResponseWriter, r *http.Request) {
	wallet, ok := s.setupTarget(w)
	if !ok {
		return
	}

	live := s.Runs.Live()
	var b strings.Builder
	b.WriteString(screen("the cold wallet's setup", "/setup",
		setupIntro(wallet)+"\n"+setupBusy(live)))
	if live == nil {
		b.WriteString(setupForm())
	}
	serve(w, b.String())
}

// startSetup is the POST that gives setup.Ask its first caller.
func (s *Server) startSetup(w http.ResponseWriter, r *http.Request) {
	wallet, ok := s.setupTarget(w)
	if !ok {
		return
	}

	id, err := NewRunID()
	if err != nil {
		s.refuseScreen(w, http.StatusInternalServerError, "setup not started",
			prose.Para(err.Error()))
		return
	}

	// Decision 2, the same line startRun has. The context is the server's with a
	// cancel of its own, and nothing derived from this request reaches it: an
	// operator walking to a safe with the tab open is the ordinary case, and a
	// response that ended while they were gone must not end the question.
	ctx, cancel := context.WithCancel(s.base())

	sess, err := s.Runs.Start(id, KindSetup, wallet, cancel)
	if err != nil {
		cancel()
		s.refuseScreen(w, http.StatusConflict, "setup not started",
			setupRefused(err, s.Runs.Live()))
		return
	}

	go func() {
		defer cancel()
		sess.Finish(s.opts.Launcher.StartSetup(ctx, sess))
	}()

	http.Redirect(w, r, runPath(id), http.StatusSeeOther)
}

// setupTarget is the cold wallet both handlers need, or the refusal neither can
// get past.
//
// One function because the two refusals have to be the same on the GET and the
// POST. A screen that offered the control and a handler that then said the
// server cannot run a setup would teach an operator to press it twice.
func (s *Server) setupTarget(w http.ResponseWriter) (string, bool) {
	if s.opts.Launcher == nil {
		s.refuseScreen(w, http.StatusNotImplemented, "the cold wallet's setup",
			noSetup())
		return "", false
	}
	wallet := s.opts.Launcher.Wallet()
	if wallet == "" {
		s.refuseScreen(w, http.StatusNotImplemented, "the cold wallet's setup",
			noSetup())
		return "", false
	}
	return wallet, true
}

// setupForm is the control. One button and no fields: see the file comment on
// why there is no descriptor path on it.
func setupForm() string {
	return `
<form method="post" action="/setup">
<button type="submit">Derive the addresses and ask</button>
</form>
`
}

func setupIntro(wallet string) string {
	var b strings.Builder
	b.WriteString("the cold wallet's setup\n\n")
	b.WriteString(prose.Para("This asks the one question this program cannot " +
		"answer for itself. Nothing it can check separates a correct " +
		"cold-storage descriptor from a plausible wrong one: Core parses both, " +
		"the import succeeds for both, and the balance does not separate them " +
		"either — on this build's own 2-of-2 harness, a wallet built with multi() " +
		"where you wanted sortedmulti() found six of the cold wallet's eight " +
		"BTC, because sortedmulti sorts the derived keys and the two descriptors " +
		"therefore agree at every index where the keys already happen to be in " +
		"ascending order. A plausible partial balance is a far better disguise " +
		"than zero."))
	b.WriteString("\n")
	b.WriteString(prose.Para("So the verdict is a comparison a human made. " +
		"This reads the descriptors the wallet " + wallet + " actually holds, " +
		"derives the first few addresses of each branch from them, and puts them " +
		"in front of you to check against the device they are supposed to have " +
		"come from."))
	b.WriteString("\n")
	b.WriteString(prose.Para("Three answers, and the third one is not a cancel. " +
		"\"I have not compared them yet\" records nothing, and it is the right " +
		"answer until you have been to the device — as is closing this tab and " +
		"coming back. Nothing is created and nothing is spent by any of the " +
		"three: deriveaddresses has no side effect, so the same addresses are " +
		"here tomorrow."))
	b.WriteString("\n")
	b.WriteString(prose.Para("What this screen will not do is install " +
		"descriptors. That needs a file, and a file needs a path — and a path " +
		"typed into a browser is a browser choosing which file this process " +
		"reads and imports. `winthistle setup --descriptors FILE` in the " +
		"terminal is the install, where you name the file yourself; it is also " +
		"the slow half, because importdescriptors blocks for the whole rescan. " +
		"This is the half worth asking twice."))
	return b.String()
}

// setupBusy is why the control is absent, or nothing.
//
// Absent rather than disabled, like the batch's: a control that is offered and
// then refused teaches an operator to press it twice.
func setupBusy(live *Run) string {
	if live == nil {
		return ""
	}
	// One paragraph, and it points somewhere. The second one used to say this
	// question has "no clock of its own worth racing", which is a sentence about
	// the setup you would start and was being read next to one that is already
	// running under a fifteen-minute window — and it repeated the intro's own
	// closing line. A refusal that names something the operator cannot reach is
	// the dead-end defect; this one carries the path.
	return prose.Para(fmt.Sprintf("%s, so there is nothing to start here yet — "+
		"one at a time, because there is one journal and one cold wallet. It is at "+
		"/runs/%s, which is the screen with its transcript and whatever it is "+
		"waiting on.", whatIsGoing(live), live.ID))
}

func setupRefused(err error, live *Run) string {
	var b strings.Builder
	b.WriteString("setup not started\n\n")
	b.WriteString(prose.Para("Nothing was started: " + err.Error() + "."))
	if live != nil {
		b.WriteString("\n")
		b.WriteString(setupBusy(live))
	}
	return b.String()
}

func noSetup() string {
	var b strings.Builder
	b.WriteString("the cold wallet's setup\n\n")
	b.WriteString(prose.Para("This server cannot run a setup: it does not know " +
		"which wallet to question. The name comes from winthistle.toml's " +
		"[bitcoind] wallet rather than from a field on this page, because the " +
		"wallet a setup questions has to be the wallet a batch will spend from, " +
		"and a second place to name it is a second thing to get wrong."))
	b.WriteString("\n")
	b.WriteString(prose.Para("`winthistle setup` in the terminal reads the same " +
		"file and will say the same thing in more detail. It needs nothing from " +
		"this process, and nothing from LND either — setting up the cold wallet " +
		"has no channel, no peer and no macaroon in it, so it works on a machine " +
		"where the node is down."))
	return b.String()
}

// whatIsGoing is one clause naming the live run, in the vocabulary of whichever
// of the three things it is.
//
// It exists because the sentence that used to be written here — the second run's
// dress rehearsal building a decoy over the same coins — is true of a batch and
// false of the other two. A refusal that explained a collision that could not
// have happened would be a false statement in operator copy on the one screen
// somebody reads while they are already lost.
func whatIsGoing(live *Run) string {
	switch live.Kind {
	case KindSetup:
		return fmt.Sprintf("A setup of the cold wallet %s is going in this "+
			"process right now, as run %s", live.About, live.ID)
	default:
		return fmt.Sprintf("Run %s is opening the batch right now", live.ID)
	}
}
