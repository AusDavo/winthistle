package combine_test

import (
	"bytes"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/AusDavo/winthistle/internal/combine"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
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
	// Re-keyed with the sentinel, which is ErrIncompleteWitnesses now: its old
	// name and its old text — "this transaction carries no signatures" — were
	// returned for the partly-signed case too, where both are false. Asserting
	// on the identifier is why this failed to compile rather than passing in
	// silence when the rename landed.
	if _, err := combine.SignedFromTX(b.base, buf.Bytes()); !errors.Is(err,
		combine.ErrIncompleteWitnesses) {

		t.Fatalf("an unsigned transaction was accepted at step 7, or refused for "+
			"another reason: %v", err)
	}
}

// TestWhatIsSaidAboutAMissingWitnessIsWhatWasCounted is issue #22.
//
// Three states, all knowable from the bytes in hand, and the refusal used to
// return from inside the lifting loop on the first witnessless input — so it
// established "input i is short" and said "this transaction carries no
// signatures … that is the transaction you built at step 4". For a transaction
// with input 0 signed and input 3 not, both of those are false.
//
// Node-free on purpose. What is under test is the counting and the sentence, and
// SignedFromTX validates no signature — combine.Accept is what executes a
// witness against its script, and it runs after this. So the witnesses here are
// bytes, and the fixture is a plain n-input transaction rather than the package's
// single-input 2-of-2.
func TestWhatIsSaidAboutAMissingWitnessIsWhatWasCounted(t *testing.T) {
	for _, tc := range []struct {
		name    string
		inputs  int
		signed  []int
		want    []string
		notWant []string
	}{
		{
			name: "none of them", inputs: 3, signed: nil,
			want: []string{
				"not one of its 3 inputs carries a witness",
				"That is the transaction you built at step 4",
				"sign it and save it again",
			},
		},
		{
			name: "one of four", inputs: 4, signed: []int{2},
			want: []string{
				"1 of its 4 inputs carries a witness",
				"inputs 0, 1 and 3 do not",
				"not the transaction you built at step 4",
				"nothing here can see which signer is missing",
				"finish signing it and save it again",
			},
			// The claims the count refutes.
			notWant: []string{
				"carries no signatures",
				"there is nothing to take off it",
			},
		},
		{
			name: "all but the last", inputs: 2, signed: []int{0},
			want: []string{"1 of its 2 inputs carries a witness", "input 1 does not"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base, body := witnessFixture(t, tc.inputs, tc.signed)
			_, err := combine.SignedFromTX(base, body)
			if !errors.Is(err, combine.ErrIncompleteWitnesses) {
				t.Fatalf("a transaction missing a witness was accepted, or refused "+
					"for another reason: %v", err)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not say %q: %v", want, err)
				}
			}
			for _, gone := range tc.notWant {
				if strings.Contains(err.Error(), gone) {
					t.Errorf("the refusal claims %q about a transaction that carries "+
						"signatures: %v", gone, err)
				}
			}
		})
	}

	// And the third state, which is the one that carries on.
	t.Run("all of them", func(t *testing.T) {
		base, body := witnessFixture(t, 3, []int{0, 1, 2})
		signed, err := combine.SignedFromTX(base, body)
		if err != nil {
			t.Fatalf("a transaction with a witness on every input was refused: %v", err)
		}
		for i, in := range parse(t, signed).Inputs {
			if len(in.FinalScriptWitness) == 0 {
				t.Errorf("input %d's witness was not lifted onto the packet", i)
			}
		}
	})
}

// witnessFixture is an n-input transaction and the base packet it was built
// from, with a witness on the inputs named and nothing on the rest.
func witnessFixture(t *testing.T, inputs int, signed []int) (base, body []byte) {
	t.Helper()

	tx := wire.NewMsgTx(2)
	for i := 0; i < inputs; i++ {
		tx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: wire.OutPoint{
				Hash:  chainhash.Hash{0xab, byte(i)},
				Index: uint32(i),
			},
			Sequence: wire.MaxTxInSequenceNum,
		})
	}
	_, script := freshP2WPKH(t)
	tx.AddTxOut(&wire.TxOut{Value: 500_000, PkScript: script})

	packet, err := psbt.NewFromUnsignedTx(tx.Copy())
	if err != nil {
		t.Fatalf("building the base packet: %v", err)
	}
	for i := range packet.Inputs {
		packet.Inputs[i].WitnessUtxo = &wire.TxOut{Value: 400_000, PkScript: script}
	}

	for _, i := range signed {
		// Bytes, not a signature. Nothing between here and combine.Accept looks
		// at what a witness says, and a real one would suggest otherwise.
		tx.TxIn[i].Witness = wire.TxWitness{[]byte{0x30, 0x44}, []byte{0x02, 0xff}}
	}
	var buf bytes.Buffer
	if err := tx.Serialize(&buf); err != nil {
		t.Fatalf("serialising the fixture: %v", err)
	}
	return serialize(t, packet), buf.Bytes()
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
