package prose

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/AusDavo/winthistle/internal/abort"
	"github.com/AusDavo/winthistle/internal/journal"
)

// The recovery screen.
//
// CLAUDE.md says this is the highest-stakes copy in the product and that it
// should be written before the happy path. It arrives after, which is a debt
// rather than a decision, and it is paid here.
//
// It is read by somebody who has just found a run that did not finish, on a node
// they may not have looked at for hours, possibly after a crash they did not see.
// Three things decide everything they do next, and all three come out of the
// journal:
//
//   - whether that run's transaction might be public. If it is, an abort is the
//     worst available action: abandoning a pending channel whose funding
//     transaction then confirms strands its funds with no force-close path. The
//     journal refuses one, and this screen has to say why in a way that does not
//     read as a technical hiccup.
//   - which channels reached chan_pending and which did not. The first are
//     abandoned by outpoint, the second cancelled by pending channel id, and the
//     difference is not cosmetic — a cancelled shim costs nothing and an
//     abandoned channel costs one of the peer's pending slots for 2016 blocks.
//   - what an abort will and will not clean up. It is local. The peers keep
//     their side, and telling somebody their node is clean when their peers are
//     not is the kind of half-truth that produces a second incident.
//
// Register, per CLAUDE.md: say what happened and what to do, and never
// euphemise a risk.

// RecoveryList is the screen shown at startup when the journal has runs that
// stopped somewhere they should not have.
func RecoveryList(runs []*journal.Run, now time.Time) string {
	if len(runs) == 0 {
		return Para("No unfinished runs. Nothing to recover.")
	}

	var b strings.Builder
	// "Unless something is driving one", rather than "they stopped". The journal
	// records what a run wrote, not whether it is still writing: a run is in here
	// from the moment its streams open until it is published or aborted, so a
	// batch being armed right now is in this list beside three that died in
	// August. Saying they stopped was a claim the journal cannot make, and it was
	// read next to a live run the first time this screen reached a browser.
	b.WriteString(Para(fmt.Sprintf(
		"%d unfinished run%s in the journal: neither published nor aborted. "+
			"Unless something is driving one right now, %s stopped somewhere %s "+
			"should not have and %s waiting for a decision.",
		len(runs), Plural(len(runs)), theyLower(len(runs)), theyLower(len(runs)),
		IsAre(len(runs)))))
	b.WriteString("\n")

	width := idColumn(runs)
	for _, r := range runs {
		b.WriteString(fmt.Sprintf("  %-*s  %-11s %s\n", width, r.ID,
			stateColumn(r.State), ageOf(r.UpdatedAt, now)))
		// The details of the row, all at one small indent. A txid is 64
		// characters and cannot be wrapped or abbreviated — it is the string the
		// operator pastes into Core — so the layout gives way to it rather than
		// the other way round, and the breakdown sits beside it rather than in a
		// column of its own.
		b.WriteString(channelBreakdown(r, rowIndent))
		if r.TxID != "" {
			b.WriteString(rowIndent + r.TxID + "\n")
		}
	}

	if pub := idsWhere(runs, mayBePublic); len(pub) > 0 {
		b.WriteString("\n")
		b.WriteString(Para(fmt.Sprintf(
			"%d of these reached the publish call: %s. A run in that state must "+
				"not be aborted, and nothing in this tool will abort one — its "+
				"funding transaction may be in a mempool or already in a block, and "+
				"abandoning a pending channel whose funding transaction then "+
				"confirms strands its funds with no force-close path. Read that run "+
				"on its own for what to do instead, which begins with looking for "+
				"the txid rather than touching anything.",
			len(pub), strings.Join(pub, ", "))))
	}
	return b.String()
}

// rowIndent is where a run's details sit under its own line in the list.
//
// Four spaces, and deliberately not a column under the state. It used to be
// seventeen — two spaces plus a fourteen-wide id field plus one — which stopped
// being right the day NewRunID started producing twenty-two characters: the
// state column moved right and the details stayed put, so the second line of
// every row lined up with the middle of the id above it. A browser is where that
// was finally seen.
const rowIndent = "    "

// idColumn is how wide the id column is: the widest id in the list, so the
// state column lines up across rows.
//
// Measured rather than pinned, because a journal id is whatever is on disk —
// NewRunID's twenty-two characters for both front doors, and whatever
// `winthistle run --id` was given otherwise. Clamped, because one hand-picked id
// must not pad every other row off the pane; a longer id simply pushes its own
// line out, the way a txid does.
func idColumn(runs []*journal.Run) int {
	const clamp = 24
	width := 0
	for _, r := range runs {
		if n := len([]rune(r.ID)); n > width {
			width = n
		}
	}
	if width > clamp {
		return clamp
	}
	return width
}

// Recovery is the screen for one unfinished run: what it is, and what an abort
// would do to it.
//
// Written to be read before anything is touched. Every figure in it is off the
// journal's own row; nothing here asks LND anything, so it is the same screen on
// a node that is down.
func Recovery(r *journal.Run, now time.Time) string {
	if r == nil {
		return Para("No such run.")
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("Run %s — %s, last touched %s.\n\n",
		r.ID, stateColumn(r.State), ageOf(r.UpdatedAt, now)))

	if mayBePublic(r) {
		return b.String() + recoveryPublished(r)
	}

	b.WriteString(Para(stateMeans(r.State)))
	b.WriteString("\n")
	b.WriteString(channelTable(r))

	pending, shims := split(r)
	if len(pending) == 0 && len(shims) == 0 {
		b.WriteString("\n")
		b.WriteString(Para(
			"Nothing of this run is still standing in LND. The journal row is a " +
				"record, not a job."))
	}

	b.WriteString("\n")
	b.WriteString(signerNote(r))
	b.WriteString("\n")
	b.WriteString(recoveryPlan(r, pending, shims))
	return b.String()
}

// recoveryPublished is the screen for a run that reached the publish call.
//
// The one screen in the product where the right action is to do nothing yet.
func recoveryPublished(r *journal.Run) string {
	var b strings.Builder

	b.WriteString(Para(
		"This run reached the publish call, so its funding transaction may be in " +
			"a mempool or already in a block. It will not be aborted."))
	b.WriteString("\n")
	b.WriteString(Para(
		"That is not caution about the journal being unsure. It is the one "+
			"combination in this design that loses money: abandoning a pending "+
			"channel removes it from this node, and if the funding transaction then "+
			"confirms, the outputs it pays are 2-of-2 outputs this node no longer "+
			"has a channel for. There is no force-close path back out of that, and "+
			"no way to rebuild one.") + "\n")

	// Counts, not amounts: prose.Table renders every figure as satoshis, which
	// is right for the reserve arithmetic and wrong for "three channels".
	b.WriteString(fmt.Sprintf("  %-24s  %d\n", "channels in this batch",
		len(r.Channels)))
	b.WriteString(fmt.Sprintf("  %-24s  %s\n", "raw transaction on disk",
		yesNo(r.RawTx != "")))
	b.WriteString("  funding transaction\n    " + orNone(r.TxID) + "\n")

	b.WriteString("\nWhat to do, in this order:\n")
	b.WriteString(Bullet(fmt.Sprintf(
		"Look for %s in the mempool or the chain. Core's getmempoolentry and "+
			"getrawtransaction both answer, and that answer decides everything else.",
		orNone(r.TxID))))
	b.WriteString(Bullet(
		"If it is there: nothing is wrong. Every channel in the batch was already " +
			"recoverable before it went out, and the settlement pass takes over — " +
			"confirmation, then the forwarding policy on each channel."))
	b.WriteString(Bullet(
		"If it is not there: re-broadcast it yourself, with bitcoin-cli " +
			"sendrawtransaction <hex>. The raw transaction is in this run's journal " +
			"row for exactly this reason. winthistle will not do it — the journal " +
			"refuses a second publish for a run that already reached the call — and " +
			"no_publish turned off LND's own rebroadcaster for this transaction."))
	b.WriteString(Bullet(
		"Do not abandon anything, and do not build a replacement. Replacing the " +
			"transaction changes every funding outpoint in it, and the peers hold " +
			"commitment signatures against the old ones."))
	return b.String()
}

// recoveryPlan says exactly what the abort will do.
func recoveryPlan(r *journal.Run, pending, shims []journal.Channel) string {
	var b strings.Builder

	b.WriteString("An abort of this run would:\n")

	if len(shims) > 0 {
		b.WriteString(Bullet(fmt.Sprintf(
			"Cancel %d funding shim%s. Free: nothing was finalized, so no peer "+
				"holds a commitment signature and nothing has to be undone. The "+
				"peer releases its own reservation within about eleven minutes.",
			len(shims), Plural(len(shims)))))
	}
	if len(pending) > 0 {
		b.WriteString(Bullet(fmt.Sprintf(
			"Abandon %d channel%s that reached chan_pending. Not free — see below.",
			len(pending), Plural(len(pending)))))
	}
	if len(pending) > 0 {
		b.WriteString("\n")
		b.WriteString(Para(
			"Abandoning is local. AbandonChannel removes the channel from this " +
				"node's database and tells the peer nothing at all, so the peer " +
				"keeps its side pending. It gives up after 2016 blocks from the " +
				"funding height — about two weeks — and until then it is holding " +
				"one of its own pending-channel slots for a channel that no longer " +
				"exists here."))
		b.WriteString("\n")
		b.WriteString(Para(
			"Two consequences worth knowing before you press it. Re-arming against " +
				"the same peer costs another of its slots, and a peer running LND's " +
				"default of one will refuse the next open with \"Number of pending " +
				"channels exceed maximum\". And the abandon will almost certainly " +
				"ask you to authorise LND's blunt flag — that is expected on this " +
				"path, not a warning sign. It asks once per channel, naming that " +
				"channel and quoting LND's own refusal, and it says there what the " +
				"flag gives up and what has already been checked on your behalf."))
		b.WriteString("\n")
		b.WriteString(Para(
			"Nothing was broadcast, so nothing was spent. The cost of this abort " +
				"is one more signing round and some of your peers' patience."))
	}
	return b.String()
}

// BluntConfirmation is what the operator is asked before LND's
// i_know_what_i_am_doing flag is used.
//
// This is the only place in the product where a human authorises something that
// could lose funds if the premise were wrong, so it says what the premise is and
// what already checked it.
func BluntConfirmation(req abort.BluntRequest) string {
	var b strings.Builder

	b.WriteString("Abandon this channel?\n\n    " + req.Channel.String() + "\n\n")
	b.WriteString(Para(fmt.Sprintf("LND refused the safe flag: %q", req.Rejection)))
	b.WriteString("\n")
	b.WriteString(Para(
		"That refusal is expected here and does not mean anything is wrong. The " +
			"safe flag, pending_funding_shim_only, decides \"shim funded\" by " +
			"looking for a thaw height, and a plain PSBT open — which is what this " +
			"tool produces — has none. So it declines every channel this tool " +
			"opens, and the fallback is the ordinary route rather than an edge case."))
	b.WriteString("\n")
	b.WriteString(Para(
		"The fallback gives up every protection in that call, including the one " +
			"that matters: it would remove a confirmed channel just as readily, and " +
			"a confirmed channel removed this way has its funds stranded with no " +
			"force-close path."))
	b.WriteString("\n")
	b.WriteString(Para(
		"So the missing half of LND's check has already been made here: this " +
			"channel was looked up in PendingChannels and it is pending. If it had " +
			"not been, you would not be being asked — there would be nothing worth " +
			"authorising."))
	b.WriteString("\n")
	b.WriteString(Para(
		"Answering yes removes this channel from this node. It does not tell the " +
			"peer, and it does not touch the chain."))
	return b.String()
}

// RecoveryOutcome is the screen after an abort has run.
//
// abort.Run never stops at the first failure, so a partial result is the normal
// shape of a bad night and this has to be able to say "these three worked, this
// one did not, here is what is left".
func RecoveryOutcome(r *journal.Run, rep *abort.Report, err error) string {
	var b strings.Builder

	if rep == nil {
		b.WriteString(Para(fmt.Sprintf("The abort of run %s did not start: %v",
			idOf(r), err)))
		return b.String()
	}

	b.WriteString(fmt.Sprintf("Abort of run %s\n\n", idOf(r)))

	b.WriteString(fmt.Sprintf("  %-24s  %d\n", "channels abandoned", len(rep.Abandoned)))
	blunt := 0
	for _, a := range rep.Abandoned {
		if a.UsedBlunt {
			blunt++
		}
	}
	if blunt > 0 {
		b.WriteString(fmt.Sprintf("  %-24s  %d\n", "of those, blunt flag", blunt))
	}

	cancelled, alreadyGone := 0, 0
	for _, c := range rep.Cancelled {
		if c.AlreadyGone {
			alreadyGone++
			continue
		}
		cancelled++
	}
	b.WriteString(fmt.Sprintf("  %-24s  %d\n", "shims cancelled", cancelled))
	if alreadyGone > 0 {
		b.WriteString(fmt.Sprintf("  %-24s  %d\n", "shims already gone", alreadyGone))
	}
	b.WriteString("\n")
	if rep.Clean() && err == nil {
		b.WriteString(Para(
			"Nothing of this run is left on this node. Nothing was broadcast, so " +
				"nothing was spent."))
		if len(rep.Abandoned) > 0 {
			b.WriteString("\n")
			b.WriteString(Para(fmt.Sprintf(
				"Your peers are not clean. %d channel%s %s abandoned here and "+
					"%s still pending on the other side, for 2016 blocks from the "+
					"funding height. Re-arming against those peers costs another of "+
					"their pending-channel slots.",
				len(rep.Abandoned), Plural(len(rep.Abandoned)),
				WasWere(len(rep.Abandoned)), IsAre(len(rep.Abandoned)))))
		}
		if alreadyGone > 0 {
			b.WriteString("\n")
			b.WriteString(Para(fmt.Sprintf(
				"%d shim%s already gone before this ran. That is the expected "+
					"result of a second abort over the same run, and of a peer that "+
					"timed the reservation out first. It is not a failure.",
				alreadyGone, Plural(alreadyGone))))
		}
		return b.String()
	}

	b.WriteString(Para(fmt.Sprintf(
		"This abort did not fully complete. %d step%s failed; everything else above "+
			"did happen, and none of it has to be done again.",
		len(rep.Failures), Plural(len(rep.Failures)))))
	b.WriteString("\n")
	for _, f := range rep.Failures {
		b.WriteString(Bullet(failureLine(f)))
	}
	b.WriteString("\n")
	b.WriteString(Para(
		"Running the recovery again is safe and is the right next step. Every step " +
			"in it is written to be safe to call twice: an already-cancelled shim " +
			"reports itself as already gone, an already-abandoned channel is not an " +
			"error in LND, and Core's lock release only ever frees what Core is " +
			"actually holding."))
	b.WriteString("\n")
	b.WriteString(Para(
		"The run stays marked as aborting until it completes cleanly, so it will " +
			"still be in this list next time."))
	return b.String()
}

// failureLine renders one failure, naming the sentinel where there is one so the
// operator reads a cause rather than a stack of wrapped verbs.
func failureLine(err error) string {
	switch {
	case errors.Is(err, abort.ErrNotPending):
		return "A channel LND says is not pending was refused, and no confirmation " +
			"was offered for it. That is the refusal working: the blunt flag would " +
			"have removed it, and a confirmed channel removed that way has its funds " +
			"stranded. Look at that channel before doing anything else — " + err.Error()
	case errors.Is(err, abort.ErrBluntNotConfirmed):
		// "It is still pending here and on the peer" — #27. The first half is
		// what abort read, in this node's PendingChannels. The second was an
		// inference, and a sound one, asserted bare on the screen this file's
		// own comment calls the highest-stakes copy in the build. What is
		// established is narrower and is enough: nothing was done, so nothing
		// changed anywhere. Pointing at the paragraph that shows the working was
		// the other candidate and is not available — that paragraph is on the
		// Recovery screen and this is RecoveryOutcome, and a screen may not point
		// at a section it does not have (TestNoScreenPointsBelowItself).
		return "An abandon needed LND's blunt flag and it was not authorised, so " +
			"nothing was done to that channel. This node still has it pending, " +
			"which is what was read. And an abandon tells the peer nothing either " +
			"way, so this refusal changed nothing on the peer's side: whatever it " +
			"was holding, it still is — " + err.Error()
	case errors.Is(err, abort.ErrNoShim):
		return "There was no funding intent to cancel — " + err.Error()
	default:
		return err.Error()
	}
}

// ---- small helpers ----

// stateMeans is the one paragraph that renders a run's state.
//
// Three of these states are written *before* the work they name — arming when
// the streams open, signing before the wallet is asked, aborting before the
// first call of the abort — because a crash must leave artifacts rather than
// mystery. So a run holding any of them may be running right now, in another
// terminal or in this process's own teardown, and this paragraph used to say
// otherwise: "streams *were* open", "*had* gone out to be signed", and worst,
// "an abort of this run was started and did not finish". That last is #24 one
// state over — journal.go:86 and docs/design.html both hedge the same claim
// correctly, and only this screen asserted it.
//
// The hedge is here rather than at the caller, which is where #24 put it, and
// the difference is that here the narrower question can be answered honestly.
// journal.Unfinished could not say which of its runs had stopped: a run that
// died mid-arming and one being armed write identical rows, so the hedge had to
// go where a sentence could carry it. This function is handed the state itself,
// and each state has an honest reading — "in progress, or interrupted partway"
// is exactly what aborting establishes. A blanket paragraph at the caller would
// also over-apply: armed and published are *not* written ahead of their work,
// and hedging them would weaken two sentences that are simply true.
//
// It renders one state per screen, so a clause in each of the three costs the
// reader nothing.
func stateMeans(s journal.State) string {
	switch s {
	case journal.StateArming:
		return "Funding streams are open and no channel has reached chan_pending. " +
			"That state is written when the streams open and does not move again " +
			"until the gate does, so it says where this run got to and not whether " +
			"something is driving it right now. Whatever is cancellable for free is " +
			"listed below and most of it will be: a stream that only registered its " +
			"shim costs nothing to release. Read the channel list rather than this " +
			"line — psbt_verify starts the funding flow now, so a run can hold both " +
			"kinds at once."
	case journal.StateArmed:
		return "Every channel in this batch reached chan_pending, which means every " +
			"one of them is already recoverable by force-close — and the publish " +
			"never happened. Nothing was signed yet either, so nothing existed for " +
			"anybody to broadcast. Taking it apart is n abandons, and LND wants its " +
			"blunt flag for each one."
	case journal.StateSigning:
		return "Every channel in this batch reached chan_pending and the unsigned " +
			"transaction is out with the signing wallet. That state is written " +
			"before the wallet is asked for anything and holds until a signature " +
			"comes back, so it says where this run got to and not whether somebody " +
			"is at the wallet right now. Nothing was broadcast — the " +
			"signing wallet may hold a complete transaction, and it front-runs " +
			"nothing, because each of these channels was already recoverable before " +
			"it was asked. Taking it apart costs what an armed run costs: each " +
			"channel has to be abandoned, and LND wants its blunt flag for each one."
	case journal.StateAborting:
		return "An abort of this run is in progress, or one was interrupted " +
			"partway. That state is written before the first call it describes, so " +
			"a winthistle recover running in another terminal right now writes " +
			"exactly this row, and so does an abort that stopped halfway through " +
			"one. What follows is what is left, not what there was."
	case "":
		// Unreachable through this journal — see stateColumn — and written down
		// anyway, because the default branch below would render it as a sentence
		// with the subject missing. A blank state must not read as a run that
		// never started: the channel list underneath is the only fact here.
		return "The journal has no state at all for this run, which should not be " +
			"possible: the column is NOT NULL and every writer sets it. Read the " +
			"channels below as the only fact on this screen, and do not read the " +
			"blank as a run that never got anywhere."
	default:
		return fmt.Sprintf("The journal has this run as %s.", s)
	}
}

// channelBreakdown is the per-state counts under one run in the list, wrapped.
//
// It breaks between items and never inside one. "12 abandoned" split across a
// line break reads as a count of twelve of something unnamed followed by a state
// with no number, and this line exists to say exactly which channels are on
// which side of the abort.
//
// It has to wrap at all because a run in aborting with a partial abort behind it
// can carry channels in all five states at once — the normal shape of a bad
// night, and the shape RecoveryOutcome tells the operator to expect. Even at one
// channel per state that came out at 83 columns against a 78-column pane, and on
// a batch of a few dozen at 88.
func channelBreakdown(r *journal.Run, indent string) string {
	counts := map[journal.ChannelState]int{}
	for _, c := range r.Channels {
		counts[c.State]++
	}
	var parts []string
	for _, st := range []journal.ChannelState{
		journal.ChanShimRegistered, journal.ChanVerified, journal.ChanPending,
		journal.ChanAbandoned, journal.ChanCancelled,
	} {
		if n := counts[st]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, st))
		}
	}
	if len(parts) == 0 {
		// Not a blank line. A run with no channels journalled is a run that
		// stopped before Begin wrote any, and saying so beats an empty column
		// that reads as a row the renderer gave up on.
		return indent + "no channels\n"
	}

	var b strings.Builder
	line := indent
	for i, part := range parts {
		if i < len(parts)-1 {
			part += ","
		}
		switch {
		case line == indent:
			line += part
		case len([]rune(line))+1+len([]rune(part)) > PaneWidth:
			b.WriteString(line + "\n")
			line = indent + part
		default:
			line += " " + part
		}
	}
	b.WriteString(line + "\n")
	return b.String()
}

// stateColumn keeps a blank column from reading as a state.
//
// The runs table has state NOT NULL and every writer sets it, so this should be
// unreachable — which is the reason to render it as something rather than as
// eleven spaces. A row whose state column is empty would otherwise be the one
// row on the screen that says nothing at all about where its run got to.
func stateColumn(s journal.State) string {
	if s == "" {
		return "(no state)"
	}
	return string(s)
}

func channelTable(r *journal.Run) string {
	var b strings.Builder
	for _, c := range r.Channels {
		where := c.Outpoint.TxID
		if where == "" {
			where = "no outpoint yet"
		} else {
			where = c.Outpoint.String()
		}
		b.WriteString(fmt.Sprintf("  %-18s %-16s %s\n", shortKey(c.PeerPubkey),
			c.State, Sats(c.AmountSat)))
		b.WriteString("    " + where + "\n")
	}
	return b.String()
}

// split divides the run's channels the way the abort divides them.
func split(r *journal.Run) (pending, shims []journal.Channel) {
	for _, c := range r.Channels {
		switch c.State {
		case journal.ChanPending:
			pending = append(pending, c)
		case journal.ChanShimRegistered, journal.ChanVerified:
			shims = append(shims, c)
		}
	}
	return pending, shims
}

// signerNote is the only place a journal.SignerState is rendered, which is why
// the counts have to add up to len(r.Signers) and why there is a default branch.
//
// The switch used to have three arms and no default, so a state it did not know
// about was counted in nothing: two signers at an unrecognised value rendered as
// "0 returned a partial, 0 still awaited, 0 declined" — a run that reads as
// though nobody had been asked anything, on the highest-stakes screen in the
// product. The journal has no CHECK constraint and no migration table, so a value
// this function has not heard of is a thing that can happen: an older build's
// journal, or a newer one's.
func signerNote(r *journal.Run) string {
	if len(r.Signers) == 0 {
		// "No signer had been asked for anything when this stopped" — #30 item 3.
		// The second half was a claim about the run, not about the signers, and
		// this screen renders for a run that may be going right now. The first
		// half survives because the narrower question *can* be answered here:
		// run.sign writes the SignerAwaiting row before it calls
		// SigningWallet.Signed, so no row at all means step 7 was never entered.
		return Para("No signer row was written for this run, and step 7 writes " +
			"one before it asks a wallet for anything — so nothing has been asked " +
			"of a wallet.")
	}
	var awaiting, signed, partial, declined, unknown int
	for _, s := range r.Signers {
		switch s.State {
		case journal.SignerAwaiting:
			awaiting++
		case journal.SignerSigned:
			signed++
		case journal.SignerPartial:
			partial++
		case journal.SignerDeclined:
			declined++
		default:
			unknown++
		}
	}

	var counts []string
	if signed > 0 {
		counts = append(counts, fmt.Sprintf("%d signed", signed))
	}
	if partial > 0 {
		// Nothing in this build writes this state: it is a journal an earlier build
		// wrote, from a CPFP child's round or from a batch round of before the
		// inversion. Said in its own words rather than folded into "signed", because
		// a partial signature is not a transaction.
		counts = append(counts, fmt.Sprintf("%d returned a partial signature", partial))
	}
	if awaiting > 0 {
		counts = append(counts, fmt.Sprintf("%d still awaited", awaiting))
	}
	if declined > 0 {
		counts = append(counts, fmt.Sprintf("%d marked declined", declined))
	}
	if unknown > 0 {
		counts = append(counts, fmt.Sprintf("%d in a state this build does not "+
			"recognise", unknown))
	}

	out := Para(fmt.Sprintf(
		"Signers: %s. No key material, PSBT or descriptor is stored in the journal "+
			"— a signer here is a label and a state.", andList(counts)))

	if declined > 0 {
		// "%d declined" was the whole of it, and it said a wallet refused.
		// Nothing in this build can observe a refusal — a file transport has no
		// channel through which a wallet says no — and since issue #25 nothing
		// writes the value either. So every row this branch will ever see was
		// written by the frame that fix removed, which recorded it on *any*
		// failure of step 7. It stays renderable, because those rows are on
		// operators' disks; what it may not do is name a cause that was never
		// observed. See journal.SignerDeclined, which documents the same thing
		// at the value.
		out += "\n" + Para(
			"That declined count comes off a journal an earlier build wrote, and it "+
				"never meant a wallet said no. The old step 7 recorded it whenever "+
				"the signing step failed at all — a file that never appeared, one "+
				"that could not be read, a txid that had moved, or Ctrl-C. Read it "+
				"as a signing step that did not finish, and the error the run "+
				"printed at the time for why.")
	}
	return out
}

// andList renders a list the way a sentence wants it.
func andList(items []string) string {
	switch len(items) {
	case 0:
		return "none"
	case 1:
		return items[0]
	default:
		return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
	}
}

func mayBePublic(r *journal.Run) bool {
	return r.State == journal.StatePublishing || r.State == journal.StatePublished
}

// idsWhere names the runs a warning is about, rather than saying "at least one
// of these" and leaving the reader to work out which.
func idsWhere(runs []*journal.Run, pred func(*journal.Run) bool) []string {
	var out []string
	for _, r := range runs {
		if pred(r) {
			out = append(out, r.ID)
		}
	}
	return out
}

func ageOf(t time.Time, now time.Time) string {
	if t.IsZero() {
		return "at an unknown time"
	}
	d := now.Sub(t)
	if d < 0 {
		return t.Format(time.RFC3339)
	}
	return d.Round(time.Second).String() + " ago"
}

func idOf(r *journal.Run) string {
	if r == nil {
		return "(unknown)"
	}
	return r.ID
}

func orNone(s string) string {
	if s == "" {
		return "(none journalled)"
	}
	return s
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "NO — there is nothing to re-broadcast"
}

func shortKey(pubkey string) string {
	if len(pubkey) <= 16 {
		return pubkey
	}
	return pubkey[:16] + "…"
}

func theyLower(n int) string {
	if n == 1 {
		return "it"
	}
	return "they"
}
