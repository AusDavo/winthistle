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

	"github.com/AusDavo/winthistle/internal/journal"
	"github.com/AusDavo/winthistle/internal/lnd"
	"github.com/AusDavo/winthistle/internal/plan"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The ways a batch dies between the first psbt_verify and the last receipt,
// against a stub rather than the cluster.
//
// Both are failures of the *counterparty* — a peer that stops answering, a node
// that restarts underneath us — and what is under test is our handling of them,
// not their occurrence. A stub returns them at exactly the channel we choose,
// every time; breaking a container at the right moment does not. What the
// harness is for is what LND actually does, and that is tested where it is
// observed: arm_regtest_test.go arms a real batch and abandon_regtest_test.go
// tears a half-armed one down.
//
// The assertion that matters in all of them is the same, and it is I-1's other
// half.
// A batch that lost a channel partway must end up neither armed nor published
// *and still abortable*: the journal must hold exactly the receipts that
// arrived, so AbortTarget abandons those channels and cancels the rest. Getting
// that split wrong in either direction is a channel with no force-close path.
//
// # What changed with the inversion
//
// The failures used to happen around psbt_finalize. There is no psbt_finalize
// any more: psbt_verify carries skip_finalize, which completes LND's funding
// flow rather than pausing it, and the receipts arrive on their own afterwards.
// So the two phases that can die are Verify — n unary calls, before any peer has
// answered — and Receipts — n stream reads, with peers answering one at a time.
// They fail differently and the split they leave behind is different, which is
// why both are here.

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
		// The scenario: LND accepted psbt_verify, sent funding_created, and the
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
	// verifyErr is consulted with the zero-based index of each psbt_verify.
	verifyErr func(i int) error
	verifies  int

	// finalizes counts psbt_finalize calls, which must stay at zero. After a
	// skip_finalize verify LND's intent is already PsbtFinalized and both
	// finalize entry points require PsbtVerified, so the call would be refused
	// with "invalid state. got finalized expected verified" — and a build that
	// made it would be asking a question it has no use for the answer to.
	finalizes int

	// skipFinalize records the flag on every psbt_verify seen, so a test can
	// assert the whole batch carried it rather than the first one.
	skipFinalize []bool

	// pending is what PendingChannels reports, and lookups counts the asking.
	pending []lnd.ChannelPoint
	lookups int

	// pendingErr, when set, is what PendingChannels returns instead. A node that
	// restarted is not there to answer the fallback question either, and that is
	// the difference between "this channel did not arm" and "this channel's state
	// is unknown".
	pendingErr error

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
	if in.GetPsbtFinalize() != nil {
		c.finalizes++
		return &lnrpc.FundingStateStepResp{}, nil
	}
	if in.GetPsbtVerify() == nil {
		return &lnrpc.FundingStateStepResp{}, nil
	}
	c.skipFinalize = append(c.skipFinalize, in.GetPsbtVerify().GetSkipFinalize())
	i := c.verifies
	c.verifies++
	if c.verifyErr != nil {
		if err := c.verifyErr(i); err != nil {
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
	if c.pendingErr != nil {
		return nil, c.pendingErr
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

// batchFixture is n streams, the unsigned PSBT that pays them, and a journal
// that has begun the run.
//
// The funding addresses are derived here rather than taken from LND, because
// what Verify does with them is arithmetic: resolve each to a script, find the
// output paying that script for that exact amount, and hold the receipt to it.
// A stub node cannot make that arithmetic pass by agreeing with itself — the
// transaction has to really pay the addresses.
type batchFixture struct {
	runID   string
	j       *journal.Journal
	streams *Streams
	txID    string

	// psbtRaw is the batch transaction as a wallet would hand it over: a real
	// PSBT with no signatures in it, which is what the whole sequence now runs
	// on. Verify parses it and takes the txid off UnsignedTx.
	psbtRaw []byte
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
		Sequence:         wire.MaxTxInSequenceNum - 1,
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
			// What open() sets, and what Verify refuses to send skip_finalize
			// without. A fixture that left it false would be testing the refusal
			// rather than the scenario.
			noPublish: true,
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

	packet, err := psbt.NewFromUnsignedTx(tx)
	if err != nil {
		t.Fatalf("wrapping the fixture transaction in a PSBT: %v", err)
	}
	var raw bytes.Buffer
	if err := packet.Serialize(&raw); err != nil {
		t.Fatalf("serializing the fixture PSBT: %v", err)
	}
	return &batchFixture{
		runID: runID, j: j, streams: s,
		txID: tx.TxHash().String(), psbtRaw: raw.Bytes(),
	}
}

// outpointOf is the receipt LND would send for stream i, taken from the
// transaction rather than asserted, so a fixture that stopped paying a stream
// fails as a fixture rather than as a scenario.
func (b *batchFixture) outpointOf(t *testing.T, i int) lnd.ChannelPoint {
	t.Helper()
	return lnd.ChannelPoint{TxID: b.txID, Index: uint32(i)}
}

// verifyAll runs step 5 and fails the test if it does not get through, for the
// scenarios whose subject is step 6.
func (b *batchFixture) verifyAll(t *testing.T, ctx context.Context, cli Client) *Verified {
	t.Helper()
	v, err := Verify(ctx, cli, b.j, b.runID, b.streams, b.psbtRaw)
	if err != nil {
		t.Fatalf("psbt_verify for the whole batch: %v", err)
	}
	return v
}

// LND restarting between the receipts: some in hand, and the next stream read
// meets a node that is not there.
//
// gRPC reports it on the stream, and the fallback lookup fails too because the
// node is what is missing. What has to survive it is the journal's split: the
// channels that reached chan_pending are in LND's channel database and outlive
// the restart, so they need abandoning; the streams that did not are
// reservations LND held in memory and no longer has, so their shims need
// cancelling. Recording either as the other leaves a channel with no
// force-close path or an abandon that fails.
func TestLNDRestartingMidBatchLeavesExactlyTheReceiptsThatArrived(t *testing.T) {
	ctx := context.Background()
	b := newBatch(t, "restart", 3)

	cli := &stubLND{}
	v := b.verifyAll(t, ctx, cli)

	gone := status.Error(codes.Unavailable, "connection error: desc = transport is closing")
	first := b.outpointOf(t, 0)
	b.streams.All[0].recv = &stubStream{pending: &first}
	b.streams.All[1].recv = &stubStream{err: gone}
	b.streams.All[2].recv = &stubStream{err: gone}

	// The node is down, so the question "did it arm anyway?" cannot be asked
	// either. That is the honest shape of a restart, and the refusal has to say
	// the state is unknown rather than pick an answer.
	cli.ctxDies = false
	cli.pendingErr = gone

	_, err := Receipts(ctx, cli, b.j, b.streams, v)
	if err == nil {
		t.Fatal("Receipts returned an armed batch through a node that was restarting")
	}
	if !strings.Contains(err.Error(), b.streams.All[1].PendingChanID.String()) {
		t.Errorf("the error does not name the channel that failed: %v", err)
	}
	if !strings.Contains(err.Error(), "Do not publish") {
		t.Errorf("the refusal does not tell the operator not to publish: %v", err)
	}

	run, err := b.j.Load(ctx, b.runID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if run.State == journal.StateArmed {
		t.Fatal("the run is armed with one receipt of three — I-1 counts channels, " +
			"and a publish from here would breach it")
	}
	if run.TxID != b.txID {
		t.Errorf("the journal pinned %q, want %s. The txid goes in before the first "+
			"psbt_verify precisely so a mid-batch death leaves it behind: peer 1 is "+
			"holding a commitment signature against an outpoint of that transaction",
			run.TxID, b.txID)
	}
	if run.RawTx != "" {
		t.Error("the journal holds a raw transaction for a batch that was never " +
			"signed. Nothing signs anything until the gate is open")
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

// The journal's row for a channel must never be weaker than what LND has been
// asked to do with it, and the ordering of one write is the whole of that.
//
// MarkVerified used to be written after FundingStateStep returned. A process
// that died in that gap left the row saying shim_registered for a channel whose
// funding flow LND had already completed — psbt_verify carries skip_finalize, so
// it does not park the flow, and that channel goes on to reach chan_pending on
// its own with the peer holding its side.
//
// #32 made that gap load-bearing rather than merely untidy. recordAbort reads
// this row to decide what an absent funding intent means: shim_registered is
// taken to establish that no verify happened, so LND created nothing and
// ChanShimGone is terminal. A row written after the RPC would have made that
// inference false for exactly the channel it most matters for.
//
// The stub refuses the first verify, which is the moment a crash between the two
// statements looks like from the journal's side: the call did not succeed, and
// the row must already say verified.
func TestTheVerifyRowIsWrittenBeforeTheCallItNames(t *testing.T) {
	ctx := context.Background()
	b := newBatch(t, "verify-row-first", 2)

	cli := &stubLND{verifyErr: func(int) error {
		return status.Error(codes.Unavailable, "connection error: desc = transport is closing")
	}}
	if _, err := Verify(ctx, cli, b.j, b.runID, b.streams, b.psbtRaw); err == nil {
		t.Fatal("Verify succeeded through a node that refused every call")
	}

	run, err := b.j.Load(ctx, b.runID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var verified int
	for _, c := range run.Channels {
		if c.State == journal.ChanVerified {
			verified++
		}
	}
	if verified != 1 {
		t.Fatalf("%d channels are %s, want the one this run asked LND about: %+v",
			verified, journal.ChanVerified, run.Channels)
	}
	if cli.verifies != 1 {
		t.Errorf("Verify made %d calls after the first was refused, want 1", cli.verifies)
	}
}

// LND going away partway through the *verify* loop, which is the other phase.
//
// Nothing has been armed and no peer has answered, so this is the cheap failure:
// n shims to cancel and nothing to abandon. The thing worth pinning is that the
// txid is on disk anyway. RecordPinnedTxID runs before the first psbt_verify
// because that call does not pause LND's funding flow — it completes it — so from
// the first one onwards a peer may be storing a commitment signature against an
// outpoint of a transaction this run would otherwise have forgotten.
func TestARestartDuringVerifyStillLeavesTheTxidPinned(t *testing.T) {
	ctx := context.Background()
	b := newBatch(t, "restart-verify", 3)

	cli := &stubLND{verifyErr: func(i int) error {
		if i == 0 {
			return nil
		}
		return status.Error(codes.Unavailable, "connection error: desc = transport is closing")
	}}

	if _, err := Verify(ctx, cli, b.j, b.runID, b.streams, b.psbtRaw); err == nil {
		t.Fatal("Verify succeeded through a node that was restarting")
	} else if !strings.Contains(err.Error(), "psbt_verify") {
		t.Errorf("the error does not say which step failed: %v", err)
	}

	run, err := b.j.Load(ctx, b.runID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if run.TxID != b.txID {
		t.Errorf("the journal pinned %q, want %s — before the first psbt_verify, "+
			"not after the last", run.TxID, b.txID)
	}
	if run.State != journal.StateArming {
		t.Errorf("the run is %s, want %s: no channel reached chan_pending",
			run.State, journal.StateArming)
	}

	target, err := run.AbortTarget()
	if err != nil {
		t.Fatalf("AbortTarget: %v", err)
	}
	if len(target.Channels) != 0 || len(target.Shims) != 3 {
		t.Errorf("abort would abandon %d and cancel %d, want 0 and 3 — nothing "+
			"answered, so there is nothing to abandon",
			len(target.Channels), len(target.Shims))
	}
}

// Every psbt_verify in the batch carries skip_finalize, and no psbt_finalize is
// ever sent.
//
// Both halves matter and they are not the same statement. The flag is what makes
// chan_pending arrive over an unsigned transaction, and it has to be on all n or
// the batch splits into two kinds of channel. The absent call is the consequence:
// LND's intent is already PsbtFinalized, so a psbt_finalize would be refused with
// "invalid state. got finalized expected verified", and a build that still made
// one would be one refactor away from re-introducing the signed-transaction
// handover the inversion removed.
func TestEveryVerifyCarriesSkipFinalizeAndNothingIsFinalized(t *testing.T) {
	ctx := context.Background()
	b := newBatch(t, "skip-finalize-all", 3)

	cli := &stubLND{}
	if _, err := Verify(ctx, cli, b.j, b.runID, b.streams, b.psbtRaw); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(cli.skipFinalize) != 3 {
		t.Fatalf("LND saw %d psbt_verify calls for 3 channels", len(cli.skipFinalize))
	}
	for i, got := range cli.skipFinalize {
		if !got {
			t.Errorf("psbt_verify %d of 3 did not set skip_finalize", i+1)
		}
	}

	for i := range b.streams.All {
		cp := b.outpointOf(t, i)
		b.streams.All[i].recv = &stubStream{pending: &cp}
	}
	if _, err := Receipts(ctx, cli, b.j, b.streams,
		&Verified{RunID: b.runID, TxID: b.txID, outpoints: outpointsFor(t, b)}); err != nil {
		t.Fatalf("Receipts: %v", err)
	}
	if cli.finalizes != 0 {
		t.Errorf("%d psbt_finalize call(s) were made. There is no finalize step: "+
			"skip_finalize already took the intent to PsbtFinalized", cli.finalizes)
	}
}

// Verify will not send skip_finalize for a stream that did not set no_publish.
//
// LND refuses the pair itself — PsbtFundingVerify checks it before it touches the
// intent — so this is the same rule stated on our side of the wire. It is worth
// stating twice because the combination is the one that would be dangerous:
// skip_finalize with publishing enabled asks LND to arm a channel and broadcast
// the funding transaction, which is I-1 breached from inside. Today the only
// thing standing between this program and that request is a check in somebody
// else's codebase.
func TestVerifyRefusesAStreamThatDidNotSetNoPublish(t *testing.T) {
	ctx := context.Background()
	b := newBatch(t, "no-publish-unset", 2)
	b.streams.All[1].noPublish = false

	cli := &stubLND{}
	_, err := Verify(ctx, cli, b.j, b.runID, b.streams, b.psbtRaw)
	if err == nil {
		t.Fatal("Verify sent skip_finalize for a stream with no_publish unset")
	}
	if !strings.Contains(err.Error(), "no_publish") ||
		!strings.Contains(err.Error(), b.streams.All[1].PendingChanID.String()) {
		t.Errorf("the refusal does not name the flag and the stream: %v", err)
	}
	if got := len(cli.skipFinalize); got != 1 {
		t.Errorf("LND saw %d psbt_verify calls; the refusal has to land before the "+
			"RPC for the offending stream, not after it", got)
	}
}

// A peer that never answers, with the rest of the batch already reporting in.
//
// Two things are pinned here and the second is a limitation rather than a
// property. The first: nothing is armed and nothing is publishable, and the
// refusal names the channel so an operator knows which peer to chase.
//
// The second: the only thing that ends the wait is the caller's context.
// receiptFor blocks in Recv with no deadline of its own — LND has sent
// funding_created and there is nothing to do but wait for funding_signed — so on
// a run whose context has no deadline (`winthistle run` builds one from
// signal.NotifyContext and nothing else) this is Ctrl-C or forever. The test
// supplies the deadline the production path does not, which is the honest way to
// write it and also the statement of the gap.
//
// This matters more after the inversion, not less: step 6 is the gate and the
// only thing left inside the peers' ten minutes.
func TestAPeerThatNeverAnswersLeavesTheBatchUnarmed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	b := newBatch(t, "silent-peer", 3)
	cli := &stubLND{}
	v := b.verifyAll(t, ctx, cli)

	first := b.outpointOf(t, 0)
	b.streams.All[0].recv = &stubStream{pending: &first}
	b.streams.All[1].recv = &stubStream{ctx: ctx} // parks
	b.streams.All[2].recv = &stubStream{ctx: ctx}

	// ctxDies is what gRPC does: once the caller's context is done, every call on
	// that context fails, including the fallback lookup receiptFor makes.
	cli.ctxDies = true

	done := make(chan error, 1)
	go func() {
		_, err := Receipts(ctx, cli, b.j, b.streams, v)
		done <- err
	}()

	var err error
	select {
	case err = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Receipts did not return after its context expired")
	}
	if err == nil {
		t.Fatal("Receipts armed a batch whose second peer never answered")
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
		t.Error("receiptFor did not ask PendingChannels whether the channel armed " +
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

	cli := &stubLND{pending: []lnd.ChannelPoint{first, second}}
	v := b.verifyAll(t, ctx, cli)

	b.streams.All[0].recv = &stubStream{pending: &first}
	b.streams.All[1].recv = &stubStream{
		err: status.Error(codes.Unavailable, "transport is closing"),
	}

	armed, err := Receipts(ctx, cli, b.j, b.streams, v)
	if err != nil {
		t.Fatalf("Receipts gave up on a channel LND lists as a pending open: %v", err)
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
	if run.RawTx != "" {
		t.Error("the run is armed and holds a raw transaction. The gate now opens " +
			"over an unsigned transaction: nothing should be on disk to broadcast")
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
	cli := &stubLND{pending: []lnd.ChannelPoint{first}}
	v := b.verifyAll(t, ctx, cli)

	b.streams.All[0].recv = &stubStream{pending: &first}
	b.streams.All[1].recv = &stubStream{
		err: status.Error(codes.Unavailable, "transport is closing"),
	}

	_, err := Receipts(ctx, cli, b.j, b.streams, v)
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

// outpointsFor resolves the fixture's expected outpoints the way Verify does, for
// the one test that builds a Verified by hand.
func outpointsFor(t *testing.T, b *batchFixture) map[lnd.PendingChanID]lnd.ChannelPoint {
	t.Helper()
	out := make(map[lnd.PendingChanID]lnd.ChannelPoint, len(b.streams.All))
	for i, st := range b.streams.All {
		out[st.PendingChanID] = b.outpointOf(t, i)
	}
	return out
}
