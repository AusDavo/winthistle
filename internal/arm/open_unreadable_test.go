package arm

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/AusDavo/winthistle/internal/lnd"
	"github.com/lightningnetwork/lnd/lnrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// What open() does with a funding stream LND answered on but this program
// cannot read — issue #57.
//
// These are the two branches where LND has *not* errored. That distinction is
// the whole subject: a peer's refusal reaches Manager.handleErrorMsg
// (funding/manager.go:5301) and LND cancels its own reservation, so the Recv
// failure needs nothing from us. An update that arrives intact and says
// something unreadable does not go near that path. The reservation is live, the
// hang-up is ours, and hanging up releases nothing — abort's
// TestAShimSurvivesItsStreamBeingHungUp measures that against a real node.
//
// So what is under test is not whether LND behaves this way. It is that the
// pending channel id gets used before it goes out of scope, which cannot be
// observed against a node that never sends an unreadable update. A stub is the
// only way to reach the branch at all.

// openStub is arm.Client with one funding stream scripted, and every shim
// cancel recorded.
type openStub struct {
	// update is what the stream's first Recv answers with. Nil with recvErr
	// unset is a stream that says nothing readable at all.
	update *lnrpc.OpenStatusUpdate

	// recvErr, when set, is returned from Recv instead — the path LND cleans up
	// after itself, and the control for these tests.
	recvErr error

	// cancelErr, when set, is what FundingStateStep returns for a shim cancel.
	cancelErr error

	// opened is the pending channel id open() put in the shim, and cancelled is
	// every id it later asked LND to release. Keeping both is the point: the
	// assertion is that the second contains the first, not merely that some
	// cancel happened.
	opened    []lnd.PendingChanID
	cancelled []lnd.PendingChanID
}

func (c *openStub) OpenChannel(_ context.Context, in *lnrpc.OpenChannelRequest,
	_ ...grpc.CallOption) (lnrpc.Lightning_OpenChannelClient, error) {

	id, err := idOf(in.GetFundingShim().GetPsbtShim().GetPendingChanId())
	if err != nil {
		return nil, fmt.Errorf("the shim carried no readable pending channel id: %w", err)
	}
	c.opened = append(c.opened, id)
	return &openStubStream{stub: c}, nil
}

func (c *openStub) FundingStateStep(_ context.Context, in *lnrpc.FundingTransitionMsg,
	_ ...grpc.CallOption) (*lnrpc.FundingStateStepResp, error) {

	sc := in.GetShimCancel()
	if sc == nil {
		return nil, fmt.Errorf("open() sent a %T, and the only thing it may send "+
			"is a shim cancel", in.GetTrigger())
	}
	id, err := idOf(sc.GetPendingChanId())
	if err != nil {
		return nil, err
	}
	c.cancelled = append(c.cancelled, id)
	if c.cancelErr != nil {
		return nil, c.cancelErr
	}
	return &lnrpc.FundingStateStepResp{}, nil
}

func (c *openStub) ExportAllChannelBackups(_ context.Context, _ *lnrpc.ChanBackupExportRequest,
	_ ...grpc.CallOption) (*lnrpc.ChanBackupSnapshot, error) {

	return nil, fmt.Errorf("open() does not export backups")
}

func (c *openStub) PendingChannels(_ context.Context, _ *lnrpc.PendingChannelsRequest,
	_ ...grpc.CallOption) (*lnrpc.PendingChannelsResponse, error) {

	return nil, fmt.Errorf("open() does not list pending channels")
}

// openStubStream is the server-streaming half. grpc.ClientStream is embedded nil
// for the reason stubStream embeds it nil: open() calls Recv and nothing else,
// and a later change that started using another method should fail here loudly
// rather than meet a stub answer.
type openStubStream struct {
	grpc.ClientStream
	stub *openStub
}

func (s *openStubStream) Recv() (*lnrpc.OpenStatusUpdate, error) {
	if s.stub.recvErr != nil {
		return nil, s.stub.recvErr
	}
	return s.stub.update, nil
}

// idOf reads a pending channel id off the wire the way the journal reads it off
// disk, through the same parser, so a stub cannot accept 32 bytes the rest of
// the program would refuse.
func idOf(b []byte) (lnd.PendingChanID, error) {
	return lnd.ParsePendingChanID(hex.EncodeToString(b))
}

func oneChannel() []Channel {
	return []Channel{{Peer: fmt.Sprintf("02%064x", 1), AmountSat: 250_000}}
}

// The first branch: an update that is not psbt_fund at all.
//
// chan_pending is the update chosen because it is a real member of the oneof and
// arriving out of order is exactly the shape of the defect — LND answering a
// question this program did not ask yet. The id it was opened with has to be the
// id that gets cancelled: an assertion on the count alone would pass for a stub
// that cancelled something else.
func TestAnUpdateThatIsNotPsbtFundReleasesItsShim(t *testing.T) {
	cli := &openStub{update: &lnrpc.OpenStatusUpdate{
		Update: &lnrpc.OpenStatusUpdate_ChanPending{
			ChanPending: &lnrpc.PendingUpdate{},
		},
	}}

	_, err := Open(context.Background(), cli, "regtest", oneChannel())
	if err == nil {
		t.Fatal("Open accepted a stream that never sent psbt_fund")
	}
	if !strings.Contains(err.Error(), "psbt_fund") {
		t.Errorf("the refusal does not say what was missing: %v", err)
	}
	assertCancelledWhatItOpened(t, cli)
}

// The second: psbt_fund arrived, and what it names is not a funding output.
//
// An empty address and a zero amount are one branch in open() and one scenario
// here, because the check is a single condition and splitting the test would be
// asserting on the shape of an `if` rather than on the behaviour.
func TestAPsbtFundNamingNoOutputReleasesItsShim(t *testing.T) {
	cli := &openStub{update: &lnrpc.OpenStatusUpdate{
		Update: &lnrpc.OpenStatusUpdate_PsbtFund{
			PsbtFund: &lnrpc.ReadyForPsbtFunding{FundingAddress: "", FundingAmount: 0},
		},
	}}

	_, err := Open(context.Background(), cli, "regtest", oneChannel())
	if err == nil {
		t.Fatal("Open accepted a psbt_fund with no funding output in it")
	}
	if !strings.Contains(err.Error(), "0 sat") {
		t.Errorf("the refusal does not say what LND named: %v", err)
	}
	assertCancelledWhatItOpened(t, cli)
}

// LND holding no intent for the id is the end state, not a failure to reach it.
//
// CancelShim reports that as ErrNoShim, and open() must not turn it into a
// second sentence telling the operator to go and cancel something that is not
// there. What they read has to be LND's own complaint and nothing else.
func TestAShimThatWasNeverRegisteredIsNotReportedAsAProblem(t *testing.T) {
	cli := &openStub{
		update: &lnrpc.OpenStatusUpdate{
			Update: &lnrpc.OpenStatusUpdate_ChanPending{ChanPending: &lnrpc.PendingUpdate{}},
		},
		cancelErr: status.Error(codes.Unknown,
			"no funding intent found for pendingChannelID(deadbeef)"),
	}

	_, err := Open(context.Background(), cli, "regtest", oneChannel())
	if err == nil {
		t.Fatal("Open accepted a stream that never sent psbt_fund")
	}
	if strings.Contains(err.Error(), "shim_cancel") {
		t.Errorf("the operator is told to cancel a shim LND says does not exist: %v", err)
	}
	assertCancelledWhatItOpened(t, cli)
}

// A cancel that genuinely failed is reported alongside LND's complaint, with the
// id in it.
//
// Both causes, because they are two different things that happened and the
// program established each of them separately. The id, because this is the last
// moment anything knows it — after open() returns, the only route left is the
// out-of-band one the sentence names.
func TestACancelThatFailedIsReportedWithTheIdItCouldNotRelease(t *testing.T) {
	cli := &openStub{
		update: &lnrpc.OpenStatusUpdate{
			Update: &lnrpc.OpenStatusUpdate_ChanPending{ChanPending: &lnrpc.PendingUpdate{}},
		},
		cancelErr: status.Error(codes.Unavailable, "transport is closing"),
	}

	_, err := Open(context.Background(), cli, "regtest", oneChannel())
	if err == nil {
		t.Fatal("Open accepted a stream that never sent psbt_fund")
	}
	msg := err.Error()
	if !strings.Contains(msg, "psbt_fund") {
		t.Errorf("the refusal does not carry LND's own complaint: %v", err)
	}
	if !strings.Contains(msg, "transport is closing") {
		t.Errorf("the refusal does not say why the cancel failed: %v", err)
	}
	if len(cli.opened) != 1 {
		t.Fatalf("the stub saw %d opens, want 1", len(cli.opened))
	}
	if !strings.Contains(msg, cli.opened[0].String()) {
		t.Errorf("the refusal does not name the pending channel id %s, which is the "+
			"only handle left: %v", cli.opened[0], err)
	}
	if !strings.Contains(msg, "shim_cancel") {
		t.Errorf("the refusal does not say how to release it out of band: %v", err)
	}
}

// The control, and the reason the fix is two branches rather than every failure
// path in open().
//
// A refusal arriving *as an error* has already been cleaned up by LND: the
// peer's lnwire.Error reached Manager.handleErrorMsg (funding/manager.go:5301) →
// cancelReservationCtx (:5308) → ChannelReservation.Cancel, whose handler
// deletes the intent (lnwallet/wallet.go:1488). Cancelling here would be a
// second call for an intent that is already gone, and on the commonest way to
// reach this branch — Ctrl-C — it would be a call on a dead context whose
// failure would print a sentence about a shim nobody needs to chase.
func TestARefusalFromLNDIsLeftForLNDToCleanUp(t *testing.T) {
	cli := &openStub{recvErr: status.Error(codes.Unknown,
		"Number of pending channels exceed maximum")}

	_, err := Open(context.Background(), cli, "regtest", oneChannel())
	if err == nil {
		t.Fatal("Open accepted a stream LND refused")
	}
	if !strings.Contains(err.Error(), "exceed maximum") {
		t.Errorf("the refusal does not carry LND's own words: %v", err)
	}
	if len(cli.cancelled) != 0 {
		t.Errorf("open() cancelled %d shim(s) on a path where LND had already "+
			"cancelled its own reservation", len(cli.cancelled))
	}
}

// A partially-open batch still hands back the streams that opened, and the
// stream that failed is the only one whose shim open() touched.
//
// The two halves are the same rule read from both ends. Streams.All are alive
// and journalled by the caller, so cancelling one here would take down a channel
// nobody asked to take down; the stream that never became a Stream has no other
// route, so it is cancelled on the spot. Getting either backwards is a shim with
// no handle or a batch torn up from underneath.
func TestOnlyTheStreamThatFailedHasItsShimCancelled(t *testing.T) {
	cli := &batchStub{failAt: 2}
	chans := []Channel{
		{Peer: fmt.Sprintf("02%064x", 1), AmountSat: 250_000},
		{Peer: fmt.Sprintf("02%064x", 2), AmountSat: 250_000},
		{Peer: fmt.Sprintf("02%064x", 3), AmountSat: 250_000},
	}

	streams, err := Open(context.Background(), cli, "regtest", chans)
	var oe *OpenError
	if !errors.As(err, &oe) {
		t.Fatalf("error is %v, want an *OpenError so the caller can journal the "+
			"streams that did open", err)
	}
	if streams == nil || len(streams.All) != 1 {
		t.Fatalf("Open handed back %v, want the one stream that opened", streams)
	}
	if len(cli.opened) != 2 {
		t.Fatalf("the stub saw %d opens, want 2", len(cli.opened))
	}
	if len(cli.cancelled) != 1 || cli.cancelled[0] != cli.opened[1] {
		t.Errorf("open() cancelled %v, want exactly %s — the stream that failed, "+
			"and not the one the caller is about to journal",
			cli.cancelled, cli.opened[1])
	}
	if streams.All[0].PendingChanID != cli.opened[0] {
		t.Errorf("the surviving stream carries %s, want %s",
			streams.All[0].PendingChanID, cli.opened[0])
	}
}

// batchStub answers psbt_fund properly until the nth open, which sends an
// unreadable update instead.
type batchStub struct {
	openStub
	failAt int
}

func (c *batchStub) OpenChannel(ctx context.Context, in *lnrpc.OpenChannelRequest,
	opts ...grpc.CallOption) (lnrpc.Lightning_OpenChannelClient, error) {

	if len(c.opened)+1 == c.failAt {
		c.update = &lnrpc.OpenStatusUpdate{
			Update: &lnrpc.OpenStatusUpdate_ChanPending{ChanPending: &lnrpc.PendingUpdate{}},
		}
	} else {
		c.update = &lnrpc.OpenStatusUpdate{
			Update: &lnrpc.OpenStatusUpdate_PsbtFund{
				PsbtFund: &lnrpc.ReadyForPsbtFunding{
					// A regtest P2WSH, so nothing downstream has to pretend.
					FundingAddress: "bcrt1qrp33g0q5c5txsp9arysrx4k6zdkfs4nce4xj0gdcccefvpysxf3q0sl5k7",
					FundingAmount:  250_000,
				},
			},
		}
	}
	return c.openStub.OpenChannel(ctx, in, opts...)
}

func assertCancelledWhatItOpened(t *testing.T, cli *openStub) {
	t.Helper()
	if len(cli.opened) != 1 {
		t.Fatalf("the stub saw %d opens, want 1", len(cli.opened))
	}
	if len(cli.cancelled) != 1 {
		t.Fatalf("open() cancelled %d shim(s), want 1 — LND did not error here, so "+
			"nothing on its side is going to release the reservation", len(cli.cancelled))
	}
	if cli.cancelled[0] != cli.opened[0] {
		t.Errorf("open() cancelled %s but opened %s", cli.cancelled[0], cli.opened[0])
	}
}
