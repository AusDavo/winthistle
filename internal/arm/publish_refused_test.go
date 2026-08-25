package arm

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AusDavo/winthistle/internal/journal"
	"github.com/AusDavo/winthistle/internal/lnd"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"google.golang.org/grpc"
)

// In-package, because an Armed carrying a transaction is the thing no other
// package can build — rawTx is unexported and only Finalize fills it, which is
// what TestAnArmedValueFromNowhereCarriesNoTransaction pins from outside. Here
// the point is the opposite: reach the RPC with a legitimate-looking Armed so the
// two ways LND can refuse are exercised without a harness and without a peer.
type refusingPublisher struct {
	err   error
	inRep string // publish_error inside an otherwise successful response
	calls int
}

func (p *refusingPublisher) PublishTransaction(_ context.Context,
	_ *walletrpc.Transaction, _ ...grpc.CallOption) (*walletrpc.PublishResponse, error) {

	p.calls++
	if p.err != nil {
		return nil, p.err
	}
	return &walletrpc.PublishResponse{PublishError: p.inRep}, nil
}

const refusedTxID = "8f52b022b79c198551d965f6985ef85d879820fa1a3a4899b1349e32fe4063d9"

// armedRun builds a journal that agrees a two-channel batch is armed, and the
// Armed value that goes with it.
func armedRun(t *testing.T, runID string) (*journal.Journal, *Armed) {
	t.Helper()
	ctx := context.Background()

	j, err := journal.Open(ctx, filepath.Join(t.TempDir(), "runs.db"))
	if err != nil {
		t.Fatalf("opening the journal: %v", err)
	}
	t.Cleanup(func() { j.Close() })

	var chans []journal.NewChannel
	var ids []lnd.PendingChanID
	for i := range 2 {
		id, err := lnd.NewPendingChanID()
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		chans = append(chans, journal.NewChannel{
			PendingChanID: id,
			PeerPubkey:    fmt.Sprintf("02%062x", i+1),
			AmountSat:     250_000,
		})
	}
	if err := j.Begin(ctx, runID, chans); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := j.RecordFinalizedTx(ctx, runID, refusedTxID, "0200000000ffffffff"); err != nil {
		t.Fatalf("RecordFinalizedTx: %v", err)
	}

	a := &Armed{
		RunID: runID, TxID: refusedTxID,
		rawTx:  []byte{0x02, 0x00, 0x00, 0x00},
		Backup: &lnrpc.ChanBackupSnapshot{},
	}
	for i, id := range ids {
		cp := lnd.ChannelPoint{TxID: refusedTxID, Index: uint32(i)}
		if err := j.MarkPending(ctx, runID, id, cp); err != nil {
			t.Fatalf("MarkPending: %v", err)
		}
		a.Channels = append(a.Channels, cp)
	}
	if r, err := j.Load(ctx, runID); err != nil {
		t.Fatal(err)
	} else if r.State != journal.StateArmed {
		t.Fatalf("the fixture run is %s, want %s", r.State, journal.StateArmed)
	}
	return j, a
}

// A refused publish must leave the run unabortable, both ways LND can refuse.
//
// This is the asymmetry the whole design turns on. A broadcast that returned an
// error is not a broadcast that did not happen: the bytes may be in a mempool
// already. So the run stays in publishing, and an abort of a run in publishing
// would abandon pending channels whose funding transaction may still confirm —
// stranding their funds with no force-close path, which is the worst outcome
// this design has. "It failed, so clean up" is the intuition that has to be
// wrong here, and it is refused rather than documented.
func TestARefusedPublishLeavesTheRunUnabortable(t *testing.T) {
	cases := []struct {
		name string
		pub  *refusingPublisher
		// what the operator has to be able to read in the message
		says string
	}{{
		// The general case: whatever the node said, said back. The two cases
		// below are the specific shapes v0.19.3-beta actually produces, and this
		// one is here because our wrapping must not swallow a message it does not
		// recognise either.
		name: "an error the node returned and this build has never seen",
		pub:  &refusingPublisher{err: errors.New("insufficient fee, rejecting replacement")},
		says: "insufficient fee",
	}, {
		// The UpdateChannelPolicy shape: a refusal arriving inside a successful
		// response. WalletKit does not use it at v0.19.3-beta, but the field is
		// in the proto, and a caller that looked only at err would journal a
		// publish that never happened and then report success to the operator.
		name: "publish_error inside a successful response",
		pub:  &refusingPublisher{inRep: "txn-mempool-conflict"},
		says: "txn-mempool-conflict",
	}, {
		// A real mempool rejection, in the form it actually arrives in.
		//
		// BtcWallet.PublishTransaction runs the backend's TestMempoolAccept
		// first and, when it refuses, maps the reject reason through
		// mapRpcclientError. ErrMempoolConflict, ErrMissingInputs,
		// ErrTxAlreadyKnown and ErrTxAlreadyConfirmed all collapse into a bare
		// lnwallet.ErrDoubleSpend — Core's reject string is *dropped*, so
		// "txn-mempool-conflict" above is what the proto could carry and not what
		// v0.19.3-beta says. Which is why the assertion here is that our message
		// keeps what we were told, however little that is: an operator with n
		// peers holding reservations gets "output already spent" and no reason
		// code, and must not also lose it to our own wrapping.
		name: "a mempool conflict, which LND reduces to ErrDoubleSpend",
		pub:  &refusingPublisher{err: lnwallet.ErrDoubleSpend},
		says: "output already spent",
	}, {
		// The one rejection whose reason survives: ErrMempoolMinFeeNotMet is
		// wrapped rather than replaced, so the operator sees the backend's text.
		name: "a fee too low for the mempool, which keeps its reason",
		pub: &refusingPublisher{err: fmt.Errorf("%w: %v", lnwallet.ErrMempoolFee,
			"min relay fee not met, 100 < 141")},
		says: "min relay fee not met",
	}, {
		// bitcoind unreachable at publish time.
		//
		// We never call Core here — the only pre-flight is testmempoolaccept, and
		// that ran back in the armed window. LND is the one that needs the
		// backend, and it needs it twice inside this single RPC: BtcWallet asks
		// the chain for TestMempoolAccept before publishing, and a failure that
		// is not ErrBackendVersion is returned raw. So a dead bitcoind arrives
		// here as an ordinary transport error, and the important part is what it
		// does *not* let us say: nothing distinguishes "the backend was gone
		// before the broadcast" from "the broadcast happened and the answer was
		// lost", so the run stays in publishing like any other refusal.
		name: "bitcoind unreachable, which reaches us as LND's transport error",
		pub: &refusingPublisher{err: errors.New(
			"rpc error: code = Unknown desc = Post \"http://127.0.0.1:18443\": " +
				"dial tcp 127.0.0.1:18443: connect: connection refused")},
		says: "connection refused",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			runID := "refused-" + t.Name()
			j, a := armedRun(t, runID)

			err := Publish(ctx, tc.pub, j, a)
			if err == nil {
				t.Fatal("a refused publish was reported as a success")
			}
			if !errors.Is(err, ErrPublishRefused) {
				t.Errorf("error does not wrap ErrPublishRefused: %v", err)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("the message loses what the node actually said (%q): %v",
					tc.says, err)
			}
			if tc.pub.calls != 1 {
				t.Errorf("PublishTransaction called %d times, want 1", tc.pub.calls)
			}

			// The write landed before the RPC, and it stays landed.
			run, err := j.Load(ctx, runID)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if run.State != journal.StatePublishing {
				t.Fatalf("the run is %s after a refused publish, want %s. Any "+
					"other state either hides that the bytes may be public or "+
					"claims they are", run.State, journal.StatePublishing)
			}

			// And the consequence, asked of the component that owns it.
			if _, err := run.AbortTarget(); !errors.Is(err, journal.ErrMayBePublished) {
				t.Fatalf("AbortTarget gave %v, want ErrMayBePublished. A run whose "+
					"transaction may be in a mempool must not be torn down", err)
			}
		})
	}
}

// The raw transaction has to survive a refusal, because re-broadcasting it is
// the only thing left to do: no_publish set NoFundingTxBit, which also gates
// rebroadcastFundingTx, so LND will not retry the funding transaction itself.
func TestARefusedPublishKeepsTheTransactionToRebroadcast(t *testing.T) {
	ctx := context.Background()
	runID := "refused-keeps-tx"
	j, a := armedRun(t, runID)

	pub := &refusingPublisher{err: errors.New("bad-txns-inputs-missingorspent")}
	if err := Publish(ctx, pub, j, a); err == nil {
		t.Fatal("expected a refusal")
	}

	run, err := j.Load(ctx, runID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if run.RawTx == "" {
		t.Fatal("the finalized transaction is gone from the journal, so there is " +
			"nothing to re-broadcast — and LND will not do it either")
	}
	if run.TxID != refusedTxID {
		t.Errorf("journalled txid %s, want %s", run.TxID, refusedTxID)
	}
}

// After a refusal, there is no retry through this program — and the operator
// copy has to say so, because "re-broadcast it" is the advice.
//
// MarkPublishing accepts only a run in armed, and a refused publish leaves the
// run in publishing. That is deliberate rather than an oversight: the state is
// what stops an abort, and moving back out of it to allow a retry would be
// moving back out of the only thing standing between a pending channel and an
// abandon it must not have. But it means the raw transaction in the journal is
// re-broadcast by the operator with their own tools —
// `bitcoin-cli sendrawtransaction <hex>` — and not by a second call from here. A
// retry path in this repository would be a third WalletKit.PublishTransaction
// call site, which the pinned count of 2 forbids.
func TestARefusedPublishCannotBeRetriedThroughThisProgram(t *testing.T) {
	ctx := context.Background()
	runID := "refused-no-retry"
	j, a := armedRun(t, runID)

	pub := &refusingPublisher{err: lnwallet.ErrDoubleSpend}
	if err := Publish(ctx, pub, j, a); err == nil {
		t.Fatal("expected a refusal")
	}

	// The same call again, as an operator would reach for it.
	err := Publish(ctx, pub, j, a)
	if err == nil {
		t.Fatal("a second publish was accepted. The run is in publishing, which " +
			"is the state that refuses an abort — it must not also be a state a " +
			"fresh broadcast can start from")
	}
	if !errors.Is(err, journal.ErrNotArmed) {
		t.Errorf("error is %v, want ErrNotArmed: the journal is what refuses, and "+
			"a caller has to be able to tell that from a node that said no", err)
	}
	if pub.calls != 1 {
		t.Errorf("PublishTransaction was called %d times, want 1 — the second "+
			"attempt must not reach the node at all", pub.calls)
	}

	run, err := j.Load(ctx, runID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if run.State != journal.StatePublishing {
		t.Errorf("the run is %s, want %s", run.State, journal.StatePublishing)
	}
	if run.RawTx == "" {
		t.Error("the raw transaction is gone, and it is the only thing an operator " +
			"has left to re-broadcast with")
	}
}
