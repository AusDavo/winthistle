package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Registry is every run this process is driving, addressed by an id this
// process minted.
//
// For a batch that id is also the journal's run id; for the other Kinds it is
// nothing but this registry's key — see Kind for why that asymmetry is stated
// rather than smoothed over.
//
// This type is the guard on decision 2: a closing browser tab does not abort a
// run, so a run cannot live in a connection. It lives here, and a tab is only
// ever a view of it.
//
// Three consequences, all of them deliberate:
//
//   - Nothing in this package cancels a run's context when a response ends.
//     There is no OnDisconnect, no r.Context() plumbed into a run, and no
//     heartbeat whose absence means anything. A tab closing is indistinguishable
//     from a reload, a laptop lid and a Wi-Fi blip, and the batch it would abort
//     is one that has peers holding reservations against outpoints.
//   - The token is per server start, not per tab, so any tab that has it can
//     attach to any run. "Joinable per run rather than per tab" is what makes a
//     reconnect work at all: the second tab is not a second session.
//   - The transcript accumulates on the Run and is rendered from the beginning
//     on every attach. So an attach is idempotent and complete rather than a
//     subscription that missed what happened while nobody was looking.
//
// What ends a run is the clock — the 5:00 gate that rehearsal.Gate measures
// against, and the peers' ten minutes, which is their clock and not ours — or
// the operator's Ctrl-C on the process. Not the transport.
//
// # One run at a time
//
// Start refuses a second concurrent run, and the refusal belongs here rather
// than in the handler for the same reason the rest of decision 2 does: it is a
// property of the runs, not of the request that asked for one. There is one
// journal, one cold wallet and one armed window, and two runs would collide in
// the place it is most expensive to collide — the second run's dress rehearsal
// builds a decoy over the same coins the first run is about to spend, so it
// would either lose coin selection or take the inputs out from under a batch
// that is already armed, with the cold wallet out and n peers waiting.
//
// The refusal covers this process. Two winthistles against one journal is a
// different problem and is not solved here: SQLite serialises the writes, which
// keeps the journal honest and does nothing at all about the coins. `winthistle
// doctor` is what reports that state.
type Registry struct {
	mu   sync.Mutex
	runs map[string]*Run
}

// NewRegistry is an empty one.
func NewRegistry() *Registry {
	return &Registry{runs: map[string]*Run{}}
}

// Start puts a run in the registry, or refuses because one is already going.
//
// cancel is what the explicit abort control calls, and it must cancel the
// context the run was started with — not a request's. Nothing else in this
// package holds it.
func (reg *Registry) Start(id string, kind Kind, about string,
	cancel func()) (*Run, error) {

	reg.mu.Lock()
	defer reg.mu.Unlock()

	for _, r := range reg.runs {
		if _, finished, _ := r.State(); !finished {
			return nil, fmt.Errorf("%w: run %s is still going", ErrRunInFlight, r.ID)
		}
	}
	if _, taken := reg.runs[id]; taken {
		return nil, fmt.Errorf("run %s is already in this registry", id)
	}

	r := reg.addKind(id, kind, about)
	r.cancel = cancel
	return r, nil
}

// Kind is which of the things a run in this registry is doing.
//
// The registry exists because internal/webrun's adapters ask their questions
// through a *Run — that is the only way a browser answers a blocking callback
// seam — and three of this build's commands have such a seam. So a setup goes in
// here beside a batch, and so does a bump. The difference has to be readable from
// the run, because almost every sentence a screen says about a batch is false
// about the other two: a setup opens no channel at all, a bump's channels are
// already funded and no peer is waiting on its round, and neither has an armed
// window to be taken apart.
//
// KindBatch is the zero value, which is right for the constructor tests use and
// is not a default anything in production leans on: Start takes the kind, so
// every production run states which it is.
//
// This is deliberately not the journal's vocabulary. A batch run's id is the
// journal's run id; a setup writes a setups row keyed by the wallet and no run
// row at all; a bump writes bump rows under the id of the batch it is
// accelerating rather than under its own. So a setup's or a bump's id is this
// process's alone, /recover/{that id} has nothing, and the screens say so instead
// of implying a row that will never be written.
//
// A bump deliberately does not reuse the batch's run id as its registry key, even
// though that is the id it is about. It would put a bump on /runs/{the batch's
// id} — the URL that means "the batch is going in this process" — and make the
// journal screens say a run is live when what is live is a second cold-wallet
// session about a transaction that is already public. About is where that id
// goes.
type Kind int

const (
	// KindBatch is `winthistle run`: the batch, its dress rehearsal, its armed
	// window and its abort path. The only kind with an abort control, because it
	// is the only kind with something to take apart.
	KindBatch Kind = iota

	// KindSetup is `winthistle setup`'s resume path: derive the cold wallet's
	// addresses and ask a human whether they match. Nothing is created, nothing
	// is spent, and the way out is the third button.
	KindSetup

	// KindBump is `winthistle bump`: the CPFP child of a batch that is already
	// public. Its own signing round, and its own way out — a device that cannot
	// sign fails the round, which releases the coin lock on the batch's change.
	KindBump
)

// ErrRunInFlight is the second concurrent run, refused. See "One run at a time".
var ErrRunInFlight = errors.New("one run at a time")

// add is the unguarded constructor for a batch run. Start is the only
// production route to it: the guard is a property of the registry, so it lives
// on the way in.
func (reg *Registry) add(id string) *Run { return reg.addKind(id, KindBatch, "") }

// addKind is add for the other things this registry drives. Kind and About are
// filled before the run is in the map, because List reads a run the moment it is
// there and neither field is ever written again.
func (reg *Registry) addKind(id string, kind Kind, about string) *Run {
	r := &Run{ID: id, Kind: kind, About: about, Started: time.Now()}
	reg.runs[id] = r
	return r
}

// Live is the run that is still going, or nil. The index needs it to decide
// whether to offer the control that starts one.
func (reg *Registry) Live() *Run {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	for _, r := range reg.runs {
		if _, finished, _ := r.State(); !finished {
			return r
		}
	}
	return nil
}

// Get is the attach: a run id, and whatever that run has said so far.
func (reg *Registry) Get(id string) *Run {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	return reg.runs[id]
}

// List is every run, newest first, for the index.
func (reg *Registry) List() []*Run {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	out := make([]*Run, 0, len(reg.runs))
	for _, r := range reg.runs {
		out = append(out, r)
	}
	// Newest first: a run started ten minutes ago is history and the one started
	// a moment ago is what the operator came back for.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Started.After(out[j-1].Started); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// Run is one run's screen: what it has said, and whether it is still saying it.
//
// It is an io.Writer, which is what run.Deps.Out wants. That is the whole seam
// between the composition and the UI for everything the run only *reports*; the
// four things a run has to *ask* go through Ask, in ask.go, and are turned into
// the concrete seam types by internal/webrun.
type Run struct {
	ID      string
	Started time.Time

	// Kind is which of the things this run is doing, and About is what makes it
	// identifiable: the cold wallet a setup is questioning, or the journalled run
	// a bump is accelerating. Empty for a batch, whose own id is already the
	// journal's.
	//
	// Both are set once, before this run is in the registry's map, and never
	// written again — so a screen may read them without the mutex.
	Kind  Kind
	About string

	mu       sync.Mutex
	said     strings.Builder
	finished bool
	err      error

	// pending and replies are the seam: one question at a time, and the channel
	// the answer arrives on. See ask.go.
	pending *Question
	replies chan Answer
	seq     int

	// cancel ends the run. It cancels the context the run was started with,
	// which is the server's and never a request's — decision 2 — and it is
	// called by exactly one thing: the explicit abort control.
	cancel func()

	// abortedAt is when that control was used, so the screen can say the run is
	// unwinding rather than leave the operator pressing the button again.
	abortedAt time.Time
}

// Abort ends the run the way Ctrl-C ends a CLI run: it cancels the context, and
// the run unwinds through the abort path — cancel the shims, abandon what
// reached pending, release Core's locks.
//
// This is the control decision 2 makes mandatory rather than optional. If a
// closing tab does not abort, the operator needs a thing that does, and it has
// to be a button rather than a disconnection. What it must not be offered for is
// a run that reached the publish call; that check is not this method's, because
// it is the journal's — see Launcher.AbortRefusal.
func (r *Run) Abort() {
	r.mu.Lock()
	if r.abortedAt.IsZero() {
		r.abortedAt = time.Now()
	}
	cancel := r.cancel
	r.mu.Unlock()

	if cancel != nil {
		cancel()
	}
}

// Aborting reports whether the abort control has been used on this run.
func (r *Run) Aborting() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.abortedAt.IsZero()
}

// NewRunID is the journal's key for a run: sortable, and unique even if two
// start in the same second.
//
// Both front doors use this one function, so a run started from the browser and
// a run started from the command line sort together in `winthistle recover`.
func NewRunID() (string, error) {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("making a run id: %w", err)
	}
	return time.Now().UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b[:]), nil
}

// Launcher is what a POST to /runs hands the work to.
//
// An interface, and a narrow one, because internal/server may not name the
// run's types at all: decision 1's import ban keeps internal/arm, internal/bump
// and internal/journal out of this package, and a Launcher that took a
// run.Options would drag internal/run — and with it a reachable *arm.Armed —
// straight back in. internal/webrun implements this.
type Launcher interface {
	// Batch is what a run started from here would open, as text for the screen.
	// Empty means there is nothing to start, and the control is not offered.
	Batch() string

	// Start drives one run to completion and blocks until it is done. The
	// context is the server's, never a request's.
	Start(ctx context.Context, r *Run, req StartRequest) error

	// Wallet is the cold wallet's name, from winthistle.toml's [bitcoind]
	// wallet. Empty means this server cannot run a setup, and the screen says
	// so rather than offering a control that would fail.
	//
	// It is the launcher's rather than read from this package's own config for
	// the reason Batch is: one thing owns what a run would be against, so a
	// screen cannot name a wallet a setup would not question.
	Wallet() string

	// StartSetup drives `winthistle setup`'s resume path and blocks until it is
	// done: read the descriptors the wallet holds, derive their addresses, and
	// ask a human whether they match.
	//
	// The resume path only, and there is no parameter that would change that.
	// Installing descriptors needs a file, a file needs a path, and a path
	// posted from a browser is a browser choosing which file this process reads
	// — so the install stays on the command line, where the operator names the
	// file themselves. What a browser is for is the question, which is the half
	// that has to be asked twice.
	//
	// The context is the server's, never a request's, for the reason Start's is.
	// Nothing here is armed, but a comparison the operator is in the middle of
	// making must not be cancelled by a reload.
	StartSetup(ctx context.Context, r *Run) error

	// StartBump drives `winthistle bump` and blocks until it is done: find the
	// parent in Core's mempool, price the lift, build and verify the child, ask,
	// and then run a second cold-wallet signing round over it.
	//
	// It gives bump.Approve its caller. That seam is asked once, after the child
	// is built and verified and before it goes to any device, and what it
	// protects is the operator's evening rather than the batch — nothing after
	// it can lose one. Declining releases the coin lock and leaves the batch
	// exactly as it was.
	//
	// This package cannot name a *bump.Signed, which is the second of the two
	// production call sites of WalletKit.PublishTransaction and the reason the
	// import ban exists. What it can do is start the sequence and answer its
	// questions.
	//
	// The context is the server's, never a request's: the round in here is a
	// cold-wallet round with the same devices a batch uses, and a reload must not
	// end one.
	StartBump(ctx context.Context, r *Run, req BumpRequest) error

	// AbortRefusal says why this run must not be aborted, or nil.
	//
	// It exists so the answer comes from the journal rather than from a copy of
	// the journal's rules kept here: a run that reached the publish call is
	// refused with journal.ErrMayBePublished, which is the same refusal
	// run.RecoverOne makes, because abandoning a pending channel whose funding
	// transaction later confirms strands its funds with no force-close path.
	//
	// Both controls that could act on a run ask it, and the read-only recovery
	// screen asks it too — not to decide whether to act, since it never acts,
	// but to decide whether to point the operator at a command that would.
	AbortRefusal(ctx context.Context, runID string) error

	// Unfinished is the journal's recovery screen: the runs that stopped, and
	// then the CPFP children left half-done, as one piece of text — plus the ids
	// of the runs in it.
	//
	// Text and not rows, for decision 3's reason. prose.RecoveryList and
	// prose.Recovery are the highest-stakes copy in the product and they have
	// the overrun tests over them; a data type this package could render would
	// be a second rendering of that copy, measured by nothing, drifting from the
	// one `winthistle recover` prints. The ids are the exception and they are
	// not copy: they are what a link needs, and they are the same strings
	// PathValue already hands this package.
	//
	// The pairing of runs with children is the launcher's, and behind it
	// run.Unfinished is the only exported way to get either — a screen that
	// listed the runs and not the children would tell somebody their node is
	// clean while a coin of theirs is locked.
	Unfinished(ctx context.Context) (text string, ids []string, err error)

	// Progress is a running batch's own state as text: one row per channel, how many
	// receipts are in, whether the funding transaction is still held, and what
	// is left of the peers' window.
	//
	// It is here rather than on this package's Run for the reason AbortRefusal
	// is: the receipt count is the number I-1 turns on, and the journal is what
	// decides it. A Run that reported its own progress would be a second copy of
	// the arming rule sitting in the one place a mistake reads as reassurance —
	// so the attach screen asks, and the answer comes from the same rows
	// MarkPending counts.
	//
	// ErrNoJournalledRun means the run has journalled nothing yet, which is an
	// ordinary state and not an error: its streams have not opened. The screen
	// shows its transcript alone rather than saying the run is missing.
	Progress(ctx context.Context, runID string) (string, error)

	// Journalled is one journalled run's recovery screen, as text, and
	// ErrNoJournalledRun when the journal has no such run.
	//
	// Read-only, like Unfinished, and for the same reason nothing beside it
	// aborts: see recover.go.
	Journalled(ctx context.Context, runID string) (string, error)

	// The three pre-flight reports that can be reached without a run, as text.
	//
	// They are here for the reason Batch is: the launcher is the thing holding
	// the batch file and the connection details, and a second reader of either
	// would be a second thing to keep in step. They return the same Report()
	// strings `winthistle run` prints in Phase 0, so decision 3's oracle covers
	// them without a second rendering existing anywhere.
	//
	// Each dials only what its own check needs and closes it again — see
	// internal/webrun. None of the three opens the run journal, which is what
	// makes them safe to serve beside doctor rather than behind doctorMu: the
	// thing doctorMu serialises is a second journal handle, and these have none.
	//
	// reports.go is where the decision behind them is written down: they re-run
	// the check rather than replaying what a run produced, because nothing in
	// this build stores a peers.Facts, a fees.Rate or a reserve.Finding — a run's
	// copy of all three is transcript text, and it is already served.

	// Peers is peers.Facts.Report() for every channel in the batch: the local
	// graph's evidence about each peer, and what this node already has pending
	// with it.
	//
	// It never connects to a peer. peers.Check will dial one whose Want carries a
	// host, and a screen that did that would be a diagnostic changing the node —
	// so the hosts are stripped before it is called, exactly as `winthistle
	// doctor` strips them without --connect.
	//
	// ErrNoBatch when there is no batch: the peers are the batch's, and there is
	// nothing to be about.
	Peers(ctx context.Context) (string, error)

	// Fees is fees.Rate.Report(): the rate a batch built right now would pay, and
	// which of Core's estimate, the configured floor and this node's relay floor
	// it came from.
	//
	// The one of the three that needs no batch and no LND. It is Core alone, and
	// it answers on a node whose LND is down.
	Fees(ctx context.Context) (string, error)

	// Reserve is reserve.Finding.Report(): whether this node's own on-chain
	// wallet clears the anchor reserve psbt_verify will demand, and whether it
	// still clears it once the batch's channels exist.
	//
	// With no batch it answers for one announced channel rather than refusing,
	// which is what doctor does with no --batch. That is a useful answer — the
	// reserve is a fact about this node, not about the batch — and the screen
	// says which question was asked.
	Reserve(ctx context.Context) (string, error)
}

// ErrNoBatch is a report that is about a batch, asked of a server that has none.
//
// A sentinel rather than a bare error for ErrNoJournalledRun's reason: "there is
// no batch" is not a failure to look, and the screen that hears it says what to
// do rather than reporting that something went wrong.
var ErrNoBatch = errors.New("this server was started without a batch")

// ErrNoJournalledRun is the journal having no such run, as this package is
// allowed to hear it.
//
// journal.ErrNoRun is the real sentinel and internal/server may not name it —
// the import ban — so the launcher translates. A sentinel rather than a bare
// error because the difference between "no such run" and "the journal could not
// be read" is the difference between a 404 and a page that must not pretend it
// looked.
var ErrNoJournalledRun = errors.New("the journal has no such run")

// BumpRequest is what the operator chose on the bump screen.
//
// RunID is the journalled run whose batch is being accelerated, and it is a run
// id rather than a txid for the reason `winthistle bump` takes one: the journal is
// what says a transaction was actually published, what its change output was, and
// whether an earlier child of it is already holding a coin lock.
type BumpRequest struct {
	RunID string

	// TargetSatPerVB is the rate to lift the parent and child to together, not
	// the child's own rate. Zero means ask Core, through internal/fees — never a
	// fee API, which would be handed the size of what is being built and the
	// moment it is being built.
	TargetSatPerVB float64

	// BuildOnly stops after the child is built and verified, asking no device for
	// anything. The bump's dry run: the arithmetic is the part that can be wrong,
	// and checking it reveals whether a cold wallet is worth bringing out.
	BuildOnly bool
}

// StartRequest is what the operator chose on the form. The batch itself is the
// launcher's, from `winthistle serve --batch`.
type StartRequest struct {
	// Probe shim-probes every peer first, and then waits out the pending-channel
	// slot that costs.
	Probe bool

	// StopBeforePublish is the cold probe: the whole production path with step 9
	// withheld.
	StopBeforePublish bool
}

// Write appends to the transcript. It never fails: a run must not stop because
// nobody is looking at it.
func (r *Run) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.said.Write(p)
	return len(p), nil
}

// Finish records that the run is over, and how.
func (r *Run) Finish(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.finished = true
	r.err = err
}

// State is the transcript and what became of the run, read together so a screen
// cannot render a transcript from before the run ended beside a verdict from
// after it.
func (r *Run) State() (transcript string, finished bool, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.said.String(), r.finished, r.err
}
