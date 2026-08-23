// Package arm is the armed window: steps 2 through 9 of the sequence, and the
// only place in this repo that can broadcast a transaction.
//
// Everything before it is repeatable and free. Phase 0 chose the peers, the
// amounts, the coin set and the fee rate, checked the anchor reserve, and set
// the cold wallet up; none of that opens a funding stream and none of it costs
// anything to redo. From Open onwards there are n peers holding reservations and
// n clocks running, and from the first Finalize onwards there are peers holding
// commitment signatures against outpoints in a transaction only this process
// has.
//
// # The gate
//
// I-1 says: publish only once every channel is already recoverable. Three things
// enforce it here, and they are deliberately not the same thing said three times.
//
//  1. no_publish is set on every stream, in Open, with no way to unset it. That
//     is what stops LND broadcasting on its own at psbt_finalize — NoFundingTxBit
//     clears ChanType.HasFundingTx(), which is the condition gating the broadcast
//     block in funderProcessFundingSigned, between CompleteReservation and the
//     chan_pending emission.
//  2. Publish will not accept anything but an *Armed, and Armed cannot be
//     constructed outside this package with a transaction in it: the raw bytes
//     live in an unexported field that only Finalize fills. So "there is no path
//     to the publish call that skips the gate" is a fact about the type system
//     rather than a convention.
//  3. The journal refuses. MarkPublishing declines a run that is not armed, and
//     it is the journal that decides whether a run is armed — MarkPending counts
//     its own rows. No caller gets a vote, including this package.
//
// The third is the one that would still hold if this package were wrong.
//
// # What LND does not check, and therefore what we must
//
// It is tempting to read psbt_finalize as a second opinion on I-3. It is not.
// PsbtIntent.FinalizeRawTX compares the outputs with psbt.VerifyOutputsEqual and
// the input *previous outpoints* with psbt.VerifyInputPrevOutpointsEqual, and
// stops there — "the fields in the PSBT part are allowed to change". Sequence
// numbers, version and locktime live in the wire transaction rather than in the
// PSBT part, and none of them is compared. Then CompileFundingTx takes the
// channel point from i.FinalTX.TxHash(), the transaction we just handed it.
//
// So a returned transaction with different sequence numbers has a different txid
// and LND would adopt it — including a transaction that is BIP-125 replaceable,
// which is precisely what I-4 exists to prevent. Nor does LND check the
// signatures: verifyInputsSigned only asserts that each input has *something*
// attached. Both gaps are closed before LND is asked: internal/combine refuses a
// returned packet whose unsigned txid moved and executes every witness against
// its own script, and internal/plan refuses any input below sequence 0xfffffffe.
// This package's job is to call them in the right order.
package arm

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/AusDavo/winthistle/internal/coldwallet"
	"github.com/AusDavo/winthistle/internal/combine"
	"github.com/AusDavo/winthistle/internal/journal"
	"github.com/AusDavo/winthistle/internal/lnd"
	"github.com/AusDavo/winthistle/internal/plan"
	"github.com/AusDavo/winthistle/internal/reserve"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnrpc"
	"google.golang.org/grpc"
)

// Client is the slice of LND the armed window uses to build and arm a batch.
//
// Narrow for the reason internal/reserve's and internal/plan's are: the type
// says what a stage can do. This one can open a funding stream, drive its state
// machine and read a channel backup. It cannot broadcast — that is Publisher,
// separately, because broadcasting is the one action here that cannot be undone.
type Client interface {
	OpenChannel(ctx context.Context, in *lnrpc.OpenChannelRequest,
		opts ...grpc.CallOption) (lnrpc.Lightning_OpenChannelClient, error)

	FundingStateStep(ctx context.Context, in *lnrpc.FundingTransitionMsg,
		opts ...grpc.CallOption) (*lnrpc.FundingStateStepResp, error)

	ExportAllChannelBackups(ctx context.Context, in *lnrpc.ChanBackupExportRequest,
		opts ...grpc.CallOption) (*lnrpc.ChanBackupSnapshot, error)

	PendingChannels(ctx context.Context, in *lnrpc.PendingChannelsRequest,
		opts ...grpc.CallOption) (*lnrpc.PendingChannelsResponse, error)
}

// Channel is one member of the batch, as Phase 0 chose it.
type Channel struct {
	// Peer is the peer's pubkey, hex-encoded.
	Peer string

	// AmountSat is the channel capacity to ask for. The peer may refuse it, and
	// that refusal arrives at Open — which is why Phase 0 probes limits first.
	AmountSat int64

	// Private makes the channel unannounced.
	//
	// It matters past privacy: enforceNewReservedValue returns before counting
	// anything for an unannounced channel, so a private member is invisible to
	// LND's anchor-reserve check twice over. internal/reserve is what knows that;
	// this field is what tells it.
	Private bool
}

// Stream is one open funding stream, held at the point where LND has named the
// funding address and is waiting to be shown a transaction.
type Stream struct {
	PendingChanID lnd.PendingChanID
	Peer          string
	Private       bool

	// FundingAddress and FundingAmount are LND's own psbt_fund answer, and both
	// are exact. LND compares its expected output with psbt.TxOutsEqual, which
	// compares the value as well as the script, so a satoshi of rounding here is
	// a channel that never opens.
	FundingAddress string
	FundingAmount  int64

	cancel context.CancelFunc
	recv   lnrpc.Lightning_OpenChannelClient
}

// Streams are the batch's open funding streams.
type Streams struct {
	All []*Stream

	// Opened is when the last stream came up. The countdown the UI shows is the
	// minimum remaining across all streams, and the peers' clocks start at
	// slightly different moments — see docs/design.html on the ten minutes.
	Opened time.Time

	chain string
}

// PendingChanIDs are the handles that can cancel these streams. Written to the
// journal before anything is shown to a wallet, because until they are on disk a
// crash orphans them.
func (s *Streams) PendingChanIDs() []lnd.PendingChanID {
	out := make([]lnd.PendingChanID, 0, len(s.All))
	for _, st := range s.All {
		out = append(out, st.PendingChanID)
	}
	return out
}

// NewChannels is the journal's view of this batch at the moment its streams open.
func (s *Streams) NewChannels() []journal.NewChannel {
	out := make([]journal.NewChannel, 0, len(s.All))
	for _, st := range s.All {
		out = append(out, journal.NewChannel{
			PendingChanID: st.PendingChanID,
			PeerPubkey:    st.Peer,
			AmountSat:     st.FundingAmount,
		})
	}
	return out
}

// PublicCount is how many members of the batch are announced, which is the
// figure RequiredReserve should be asked about.
//
// Not len(s.All). enforceNewReservedValue returns before it counts anything for
// an unannounced channel, and CurrentNumAnchorChans skips private channels when
// counting, so a private member is invisible to LND's reserve check twice over.
// Passing n for a mixed batch overstates the requirement and would have the
// operator top the node up for a check that is not going to run.
func (s *Streams) PublicCount() int {
	n := 0
	for _, st := range s.All {
		if !st.Private {
			n++
		}
	}
	return n
}

// Batch is the reserve pre-flight's view of the streams that actually opened.
//
// The pre-flight runs in Phase 0, against the planned channel list and before
// any stream exists — see BatchOf. This is the same count taken again once LND
// has the streams, so that the two can be compared: a Phase 0 finding is about a
// particular batch, and a batch that changed shape between the check and the arm
// has a finding that no longer describes it.
func (s *Streams) Batch() reserve.Batch {
	public := s.PublicCount()
	return reserve.Batch{Public: public, Private: len(s.All) - public}
}

// BatchOf is the reserve pre-flight's view of a batch that has not opened yet.
//
// This is what Phase 0 passes to reserve.Check: the announced and unannounced
// counts, taken from the plan rather than from n. It is deliberately the same
// arithmetic Streams.Batch does, so that comparing the two compares the batch
// rather than two different ways of counting it.
func BatchOf(chans []Channel) reserve.Batch {
	var b reserve.Batch
	for _, c := range chans {
		if c.Private {
			b.Private++
			continue
		}
		b.Public++
	}
	return b
}

// Close hangs up every stream without cancelling its shim.
//
// Hanging up is not aborting. The funding intent lives in LND, and taking it
// down is internal/abort's job — CancelShim for a stream that never finalized,
// AbandonPending for one that reached chan_pending. Doing it here as a side
// effect of a deferred Close would abort batches nobody asked to abort.
func (s *Streams) Close() {
	for _, st := range s.All {
		if st.cancel != nil {
			st.cancel()
		}
	}
}

// FundingOutputs are the outputs the batch has to pay, for directed mode's
// step 4.
func (s *Streams) FundingOutputs() []coldwallet.Output {
	out := make([]coldwallet.Output, 0, len(s.All))
	for _, st := range s.All {
		out = append(out, coldwallet.Output{
			Address: st.FundingAddress, AmountSat: st.FundingAmount,
		})
	}
	return out
}

// Blueprint is the plan minus the funding addresses, which do not exist until
// the streams are open.
//
// Everything in it was decided in Phase 0, with no clock running: the fee rate
// against a live estimate, the coin set after SelectCoins judged it, the change
// address from the cold wallet, and the reserve top-up from internal/reserve.
// Combining it with the streams is the last thing that happens before a
// transaction is built.
type Blueprint struct {
	Chain  string
	Fee    plan.Fee
	TopUp  *plan.TopUp
	Change plan.Change
	Inputs plan.Inputs
}

// Plan fills the blueprint in with what LND asked for.
func (s *Streams) Plan(b Blueprint) (*plan.Plan, error) {
	p := &plan.Plan{
		Chain:  b.Chain,
		TopUp:  b.TopUp,
		Change: b.Change,
		Fee:    b.Fee,
		Inputs: b.Inputs,
	}
	for _, st := range s.All {
		p.Channels = append(p.Channels, plan.Channel{
			Peer:          st.Peer,
			PendingChanID: st.PendingChanID.String(),
			Address:       st.FundingAddress,
			AmountSat:     st.FundingAmount,
		})
	}
	// Resolving the outputs is the plan's own validation, and doing it here means
	// a plan that could not be reasoned about is refused before a wallet is
	// shown anything.
	if _, err := p.Outputs(); err != nil {
		return nil, err
	}
	return p, nil
}

// Open is steps 2 and 3: one funding stream per channel, each with a PSBT shim
// and no_publish, read as far as psbt_fund.
//
// no_publish is set unconditionally and there is no parameter that changes it.
// I-1 depends on the app holding the only copy of the transaction until the gate
// opens, and "all but the last" — which is what LND's own docs recommend — is an
// lncli limitation rather than a protocol constraint: the CLI has no way to
// broadcast afterwards. We do.
//
// On a partial failure it returns the streams that did open *alongside* the
// error, rather than only the error. Those shims exist in LND whether this call
// succeeded or not, and their pending channel ids are the only handles that can
// release them — so a caller that drops them on the floor leaves n peers holding
// reservations with nothing on disk to cancel. Journal them, then abort them.
func Open(ctx context.Context, cli Client, chain string, chans []Channel) (*Streams, error) {
	if len(chans) == 0 {
		return nil, fmt.Errorf("a batch with no channels in it is not a batch")
	}
	if _, err := plan.Params(chain); err != nil {
		return nil, err
	}

	s := &Streams{chain: chain}
	for i, c := range chans {
		st, err := open(ctx, cli, c)
		if err != nil {
			return s, &OpenError{Streams: s, Failed: c,
				Err: fmt.Errorf("channel %d of %d: %w", i+1, len(chans), err)}
		}
		s.All = append(s.All, st)
		s.Opened = time.Now()
	}
	return s, nil
}

// OpenError reports which channel could not be opened, and hands back the
// streams that did open so they can be cancelled.
//
// A partially-open batch is not an error state anyone can walk away from: each
// stream that came up is a reservation a peer is holding, and each pending
// channel id is the only handle that can release it.
type OpenError struct {
	Streams *Streams
	Failed  Channel
	Err     error
}

func (e *OpenError) Error() string {
	return fmt.Sprintf("opening the batch's funding streams (%d already open, "+
		"and cancellable): %v", len(e.Streams.All), e.Err)
}

func (e *OpenError) Unwrap() error { return e.Err }

func open(ctx context.Context, cli Client, c Channel) (*Stream, error) {
	if c.AmountSat <= 0 {
		return nil, fmt.Errorf("a channel capacity of %d is not a capacity", c.AmountSat)
	}
	pubkey, err := hex.DecodeString(c.Peer)
	if err != nil {
		return nil, fmt.Errorf("peer %q is not hex: %w", c.Peer, err)
	}
	if len(pubkey) != 33 {
		return nil, fmt.Errorf("peer %q is %d bytes, not a 33-byte compressed pubkey",
			c.Peer, len(pubkey))
	}
	id, err := lnd.NewPendingChanID()
	if err != nil {
		return nil, fmt.Errorf("generating a pending channel id: %w", err)
	}

	// A child of the window's context, cancellable on its own: the stream has to
	// outlive this call, and Close is what hangs it up.
	streamCtx, cancel := context.WithCancel(ctx)

	recv, err := cli.OpenChannel(streamCtx, &lnrpc.OpenChannelRequest{
		NodePubkey:         pubkey,
		LocalFundingAmount: c.AmountSat,
		Private:            c.Private,
		FundingShim: &lnrpc.FundingShim{
			Shim: &lnrpc.FundingShim_PsbtShim{
				PsbtShim: &lnrpc.PsbtShim{
					PendingChanId: id.Bytes(),
					// I-1. Not a parameter, here or anywhere.
					NoPublish: true,
				},
			},
		},
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("opening a funding stream to %s: %w", short(c.Peer), err)
	}

	upd, err := recv.Recv()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("waiting for psbt_fund from %s: %w", short(c.Peer), err)
	}
	fund := upd.GetPsbtFund()
	if fund == nil {
		cancel()
		return nil, fmt.Errorf("expected psbt_fund from %s, got %T",
			short(c.Peer), upd.GetUpdate())
	}
	if fund.GetFundingAddress() == "" || fund.GetFundingAmount() <= 0 {
		cancel()
		return nil, fmt.Errorf("%s named a funding output of %d sat to %q",
			short(c.Peer), fund.GetFundingAmount(), fund.GetFundingAddress())
	}

	return &Stream{
		PendingChanID:  id,
		Peer:           c.Peer,
		Private:        c.Private,
		FundingAddress: fund.GetFundingAddress(),
		FundingAmount:  fund.GetFundingAmount(),
		cancel:         cancel,
		recv:           recv,
	}, nil
}

// Verify is step 5: show the unsigned transaction to every stream in the batch.
//
// Every one of them, and all of them before anything is finalized. That
// ordering is not caution, it is what makes the batch possible: PsbtIntent.Verify
// locates its own output with psbt.TxOutsEqual and never asserts that its output
// is the only one, so n streams can each verify the same n-output transaction and
// each commits to the same unsigned TXID.
//
// A refusal here costs nothing. Nothing has been signed, nothing has been
// finalized, and shim_cancel still works after a successful psbt_verify — so a
// batch that fails at this step is re-armable for the price of one signing round.
// It is also the step that can be refused for a reason that has nothing to do
// with the transaction: psbt_verify runs enforceNewReservedValue over the node's
// own hot wallet afterwards. internal/reserve predicts that before the cold
// wallet is brought out.
//
// Each success is journalled as it happens, and the journal moves the run to
// signing once the last one lands. That is what makes a crash here legible: a
// channel recorded as verified is one LND has committed an outpoint for, and a
// channel still recorded as shim_registered is one that can be cancelled for
// free — which is exactly the split abort.Target needs.
func Verify(ctx context.Context, cli Client, j *journal.Journal, runID string,
	s *Streams, psbtRaw []byte) error {

	if len(psbtRaw) == 0 {
		return fmt.Errorf("nothing to verify")
	}
	for _, st := range s.All {
		_, err := cli.FundingStateStep(ctx, &lnrpc.FundingTransitionMsg{
			Trigger: &lnrpc.FundingTransitionMsg_PsbtVerify{
				PsbtVerify: &lnrpc.FundingPsbtVerify{
					PendingChanId: st.PendingChanID.Bytes(),
					// LND parses raw bytes, not base64.
					FundedPsbt: psbtRaw,
				},
			},
		})
		if err != nil {
			return fmt.Errorf("psbt_verify for the channel to %s (%s): %w",
				short(st.Peer), st.PendingChanID, err)
		}
		if err := j.MarkVerified(ctx, runID, st.PendingChanID); err != nil {
			return fmt.Errorf("journalling psbt_verify for %s: %w", st.PendingChanID, err)
		}
	}
	return nil
}

// Armed is the receipt that every channel in the batch is recoverable by
// force-close, and the only thing Publish will accept.
//
// It cannot usefully be constructed outside this package: RawTx is unexported and
// only Finalize sets it. That is I-1 in the type system — not a rule the publish
// path remembers to check, but a value it cannot be called without.
type Armed struct {
	RunID string

	// TxID and Channels are what the journal now holds: one transaction, n
	// funding outpoints in it.
	TxID     string
	Channels []lnd.ChannelPoint

	// Backup is step 8's export, taken before the transaction reaches anyone's
	// mempool. It is what recovers these channels if this node's channel
	// database is lost, and the commitment signature it depends on lives in that
	// database rather than in the protocol.
	Backup *lnrpc.ChanBackupSnapshot

	// rawTx is the transaction to broadcast. Unexported so that the only way to
	// hold a publishable Armed is to have been through Finalize.
	rawTx []byte
}

var (
	// ErrReceiptMissing means a channel was finalized and no chan_pending
	// arrived, and PendingChannels does not show it either. The batch is neither
	// armed nor clean, and it must not be published.
	ErrReceiptMissing = errors.New("a finalized channel produced no chan_pending")

	// ErrOutpointMoved means LND's chan_pending named a different outpoint from
	// the one the funding address resolves to in the transaction.
	ErrOutpointMoved = errors.New("lnd reported a funding outpoint this transaction does not contain")
)

// Finalize is steps 7 and 8: finalize all n channels, collect all n
// chan_pending, and export the channel backups.
//
// The order inside it is the part worth reading.
//
// The finalized transaction goes into the journal *first*, before the first
// psbt_finalize. From the moment LND is handed this transaction the peers begin
// storing commitment signatures against its outpoints, and because no_publish
// also gates rebroadcastFundingTx we own rebroadcast — so losing the bytes after
// that point is the worst outcome available.
//
// Then one channel at a time: finalize, wait for its receipt, record it. Issuing
// every finalize first and collecting afterwards would be marginally faster and
// would leave up to n channels in the state "LND has the transaction and we do
// not know whether it armed". Sequentially there is at most one, and this call
// says which.
//
// The receipt is checked against the outpoint the funding address resolves to in
// the transaction, rather than believed. That costs nothing and it is the only
// independent confirmation available that LND put the channel where the plan
// says it is.
func Finalize(ctx context.Context, cli Client, j *journal.Journal, runID string,
	s *Streams, f *combine.Finalized) (*Armed, error) {

	if f == nil || len(f.RawTx) == 0 {
		return nil, fmt.Errorf("nothing to finalize with")
	}
	if len(s.All) == 0 {
		return nil, fmt.Errorf("no streams to finalize")
	}

	expected, err := s.outpoints(f.Tx, f.TxID)
	if err != nil {
		return nil, err
	}

	if err := j.RecordFinalizedTx(ctx, runID, f.TxID, hex.EncodeToString(f.RawTx)); err != nil {
		return nil, fmt.Errorf("journalling the finalized transaction before "+
			"handing it to LND: %w", err)
	}

	armed := &Armed{RunID: runID, TxID: f.TxID, rawTx: f.RawTx}
	for _, st := range s.All {
		cp, err := finalizeOne(ctx, cli, st, f.RawTx, expected[st.PendingChanID])
		if err != nil {
			return nil, err
		}
		if err := j.MarkPending(ctx, runID, st.PendingChanID, cp); err != nil {
			return nil, fmt.Errorf("journalling chan_pending for the channel to %s "+
				"(%s at %s): %w", short(st.Peer), st.PendingChanID, cp, err)
		}
		armed.Channels = append(armed.Channels, cp)
	}

	// The journal flipped the run to armed itself when the last row landed. Read
	// it back rather than assuming: MarkPending counts, and this is the assertion
	// that the count came out right.
	run, err := j.Load(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("reading back run %s to confirm the batch is armed: %w",
			runID, err)
	}
	if run.State != journal.StateArmed {
		return nil, fmt.Errorf("run %s reached the end of finalize in state %s "+
			"rather than %s, so the batch is not fully armed and %w",
			runID, run.State, journal.StateArmed, journal.ErrNotArmed)
	}

	// Step 8's other half. On pending channels, which is the case that matters:
	// ExportAllChannelBackups goes through chanbackup.FetchStaticChanBackups
	// over ChannelStateDB.FetchAllChannels, which is documented as "all open
	// channels ... including pending open", so a channel that has only just
	// reached chan_pending is in the snapshot.
	backup, err := cli.ExportAllChannelBackups(ctx, &lnrpc.ChanBackupExportRequest{})
	if err != nil {
		return nil, fmt.Errorf("exporting the channel backups before publishing: %w\n"+
			"The batch is armed and nothing has been broadcast, so this is safe to "+
			"retry — but do not publish without the backups: the commitment "+
			"signature that makes these channels recoverable lives in this node's "+
			"channel database and nowhere else", err)
	}
	if backup.GetMultiChanBackup() == nil || len(backup.GetMultiChanBackup().GetMultiChanBackup()) == 0 {
		return nil, fmt.Errorf("LND returned an empty channel backup for %d pending "+
			"channels", len(armed.Channels))
	}
	armed.Backup = backup

	return armed, nil
}

// finalizeOne finalizes one channel and waits for its receipt.
func finalizeOne(ctx context.Context, cli Client, st *Stream, rawTx []byte,
	want lnd.ChannelPoint) (lnd.ChannelPoint, error) {

	_, err := cli.FundingStateStep(ctx, &lnrpc.FundingTransitionMsg{
		Trigger: &lnrpc.FundingTransitionMsg_PsbtFinalize{
			PsbtFinalize: &lnrpc.FundingPsbtFinalize{
				PendingChanId: st.PendingChanID.Bytes(),
				FinalRawTx:    rawTx,
			},
		},
	})
	if err != nil {
		return lnd.ChannelPoint{}, fmt.Errorf("psbt_finalize for the channel to %s "+
			"(%s): %w", short(st.Peer), st.PendingChanID, err)
	}

	upd, err := st.recv.Recv()
	if err != nil {
		// The finalize succeeded and the receipt did not arrive. LND may or may
		// not have completed the reservation, and the difference decides whether
		// this channel needs cancelling or abandoning — so ask, rather than guess.
		cp, pending, lookupErr := isPending(ctx, cli, want)
		switch {
		case lookupErr != nil:
			return lnd.ChannelPoint{}, fmt.Errorf("psbt_finalize for %s succeeded, "+
				"its chan_pending did not arrive (%v), and PendingChannels could not "+
				"be read either (%v). Do not publish: this channel's state is unknown",
				st.PendingChanID, err, lookupErr)
		case pending:
			// Present in pending_open_channels means the channel is in the channel
			// database, which happens in CompleteReservation — the same call that
			// stores the peer's commitment signature, and the one chan_pending is
			// emitted after. So this is the same fact by a different route.
			return cp, nil
		default:
			return lnd.ChannelPoint{}, fmt.Errorf("%w: %s to %s (%v), and LND does "+
				"not list %s as a pending open. Do not publish",
				ErrReceiptMissing, st.PendingChanID, short(st.Peer), err, want)
		}
	}

	pending := upd.GetChanPending()
	if pending == nil {
		return lnd.ChannelPoint{}, fmt.Errorf("expected chan_pending for %s, got %T",
			st.PendingChanID, upd.GetUpdate())
	}
	cp, err := lnd.ChannelPointFromPending(pending.GetTxid(), pending.GetOutputIndex())
	if err != nil {
		return lnd.ChannelPoint{}, fmt.Errorf("reading the funding outpoint out of "+
			"chan_pending for %s: %w", st.PendingChanID, err)
	}
	if cp != want {
		return lnd.ChannelPoint{}, fmt.Errorf("%w: chan_pending for %s says %s, and "+
			"this transaction pays %s at %s. Do not publish",
			ErrOutpointMoved, st.PendingChanID, cp, st.FundingAddress, want)
	}
	return cp, nil
}

// isPending asks LND whether it holds that channel as a pending open.
func isPending(ctx context.Context, cli Client, cp lnd.ChannelPoint) (
	lnd.ChannelPoint, bool, error) {

	resp, err := cli.PendingChannels(ctx, &lnrpc.PendingChannelsRequest{})
	if err != nil {
		return lnd.ChannelPoint{}, false, err
	}
	want := cp.String()
	for _, p := range resp.GetPendingOpenChannels() {
		if p.GetChannel().GetChannelPoint() == want {
			return cp, true, nil
		}
	}
	return lnd.ChannelPoint{}, false, nil
}

// outpoints resolves each stream's funding address to its output in the
// transaction.
//
// This is what makes the chan_pending receipt checkable rather than merely
// received. It also catches a batch whose transaction does not actually pay one
// of the streams before that stream is finalized — which internal/plan would
// already have refused, so reaching this error means the plan and the
// transaction disagree about which streams are in the batch.
func (s *Streams) outpoints(tx *wire.MsgTx, txid string) (
	map[lnd.PendingChanID]lnd.ChannelPoint, error) {

	params, err := plan.Params(s.chain)
	if err != nil {
		return nil, err
	}
	out := make(map[lnd.PendingChanID]lnd.ChannelPoint, len(s.All))
	for _, st := range s.All {
		script, err := plan.ScriptFor(st.FundingAddress, params)
		if err != nil {
			return nil, fmt.Errorf("the funding address for %s: %w", st.PendingChanID, err)
		}
		found := -1
		for i, o := range tx.TxOut {
			if o.Value == st.FundingAmount && string(o.PkScript) == string(script) {
				if found >= 0 {
					return nil, fmt.Errorf("the transaction pays %s twice, at outputs "+
						"%d and %d. LND would be satisfied — psbt_verify sets a found "+
						"flag and does not count — but the batch would pay twice",
						st.FundingAddress, found, i)
				}
				found = i
			}
		}
		if found < 0 {
			return nil, fmt.Errorf("the transaction does not pay %d sat to %s, which "+
				"is what LND asked for on %s", st.FundingAmount, st.FundingAddress,
				st.PendingChanID)
		}
		out[st.PendingChanID] = lnd.ChannelPoint{TxID: txid, Index: uint32(found)}
	}
	return out, nil
}

func short(pubkey string) string {
	if len(pubkey) <= 16 {
		return pubkey
	}
	return pubkey[:16] + "…"
}
