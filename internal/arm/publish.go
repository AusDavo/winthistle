package arm

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/AusDavo/winthistle/internal/journal"
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

// ErrPublishRefused means the node declined to broadcast.
var ErrPublishRefused = errors.New("lnd refused to publish the funding transaction")

// Publish is step 9, and the only call to WalletKit.PublishTransaction in this
// repository.
//
// Four things have to be true before the bytes go out, and none of them is
// checked here as a courtesy:
//
//   - the caller holds an *Armed. It cannot have built one: rawTx is unexported
//     and only Finalize fills it, and Finalize only returns after every channel
//     produced its receipt.
//   - the journal agrees. MarkPublishing refuses a run that is not armed, and
//     refuses an armed run with no finalized transaction on disk. It is the
//     journal that decided the run was armed in the first place, by counting its
//     own rows in MarkPending.
//   - that write has landed. The journal is opened with synchronous=full under
//     WAL precisely so that "committed" means "on the disk", and this is the
//     write the claim exists for: if the row does not say publishing, the
//     publish did not happen.
//   - the backups are in hand. Finalize will not return an Armed without them.
//
// Why WalletKit and not Core. no_publish sets NoFundingTxBit, and that bit also
// gates rebroadcastFundingTx — so LND will not re-broadcast the funding
// transaction on restart and the duty is ours. Handing it to LND's own wallet
// puts it in the wallet-level rebroadcaster, which keeps trying until the
// transaction enters the chain, restoring by another route the property
// no_publish took away. The raw transaction is in the journal too, so we can
// re-broadcast independently of that.
//
// After this returns there is no abort path. Run.AbortTarget refuses a run in
// publishing or published with ErrMayBePublished, because abandoning a pending
// channel whose funding transaction then confirms strands its funds with no
// force-close path.
func Publish(ctx context.Context, pub Publisher, j *journal.Journal, a *Armed) error {
	switch {
	case a == nil:
		return fmt.Errorf("nothing to publish")
	case len(a.rawTx) == 0:
		return fmt.Errorf("run %s: the armed batch carries no transaction. An Armed "+
			"only comes from Finalize, so this one was not built by the gate", a.RunID)
	case len(a.Channels) == 0:
		return fmt.Errorf("run %s: the armed batch has no channels in it", a.RunID)
	case a.Backup == nil:
		return fmt.Errorf("run %s: no channel backup was exported. Step 8 exports "+
			"before step 9 publishes, because the commitment signature that makes "+
			"these channels recoverable lives in this node's channel database and "+
			"nowhere else", a.RunID)
	}

	// The write that must land before the RPC goes out. It is also the gate: this
	// call fails if the journal does not believe the batch is armed, and the
	// journal is the only component that knows whether every chan_pending arrived.
	if err := j.MarkPublishing(ctx, a.RunID); err != nil {
		return fmt.Errorf("refusing to publish run %s: %w", a.RunID, err)
	}

	resp, err := pub.PublishTransaction(ctx, &walletrpc.Transaction{
		TxHex: a.rawTx,
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
