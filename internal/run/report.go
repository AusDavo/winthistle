package run

import (
	"fmt"
	"io"
	"strings"

	"github.com/AusDavo/winthistle/internal/arm"
	"github.com/AusDavo/winthistle/internal/prose"
)

// Recipient is one output the wallet is asked to pay: an address, an amount, and
// what it is for. It is rendered twice — recipientTable for the operator's screen
// and recipientsCSV for their wallet — and both come off the same slice, so the
// two cannot disagree.
type Recipient struct {
	// Label is what this output is, in the operator's terms — "channel 2 acinq",
	// or the anchor reserve. It is for the screen; nothing matches on it.
	Label string

	Address   string
	AmountSat int64
}

// recipientsOf is every output the batch has to pay for itself.
//
// Every output the plan will name except the change, which is theirs. The reserve
// top-up is in the list because it is an output the batch has to pay and a
// transaction missing it fails at step 5 — it is not a channel, so it is easy to
// read past in a list that otherwise looks like the batch file.
func recipientsOf(streams *arm.Streams, p *prepared) []Recipient {
	byKey := aliases(p.facts)
	out := make([]Recipient, 0, len(streams.All)+1)
	for i, st := range streams.All {
		label := byKey[strings.ToLower(st.Peer)]
		if label == "" {
			label = short(st.Peer)
		}
		out = append(out, Recipient{
			Label:     fmt.Sprintf("channel %d  %s", i+1, label),
			Address:   st.FundingAddress,
			AmountSat: st.FundingAmount,
		})
	}
	if p.topUp != nil {
		out = append(out, Recipient{
			Label:     "anchor reserve",
			Address:   p.topUp.Address,
			AmountSat: p.topUp.AmountSat,
		})
	}
	return out
}

// recipientTable prints them: what and how much on one line, the address alone
// on the next.
//
// The address gets a line of its own because it does not fit beside anything. A
// P2WSH funding address is 62 characters and the pane is 78, so a label column
// wide enough to read pushes it past the edge — and a wrapped address is one an
// operator has to reassemble by hand at the one step where a wrong character
// costs a channel. Alone on a line it is also one double-click to select.
//
// Printed in full for the same reason, and an abbreviated address is one somebody
// might reconstruct. That the recipients also go out as a CSV does not soften any
// of this: the file is how they get into the wallet, and this table is how the
// operator sees which peer is getting which output — the attribution the program
// exists for, which no other screen shows. Amounts aligned, so a swapped pair is
// visible, which is one of the things step 5 catches after the fact and this is
// the chance to catch before it.
func recipientTable(rs []Recipient) string {
	width := 0
	for _, r := range rs {
		if n := len([]rune(r.Label)); n > width {
			width = n
		}
	}
	var b strings.Builder
	for _, r := range rs {
		// prose.Sats carries its own unit. It was given another one here for one
		// commit, which printed "250,000 sat sat" on the one screen an operator
		// reads addresses off.
		fmt.Fprintf(&b, "  %-*s  %14s\n", width, r.Label, prose.Sats(r.AmountSat))
		fmt.Fprintf(&b, "      %s\n\n", r.Address)
	}
	return b.String()
}

// satsPerBTC is the divisor, and the only place in this build that the two units
// meet. Nothing else converts, because nothing else needs to: the batch file is
// in sats, LND is in sats, the plan is in sats, and the one consumer that is not
// is a text file read by another program.
const satsPerBTC = 100_000_000

// recipientsCSV is the same recipients again, for Sparrow's Send to Many → Load
// CSV. Three columns, address first, amount second, label third, which is the
// order SendToManyDialog reads them in — get(0), get(1), get(2) — and the label
// it reads goes straight onto the Payment, so the peer alias lands on the output.
//
// # Why this is a second renderer and not a shared one
//
// recipientTable prints 250,000 sat, with a grouping separator, because a human
// reading a terminal is helped by one. This file must not have one under any
// circumstances, and the two facts are not in tension — they are two audiences.
// Unifying them would mean making the table worse to serve a file format.
//
// # BTC, eight decimal places, and the reason is which way the failure falls
//
// The amount column carries no unit and Sparrow reads it in whatever unit the
// operator's preference is set to, which this program cannot see. The two
// readings are 10^8 apart, so one of them is always wrong — the question is only
// which wrong is survivable. Measured on Sparrow 2.5.3, both ways round (issue
// #15):
//
//   - Sat integers loaded in BTC mode become 10^8 too large, silently. 250000
//     loads as 250000.00000000 BTC. That is under the supply cap, so nothing on
//     screen looks absurd, and Sparrow only objects much later at coin selection
//     with insufficient funds — by which point the operator has stopped reading
//     amounts.
//   - BTC decimals loaded in sats mode throw on Long.parseLong, and Sparrow skips
//     the row. If every row goes, which it does, Sparrow says "No recipients
//     found. Use a CSV file with three columns, and ensure amounts are in sats."
//
// The second names its own cause. The first does not. So this emits BTC.
//
// The formatting is integer arithmetic. A float and a %.8f gives the same answer
// for every value an int64 of sats can hold — they are all inside float64's
// 53-bit mantissa — so this is not a bug that was found, it is an argument that
// does not have to be made.
//
// # No grouping separator, quoted or not
//
// Sparrow strips the grouping separator rather than rejecting it, so a quoted
// "250,000" and a bare 250000 converge on the same wrong value. An *unquoted*
// 250,000 is worse still: it shifts the columns, so get(1) is 250, get(2) is 000,
// and the alias is discarded — a row that loads clean and pays 250 BTC to an
// unlabelled output. That is what a generator lifting amounts off the table above
// would have emitted.
//
// # The label is always quoted
//
// Peer aliases contain commas — ACINQ, Inc. — and an unquoted one shifts the
// columns exactly as above. Quoted unconditionally rather than when needed, so
// there is no case analysis to get wrong, and a stray quote inside an alias is
// doubled per RFC 4180. A carriage return or newline in an alias becomes a space:
// a quoted line break is legal CSV, but a row that spans two lines in a file the
// operator may open in a text editor is not worth the fidelity.
//
// # The header row
//
// Skipped by Sparrow, and by accident rather than by design — the amount throws
// NumberFormatException and the catch is commented "ignore and continue -
// probably a header line". Confirmed on a live load: the header row vanished. It
// is here because the file cannot otherwise say what unit it is in, and a human
// who opens it should not have to guess.
//
// None of this is a check on anything. Every failure above is caught at step 5,
// by the same verifier that catches a mis-paste: a 10^8 amount is WrongAmount, a
// dropped row is MissingOutput, and a shifted column is both. This saves typing;
// it does not move the trust boundary an inch.
func recipientsCSV(rs []Recipient) []byte {
	var b strings.Builder
	b.WriteString("address,amount_btc,label\n")
	for _, r := range rs {
		fmt.Fprintf(&b, "%s,%s,%s\n", r.Address, btcAmount(r.AmountSat), csvQuoted(r.Label))
	}
	return []byte(b.String())
}

// btcAmount renders sats as BTC with eight decimal places and no separators.
func btcAmount(sat int64) string {
	return fmt.Sprintf("%d.%08d", sat/satsPerBTC, sat%satsPerBTC)
}

// csvQuoted wraps a field in quotes, always, and doubles any quote inside it.
func csvQuoted(s string) string {
	s = strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// reportArmed is the screen at the last reversible moment: every channel is
// recoverable, the transaction is signed, and nothing has been broadcast.
//
// The gate itself opened earlier — step 6, before the signing round, which is
// where the armed window says so as it happens. This screen is the summary the
// operator reads with one call left: everything on it is reversible, and
// everything after it is not, so it says which channels exist, where their funds
// will be, and what the one remaining action does.
func reportArmed(w io.Writer, armed *arm.Armed, p *prepared) {
	section(w, "Armed")

	fmt.Fprint(w, prose.Para(fmt.Sprintf(
		"All %d channel%s reached chan_pending before anything was signed, so every "+
			"one of them is already recoverable: the peer has stored its commitment "+
			"signature against an outpoint in this transaction, and a force-close "+
			"would get the funds back even if this node vanished. Nothing is in any "+
			"mempool.",
		len(armed.Channels), prose.Plural(len(armed.Channels)))))

	fmt.Fprintf(w, "\n  txid\n      %s\n\n", armed.TxID)
	for i, cp := range armed.Channels {
		peer := ""
		if i < len(p.chans) {
			peer = fmt.Sprintf("  to %s", short(p.chans[i].Peer))
		}
		fmt.Fprintf(w, "  output %d%s\n", cp.Index, peer)
	}

	if n := len(armed.Backup.GetSingleChanBackups().GetChanBackups()); n > 0 {
		fmt.Fprintf(w, "\n  channel backups exported: %d, %d bytes\n", n,
			len(armed.Backup.GetMultiChanBackup().GetMultiChanBackup()))
		fmt.Fprint(w, prose.Para("Taken before anything was broadcast, which is the "+
			"only time it can be. The commitment signature these channels depend on "+
			"lives in this node's channel database rather than in the protocol, so "+
			"the backup plus the peer's data-loss protection is what recovers them "+
			"if the database is lost. Store it off this box."))
	}
}

// withheld is the cold probe's ending: the call that was not made.
func withheld(armed *arm.Armed) string {
	var b strings.Builder
	section(&b, "Step 8 was not made")

	b.WriteString(prose.Para("This run was asked to stop before publishing, so it " +
		"did every step of the real sequence and then did not call " +
		"WalletKit.PublishTransaction. That is the whole difference: there is no " +
		"second, gentler path through the steps above, because a path that " +
		"reached the publish call by another route would be a way around the I-1 " +
		"gate rather than a rehearsal of it."))
	b.WriteString("\n")
	b.WriteString(prose.Para("What that proves is everything except the broadcast: " +
		"these peers accepted these amounts, LND committed to the funding outpoints " +
		"and returned every chan_pending over an unsigned transaction, the backups " +
		"exported off pending channels, and the signing wallet then returned a " +
		"transaction whose txid had not moved."))
	b.WriteString("\n")
	b.WriteString(prose.Para("The batch is now taken apart, which is the other half " +
		"of what the probe proves. Nothing was published, so this costs nothing " +
		"but the peers' patience — each peer keeps its side pending until it " +
		"times out, roughly 2016 blocks from now, so a second probe against the " +
		"same peer costs another of its pending-channel slots. Each channel is " +
		"abandoned rather than cancelled, because each one reached chan_pending, " +
		"and LND wants its blunt flag for every one of them."))
	return b.String()
}

// mayBePublic is the screen after a failed publish call.
//
// The worst state in the design and the one place the recovery tree has no
// clean answer, so the copy says exactly what is known and what must not be
// done. journal.MarkPublishing lands before the RPC precisely so this state is
// visible rather than inferred.
func mayBePublic(armed *arm.Armed, err error) string {
	var b strings.Builder
	section(&b, "The publish call failed, and the transaction may be public")

	b.WriteString(prose.Para(fmt.Sprintf("LND refused or did not answer: %v", err)))
	b.WriteString("\n")
	b.WriteString(prose.Para("The journal records this run as publishing, which was " +
		"written to disk before the call went out. That means the transaction may " +
		"be in a mempool or already in a block, and it may equally have gone " +
		"nowhere. Nothing here can tell the difference, and the difference decides " +
		"everything."))
	b.WriteString("\n  txid\n      " + armed.TxID + "\n\n")
	b.WriteString(prose.Bullet("Look for that txid on your own node first. " +
		"getmempoolentry, then getrawtransaction."))
	b.WriteString(prose.Bullet("If it is there, this is an ordinary settlement: " +
		"every channel is recoverable and the batch simply needs to confirm."))
	b.WriteString(prose.Bullet("If it is not, re-broadcast it yourself: " +
		"bitcoin-cli sendrawtransaction <hex>, with the signed transaction from " +
		"the journal, which is why it is stored — or from Sparrow, which has the " +
		"same bytes. winthistle will not make the call " +
		"twice — the journal refuses a run that already reached it — and no_publish " +
		"also gates LND's own rebroadcaster. That duty is yours."))
	b.WriteString(prose.Bullet("Do not abandon these channels, and the tool will " +
		"not let you: abandoning a pending channel whose funding transaction then " +
		"confirms strands its funds with no force-close path. That is the one " +
		"outcome in this design that loses money."))
	b.WriteString(prose.Bullet("Do not replace the transaction. Replacing it moves " +
		"every outpoint, and every peer holds a commitment signature against the " +
		"old ones (I-4)."))
	return b.String()
}

func short(pubkey string) string {
	if len(pubkey) <= 12 {
		return pubkey
	}
	return pubkey[:12] + "…"
}
