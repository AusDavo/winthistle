package run

import (
	"fmt"
	"io"
	"strings"

	"github.com/AusDavo/winthistle/internal/arm"
	"github.com/AusDavo/winthistle/internal/prose"
)

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
