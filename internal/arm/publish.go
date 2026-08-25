package arm

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/AusDavo/winthistle/internal/journal"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"google.golang.org/grpc"
)

// Publisher is the slice of LND that can broadcast: one method, in its own type.
//
// Separate from Client rather than a fifth method on it, because this is the one
// capability in the sequence that cannot be taken back. A stage that does not
// need to broadcast should not be handed something that can, and the smallest
// way to say that is a one-method interface — the same reason internal/reserve
// takes three methods rather than a client.
type Publisher interface {
	PublishTransaction(ctx context.Context, in *walletrpc.Transaction,
		opts ...grpc.CallOption) (*walletrpc.PublishResponse, error)
}

var (
	// ErrPublishRefused means the node declined to broadcast.
	ErrPublishRefused = errors.New("lnd refused to publish the funding transaction")

	// ErrTXIDMoved is I-3 at the last possible moment: the bytes handed to
	// Publish are not the transaction LND committed to.
	//
	// It is the only load-bearing check on what comes back from the signing
	// wallet, and it is fatal to the batch rather than to the attempt. LND pinned
	// the funding outpoints at psbt_verify; a transaction that hashes differently
	// does not contain them, and n peers are holding commitment signatures
	// against outpoints that would never exist.
	ErrTXIDMoved = errors.New("the transaction to publish is not the one that was verified")
)

// Publish is step 8, and the only call to WalletKit.PublishTransaction on the
// funding path.
//
// rawTx is the transaction as the signing wallet returned it, and it arrives
// here as a parameter rather than inside the Armed because after the inversion
// this program never held it: the batch was armed in step 6 over an unsigned
// transaction and signed in step 7 by Sparrow. Five things have to be true
// before the bytes go out, and none of them is checked here as a courtesy:
//
//   - the caller holds an *Armed. It cannot have built one: receipts is
//     unexported and only Receipts fills it, one entry per chan_pending it read.
//   - the bytes hash to the txid LND pinned. That is I-3, and it is the whole of
//     what stands between a returned transaction and n destroyed channels — LND
//     committed to these outpoints at psbt_verify, so bytes with a different txid
//     fund nothing. It is checked against the transaction actually about to be
//     broadcast rather than against a struct field, because a field is what a
//     caller fills in and the bytes are what the network sees.
//   - the journal agrees. MarkPublishing refuses a run whose channels are not all
//     at chan_pending, and refuses one with no transaction on disk. It is the
//     journal that counted them, in MarkPending.
//   - that write has landed. The journal is opened with synchronous=full under
//     WAL precisely so that "committed" means "on the disk", and this is the
//     write the claim exists for: if the row does not say publishing, the
//     publish did not happen.
//   - the backups are in hand. Receipts will not return an Armed without them.
//
// The transaction goes into the journal immediately before that write. We own
// rebroadcast — no_publish sets NoFundingTxBit, which also gates
// rebroadcastFundingTx — so the bytes have to be on disk before they can be in
// anyone's mempool, and this is the first moment they exist at all.
//
// Why WalletKit and not the operator's own wallet. Handing it to LND's wallet
// puts it in the wallet-level rebroadcaster, which keeps trying until the
// transaction enters the chain, restoring by another route the property
// no_publish took away. It is a convenience rather than a gate: the gate closed
// at step 6, and Sparrow could broadcast these bytes just as well. Keeping it is
// worth one call site; calling it a safety mechanism is not.
//
// After this returns there is no abort path. Run.AbortTarget refuses a run in
// publishing or published with ErrMayBePublished, because abandoning a pending
// channel whose funding transaction then confirms strands its funds with no
// force-close path.
func Publish(ctx context.Context, pub Publisher, j *journal.Journal, a *Armed,
	rawTx []byte) error {

	switch {
	case a == nil:
		return fmt.Errorf("nothing to publish")
	case len(rawTx) == 0:
		return fmt.Errorf("run %s: no signed transaction to publish", a.RunID)
	case len(a.Channels) == 0:
		return fmt.Errorf("run %s: the armed batch has no channels in it", a.RunID)
	case len(a.receipts) != len(a.Channels):
		return fmt.Errorf("run %s: this Armed carries %d receipt(s) for %d channel(s). "+
			"An Armed only comes from Receipts, which records one per chan_pending it "+
			"read, so this one was not built by the gate",
			a.RunID, len(a.receipts), len(a.Channels))
	case a.Backup == nil:
		return fmt.Errorf("run %s: no channel backup was exported. The backups are "+
			"taken while every channel is pending and before anything is broadcast, "+
			"because the commitment signature that makes these channels recoverable "+
			"lives in this node's channel database and nowhere else", a.RunID)
	}

	// I-3, against the bytes rather than against anybody's claim about them.
	var tx wire.MsgTx
	if err := tx.Deserialize(bytes.NewReader(rawTx)); err != nil {
		return fmt.Errorf("run %s: the transaction to publish does not deserialize: %w",
			a.RunID, err)
	}
	if got := tx.TxHash().String(); got != a.TxID {
		return fmt.Errorf("%w: these bytes are %s and LND committed to %s at "+
			"psbt_verify. Every channel in this batch funds an outpoint of %s, so "+
			"broadcasting %s would fund nothing and destroy all %d of them. Do not "+
			"publish it by another route either: abort the run and rebuild",
			ErrTXIDMoved, got, a.TxID, a.TxID, got, len(a.Channels))
	}

	// We own rebroadcast, so the bytes are on disk before they can be anywhere
	// else. The journal refuses a transaction whose txid is not the pinned one
	// too, which is the same check made by the component that holds the pin.
	if err := j.RecordFinalizedTx(ctx, a.RunID, a.TxID, hex.EncodeToString(rawTx)); err != nil {
		return fmt.Errorf("journalling the signed transaction before publishing "+
			"run %s: %w", a.RunID, err)
	}

	// The write that must land before the RPC goes out. It is also the gate: this
	// call fails if the journal does not believe every channel is pending, and the
	// journal is the only component that knows whether every chan_pending arrived.
	if err := j.MarkPublishing(ctx, a.RunID); err != nil {
		return fmt.Errorf("refusing to publish run %s: %w", a.RunID, err)
	}

	resp, err := pub.PublishTransaction(ctx, &walletrpc.Transaction{
		TxHex: rawTx,
		// The label turns up in LND's own transaction list, which is where an
		// operator looks first when a batch is stuck. labels.ValidateAPI caps it
		// at 500 characters; this is nowhere near.
		Label: "winthistle batch " + a.RunID,
	})
	if err != nil {
		// The transaction may still have reached the network — a broadcast that
		// errored is not a broadcast that did not happen. The run stays in
		// publishing, which is what stops an abort from touching it.
		return fmt.Errorf("%w (%s): %w\n"+
			"The run is journalled as publishing, so it will not be aborted: this "+
			"transaction may be in a mempool. Every channel in it is already "+
			"recoverable by force-close, and the transaction is in the journal to "+
			"re-broadcast", ErrPublishRefused, a.TxID, err)
	}

	// PublishResponse carries a publish_error string as well. At v0.19.3-beta
	// WalletKit.PublishTransaction never sets it — it returns the wallet's error
	// as a gRPC error and an empty response otherwise — but the field is in the
	// proto and a caller that only looked at err would report a success it did
	// not have.
	if msg := strings.TrimSpace(resp.GetPublishError()); msg != "" {
		return fmt.Errorf("%w (%s): %s", ErrPublishRefused, a.TxID, msg)
	}

	if err := j.MarkPublished(ctx, a.RunID); err != nil {
		return fmt.Errorf("run %s: the funding transaction %s was published and the "+
			"journal could not be updated to say so: %w\n"+
			"The run is still journalled as publishing, which is the safe of the two "+
			"states to be wrong in — an abort will refuse it", a.RunID, a.TxID, err)
	}
	return nil
}
