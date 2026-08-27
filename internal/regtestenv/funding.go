package regtestenv

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/AusDavo/winthistle/internal/bitcoind"
	"github.com/AusDavo/winthistle/internal/combine"
	"github.com/AusDavo/winthistle/internal/lnd"
	"github.com/AusDavo/winthistle/internal/regtestenv/coldwallet"
	lnrpc "github.com/lightningnetwork/lnd/lnrpc"
)

// Stream is a funding stream held open at some point in the sequence, so a test
// can act on it — verify it, finalize it, or abandon it half-built.
type Stream struct {
	PendingChanID  lnd.PendingChanID
	PeerPubkey     string
	FundingAddress string
	FundingAmount  int64

	cancel context.CancelFunc
	recv   lnrpc.Lightning_OpenChannelClient
}

// Close hangs up the stream without cancelling the shim. Cancelling is the thing
// under test, so this must not do it as a side effect.
func (s *Stream) Close() {
	if s.cancel != nil {
		s.cancel()
	}
}

// Peers returns the pubkeys of alice's connected peers, sorted.
//
// Sorted rather than in ListPeers order, which is the order LND happens to hold
// them in and is not stable across calls. A batch test that picks peers by index
// needs the indices to mean the same thing twice.
func (e *Env) Peers(t *testing.T) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	resp, err := e.Alice.Lightning.ListPeers(ctx, &lnrpc.ListPeersRequest{})
	if err != nil {
		t.Fatalf("listing peers: %v", err)
	}
	var out []string
	for _, p := range resp.GetPeers() {
		out = append(out, p.GetPubKey())
	}
	if len(out) == 0 {
		t.Skip("alice has no peers — run: make -C regtest reset")
	}
	sort.Strings(out)
	return out
}

// OpenShimStream performs step 2 of the sequence for one channel: open a funding
// stream with a PSBT shim and no_publish set, and read back the funding address
// and amount LND expects.
//
// no_publish is always set, in fixtures as in production. There is no path in
// this package that opens a stream without it — the point of I-1 is that the app
// holds the only copy of the transaction, and a test fixture that quietly opted
// out would be testing a different program.
func (e *Env) OpenShimStream(t *testing.T, peerPubkey string, amountSat int64) *Stream {
	t.Helper()

	id, err := lnd.NewPendingChanID()
	if err != nil {
		t.Fatalf("generating pending channel id: %v", err)
	}
	pubkeyBytes, err := hex.DecodeString(peerPubkey)
	if err != nil {
		t.Fatalf("peer pubkey %q is not hex: %v", peerPubkey, err)
	}

	// Not the test's context: the stream has to outlive this call, and closing
	// it is what Stream.Close is for.
	ctx, cancel := context.WithCancel(context.Background())

	recv, err := e.Alice.Lightning.OpenChannel(ctx, &lnrpc.OpenChannelRequest{
		NodePubkey:         pubkeyBytes,
		LocalFundingAmount: amountSat,
		FundingShim: &lnrpc.FundingShim{
			Shim: &lnrpc.FundingShim_PsbtShim{
				PsbtShim: &lnrpc.PsbtShim{
					PendingChanId: id.Bytes(),
					NoPublish:     true,
				},
			},
		},
	})
	if err != nil {
		cancel()
		t.Fatalf("opening channel stream to %s: %v", peerPubkey[:16], err)
	}

	upd, err := recv.Recv()
	if err != nil {
		cancel()
		t.Fatalf("waiting for psbt_fund from %s: %v", peerPubkey[:16], err)
	}
	fund := upd.GetPsbtFund()
	if fund == nil {
		cancel()
		t.Fatalf("expected psbt_fund, got %T", upd.GetUpdate())
	}

	s := &Stream{
		PendingChanID:  id,
		PeerPubkey:     peerPubkey,
		FundingAddress: fund.GetFundingAddress(),
		FundingAmount:  fund.GetFundingAmount(),
		cancel:         cancel,
		recv:           recv,
	}
	t.Cleanup(s.Close)
	return s
}

// FundedPSBT is one unsigned transaction paying every stream in the batch.
type FundedPSBT struct {
	Base64  string
	Raw     []byte              // the same packet in bytes, which is what LND parses
	TxID    string              // the unsigned txid — what I-3 pins
	Inputs  []bitcoind.Outpoint // locked by Core when lockUnspents is set
	ChangeI int
}

// BuildFundingPSBT performs step 4: one unsigned PSBT with an output for every
// stream, funded and change-derived by a Core wallet.
//
// It is a thin wrapper over coldwallet.Build rather than its own call to
// walletcreatefundedpsbt, and that is the point: a fixture with its own builder
// is a fixture that can drift from the thing it is meant to be testing. The
// non-negotiable options — replaceable:false, lockUnspents:true, bip32derivs —
// live in the app, once.
//
// wallet chooses the funding source. In practice that is always the watch-only
// cold wallet, because that is what production uses and because the signing path
// on the way back out is the cold wallet's two halves.
func (e *Env) BuildFundingPSBT(t *testing.T, wallet *bitcoind.Client,
	streams []*Stream, feeRate float64) FundedPSBT {

	t.Helper()
	outputs := make([]coldwallet.Output, 0, len(streams))
	for _, s := range streams {
		outputs = append(outputs, coldwallet.Output{
			Address: s.FundingAddress, AmountSat: s.FundingAmount,
		})
	}
	return e.BuildPSBTPaying(t, wallet, outputs, feeRate)
}

// BuildPSBTPaying is BuildFundingPSBT for a caller that has the addresses but not
// the streams.
//
// That caller is the harness playing Sparrow: after the inversion the app prints
// the recipients and something else builds the transaction, so a fixture standing
// in for that something else is handed a list of outputs the way an operator's
// Sparrow is handed the recipients CSV. It is the same builder underneath —
// coldwallet.Build, with the app's own non-negotiable options — because a fixture
// with its own builder is a fixture that can drift from the thing it is testing.
func (e *Env) BuildPSBTPaying(t *testing.T, wallet *bitcoind.Client,
	outputs []coldwallet.Output, feeRate float64) FundedPSBT {

	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Keep Core's coin selection off anything LND would refuse.
	//
	// This is not hypothetical tidiness. Core derives change of the same type as
	// the payment, so a single send to a legacy address leaves a legacy change
	// output in the wallet — and the next batch built from that wallet fails at
	// psbt_verify with "not all inputs are SegWit spends", naming an input that
	// has nothing to do with the test that produced it. One fixture that needed
	// a legacy coin poisoned every abort test this way.
	//
	// Core has no "do not spend these" option, and it does skip locked outputs,
	// so the exclusion is a lock — the same mechanism directed mode uses.
	fenceOffLegacy(t, wallet)

	change, err := coldwallet.ChangeAddress(ctx, wallet)
	if err != nil {
		t.Fatalf("asking the funding wallet for a change address: %v", err)
	}

	built, err := coldwallet.Build(ctx, wallet, coldwallet.BuildRequest{
		Outputs:          outputs,
		ChangeAddress:    change,
		FeeRateSatPerVB:  feeRate,
		MinConfirmations: 1,
	})
	if err != nil {
		t.Fatalf("building the batch transaction: %v", err)
	}

	// The harness owns the locks it takes now.
	//
	// walletcreatefundedpsbt is called with lockUnspents, so Core holds these
	// coins unspendable, and until item 5 the application's abort path released
	// them: every fixture that built a batch and then aborted got its coins back
	// as a side effect of the thing it was testing. The app selects no coins and
	// dials no Bitcoin node, so nothing releases them any more — and a leaked
	// lock does not announce itself, it starves the next test in the same
	// binary with "Insufficient funds" from a wallet whose balance is fine.
	e.ReleaseLocksAtCleanup(t, wallet, built.Inputs)

	return FundedPSBT{
		Base64:  built.PSBT,
		Raw:     built.Raw,
		TxID:    built.TxID,
		Inputs:  built.Inputs,
		ChangeI: built.ChangeIndex,
	}
}

// fenceOffLegacy locks the wallet's non-SegWit coins for the duration of the
// test, and gives them back afterwards.
func fenceOffLegacy(t *testing.T, wallet *bitcoind.Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	coins, err := coldwallet.SelectCoins(ctx, wallet, 1)
	if err != nil {
		t.Fatalf("splitting the funding wallet's coins: %v", err)
	}
	locked, err := coldwallet.FenceOff(ctx, wallet, coins)
	if err != nil {
		t.Fatalf("fencing off the coins LND would refuse: %v", err)
	}
	if len(locked) == 0 {
		return
	}
	t.Logf("fenced off %d coin(s) the batch may not spend", len(locked))
	t.Cleanup(func() {
		c, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		if _, err := wallet.ReleaseLocks(c, locked); err != nil {
			t.Errorf("releasing the fence: %v", err)
		}
	})
}

// Verify performs step 5 for one stream: hand LND the unsigned PSBT so it can
// check that its own script and amount are present.
//
// After this call LND has committed to the funding outpoint — only signatures
// may be added (I-3).
func (e *Env) Verify(t *testing.T, s *Stream, psbtB64 string) {
	t.Helper()
	if err := e.TryVerify(t, s, psbtB64); err != nil {
		t.Fatalf("psbt_verify for %s: %v", s.PendingChanID, err)
	}
}

// TryVerify is Verify for the tests whose subject is the refusal.
//
// psbt_verify has a rejection that has nothing to do with the PSBT — it runs
// enforceNewReservedValue over the node's own wallet afterwards — and a fixture
// that could only fatal on it could not prove anything about it. See
// internal/reserve.
func (e *Env) TryVerify(t *testing.T, s *Stream, psbtB64 string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	raw, err := base64.StdEncoding.DecodeString(psbtB64)
	if err != nil {
		t.Fatalf("psbt is not base64: %v", err)
	}
	// LND parses raw bytes, not base64 — psbt.NewFromRawBytes(r, false).
	_, err = e.Alice.Lightning.FundingStateStep(ctx, &lnrpc.FundingTransitionMsg{
		Trigger: &lnrpc.FundingTransitionMsg_PsbtVerify{
			PsbtVerify: &lnrpc.FundingPsbtVerify{
				PendingChanId: s.PendingChanID.Bytes(),
				FundedPsbt:    raw,
			},
		},
	})
	return err
}

// VerifySkippingFinalize is step 5 as the app now makes it: psbt_verify with
// skip_finalize, which completes LND's funding flow rather than pausing it.
//
// There is no step after it for this stream. LND takes the *unsigned*
// transaction's outpoint as final, exchanges funding_created and funding_signed
// with the peer, and emits chan_pending on its own — read it with Receipt. A
// psbt_finalize afterwards is refused with "invalid state. got finalized expected
// verified", because the intent is already PsbtFinalized.
//
// Verify and Finalize are still here, and still drive the pre-inversion flow.
// That is deliberate: a fixture whose subject is the abort path only needs a
// channel at chan_pending, and either route produces one. Use this pair when the
// ordering itself is what the test is about.
func (e *Env) VerifySkippingFinalize(t *testing.T, s *Stream, psbtB64 string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	raw, err := base64.StdEncoding.DecodeString(psbtB64)
	if err != nil {
		t.Fatalf("psbt is not base64: %v", err)
	}
	if _, err := e.Alice.Lightning.FundingStateStep(ctx, &lnrpc.FundingTransitionMsg{
		Trigger: &lnrpc.FundingTransitionMsg_PsbtVerify{
			PsbtVerify: &lnrpc.FundingPsbtVerify{
				PendingChanId: s.PendingChanID.Bytes(),
				FundedPsbt:    raw,
				SkipFinalize:  true,
			},
		},
	}); err != nil {
		t.Fatalf("psbt_verify(skip_finalize) for %s: %v", s.PendingChanID, err)
	}
}

// Receipt reads one stream's chan_pending, which arrives on its own after a
// skip_finalize verify.
//
// The returned ChannelPoint is the receipt that the channel is recoverable: LND
// emits chan_pending only after CompleteReservation has stored the peer's
// commitment signature and WatchNewChannel has handed the outpoint to the
// ChainArbitrator. Nothing is broadcast, because no_publish cleared
// ChanType.HasFundingTx() and that is what gates the broadcast block — and
// because the transaction has no witnesses yet for anyone to broadcast it with.
func (e *Env) Receipt(t *testing.T, s *Stream) lnd.ChannelPoint {
	t.Helper()
	upd, err := s.recv.Recv()
	if err != nil {
		t.Fatalf("waiting for chan_pending on %s: %v", s.PendingChanID, err)
	}
	pending := upd.GetChanPending()
	if pending == nil {
		t.Fatalf("expected chan_pending, got %T", upd.GetUpdate())
	}
	cp, err := lnd.ChannelPointFromPending(pending.GetTxid(), pending.GetOutputIndex())
	if err != nil {
		t.Fatalf("reading chan_pending outpoint: %v", err)
	}
	return cp
}

// SignAndCombine is step 6, the real one: collect a partial signature from each
// half of the simulated cold wallet, combine and finalize them in-app, and
// confirm the mempool would accept the result.
//
// It replaced a SignWithMiner helper that signed with one key so the abort
// fixtures had a complete transaction. That shortcut did not model I-2 — for the
// duration of the call one external wallet held a broadcastable transaction — and
// once internal/combine existed there was no reason to keep a second, weaker
// path around for tests to lean on.
//
// testmempoolaccept validates without relaying, so nothing here publishes.
func (e *Env) SignAndCombine(t *testing.T, funded FundedPSBT) (rawTxHex, txid string) {
	t.Helper()

	parts := e.SignWithColdWallet(t, funded.Base64)
	merged, err := combine.Merge(funded.Raw, parts)
	if err != nil {
		t.Fatalf("combining the cold wallet's partials: %v", err)
	}
	final, err := combine.Finalize(merged)
	if err != nil {
		t.Fatalf("finalizing in-app: %v", err)
	}
	if final.TxID != funded.TxID {
		t.Fatalf("I-3: the txid moved from %s to %s", funded.TxID, final.TxID)
	}

	hexTx := hex.EncodeToString(final.RawTx)
	if ok, why := e.AcceptsToMempool(t, hexTx); !ok {
		t.Fatalf("testmempoolaccept refused the batch: %s", why)
	}
	return hexTx, final.TxID
}

// Finalize performs step 7 for one stream and waits for its chan_pending.
//
// The returned ChannelPoint is the receipt that the channel is recoverable: LND
// emits chan_pending only after CompleteReservation has stored the peer's
// commitment signature and WatchNewChannel has handed the outpoint to the
// ChainArbitrator. Nothing is broadcast, because no_publish cleared
// ChanType.HasFundingTx() and that is what gates the broadcast block.
func (e *Env) Finalize(t *testing.T, s *Stream, rawTxHex string) lnd.ChannelPoint {
	t.Helper()
	if err := e.TryFinalize(t, s, rawTxHex); err != nil {
		t.Fatalf("psbt_finalize for %s: %v", s.PendingChanID, err)
	}
	upd, err := s.recv.Recv()
	if err != nil {
		t.Fatalf("waiting for chan_pending on %s: %v", s.PendingChanID, err)
	}
	pending := upd.GetChanPending()
	if pending == nil {
		t.Fatalf("expected chan_pending, got %T", upd.GetUpdate())
	}
	cp, err := lnd.ChannelPointFromPending(pending.GetTxid(), pending.GetOutputIndex())
	if err != nil {
		t.Fatalf("reading chan_pending outpoint: %v", err)
	}
	return cp
}

// TryFinalize is step 7 for one stream, with the refusal handed back.
//
// Finalize fatals, which is right almost everywhere: psbt_finalize failing is
// not what those tests are about. It is exactly what one test is about — a
// reservation the peer has already swept — and there the refusal is the
// measurement, so it cannot be a t.Fatal. It does not wait for chan_pending:
// a call that was refused has no receipt coming.
func (e *Env) TryFinalize(t *testing.T, s *Stream, rawTxHex string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	rawTx, err := hex.DecodeString(rawTxHex)
	if err != nil {
		t.Fatalf("raw tx is not hex: %v", err)
	}
	_, err = e.Alice.Lightning.FundingStateStep(ctx, &lnrpc.FundingTransitionMsg{
		Trigger: &lnrpc.FundingTransitionMsg_PsbtFinalize{
			PsbtFinalize: &lnrpc.FundingPsbtFinalize{
				PendingChanId: s.PendingChanID.Bytes(),
				FinalRawTx:    rawTx,
			},
		},
	})
	return err
}

// OpenAndConfirmPlainChannel opens a channel the ordinary way — LND funds it
// from its own wallet and broadcasts immediately — then mines it to confirmation.
//
// Only the abort tests use this, and only as the thing that must *not* be
// abandoned. It is the opposite of what the app does, which is the point: it
// produces a confirmed channel carrying no thaw height, so LND's safe flag
// declines it for exactly the same reason it declines a legitimate batch member.
// The pending check is then the only thing standing between a confirmation and a
// channel with no force-close path left.
func (e *Env) OpenAndConfirmPlainChannel(t *testing.T, peerPubkey string, amountSat int64) lnd.ChannelPoint {
	t.Helper()
	cp := e.OpenPlainChannel(t, peerPubkey, amountSat)
	e.Mine(t, 6)

	deadline := time.Now().Add(90 * time.Second)
	for {
		if e.HasOpenChannel(t, cp) {
			return cp
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not confirm within 90s", cp)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// OpenPlainChannel is OpenAndConfirmPlainChannel without the mining.
//
// Split out for the settlement tests, which have to advance the chain one block
// at a time: the depth at which a channel first appears open is an upper bound
// on the peer's own minimum_depth, read from above, and mining six blocks at
// once throws that reading away. It is not the only route to the number — LND
// hands the peer's own figure over on PendingChannels while the funding
// transaction is unconfirmed, which is issue #47 — and that is exactly why it is
// worth keeping: two numbers from two sources, checked against each other on a
// running node.
func (e *Env) OpenPlainChannel(t *testing.T, peerPubkey string, amountSat int64) lnd.ChannelPoint {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	pubkeyBytes, err := hex.DecodeString(peerPubkey)
	if err != nil {
		t.Fatalf("peer pubkey %q is not hex: %v", peerPubkey, err)
	}

	point, err := e.Alice.Lightning.OpenChannelSync(ctx, &lnrpc.OpenChannelRequest{
		NodePubkey:         pubkeyBytes,
		LocalFundingAmount: amountSat,
	})
	if err != nil {
		t.Fatalf("plain OpenChannelSync to %s: %v", peerPubkey[:16], err)
	}

	// OpenChannelSync returns funding_txid_bytes: chainhash order, so reverse.
	cp, err := lnd.ChannelPointFromPending(point.GetFundingTxidBytes(), point.GetOutputIndex())
	if err != nil {
		t.Fatalf("reading the funding outpoint: %v", err)
	}
	return cp
}

// HasOpenChannel reports whether the channel is in alice's set of open channels.
func (e *Env) HasOpenChannel(t *testing.T, cp lnd.ChannelPoint) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := e.Alice.Lightning.ListChannels(ctx, &lnrpc.ListChannelsRequest{})
	if err != nil {
		t.Fatalf("ListChannels: %v", err)
	}
	want := cp.String()
	for _, ch := range resp.GetChannels() {
		if ch.GetChannelPoint() == want {
			return true
		}
	}
	return false
}

// AwaitStreamFailure blocks until LND reports that a funding stream has failed,
// and returns how long that took.
//
// This is what a lapsed ten-minute window looks like from our side: the peer
// stops holding its reservation, tells us so, and our funding manager fails the
// flow, which closes the stream with an error. Only meaningful before
// psbt_finalize — after it the next update on the stream is chan_pending, and
// reading it here would take it away from whoever is waiting for it.
func (e *Env) AwaitStreamFailure(t *testing.T, s *Stream, timeout time.Duration) (
	time.Duration, error) {

	t.Helper()
	started := time.Now()

	type result struct {
		upd *lnrpc.OpenStatusUpdate
		err error
	}
	done := make(chan result, 1)
	go func() {
		upd, err := s.recv.Recv()
		done <- result{upd, err}
	}()

	select {
	case r := <-done:
		if r.err == nil {
			t.Fatalf("the stream produced an update rather than failing: %T",
				r.upd.GetUpdate())
		}
		return time.Since(started), r.err
	case <-time.After(timeout):
		return time.Since(started), nil
	}
}

// RecipientsIn scrapes the step-3 table out of a run's transcript, the way an
// operator's eye does.
//
// It exists because after the inversion the app prints a table and something else
// builds the transaction — so a fixture standing in for that something else has to
// read the table, and reading it is a property worth asserting. The table is not
// how the recipients travel any more; FileWallet writes them as a CSV Sparrow
// loads. It is still what an operator reads to see which peer is getting which
// output, which is the attribution the program exists for, so a table that could
// not be read back would be a broken product however green the seam's own tests
// were. Scraping it here is the only test there is that it is legible.
//
// Anchored on the section heading rather than scanning the whole transcript,
// because everything above it is full of amounts too — the anchor reserve's
// figures, the fee report's — and a scraper that started at the top would pair the
// last of those with the first funding address.
//
// One implementation, in the harness, because two test packages want it and two
// scrapers could disagree about one table.
func RecipientsIn(t *testing.T, transcript string) []coldwallet.Output {
	t.Helper()

	const heading = "Step 3 — the plan"
	i := strings.Index(transcript, heading)
	if i < 0 {
		t.Fatalf("the transcript has no step-3 table in it:\n%s", transcript)
	}

	var got []coldwallet.Output
	var pending int64
	for _, line := range strings.Split(transcript[i+len(heading):], "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		last := fields[len(fields)-1]
		switch {
		case last == "sat" && len(fields) >= 2:
			n, err := strconv.ParseInt(
				strings.ReplaceAll(fields[len(fields)-2], ",", ""), 10, 64)
			if err == nil {
				pending = n
			}
		case strings.HasPrefix(last, "bcrt1") && len(fields) == 1:
			if pending <= 0 {
				t.Fatalf("%s is printed with no amount above it:\n%s", last, transcript)
			}
			got = append(got, coldwallet.Output{Address: last, AmountSat: pending})
			pending = 0
		}
	}
	if len(got) == 0 {
		t.Fatalf("no recipients could be read off the step-4 table:\n%s", transcript)
	}
	return got
}
