package combine

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"github.com/AusDavo/winthistle/internal/plan"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

var (
	// ErrNotEnoughSignatures means an input has fewer signatures than its script
	// requires. Nothing has been published and nothing has been finalized, so
	// the cost is another signing round.
	ErrNotEnoughSignatures = errors.New("not enough signatures to finalize")

	// ErrTooManySignatures means an input has more signatures than its script
	// requires.
	//
	// Harmless on the network — a 2-of-3 spend with three signatures is simply
	// not a thing the script can express — but btcd's finalizer refuses it, with
	// "Unsupported script type", because checkIsMultiSigScript insists that the
	// number of signatures equal the number the script demands. Better to say so
	// with the numbers in hand.
	ErrTooManySignatures = errors.New("more signatures than the script requires")

	// ErrWitnessInvalid means the finalized witness does not satisfy the input's
	// script when it is actually executed.
	//
	// This is the check the design says stands in for testmempoolaccept in
	// assisted mode: "executing every input's script locally against the witness
	// data answers the question we actually care about — will each input
	// validate — more directly than a mempool acceptance test does, and needs no
	// chain data".
	ErrWitnessInvalid = errors.New("an input's witness does not satisfy its script")

	// ErrTXIDMoved is I-3 at the last possible moment: the transaction that came
	// out of the extractor is not the one LND committed to.
	ErrTXIDMoved = errors.New("the extracted transaction's txid is not the one that was verified")

	// ErrPlanBroken means the extracted transaction fails the batch plan.
	ErrPlanBroken = errors.New("the extracted transaction does not match the plan")
)

// Finalized is the transaction the merge produced, checked.
//
// Everything on it is a fact about the bytes in RawTx rather than about the
// packet that produced them: the witnesses have been executed against their
// inputs' scripts, and the txid is the one that was verified.
type Finalized struct {
	// RawTx is the network serialization: what the journal stores and what
	// PublishTransaction eventually broadcasts — one set of bytes, produced once,
	// so nothing downstream can re-derive it slightly differently. Nothing hands
	// these to LND's funding flow; there is no psbt_finalize call in this build,
	// because a skip_finalize verify has already left the intent finalized.
	RawTx []byte

	// TxID is the transaction's txid, in the byte order humans and Core use. It
	// equals the unsigned txid LND committed to at psbt_verify: adding witnesses
	// cannot move it, and this is where that is checked rather than assumed.
	TxID string

	// Vsize is exact. Every witness is present, so there is nothing to estimate.
	Vsize int64

	// FeeSat is the fee the transaction actually pays.
	FeeSat int64

	// Signers are the labels that contributed at least one signature.
	Signers []string

	// Tx is the parsed transaction, for a caller that needs to look at it.
	Tx *wire.MsgTx

	// packet is the finalized PSBT, and view is the same transaction expressed
	// the way internal/plan's verifier wants it. Unexported because the useful
	// artifact is RawTx and a caller reaching past it would be re-deriving the
	// bytes that matter.
	packet *psbt.Packet
	view   *psbt.Packet
}

// Finalize finalizes the merged packet, extracts the transaction, and executes
// every input's witness against its own script.
//
// The order is deliberate. Signature counts are checked before MaybeFinalizeAll
// so the operator hears about their wallet rather than about btcd; the witnesses
// are executed after extraction, against the serialized bytes, so what was
// checked is what would be broadcast. Nothing here touches the network, and
// nothing here calls Core.
func Finalize(m *Merged) (*Finalized, error) {
	if m == nil || m.Packet == nil {
		return nil, fmt.Errorf("nothing to finalize")
	}

	// Snapshot before finalizing. btcd's finalizeWitnessInput replaces the whole
	// input with NewPsbtInput(nil, pInput.WitnessUtxo) plus the final witness, so
	// the redeem script, the witness script and any non-witness UTXO are gone
	// afterwards — and those are exactly what internal/plan needs to classify the
	// input and size the transaction.
	before := copyPacket(m.Packet)

	if err := checkSignatureCounts(m.Packet, m.Contributors); err != nil {
		return nil, err
	}

	packet := copyPacket(m.Packet)
	if err := psbt.MaybeFinalizeAll(packet); err != nil {
		return nil, fmt.Errorf("finalizing the combined transaction: %w", err)
	}
	if !packet.IsComplete() {
		return nil, fmt.Errorf("%w: the finalizer reported no error but %s",
			ErrNotEnoughSignatures, unfinalized(packet))
	}

	tx, err := psbt.Extract(packet)
	if err != nil {
		return nil, fmt.Errorf("extracting the transaction: %w", err)
	}

	var buf bytes.Buffer
	if err := tx.Serialize(&buf); err != nil {
		return nil, fmt.Errorf("serialising the transaction: %w", err)
	}
	raw := buf.Bytes()

	// Re-read it from those bytes rather than keeping the in-memory value. Every
	// check below is then a check of what would actually go on the wire.
	published := &wire.MsgTx{}
	if err := published.Deserialize(bytes.NewReader(raw)); err != nil {
		return nil, fmt.Errorf("the transaction did not survive a serialisation "+
			"round trip: %w", err)
	}

	// I-3, last call. LND committed to this txid at psbt_verify and the peers are
	// about to store commitment signatures against its outpoints.
	if got, want := published.TxHash().String(), m.TxID; got != want {
		return nil, fmt.Errorf("%w: %s, not %s. Every returned packet agreed on "+
			"the unsigned transaction, so this is finalization itself having changed "+
			"it, which should not be possible — do not finalize this batch",
			ErrTXIDMoved, got, want)
	}

	if err := executeWitnesses(published, before); err != nil {
		return nil, err
	}

	fee, err := before.GetTxFee()
	if err != nil {
		return nil, fmt.Errorf("working out the fee: %w", err)
	}

	f := &Finalized{
		RawTx:   raw,
		TxID:    published.TxHash().String(),
		Vsize:   vsize(published),
		FeeSat:  int64(fee),
		Signers: append([]string(nil), m.Contributors...),
		Tx:      published,
		packet:  packet,
	}
	f.view, err = viewOf(published, before)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// Base64 renders the finalized PSBT, for an operator who wants to keep it.
//
// The transaction to publish is RawTx; this is for the record. A finalized PSBT
// is as broadcastable as the raw transaction, so it is no less sensitive.
func (f *Finalized) Base64() (string, error) { return f.packet.B64Encode() }

// View renders the extracted transaction the way a verifier wants it: the signed
// bytes, carrying each input's previous output and spend scripts, so a verifier
// can classify the inputs and measure the size exactly rather than estimating.
//
// It exists because a signed transaction has to be re-checked against the plan
// it was built for, and the check needs more than the raw bytes. Recheck below
// runs internal/plan's verifier over exactly this view. It had a second consumer
// once — the CPFP child, which is one-in one-out and cannot go through a
// plan.Plan, so it carried a verifier of its own — and that is gone.
//
// The alternative was for a caller to rebuild this view itself. Assembling
// it is fiddly in a way that matters: btcd's finalizer replaces each input with
// NewPsbtInput(nil, WitnessUtxo) plus the final witness, discarding the redeem
// script, the witness script and any non-witness UTXO, which are precisely the
// fields a verifier needs. A second copy of that reassembly would be a second
// thing to be wrong about the bytes n channels depend on, so the derivation
// stays here and only the accessor is new.
func (f *Finalized) View() ([]byte, error) {
	var buf bytes.Buffer
	if err := f.view.Serialize(&buf); err != nil {
		return nil, fmt.Errorf("serialising the transaction for the verifier: %w", err)
	}
	return buf.Bytes(), nil
}

// Recheck runs internal/plan's verifier over the extracted transaction.
//
// This is the last check before LND is handed the transaction, and it is a
// re-run rather than a new check on purpose: the plan verified the *unsigned*
// PSBT before the signers saw it, and this proves the thing that came back is
// still that transaction — every funding output present once at the exact
// amount, no output the plan does not name, the fee rate inside tolerance,
// change big enough for a CPFP child, every input a SegWit spend, nothing
// replaceable.
//
// It verifies the transaction rather than the packet. The packet that arrived
// and the bytes that will be broadcast are not the same artifact, and the second
// is the one n channels will depend on. The verifier's fee rate here is exact
// rather than an upper bound, because every witness is present.
func (f *Finalized) Recheck(p *plan.Plan) (*plan.Verification, error) {
	raw, err := f.View()
	if err != nil {
		return nil, err
	}
	v, err := p.Verify(raw)
	if err != nil {
		return nil, err
	}

	if !v.OK() {
		return v, fmt.Errorf("%w: %s", ErrPlanBroken, summarise(v.Problems))
	}

	// The verifier estimates size from the inputs' spend shapes. With every
	// witness present that estimate is not an estimate, so it has to equal the
	// real thing — and if it does not, one of the two is wrong about the
	// transaction and neither answer can be used. Checked after the findings, so
	// a real problem is never reported as an arithmetic disagreement.
	if v.Size.Vsize != f.Vsize {
		return v, fmt.Errorf("the verifier sizes this transaction at %d vB and the "+
			"transaction itself is %d vB. One of them is wrong about these bytes",
			v.Size.Vsize, f.Vsize)
	}
	return v, nil
}

// SigningWalletLabel is what a refusal calls the wallet at step 7.
//
// The labels exist so that a refusal can name which of m devices caused it. At
// step 7 there is one wallet and one file, so there is nothing to disambiguate —
// but DeviceError still renders a label, and an empty one would read as a packet
// from nowhere.
const SigningWalletLabel = "the signing wallet"

// Accept is step 7's answer, checked: the whole of Complete bar the merge.
//
// base is the unsigned PSBT the wallet built at step 4 — the one this app
// verified against the plan and handed to all n streams at step 5, so it is the
// only packet whose provenance is known and it is what everything below is
// checked against. signed is the same transaction with signatures on it, however
// the wallet chose to hand them back: complete witnesses, which is what Sparrow
// produces, or a complete set of partial signatures, which is what a wallet
// holding fewer than m keys produces. Either is finalized and executed here.
//
// It is not a weaker Complete. The base-packet guard, the txid pin, the UTXO
// check, the field-conflict rules, the witness execution and the plan re-check
// all run exactly as they do when several packets come back. What is absent is
// the only thing a single packet cannot need, which is a union.
func Accept(p *plan.Plan, base, signed []byte) (*Finalized, *plan.Verification, error) {
	merged, err := merge(base, []Part{{Label: SigningWalletLabel, PSBT: signed}},
		carryFinalized)
	if err != nil {
		return nil, nil, err
	}
	final, err := Finalize(merged)
	if err != nil {
		return nil, nil, err
	}
	v, err := final.Recheck(p)
	if err != nil {
		return nil, v, err
	}
	return final, v, nil
}

// Complete is the whole of half one: merge, finalize, and re-verify against the
// plan. Nothing else in this package needs to be called in order.
func Complete(p *plan.Plan, base []byte, parts []Part) (*Finalized, *plan.Verification, error) {
	merged, err := Merge(base, parts)
	if err != nil {
		return nil, nil, err
	}
	final, err := Finalize(merged)
	if err != nil {
		return nil, nil, err
	}
	v, err := final.Recheck(p)
	if err != nil {
		return nil, v, err
	}
	return final, v, nil
}

// checkSignatureCounts reports a multisig input that btcd's finalizer would
// refuse, in terms of the wallet rather than of btcd.
//
// It covers both shapes a multisig cold wallet takes: native P2WSH, and P2WSH
// wrapped in P2SH — sh(wsh(sortedmulti(…))), which is what an older descriptor
// produces. In the wrapped case the redeem script is the witness program and the
// witness script is still what the finalizer counts against.
func checkSignatureCounts(p *psbt.Packet, signers []string) error {
	for i := range p.Inputs {
		in := p.Inputs[i]
		if isFinal(in) {
			// Already a complete witness, which is what Accept's input carries.
			// Finalization discarded the partial signatures, so counting them here
			// would report a fully signed input as having none — and btcd's finalizer
			// will not touch it either. What checks this input is executeWitnesses,
			// which runs the witness rather than counting it.
			continue
		}
		if in.WitnessUtxo == nil {
			continue
		}
		script := in.WitnessUtxo.PkScript
		if txscript.IsPayToScriptHash(script) {
			script = in.RedeemScript
		}
		if !txscript.IsPayToWitnessScriptHash(script) {
			continue
		}
		need, ok := requiredSignatures(in.WitnessScript)
		if !ok {
			// Not a bare multisig. btcd has no finalizer for it, and saying so
			// now beats ErrNotFinalizable later.
			return fmt.Errorf("input %d is a P2WSH spend whose witness script is not "+
				"a bare m-of-n multisig (%s). btcd's finalizer only assembles multisig "+
				"witnesses, so this wallet needs a finalizer this build does not have",
				i, txscript.GetScriptClass(in.WitnessScript))
		}
		switch got := len(in.PartialSigs); {
		case got < need:
			return fmt.Errorf("%w: input %d has %d of the %d it needs, from %s",
				ErrNotEnoughSignatures, i, got, need, listOf(signers))
		case got > need:
			return fmt.Errorf("%w: input %d has %d signatures and the script takes "+
				"%d, from %s. Drop one device's partial and combine again — an extra "+
				"signature is not a spendable witness",
				ErrTooManySignatures, i, got, need, listOf(signers))
		}
	}
	return nil
}

// executeWitnesses runs each input's script with the witness that came out of
// the extractor.
//
// This is the strongest statement available without a node: not "the finalizer
// did not complain" but "this witness satisfies this script for this
// transaction". It is also what makes not needing Core's finalizepsbt a claim
// rather than a hope — a wrong witness assembled here would be caught here.
func executeWitnesses(tx *wire.MsgTx, before *psbt.Packet) error {
	prevOuts := make(map[wire.OutPoint]*wire.TxOut, len(tx.TxIn))
	for i, txIn := range tx.TxIn {
		out, err := prevOutOf(before, i)
		if err != nil {
			return err
		}
		prevOuts[txIn.PreviousOutPoint] = out
	}
	fetcher := txscript.NewMultiPrevOutFetcher(prevOuts)
	sigHashes := txscript.NewTxSigHashes(tx, fetcher)

	for i, txIn := range tx.TxIn {
		out := prevOuts[txIn.PreviousOutPoint]
		engine, err := txscript.NewEngine(out.PkScript, tx, i,
			txscript.StandardVerifyFlags, nil, sigHashes, out.Value, fetcher)
		if err != nil {
			return fmt.Errorf("%w: input %d (%s) could not be executed at all: %v",
				ErrWitnessInvalid, i, txIn.PreviousOutPoint, err)
		}
		if err := engine.Execute(); err != nil {
			return fmt.Errorf("%w: input %d (%s): %v", ErrWitnessInvalid, i,
				txIn.PreviousOutPoint, err)
		}
	}
	return nil
}

// prevOutOf reads what an input spends, from the metadata the packet carried
// before finalization stripped it.
func prevOutOf(p *psbt.Packet, i int) (*wire.TxOut, error) {
	in := p.Inputs[i]
	switch {
	case in.WitnessUtxo != nil:
		return in.WitnessUtxo, nil
	case in.NonWitnessUtxo != nil:
		idx := int(p.UnsignedTx.TxIn[i].PreviousOutPoint.Index)
		if h := in.NonWitnessUtxo.TxHash(); h != p.UnsignedTx.TxIn[i].PreviousOutPoint.Hash {
			return nil, fmt.Errorf("input %d: the attached previous transaction is %s, "+
				"but the input spends %s", i, h, p.UnsignedTx.TxIn[i].PreviousOutPoint.Hash)
		}
		if idx >= len(in.NonWitnessUtxo.TxOut) {
			return nil, fmt.Errorf("input %d: the attached previous transaction has no "+
				"output %d", i, idx)
		}
		return in.NonWitnessUtxo.TxOut[idx], nil
	default:
		return nil, fmt.Errorf("input %d says nothing about what it spends, so its "+
			"witness cannot be checked and its amount is unknown", i)
	}
}

// viewOf expresses the extracted transaction as a PSBT internal/plan can verify.
//
// The unsigned transaction is the extracted one with its witnesses stripped —
// which is the same bytes as the base's, and is built this way rather than
// reused so that the verifier is looking at the transaction that came out rather
// than the one that went in. Everything else is the metadata the packet carried
// before finalization discarded it, plus each input's final witness taken
// straight from the serialized transaction, which is what makes the verifier's
// size and fee rate exact.
func viewOf(tx *wire.MsgTx, before *psbt.Packet) (*psbt.Packet, error) {
	unsigned := tx.Copy()
	for _, in := range unsigned.TxIn {
		in.SignatureScript = nil
		in.Witness = nil
	}
	view, err := psbt.NewFromUnsignedTx(unsigned)
	if err != nil {
		return nil, fmt.Errorf("rebuilding the transaction for the verifier: %w", err)
	}

	for i := range view.Inputs {
		src := before.Inputs[i]
		dst := &view.Inputs[i]
		dst.WitnessUtxo = src.WitnessUtxo
		dst.NonWitnessUtxo = src.NonWitnessUtxo
		dst.RedeemScript = src.RedeemScript
		dst.WitnessScript = src.WitnessScript
		dst.SighashType = src.SighashType
		dst.Bip32Derivation = src.Bip32Derivation
		dst.TaprootBip32Derivation = src.TaprootBip32Derivation

		// The witness as the transaction carries it, not as the finalizer
		// recorded it. Partial signatures are deliberately not copied: they are
		// not part of the transaction being published.
		if len(tx.TxIn[i].SignatureScript) > 0 {
			dst.FinalScriptSig = tx.TxIn[i].SignatureScript
		}
		if len(tx.TxIn[i].Witness) > 0 {
			var wit bytes.Buffer
			if err := psbt.WriteTxWitness(&wit, tx.TxIn[i].Witness); err != nil {
				return nil, fmt.Errorf("input %d: re-serialising the witness: %w", i, err)
			}
			dst.FinalScriptWitness = wit.Bytes()
		}
	}
	for i := range view.Outputs {
		view.Outputs[i] = copyOutput(before.Outputs[i])
	}
	return view, nil
}

// vsize is BIP-141's virtual size of a serialized transaction.
func vsize(tx *wire.MsgTx) int64 {
	base := tx.SerializeSizeStripped()
	total := tx.SerializeSize()
	weight := base*3 + total
	return int64((weight + 3) / 4)
}

// unfinalized names the inputs that did not finalize.
func unfinalized(p *psbt.Packet) string {
	var idx []string
	for i := range p.Inputs {
		if !isFinal(p.Inputs[i]) {
			idx = append(idx, fmt.Sprintf("input %d", i))
		}
	}
	if len(idx) == 0 {
		return "every input is finalized"
	}
	return listOf(idx) + " still carry no witness"
}

// summarise renders the verifier's findings for an error string. The full
// Verification is returned alongside, for a screen that can show all of it.
func summarise(problems []plan.Problem) string {
	out := make([]string, 0, len(problems))
	for _, p := range problems {
		out = append(out, p.String())
	}
	return listOf(out)
}

// listOf renders a list the way a sentence wants it.
func listOf(items []string) string {
	switch len(items) {
	case 0:
		return "nothing"
	case 1:
		return items[0]
	default:
		return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
	}
}
