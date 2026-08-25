// Package arm is the armed window: steps 2 through 9 of the sequence, and the
// only place in this repo that can broadcast a transaction.
//
// Everything before it is repeatable and free. Phase 0 chose the peers and the
// amounts and checked the anchor reserve; none of that opens a funding stream and
// none of it costs anything to redo. From Open onwards there are n peers holding
// reservations and n clocks running, and from the first Verify onwards there are
// peers storing commitment signatures against outpoints in a transaction nobody
// has signed yet.
//
// # The inversion
//
// This package used to sign inside the peers' ten minutes and collect the
// receipts afterwards. It now does the opposite, and that is the whole point:
//
//	5  psbt_verify with skip_finalize, all n     ← LND pins the outpoints
//	6  n × chan_pending                          ← GATE OPEN, nothing signed
//	7  sign, with no clock A running
//	8  publish once, if the txid did not move
//
// What makes it possible is one line of LND. PsbtIntent.Verify ends
// (chanfunding/psbt_assembler.go:290-304) with, when !shouldPublish &&
// skipFinalize, i.FinalTX = packet.UnsignedTx, i.State = PsbtFinalized and
// close(i.PsbtReady) — so the funding flow continues from the *unsigned*
// transaction, CompileFundingTx sets the outpoint in stone from it, and
// chan_pending arrives with nothing signed. Proved on regtest at n = 2:
// TestSkipFinalizeReachesChanPendingWithNothingSigned.
//
// A consequence, not a detail: after a skip_finalize verify there is no
// psbt_finalize to make. Both of LND's finalize entry points require
// PsbtVerified and the state is already PsbtFinalized, so the call would return
// "invalid state. got finalized expected verified". Receipts reads the stream;
// it does not step the machine.
//
// # The gate
//
// I-1 says: publish only once every channel is already recoverable. Three things
// enforce it here, and they are deliberately not the same thing said three times.
//
//  1. no_publish is set on every stream, in Open, with no way to unset it. That
//     is what stops LND broadcasting on its own — NoFundingTxBit clears
//     ChanType.HasFundingTx(), which is the condition gating the broadcast block
//     in funderProcessFundingSigned, between CompleteReservation and the
//     chan_pending emission. It is also what makes skip_finalize legal at all:
//     PsbtFundingVerify refuses "skip_finalize for channel that did not set
//     no_publish" (lnwallet/wallet.go:764). Verify asserts the flag on its own
//     side rather than relying on that refusal.
//
//  2. Publish will not accept anything but an *Armed, and an Armed carrying a
//     batch cannot be constructed outside this package: its receipts live in an
//     unexported map that only Receipts fills, one entry per chan_pending it
//     actually read. So "there is no path to the publish call that skips the
//     gate" is a fact about the type system rather than a convention.
//
//     This used to be the raw transaction, which was unforgeable for the same
//     reason. After the inversion the transaction arrives at the publish call
//     from outside — it was signed in step 7, by something that is not this
//     program — so the unforgeable thing has to be the receipts. Publish
//     re-derives the txid from the bytes it is handed and refuses any that do
//     not hash to the pinned one (I-3).
//
//  3. The journal refuses. MarkPublishing declines a run whose channels are not
//     all pending, and it is the journal that counts them. No caller gets a
//     vote, including this package.
//
// The third is the one that would still hold if this package were wrong.
//
// # What LND does not check, and therefore what we must
//
// LND's own txid check is narrower than it sounds, and after the inversion it is
// not in the path at all. PsbtIntent.FinalizeRawTX compares the outputs with
// psbt.VerifyOutputsEqual and the input *previous outpoints* with
// psbt.VerifyInputPrevOutpointsEqual, and stops there — "the fields in the PSBT
// part are allowed to change". Sequence numbers, version and locktime live in
// the wire transaction rather than in the PSBT part, and each moves the txid. We
// no longer call it, so it is not even a second opinion: I-3 is ours alone, and
// it is checked twice — internal/combine refuses a returned packet whose unsigned
// txid moved, and Publish refuses bytes that do not hash to the txid LND pinned.
//
// This is also why every input must be segwit. LND enforces it in Verify —
// verifyAllInputsSegWit, called with "risk of malleability" — and it is what
// makes an unsigned transaction's txid the final one.
package arm

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/AusDavo/winthistle/internal/coldwallet"
	"github.com/AusDavo/winthistle/internal/journal"
	"github.com/AusDavo/winthistle/internal/lnd"
	"github.com/AusDavo/winthistle/internal/plan"
	"github.com/AusDavo/winthistle/internal/policy"
	"github.com/AusDavo/winthistle/internal/reserve"
	"github.com/btcsuite/btcd/btcutil/psbt"
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

	// Policy is what this channel will route at once Phase 2 has applied it.
	//
	// Chosen in Phase 0 and carried through the armed window untouched: nothing
	// here reads it, and it is here so that the plan the operator approves shows
	// the forwarding policy beside the amount. A nil policy is a channel left at
	// LND's defaults — 1000 msat base and 1 ppm — which the plan document says
	// out loud rather than leaving blank, because that is the fee-drain hazard
	// and not an absence of information.
	Policy *policy.Policy
}

// Stream is one open funding stream, held at the point where LND has named the
// funding address and is waiting to be shown a transaction.
type Stream struct {
	PendingChanID lnd.PendingChanID
	Peer          string
	Private       bool

	// Policy is the forwarding policy Phase 0 chose for this channel, carried
	// from the Channel that opened the stream so the plan can show it.
	Policy *policy.Policy

	// FundingAddress and FundingAmount are LND's own psbt_fund answer, and both
	// are exact. LND compares its expected output with psbt.TxOutsEqual, which
	// compares the value as well as the script, so a satoshi of rounding here is
	// a channel that never opens.
	FundingAddress string
	FundingAmount  int64

	// noPublish records that this stream's shim set no_publish, which open does
	// unconditionally. Verify refuses to send skip_finalize for a stream that
	// does not have it.
	//
	// LND refuses the pair itself — PsbtFundingVerify checks skipFinalize &&
	// ShouldPublishFundingTX() before it touches the intent — so this is a second
	// statement of the same rule, on our side of the wire. It is worth having
	// because it is the combination that would be dangerous: skip_finalize
	// without no_publish would ask LND to arm a channel and broadcast, and the
	// only reason that cannot happen today is a check in somebody else's
	// codebase. If a parameter ever reaches Open, this refuses before the RPC.
	noPublish bool

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

	// Aliases maps a peer's pubkey, lowercased, to the alias Phase 0 read out of
	// the gossip graph. Missing entries are ordinary and render as the key alone.
	//
	// It arrives here rather than on Channel or Stream because it is not part of
	// opening a channel: nothing in the funding flow, the verifier or the journal
	// consults it, and a wrong alias can only ever make a sheet less legible. The
	// things that decide something travel on Stream.
	Aliases map[string]string
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
			Alias:         b.Aliases[strings.ToLower(st.Peer)],
			PendingChanID: st.PendingChanID.String(),
			Address:       st.FundingAddress,
			AmountSat:     st.FundingAmount,
			Private:       st.Private,
			Policy:        st.Policy,
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
		Policy:         c.Policy,
		FundingAddress: fund.GetFundingAddress(),
		FundingAmount:  fund.GetFundingAmount(),
		noPublish:      true,
		cancel:         cancel,
		recv:           recv,
	}, nil
}

// Verified is what step 5 produced: LND has pinned one funding outpoint per
// stream, out of an unsigned transaction.
//
// It is the input to Receipts and it carries the outpoints the receipts will be
// checked against. Holding one means the batch is committed in LND and not yet
// committed anywhere else: no peer has answered, nothing is signed, and every
// stream is still cancellable.
type Verified struct {
	RunID string

	// TxID is the unsigned transaction's txid, which is the final one — every
	// input is segwit, so witnesses cannot move it. This is the value LND
	// committed to and the one I-3 pins.
	TxID string

	// outpoints is what each stream's funding address resolves to in that
	// transaction, keyed by pending channel id. Unexported because it is
	// evidence rather than data: Receipts checks each chan_pending against it,
	// and a Verified assembled elsewhere would have nothing to check with.
	outpoints map[lnd.PendingChanID]lnd.ChannelPoint
}

// Verify is step 5: show the unsigned transaction to every stream in the batch,
// with skip_finalize, and let LND pin the funding outpoints.
//
// Every one of them, and all of them before any receipt is read. That ordering
// is not caution, it is what makes the batch possible: PsbtIntent.Verify locates
// its own output with psbt.TxOutsEqual and never asserts that its output is the
// only one, so n streams can each verify the same n-output transaction and each
// commits to the same unsigned TXID.
//
// # What skip_finalize does here
//
// It ends the flow rather than pausing it. With no_publish set — which Open sets
// unconditionally — PsbtIntent.Verify assigns i.FinalTX = packet.UnsignedTx, sets
// PsbtFinalized and closes PsbtReady, so the funding manager continues straight
// on to funding_created and the peer's funding_signed. Every one of these calls
// therefore *starts* a channel: there is no psbt_finalize to make afterwards, and
// making one would be refused with "invalid state. got finalized expected
// verified".
//
// So the txid goes into the journal before the first call, not after the last.
// From that call onwards a peer may be storing a commitment signature against
// these outpoints, and a run that loses the txid loses the only handle on what it
// committed to.
//
// # What a refusal costs
//
// Little, and the same little for every channel. Nothing is signed, nothing is
// broadcast, and a stream whose verify was refused is untouched — LND checks the
// skip_finalize/no_publish pair before it advances the intent, so even that
// refusal leaves a shim that shim_cancel still releases. A batch that fails here
// is re-armable, and the price is a rebuild in Sparrow rather than a signing
// round.
//
// It is also the step that can be refused for a reason that has nothing to do
// with the transaction: psbt_verify runs enforceNewReservedValue over the node's
// own hot wallet. internal/reserve predicts that in Phase 0.
//
// Each success is journalled as it happens. That is what makes a crash here
// legible: a channel recorded as verified is one LND has committed an outpoint
// for, and a channel still recorded as shim_registered is one that can be
// cancelled for free — which is exactly the split abort.Target needs.
func Verify(ctx context.Context, cli Client, j *journal.Journal, runID string,
	s *Streams, psbtRaw []byte) (*Verified, error) {

	if len(psbtRaw) == 0 {
		return nil, fmt.Errorf("nothing to verify")
	}
	if len(s.All) == 0 {
		return nil, fmt.Errorf("no streams to verify against")
	}

	packet, err := psbt.NewFromRawBytes(bytes.NewReader(psbtRaw), false)
	if err != nil {
		return nil, fmt.Errorf("the transaction to verify does not parse as a PSBT: %w", err)
	}
	if err := packet.SanityCheck(); err != nil {
		return nil, fmt.Errorf("the transaction to verify is a malformed PSBT: %w", err)
	}
	txid := packet.UnsignedTx.TxHash().String()

	// Resolved before the first RPC, so a transaction that does not pay one of
	// the streams is refused while every stream is still free to cancel.
	expected, err := s.outpoints(packet.UnsignedTx, txid)
	if err != nil {
		return nil, err
	}

	// Before the first psbt_verify, because the first psbt_verify is what starts
	// a peer storing a commitment signature against these outpoints.
	if err := j.RecordPinnedTxID(ctx, runID, txid); err != nil {
		return nil, fmt.Errorf("journalling the txid LND is about to pin: %w", err)
	}

	for _, st := range s.All {
		// I-1, on our side of the wire. LND refuses this pair itself; asserting
		// it here means a parameter that ever reaches Open cannot turn this into
		// a request to arm a channel and broadcast it.
		if !st.noPublish {
			return nil, fmt.Errorf("the stream to %s (%s) did not set no_publish, "+
				"and skip_finalize without it asks LND to broadcast the funding "+
				"transaction itself. That breaches I-1 and this call will not make it",
				short(st.Peer), st.PendingChanID)
		}
		_, err := cli.FundingStateStep(ctx, &lnrpc.FundingTransitionMsg{
			Trigger: &lnrpc.FundingTransitionMsg_PsbtVerify{
				PsbtVerify: &lnrpc.FundingPsbtVerify{
					PendingChanId: st.PendingChanID.Bytes(),
					// LND parses raw bytes, not base64.
					FundedPsbt: psbtRaw,
					// The inversion. Safe only with no_publish, asserted above.
					SkipFinalize: true,
				},
			},
		})
		if err != nil {
			return nil, fmt.Errorf("psbt_verify for the channel to %s (%s): %w",
				short(st.Peer), st.PendingChanID, err)
		}
		if err := j.MarkVerified(ctx, runID, st.PendingChanID); err != nil {
			return nil, fmt.Errorf("journalling psbt_verify for %s: %w", st.PendingChanID, err)
		}
	}
	return &Verified{RunID: runID, TxID: txid, outpoints: expected}, nil
}

// Armed is the receipt that every channel in the batch is recoverable by
// force-close, and the only thing Publish will accept.
//
// It cannot usefully be constructed outside this package: receipts is unexported
// and only Receipts fills it, one entry per chan_pending actually read. That is
// I-1 in the type system — not a rule the publish path remembers to check, but a
// value it cannot be called without.
type Armed struct {
	RunID string

	// TxID and Channels are what the journal now holds: one transaction, n
	// funding outpoints in it. The transaction is unsigned at this point; the
	// txid is final anyway.
	TxID     string
	Channels []lnd.ChannelPoint

	// Backup is the export taken before the transaction reaches anyone's
	// mempool. It is what recovers these channels if this node's channel
	// database is lost, and the commitment signature it depends on lives in that
	// database rather than in the protocol.
	Backup *lnrpc.ChanBackupSnapshot

	// receipts is one chan_pending per member of the batch, keyed by pending
	// channel id, as Receipts read and journalled them. Unexported so that the
	// only way to hold an Armed the gate agrees with is to have been through
	// Receipts.
	receipts map[lnd.PendingChanID]lnd.ChannelPoint
}

var (
	// ErrReceiptMissing means a verified channel produced no chan_pending and
	// PendingChannels does not show it either. The batch is neither armed nor
	// clean, and it must not be published.
	ErrReceiptMissing = errors.New("a verified channel produced no chan_pending")

	// ErrOutpointMoved means LND's chan_pending named a different outpoint from
	// the one the funding address resolves to in the transaction.
	ErrOutpointMoved = errors.New("lnd reported a funding outpoint this transaction does not contain")
)

// Receipts is step 6, and it is the gate: collect all n chan_pending, then
// export the channel backups.
//
// Nothing is sent to LND here. After a skip_finalize verify the funding flow is
// already running on its own — LND has the unsigned transaction, has sent
// funding_created, and is waiting on each peer's funding_signed — so this call
// reads n streams and writes n journal rows. The receipt buffer is what makes
// that safe to do after all n verifies rather than one at a time: LND gives each
// stream a buffer of two updates and this flow produces exactly two, psbt_fund
// (read in Open) and chan_pending. There is room for the receipt and no room for
// anything else, which is why nothing may be added to the flow without moving
// this read earlier.
//
// Each receipt is checked against the outpoint the funding address resolves to in
// the transaction, rather than believed. That costs nothing and it is the only
// independent confirmation available that LND put the channel where the plan says
// it is.
//
// When this returns, every channel in the batch is recoverable by force-close and
// nothing has been signed. Clock A has stopped and clock B — 2016 blocks from
// broadcast — has not started, because there is nothing to broadcast yet.
func Receipts(ctx context.Context, cli Client, j *journal.Journal, s *Streams,
	v *Verified) (*Armed, error) {

	if v == nil || len(v.outpoints) == 0 {
		return nil, fmt.Errorf("nothing verified to collect receipts for")
	}
	if len(s.All) == 0 {
		return nil, fmt.Errorf("no streams to read receipts from")
	}

	armed := &Armed{
		RunID: v.RunID, TxID: v.TxID,
		receipts: make(map[lnd.PendingChanID]lnd.ChannelPoint, len(s.All)),
	}
	for _, st := range s.All {
		want, ok := v.outpoints[st.PendingChanID]
		if !ok {
			return nil, fmt.Errorf("no verified outpoint for the channel to %s (%s), "+
				"so its receipt cannot be checked against anything",
				short(st.Peer), st.PendingChanID)
		}
		cp, err := receiptFor(ctx, cli, st, want)
		if err != nil {
			return nil, err
		}
		if err := j.MarkPending(ctx, v.RunID, st.PendingChanID, cp); err != nil {
			return nil, fmt.Errorf("journalling chan_pending for the channel to %s "+
				"(%s at %s): %w", short(st.Peer), st.PendingChanID, cp, err)
		}
		armed.Channels = append(armed.Channels, cp)
		armed.receipts[st.PendingChanID] = cp
	}

	// The journal flipped the run to armed itself when the last row landed. Read
	// it back rather than assuming: MarkPending counts, and this is the assertion
	// that the count came out right.
	run, err := j.Load(ctx, v.RunID)
	if err != nil {
		return nil, fmt.Errorf("reading back run %s to confirm the batch is armed: %w",
			v.RunID, err)
	}
	if run.State != journal.StateArmed {
		return nil, fmt.Errorf("run %s reached the end of the receipts in state %s "+
			"rather than %s, so the batch is not fully armed and %w",
			v.RunID, run.State, journal.StateArmed, journal.ErrNotArmed)
	}

	// The backups, while every channel is pending and before anything is signed.
	// ExportAllChannelBackups goes through chanbackup.FetchStaticChanBackups over
	// ChannelStateDB.FetchAllChannels, which is documented as "all open channels
	// ... including pending open", so a channel that has only just reached
	// chan_pending is in the snapshot.
	backup, err := cli.ExportAllChannelBackups(ctx, &lnrpc.ChanBackupExportRequest{})
	if err != nil {
		return nil, fmt.Errorf("exporting the channel backups: %w\n"+
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

// receiptFor waits for one channel's chan_pending.
func receiptFor(ctx context.Context, cli Client, st *Stream,
	want lnd.ChannelPoint) (lnd.ChannelPoint, error) {

	upd, err := st.recv.Recv()
	if err != nil {
		// The verify succeeded and the receipt did not arrive. LND may or may
		// not have completed the reservation, and the difference decides whether
		// this channel needs cancelling or abandoning — so ask, rather than guess.
		cp, pending, lookupErr := isPending(ctx, cli, want)
		switch {
		case lookupErr != nil:
			return lnd.ChannelPoint{}, fmt.Errorf("psbt_verify for %s succeeded, "+
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
// of the streams before that stream is verified — which internal/plan would
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
