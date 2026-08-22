package regtestenv

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/AusDavo/winthistle/internal/bitcoind"
	"github.com/AusDavo/winthistle/internal/lnd"
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
	Inputs  []bitcoind.Outpoint // locked by Core when lockUnspents is set
	ChangeI int
}

// BuildFundingPSBT performs step 4: one unsigned PSBT with an output for every
// stream, funded and change-derived by a Core wallet.
//
// wallet chooses the funding source. Tests that only need a *shape* — anything
// up to psbt_verify — use the watch-only cold wallet, exactly as production
// does. Tests that need a signature use the miner wallet, which holds keys; see
// SignWithMiner for why that is acceptable in a fixture and not in the app.
func (e *Env) BuildFundingPSBT(t *testing.T, wallet *bitcoind.Client,
	streams []*Stream, feeRate float64) FundedPSBT {

	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	outputs := make([]map[string]any, 0, len(streams))
	for _, s := range streams {
		// Core wants BTC. The funding amount is exact — LND checks its own
		// output is present at the satoshi, so this must not be rounded.
		outputs = append(outputs, map[string]any{
			s.FundingAddress: btcFromSat(s.FundingAmount),
		})
	}

	var built struct {
		PSBT      string `json:"psbt"`
		ChangePos int    `json:"changepos"`
	}
	opts := map[string]any{
		"fee_rate": feeRate,
		// Core then holds the chosen coins unspendable, which is the state the
		// UTXO-lock release exists to undo.
		"lockUnspents": true,
		// I-4: never signal replaceability. Replacing the funding transaction
		// moves every outpoint and destroys every channel in the batch.
		"replaceable": false,
	}
	err := wallet.Call(ctx, "walletcreatefundedpsbt",
		[]any{[]any{}, outputs, 0, opts, true}, &built)
	if err != nil {
		t.Fatalf("walletcreatefundedpsbt: %v", err)
	}

	return FundedPSBT{
		Base64:  built.PSBT,
		Inputs:  e.psbtInputs(t, wallet, built.PSBT),
		ChangeI: built.ChangePos,
	}
}

// psbtInputs reads the outpoints the PSBT spends, which are the ones Core just
// locked.
func (e *Env) psbtInputs(t *testing.T, wallet *bitcoind.Client, psbtB64 string) []bitcoind.Outpoint {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var decoded struct {
		Tx struct {
			Vin []struct {
				TxID string `json:"txid"`
				Vout uint32 `json:"vout"`
			} `json:"vin"`
		} `json:"tx"`
	}
	if err := wallet.Call(ctx, "decodepsbt", []any{psbtB64}, &decoded); err != nil {
		t.Fatalf("decodepsbt: %v", err)
	}
	var out []bitcoind.Outpoint
	for _, in := range decoded.Tx.Vin {
		out = append(out, bitcoind.Outpoint{TxID: in.TxID, Vout: in.Vout})
	}
	return out
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

// SignWithMiner signs and finalizes the PSBT with Core's miner wallet, returning
// the raw transaction hex.
//
// This is a fixture shortcut and it deliberately does NOT model I-2: the miner
// wallet holds a single key and can complete the transaction alone, so for the
// duration of this call one external party does hold a broadcastable
// transaction. That is tolerable here only because these tests exist to tear
// batches down, they never publish, and each asserts the funding transaction
// stayed out of the mempool.
//
// The real path — m partial signatures combined and finalized in-app, with no
// party ever holding a complete transaction — belongs to the funding flow, and
// `make -C regtest verify` covers the 2-of-2 fixture it will use. Do not reach
// for this function when that arrives.
func (e *Env) SignWithMiner(t *testing.T, psbtB64 string) (rawTxHex, txid string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var processed struct {
		PSBT     string `json:"psbt"`
		Complete bool   `json:"complete"`
	}
	err := e.Miner.Call(ctx, "walletprocesspsbt",
		[]any{psbtB64, true, "ALL", false}, &processed)
	if err != nil {
		t.Fatalf("walletprocesspsbt: %v", err)
	}
	if !processed.Complete {
		t.Fatalf("miner wallet could not complete the psbt")
	}

	var final struct {
		Hex      string `json:"hex"`
		Complete bool   `json:"complete"`
	}
	if err := e.Miner.Call(ctx, "finalizepsbt", []any{processed.PSBT}, &final); err != nil {
		t.Fatalf("finalizepsbt: %v", err)
	}
	if !final.Complete || final.Hex == "" {
		t.Fatalf("finalizepsbt did not complete")
	}

	// testmempoolaccept validates without relaying — the only pre-flight there
	// is, and specifically not a broadcast.
	var accept []struct {
		Allowed      bool   `json:"allowed"`
		TxID         string `json:"txid"`
		RejectReason string `json:"reject-reason"`
	}
	err = e.Node.Call(ctx, "testmempoolaccept", []any{[]string{final.Hex}}, &accept)
	if err != nil {
		t.Fatalf("testmempoolaccept: %v", err)
	}
	if len(accept) != 1 || !accept[0].Allowed {
		t.Fatalf("testmempoolaccept refused the funding tx: %+v", accept)
	}
	return final.Hex, accept[0].TxID
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
	if err != nil {
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

// btcFromSat renders satoshis as a BTC amount Core will parse without loss.
// A float would be wrong here for the same reason it is wrong everywhere else in
// Bitcoin, so the conversion is done as text.
func btcFromSat(sat int64) string {
	neg := ""
	if sat < 0 {
		neg, sat = "-", -sat
	}
	return fmt.Sprintf("%s%d.%08d", neg, sat/1e8, sat%1e8)
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
