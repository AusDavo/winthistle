package arm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AusDavo/winthistle/internal/combine"
	"github.com/AusDavo/winthistle/internal/journal"
	"github.com/AusDavo/winthistle/internal/lnd"
	"github.com/AusDavo/winthistle/internal/plan"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The ways a batch dies between the first receipt and the last, against a stub
// rather than the cluster.
//
// Both are failures of the *counterparty* — a peer that stops answering, a node
// that restarts underneath us — and what is under test is our handling of them,
// not their occurrence. A stub returns them at exactly the channel we choose,
// every time; breaking a container at the right moment does not. What the
// harness is for is what LND actually does, and that is tested where it is
// observed: arm_regtest_test.go finalizes a real batch and abandon_regtest_test.go
// tears a half-armed one down.
//
// The assertion that matters in all of them is the same, and it is I-1's other
// half.
// A batch that lost a channel partway must end up neither armed nor published
// *and still abortable*: the journal must hold exactly the receipts that
// arrived, so AbortTarget abandons those channels and cancels the rest. Getting
// that split wrong in either direction is a channel with no force-close path.

// stubStream is one funding stream's server-streaming half.
//
// grpc.ClientStream is embedded nil deliberately. arm calls Recv and nothing
// else on a stream, so a stub that implemented the other six methods would be
// claiming a surface this package does not use — and a later change that started
// using one should fail loudly here rather than meet a stub answer.
type stubStream struct {
	grpc.ClientStream

	// pending, when set, is the chan_pending this stream answers with.
	pending *lnd.ChannelPoint

	// err, when set, is what Recv returns instead.
	err error

	// ctx, when set, is what a stream with neither of the above waits on. That
	// is what a peer that never answers looks like — nothing in this package
	// bounds the wait, so the only thing that ends it is the window's context,
	// and open() makes every stream's context a child of that one.
	ctx context.Context
}

func (s *stubStream) Recv() (*lnrpc.OpenStatusUpdate, error) {
	switch {
	case s.err != nil:
		return nil, s.err
	case s.pending != nil:
		h, err := chainhash.NewHashFromStr(s.pending.TxID)
		if err != nil {
			return nil, err
		}
		return &lnrpc.OpenStatusUpdate{
			Update: &lnrpc.OpenStatusUpdate_ChanPending{
				ChanPending: &lnrpc.PendingUpdate{
					Txid:        h[:],
					OutputIndex: s.pending.Index,
				},
			},
		}, nil
	case s.ctx != nil:
		// The scenario: LND accepted psbt_finalize, sent funding_created, and the
		// peer's funding_signed never came back. gRPC surfaces the expiry as a
		// status error rather than the bare context error, which is what a caller
		// matching on codes would see.
		<-s.ctx.Done()
		return nil, status.FromContextError(s.ctx.Err()).Err()
	}
	return nil, fmt.Errorf("this stream was given no answer to script")
}

// stubLND is arm.Client with every answer scripted per call.
type stubLND struct {
	// finalizeErr is consulted with the zero-based index of each psbt_finalize.
	finalizeErr func(i int) error
	finalizes   int

	// pending is what PendingChannels reports, and lookups counts the asking.
	pending []lnd.ChannelPoint
	lookups int

	// ctxDies makes every call honour the caller's context, which is what gRPC
	// does and what makes the cancelled-context case honest.
	ctxDies bool
}

func (c *stubLND) OpenChannel(_ context.Context, _ *lnrpc.OpenChannelRequest,
	_ ...grpc.CallOption) (lnrpc.Lightning_OpenChannelClient, error) {

	return nil, fmt.Errorf("these fixtures hold streams already open")
}

func (c *stubLND) FundingStateStep(ctx context.Context, in *lnrpc.FundingTransitionMsg,
	_ ...grpc.CallOption) (*lnrpc.FundingStateStepResp, error) {

	if c.ctxDies && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if in.GetPsbtFinalize() == nil {
		return &lnrpc.FundingStateStepResp{}, nil
	}
	i := c.finalizes
	c.finalizes++
	if c.finalizeErr != nil {
		if err := c.finalizeErr(i); err != nil {
			return nil, err
		}
	}
	return &lnrpc.FundingStateStepResp{}, nil
}

func (c *stubLND) ExportAllChannelBackups(_ context.Context, _ *lnrpc.ChanBackupExportRequest,
	_ ...grpc.CallOption) (*lnrpc.ChanBackupSnapshot, error) {

	return &lnrpc.ChanBackupSnapshot{
		MultiChanBackup: &lnrpc.MultiChanBackup{MultiChanBackup: []byte("backup")},
	}, nil
}

func (c *stubLND) PendingChannels(ctx context.Context, _ *lnrpc.PendingChannelsRequest,
	_ ...grpc.CallOption) (*lnrpc.PendingChannelsResponse, error) {

	c.lookups++
	if c.ctxDies && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	resp := &lnrpc.PendingChannelsResponse{}
	for _, cp := range c.pending {
		resp.PendingOpenChannels = append(resp.PendingOpenChannels,
			&lnrpc.PendingChannelsResponse_PendingOpenChannel{
				Channel: &lnrpc.PendingChannelsResponse_PendingChannel{
					ChannelPoint: cp.String(),
				},
			})
	}
	return resp, nil
}

// batchFixture is n streams, the transaction that pays them, and a journal that
// has begun the run.
//
// The funding addresses are derived here rather than taken from LND, because
// what Finalize does with them is arithmetic: resolve each to a script, find the
// output paying that script for that exact amount, and hold the receipt to it.
// A stub node cannot make that arithmetic pass by agreeing with itself — the
// transaction has to really pay the addresses.
type batchFixture struct {
	runID   string
	j       *journal.Journal
	streams *Streams
	txID    string

	// fin is a combine.Finalized with only the three fields Finalize reads set.
	// Producing a real one would mean signing, which is internal/combine's
	// subject and not this file's — and its unexported fields are exactly the
	// ones a caller outside that package is not supposed to be able to fill.
	fin *combine.Finalized
}

const fixtureAmount = 250_000

func newBatch(t *testing.T, runID string, n int) *batchFixture {
	t.Helper()
	ctx := context.Background()

	j, err := journal.Open(ctx, filepath.Join(t.TempDir(), "runs.db"))
	if err != nil {
		t.Fatalf("opening the journal: %v", err)
	}
	t.Cleanup(func() { j.Close() })

	params := &chaincfg.RegressionNetParams
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: 0},
		Sequence:         plan.MaxNonReplaceableSequence,
	})

	s := &Streams{chain: "regtest", Opened: time.Now()}
	var chans []journal.NewChannel
	for i := range n {
		var hash [20]byte
		hash[0] = byte(i + 1)
		addr, err := btcutil.NewAddressWitnessPubKeyHash(hash[:], params)
		if err != nil {
			t.Fatalf("deriving a funding address: %v", err)
		}
		script, err := plan.ScriptFor(addr.EncodeAddress(), params)
		if err != nil {
			t.Fatalf("scripting %s: %v", addr, err)
		}
		tx.AddTxOut(wire.NewTxOut(fixtureAmount, script))

		id, err := lnd.NewPendingChanID()
		if err != nil {
			t.Fatal(err)
		}
		peer := fmt.Sprintf("02%062x", i+1)
		s.All = append(s.All, &Stream{
			PendingChanID:  id,
			Peer:           peer,
			FundingAddress: addr.EncodeAddress(),
			FundingAmount:  fixtureAmount,
		})
		chans = append(chans, journal.NewChannel{
			PendingChanID: id, PeerPubkey: peer, AmountSat: fixtureAmount,
		})
	}
	// Change, so the transaction looks like one this build would ever produce.
	var changeHash [20]byte
	changeHash[19] = 0xff
	change, err := btcutil.NewAddressWitnessPubKeyHash(changeHash[:], params)
	if err != nil {
		t.Fatal(err)
	}
	changeScript, err := plan.ScriptFor(change.EncodeAddress(), params)
	if err != nil {
		t.Fatal(err)
	}
	tx.AddTxOut(wire.NewTxOut(90_000, changeScript))

	if err := j.Begin(ctx, runID, chans); err != nil {
		t.Fatalf("Begin: %v", err)
	}

	txID := tx.TxHash().String()
	var raw bytes.Buffer
	if err := tx.Serialize(&raw); err != nil {
		t.Fatalf("serializing the fixture transaction: %v", err)
	}
	return &batchFixture{
		runID: runID, j: j, streams: s, txID: txID,
		fin: &combine.Finalized{RawTx: raw.Bytes(), TxID: txID, Tx: tx},
	}
}

// outpointOf is the receipt LND would send for stream i, taken from the
// transaction rather than asserted, so a fixture that stopped paying a stream
// fails as a fixture rather than as a scenario.
func (b *batchFixture) outpointOf(t *testing.T, i int) lnd.ChannelPoint {
	t.Helper()
	return lnd.ChannelPoint{TxID: b.txID, Index: uint32(i)}
}

// LND restarting mid-batch: some receipts in hand, and the next psbt_finalize
// meets a node that is not there.
//
// gRPC reports it as Unavailable, on the unary call rather than on the stream,
// because FundingStateStep is what we touch next. What has to survive it is the
// journal's split: the channels that reached chan_pending are in LND's channel
// database and outlive the restart, so they need abandoning; the streams that
// did not are reservations LND held in memory and no longer has, so their shims
// need cancelling. Recording either as the other leaves a channel with no
// force-close path or an abandon that fails.
func TestLNDRestartingMidBatchLeavesExactlyTheReceiptsThatArrived(t *testing.T) {
	ctx := context.Background()
	b := newBatch(t, "restart", 3)

	first := b.outpointOf(t, 0)
	b.streams.All[0].recv = &stubStream{pending: &first}
	b.streams.All[1].recv = &stubStream{ctx: ctx}
	b.streams.All[2].recv = &stubStream{ctx: ctx}

	cli := &stubLND{finalizeErr: func(i int) error {
		if i == 0 {
			return nil
		}
		return status.Error(codes.Unavailable,
			"connection error: desc = transport is closing")
	}}

	_, err := Finalize(ctx, cli, b.j, b.runID, b.streams, b.fin)
	if err == nil {
		t.Fatal("Finalize returned an armed batch through a node that was restarting")
	}
	if !strings.Contains(err.Error(), "psbt_finalize") {
		t.Errorf("the error does not say which step failed: %v", err)
	}
	if !strings.Contains(err.Error(), b.streams.All[1].PendingChanID.String()) {
		t.Errorf("the error does not name the channel that failed: %v", err)
	}

	run, err := b.j.Load(ctx, b.runID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if run.State == journal.StateArmed {
		t.Fatal("the run is armed with one receipt of three — I-1 counts channels, " +
			"and a publish from here would breach it")
	}
	if run.RawTx == "" {
		t.Error("the finalized transaction is not on disk. It went in before the " +
			"first psbt_finalize precisely so a mid-batch death leaves the bytes " +
			"behind: peer 1 is holding a commitment signature against them")
	}

	target, err := run.AbortTarget()
	if err != nil {
		t.Fatalf("AbortTarget refused a run that never reached publish: %v", err)
	}
	if len(target.Channels) != 1 || target.Channels[0] != first {
		t.Errorf("abort would abandon %v, want exactly %s — the one channel that "+
			"reached chan_pending", target.Channels, first)
	}
	if len(target.Shims) != 2 {
		t.Errorf("abort would cancel %d shims, want 2 — the streams that never "+
			"produced a receipt", len(target.Shims))
	}
}

// A peer that never answers psbt_finalize, with the rest of the batch already
// armed behind it.
//
// Two things are pinned here and the second is a limitation rather than a
// property. The first: nothing is armed and nothing is publishable, and the
// refusal names the channel so an operator knows which peer to chase.
//
// The second: the only thing that ends the wait is the caller's context.
// finalizeOne blocks in Recv with no deadline of its own — LND has sent
// funding_created and there is nothing to do but wait for funding_signed — so on
// a run whose context has no deadline (`winthistle run` builds one from
// signal.NotifyContext and nothing else) this is Ctrl-C or forever. The test
// supplies the deadline the production path does not, which is the honest way to
// write it and also the statement of the gap.
func TestAPeerThatNeverAnswersFinalizeLeavesTheBatchUnarmed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	b := newBatch(t, "silent-peer", 3)
	first := b.outpointOf(t, 0)
	b.streams.All[0].recv = &stubStream{pending: &first}
	b.streams.All[1].recv = &stubStream{ctx: ctx} // parks
	b.streams.All[2].recv = &stubStream{ctx: ctx}

	// ctxDies is what gRPC does: once the caller's context is done, every call on
	// that context fails, including the fallback lookup finalizeOne makes.
	cli := &stubLND{ctxDies: true}

	done := make(chan error, 1)
	go func() {
		_, err := Finalize(ctx, cli, b.j, b.runID, b.streams, b.fin)
		done <- err
	}()

	var err error
	select {
	case err = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Finalize did not return after its context expired")
	}
	if err == nil {
		t.Fatal("Finalize armed a batch whose second peer never answered")
	}

	// The message an operator reads. It has to name the channel, and it has to
	// say plainly that the state is unknown rather than implying either answer:
	// the deadline that killed the receipt also killed the lookup that would have
	// settled it.
	msg := err.Error()
	if !strings.Contains(msg, b.streams.All[1].PendingChanID.String()) {
		t.Errorf("the refusal does not name the channel that hung: %v", err)
	}
	if !strings.Contains(msg, "Do not publish") {
		t.Errorf("the refusal does not tell the operator not to publish: %v", err)
	}
	if cli.lookups == 0 {
		t.Error("finalizeOne did not ask PendingChannels whether the channel armed " +
			"anyway. That question is what decides abandon-versus-cancel")
	}

	run, err := b.j.Load(context.Background(), b.runID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if run.State == journal.StateArmed {
		t.Fatal("the run is armed with one receipt of three")
	}
	if _, err := run.AbortTarget(); errors.Is(err, journal.ErrMayBePublished) {
		t.Fatal("the run is unabortable, but nothing here reached the publish call")
	}
}

// The same silence, with the node still reachable: the receipt is lost and the
// channel armed anyway.
//
// This is the case that saves a ceremony. A stream can die — LND restarting is
// enough — after CompleteReservation stored the peer's commitment signature and
// before chan_pending reaches us. The channel is recoverable; only our copy of
// the news is gone. PendingChannels is the second route to the same fact, and
// taking it is the difference between a batch that publishes and a batch that
// gets torn down with every peer's cooperation spent.
func TestALostReceiptIsRecoveredFromPendingChannels(t *testing.T) {
	ctx := context.Background()
	b := newBatch(t, "lost-receipt", 2)

	first := b.outpointOf(t, 0)
	second := b.outpointOf(t, 1)
	b.streams.All[0].recv = &stubStream{pending: &first}
	b.streams.All[1].recv = &stubStream{
		err: status.Error(codes.Unavailable, "transport is closing"),
	}

	cli := &stubLND{pending: []lnd.ChannelPoint{first, second}}

	armed, err := Finalize(ctx, cli, b.j, b.runID, b.streams, b.fin)
	if err != nil {
		t.Fatalf("Finalize gave up on a channel LND lists as a pending open: %v", err)
	}
	if len(armed.Channels) != 2 {
		t.Fatalf("armed %d channels, want 2", len(armed.Channels))
	}
	if armed.Channels[1] != second {
		t.Errorf("the recovered receipt is %s, want %s — PendingChannels is where "+
			"the outpoint came from and it must be the one the transaction pays",
			armed.Channels[1], second)
	}

	run, err := b.j.Load(ctx, b.runID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if run.State != journal.StateArmed {
		t.Fatalf("the run is %s, want %s", run.State, journal.StateArmed)
	}
}

// And the same silence with the node reachable and the channel *not* there.
//
// Nothing armed it, so this is the clean half of the failure: cancel the shim,
// keep the coins, run again. It has to be told apart from the case above by
// something better than the error text, which is what ErrReceiptMissing is for.
func TestASilentPeerWithNothingPendingIsAMissingReceipt(t *testing.T) {
	ctx := context.Background()
	b := newBatch(t, "no-receipt", 2)

	first := b.outpointOf(t, 0)
	b.streams.All[0].recv = &stubStream{pending: &first}
	b.streams.All[1].recv = &stubStream{
		err: status.Error(codes.Unavailable, "transport is closing"),
	}

	cli := &stubLND{pending: []lnd.ChannelPoint{first}}

	_, err := Finalize(ctx, cli, b.j, b.runID, b.streams, b.fin)
	if !errors.Is(err, ErrReceiptMissing) {
		t.Fatalf("error is %v, want it to wrap ErrReceiptMissing so a caller can "+
			"tell a lost receipt from a channel that never armed", err)
	}
	if !strings.Contains(err.Error(), "Do not publish") {
		t.Errorf("the refusal does not tell the operator not to publish: %v", err)
	}

	run, err := b.j.Load(ctx, b.runID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	target, err := run.AbortTarget()
	if err != nil {
		t.Fatalf("AbortTarget: %v", err)
	}
	if len(target.Channels) != 1 || len(target.Shims) != 1 {
		t.Errorf("abort would abandon %d and cancel %d, want 1 and 1",
			len(target.Channels), len(target.Shims))
	}
}
