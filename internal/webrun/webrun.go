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
// limits.abort_after_signing_seconds, and not the peers' ten minutes. The
// difference is not stylistic:
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
	"github.com/AusDavo/winthistle/internal/coldwallet"
	"github.com/AusDavo/winthistle/internal/combine"
	"github.com/AusDavo/winthistle/internal/config"
	"github.com/AusDavo/winthistle/internal/journal"
	"github.com/AusDavo/winthistle/internal/lnd"
	"github.com/AusDavo/winthistle/internal/peers"
	"github.com/AusDavo/winthistle/internal/prose"
	"github.com/AusDavo/winthistle/internal/rehearsal"
	"github.com/AusDavo/winthistle/internal/run"
	"github.com/AusDavo/winthistle/internal/server"
	"github.com/AusDavo/winthistle/internal/setup"
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
			"A browser-signed run needs the labels and the count; it does not need " +
			"a command, because the packet goes out through the page")
	}

	d, closeAll, err := run.Connect(ctx, l.cfg)
	if err != nil {
		return err
	}
	defer closeAll()

	d.Out = r
	d.Confirm = Confirmation(r, l.gate())
	d.Signers = &Signers{run: r, cfg: l.cfg, gate: l.gate()}

	_, err = run.Do(ctx, d, run.Options{
		Config:            l.cfg,
		Batch:             l.batch,
		RunID:             r.ID,
		Probe:             req.Probe,
		StopBeforePublish: req.StopBeforePublish,
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
// the configuration, and the packet goes out through the page.
//
// The devices are the same devices `winthistle run` uses. What changes is the
// transport: internal/signers has a command on stdin/stdout and a file handshake,
// and this is the third one it always said was coming — the browser's. The packet
// goes out two ways and they are one choice rather than two seams: a read-only
// field to copy, and a .psbt download of the same bytes. Both are built from
// Question.Payload, which is why there is no second copy of the packet anywhere.
//
// What this does not do yet is mix transports. A browser-driven run uses the
// browser for every device, even when a [[signer]] block names a working command,
// and choosing per device belongs with the rest of the transports.
type Signers struct {
	run  *server.Run
	cfg  *config.Config
	gate time.Duration
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
// One deadline for the whole round, shared by every device in it. See the
// package comment: m devices each given the gate's worth would let a round run
// past the peers' window, which is the one thing the gate is carved out of.
func (s *Signers) Round(name string) []rehearsal.Device {
	deadline := time.Now().Add(s.gate)
	labels := s.Labels()

	out := make([]rehearsal.Device, 0, len(labels))
	for i, label := range labels {
		out = append(out, rehearsal.Device{
			Label: label,
			Sign: Signer(s.run, SignRequest{
				Round:    name,
				Label:    label,
				Index:    i + 1,
				Of:       len(labels),
				Deadline: deadline,
			}),
		})
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
// Its POST arrives with the bump screen, item 1.6. The adapter is here now
// because it is one of the four seams and because its expiry verdict is part of
// the same property the other three are tested for: an abandoned browser answers
// the way no browser would.
func Approve(r *server.Run, gate time.Duration) func(context.Context, string) (bool, error) {
	return func(ctx context.Context, question string) (bool, error) {
		a, err := r.Ask(ctx, server.Question{
			Prompt: prose.Para(question),
			Choices: []server.Choice{
				{Value: ChoiceNo, Label: "No — do not build the child"},
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
// Its POST arrives with the setup screen, item 1.6.
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

func signPrompt(req SignRequest) string {
	var b strings.Builder
	if req.Round == "rehearsal" {
		fmt.Fprintf(&b, "\nThe dress rehearsal — %s, device %d of %d\n\n",
			req.Label, req.Index, req.Of)
		b.WriteString(prose.Para("This is not the batch. It is a decoy over the " +
			"same coins, the same fee rate and the same number of outputs, all of " +
			"them paying back into the cold wallet, and it is thrown away when the " +
			"round is over. What is being measured is how long a full signing round " +
			"takes with these devices, in this room, tonight — because the peers' " +
			"ten minutes start whether or not anyone is ready, and until the round " +
			"has been measured, arming is a bet."))
	} else {
		fmt.Fprintf(&b, "\nThe signing round — %s, device %d of %d\n\n",
			req.Label, req.Index, req.Of)
		b.WriteString(prose.Para("This is the batch. The peers' reservations are " +
			"open and their clocks are running."))
	}
	b.WriteString("\n")
	b.WriteString(prose.Para("Take the packet below to " + req.Label + ", sign it " +
		"there, and paste back what it gives you."))
	b.WriteString("\n")
	b.WriteString(prose.Para("Do not let the wallet finalize. This tool combines " +
		"the partial signatures itself, and a packet that arrives already " +
		"finalized is refused, naming the device: a finalized input is a complete " +
		"witness, which means that device held a transaction it could have " +
		"broadcast, and no external party may ever hold one."))
	b.WriteString("\n")
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
