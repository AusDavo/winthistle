package combine_test

import (
	"bytes"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/AusDavo/winthistle/internal/combine"
	"github.com/btcsuite/btcd/wire"
)

// Step 7 reads a raw transaction as well as a PSBT, and these are the tests that
// say what that does and does not change. The fixture is the same real 2-of-2
// P2WSH spend the rest of the package uses, so the witnesses lifted off the
// transaction here are witnesses that actually satisfy a script.

// finalizedRawTX is what the operator would be holding at step 7: the finished
// transaction, which is what Sparrow's View Final Transaction shows.
func (b *batch) finalizedRawTX(t *testing.T) []byte {
	t.Helper()
	final, _, err := combine.Complete(b.plan, b.base,
		[]combine.Part{b.sign(t, 0), b.sign(t, 1)})
	if err != nil {
		t.Fatalf("building the fixture's signed transaction: %v", err)
	}
	return final.RawTx
}

// TestStepSevenTakesAFinalizedRawTransaction is issue #3.
//
// A finalized raw transaction carries the one thing the base PSBT does not,
// which is the completed witnesses, and everything else the verifier reasons
// about is already in the base. So the two encodings have to reach the same
// place — the same bytes, the same txid, the same fee, the same size — or the
// relaxation would be a second path to a different answer rather than a wrapper
// being read.
func TestStepSevenTakesAFinalizedRawTransaction(t *testing.T) {
	w := newWallet(t, 2, 2)
	b := newBatch(t, w)
	rawTX := b.finalizedRawTX(t)

	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"binary", rawTX},
		// Sparrow's View Final Transaction, and lncli's second prompt: hex text,
		// with the newline an editor or a shell redirect leaves on the end.
		{"hex", []byte(hex.EncodeToString(rawTX) + "\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			signed, err := combine.SignedFromTX(b.base, tc.body)
			if err != nil {
				t.Fatalf("a correctly signed transaction was refused: %v", err)
			}
			final, v, err := combine.Accept(b.plan, b.base, signed)
			if err != nil {
				t.Fatalf("Accept refused the packet the raw transaction produced: %v", err)
			}
			if !v.OK() {
				t.Fatalf("the plan refused it:\n%s", v.Report())
			}
			if !bytes.Equal(final.RawTx, rawTX) {
				t.Error("the raw-transaction path produced different bytes from the " +
					"PSBT path, so which encoding the operator exported would decide " +
					"what gets broadcast")
			}
			if got, want := final.TxID, b.packet.UnsignedTx.TxHash().String(); got != want {
				t.Errorf("txid is %s, want the pinned %s", got, want)
			}
			if final.FeeSat != feeSat {
				t.Errorf("fee is %d sat, want %d", final.FeeSat, feeSat)
			}
			if final.Vsize != v.Size.Vsize {
				t.Errorf("the verifier sizes it at %d vB and it is %d vB",
					v.Size.Vsize, final.Vsize)
			}
			if len(final.Signers) != 1 {
				t.Errorf("Signers = %v, want the one wallet that returned it",
					final.Signers)
			}
		})
	}
}

// TestARawTransactionWhoseTXIDMovedIsRefused is I-3 on the new path.
//
// LND committed to the funding outpoints at psbt_verify and n peers are holding
// commitment signatures against them. A signature cannot move a txid, so one
// that moved means something else changed — here a sequence number, which is
// exactly what LND's own FinalizeRawTX does not check.
func TestARawTransactionWhoseTXIDMovedIsRefused(t *testing.T) {
	w := newWallet(t, 2, 2)
	b := newBatch(t, w)

	tx := &wire.MsgTx{}
	if err := tx.Deserialize(bytes.NewReader(b.finalizedRawTX(t))); err != nil {
		t.Fatal(err)
	}
	tx.TxIn[0].Sequence = wire.MaxTxInSequenceNum - 2
	var buf bytes.Buffer
	if err := tx.Serialize(&buf); err != nil {
		t.Fatal(err)
	}

	_, err := combine.SignedFromTX(b.base, buf.Bytes())
	if !errors.Is(err, combine.ErrTXIDMoved) {
		t.Fatalf("a transaction that is not the pinned one was accepted, or refused "+
			"for another reason: %v", err)
	}
	if !bytes.Contains([]byte(err.Error()), []byte(b.packet.UnsignedTx.TxHash().String())) {
		t.Errorf("the refusal does not say which txid LND committed to: %v", err)
	}
}

// TestAnUnsignedRawTransactionAtStepSevenIsRefused.
//
// The likeliest way it happens: the operator exports the transaction rather than
// the signed one, from a wallet that offers both. It has the right txid — it is
// the right transaction — and it carries nothing, so there is nothing to lift
// off it and saying so is the whole of the fix.
func TestAnUnsignedRawTransactionAtStepSevenIsRefused(t *testing.T) {
	w := newWallet(t, 2, 2)
	b := newBatch(t, w)

	var buf bytes.Buffer
	if err := b.packet.UnsignedTx.Serialize(&buf); err != nil {
		t.Fatal(err)
	}
	if _, err := combine.SignedFromTX(b.base, buf.Bytes()); !errors.Is(err,
		combine.ErrNoWitnesses) {

		t.Fatalf("an unsigned transaction was accepted at step 7, or refused for "+
			"another reason: %v", err)
	}
}

// TestSomethingThatIsNeitherIsNamedAsNeither. A refusal that says "not a PSBT"
// about a file that was never meant to be one sends the operator to check the
// wrong half of their export.
func TestSomethingThatIsNeitherIsNamedAsNeither(t *testing.T) {
	w := newWallet(t, 2, 2)
	b := newBatch(t, w)

	if _, err := combine.SignedFromTX(b.base, []byte("signed it, all good\n")); !errors.Is(
		err, combine.ErrNotATransaction) {

		t.Fatalf("a note from the operator parsed as something: %v", err)
	}

	// Truncation is the case a lenient reader gets wrong: it decodes as a whole
	// transaction with a different txid, which would be reported as a signer
	// having changed something rather than as a half-written file.
	raw := b.finalizedRawTX(t)
	if _, err := combine.ParseTX(append(append([]byte(nil), raw...), 0x00)); !errors.Is(
		err, combine.ErrNotATransaction) {

		t.Fatalf("trailing bytes after a transaction were ignored: %v", err)
	}
}
