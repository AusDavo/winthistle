package combine

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/wire"
)

var (
	// ErrNotATransaction means the bytes are not the network serialization of a
	// transaction, in either hex or binary.
	ErrNotATransaction = errors.New("that is not a raw transaction")

	// ErrIncompleteWitnesses means a raw transaction arrived without a witness on
	// every input.
	//
	// A raw transaction is only useful at step 7 because of what it carries that
	// the base PSBT does not, which is the completed witnesses. One missing any
	// of them cannot be published, and the two ways it can be missing them are
	// different situations for the operator — see SignedFromTX, which
	// distinguishes them in the sentence it writes.
	//
	// One sentinel and not two, because no caller tells them apart: step 7's only
	// caller refuses. Its text was "this transaction carries no signatures",
	// which was the over-claim at the sentinel level — it was returned for the
	// partial case too, where it is plainly false — and the name said the same
	// thing, which is why the name moved with it.
	ErrIncompleteWitnesses = errors.New("this transaction is not completely signed")
)

// ParseTX reads a network-serialized transaction that arrived as either hex text
// or raw bytes.
//
// The counterpart to Parse, and it exists for the same reason: an operator
// moving a file by hand should not have to know which form their wallet wrote.
// Sparrow's *View Final Transaction* yields hex, which is also what lncli is fed
// at the equivalent prompt; a wallet that writes the bytes writes binary.
//
// It is deliberately strict about trailing bytes. A reader that stops early
// would accept a truncated file as a whole transaction, and the txid of a
// truncated transaction is a different txid — which the caller would then report
// as a signer having changed something, rather than as a half-written file.
func ParseTX(body []byte) (*wire.MsgTx, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("it is empty")
	}

	// Hex first, then the bytes themselves. A hex transaction is text and cannot
	// also be a valid binary transaction — its first byte would have to be an
	// ASCII digit, and the version field it would land in is little-endian, so
	// "3..." is version 0x33333333 and the rest does not follow.
	candidates := [][]byte{trimmed}
	if decoded, err := hex.DecodeString(string(unspaced(trimmed))); err == nil {
		candidates = [][]byte{decoded, trimmed}
	}

	var first error
	for _, candidate := range candidates {
		r := bytes.NewReader(candidate)
		tx := &wire.MsgTx{}
		err := tx.Deserialize(r)
		switch {
		case err != nil:
			if first == nil {
				first = err
			}
		case r.Len() != 0:
			if first == nil {
				first = fmt.Errorf("%d bytes are left over after it", r.Len())
			}
		default:
			return tx, nil
		}
	}
	return nil, fmt.Errorf("%w: %v. It starts %q", ErrNotATransaction, first,
		firstBytes(trimmed))
}

// unspaced drops whitespace, so hex that arrived wrapped across lines still
// decodes. Only ever applied to a hex candidate: the binary form is not text and
// a byte that happens to equal 0x20 there is data.
func unspaced(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for _, c := range b {
		switch c {
		case ' ', '\t', '\r', '\n':
		default:
			out = append(out, c)
		}
	}
	return out
}

// SignedFromTX lifts the witnesses off a finalized raw transaction and writes
// them into a packet against the base, so that step 7 can accept either
// encoding while everything downstream of it stays one path.
//
// # Why this exists
//
// The natural output of the signing wallet at step 7 is a finished transaction.
// Sparrow's *View Final Transaction* produces exactly that, lncli takes it at
// the equivalent prompt, and a cold probe lost a batch — two peers' pending
// slots, ~2016 blocks each — to this build being stricter than lncli about a
// wrapper. See issue #3.
//
// Nothing is relaxed by it. The base is the packet this app verified at step 5
// and LND pinned at psbt_verify, and it is where every fact the verifier reasons
// about lives: the witness UTXOs, the witness scripts, the key origins that
// recognise the change output. A raw transaction contributes the one thing the
// base does not have, which is the completed witnesses. What comes back from
// here is a PSBT carrying the base's own unsigned transaction and those
// witnesses, so Accept — the txid pin, the UTXO check, the witness execution
// against each input's own script, the plan re-check — runs over it unchanged.
//
// # Why it is not in Parse
//
// Parse is the sniffer step 4 shares. Teaching it about raw transactions would
// teach step 4 about them too, and step 4 is before the gate: a raw *signed*
// transaction landing there is precisely the packet Unsigned exists to refuse,
// and Unsigned reads a PSBT's partial signatures, which a raw transaction does
// not have. So the acceptance is here, reachable only from step 7's call site.
func SignedFromTX(base, body []byte) ([]byte, error) {
	basePacket, err := psbt.NewFromRawBytes(bytes.NewReader(base), false)
	if err != nil {
		return nil, fmt.Errorf("the base PSBT does not parse: %w", err)
	}
	if err := basePacket.SanityCheck(); err != nil {
		return nil, fmt.Errorf("the base PSBT is malformed: %w", err)
	}

	tx, err := ParseTX(body)
	if err != nil {
		return nil, err
	}

	// I-3, at the first moment it can be asked of these bytes — and asked more
	// directly of a raw transaction than of a PSBT, because a raw transaction's
	// txid is a fact about the thing that would be broadcast rather than about a
	// template inside a wrapper.
	//
	// TxHash is the non-witness serialization, so it covers the version, the
	// inputs' outpoints and sequences, every scriptSig, the outputs and the
	// locktime, and nothing a signature touches. Equal txids therefore mean this
	// is the transaction LND committed to, with signatures added and nothing
	// else changed.
	want := basePacket.UnsignedTx.TxHash().String()
	if got := tx.TxHash().String(); got != want {
		return nil, deviceErr(SigningWalletLabel, "", fmt.Errorf("%w: it is %s, and "+
			"LND committed to %s at psbt_verify. Adding a signature cannot move a "+
			"txid, so something else about the transaction changed — a sequence "+
			"number, the locktime, the version, or the coins it spends. LND will "+
			"accept only added signatures, so this batch cannot be salvaged by "+
			"re-signing; it has to be rebuilt", ErrTXIDMoved, got, want))
	}

	if len(tx.TxIn) != len(basePacket.Inputs) {
		return nil, deviceErr(SigningWalletLabel, "", fmt.Errorf(
			"it has %d inputs for %d", len(tx.TxIn), len(basePacket.Inputs)))
	}

	part, err := psbt.NewFromUnsignedTx(basePacket.UnsignedTx.Copy())
	if err != nil {
		return nil, fmt.Errorf("rebuilding the base's transaction: %w", err)
	}
	// Counted before anything is said about it, which is issue #22. This used to
	// return from inside the lifting loop on the first witnessless input, so what
	// it had established was "input i carries no witness" and what it said was
	// "this transaction carries no signatures … that is the transaction you built
	// at step 4". Both are false of a transaction whose input 0 is signed and
	// whose input 3 is not, and the error named the input in its own device label
	// while the sentence beside it contradicted that.
	//
	// The txid check above makes the claim sharper rather than softer: these
	// bytes *are* the transaction LND committed to, so "that is the step-4 file"
	// is a specific claim about which file the operator saved, made from evidence
	// covering one input.
	var missing []int
	for i, in := range tx.TxIn {
		if len(in.Witness) == 0 {
			missing = append(missing, i)
		}
	}
	switch {
	case len(missing) == len(tx.TxIn):
		// The likely case, and the one the old sentence was written for: the
		// operator saved the unsigned transaction rather than the signed one.
		none := fmt.Sprintf("not one of its %d inputs carries a witness", len(tx.TxIn))
		if len(tx.TxIn) == 1 {
			none = "it carries no witness"
		}
		return nil, deviceErr(SigningWalletLabel, "", fmt.Errorf("%w: %s, so there "+
			"is nothing to take off it. That is the transaction you built at step "+
			"4, not the one you signed. Nothing is lost — every channel is already "+
			"recoverable and nothing has been broadcast — so sign it and save it "+
			"again", ErrIncompleteWitnesses, none))

	case len(missing) > 0:
		// Partly signed. It is demonstrably not the step-4 transaction — that one
		// carries nothing and this one carries something — so the sentence says
		// which inputs are short and stops. Why they are short is not in these
		// bytes: a signer that was never asked and a signer that declined produce
		// the same file, and naming either would be the defect this is fixing.
		return nil, deviceErr(SigningWalletLabel, inputList(missing),
			fmt.Errorf("%w: %d of its %d inputs %s a witness and %s %s not. "+
				"That is not the transaction you built at step 4 — it has "+
				"signatures on it — but it cannot be published as it stands, and "+
				"nothing here can see which signer is missing. Nothing is lost — "+
				"every channel is already recoverable and nothing has been "+
				"broadcast — so finish signing it and save it again",
				ErrIncompleteWitnesses, len(tx.TxIn)-len(missing), len(tx.TxIn),
				agree(len(tx.TxIn)-len(missing), "carries", "carry"),
				inputList(missing), agree(len(missing), "does", "do")))
	}

	for i, in := range tx.TxIn {
		// Only the witness is lifted, and the txid check above is why there is
		// nothing else to lift: a scriptSig is inside the txid, so an input that
		// carried one would have failed that check rather than reached here.
		var wit bytes.Buffer
		if err := psbt.WriteTxWitness(&wit, in.Witness); err != nil {
			return nil, fmt.Errorf("re-encoding input %d's witness: %w", i, err)
		}
		part.Inputs[i].FinalScriptWitness = wit.Bytes()
	}

	var buf bytes.Buffer
	if err := part.Serialize(&buf); err != nil {
		return nil, fmt.Errorf("serialising the signed packet: %w", err)
	}
	return buf.Bytes(), nil
}

// inputList names the inputs a refusal is about, in the operator's terms.
//
// The whole of issue #22 is that this program knew exactly which inputs were
// short while telling the operator something else, so the list is the fix and
// not a decoration.
func inputList(idx []int) string {
	switch len(idx) {
	case 0:
		return ""
	case 1:
		return fmt.Sprintf("input %d", idx[0])
	}
	parts := make([]string, len(idx))
	for i, n := range idx {
		parts[i] = strconv.Itoa(n)
	}
	return "inputs " + strings.Join(parts[:len(parts)-1], ", ") + " and " +
		parts[len(parts)-1]
}

// agree keeps a counted refusal grammatical. Not prose.Plural: this package
// renders no reports, and importing prose for a word would be the wrong
// dependency.
//
// Worth its own function. These sentences exist to say exactly how many inputs
// were short, and "1 of its 4 inputs carry a witness" undermines the count in
// the same breath as making it.
func agree(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
