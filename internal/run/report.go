package run

import (
	"fmt"
	"io"
	"strings"

	"github.com/AusDavo/winthistle/internal/arm"
	"github.com/AusDavo/winthistle/internal/prose"
)

// reportArmed is the screen at the I-1 gate: every channel is recoverable, and
// nothing has been broadcast.
//
// This is the last moment at which the batch costs nothing. Everything on it is
// reversible until the next call, and everything after it is not, so the screen
// says which channels exist, where their funds will be, and what the one
// remaining action does.
func reportArmed(w io.Writer, armed *arm.Armed, p *prepared) {
	section(w, "Armed")

	fmt.Fprint(w, prose.Para(fmt.Sprintf(
		"All %d channel%s reached chan_pending, so every one of them is already "+
			"recoverable: the peer has stored its commitment signature against an "+
			"outpoint in this transaction, and a force-close would get the funds "+
			"back even if this node vanished. Nothing is in any mempool.",
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
	section(&b, "Step 9 was not made")

	b.WriteString(prose.Para("This run was asked to stop before publishing, so it " +
		"did every step of the real sequence and then did not call " +
		"WalletKit.PublishTransaction. That is the whole difference: there is no " +
		"second, gentler path through the steps above, because a path that " +
		"reached the publish call by another route would be a way around the I-1 " +
		"gate rather than a rehearsal of it."))
	b.WriteString("\n")
	b.WriteString(prose.Para("What that proves is everything except the broadcast: " +
		"these peers accepted these amounts, this cold wallet produced partial " +
		"signatures that combine and finalize here, LND committed to the funding " +
		"outpoints and returned every chan_pending, and the backups exported off " +
		"pending channels."))
	b.WriteString("\n")
	b.WriteString(prose.Para("The batch is now taken apart, which is the other half " +
		"of what the probe proves. Nothing was published, so this costs nothing " +
		"but the peers' patience — each peer keeps its side pending until it " +
		"times out, roughly 2016 blocks from now, so a second probe against the " +
		"same peer costs another of its pending-channel slots."))
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
	b.WriteString(prose.Bullet("If it is not, re-broadcast it. The finalized " +
		"transaction is in the journal, which is why it is stored — no_publish " +
		"also gates LND's own rebroadcaster, so that duty is ours."))
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
