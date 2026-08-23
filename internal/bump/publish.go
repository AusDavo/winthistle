package bump

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/AusDavo/winthistle/internal/journal"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"google.golang.org/grpc"
)

// MaxChildFeeRateSatPerVB is the highest fee rate a transaction can carry and
// still be broadcast through WalletKit.PublishTransaction.
//
// It is not a policy of this build and it cannot be raised from here. The path
// is fixed at v0.19.3-beta and there is no parameter anywhere along it:
//
//	WalletKit.PublishTransaction deserializes and calls w.cfg.Wallet.PublishTransaction
//	  → btcwallet BtcWallet.PublishTransaction, which first runs
//	    b.chain.TestMempoolAccept([]*wire.MsgTx{tx}, 0) — its own comment says a
//	    max feerate of 0 means the default, "0.10 BTC/kvb, or 10,000 sat/vb" —
//	    then calls w.wallet.PublishTransaction
//	      → wallet.publishTransaction, which calls
//	        chainClient.SendRawTransaction(tx, false). The false is allowHighFees,
//	        hard-coded.
//	          → rpcclient.SendRawTransactionAsync turns allowHighFees=false into
//	            maxfeerate: defaultMaxFeeRate, and defaultMaxFeeRate is 0.1 BTC/kvB.
//
// Core's own help for sendrawtransaction agrees from the other end: maxfeerate
// defaults to "0.10" BTC/kvB, "Set to 0 to accept any fee rate", and — a limit
// on the limit — "Fee rates larger than 1BTC/kvB are rejected", so the escape is
// passing zero rather than passing something big.
//
// # Why this bites a CPFP child and nothing else in this build
//
// A child concentrates a whole package's lift into about 150 virtual bytes. The
// fee it has to pay is (parentVsize + childVsize) × target − parentFee, so its
// own rate is roughly (parentVsize / childVsize) × target — a multiplier of
// around fifty on a three-channel batch and more on a larger one. A package
// target that is unremarkable on mainnet therefore puts the child's own rate
// through this ceiling: about 210 sat/vB on a 7,000 vB parent, and lower as the
// batch gets bigger.
//
// The funding transaction itself never comes close, which is why nothing has met
// this before: it is thousands of virtual bytes paying its own rate.
//
// Verify refuses above this line rather than letting the operator discover it
// after a cold-wallet signing round. The remedy is in ceilingDetail, and it is
// real: the child is a valid transaction that this node declines to relay, so it
// can be handed to Core directly with the ceiling switched off.
const MaxChildFeeRateSatPerVB = 10_000

// IncrementalRelaySatPerVB is the extra fee per virtual byte a BIP-125
// replacement has to pay for its own bandwidth, on top of matching the fee of
// the transaction it replaces.
//
// One sat/vB, which is Core's default -incrementalrelayfee. It is a constant
// here rather than something asked of the node, deliberately: the point of
// checking rule 4 before the signing round is to produce a refusal an operator
// can act on, and a figure read from a node setting they cannot see in the
// message would make it harder to act on rather than easier. A node running a
// higher incremental fee will refuse a replacement this passes — at
// testmempoolaccept, before anything is broadcast, with Core's own wording.
const IncrementalRelaySatPerVB = 1

// ceilingDetail is the operator's way out when the child is too dear to relay
// through LND.
func ceilingDetail(childRate float64) string {
	return fmt.Sprintf("Nothing is wrong with the child: it is a valid "+
		"transaction that this node will not relay. LND hands it to btcwallet, "+
		"which calls sendrawtransaction with maxfeerate fixed at 0.10 BTC/kvB, "+
		"and no WalletKit parameter reaches that. Two ways forward. Ask for a "+
		"smaller lift, which lowers the child's own rate roughly in proportion. "+
		"Or sign this child and hand it to Core yourself with the ceiling off — "+
		"`bitcoin-cli sendrawtransaction <hex> 0` — which is the same bytes by a "+
		"route that takes an argument. The second is not a way around anything: "+
		"the child is already public knowledge the moment it relays, and the "+
		"parent it accelerates has been public since the batch went out. What it "+
		"gives up is LND's wallet-level rebroadcaster, so the child would need "+
		"re-sending by hand if it fell out of the mempool. (%.0f sat/vB is what "+
		"this one asks for.)", childRate)
}

// Publisher is the slice of LND that can broadcast: one method, in its own type.
//
// Structurally identical to arm.Publisher and deliberately not the same type. A
// stage that may broadcast a CPFP child must not be able to broadcast a funding
// transaction, and the way to say that is for the two publish paths to accept
// different things — see Signed.
type Publisher interface {
	PublishTransaction(ctx context.Context, in *walletrpc.Transaction,
		opts ...grpc.CallOption) (*walletrpc.PublishResponse, error)
}

// ErrPublishRefused means the node declined to broadcast the child.
var ErrPublishRefused = errors.New("lnd refused to publish the CPFP child")

// Signed is a CPFP child that has been merged, finalized and verified in-app.
//
// rawTx is unexported and only Sign fills it, which is the whole reason this
// type exists. It is the same device arm.Armed uses, pointed at the same
// problem from the other side: there are two calls to
// WalletKit.PublishTransaction in this build, and neither can be handed the
// other's bytes, because arm.Publish takes an *arm.Armed and this one takes a
// *Signed and there is no way to build either except by going through the checks
// in front of it.
//
// internal/methods pins the count of production call sites at two, and
// TestEveryLNDCallSiteIsRegistered fails if a third appears.
type Signed struct {
	RunID string
	Seq   int64

	// TxID is the child's, and it is the txid the build reported: the merge
	// refuses a packet that moved it.
	TxID string

	// VsizeVB and FeeSat are exact — every witness is present.
	VsizeVB int64
	FeeSat  int64

	// PackageRate is what the pair pays together, which is the number the whole
	// transaction exists to move.
	PackageRate float64

	// Signers are the labels that contributed a partial signature.
	Signers []string

	rawTx []byte
}

// Publish broadcasts the CPFP child, and is one of exactly two calls to
// WalletKit.PublishTransaction in this repository.
//
// # Why this one is allowed to exist
//
// I-1 says publish only once every channel in the batch is already recoverable,
// and the gate that enforces it is a count of chan_pending receipts. A CPFP child
// has no such gate to sit behind and needs none: by the time one can be built
// the parent is in a mempool, every channel in the batch reached chan_pending
// before that happened, and the child spends the batch's change — an output no
// channel depends on. Broadcasting it early is not a thing that can be done,
// because there is no "early": the parent is already out.
//
// What the child could damage if it were wrong is the change output, and that is
// what Verify and Recheck are for. What it cannot do is strand a channel.
//
// # What has to be true before the bytes go out
//
//   - the caller holds a *Signed, which cannot be constructed here: rawTx is
//     unexported and only Sign fills it, after the merge, the finalize and a
//     verification against the parent this child claims to accelerate.
//   - the journal agrees. MarkBumpPublishing refuses a child that is not signed
//     and refuses a signed child with no bytes on disk.
//   - that write has landed. The journal is opened with synchronous=full under
//     WAL, so a child found in publishing afterwards is one that may be in a
//     mempool — which is the only reason this state exists, since unlike the
//     batch there is nothing here an abort could make worse.
//
// # Why WalletKit rather than Core
//
// The same reason as the parent, and one more. LND's wallet-level rebroadcaster
// keeps trying until the transaction confirms, which is exactly what a
// transaction whose entire purpose is to get another one mined wants. And the
// child's bytes are in the journal too, so re-broadcasting is possible without
// LND's help.
//
// The exception is a child above MaxChildFeeRateSatPerVB, which this path cannot
// carry at all. Verify refuses those before the signing round rather than here.
func Publish(ctx context.Context, pub Publisher, j *journal.Journal, s *Signed) error {
	switch {
	case s == nil:
		return errors.New("nothing to publish")
	case len(s.rawTx) == 0:
		return fmt.Errorf("run %s bump %d: this child carries no transaction. A "+
			"Signed only comes from Sign, so this one did not go through the "+
			"verification", s.RunID, s.Seq)
	case s.TxID == "":
		return fmt.Errorf("run %s bump %d: this child has no txid", s.RunID, s.Seq)
	}

	// The write that must land before the RPC goes out.
	if err := j.MarkBumpPublishing(ctx, s.RunID, s.Seq); err != nil {
		return fmt.Errorf("refusing to publish the CPFP child of run %s: %w", s.RunID, err)
	}

	resp, err := pub.PublishTransaction(ctx, &walletrpc.Transaction{
		TxHex: s.rawTx,
		// The label is where an operator looks first when they are trying to
		// work out what is in their own mempool. labels.ValidateAPI caps it at
		// 500 characters.
		Label: fmt.Sprintf("winthistle bump %d of %s", s.Seq, s.RunID),
	})
	if err != nil {
		return fmt.Errorf("%w (%s): %w\n%s", ErrPublishRefused, s.TxID, err,
			refusalAdvice(err, s))
	}

	// PublishResponse carries a publish_error string as well. At v0.19.3-beta
	// WalletKit never sets it, but the field is in the proto and a caller
	// reading only err would report a success it did not have.
	if msg := strings.TrimSpace(resp.GetPublishError()); msg != "" {
		return fmt.Errorf("%w (%s): %s", ErrPublishRefused, s.TxID, msg)
	}

	if err := j.MarkBumpPublished(ctx, s.RunID, s.Seq); err != nil {
		return fmt.Errorf("run %s bump %d: the child %s was published and the "+
			"journal could not be updated to say so: %w\nThe bump is still "+
			"journalled as publishing, which is the safe of the two states to be "+
			"wrong in", s.RunID, s.Seq, s.TxID, err)
	}
	return nil
}

// refusalAdvice says what a refusal means for the batch, which is the question
// the operator actually has.
//
// Nothing about a refused child is dangerous, and saying so plainly is the point:
// the parent is unaffected, every channel in the batch is still recoverable, and
// the change output is still the cold wallet's. The one thing that has changed is
// that Core is holding a lock on that change, and the bump's own abandon is what
// releases it.
func refusalAdvice(err error, s *Signed) string {
	var b strings.Builder
	b.WriteString("The batch is unaffected: the parent is where it was, every " +
		"channel in it is still recoverable by force-close, and the change output " +
		"is still yours. Nothing about a refused child can make the batch worse.\n")

	if strings.Contains(err.Error(), "max-fee-exceeded") ||
		strings.Contains(strings.ToLower(err.Error()), "absurdly-high-fee") ||
		strings.Contains(err.Error(), "Fee exceeds maximum") {

		b.WriteString(ceilingDetail(float64(s.FeeSat) / float64(s.VsizeVB)))
		b.WriteString("\n")
		return b.String()
	}
	b.WriteString("The child's bytes are in the journal. If the refusal was " +
		"transient, publishing again is safe — this bump stays journalled as " +
		"publishing, and a transaction already in the mempool is not an error to " +
		"broadcast twice. If it was not, give up on this child to release the coin " +
		"lock it is holding on the change output, and build another.\n")
	return b.String()
}
