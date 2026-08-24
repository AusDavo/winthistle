package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/AusDavo/winthistle/internal/prose"
)

// The three pre-flight reports as screens of their own: the peers, the fee rate,
// and this node's anchor reserve.
//
// They are the standalone half of the five Report() renderers the UI was missing.
// The other two are settled below rather than built, and the reason is not that
// they were left for later.
//
// # The decision: these screens re-run the check
//
// The alternative was to view what a run already produced, and it is not
// available. Nothing in this build stores a peers.Facts, a fees.Rate or a
// reserve.Finding: run.Do prints each Report() to its Out and moves on, and for a
// browser-driven run that Out is the *Run — so a run's copy of all five reports
// is transcript text, and the transcript is already served, verbatim, at
// /runs/{id}. A "view" screen would therefore be either a second rendering of the
// transcript or a new store for values nothing else keeps. Neither is worth a
// route.
//
// So these re-run, and three things make that cheap rather than a second
// pre-flight with a second pre-flight's hazards:
//
//   - They are read-only by construction. peers.Check's own comment says it
//     opens no funding stream, so it starts no clock and costs nothing to run
//     again; fees.Estimate is one estimatesmartfee; reserve.Check is a balance,
//     a lease total and three RequiredReserve calls. The one leg that would
//     change the node is peers.Check's connect, and internal/webrun strips the
//     hosts before calling it.
//   - None of them opens the run journal. That is what doctorMu is actually
//     about — two concurrent pre-flights opening the journal twice and reporting
//     one failure in two places — so these need no share of it and take none.
//     A doctor screen and a report screen loaded at once collide over nothing.
//   - They are not a second rendering. doctor calls the same three checks and
//     prints Summary() plus its own verdict-and-fix lines; these print Report(),
//     which is the text `winthistle run` prints in Phase 0. Both already exist
//     and both are already measured against the pane in their own packages, so
//     serving the second one adds a route rather than a copy.
//
// What separates them is what they are for. doctor answers "is anything wrong",
// one line per check with the command that fixes it. These answer "what are the
// figures I am about to commit to", which is a different question asked at a
// different moment — and on a batch, at the moment I-4 makes irrevocable.
//
// # Why there is no plan screen, and no settlement screen
//
// Both are decisions, and both are already answered by the transcript.
//
// A plan.Verification is produced by verifying a PSBT that has already been
// built, and building one is coin selection against the cold wallet — step 7,
// inside a run, not a pre-flight. A screen that built one to display would be
// exactly the thing the overview already refuses a second run for: a decoy over
// the coins a batch is about to spend. There is no cheaper way to reach a
// Verification, so the plan document has one route and it is the run that
// produces it.
//
// A settle.Result is worse than expensive to reach — it is unreachable from
// here. settle.Settle is called with members(armed, …), so holding one means
// holding an *arm.Armed, which is precisely the type decision 1's import ban
// stops this package from naming. It cannot come off the journal either: the
// journal has no settlement table, and it does not grow one for this, because it
// has no migrations and a table added to serve a screen is schema this build
// would then owe an operator forever.
//
// So the settlement report is not a screen, and that is the finding rather than
// the gap. It reaches a browser the same way it reaches a terminal — through the
// run that produced it — and /runs/{id} already carries it.

// reportTimeout bounds the RPCs behind these three screens.
//
// Not journalReadTimeout, which bounds a local SQLite read. This is LND and Core
// over a socket, and the case it is sized for is a node that is up but slow —
// syncing, or under load — where an answer that takes twenty seconds is still an
// answer worth having. Long enough for that, short enough that a browser is told
// something rather than left holding a socket open.
//
// The doctor screen has no such bound because it has the request's context and a
// browser that gives up ends it. These take the server's, for the reason every
// other handler here does, so the bound has to be written down instead.
const reportTimeout = 60 * time.Second

// report is the shape all three screens share: a note about anything that is
// going, then the check's own text.
//
// One function because the three differ only in what they call and in what they
// say when there is nothing to call it about. Everything an operator reads goes
// through screen() from here, so decision 3's oracle covers all three without any
// of them having to remember to.
func (s *Server) report(w http.ResponseWriter, title, path, note string,
	check func(context.Context) (string, error)) {

	if s.opts.Launcher == nil {
		s.refuseScreen(w, http.StatusNotImplemented, title, noReports(title))
		return
	}

	ctx, cancel := context.WithTimeout(s.base(), reportTimeout)
	defer cancel()

	text, err := check(ctx)
	switch {
	case errors.Is(err, ErrNoBatch):
		s.refuseScreen(w, http.StatusNotImplemented, title, noBatchFor(title))
		return
	case err != nil:
		s.refuseScreen(w, http.StatusServiceUnavailable, title,
			reportFailed(title, err))
		return
	}

	head := title + "\n\n"
	if note != "" {
		head += note + "\n"
	}
	serve(w, screen(title, path, head+text))
}

// peersScreen is peers.Facts.Report() for every peer in the batch.
func (s *Server) peersScreen(w http.ResponseWriter, r *http.Request) {
	s.report(w, "the peers", "/peers",
		notes(peersAlways(), peersNote(s.Runs.Live())),
		func(ctx context.Context) (string, error) {
			return s.opts.Launcher.Peers(ctx)
		})
}

// feesScreen is fees.Rate.Report(): the rate now, and where it came from.
func (s *Server) feesScreen(w http.ResponseWriter, r *http.Request) {
	s.report(w, "the fee rate", "/fees",
		notes(feesAlways(), feesNote(s.Runs.Live())),
		func(ctx context.Context) (string, error) {
			return s.opts.Launcher.Fees(ctx)
		})
}

// reserveScreen is reserve.Finding.Report(), about this node's own wallet.
//
// hypotheticalChannel is conditional where the other two screens' standing notes
// are not, because it is about which question was asked rather than about what
// the answer means. With a batch there is only one question.
func (s *Server) reserveScreen(w http.ResponseWriter, r *http.Request) {
	var hypothetical string
	if s.opts.Launcher != nil && s.opts.Launcher.Batch() == "" {
		hypothetical = hypotheticalChannel()
	}
	s.report(w, "the anchor reserve", "/reserve",
		notes(reserveAlways(), hypothetical, reserveNote(s.Runs.Live())),
		func(ctx context.Context) (string, error) {
			return s.opts.Launcher.Reserve(ctx)
		})
}

// notes joins the paragraphs a screen puts above its report, skipping the empty
// ones.
//
// The empty ones are the point. Each screen has a standing note and one or two
// that depend on what is going, and a blank line where a paragraph did not apply
// is how a page grows a gap nobody can account for.
func notes(paras ...string) string {
	kept := make([]string, 0, len(paras))
	for _, p := range paras {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "\n")
}

// peersAlways is what this screen is, said before the figures.
//
// The sentence that earns it is the second one. This screen does not connect to
// a peer, and a report full of "not tried" lines is otherwise read as a node that
// cannot reach anybody rather than as a diagnostic that declined to try.
//
// The third sentence is the correction a browser earned. This UI does have a
// screen that dials — the doctor screen, when `winthistle serve --connect` was
// given, because doctor.Options.Connect is the server's — so copy saying the
// connecting version lives in the terminal would have been false about the tab
// next to this one. What is true is narrower and is the decision: this screen
// never dials, whatever the server was started with, because it is the one meant
// to be reloaded.
func peersAlways() string {
	return prose.Para("These are the batch's peers, read out of this node's own " +
		"gossip graph and its own pending-channel list. Nothing was asked of a " +
		"peer and no connection was made: a peer LND is not already connected to " +
		"is reported as not tried rather than dialled, because a screen meant to " +
		"be reloaded must not be one that changes the node. That is this screen's " +
		"rule and not the whole UI's — the doctor screen will connect if this " +
		"server was started with `serve --connect`, and so will `winthistle " +
		"doctor --connect` in a terminal.")
}

// feesAlways says which transaction the report is about, when there is none.
//
// fees.Rate.Report() closes on "there is no RBF on this transaction (I-4)",
// written for Phase 0 with a transaction about to exist. Read on a screen with
// nothing going, "this transaction" names nothing — so this says which one it
// would be, and stops there.
//
// It deliberately does not restate I-4. The report says it in its closing
// paragraph and feesNote says it again over a live run, and a browser showed all
// three on one page: two of them said the same sentence four lines apart. The one
// that earns its place is feesNote's, because that one is aimed at an operator
// looking at a higher number with a batch already going.
func feesAlways() string {
	return prose.Para("Nothing is being built by looking at this. Core is asked " +
		"what it would estimate right now, and the answer changes between one " +
		"reload and the next — so this is the rate a batch started from this page " +
		"would be built at, and the transaction the report below talks about is " +
		"that one.")
}

// reserveAlways is which wallet this is about.
//
// The report says so at length when the reserve is short and barely at all when
// it is clear, and the clear case is the one an operator reads first. LND's own
// error names no wallet, and its most natural reading is that something is wrong
// with the coins just brought out of cold storage. Nothing is.
func reserveAlways() string {
	return prose.Para("This is about the LND node's own on-chain wallet and not " +
		"about the cold wallet. LND keeps a reserve it can fee-bump a force-close " +
		"with, and it re-checks that reserve inside psbt_verify — so a batch fully " +
		"funded from cold storage can still be refused at step 5 over coins the " +
		"cold wallet cannot supply. The figures below are the node's.")
}

// peersNote is what a live run does to the figures below it.
//
// Kind-aware, because what would be misread differs for each of the three and is
// absent for one. What the peer report shows that a run can move is the
// pending-channel section: it comes from this node's own PendingChannels, so a
// run that has opened channels is in it.
func peersNote(live *Run) string {
	if live == nil {
		return ""
	}
	switch live.Kind {
	case KindBatch:
		return prose.Para(fmt.Sprintf("Run %s is opening the batch right now, so "+
			"read the pending-channel figures below as including its own. A channel "+
			"that has reached chan_pending is pending on this node, and this report "+
			"cannot tell one of that run's from one left behind by something else. "+
			"Its own screen at /runs/%s is where its channels are counted as its "+
			"channels.", live.ID, live.ID))
	case KindBump:
		return prose.Para(fmt.Sprintf("A CPFP child of run %s is being built in "+
			"this process right now. That batch's funding transaction is public and "+
			"unconfirmed, which is what a child is for — so its channels are still "+
			"pending on this node and appear in the figures below. They are not a "+
			"competing open; they are the batch being accelerated.", live.About))
	}
	// A setup opens no channel, connects to no peer and reads no graph. Nothing
	// below is about it, and saying so would be a paragraph about a collision
	// that cannot happen.
	return ""
}

// feesNote is what a live run does to a rate read now.
//
// The point it exists to make is I-4's: a batch that is already going chose its
// rate when it built its transaction, and that choice is not revisable. A screen
// showing a higher number beside a run in flight, with nothing said, invites
// exactly the move I-4 forbids.
func feesNote(live *Run) string {
	if live == nil {
		return ""
	}
	switch live.Kind {
	case KindBatch:
		return prose.Para(fmt.Sprintf("Run %s is going, and it priced itself when "+
			"it built its transaction. The rate below is what Core says now, which "+
			"is a different number as often as not. It is not a rate that run can "+
			"be moved to: there is no RBF on a funding transaction (I-4), and the "+
			"remedy if a batch turns out to be underpriced is a CPFP child spending "+
			"its change — `winthistle bump`, once it is published.", live.ID))
	case KindBump:
		return prose.Para(fmt.Sprintf("A CPFP child of run %s is being priced in "+
			"this process right now, and it is asking Core the same question this "+
			"screen is. The child's own rate is not this number: it carries the "+
			"lift of the whole package in about 150 vB, so it pays many times the "+
			"package target. The two are not meant to match.", live.About))
	}
	// A setup spends nothing and pays no fee.
	return ""
}

// reserveNote is what a live run does to the reserve arithmetic.
//
// The subtlety is worth the paragraph: RequiredReserve is asked about n
// *additional* announced channels, so once a batch's channels exist LND already
// counts them and the "after the batch" figure is being added on top of a node
// that has partly acquired them.
func reserveNote(live *Run) string {
	if live == nil {
		return ""
	}
	switch live.Kind {
	case KindBatch:
		return prose.Para(fmt.Sprintf("Run %s is opening the batch right now, so "+
			"the arithmetic below is being read against a node that is partway "+
			"through acquiring it. LND is asked what it would require for n more "+
			"announced channels, and every channel of that run which has reached "+
			"chan_pending is one it already counts — so the \"after the batch\" "+
			"line adds that figure on top of some of the same channels. The reading "+
			"that stays true throughout is the available balance.", live.ID))
	case KindBump:
		return prose.Para(fmt.Sprintf("A CPFP child of run %s is being built in "+
			"this process right now. That batch's channels are already pending or "+
			"open, so LND counts them in what it requires today and the \"after the "+
			"batch\" line below is about a batch that would come after them. The "+
			"child changes nothing here: it spends the batch's change, which is the "+
			"cold wallet's rather than this node's.", live.About))
	}
	// A setup touches neither this node's wallet nor its channels.
	return ""
}

// hypotheticalChannel says which of the two questions was asked, when there is
// no batch.
//
// `winthistle doctor` makes the same substitution and says so in one line. It is
// said at more length here because this screen is the reserve and nothing else on
// it would give the figures away as a hypothetical.
//
// It says nothing about whose wallet this is, which reserveAlways has already
// said in the paragraph directly above it. A browser found the version that said
// it twice — two paragraphs opening on "this node's own on-chain wallet", four
// lines apart, which reads as a page repeating itself rather than as two facts.
func hypotheticalChannel() string {
	return prose.Para("This server was started without a batch, so the figures " +
		"below are for one announced channel rather than for anything you have " +
		"asked for. They are still worth reading: a node that cannot clear the " +
		"reserve for one announced channel will not clear it for several. What a " +
		"batch changes is the \"after\" figure, which grows with the number of " +
		"announced channels in it.")
}

// noBatchFor is a report that is about a batch, on a server that has none.
func noBatchFor(title string) string {
	var b strings.Builder
	b.WriteString(title + "\n\n")
	b.WriteString(prose.Para("There is no batch, and this report is about one: " +
		"these are the batch's peers, and with no file naming them there is nobody " +
		"to look up. Nothing was asked of the node."))
	b.WriteString("\n")
	b.WriteString(prose.Para("Start winthistle again with `serve --batch FILE` " +
		"and this screen has something to be about; `winthistle example-batch` " +
		"prints one to start from. The fee rate and the anchor reserve are on " +
		"their own screens and both answer without a batch."))
	return b.String()
}

// reportFailed is the check that could not be made.
//
// It points at doctor rather than guessing at a cure. Every one of these three
// checks is also a doctor check, and doctor is the screen that prints the command
// that fixes what it finds — so the honest thing to say about a node that is down
// is where the fix is written, not a second and shorter attempt at writing it.
func reportFailed(title string, err error) string {
	var b strings.Builder
	b.WriteString(title + "\n\n")
	b.WriteString(prose.Para("This check could not be made: " + err.Error()))
	b.WriteString("\n")
	b.WriteString(prose.Para("Nothing was changed by the attempt and nothing was " +
		"recorded — these screens read and do nothing else. The doctor screen runs " +
		"this same check alongside every other one and prints the command that " +
		"fixes whatever it finds, which is the better screen to be on while " +
		"something is down."))
	return b.String()
}

// noReports is a server with no launcher, which is a shape a test drives and not
// one an operator meets.
func noReports(title string) string {
	var b strings.Builder
	b.WriteString(title + "\n\n")
	b.WriteString(prose.Para("This server cannot make that check: it was started " +
		"without the connection details one needs. Every screen still serves, and " +
		"`winthistle doctor` in the terminal reads the same winthistle.toml and " +
		"will say what is missing."))
	return b.String()
}
