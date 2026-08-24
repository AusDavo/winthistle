package prose

import (
	"fmt"
	"strings"
	"time"

	"github.com/AusDavo/winthistle/internal/journal"
)

// Progress is the state of a run that is still going: one row per channel, the
// receipt count, whether the funding transaction is still held, and what is left
// of the peers' window.
//
// It renders journal.Run because the journal is the only thing that knows. The
// alternative — a run loop reporting its own progress — would be a second copy
// of the arming rule, and the number this screen exists to show is exactly the
// one I-1 turns on. So the count is derived by looking at the rows, the same way
// MarkPending derives it.
//
// # What it deliberately does not show
//
// The mock this was built from had a per-channel "receipt 2m14s ago" column and
// peer aliases. Neither is in the journal: channels carry no timestamp of their
// own, and no alias — and the journal grows by new tables rather than new
// columns, so inventing either here would mean fabricating it from the run's
// single updated_at or from a batch file this package cannot see. A column that
// looks measured and is not is worse on this screen than no column, because this
// is the screen somebody reads to decide whether to wait.
//
// window is how long a peer holds a funding reservation — the caller's to supply,
// because it is a fact about the peer's build rather than ours and this package
// should not be the one asserting it.
func Progress(r *journal.Run, now time.Time, window time.Duration) string {
	if r == nil {
		return Para("No such run.")
	}

	var b strings.Builder
	// The phase, not the run id: every caller has just printed the id, and this
	// block sits directly under it.
	b.WriteString(fmt.Sprintf("phase: %s\n\n", stateColumn(r.State)))
	b.WriteString(liveChannels(r))
	b.WriteString("\n")
	b.WriteString(liveSummary(r, now, window))
	return b.String()
}

// liveChannels is one row per channel.
//
// Deliberately not numbered, and the reason is worth keeping. The plan document
// numbers channels in the batch file's order — "channel 2 to bitrefill" — and a
// number here would be read against that. It cannot be: journal.loadChannels
// orders by pending_chan_id, which is 32 random bytes, so these rows arrive in
// an order unrelated to the batch's. Numbering them would invite an operator to
// match row 2 against the sheet's channel 2 and get a different channel.
//
// Fixing the order rather than dropping the numbers would need a position column
// on the channels table, and the journal grows by new tables rather than new
// columns. The peer key and the amount are what identify a row here, which is
// what the recovery screen's table already relies on.
func liveChannels(r *journal.Run) string {
	if len(r.Channels) == 0 {
		return Para("No channels are journalled for this run yet, so its funding " +
			"streams have not opened. Nothing has been asked of any peer.")
	}

	var b strings.Builder
	for _, c := range r.Channels {
		b.WriteString(fmt.Sprintf("  %-18s %-16s %s\n",
			shortKey(c.PeerPubkey), c.State, Sats(c.AmountSat)))
		// The outpoint on its own line when there is one: it is 64 hex
		// characters plus an index and cannot be wrapped or shortened, and its
		// presence is itself the receipt.
		if c.Outpoint.TxID != "" {
			b.WriteString("    " + c.Outpoint.String() + "\n")
		}
	}
	return b.String()
}

// liveSummary is the three lines an operator actually watches.
func liveSummary(r *journal.Run, now time.Time, window time.Duration) string {
	var b strings.Builder

	pending, total := receipts(r)
	b.WriteString(fmt.Sprintf("  receipts      %d of %d\n", pending, total))
	b.WriteString("  funding tx    " + heldLine(r) + "\n")

	if left, show := peerWindow(r, now, window); show {
		b.WriteString("  peer window   " + left + "\n")
	}
	b.WriteString("\n")
	b.WriteString(Para(heldMeans(r, pending, total)))
	return b.String()
}

func receipts(r *journal.Run) (pending, total int) {
	for _, c := range r.Channels {
		switch c.State {
		case journal.ChanPending, journal.ChanAbandoned:
			// An abandoned channel reached pending before it was abandoned:
			// AbortTarget only abandons by outpoint, and an outpoint only exists
			// after chan_pending. Counting it keeps the receipt column from
			// going backwards during a teardown.
			pending++
		}
		total++
	}
	return pending, total
}

// heldLine is the live assertion of I-1, and it is the journal's answer rather
// than this package's.
//
// HELD means the transaction has not been handed to the network by us. The
// moment that stops being certain — the write that lands before the publish call
// — it stops saying HELD, because a screen that said it for one request longer
// than it was true would be the most dangerous line in the product.
func heldLine(r *journal.Run) string {
	switch r.State {
	case journal.StatePublishing:
		return "MAY BE PUBLIC — the publish call was made"
	case journal.StatePublished:
		return "PUBLISHED"
	default:
		return "HELD — nothing has been broadcast"
	}
}

func heldMeans(r *journal.Run, pending, total int) string {
	switch r.State {
	case journal.StatePublishing:
		return "The publish call was made and the journal recorded that before it " +
			"went out, so these bytes may be in a mempool. Nothing here will abort " +
			"this run: abandoning a pending channel whose funding transaction later " +
			"confirms strands its funds with no force-close path. Look for the txid " +
			"in a mempool or a block before touching anything."
	case journal.StatePublished:
		return "The transaction is public and every channel above reached its " +
			"receipt before it went out, which is the only condition under which " +
			"this tool publishes. Each one is recoverable by force-close from here, " +
			"whatever happens next."
	case journal.StateAborting, journal.StateAborted:
		return "This run is being taken apart, or was. Nothing was broadcast."
	}
	if pending == total && total > 0 {
		return "Every channel has its receipt, so each one is already recoverable " +
			"by force-close and the gate is open. The transaction is still held " +
			"here: publishing it is the next call, and it is the only one that " +
			"cannot be taken back."
	}
	return fmt.Sprintf("The funding transaction stays here until all %d channels "+
		"have their receipt. Until then nothing has been broadcast, nothing is at "+
		"risk, and stopping costs the ceremony rather than any coins.", total)
}

// peerWindow is what is left of the peers' ten minutes, and whether to show it
// at all.
//
// Two honesty problems, both of which change what is printed rather than being
// noted somewhere else.
//
// It is not our clock. A peer starts counting when it handles open_channel,
// which is before our journal row was written, so this estimate is optimistic by
// however long that round trip took. It is stated as "about".
//
// And it stops applying once a channel reaches chan_pending: the reservation has
// become a channel by then and no peer takes that back. A countdown on a fully
// armed batch would be inviting somebody to hurry for no reason, so it is not
// shown — the line disappears when the last receipt lands.
func peerWindow(r *journal.Run, now time.Time, window time.Duration) (string, bool) {
	if window <= 0 || r.CreatedAt.IsZero() {
		return "", false
	}
	switch r.State {
	case journal.StatePublishing, journal.StatePublished,
		journal.StateAborting, journal.StateAborted:
		return "", false
	}
	waiting := false
	for _, c := range r.Channels {
		if c.State == journal.ChanShimRegistered || c.State == journal.ChanVerified {
			waiting = true
		}
	}
	if !waiting {
		return "", false
	}

	left := r.CreatedAt.Add(window).Sub(now)
	if left <= 0 {
		return "may have lapsed — re-arming costs one more signing round", true
	}
	return fmt.Sprintf("about %s left for the channels still waiting",
		left.Round(time.Second)), true
}
