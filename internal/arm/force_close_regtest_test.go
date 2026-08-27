package arm_test

import (
	"context"
	"testing"
	"time"

	"github.com/AusDavo/winthistle/internal/arm"
	"github.com/AusDavo/winthistle/internal/lnd"
	"github.com/AusDavo/winthistle/internal/regtestenv"
	"github.com/lightningnetwork/lnd/lnrpc"
)

// TestTheReceiptIsProvedByForceClosingTheChannel executes the one claim the whole
// safety model rests on, rather than reading it.
//
// I-1 says each chan_pending is a receipt that the channel is already recoverable
// by force-close. Until this test that was *inferred*, out of
// funding/manager.go at v0.21.2-beta: funderProcessFundingSigned calls
// CompleteReservation(nil, commitSig) at :2813, storing the peer's commitment
// signature, and emits chan_pending at :2897, after it. Correct, checkable, and
// exercised by nothing.
//
// The failure that mattered was the silent one. If skip_finalize or no_publish
// stops behaving, TestSkipFinalizeReachesChanPendingWithNothingSigned fails
// loudly — no receipts, or a transaction in a mempool that should be clear. If
// the *ordering* moves, so that chan_pending is emitted before the signature is
// stored, the receipt still arrives, looks identical, and every other assertion
// in this package still passes. What changed is what the receipt means.
//
// So: arm a batch the way the app does, publish it, mine it to confirmation, and
// then force-close the channel with the operator's own tool. A unilateral close
// broadcasts our local commitment transaction, whose single input is the 2-of-2
// funding output. Nothing here asks the peer for anything, and nothing could: the
// peer's signature either arrived before the receipt or it did not exist.
//
// The judge is Bitcoin Core, not LND. The assertion is that the commitment
// reaches the mempool and then a block, spending the funding outpoint with a
// four-element P2WSH witness over a 2-of-2 script. Consensus checked both
// signatures against that script, and this node holds exactly one of the two
// keys. LND saying the close succeeded would prove much less.
//
// What was checked about the test itself, and what turned out not to exist. The
// obvious negative control is that this should fail if the force-close is
// attempted before the funding transaction confirms — a chan_pending channel is
// recoverable *once the funding transaction confirms*, and before that there is
// no channel, only a promise. It was tried, against this harness at
// v0.21.2-beta, and LND does not refuse it: it accepts a unilateral close on a
// pending channel, marks it ChanStatusBorked|ChanStatusCommitBroadcasted, and
// broadcasts the commitment as a mempool child of the unconfirmed funding
// transaction, which Core accepts. (Which is one more reason internal/abort
// abandons rather than closes, and CloseChannel is never-listed: on an
// *unpublished* batch that same close produces a commitment whose parent does not
// exist anywhere.) So confirming first is not what makes this test valid — it is
// what makes it a test of the state the claim is about. What discriminates is
// Core, below, and the assertion is deliberately specific about the shape of what
// it mined.
//
// Where this stops, and why. The issue's shape ended by mining past to_self_delay
// and asserting the swept output lands back in alice's wallet. That is a second
// claim — "the funds come back", through LND's own sweeper, over an anchor
// channel, across several transactions it batches at its own discretion — and it
// fails for reasons that have nothing to do with the funding ordering. A test
// that can fail for two unrelated causes names neither. The commitment reaching a
// block is the whole of what the receipt promises; the CSV delay after it is
// logged below and deliberately not waited out.
//
// Not gated behind WINTHISTLE_SLOW. That gate exists for clock A, which is
// wall-clock and cannot be mined forward (internal/arm/clock_regtest_test.go).
// Everything here is mining, which is seconds, and a gated test is a test that
// does not run.
func TestTheReceiptIsProvedByForceClosingTheChannel(t *testing.T) {
	w := drive(t, 1)
	env, ctx := w.env, harnessCtx(t)

	if len(w.armed.Channels) != 1 {
		t.Fatalf("armed with %d channels, expected 1", len(w.armed.Channels))
	}
	cp := w.armed.Channels[0]

	// Step 8, once. drive() has already held the gate: the receipt arrived over an
	// unsigned transaction and the mempool was watched throughout.
	assertMempoolEmpty(t, env, w.final.TxID, "with the batch armed and signed")
	if err := arm.Publish(ctx, env.Alice.WalletKit, w.j, w.armed, w.final.RawTx); err != nil {
		t.Fatalf("publishing: %v", err)
	}
	if !env.InMempool(t, w.final.TxID) {
		t.Fatalf("%s is not in the mempool after the publish call returned", w.final.TxID)
	}

	// Confirmation first, because a channel is recoverable once its funding
	// transaction confirms and not before — see the doc comment for what LND does
	// if you ignore that, which is not what this test originally assumed.
	env.Mine(t, 6)
	awaitOpen(t, env, cp)

	// Read rather than assumed, because a constant like this rots quietly. Logged
	// rather than waited out: see the doc comment.
	t.Logf("%s is open; our to_self_delay is %d blocks, which this test "+
		"deliberately does not mine past", cp, csvDelayOf(ctx, t, env, cp))

	// Out of band of the Go client entirely — see ForceCloseOutOfBand. CloseChannel
	// is on internal/methods' never-list, and this repository stays incapable of
	// calling it.
	commitTxID := env.ForceCloseOutOfBand(t, aliceNode, cp)
	t.Logf("force-closed %s; LND broadcast commitment %s", cp, commitTxID)

	awaitMempool(t, env, commitTxID)
	commitment := rawTransaction(ctx, t, env, commitTxID)
	assertSpendsTheFundingOutput(t, commitment, cp)

	// And a block, which is the assertion. Core validated the 2-of-2 witness to
	// accept it, and validated it again to mine it.
	env.Mine(t, 1)
	if mined := rawTransaction(ctx, t, env, commitTxID); mined.Confirmations < 1 {
		t.Fatalf("the commitment transaction %s is not confirmed after a block",
			commitTxID)
	}
	t.Logf("commitment %s confirmed, spending %s with a 2-of-2 witness. The "+
		"chan_pending receipt for this channel was a promise the peer's "+
		"signature was already stored, and it was", commitTxID, cp)
}

// aliceNode is the harness's name for our own node, which is the one whose
// channel is being closed. regtest/bin/lncli takes it as its first argument.
const aliceNode = "alice"

// awaitOpen blocks until alice lists the channel as open.
func awaitOpen(t *testing.T, env *regtestenv.Env, cp lnd.ChannelPoint) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		if env.HasOpenChannel(t, cp) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not open within two minutes of confirmation", cp)
		}
		time.Sleep(time.Second)
	}
}

// csvDelayOf reads our own to_self_delay off ListChannels.
//
// local_constraints, not Channel.csv_delay, which lightning.proto marks
// deprecated at v0.21.2-beta.
func csvDelayOf(ctx context.Context, t *testing.T, env *regtestenv.Env,
	cp lnd.ChannelPoint) uint32 {

	t.Helper()
	resp, err := env.Alice.Lightning.ListChannels(ctx, &lnrpc.ListChannelsRequest{})
	if err != nil {
		t.Fatalf("ListChannels: %v", err)
	}
	for _, ch := range resp.GetChannels() {
		if ch.GetChannelPoint() == cp.String() {
			return ch.GetLocalConstraints().GetCsvDelay()
		}
	}
	t.Fatalf("%s is not in alice's open channels", cp)
	return 0
}

// awaitMempool blocks until Core has that transaction in its mempool.
//
// LND broadcasts the commitment from the ChainArbitrator after CloseChannel's
// ClosePending update has already been sent, so lncli can return a txid a moment
// before Core has the bytes.
func awaitMempool(t *testing.T, env *regtestenv.Env, txid string) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for {
		if env.InMempool(t, txid) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the commitment transaction %s never reached the mempool. "+
				"A commitment that cannot be broadcast is a channel that was "+
				"never recoverable, which is I-1 failing", txid)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// rawTx is as much of Core's getrawtransaction as this test reads.
type rawTx struct {
	TxID          string `json:"txid"`
	Confirmations int64  `json:"confirmations"`
	Vin           []struct {
		TxID    string   `json:"txid"`
		Vout    uint32   `json:"vout"`
		Witness []string `json:"txinwitness"`
	} `json:"vin"`
}

func rawTransaction(ctx context.Context, t *testing.T, env *regtestenv.Env,
	txid string) rawTx {

	t.Helper()
	var tx rawTx
	if err := env.Node.Call(ctx, "getrawtransaction", []any{txid, true}, &tx); err != nil {
		t.Fatalf("getrawtransaction %s: %v", txid, err)
	}
	return tx
}

// assertSpendsTheFundingOutput is the shape of the evidence.
//
// One input, and it is the outpoint chan_pending named. Its witness is the
// four-element P2WSH form — an empty item for CHECKMULTISIG's off-by-one, two
// signatures, and the witness script — over a script that begins OP_2 and ends
// OP_CHECKMULTISIG. Alice holds one of those two keys. Core accepted the
// transaction, so the other signature was there and valid, and the only moment
// the peer ever provided it was funding_signed, before the receipt.
func assertSpendsTheFundingOutput(t *testing.T, tx rawTx, cp lnd.ChannelPoint) {
	t.Helper()
	if len(tx.Vin) != 1 {
		t.Fatalf("the commitment transaction %s has %d inputs, expected the one "+
			"funding output", tx.TxID, len(tx.Vin))
	}
	in := tx.Vin[0]
	if in.TxID != cp.TxID || in.Vout != cp.Index {
		t.Fatalf("the commitment transaction spends %s:%d, and the channel's "+
			"funding outpoint is %s", in.TxID, in.Vout, cp)
	}
	if n := len(in.Witness); n != 4 {
		t.Fatalf("the commitment's witness has %d elements, and a P2WSH 2-of-2 "+
			"spend has four: %v", n, in.Witness)
	}
	if in.Witness[0] != "" {
		t.Errorf("the commitment's witness does not begin with the empty element "+
			"a CHECKMULTISIG spend needs: %q", in.Witness[0])
	}
	script := in.Witness[3]
	if len(script) < 4 || script[:2] != "52" || script[len(script)-2:] != "ae" {
		t.Errorf("the witness script is not OP_2 … OP_CHECKMULTISIG, so what "+
			"consensus checked was not a 2-of-2: %s", script)
	}
	for i, sig := range in.Witness[1:3] {
		if sig == "" {
			t.Errorf("witness signature %d is empty", i+1)
		}
	}
}
