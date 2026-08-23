package regtestenv

import (
	"context"
	"testing"
	"time"

	"github.com/AusDavo/winthistle/internal/bitcoind"
	"github.com/AusDavo/winthistle/internal/combine"
)

// The two key-holding halves of regtest/cold-wallet.py's simulated 2-of-2. Each
// holds one private key and the other's public key, so neither can complete a
// transaction alone — which is the property invariant I-2 depends on, and the
// reason this fixture exists rather than a single-key one.
const (
	Cold1 = "cold1"
	Cold2 = "cold2"
)

// ColdSigners are the labels the fixture signs with, in order.
func ColdSigners() []string { return []string{Cold1, Cold2} }

// SignPartial signs a PSBT with one half of the simulated cold wallet and
// returns what that half hands back.
//
// finalize is false, which is the whole point. Core's walletprocesspsbt will
// combine and finalize for you if you let it, and letting it would put a
// complete transaction in a wallet outside the app — exactly what I-2 forbids.
// With finalize=false each signer returns a PSBT carrying only its own partial
// signature, and combining them is internal/combine's job.
//
// It fails the test if the signer completed the transaction on its own, because
// a fixture where one signer is enough would quietly stop testing the thing it
// exists to test.
func (e *Env) SignPartial(t *testing.T, signer, psbtB64 string) combine.Part {
	t.Helper()
	part, complete := e.trySignPartial(t, signer, psbtB64)
	if complete {
		t.Fatalf("%s completed the PSBT alone. The 2-of-2 fixture is not a 2-of-2, "+
			"so nothing this test says about I-2 means anything — run: make harness",
			signer)
	}
	return part
}

// trySignPartial is SignPartial for a test whose subject is the completeness
// flag itself.
func (e *Env) trySignPartial(t *testing.T, signer, psbtB64 string) (combine.Part, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	wallet := e.WalletClient(t, signer, 0)
	var out struct {
		PSBT     string `json:"psbt"`
		Complete bool   `json:"complete"`
	}
	// psbt, sign=true, sighashtype=ALL, bip32derivs=false, finalize=false.
	err := wallet.Call(ctx, "walletprocesspsbt",
		[]any{psbtB64, true, "ALL", false, false}, &out)
	if err != nil {
		t.Fatalf("%s walletprocesspsbt: %v", signer, err)
	}
	raw, err := combine.ParseBase64(out.PSBT)
	if err != nil {
		t.Fatalf("%s returned a PSBT this build cannot read: %v", signer, err)
	}
	return combine.Part{Label: signer, PSBT: raw}, out.Complete
}

// SignWithColdWallet collects a partial signature from each half of the cold
// wallet, in the order an operator would visit them.
//
// This is the real signing path: m partials, none of them complete, combined in
// the app. It replaced a helper that signed with one key so the abort fixtures
// had a complete transaction, which did not model I-2 — see SignAndCombine.
func (e *Env) SignWithColdWallet(t *testing.T, psbtB64 string) []combine.Part {
	t.Helper()
	var parts []combine.Part
	for _, signer := range ColdSigners() {
		parts = append(parts, e.SignPartial(t, signer, psbtB64))
	}
	return parts
}

// AcceptsToMempool runs testmempoolaccept over a raw transaction.
//
// Validates without relaying, which is the only pre-flight there is and
// specifically not a broadcast. Returns Core's own reject reason when it says no,
// because the reason is usually the answer.
func (e *Env) AcceptsToMempool(t *testing.T, rawTxHex string) (bool, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var accept []struct {
		Allowed      bool   `json:"allowed"`
		TxID         string `json:"txid"`
		Vsize        int64  `json:"vsize"`
		RejectReason string `json:"reject-reason"`
	}
	if err := e.Node.Call(ctx, "testmempoolaccept", []any{[]string{rawTxHex}}, &accept); err != nil {
		t.Fatalf("testmempoolaccept: %v", err)
	}
	if len(accept) != 1 {
		t.Fatalf("testmempoolaccept answered about %d transactions", len(accept))
	}
	if accept[0].Allowed {
		t.Logf("testmempoolaccept: allowed, %d vB", accept[0].Vsize)
	}
	return accept[0].Allowed, accept[0].RejectReason
}

// ReleaseLocksAtCleanup gives Core's coin locks back when the test ends.
//
// Directed mode locks the batch's inputs at walletcreatefundedpsbt and the abort
// path releases them; a test that leaves them held leaves a wallet that silently
// refuses to spend its own money, and the next test sees "insufficient funds".
func (e *Env) ReleaseLocksAtCleanup(t *testing.T, wallet *bitcoind.Client,
	ops []bitcoind.Outpoint) {

	t.Helper()
	if len(ops) == 0 {
		return
	}
	t.Cleanup(func() {
		ctx, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		if _, err := wallet.ReleaseLocks(ctx, ops); err != nil {
			t.Errorf("releasing %d coin lock(s): %v", len(ops), err)
		}
	})
}
