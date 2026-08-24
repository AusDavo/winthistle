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

	if len(r.Locks) > 0 {
		held := 0
		for _, l := range r.Locks {
			if !l.Released {
				held++
			}
		}
		if held > 0 {
			b.WriteString("\n")
			b.WriteString(Para(fmt.Sprintf(
				"%d of this run's coin%s %s still locked in Core. A locked coin is "+
					"not lost and not spent — it is a wallet that will decline to "+
					"spend its own money and not say why. Core's locks live in "+
					"memory, so a restart clears them all at once and this list "+
					"becomes stale rather than wrong.",
				held, Plural(held), IsAre(held))))
		}
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
		"If it is not there: re-broadcast it. The raw transaction is in this run's " +
			"journal row for exactly this reason. no_publish also turns off LND's " +
			"own rebroadcaster for this transaction, so nothing else is going to do " +
			"it."))
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
	held := 0
	for _, l := range r.Locks {
		if !l.Released {
			held++
		}
	}
	if held > 0 {
		b.WriteString(Bullet(fmt.Sprintf(
			"Release %d coin lock%s in Core.", held, Plural(held))))
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
	b.WriteString(fmt.Sprintf("  %-24s  %d\n", "coin locks freed", len(rep.LocksFreed)))

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
		return "An abandon needed LND's blunt flag and it was not authorised, so " +
			"nothing was done to that channel. It is still pending here and on the " +
			"peer — " + err.Error()
	case errors.Is(err, abort.ErrNoShim):
		return "There was no funding intent to cancel — " + err.Error()
	default:
		return err.Error()
	}
}

// ---- small helpers ----

func stateMeans(s journal.State) string {
	switch s {
	case journal.StateArming:
		return "Funding streams were open and nothing had been finalized. " +
			"Everything about this run is cancellable for free: no peer holds a " +
			"commitment signature, no transaction exists that anybody could " +
			"broadcast, and the cost of taking it apart is nothing at all."
	case journal.StateSigning:
		return "Every stream had verified the unsigned transaction, so LND has " +
			"committed to the funding outpoints and the transaction was out with " +
			"the signers. Still cancellable for free — psbt_verify commits LND to " +
			"an outpoint, not to a channel, and shim_cancel still works after one."
	case journal.StateArmed:
		return "Every channel in this batch reached chan_pending, which means every " +
			"one of them is already recoverable by force-close — and the publish " +
			"never happened. This is the gate working: the batch got as far as it " +
			"is allowed to get without going out, and stopped."
	case journal.StateAborting:
		return "An abort of this run was started and did not finish. What follows " +
			"is what is left, not what there was."
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

func signerNote(r *journal.Run) string {
	if len(r.Signers) == 0 {
		return Para("No signer had been asked for anything when this stopped.")
	}
	var awaiting, partial, declined int
	for _, s := range r.Signers {
		switch s.State {
		case journal.SignerAwaiting:
			awaiting++
		case journal.SignerPartial:
			partial++
		case journal.SignerDeclined:
			declined++
		}
	}
	return Para(fmt.Sprintf(
		"Signers: %d returned a partial, %d still awaited, %d declined. No key "+
			"material, PSBT or descriptor is stored in the journal — a signer here "+
			"is a label and a state.", partial, awaiting, declined))
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
