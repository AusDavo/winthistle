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
		name: "a gRPC error, which is how v0.19.3-beta reports a rejection",
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
