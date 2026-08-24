// Package webrun is the browser's side of internal/run's four callback seams,
// and the goroutine that drives run.Do behind them.
//
// It exists so that internal/server can answer a run's questions without being
// able to name the run's types. Decision 1's import ban keeps internal/arm,
// internal/bump and internal/journal out of internal/server — the two packages
// that reach the network and the one that owns the record — and a server that
// took a run.Options would have dragged internal/run in, and with it a
// reachable *arm.Armed through run.Result. So the boundary is a narrow
// interface, server.Launcher, and this is what implements it.
//
// # The four seams, and what an abandoned browser means to each
//
// A tab that closes mid-question is not detectable (decision 2), so every seam
// here is bounded by a deadline rather than by a disconnection. server.Run.Ask
// returns ErrUnanswered when it passes, and each adapter maps that to the answer
// a nil seam would have given:
//
//   - setup.Ask — NotAnswered. The whole reason that type has three values is
//     that a comparison nobody made must never be recorded as a verdict, and an
//     abandoned web form is exactly a comparison nobody made. This is the
//     highest-stakes of the four, and it has two guards below it: the mapping
//     here, which is total — every path that is not an explicit "yes" or "no"
//     returns NotAnswered — and setup.Do, which returns before RecordSetup on
//     NotAnswered and is the only writer of the setups table. internal/server
//     cannot reach that table at all: it may not import internal/journal.
//     Its clock is the one exception to the section below: see
//     AddressCheckWindow.
//   - abort.Confirmation — false, which is what a nil Confirmation means: never
//     escalate to i_know_what_i_am_doing. It costs an abort that has to be
//     finished by hand, which is the cheap side of that trade.
//   - bump.Approve — false, which releases the child's coin lock and leaves the
//     batch exactly as it was.
//   - rehearsal.Signer — an error, which fails the round. For the rehearsal
//     round that is the same verdict rehearsal.Gate would have reached; for the
//     batch round it fails the armed window, which unwinds through the abort
//     path with nothing published.
//
// # The clock is the 5:00 gate, and it is the round's
//
// For every seam a batch has: limits.abort_after_signing_seconds, and not the
// peers' ten minutes. The difference is not stylistic:
//
//   - the gate is ours. We set it, rehearsal.Gate measures against it, and it is
//     therefore a number this code may enforce.
//   - the peers' ten minutes are theirs. LND's pruneZombieReservations skips
//     PSBT reservations, so our node never expires one; the peer's own sweeper
//     ends it, on the peer's clock.
//
// What stops the second becoming the first is already enforced elsewhere:
// config.Validate refuses a configuration whose AbortAfterSigning is greater
// than or equal to rehearsal.PeerWindow. So a deadline built from the gate is
// inside the peers' window by construction, and this package never reads
// PeerWindow — TestTheSeamsAreBoundedByTheGateAndNotThePeers holds that.
//
// The deadline is the *round's* rather than each device's, and that is the part
// worth stating. m devices each given the gate's worth would let a round run to
// m times the gate and past the peers' window. Signers.Round is called once per
// round, so it stamps one deadline and every device in that round shares it: a
// second device asked after the budget is gone is told the round is over rather
// than given another five minutes.
package webrun

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/AusDavo/winthistle/internal/abort"
	"github.com/AusDavo/winthistle/internal/bitcoind"
	"github.com/AusDavo/winthistle/internal/bump"
	"github.com/AusDavo/winthistle/internal/coldwallet"
	"github.com/AusDavo/winthistle/internal/combine"
	"github.com/AusDavo/winthistle/internal/config"
	"github.com/AusDavo/winthistle/internal/fees"
	"github.com/AusDavo/winthistle/internal/journal"
	"github.com/AusDavo/winthistle/internal/lnd"
	"github.com/AusDavo/winthistle/internal/peers"
	"github.com/AusDavo/winthistle/internal/prose"
	"github.com/AusDavo/winthistle/internal/rehearsal"
	"github.com/AusDavo/winthistle/internal/run"
	"github.com/AusDavo/winthistle/internal/server"
	"github.com/AusDavo/winthistle/internal/setup"
	"github.com/AusDavo/winthistle/internal/signers"
)

// The values a form posts back. Matched exactly, and anything else is no choice
// at all — see each adapter for what that means there.
const (
	ChoiceSigned   = "signed"
	ChoiceCannot   = "cannot"
	ChoiceYes      = "yes"
	ChoiceNo       = "no"
	ChoiceMatched  = "matched"
	ChoiceDiffered = "differed"
	ChoiceNotYet   = "not-yet"
)

// Launcher drives one run at a time on behalf of internal/server.
type Launcher struct {
	cfg   *config.Config
	batch *config.Batch
}

// New builds the launcher. A nil batch is legal and means this server cannot
// start a run: the control is not offered rather than offered and then refused.
func New(cfg *config.Config, batch *config.Batch) *Launcher {
	return &Launcher{cfg: cfg, batch: batch}
}

// Batch is the summary the overview screen shows. Empty means there is nothing
// to start.
func (l *Launcher) Batch() string {
	if l.batch == nil {
		return ""
	}
	return Summary(l.batch)
}

// gate is the seams' clock. See the package comment: the configuration's, and
// never rehearsal.PeerWindow.
func (l *Launcher) gate() time.Duration {
	if l.cfg.Limits.AbortAfterSigning > 0 {
		return l.cfg.Limits.AbortAfterSigning
	}
	return config.DefaultAbortAfter
}

// AddressCheckWindow is how long a browser-driven setup holds its question open.
//
// It is deliberately not the gate, and the reason is that it is not a safety
// bound at all. Every other deadline in this package is carved out of
// limits.abort_after_signing_seconds because a signing round has peers holding
// reservations against outpoints and a clock that is running whether or not
// anybody is ready. The address check has none of that: nothing is armed, no peer
// knows it is happening, and letting it lapse records nothing — the same verdict
// its own third button records.
//
// So the only thing this number bounds is how long the registry's one-at-a-time
// slot is held while the operator is at the safe. It has to be long enough that
// walking to a hardware wallet and reading five addresses off it is not a race,
// because that walk is the entire reason setup.Answer has three values and the
// command is re-runnable. It has to be short enough that a tab abandoned at this
// question frees the slot before somebody gives up on the UI and reaches for the
// terminal.
//
// Fifteen minutes, and nothing is lost by it passing: deriveaddresses has no side
// effect, so the same addresses are there on the next click.
const AddressCheckWindow = 15 * time.Minute

// Start drives one run to completion.
//
// The connections are opened here rather than when the server started, because
// `winthistle serve` has to work on a machine where LND is down — the doctor
// screen is exactly what an operator reads in that state, and a serve that
// refused to start would be a serve that cannot diagnose why.
//
// ctx is the server's, never a request's, and the only thing that cancels it is
// the abort control. Cancelling it unwinds run.Do through the abort path, which
// is the same code `winthistle recover` runs and which refuses outright a run
// that reached the publish call.
func (l *Launcher) Start(ctx context.Context, r *server.Run, req server.StartRequest) error {
	if l.batch == nil {
		return errors.New("there is no batch to open: this server was started " +
			"without one")
	}
	if len(l.cfg.Signers) == 0 {
		return errors.New("no signers are configured. Add a [[signer]] block per " +
			"cold-storage device to winthistle.toml — and exactly as many as the " +
			"descriptor requires, because btcd's finalizer wants exactly m " +
			"signatures and a 2-of-3 carrying three partials does not finalize. " +
			"A browser-driven run needs the labels and the count; it does not need " +
			"a command, because the packet goes out through the page for any device " +
			"that has no command. A device that has one uses it, and is never asked " +
			"here")
	}

	d, closeAll, err := run.Connect(ctx, l.cfg)
	if err != nil {
		return err
	}
	defer closeAll()

	sigs, err := newSigners(r, l.cfg, l.gate())
	if err != nil {
		return err
	}

	d.Out = r
	d.Confirm = Confirmation(r, l.gate())
	d.Signers = sigs

	_, err = run.Do(ctx, d, run.Options{
		Config:            l.cfg,
		Batch:             l.batch,
		RunID:             r.ID,
		Probe:             req.Probe,
		StopBeforePublish: req.StopBeforePublish,
	})
	return err
}

// Wallet is the cold wallet's name, from winthistle.toml's [bitcoind] wallet.
//
// The one place the name comes from, for the reason setup.Options.WalletName is
// that field rather than a flag of its own: the wallet a setup questions has to
// be the wallet a run will spend from, and a second place to name it is a second
// thing to get wrong.
func (l *Launcher) Wallet() string { return l.cfg.Bitcoind.Wallet }

// StartSetup drives `winthistle setup`'s resume path and gives setup.Ask its
// first caller.
//
// # The resume path, and there is no parameter that could make it the other one
//
// setup.Options.Descriptors is nil here and nothing can set it. The install path
// needs an operator's descriptor file, a file needs a path, and a path that came
// in over HTTP is a browser choosing which file this process opens and imports —
// so `winthistle setup --descriptors FILE` stays in the terminal, where the
// operator names their own file. What is left is coldwallet.Read and
// coldwallet.DeriveCheck, which have no side effect at all, and the question,
// which is the half built to be asked twice.
//
// It also means nothing here blocks for a rescan. importdescriptors is minutes
// to hours on mainnet and would hold the registry's one-at-a-time slot for all
// of it; the resume path is a handful of local RPCs.
//
// # Core only, and the clients are opened here
//
// No LND: setting up the cold wallet has no channel, no peer and no macaroon in
// it, so this works on a machine where the node is down — which is the same
// reason setup.Deps has no LND field. Node has no wallet scope, for
// getdescriptorinfo and deriveaddresses; Wallet is bound to the wallet being
// questioned.
//
// Opened here rather than held from New, for the reason Unfinished's journal is:
// `winthistle serve` has to start on a machine where nothing else is up, and a
// journal held open across a serving session is a write lock held against
// `winthistle recover` in another terminal.
func (l *Launcher) StartSetup(ctx context.Context, r *server.Run) error {
	if l.cfg.Bitcoind.Wallet == "" {
		return errors.New("there is no cold wallet to question: name it as " +
			"[bitcoind] wallet in winthistle.toml. It is that key rather than a " +
			"field on the page because the wallet a setup questions has to be the " +
			"wallet a batch will spend from")
	}

	nodeCfg := l.cfg.Bitcoind
	nodeCfg.Wallet = ""
	node, err := bitcoind.New(nodeCfg)
	if err != nil {
		return err
	}
	wallet, err := bitcoind.New(l.cfg.Bitcoind)
	if err != nil {
		return err
	}

	j, err := journal.Open(ctx, l.cfg.Server.Journal)
	if err != nil {
		return fmt.Errorf("opening the run journal at %s: %w",
			l.cfg.Server.Journal, err)
	}
	defer j.Close()

	_, err = setup.Do(ctx, setup.Deps{
		Node:    node,
		Wallet:  wallet,
		Journal: j,
		Out:     r,
		Ask:     Ask(r, AddressCheckWindow),
	}, setup.Options{WalletName: l.cfg.Bitcoind.Wallet})
	return err
}

// StartBump drives `winthistle bump` and gives bump.Approve its first caller.
//
// The same connections a batch needs, because a bump needs the same things: LND
// for the countdown and the broadcast, Core with no wallet scope for the mempool
// entry and the fee estimate, the watch-only cold wallet that owns the change
// output, and the journal that says which transaction is being accelerated.
// run.Connect is what opens them, so a browser-driven bump dials exactly the way
// `winthistle bump` does.
//
// The signers are mixed exactly the way a batch's are — a device with a command
// uses it, a device without one is asked on the page — and the round's name
// carries the bump's sequence number, bump.RoundName. That name matters to both
// transports for the same reason: a second bump minutes after the first must not
// pick up the first one's signed file, whether that file is in a Downloads folder
// or in the handshake directory.
//
// Approve is asked once, after the child is built and verified and before any
// device is touched. An abandoned browser answers false, which releases the coin
// lock and leaves the batch exactly as it was — see the package comment.
func (l *Launcher) StartBump(ctx context.Context, r *server.Run,
	req server.BumpRequest) error {

	if req.RunID == "" {
		return errors.New("a bump needs the id of the run whose batch it is " +
			"accelerating: the journal is what says which transaction that is")
	}
	if !req.BuildOnly && len(l.cfg.Signers) == 0 {
		return errors.New("no signers are configured, and the batch's change " +
			"output belongs to cold storage — so there is nothing that can spend " +
			"it. Add a [[signer]] block per device to winthistle.toml, or build the " +
			"child without asking any device for anything, which is the box on the " +
			"bump screen and is where the arithmetic gets checked")
	}

	d, closeAll, err := run.Connect(ctx, l.cfg)
	if err != nil {
		return err
	}
	defer closeAll()

	// Built even for a build-only bump, which asks no device for anything: the
	// refusal above already let that case through with no signers configured, and
	// newSigners over an empty configuration is an empty mix rather than an error.
	sigs, err := newSigners(r, l.cfg, l.gate())
	if err != nil {
		return err
	}

	_, err = bump.Do(ctx, bump.Deps{
		LND:       d.LND.Lightning,
		Publisher: d.LND.WalletKit,
		Node:      d.Node,
		Wallet:    d.Wallet,
		Journal:   d.Journal,
		Signers:   sigs,
		Out:       r,
		Approve:   Approve(r, l.gate()),
	}, bump.Options{
		RunID:          req.RunID,
		TargetSatPerVB: req.TargetSatPerVB,
		BuildOnly:      req.BuildOnly,
		Fees: fees.Request{
			TargetBlocks:  l.cfg.Fees.TargetBlocks,
			Mode:          l.cfg.Fees.Mode,
			FloorSatPerVB: l.cfg.Fees.FloorSatPerVB,
		},
	})
	return err
}

// AbortRefusal says why this run must not be aborted, or nil.
//
// It asks the journal the same question journal.Run.AbortTarget answers, rather
// than keeping a copy of the answer: a run in publishing or published is refused
// with journal.ErrMayBePublished, which is the refusal run.RecoverOne makes for
// the same reason — abandoning a pending channel whose funding transaction later
// confirms strands its funds with no force-close path.
//
// It fails closed. A journal that cannot be opened or read is not a permission;
// the question being asked is whether a transaction may already be public, and
// the unanswered version of that question is a no. The one error that is *not* a
// refusal is ErrNoRun: a run that has journalled nothing has opened no stream and
// has nothing to take apart, so cancelling it is free.
func (l *Launcher) AbortRefusal(ctx context.Context, runID string) error {
	j, err := journal.Open(ctx, l.cfg.Server.Journal)
	if err != nil {
		return fmt.Errorf("the run journal at %s could not be read, so whether "+
			"this run reached the publish call cannot be established — and that "+
			"question has to be answered before anything is abandoned: %w",
			l.cfg.Server.Journal, err)
	}
	defer j.Close()

	r, err := j.Load(ctx, runID)
	if errors.Is(err, journal.ErrNoRun) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading run %s out of the journal: %w", runID, err)
	}
	_, err = r.AbortTarget()
	return err
}

// Unfinished is the journal's recovery screen, for GET /recover.
//
// It opens the journal itself rather than holding one open from New, for the
// reason Start does not dial LND from New: `winthistle serve` has to work on a
// machine where nothing else does, and a journal held open across a whole
// serving session would be a write lock held against `winthistle recover` in
// another terminal — which is the tool an operator reaches for when the UI is
// the thing that looks wrong.
//
// run.Unfinished writes both halves — the runs and then the CPFP children — and
// that pairing is enforced by there being no exported way to get one without the
// other. A screen that listed the runs and not the children would tell somebody
// their node is clean while a coin of theirs is locked.
//
// Nothing here dials LND or Core, and it must stay that way: this is the screen
// an operator reads on a node that is down.
func (l *Launcher) Unfinished(ctx context.Context) (string, []string, error) {
	j, err := journal.Open(ctx, l.cfg.Server.Journal)
	if err != nil {
		return "", nil, fmt.Errorf("the run journal at %s could not be opened: %w",
			l.cfg.Server.Journal, err)
	}
	defer j.Close()

	var b strings.Builder
	runs, err := run.Unfinished(ctx, j, &b)
	if err != nil {
		return "", nil, err
	}
	ids := make([]string, 0, len(runs))
	for _, r := range runs {
		ids = append(ids, r.ID)
	}
	return b.String(), ids, nil
}

// Journalled is one run's recovery screen, for GET /recover/{id}.
//
// run.Show rather than run.RecoverOne, and the difference is the whole of
// decision 3 on that screen: Show reads the journal and renders it, RecoverOne
// abandons channels. The web UI gets the first and not the second — see
// internal/server/recover.go for why, and note that this method could not
// produce the second anyway: RecoverOne needs run.Deps, which means LND and
// Core, and this is the one screen that has to work without either.
//
// journal.ErrNoRun becomes server.ErrNoJournalledRun because internal/server may
// not name the journal's sentinel. It is a translation and not a re-decision:
// the two mean the same thing, and the reason for the second one existing is the
// import ban rather than any difference in the fact.
func (l *Launcher) Journalled(ctx context.Context, runID string) (string, error) {
	j, err := journal.Open(ctx, l.cfg.Server.Journal)
	if err != nil {
		return "", fmt.Errorf("the run journal at %s could not be opened: %w",
			l.cfg.Server.Journal, err)
	}
	defer j.Close()

	var b strings.Builder
	if _, err := run.Show(ctx, j, &b, runID); err != nil {
		if errors.Is(err, journal.ErrNoRun) {
			return "", fmt.Errorf("%w: %s", server.ErrNoJournalledRun, runID)
		}
		return "", err
	}
	return b.String(), nil
}

// Progress is the running batch's own state, for the attach screen.
//
// It reads the journal rather than asking the run loop, because the number that
// screen exists to show — how many channels have their receipt — is the number
// I-1 turns on, and the journal is what decides it. A run loop reporting its own
// progress would be a second copy of the arming rule, kept in the one place a
// mistake would read as reassurance.
//
// Text and not rows, for the reason Unfinished is: prose is where the copy with
// the overrun tests over it lives, and internal/server rendering a data type
// would be a second rendering measured by nothing.
//
// The peers' window comes from internal/peers, which reads it out of the vendored
// LND rather than restating it — chanfunding.DefaultReservationTimeout, neither
// adjustable in a release build nor ours to choose. It is passed in rather than
// looked up inside prose because it is a fact about the peer's build.
//
// ErrNoJournalledRun when there is no such run, like Journalled: a run whose
// streams have not opened has journalled nothing, and the attach screen falls
// back to its transcript for that case rather than claiming the run is missing.
func (l *Launcher) Progress(ctx context.Context, runID string) (string, error) {
	j, err := journal.Open(ctx, l.cfg.Server.Journal)
	if err != nil {
		return "", fmt.Errorf("the run journal at %s could not be opened: %w",
			l.cfg.Server.Journal, err)
	}
	defer j.Close()

	r, err := j.Load(ctx, runID)
	if errors.Is(err, journal.ErrNoRun) {
		return "", fmt.Errorf("%w: %s", server.ErrNoJournalledRun, runID)
	}
	if err != nil {
		return "", err
	}
	return prose.Progress(r, time.Now(), peers.ReservationTimeout), nil
}

// Signers is run.Signers over the browser: the labels and the count come from
// the configuration, and the packet goes out through the page for every device
// that has no other way to be reached.
//
// The devices are the same devices `winthistle run` uses. What changes is the
// transport: internal/signers has a command on stdin/stdout and a file handshake,
// and this is the third one it always said was coming — the browser's. The packet
// goes out two ways and they are one choice rather than two seams: a read-only
// field to copy, and a .psbt download of the same bytes. Both are built from
// Question.Payload, which is why there is no second copy of the packet anywhere.
//
// # The rule, which is a decision rather than a fact
//
// A round is mixed, per device, and the rule is: **a device whose [[signer]]
// block names a command signs through that command; a device with no command is
// asked on the page.** internal/signers already branched that way — Set.signer
// reads config.Signer.Command — so what newSigners does is give that branch its
// second caller rather than a second implementation of it. There is no new
// exported API here and no second copy of runCommand: byCommand holds a
// one-device signers.Set per commanded device, built through signers.New, which
// needs no Options.Dir because that requirement fires only for a signer with no
// command.
//
// The file handshake is deliberately *not* in the mix. Its instructions are "put
// this file at /some/path and wait", and on a browser-driven run the operator is
// already at a page that can hand them the bytes and take them back — so a
// no-command device is the page's, and Options.Out and Options.Dir never come
// into it.
//
// A run where every device names a command asks the page nothing about signing at
// all. That is correct rather than a hole: the page is still where the run is
// started, watched and stopped, and the transcript says which device signed and
// how long it took.
//
// # Why the choice may not be recomputed
//
// internal/signers' package comment carries the reason: the dress rehearsal's
// measurement predicts the armed window only if the two rounds go through the
// same transport. So the choice has to be per device and stable across rounds —
// a device that took the command in the rehearsal and the page in the batch
// would make the measured number a prediction about a round that never happened.
// It is stable here by construction: byCommand is resolved once, from the
// configuration, and Round indexes it rather than deciding again.
//
// One thing that follows and is not a bug: a command that takes four minutes
// leaves the browser device asked after it with one minute of the gate. The
// deadline bounds the round, not each device — see the package comment — and a
// mixed round says so on the page.
type Signers struct {
	run  *server.Run
	cfg  *config.Config
	gate time.Duration

	// byCommand is parallel to cfg.Signers: entry i is the command transport for
	// cfg.Signers[i], or nil when that device is the page's. Parallel rather than
	// a compacted list of the commanded ones, because Round walks cfg.Signers and
	// two slices of different lengths, consumed in step, is exactly how a device
	// ends up on the wrong transport.
	byCommand []*signers.Set
}

// newSigners resolves each device's transport once, for the reason above.
//
// The error is spelled out rather than swallowed even though it is unreachable
// today — signers.New refuses an empty set and a signer with no command and no
// directory, and neither is possible with one signer that has a command. If a
// later change makes it reachable, a run that dies here has published nothing,
// which is the right end for a signer this build could not construct.
func newSigners(r *server.Run, cfg *config.Config, gate time.Duration) (*Signers, error) {
	s := &Signers{
		run:       r,
		cfg:       cfg,
		gate:      gate,
		byCommand: make([]*signers.Set, len(cfg.Signers)),
	}
	for i, d := range cfg.Signers {
		if d.Command == "" {
			continue
		}
		set, err := signers.New([]config.Signer{d}, signers.Options{})
		if err != nil {
			return nil, fmt.Errorf("the command transport for signer %s: %w",
				d.Label, err)
		}
		s.byCommand[i] = set
	}
	return s, nil
}

// Labels are the devices, in the order an operator would visit them.
func (s *Signers) Labels() []string {
	out := make([]string, 0, len(s.cfg.Signers))
	for _, d := range s.cfg.Signers {
		out = append(out, d.Label)
	}
	return out
}

// Round is one signing round, and it stamps the round's deadline.
//
// One deadline for the whole round, shared by every device in it — including the
// ones signing by command, whose time comes out of the same budget. See the
// package comment: m devices each given the gate's worth would let a round run
// past the peers' window, which is the one thing the gate is carved out of.
//
// The round is in the configured order whatever the transports are, because that
// order is the order an operator walks the safe in. Index and Of count every
// device in the round rather than only the ones the page will see, so "device 3
// of 4" means the third of four — and commandedLabels is what tells the prompt
// why the numbering it shows has gaps in it.
func (s *Signers) Round(name string) []rehearsal.Device {
	deadline := time.Now().Add(s.gate)
	commanded := s.commandedLabels()

	out := make([]rehearsal.Device, 0, len(s.cfg.Signers))
	for i, d := range s.cfg.Signers {
		if s.isCommanded(i) {
			out = append(out, s.commandDevice(i, name))
			continue
		}
		out = append(out, rehearsal.Device{
			Label: d.Label,
			Sign: Signer(s.run, SignRequest{
				Round:     name,
				Label:     d.Label,
				Index:     i + 1,
				Of:        len(s.cfg.Signers),
				Deadline:  deadline,
				ByCommand: commanded,
			}),
		})
	}
	return out
}

// isCommanded reports whether cfg.Signers[i] has a command transport, and it is
// the one place that decides — both the round and the copy that explains the
// round read it, so they cannot disagree about which devices the page will see.
//
// The bounds check is for a Signers built by hand, which the tests do: byCommand
// is nil there and every device is the page's, the behaviour this had before
// there was a choice.
func (s *Signers) isCommanded(i int) bool {
	return i < len(s.byCommand) && s.byCommand[i] != nil
}

// commandDevice is cfg.Signers[i]'s command transport for this round. Only call
// it when isCommanded(i).
//
// The index is deliberate: each of these Sets holds exactly one signer and
// signers.Set.Round returns one device per signer, so devices[0] is that device.
// If that ever stops holding, this panics — which is the right end for it,
// because the alternative is a device silently answered on the transport its
// [[signer]] block did not ask for.
func (s *Signers) commandDevice(i int, round string) rehearsal.Device {
	return s.byCommand[i].Round(round)[0]
}

// commandedLabels names the devices the page will not be asked about, so the
// prompt can say so. Nil when the round is all the page's.
func (s *Signers) commandedLabels() []string {
	var out []string
	for i, d := range s.cfg.Signers {
		if s.isCommanded(i) {
			out = append(out, d.Label)
		}
	}
	return out
}

// SignRequest is what one device is being asked, minus the packet.
type SignRequest struct {
	// Round is "rehearsal" or "batch". They are minutes apart and they are asked
	// for very different things — one is a decoy that is thrown away — so the
	// copy differs and the round is what decides which.
	Round string

	Label string
	Index int
	Of    int

	// Deadline is the round's, not this device's.
	Deadline time.Time

	// ByCommand names the devices in this round that sign through the command
	// their [[signer]] block gives, and are therefore never asked here.
	//
	// It is on the request rather than derived in the prompt because the prompt
	// is the only thing that can explain the gap it leaves: Index and Of count
	// the whole round, so an operator asked for "device 2 of 3" and never for
	// device 1 is owed a sentence saying where device 1 went. Empty means the
	// round is all the page's, which is the ordinary case and says nothing.
	ByCommand []string
}

// Signer is the rehearsal.Signer seam over the browser.
//
// An unanswered question is an error rather than an empty signature, and that is
// the only safe reading: combine.Complete wants exactly m partials, so a device
// that "declined quietly" would produce a packet that does not finalize and a
// failure blamed on the wrong thing.
func Signer(r *server.Run, req SignRequest) rehearsal.Signer {
	return func(ctx context.Context, psbtB64 string) (combine.Part, error) {
		a, err := r.Ask(ctx, server.Question{
			Prompt:       signPrompt(req),
			Payload:      psbtB64,
			PayloadLabel: "the packet to take to " + req.Label + " (base64 PSBT)",
			// The round and the device, which is internal/signers' file
			// handshake naming exactly (signers.go's handshake builds
			// "<round>-<label>.psbt"). Same reason, and the reason is not
			// consistency: the rehearsal and the batch are two rounds minutes
			// apart over two different transactions, and one Downloads folder
			// holding both under one name is how a signature over the decoy
			// gets returned as the batch's.
			PayloadFilename: req.Round + "-" + req.Label + ".psbt",
			Reply:           "paste what " + req.Label + " gave back, base64",
			Choices: []server.Choice{
				{Value: ChoiceSigned, Label: "This is the signed packet"},
				{Value: ChoiceCannot, Label: req.Label + " cannot sign — stop the round"},
			},
			Deadline: deadlineFor(ctx, req.Deadline),
		})
		if err != nil {
			return combine.Part{}, fmt.Errorf("%s: %w", req.Label, err)
		}
		if a.Choice == ChoiceCannot {
			return combine.Part{}, fmt.Errorf("%s cannot sign, so the round is over. "+
				"Nothing is published and nothing is at risk", req.Label)
		}
		if a.Choice != ChoiceSigned || (a.Text == "" && len(a.Upload) == 0) {
			return combine.Part{}, fmt.Errorf("%s: the form came back with no packet "+
				"in it, so there is nothing to combine", req.Label)
		}

		// Pasted or uploaded, one function reads both. combine.Parse settles
		// binary-or-base64 on BIP174's five-byte magic, so a wallet that writes
		// a binary .psbt and one that writes base64 are both read without asking
		// the operator which theirs does — the same tolerance internal/signers'
		// file handshake has always had, in the package that owns what a PSBT is
		// rather than in a second copy here.
		body, where := []byte(a.Text), "the pasted packet"
		if len(a.Upload) > 0 {
			body, where = a.Upload, "the uploaded file"
			if a.UploadName != "" {
				where = a.UploadName
			}
		}
		raw, err := combine.Parse(body)
		if err != nil {
			return combine.Part{}, fmt.Errorf("%s: %s is not a PSBT this build can "+
				"read: %w", req.Label, where, err)
		}
		return combine.Part{Label: req.Label, PSBT: raw}, nil
	}
}

// Confirmation is the abort.Confirmation seam over the browser.
//
// A func per channel, asked at the moment of the rejection, and this adapter
// keeps it that way: every call is its own Question with its own id, carrying
// that channel's outpoint and LND's verbatim rejection. There is no stored "the
// operator allowed the blunt flag" anywhere in this package or in
// internal/server — a checkbox set once would authorise the second channel with
// the answer given about the first, which is precisely what the func type exists
// to prevent.
//
// An unanswered question is false, which is what a nil Confirmation means. Note
// what that does *not* give up: abort.AbandonPending asks PendingChannels itself
// and refuses a channel that is not pending, offering no confirmation at all,
// because there is nothing a human could usefully authorise about removing a
// live channel.
func Confirmation(r *server.Run, gate time.Duration) abort.Confirmation {
	return func(ctx context.Context, req abort.BluntRequest) (bool, error) {
		a, err := r.Ask(ctx, server.Question{
			Prompt: prose.BluntConfirmation(req),
			Choices: []server.Choice{
				{Value: ChoiceNo, Label: "No — leave this channel alone"},
				{Value: ChoiceYes, Label: "Yes — abandon " + short(req.Channel) +
					" with i_know_what_i_am_doing"},
			},
			Deadline: deadlineFor(ctx, time.Now().Add(gate)),
		})
		if errors.Is(err, server.ErrUnanswered) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return a.Choice == ChoiceYes, nil
	}
}

// Approve is the bump.Deps.Approve seam over the browser.
//
// Not the same kind of prompt as Confirmation and it does not pretend to be:
// nothing after this point can lose the batch. What it protects is the
// operator's evening — a bump is a second cold-wallet session, and the screen
// above it is the arithmetic they are agreeing to pay.
//
// It has a caller now: StartBump, behind POST /bump/{id}. Its expiry verdict is
// part of the same property the other three seams are tested for — an abandoned
// browser answers the way no browser would — and here that verdict is false,
// which releases the coin lock and leaves the batch exactly as it was.
func Approve(r *server.Run, gate time.Duration) func(context.Context, string) (bool, error) {
	return func(ctx context.Context, question string) (bool, error) {
		a, err := r.Ask(ctx, server.Question{
			Prompt: prose.Para(question),
			Choices: []server.Choice{
				// Not "do not build the child": by the time this is asked the child
				// is built, verified twice and on the screen above. Offering not to
				// build it would describe the step before this one, and a browser
				// showed exactly that.
				{Value: ChoiceNo, Label: "No — leave the batch as it is"},
				{Value: ChoiceYes, Label: "Sign and broadcast the child"},
			},
			Deadline: deadlineFor(ctx, time.Now().Add(gate)),
		})
		if errors.Is(err, server.ErrUnanswered) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return a.Choice == ChoiceYes, nil
	}
}

// Ask is the setup.Deps.Ask seam over the browser, and it is the one that has to
// be exactly right.
//
// Three answers, not two, because "no" and "not yet" are different facts and
// only one of them is a wallet that must not fund a batch. The mapping below is
// total and it fails to NotAnswered: an expired question, a cancelled run, an
// empty form, a choice this question did not offer, a stale question id — every
// one of them is "nobody compared anything", which setup.Do records as nothing
// at all.
//
// The two answers that *are* recorded are the two that can only be produced by
// pressing one of two buttons that say what they mean. Everything else records
// nothing, which is the same safe default `askComparison` has on the command
// line: a stray keypress cannot confirm a wallet nobody looked at, and it cannot
// condemn a working one either.
//
// It has a caller now: StartSetup, behind POST /setup. That route is the resume
// path only — no descriptor file crosses the HTTP boundary — so the question this
// asks is always about descriptors Core already holds.
func Ask(r *server.Run, gate time.Duration) func(context.Context, coldwallet.AddressCheck) (setup.Answer, error) {
	return func(ctx context.Context, c coldwallet.AddressCheck) (setup.Answer, error) {
		a, err := r.Ask(ctx, server.Question{
			Prompt: askPrompt(c),
			Choices: []server.Choice{
				{Value: ChoiceMatched, Label: "Yes — every address was in my wallet"},
				{Value: ChoiceDiffered, Label: "No — they did not match"},
				{Value: ChoiceNotYet, Label: "I have not compared them yet"},
			},
			Deadline: deadlineFor(ctx, time.Now().Add(gate)),
		})
		if err != nil {
			// Including a cancelled run. Nothing is recorded, and the question can
			// be asked again: deriveaddresses has no side effect, so the same
			// addresses are there tomorrow.
			if errors.Is(err, server.ErrUnanswered) {
				return setup.NotAnswered, nil
			}
			return setup.NotAnswered, err
		}
		switch a.Choice {
		case ChoiceMatched:
			return setup.Matched, nil
		case ChoiceDiffered:
			return setup.Differed, nil
		}
		return setup.NotAnswered, nil
	}
}

// clockMargin is how far inside the context's own deadline a seam's deadline
// sits when the context is the tighter of the two. Small, and its only job is to
// stop a coin flip: two timers at the same instant are a select the runtime
// picks at random.
const clockMargin = 250 * time.Millisecond

// deadlineFor is the seam's deadline, never outside the context's.
//
// The blunt-abandon confirmation is asked *during* a teardown, and the teardown
// has a budget of its own (run.TeardownBudget). Unclamped, the context would end
// first and Ask would return ctx.Err() rather than ErrUnanswered — so an operator
// who let the question lapse would get an abort that failed instead of one that
// declined to escalate, which is the difference between "finish this by hand" and
// "something went wrong".
//
// When there is less than the margin left, the answer is now: a question that
// cannot be answered inside the time remaining is already unanswered, and saying
// so deterministically beats racing the context for the right error.
func deadlineFor(ctx context.Context, by time.Time) time.Time {
	d, ok := ctx.Deadline()
	if !ok || by.Before(d) {
		return by
	}
	early := d.Add(-clockMargin)
	if now := time.Now(); early.Before(now) {
		return now
	}
	return early
}

// Summary is the batch, as the overview screen shows it.
//
// The same text `winthistle run` prints before it asks whether to open the
// batch, and for the same reason: the amounts and the peers are what the
// operator is agreeing to, and a second renderer of them is a second thing to
// keep in step with the plan document.
func Summary(b *config.Batch) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d channel%s, %s in total\n", len(b.Channels),
		prose.Plural(len(b.Channels)), prose.Sats(b.TotalSat()))
	// The peer on its own line, indented, rather than beside the amount. Two
	// spaces plus a 14-wide amount plus a 66-character pubkey is 84 characters
	// against a 78-column pane, so the old single line always soft-wrapped — on
	// the first screen an operator sees, which made a correct batch look
	// mangled. The pubkey is not abbreviated: this is the screen where it is
	// checked.
	for _, ch := range b.Channels {
		kind := ""
		if ch.Private {
			kind = "   (unannounced)"
		}
		fmt.Fprintf(&sb, "\n  %s%s\n", prose.Sats(ch.AmountSat), kind)
		fmt.Fprintf(&sb, "    %s\n", ch.Peer)
		fmt.Fprintf(&sb, "    %s\n", ch.Policy.Summary())
	}
	return sb.String()
}

// short is an outpoint an operator can still recognise, on one line.
//
// The full 66-character txid went in this label until a browser rendered it: the
// button wrapped to three centred lines and became the largest thing on the
// screen, which made the dangerous choice visually dominant over "No — leave
// this channel alone". That is the wrong affordance for the one prompt in this
// product where a human authorises something that could lose funds if the
// premise were wrong.
//
// Nothing is lost by shortening it. The prompt immediately above the buttons
// states the outpoint in full, twice — once as the question and once inside LND's
// verbatim rejection — and what stops a stale form answering about the wrong
// channel is the question id, not the reader.
func short(cp lnd.ChannelPoint) string {
	txid := cp.TxID
	if len(txid) > 12 {
		txid = txid[:12] + "…"
	}
	return fmt.Sprintf("%s:%d", txid, cp.Index)
}

// signPrompt is the copy over one device's packet, and there are three rounds
// rather than two.
//
// A bump's round used to fall through to the batch's branch, which opens "This is
// the batch. The peers' reservations are open and their clocks are running." Both
// sentences are false about a CPFP child: the batch is already public, and no peer
// holds a reservation against a transaction that spends its change. That is the
// worst kind of false copy — it would have told an operator the ten minutes were
// running during the one signing round where they are not.
//
// The round is matched on bump.RoundPrefix rather than on a literal, because the
// name is composed in internal/bump and a string spelled in two packages is how
// this copy ends up on the wrong round again.
//
// One paragraph is conditional on the round being mixed, and it is there because
// the heading counts the whole round: an operator asked for "device 2 of 3" who
// is never asked for device 1 would otherwise have to guess whether the page lost
// it. It names the devices and says where they went. On an all-page round — the
// ordinary one — it is not written at all, because a paragraph explaining an
// absence that is not there is noise on the screen where noise costs most.
func signPrompt(req SignRequest) string {
	var b strings.Builder
	switch {
	case req.Round == "rehearsal":
		fmt.Fprintf(&b, "\nThe dress rehearsal — %s, device %d of %d\n\n",
			req.Label, req.Index, req.Of)
		b.WriteString(prose.Para("This is not the batch. It is a decoy over the " +
			"same coins, the same fee rate and the same number of outputs, all of " +
			"them paying back into the cold wallet, and it is thrown away when the " +
			"round is over. What is being measured is how long a full signing round " +
			"takes with these devices, in this room, tonight — because the peers' " +
			"ten minutes start whether or not anyone is ready, and until the round " +
			"has been measured, arming is a bet."))
	case strings.HasPrefix(req.Round, bump.RoundPrefix):
		fmt.Fprintf(&b, "\nThe child's signing round — %s, device %d of %d\n\n",
			req.Label, req.Index, req.Of)
		b.WriteString(prose.Para("This is not the batch. The batch is already " +
			"public — that is what a CPFP child is for — and this transaction " +
			"spends its change output to pay a higher rate for both of them " +
			"together. No peer holds a reservation against it and no channel " +
			"depends on it, so nothing from here on can lose the batch."))
	default:
		fmt.Fprintf(&b, "\nThe signing round — %s, device %d of %d\n\n",
			req.Label, req.Index, req.Of)
		b.WriteString(prose.Para("This is the batch. The peers' reservations are " +
			"open and their clocks are running."))
	}
	b.WriteString("\n")
	b.WriteString(prose.Para("Take the packet below to " + req.Label + ", sign it " +
		"there, and paste back what it gives you."))
	b.WriteString("\n")
	if len(req.ByCommand) > 0 {
		b.WriteString(prose.Para(fmt.Sprintf("Not every device in this round is "+
			"asked here. %s %s answered by the command winthistle.toml gives, on "+
			"this machine, and this page never sees that packet — which is why the "+
			"numbering above has a gap in it. The budget below is the whole round's, "+
			"so time a command spends is time this question does not have.",
			strings.Join(req.ByCommand, ", "), prose.IsAre(len(req.ByCommand)))))
		b.WriteString("\n")
	}
	b.WriteString(prose.Para("Do not let the wallet finalize. This tool combines " +
		"the partial signatures itself, and a packet that arrives already " +
		"finalized is refused, naming the device: a finalized input is a complete " +
		"witness, which means that device held a transaction it could have " +
		"broadcast, and no external party may ever hold one."))
	b.WriteString("\n")
	if strings.HasPrefix(req.Round, bump.RoundPrefix) {
		// What expiry costs is on the run screen above this, once. Said here it
		// would be the third copy of it on one page.
		b.WriteString(prose.Para(fmt.Sprintf("This round has until %s. That is "+
			"limits.abort_after_signing_seconds — the same budget a batch's signing "+
			"round is measured against, reused here because it is the only measured "+
			"one there is for a round with these devices — and it belongs to the "+
			"round rather than to this device, so every other device's signature has "+
			"to fit inside it too.", req.Deadline.Format(time.TimeOnly))))
		return b.String()
	}
	b.WriteString(prose.Para(fmt.Sprintf("This round has until %s. That is the "+
		"signing gate the dress rehearsal is measured against, not a timeout of "+
		"this page's, and it belongs to the round rather than to this device — the "+
		"whole round has to fit inside it. Letting it pass costs one more signing "+
		"round: nothing is published while this question is open.",
		req.Deadline.Format(time.TimeOnly))))
	return b.String()
}

func askPrompt(c coldwallet.AddressCheck) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\nThe cold wallet's setup — %s\n\n", c.WalletName)
	b.WriteString(setup.Question + "\n\n")
	b.WriteString(prose.Para("The addresses are above, derived from the " +
		"descriptors the wallet named there actually holds. Nothing this program can check " +
		"separates a correct cold-storage descriptor from a plausible wrong one — " +
		"Core parses both, the import succeeds for both, and the balance does not " +
		"separate them either — so a comparison a human made is the only verdict " +
		"there is."))
	b.WriteString("\n")
	b.WriteString(prose.Para("\"I have not compared them yet\" records nothing, " +
		"and it is the right answer if you have not been to the device. Walking " +
		"away costs nothing: deriveaddresses has no side effect, and the same " +
		"addresses are here tomorrow."))
	return b.String()
}
