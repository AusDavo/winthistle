package server

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// A run asks four things, and this is the whole of how a browser answers them.
//
// internal/run's four callback seams — rehearsal.Signer, abort.Confirmation,
// bump.Approve and setup.Ask — are the places a straight-line blocking function
// stops and waits for a person. Decision 1 says the function stays straight-line
// and the handlers feed the seams over channels; this file is that channel, and
// it is deliberately transport-neutral. It knows nothing about PSBTs, blunt
// flags, address checks or fee targets. It knows: a run has stopped, here is the
// text it stopped on, here are the answers it will take, and here is when the
// answer stops being worth waiting for.
//
// The adapters that turn a Question into one of the four seam types live in
// internal/webrun, because each of them owns a verdict this package must not
// invent — see "What an unanswered question means" below.
//
// # A browser can stop answering, and the bound is a clock the product already has
//
// A tab that closes mid-question is not detectable (decision 2), so Ask cannot
// wait for a disconnection. It waits for a deadline instead, and the deadline is
// not a transport timeout: it is limits.abort_after_signing_seconds, the 5:00
// gate rehearsal.Gate measures a signing round against.
//
// Two clocks bound this product and only one of them is ours:
//
//   - the 5:00 gate is ours. We set it, we measure against it, and
//     rehearsal.Gate refuses to arm a batch whose rehearsal was slower. It is
//     enforceable, so it is the one a bound can be built from.
//   - the peers' ten minutes are theirs. LND's pruneZombieReservations skips
//     PSBT reservations, so our node never expires one: the peer's own sweeper
//     ends it, on the peer's clock, and nothing here can shorten or extend it.
//
// What stops the second becoming the first is arithmetic that is already
// enforced: config.Validate refuses a configuration whose AbortAfterSigning is
// greater than or equal to rehearsal.PeerWindow. So a deadline built from the
// gate is inside the peers' window by construction, and this package cannot
// reach the ten minutes anyway — Question.Deadline is a time, computed by
// whoever read the config, and Ask refuses a question that arrives without one.
// See TestAQuestionWithNoClockIsRefused.
//
// A bound derived from the gate can expire early relative to the peers, never
// late. That is the safe direction: expiring early costs one more ceremony —
// nothing is published while a question is pending — and expiring late costs a
// wait on peers who have already gone.
//
// # What an unanswered question means
//
// ErrUnanswered, and nothing else. This package does not decide what a run
// should do about it, because each of the four seams has a different safe
// verdict and only the seam knows which:
//
//   - setup.Ask returns NotAnswered, which records nothing. That is the highest
//     stakes of the four: a comparison nobody made must never reach the setups
//     table as a verdict.
//   - abort.Confirmation returns false, which is what a nil Confirmation means:
//     never escalate to i_know_what_i_am_doing.
//   - bump.Approve returns false, which releases the child's coin lock.
//   - rehearsal.Signer returns an error, which fails the round — and for the
//     rehearsal round that is the same verdict rehearsal.Gate would have
//     reached anyway.
//
// Every one of those is the answer a nil seam would have given. An abandoned
// browser is therefore indistinguishable from no browser, which is the property
// worth having.

// ErrUnanswered is a question whose deadline passed with nobody answering it.
//
// Not an error about the transport: the deadline is the 5:00 gate, so this is
// the run finding out that the signing round is over budget. internal/webrun
// maps it to each seam's safe verdict.
var ErrUnanswered = errors.New("nobody answered before the signing gate expired")

// ErrNoClock is a question with no deadline. See the file comment: the number
// comes from the configuration, so it comes from the caller, and a question that
// arrived without one would wait forever.
var ErrNoClock = errors.New("a question with no deadline would wait forever, " +
	"and the bound has to be limits.abort_after_signing_seconds rather than a " +
	"timeout invented here")

// ErrStaleQuestion is an answer to something else: the back button, a second
// tab that was showing an older screen, or a form submitted twice.
var ErrStaleQuestion = errors.New("that answer is to a question this run is no " +
	"longer asking")

// ErrAlreadyAsking means a second question arrived while one was pending. It
// cannot happen from run.Do, which is straight-line, and it is a refusal rather
// than a queue so that it stays that way.
var ErrAlreadyAsking = errors.New("this run is already waiting on an answer")

// Choice is one button.
type Choice struct {
	// Value is what the form posts back. It is matched exactly.
	Value string

	// Label is the button's text.
	Label string
}

// Question is one thing a run has stopped to ask.
//
// Prompt is operator copy and goes through screen() like every other string in
// this UI, so decision 3's oracle covers it. Payload is not copy — it is a
// base64 PSBT, which is machine data the operator copies out rather than reads —
// and it is the one thing on the page that is not prose.
type Question struct {
	// ID is assigned by Ask, and it is what stops a stale form from answering a
	// question the run has moved past. A counter, per run: the token already
	// gates who can post at all, and what this has to separate is one question
	// from the next rather than one stranger from another.
	ID string

	// Prompt is the text the run stopped on, in the register the rest of the
	// copy is written in: what happened, and what to do.
	Prompt string

	// Payload is a base64 PSBT, when there is one. Empty otherwise.
	//
	// A read-only field the operator copies out of, and a field they paste the
	// signed packet back into, is the whole of the browser transport in this
	// slice. The file handshake and the animated QR that belong around it are
	// item 1.4, and they are additions to this same Question rather than a
	// different seam.
	Payload string

	// PayloadLabel names the payload on the screen.
	PayloadLabel string

	// Reply, when non-empty, is the label of a free-text field the answer needs.
	// It is how a signed packet gets back.
	Reply string

	// Choices are the buttons the screen renders. They are not a filter: a
	// submission naming something else reaches the seam verbatim, and the seam's
	// own adapter decides what it means.
	//
	// That is deliberate, and it puts one obligation on every adapter: match the
	// affirmative explicitly, never the negative. `a.Choice == yes` is safe for
	// any string a form could carry; `a.Choice != no` would read a typo as
	// consent. All four adapters in internal/webrun are written the first way,
	// and their tests enumerate the garbage.
	Choices []Choice

	// Deadline is when the answer stops being worth waiting for. Required: see
	// ErrNoClock.
	Deadline time.Time

	// Asked is set by Ask.
	Asked time.Time
}

// Answer is what the operator said.
//
// The zero value is a deliberate value rather than a missing one: it is "the
// form came back with nothing recognisable in it", which every seam has a safe
// reading of.
type Answer struct {
	// Choice is the Value of the button pressed, or "" if none matched.
	Choice string

	// Text is the free-text field, when the question asked for one.
	Text string
}

// Ask puts a question in front of whoever is attached and blocks until it is
// answered, the deadline passes, or the run's context ends.
//
// It is called from the run's own goroutine — run.Do is straight-line and this
// is where it stops — so blocking here is the point. The context is the run's,
// never a request's: a request context ends when the response does, and decision
// 2 is that a response ending means nothing.
func (r *Run) Ask(ctx context.Context, q Question) (Answer, error) {
	if q.Deadline.IsZero() {
		return Answer{}, ErrNoClock
	}

	replies := make(chan Answer, 1)
	r.mu.Lock()
	if r.pending != nil {
		r.mu.Unlock()
		return Answer{}, ErrAlreadyAsking
	}
	r.seq++
	q.ID = strconv.Itoa(r.seq)
	q.Asked = time.Now()
	r.pending, r.replies = &q, replies
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		r.pending, r.replies = nil, nil
		r.mu.Unlock()
	}()

	// A deadline already past is refused rather than raced: with n devices in a
	// round sharing one deadline, the second device can legitimately be asked
	// after the budget is gone, and "the round is over" is a better thing to say
	// than a zero-length wait.
	timer := time.NewTimer(time.Until(q.Deadline))
	defer timer.Stop()

	select {
	case a := <-replies:
		return a, nil
	case <-timer.C:
		return Answer{}, fmt.Errorf("%w (asked at %s, gate %s)", ErrUnanswered,
			q.Asked.Format(time.TimeOnly), q.Deadline.Sub(q.Asked).Round(time.Second))
	case <-ctx.Done():
		return Answer{}, ctx.Err()
	}
}

// Pending is the question this run is waiting on, or nil. A copy, so a screen
// cannot mutate what the run is asking.
func (r *Run) Pending() *Question {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending == nil {
		return nil
	}
	q := *r.pending
	return &q
}

// Reply hands one answer to the run, if it is still the question being asked.
//
// The id is checked rather than trusted: a browser holding an older screen — the
// back button, a second tab, a double submit — must not be able to answer the
// question that replaced the one it was showing. That is the same protection the
// per-channel shape of abort.Confirmation asks for, in the layer below it.
func (r *Run) Reply(id string, a Answer) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending == nil || r.pending.ID != id {
		return ErrStaleQuestion
	}
	select {
	case r.replies <- a:
		return nil
	default:
		// Buffered, and cleared by the only reader on the way out, so a second
		// send can only mean a double submit that beat the redirect.
		return ErrStaleQuestion
	}
}
