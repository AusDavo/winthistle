package webrun

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AusDavo/winthistle/internal/abort"
	"github.com/AusDavo/winthistle/internal/coldwallet"
	"github.com/AusDavo/winthistle/internal/config"
	"github.com/AusDavo/winthistle/internal/lnd"
	"github.com/AusDavo/winthistle/internal/policy"
	"github.com/AusDavo/winthistle/internal/prose"
	"github.com/AusDavo/winthistle/internal/server"
	"github.com/AusDavo/winthistle/internal/setup"
)

// aRun is a server.Run to ask questions of.
func aRun(t *testing.T) *server.Run {
	t.Helper()
	r, err := server.NewRegistry().Start("run-1", server.KindBatch, "", func() {})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return r
}

// answerWith replies to whatever the run asks next, with the given choice and
// text. It returns once it has replied, or fails.
func answerWith(t *testing.T, r *server.Run, choice, text string) *server.Question {
	t.Helper()
	return answerAfter(t, r, "", choice, text)
}

// answerAfter is the same, waiting for a question that is not the one whose id
// is given. A reply is handed over before Ask has cleared the pending question,
// so a test that asks twice in a row has to wait for the second one by id — the
// same distinction the handler makes, and for the same reason.
// The bound is liveness, not latency. Every caller spawns the seam in a goroutine
// and waits here for it to ask; what is asserted is that the question arrives and
// what it says, never how fast. So the bound only has to be long enough that a
// scheduler under load cannot be mistaken for a seam that never asked.
func answerAfter(t *testing.T, r *server.Run, notID, choice, text string) *server.Question {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if q := r.Pending(); q != nil && q.ID != notID {
			if err := r.Reply(q.ID, server.Answer{Choice: choice, Text: text}); err != nil {
				t.Fatalf("Reply: %v", err)
			}
			return q
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the seam never asked anything")
	return nil
}

const testGate = 5 * time.Minute

// past is a deadline that has already gone, which is what an abandoned browser
// looks like from inside a seam.
func past(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithDeadline(context.Background(),
		time.Now().Add(-time.Second))
	t.Cleanup(cancel)
	return ctx
}

// TestNotAnsweredIsWhatEverythingElseRecords is the highest-stakes guard in this
// package.
//
// setup.Answer has three values because a comparison nobody made must never be
// recorded as a verdict, and this is the table that says which inputs are a
// verdict. Two are: the two buttons that say what they mean. Everything else —
// an expired question, an empty form, a choice this question did not offer, a
// third button that says "not yet" — is NotAnswered, which setup.Do records as
// nothing at all.
//
// Note that the safety is layered rather than resting here. setup.Do returns
// before RecordSetup on NotAnswered and is the only writer of the setups table,
// and internal/server cannot import internal/journal, so no handler can write
// that table even by accident. This is the layer that decides what a browser
// meant.
func TestNotAnsweredIsWhatEverythingElseRecords(t *testing.T) {
	check := coldwallet.AddressCheck{WalletName: "cold-watch"}

	for _, tc := range []struct {
		name   string
		choice string
		want   setup.Answer
	}{
		{"the yes button", ChoiceMatched, setup.Matched},
		{"the no button", ChoiceDiffered, setup.Differed},
		{"the not-yet button", ChoiceNotYet, setup.NotAnswered},
		{"an empty form", "", setup.NotAnswered},
		{"a choice nobody offered", "probably", setup.NotAnswered},
		{"a near miss", "match", setup.NotAnswered},
		{"the string true", "true", setup.NotAnswered},
		{"a yes from a different seam", ChoiceYes, setup.NotAnswered},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := aRun(t)
			ask := Ask(r, testGate)

			done := make(chan setup.Answer, 1)
			errs := make(chan error, 1)
			go func() {
				a, err := ask(context.Background(), check)
				done <- a
				errs <- err
			}()
			answerWith(t, r, tc.choice, "")

			if got := <-done; got != tc.want {
				t.Errorf("%q recorded %v, want %v", tc.choice, got, tc.want)
			}
			if err := <-errs; err != nil {
				t.Errorf("it also returned %v; a form that came back is not an error", err)
			}
		})
	}
}

// TestAnAbandonedAddressCheckRecordsNothing. The browser closed, or the operator
// went to find their hardware wallet and did not come back tonight. Neither is a
// comparison, and neither is an error: this command is re-runnable precisely so
// walking away is free — deriveaddresses has no side effect, so the same
// addresses are there tomorrow.
func TestAnAbandonedAddressCheckRecordsNothing(t *testing.T) {
	r := aRun(t)
	ask := Ask(r, time.Millisecond)

	got, err := ask(context.Background(), coldwallet.AddressCheck{WalletName: "cold-watch"})
	if got != setup.NotAnswered {
		t.Errorf("an abandoned form recorded %v", got)
	}
	if err != nil {
		t.Errorf("it returned %v; an unanswered question is a verdict of NotAnswered "+
			"rather than a failure", err)
	}
}

// TestACancelledRunRecordsNothingEither, and it does return the error: a run
// that was aborted is a different fact from one nobody answered, and setup.Do
// has to stop rather than carry on with a wallet it never asked about.
func TestACancelledRunRecordsNothingEither(t *testing.T) {
	r := aRun(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got, err := Ask(r, testGate)(ctx, coldwallet.AddressCheck{WalletName: "cold-watch"})
	if got != setup.NotAnswered {
		t.Errorf("a cancelled run recorded %v", got)
	}
	if err == nil {
		t.Error("a cancelled run returned no error, so setup.Do would carry on")
	}
}

// TestAnUnansweredBluntConfirmationDeclines.
//
// False is what a nil Confirmation means: never fall back to
// i_know_what_i_am_doing. It costs an abort that has to be finished by hand,
// which is the cheap side of the trade — the flag gives up every protection in
// that call, including the one that would stop it removing a confirmed channel.
//
// It declines rather than erroring, and the distinction is load-bearing: the
// confirmation is asked during a teardown, and an error there reads as "the
// abort went wrong" when what happened is "nobody authorised the blunt flag".
func TestAnUnansweredBluntConfirmationDeclines(t *testing.T) {
	r := aRun(t)
	ok, err := Confirmation(r, time.Millisecond)(context.Background(), abort.BluntRequest{
		Channel:   lnd.ChannelPoint{TxID: "abc", Index: 0},
		Rejection: "channel abc:0 is not externally funded or not pending",
	})
	if ok {
		t.Error("an unanswered confirmation authorised the blunt flag")
	}
	if err != nil {
		t.Errorf("it also returned %v, which reads as a failed abort rather than "+
			"an unauthorised escalation", err)
	}
}

// TestEachChannelIsItsOwnConfirmation is why abort.Confirmation is a func rather
// than a bool.
//
// "A func rather than a bool so it cannot be set once and forgotten: the caller
// is asked per channel, at the moment of the rejection." A handler that turned it
// into a set-once checkbox would authorise the second channel with the answer
// given about the first — so every call here is its own Question, with its own
// id and that channel's outpoint in it, and there is no stored permission
// anywhere in this package.
func TestEachChannelIsItsOwnConfirmation(t *testing.T) {
	r := aRun(t)
	confirm := Confirmation(r, testGate)

	one := lnd.ChannelPoint{TxID: "aaa", Index: 1}
	two := lnd.ChannelPoint{TxID: "bbb", Index: 2}

	type outcome struct {
		ok  bool
		err error
	}
	first := make(chan outcome, 1)
	go func() {
		ok, err := confirm(context.Background(), abort.BluntRequest{Channel: one})
		first <- outcome{ok, err}
	}()
	q1 := answerWith(t, r, ChoiceYes, "")
	if !strings.Contains(q1.Prompt, one.String()) {
		t.Errorf("the first question does not name its own channel:\n%s", q1.Prompt)
	}
	if got := <-first; !got.ok || got.err != nil {
		t.Fatalf("the authorised channel came back %+v", got)
	}

	// A second channel, and the yes given about the first must buy nothing.
	second := make(chan outcome, 1)
	go func() {
		ok, err := confirm(context.Background(), abort.BluntRequest{Channel: two})
		second <- outcome{ok, err}
	}()
	q2 := answerAfter(t, r, q1.ID, ChoiceNo, "")
	if q2.ID == q1.ID {
		t.Error("both channels were asked about under one question id, so one " +
			"answer covers both")
	}
	if !strings.Contains(q2.Prompt, two.String()) {
		t.Errorf("the second question does not name its own channel:\n%s", q2.Prompt)
	}
	if strings.Contains(q2.Prompt, one.String()) {
		t.Errorf("the second question still names the first channel:\n%s", q2.Prompt)
	}
	if got := <-second; got.ok {
		t.Error("the second channel was authorised by the answer to the first")
	}
}

// TestAnUnansweredApprovalDeclines. False releases the child's coin lock and
// leaves the batch exactly as it was; nothing about a bump can lose it.
func TestAnUnansweredApprovalDeclines(t *testing.T) {
	r := aRun(t)
	ok, err := Approve(r, time.Millisecond)(context.Background(),
		"Sign this child and broadcast it, lifting the batch to 24.00 sat/vB?")
	if ok {
		t.Error("an unanswered approval approved a signing round")
	}
	if err != nil {
		t.Errorf("it also returned %v", err)
	}
}

// TestApprovalMatchesTheAffirmativeOnly. The obligation server.Question.Choices
// puts on every adapter: `== yes`, never `!= no`, because a typo must not read as
// consent.
func TestApprovalMatchesTheAffirmativeOnly(t *testing.T) {
	for _, choice := range []string{"", "y", "yep", "true", ChoiceSigned, ChoiceMatched} {
		r := aRun(t)
		got := make(chan bool, 1)
		go func() {
			ok, _ := Approve(r, testGate)(context.Background(), "well?")
			got <- ok
		}()
		answerWith(t, r, choice, "")
		if <-got {
			t.Errorf("%q was read as approval", choice)
		}
	}
}

// TestAnUnansweredSignerFailsTheRound.
//
// An error rather than an empty signature, and that is the only safe reading:
// combine.Complete wants exactly m partials, so a device that "declined quietly"
// would produce a packet that does not finalize and a failure blamed on the wrong
// thing. For the rehearsal round the failure is the verdict rehearsal.Gate would
// have reached anyway; for the batch round it fails the armed window, which
// unwinds through the abort path with nothing published.
func TestAnUnansweredSignerFailsTheRound(t *testing.T) {
	r := aRun(t)
	sign := Signer(r, SignRequest{
		Round: "batch", Label: "cold1", Index: 1, Of: 2,
		Deadline: time.Now().Add(time.Millisecond),
	})

	_, err := sign(context.Background(), "cHNidP8BAAA=")
	if err == nil {
		t.Fatal("an unanswered signing question produced a Part")
	}
	if !errors.Is(err, server.ErrUnanswered) {
		t.Errorf("it failed with %v rather than as an unanswered question", err)
	}
	if !strings.Contains(err.Error(), "cold1") {
		t.Errorf("the failure does not name the device: %v", err)
	}
}

// TestASignerWithNoPacketIsRefused: the form came back, with nothing in it.
// There is nothing to combine, and saying so beats handing internal/combine an
// empty packet to blame a device for.
func TestASignerWithNoPacketIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name, choice, text, want string
	}{
		{"declined", ChoiceCannot, "", "cannot sign"},
		{"an empty field", ChoiceSigned, "", "no packet"},
		{"no choice at all", "", "cHNidP8BAAA=", "no packet"},
		// The message changed when the upload leg landed and both halves of the
		// transport started going through combine.Parse: it names what was
		// actually in the field rather than repeating base64's complaint about
		// padding, which told nobody anything. That is the message
		// internal/signers' file handshake already gave.
		{"not a psbt", ChoiceSigned, "not base64 at all", "neither base64 nor a PSBT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := aRun(t)
			sign := Signer(r, SignRequest{
				Round: "batch", Label: "cold2", Index: 2, Of: 2,
				Deadline: time.Now().Add(time.Minute),
			})
			errs := make(chan error, 1)
			go func() {
				_, err := sign(context.Background(), "cHNidP8BAAA=")
				errs <- err
			}()
			answerWith(t, r, tc.choice, tc.text)

			err := <-errs
			if err == nil {
				t.Fatal("it produced a Part")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal is %v, which does not say %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), "cold2") {
				t.Errorf("the refusal does not name the device: %v", err)
			}
		})
	}
}

// TestTheRoundsDeadlineIsSharedByEveryDevice.
//
// The bound is the 5:00 gate, and it belongs to the *round*. m devices each given
// the gate's worth would let a round run to m times the gate and out past the
// peers' ten minutes, which is the budget the gate is carved out of. Round is
// called once per round, so it stamps one deadline and every device shares it.
func TestTheRoundsDeadlineIsSharedByEveryDevice(t *testing.T) {
	r := aRun(t)
	s := &Signers{run: r, gate: testGate, cfg: &config.Config{
		Signers: []config.Signer{{Label: "cold1"}, {Label: "cold2"}},
	}}

	devices := s.Round("batch")
	if len(devices) != 2 {
		t.Fatalf("Round returned %d devices", len(devices))
	}

	var deadlines []time.Time
	last := ""
	for _, dev := range devices {
		dev := dev
		// Awaited, one device at a time, because Run.Ask refuses a second
		// question while one is pending — and Reply returns as soon as the answer
		// is buffered, before the asking Ask has woken up and cleared it. A test
		// that spawned the next device on Reply returning was racing that window:
		// the second Ask could see the first question still pending, return
		// ErrAlreadyAsking without asking anything, and leave answerAfter waiting
		// for a question nobody was going to post. Rare, and likelier the busier
		// the machine.
		//
		// Serial is also what production does — run.sign awaits each device's
		// Sign before calling the next — so this is the sequence being tested
		// rather than a concession to the test.
		done := make(chan struct{})
		go func() {
			defer close(done)
			dev.Sign(context.Background(), "cHNidP8BAAA=")
		}()
		q := answerAfter(t, r, last, ChoiceCannot, "")
		last = q.ID
		if !strings.Contains(q.Prompt, dev.Label) {
			t.Errorf("the question does not name %s:\n%s", dev.Label, q.Prompt)
		}
		deadlines = append(deadlines, q.Deadline)
		<-done
	}
	if !deadlines[0].Equal(deadlines[1]) {
		t.Errorf("the two devices were given different deadlines, %s and %s.\n"+
			"  The gate is the round's budget, not each device's: m devices with a "+
			"gate each would run the round out past the peers' ten minutes.",
			deadlines[0], deadlines[1])
	}
	// And the deadline really is the gate rather than something longer.
	if got := deadlines[0].Sub(time.Now()); got > testGate {
		t.Errorf("the round's deadline is %s away, past the %s gate", got, testGate)
	}
}

// TestTheDeadlineIsClampedInsideTheContext.
//
// The blunt-abandon confirmation is asked during a teardown, and the teardown has
// a budget of its own. Unclamped, the context would end first and the seam would
// return ctx.Err() rather than ErrUnanswered — so an operator who let the
// question lapse would get an abort that failed instead of one that declined to
// escalate, which is the difference between "finish this by hand" and "something
// went wrong".
func TestTheDeadlineIsClampedInsideTheContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	ctxDeadline, _ := ctx.Deadline()
	got := deadlineFor(ctx, time.Now().Add(testGate))
	if !got.Before(ctxDeadline) {
		t.Errorf("the seam's deadline %s is not inside the context's %s", got, ctxDeadline)
	}

	// And a deadline already inside the context's is left alone.
	soon := time.Now().Add(time.Millisecond)
	if !deadlineFor(ctx, soon).Equal(soon) {
		t.Error("a deadline already inside the context's was moved")
	}

	// End to end: the confirmation declines rather than erroring.
	r := aRun(t)
	ok, err := Confirmation(r, testGate)(ctx, abort.BluntRequest{
		Channel: lnd.ChannelPoint{TxID: "abc", Index: 0},
	})
	if ok {
		t.Error("it authorised the blunt flag")
	}
	if err != nil {
		t.Errorf("it returned %v rather than declining", err)
	}
}

// TestTheSeamsAreBoundedByTheGateAndNotThePeers.
//
// Two clocks bound this product and only one of them is ours. The 5:00 gate is:
// we set it, rehearsal.Gate measures against it, and it is therefore a number
// this code may enforce. The peers' ten minutes are not — LND's
// pruneZombieReservations skips PSBT reservations, so our node never expires one
// and the peer's own sweeper ends it, on the peer's clock.
//
// So this package reads AbortAfterSigning and never PeerWindow. What makes that
// sound rather than merely tidy is enforced elsewhere: config.Validate refuses a
// configuration whose AbortAfterSigning is greater than or equal to PeerWindow,
// so a deadline built from the gate is inside the peers' window by construction
// and can only ever expire early. Early is the safe direction — it costs one more
// ceremony, and nothing is published while a question is open.
func TestTheSeamsAreBoundedByTheGateAndNotThePeers(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}

	fset := token.NewFileSet()
	checked, gate := 0, 0

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") ||
			strings.HasSuffix(name, "_test.go") {

			continue
		}
		// Parsed rather than grepped, so the ban is on what the code reads and
		// not on what the comments are allowed to explain. This file's own
		// reasoning names PeerWindow several times, and it should.
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		checked++
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch sel.Sel.Name {
			case "PeerWindow":
				t.Errorf("%s: the code reads %s.PeerWindow.\n"+
					"  The peers' ten minutes are the peers' clock: "+
					"pruneZombieReservations skips PSBT reservations, so nothing here "+
					"can shorten or extend one. A seam's bound has to come from the "+
					"clock we own, which is limits.abort_after_signing_seconds.",
					fset.Position(sel.Pos()), types.ExprString(sel.X))
			case "AbortAfterSigning":
				gate++
			}
			return true
		})
	}
	if checked == 0 {
		t.Fatal("no non-test .go files were parsed; this check would pass for the " +
			"wrong reason")
	}
	if gate == 0 {
		t.Error("nothing here reads Limits.AbortAfterSigning, so the seams are " +
			"bounded by something other than the gate")
	}
}

// TestAbortRefusalFailsClosed.
//
// The question being asked is whether a transaction may already be public, and
// the unanswered version of that question is a no. A journal that cannot be read
// is not a permission.
//
// The one error that is not a refusal is ErrNoRun: a run that has journalled
// nothing opened no stream, so there is nothing to take apart and cancelling it
// is free.
func TestAbortRefusalFailsClosed(t *testing.T) {
	dir := t.TempDir()

	l := New(&config.Config{Server: config.Server{
		Journal: filepath.Join(dir, "runs.db"),
	}}, nil)

	// A run the journal has never heard of. Nothing to refuse.
	if err := l.AbortRefusal(context.Background(), "never-existed"); err != nil {
		t.Errorf("a run with nothing journalled was refused: %v", err)
	}

	// A journal that cannot be opened — the path is a directory.
	broken := New(&config.Config{Server: config.Server{Journal: dir}}, nil)
	err := broken.AbortRefusal(context.Background(), "some-run")
	if err == nil {
		t.Fatal("an unreadable journal was treated as permission to abort")
	}
	if !strings.Contains(err.Error(), "publish call") {
		t.Errorf("the refusal does not say what could not be established: %v", err)
	}
}

// TestALauncherWithNoBatchOffersNothing. Absent rather than disabled, and the
// refusal from Start says the same thing the screen does.
func TestALauncherWithNoBatchOffersNothing(t *testing.T) {
	l := New(&config.Config{}, nil)
	if l.Batch() != "" {
		t.Errorf("a launcher with no batch described one: %q", l.Batch())
	}
	err := l.Start(context.Background(), aRun(t), server.StartRequest{})
	if err == nil {
		t.Fatal("it started a run with no batch")
	}
	if !strings.Contains(err.Error(), "no batch") {
		t.Errorf("the refusal is %v", err)
	}
}

// TestEveryPromptAndButtonFitsThePane collects two defects that a browser found
// and prose did not, and it is deliberately one test: both were the same mistake,
// which is that a 66-character identifier does not fit in a 78-column pane
// beside anything else.
//
//   - The blunt-abandon button carried the full outpoint, so it wrapped to three
//     centred lines and became the largest thing on the screen — making the
//     dangerous choice visually dominant over "No — leave this channel alone".
//   - Summary put the amount and the pubkey on one line: two spaces plus a
//     14-wide amount plus 66 characters is 84, so the first screen an operator
//     sees always soft-wrapped and made a correct batch look mangled.
//
// Runes, not bytes: this copy is full of em dashes.
func TestEveryPromptAndButtonFitsThePane(t *testing.T) {
	txid := strings.Repeat("d", 64)

	t.Run("the batch summary", func(t *testing.T) {
		// A pubkey is the longest token this screen has, and the amount is the
		// widest prose.Sats renders.
		b := &config.Batch{Channels: []config.Channel{
			{Peer: strings.Repeat("0", 66), AmountSat: 21_000_000_00000000,
				Policy: policy.Policy{}},
			{Peer: strings.Repeat("0", 66), AmountSat: 1, Private: true,
				Policy: policy.Policy{}},
		}}
		for i, line := range strings.Split(Summary(b), "\n") {
			if n := len([]rune(line)); n > prose.PaneWidth {
				t.Errorf("line %d is %d runes, past the %d-column pane:\n%s",
					i+1, n, prose.PaneWidth, line)
			}
		}
	})

	t.Run("the blunt-abandon buttons", func(t *testing.T) {
		r := aRun(t)
		go func() {
			Confirmation(r, testGate)(context.Background(), abort.BluntRequest{
				Channel:   lnd.ChannelPoint{TxID: txid, Index: 4294967295},
				Rejection: "channel is not externally funded or not pending",
			})
		}()
		q := awaitQuestion(t, r)

		if len(q.Choices) != 2 {
			t.Fatalf("the confirmation offers %d choices", len(q.Choices))
		}
		for _, c := range q.Choices {
			if n := len([]rune(c.Label)); n > prose.PaneWidth {
				t.Errorf("the %q button's label is %d runes, past the %d-column "+
					"pane, so it wraps and outweighs the safe choice beside it:\n%s",
					c.Value, n, prose.PaneWidth, c.Label)
			}
			if strings.Contains(c.Label, txid) {
				t.Errorf("the %q button carries the full 64-character txid. The "+
					"prompt above it states the outpoint twice; what stops a stale "+
					"form answering about the wrong channel is the question id, not "+
					"the reader.", c.Value)
			}
		}
		// The abbreviation still has to identify the channel.
		yes := q.Choices[1]
		if !strings.Contains(yes.Label, txid[:12]) ||
			!strings.Contains(yes.Label, ":4294967295") {

			t.Errorf("the abbreviated outpoint does not identify the channel: %q",
				yes.Label)
		}
		// And the safe choice is first, so it is the one nearest the copy that
		// explains what the other one gives up.
		if q.Choices[0].Value != ChoiceNo {
			t.Errorf("the first button is %q, not the refusal", q.Choices[0].Value)
		}
	})
}

// awaitQuestion waits for the pending question without answering it.
func awaitQuestion(t *testing.T, r *server.Run) *server.Question {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if q := r.Pending(); q != nil {
			return q
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("nothing was asked")
	return nil
}

// TestTheDownloadNameSeparatesTheRoundsAndTheDevices is the webrun half of the
// file transport's naming, and the property is separation rather than a format.
//
// A batch's signing happens twice: the dress rehearsal signs a decoy over the
// same coins, and minutes later the real round signs the batch. Both rounds ask
// the same m devices, both hand over a base64 PSBT, and with the file transport
// both land in one Downloads folder. internal/signers' file handshake already
// keys its files on the round name for exactly this reason — a signed file left
// over from the rehearsal, picked up as the batch's, is a signature over the
// decoy, which internal/combine refuses at the worst possible moment with a
// message about a moved txid rather than about a leftover file.
//
// So every (round, device) pair has to produce its own name. That is what is
// asserted here; the shape of the name is internal/server's business.
func TestTheDownloadNameSeparatesTheRoundsAndTheDevices(t *testing.T) {
	seen := map[string]string{}

	for _, round := range []string{"rehearsal", "batch"} {
		for i, label := range []string{"cold1", "cold2"} {
			r := aRun(t)
			sign := Signer(r, SignRequest{
				Round: round, Label: label, Index: i + 1, Of: 2,
				Deadline: time.Now().Add(testGate),
			})

			done := make(chan struct{})
			go func() {
				defer close(done)
				sign(context.Background(), "cHNidP8BAAA=")
			}()
			q := answerWith(t, r, ChoiceSigned, "cHNidP8BAAA=")
			<-done

			if q.PayloadFilename == "" {
				t.Fatalf("%s/%s asked with no download name, so the file lands "+
					"under a name that distinguishes nothing", round, label)
			}
			if was, dup := seen[q.PayloadFilename]; dup {
				t.Errorf("%s/%s and %s both download as %q", round, label, was,
					q.PayloadFilename)
			}
			seen[q.PayloadFilename] = round + "/" + label

			// The two facts an operator matches the file against the screen by.
			for _, want := range []string{round, label} {
				if !strings.Contains(q.PayloadFilename, want) {
					t.Errorf("%s/%s downloads as %q, which does not name %q",
						round, label, q.PayloadFilename, want)
				}
			}
		}
	}
}
