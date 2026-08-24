// Package combine merges the partial signatures the signers return into one
// transaction, finalizes it here, and checks what it extracted before anything
// else sees it.
//
// This is I-2's only piece of real code. Signers return *partial* signatures;
// combining and finalizing happen in-app, so no external party ever holds a
// transaction it could broadcast. Take that away and I-1's gate — publish only
// once every channel is already recoverable — can be defeated from outside, by
// a device or a wallet that simply publishes early. The gate is not a lock if
// somebody else has a key.
//
// btcd's psbt package has no Combine. It gives Packet.Serialize, B64Encode,
// IsComplete, SanityCheck and GetTxFee, and the package gives NewFromRawBytes,
// MaybeFinalizeAll and Extract. Merging n partially-signed packets into one is
// ours, and this one trusts neither side of the exchange:
//
//   - every returned packet must carry the same unsigned transaction as the
//     base (I-3), and a mismatch names the device that caused it;
//   - per-input partial signatures are unioned by public key, and two different
//     signatures for one key are refused rather than one of them chosen;
//   - witness scripts, redeem scripts, sighash types and key derivations are
//     carried forward, and a later packet may not overwrite an earlier one's —
//     a disagreement is reported, not dropped;
//   - the UTXO a signer attaches to an input must be the one the base already
//     had. LND's psbt.SumUtxoInputValues trusts that field, so a signer that
//     could rewrite it could rewrite the fee;
//   - a packet that arrives already finalized is refused. A finalized input is
//     a complete witness, which means that device held a broadcastable
//     transaction. That is the thing I-2 exists to prevent, and it is worth
//     failing loudly on rather than accepting quietly.
//
// # This answers docs/design.html's finalizepsbt question by not asking it
//
// The design left open whether Core's finalizepsbt handles every signer's
// output for the multisig descriptor in use, or whether a fallback finalizer is
// needed. Nothing in this package calls Core. Finalization is btcd's
// MaybeFinalizeAll over a packet assembled here, and the witness it produces is
// executed locally against each input's script before the transaction leaves
// this package — see Finalize. So Core's finalizer is not in the path at all,
// in either engine, and the question stops being load-bearing.
//
// What btcd's finalizer does require is narrower than Core's, and worth knowing
// before a batch is armed rather than after:
//
//   - a P2WSH input's witness script must be a *bare* m-of-n multisig.
//     finalizeWitnessInput calls getMultisigScriptWitness, which calls
//     checkIsMultiSigScript, whose first act is
//     txscript.GetScriptClass(script) != txscript.MultiSigTy. wsh(sortedmulti)
//     produces exactly that; wsh(multi) does too. A miniscript policy with a
//     timelock in it does not, and would need a finalizer this package does not
//     have.
//   - it wants exactly m signatures, not more. checkIsMultiSigScript requires
//     numSigs == len(pubKeys) == len(sigs), so a 2-of-3 with three partials
//     fails — with "Unsupported script type", which says nothing useful. This
//     package counts first and says what actually happened.
//
// Both are checked here, before MaybeFinalizeAll is called, so the operator gets
// a sentence about their wallet rather than one about btcd.
package combine

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

var (
	// ErrDifferentTransaction is I-3, caught at the merge. LND commits to the
	// funding outpoint at psbt_verify — "no inputs or outputs can change, only
	// signatures can be added" — so a returned packet carrying a different
	// unsigned transaction is a batch that cannot be saved, only rebuilt.
	ErrDifferentTransaction = errors.New("this device returned a different unsigned transaction")

	// ErrConflictingSignature means two packets carry different signatures for
	// the same public key on the same input.
	//
	// Picking one would work — either is valid on its own — and that is exactly
	// why it is refused. Two signatures from one key means either a device
	// signed twice with different nonces, which is harmless but unexplained, or
	// two devices are claiming the same key, which is not. Neither is a thing to
	// resolve silently while assembling a transaction that n channels depend on.
	ErrConflictingSignature = errors.New("two different signatures for the same public key")

	// ErrConflictingField means a later packet disagreed with an earlier one
	// about a witness script, a redeem script, a derivation or a UTXO.
	ErrConflictingField = errors.New("this device disagrees with an earlier packet")

	// ErrAlreadyFinalized means a device returned a finalized input.
	//
	// I-2: a finalized input carries a complete witness, so that device held a
	// transaction it could have broadcast. The app must be the only party ever
	// in that position.
	ErrAlreadyFinalized = errors.New("this device returned a finalized input, " +
		"which means it held a transaction it could have broadcast itself")

	// ErrNoSignatures means the merge added nothing: every device returned the
	// packet it was given.
	ErrNoSignatures = errors.New("no device added a signature")
)

// Part is one PSBT as a signer returned it.
type Part struct {
	// Label is what the operator calls the device. It is carried so a refusal
	// can name it: with m devices in a room, "a device returned a different
	// transaction" is a hunt and "cold2 did" is a fix.
	Label string

	// PSBT is the raw bytes. Base64 is what a browser upload carries, so use
	// ParseBase64 on the way in rather than teaching this package about
	// transports.
	PSBT []byte
}

// DeviceError names the device, and the place in the packet, a refusal is about.
type DeviceError struct {
	Label string
	Where string // "input 3", or "" for the packet as a whole
	Err   error
}

func (e *DeviceError) Error() string {
	switch {
	case e.Label == "" && e.Where == "":
		return e.Err.Error()
	case e.Where == "":
		return fmt.Sprintf("%s: %v", e.Label, e.Err)
	case e.Label == "":
		return fmt.Sprintf("%s: %v", e.Where, e.Err)
	default:
		return fmt.Sprintf("%s, %s: %v", e.Label, e.Where, e.Err)
	}
}

func (e *DeviceError) Unwrap() error { return e.Err }

func deviceErr(label, where string, err error) error {
	return &DeviceError{Label: label, Where: where, Err: err}
}

// Merged is what the merge produced, and who put what into it.
type Merged struct {
	// Packet is a fresh packet. Neither the base nor any part is mutated, so a
	// failed merge leaves every input exactly as it arrived.
	Packet *psbt.Packet

	// TxID is the unsigned transaction's txid: the value LND committed to at
	// psbt_verify and the one I-3 pins. Every part agreed on it or the merge
	// would have failed.
	TxID string

	// Added is how many partial signatures each label contributed. A device that
	// returned the packet unchanged appears with zero, because "the device said
	// yes" and "the device signed" are different facts and the second is the one
	// that matters.
	Added map[string]int

	// Contributors are the labels that added at least one signature, in the
	// order they were supplied.
	Contributors []string
}

// magic is BIP174's five bytes, and it is what settles binary-or-base64 without
// guessing. A base64 PSBT begins "cHNidP8", which is these bytes encoded, so the
// two forms cannot be confused by looking at the front of them.
var magic = []byte{0x70, 0x73, 0x62, 0x74, 0xff}

// Parse reads a PSBT that arrived as either base64 text or raw bytes.
//
// Three transports need this and they need the same answer. A wallet writes
// whichever form it writes — Sparrow writes binary .psbt files, Core writes
// base64 — and an operator moving a file by hand should not have to know which
// one this build wanted. internal/signers' file handshake needed it first, a
// browser upload needs it now, and it lives here because this is the package
// that owns what a PSBT is. Two sniffers in two packages could disagree about
// one file, which is the kind of disagreement that surfaces as a device being
// blamed for something a transport did.
func Parse(body []byte) ([]byte, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("it is empty")
	}
	if bytes.HasPrefix(trimmed, magic) {
		return trimmed, nil
	}
	raw, err := ParseBase64(string(trimmed))
	if err != nil {
		// Not base64 either. Say what was actually there rather than repeating
		// base64's complaint, which is about padding and tells nobody anything.
		if _, decErr := base64.StdEncoding.DecodeString(string(trimmed)); decErr != nil {
			return nil, fmt.Errorf("it is neither base64 nor a PSBT: it starts %q",
				firstBytes(trimmed))
		}
		return nil, err
	}
	return raw, nil
}

func firstBytes(b []byte) string {
	if len(b) > 24 {
		b = b[:24]
	}
	return string(b)
}

// ParseBase64 decodes a base64 PSBT, for the transport a browser upload uses.
func ParseBase64(s string) ([]byte, error) {
	packet, err := psbt.NewFromRawBytes(bytes.NewReader([]byte(s)), true)
	if err != nil {
		return nil, fmt.Errorf("that is not a base64 PSBT: %w", err)
	}
	var buf bytes.Buffer
	if err := packet.Serialize(&buf); err != nil {
		return nil, fmt.Errorf("re-serialising the PSBT: %w", err)
	}
	return buf.Bytes(), nil
}

// Merge combines the returned packets into one.
//
// base is the unsigned PSBT the app built and LND verified — the transaction the
// signers were asked about. It is the starting point rather than one of the
// parts because it is the only packet whose provenance is known: everything a
// device returns is checked against it, and a field only a device supplied has
// to survive the check to get in.
//
// It returns an error at the first refusal rather than collecting them. This is
// unlike internal/plan's verifier, deliberately: that one runs before the
// operator has committed to anything and reports everything wrong at once,
// while this one runs with n funding streams already open and the useful answer
// is which device to go back to.
func Merge(base []byte, parts []Part) (*Merged, error) {
	basePacket, err := psbt.NewFromRawBytes(bytes.NewReader(base), false)
	if err != nil {
		return nil, fmt.Errorf("the base PSBT does not parse: %w", err)
	}
	if err := basePacket.SanityCheck(); err != nil {
		return nil, fmt.Errorf("the base PSBT is malformed: %w", err)
	}
	if len(parts) == 0 {
		return nil, fmt.Errorf("no signed PSBTs to combine")
	}

	// The base must not already be finalized. If it is, either it is not the
	// packet that went to the signers or something in this process finalized it
	// early, and both are worth stopping for.
	for i := range basePacket.Inputs {
		if isFinal(basePacket.Inputs[i]) {
			return nil, fmt.Errorf("the base PSBT's input %d is already finalized, "+
				"so it is not the unsigned transaction the signers were shown", i)
		}
	}

	out := copyPacket(basePacket)
	txid := out.UnsignedTx.TxHash().String()

	m := &Merged{Packet: out, TxID: txid, Added: map[string]int{}}

	for _, part := range parts {
		label := part.Label
		if label == "" {
			return nil, fmt.Errorf("a returned PSBT arrived with no device label; " +
				"a refusal that cannot name the device is one the operator cannot act on")
		}
		if _, dup := m.Added[label]; dup {
			return nil, fmt.Errorf("two returned PSBTs are both labelled %q; the "+
				"labels are how a refusal names a device, so they have to be distinct",
				label)
		}
		m.Added[label] = 0

		packet, err := psbt.NewFromRawBytes(bytes.NewReader(part.PSBT), false)
		if err != nil {
			return nil, deviceErr(label, "", fmt.Errorf("its PSBT does not parse: %w", err))
		}
		if err := packet.SanityCheck(); err != nil {
			return nil, deviceErr(label, "", fmt.Errorf("its PSBT is malformed: %w", err))
		}

		// I-3, at the earliest moment it can be checked. A PSBT's unsigned
		// transaction carries no witnesses by construction — SanityCheck's
		// validateUnsignedTX refuses one that does — so equal txids here means
		// equal bytes: same version, same inputs in the same order with the same
		// sequences, same outputs, same locktime.
		if got := packet.UnsignedTx.TxHash().String(); got != txid {
			return nil, deviceErr(label, "", fmt.Errorf("%w: %s, not %s. "+
				"LND has already committed to %s at psbt_verify and will accept only "+
				"added signatures, so this batch cannot be salvaged by re-signing — it "+
				"has to be rebuilt. A signer that re-selects coins or re-derives change "+
				"does this",
				ErrDifferentTransaction, got, txid, txid))
		}
		if len(packet.Inputs) != len(out.Inputs) {
			return nil, deviceErr(label, "", fmt.Errorf(
				"it has %d input records for %d inputs", len(packet.Inputs), len(out.Inputs)))
		}
		if len(packet.Outputs) != len(out.Outputs) {
			return nil, deviceErr(label, "", fmt.Errorf(
				"it has %d output records for %d outputs", len(packet.Outputs), len(out.Outputs)))
		}

		for i := range packet.Inputs {
			where := fmt.Sprintf("input %d", i)
			added, err := mergeInput(&out.Inputs[i], packet.Inputs[i], label, where)
			if err != nil {
				return nil, err
			}
			m.Added[label] += added
		}
		for i := range packet.Outputs {
			where := fmt.Sprintf("output %d", i)
			if err := mergeOutput(&out.Outputs[i], packet.Outputs[i], label, where); err != nil {
				return nil, err
			}
		}
		if err := mergeUnknowns(&out.Unknowns, packet.Unknowns, label, ""); err != nil {
			return nil, err
		}

		if m.Added[label] > 0 {
			m.Contributors = append(m.Contributors, label)
		}
	}

	if err := out.SanityCheck(); err != nil {
		return nil, fmt.Errorf("the merged PSBT is malformed: %w", err)
	}
	if len(m.Contributors) == 0 {
		return nil, fmt.Errorf("%w — every device returned the packet it was given. "+
			"Nothing is lost: no transaction has been finalized and none has been "+
			"published, so this is one more signing round", ErrNoSignatures)
	}
	return m, nil
}

// mergeInput folds one returned input into the accumulator, and returns how many
// partial signatures it contributed.
func mergeInput(dst *psbt.PInput, src psbt.PInput, label, where string) (int, error) {
	// I-2. Checked before anything else about the input, because if this fires
	// the operator has a process problem rather than a data problem.
	if isFinal(src) {
		return 0, deviceErr(label, where, fmt.Errorf("%w. Only partial signatures "+
			"may leave a signer; combining and finalizing happen here, and that is "+
			"what stops anything outside this process from publishing before every "+
			"channel in the batch is recoverable", ErrAlreadyFinalized))
	}

	// The UTXO is the input's amount and its script. LND reads the attached
	// value with psbt.SumUtxoInputValues and does not check that it belongs to
	// the input, so a device that could rewrite this could rewrite the fee.
	if src.WitnessUtxo != nil {
		switch {
		case dst.WitnessUtxo == nil:
			dst.WitnessUtxo = &wire.TxOut{
				Value:    src.WitnessUtxo.Value,
				PkScript: cloneBytes(src.WitnessUtxo.PkScript),
			}
		case !psbt.TxOutsEqual(dst.WitnessUtxo, src.WitnessUtxo):
			return 0, deviceErr(label, where, fmt.Errorf(
				"%w about what this input spends: it says %d sat to %x, the base says "+
					"%d sat to %x", ErrConflictingField,
				src.WitnessUtxo.Value, src.WitnessUtxo.PkScript,
				dst.WitnessUtxo.Value, dst.WitnessUtxo.PkScript))
		}
	}
	if src.NonWitnessUtxo != nil {
		if dst.NonWitnessUtxo == nil {
			dst.NonWitnessUtxo = src.NonWitnessUtxo.Copy()
		} else if dst.NonWitnessUtxo.TxHash() != src.NonWitnessUtxo.TxHash() {
			return 0, deviceErr(label, where, fmt.Errorf(
				"%w about the previous transaction this input spends: %s, not %s",
				ErrConflictingField, src.NonWitnessUtxo.TxHash(), dst.NonWitnessUtxo.TxHash()))
		}
	}

	if err := mergeScript(&dst.RedeemScript, src.RedeemScript, "redeem script", label, where); err != nil {
		return 0, err
	}
	if err := mergeScript(&dst.WitnessScript, src.WitnessScript, "witness script", label, where); err != nil {
		return 0, err
	}
	if err := mergeScript(&dst.TaprootInternalKey, src.TaprootInternalKey,
		"taproot internal key", label, where); err != nil {
		return 0, err
	}
	if err := mergeScript(&dst.TaprootMerkleRoot, src.TaprootMerkleRoot,
		"taproot merkle root", label, where); err != nil {
		return 0, err
	}
	if err := mergeScript(&dst.TaprootKeySpendSig, src.TaprootKeySpendSig,
		"taproot key spend signature", label, where); err != nil {
		return 0, err
	}

	// Zero means the field is absent, and an absent sighash type is read as
	// SIGHASH_ALL — psbt's own checkSigHashFlags does exactly that.
	if src.SighashType != 0 {
		switch {
		case dst.SighashType == 0:
			dst.SighashType = src.SighashType
		case dst.SighashType != src.SighashType:
			return 0, deviceErr(label, where, fmt.Errorf(
				"%w about the sighash type: %#x, not %#x", ErrConflictingField,
				uint32(src.SighashType), uint32(dst.SighashType)))
		}
	}

	added, err := mergeSigs(&dst.PartialSigs, src.PartialSigs, label, where)
	if err != nil {
		return 0, err
	}
	if err := mergeDerivations(&dst.Bip32Derivation, src.Bip32Derivation, label, where); err != nil {
		return 0, err
	}
	if err := mergeTaprootDerivations(&dst.TaprootBip32Derivation,
		src.TaprootBip32Derivation, label, where); err != nil {
		return 0, err
	}
	if err := mergeTaprootSigs(&dst.TaprootScriptSpendSig,
		src.TaprootScriptSpendSig, label, where); err != nil {
		return 0, err
	}
	if err := mergeLeafScripts(&dst.TaprootLeafScript, src.TaprootLeafScript, label, where); err != nil {
		return 0, err
	}
	if err := mergeUnknowns(&dst.Unknowns, src.Unknowns, label, where); err != nil {
		return 0, err
	}

	// A taproot signature is a signature too, so it counts as a contribution.
	if len(src.TaprootKeySpendSig) > 0 {
		added++
	}
	added += len(src.TaprootScriptSpendSig)
	return added, nil
}

// mergeOutput folds one returned output's metadata in.
//
// A device has no business changing an output — the amounts and scripts live in
// the unsigned transaction, which is already pinned — but it does legitimately
// add key-origin information, which is the evidence assisted mode's change
// recognition rests on.
func mergeOutput(dst *psbt.POutput, src psbt.POutput, label, where string) error {
	if err := mergeScript(&dst.RedeemScript, src.RedeemScript, "redeem script", label, where); err != nil {
		return err
	}
	if err := mergeScript(&dst.WitnessScript, src.WitnessScript, "witness script", label, where); err != nil {
		return err
	}
	if err := mergeScript(&dst.TaprootInternalKey, src.TaprootInternalKey,
		"taproot internal key", label, where); err != nil {
		return err
	}
	if err := mergeScript(&dst.TaprootTapTree, src.TaprootTapTree,
		"taproot tap tree", label, where); err != nil {
		return err
	}
	if err := mergeDerivations(&dst.Bip32Derivation, src.Bip32Derivation, label, where); err != nil {
		return err
	}
	if err := mergeTaprootDerivations(&dst.TaprootBip32Derivation,
		src.TaprootBip32Derivation, label, where); err != nil {
		return err
	}
	return mergeUnknowns(&dst.Unknowns, src.Unknowns, label, where)
}

// mergeScript carries a byte field forward. First value wins; an identical
// repeat is accepted; a different value is refused.
//
// "Without letting a later packet overwrite an earlier one's" is only a real
// rule if the disagreement is reported. Silently keeping the first would hide a
// device that thinks it is signing a different script from the one it is signing.
func mergeScript(dst *[]byte, src []byte, what, label, where string) error {
	switch {
	case len(src) == 0:
		return nil
	case len(*dst) == 0:
		*dst = cloneBytes(src)
		return nil
	case bytes.Equal(*dst, src):
		return nil
	default:
		return deviceErr(label, where, fmt.Errorf("%w about the %s: %x, not %x",
			ErrConflictingField, what, src, *dst))
	}
}

// mergeSigs unions the partial signatures by public key.
func mergeSigs(dst *[]*psbt.PartialSig, src []*psbt.PartialSig, label, where string) (int, error) {
	have := make(map[string]*psbt.PartialSig, len(*dst))
	for _, ps := range *dst {
		have[string(ps.PubKey)] = ps
	}

	added := 0
	for _, ps := range src {
		// Redundant against anything that came through NewFromRawBytes, and kept
		// anyway. btcd's deserializer runs PartialSig.checkValid on every partial
		// signature it reads — ParsePubKey and ParseDERSignature — so a malformed
		// one never reaches here from a parsed packet, and it also refuses two
		// records with the same pubkey inside one input. But btcd's finalizer then
		// reads sig[len(sig)-1] with no length check of its own, and the cost of
		// being wrong about which of those two facts holds is a panic in the
		// middle of the armed window.
		switch {
		case len(ps.PubKey) != 33 && len(ps.PubKey) != 65:
			return 0, deviceErr(label, where, fmt.Errorf(
				"a partial signature carries a %d-byte public key, which is neither "+
					"compressed nor uncompressed", len(ps.PubKey)))
		case len(ps.Signature) == 0:
			return 0, deviceErr(label, where, fmt.Errorf(
				"a partial signature for %x is empty", ps.PubKey))
		}

		if existing, ok := have[string(ps.PubKey)]; ok {
			if !bytes.Equal(existing.Signature, ps.Signature) {
				return 0, deviceErr(label, where, fmt.Errorf(
					"%w (%x). Either one device signed twice, or two devices are "+
						"claiming the same key. Both signatures are individually valid, "+
						"which is why choosing one is not an option here: rebuild the "+
						"signing round rather than guessing which device was right",
					ErrConflictingSignature, ps.PubKey))
			}
			continue
		}

		copied := &psbt.PartialSig{
			PubKey:    cloneBytes(ps.PubKey),
			Signature: cloneBytes(ps.Signature),
		}
		*dst = append(*dst, copied)
		have[string(copied.PubKey)] = copied
		added++
	}

	// Sorted by public key so the merged packet's bytes do not depend on the
	// order the devices came back in. The final witness order is taken from the
	// script rather than from this list, so this is about reproducibility, not
	// correctness — but a journal that records different bytes for the same set
	// of signatures is a journal that is harder to trust.
	sort.Sort(psbt.PartialSigSorter(*dst))
	return added, nil
}

// mergeDerivations unions BIP-32 key origins by public key.
func mergeDerivations(dst *[]*psbt.Bip32Derivation, src []*psbt.Bip32Derivation,
	label, where string) error {

	have := make(map[string]*psbt.Bip32Derivation, len(*dst))
	for _, d := range *dst {
		have[string(d.PubKey)] = d
	}
	for _, d := range src {
		existing, ok := have[string(d.PubKey)]
		if !ok {
			copied := &psbt.Bip32Derivation{
				PubKey:               cloneBytes(d.PubKey),
				MasterKeyFingerprint: d.MasterKeyFingerprint,
				Bip32Path:            clonePath(d.Bip32Path),
			}
			*dst = append(*dst, copied)
			have[string(copied.PubKey)] = copied
			continue
		}
		if existing.MasterKeyFingerprint != d.MasterKeyFingerprint ||
			!samePath(existing.Bip32Path, d.Bip32Path) {

			return deviceErr(label, where, fmt.Errorf(
				"%w about where %x comes from: %s, not %s", ErrConflictingField,
				d.PubKey, origin(d.MasterKeyFingerprint, d.Bip32Path),
				origin(existing.MasterKeyFingerprint, existing.Bip32Path)))
		}
	}
	return nil
}

// mergeTaprootDerivations is mergeDerivations for x-only keys.
func mergeTaprootDerivations(dst *[]*psbt.TaprootBip32Derivation,
	src []*psbt.TaprootBip32Derivation, label, where string) error {

	have := make(map[string]*psbt.TaprootBip32Derivation, len(*dst))
	for _, d := range *dst {
		have[string(d.XOnlyPubKey)] = d
	}
	for _, d := range src {
		existing, ok := have[string(d.XOnlyPubKey)]
		if !ok {
			*dst = append(*dst, d)
			have[string(d.XOnlyPubKey)] = d
			continue
		}
		if existing.MasterKeyFingerprint != d.MasterKeyFingerprint ||
			!samePath(existing.Bip32Path, d.Bip32Path) {

			return deviceErr(label, where, fmt.Errorf(
				"%w about where %x comes from: %s, not %s", ErrConflictingField,
				d.XOnlyPubKey, origin(d.MasterKeyFingerprint, d.Bip32Path),
				origin(existing.MasterKeyFingerprint, existing.Bip32Path)))
		}
	}
	return nil
}

// mergeTaprootSigs unions taproot script-path signatures by (key, leaf).
func mergeTaprootSigs(dst *[]*psbt.TaprootScriptSpendSig,
	src []*psbt.TaprootScriptSpendSig, label, where string) error {

	key := func(s *psbt.TaprootScriptSpendSig) string {
		return string(s.XOnlyPubKey) + "|" + string(s.LeafHash)
	}
	have := make(map[string]*psbt.TaprootScriptSpendSig, len(*dst))
	for _, s := range *dst {
		have[key(s)] = s
	}
	for _, s := range src {
		existing, ok := have[key(s)]
		if !ok {
			*dst = append(*dst, s)
			have[key(s)] = s
			continue
		}
		if !bytes.Equal(existing.Signature, s.Signature) {
			return deviceErr(label, where, fmt.Errorf("%w (%x, leaf %x)",
				ErrConflictingSignature, s.XOnlyPubKey, s.LeafHash))
		}
	}
	return nil
}

// mergeLeafScripts unions tap leaf scripts by control block and script.
func mergeLeafScripts(dst *[]*psbt.TaprootTapLeafScript,
	src []*psbt.TaprootTapLeafScript, label, where string) error {

	key := func(s *psbt.TaprootTapLeafScript) string {
		return string(s.ControlBlock) + "|" + string(s.Script)
	}
	have := make(map[string]bool, len(*dst))
	for _, s := range *dst {
		have[key(s)] = true
	}
	for _, s := range src {
		if have[key(s)] {
			continue
		}
		*dst = append(*dst, s)
		have[key(s)] = true
	}
	return nil
}

// mergeUnknowns carries proprietary fields forward, keyed on the key itself.
//
// Unknown to us is not unknown to the device that wrote it, so dropping one
// would quietly discard something a signer may need on a second pass. A
// conflicting value is refused for the same reason a conflicting witness script
// is.
func mergeUnknowns(dst *[]*psbt.Unknown, src []*psbt.Unknown, label, where string) error {
	have := make(map[string]*psbt.Unknown, len(*dst))
	for _, u := range *dst {
		have[string(u.Key)] = u
	}
	for _, u := range src {
		existing, ok := have[string(u.Key)]
		if !ok {
			copied := &psbt.Unknown{Key: cloneBytes(u.Key), Value: cloneBytes(u.Value)}
			*dst = append(*dst, copied)
			have[string(copied.Key)] = copied
			continue
		}
		if !bytes.Equal(existing.Value, u.Value) {
			return deviceErr(label, where, fmt.Errorf(
				"%w about the proprietary field %x: %x, not %x", ErrConflictingField,
				u.Key, u.Value, existing.Value))
		}
	}
	return nil
}

// isFinal reports whether an input already carries a complete witness.
func isFinal(in psbt.PInput) bool {
	return in.FinalScriptSig != nil || in.FinalScriptWitness != nil
}

// copyPacket deep-copies a packet, so a failed merge leaves the base untouched
// and MaybeFinalizeAll — which rewrites inputs in place — cannot reach it.
func copyPacket(p *psbt.Packet) *psbt.Packet {
	out := &psbt.Packet{
		UnsignedTx: p.UnsignedTx.Copy(),
		Inputs:     make([]psbt.PInput, len(p.Inputs)),
		Outputs:    make([]psbt.POutput, len(p.Outputs)),
	}
	for i, in := range p.Inputs {
		out.Inputs[i] = copyInput(in)
	}
	for i, o := range p.Outputs {
		out.Outputs[i] = copyOutput(o)
	}
	out.Unknowns = copyUnknowns(p.Unknowns)
	return out
}

func copyInput(in psbt.PInput) psbt.PInput {
	c := psbt.PInput{
		SighashType:        in.SighashType,
		RedeemScript:       cloneBytes(in.RedeemScript),
		WitnessScript:      cloneBytes(in.WitnessScript),
		FinalScriptSig:     cloneBytes(in.FinalScriptSig),
		FinalScriptWitness: cloneBytes(in.FinalScriptWitness),
		TaprootKeySpendSig: cloneBytes(in.TaprootKeySpendSig),
		TaprootInternalKey: cloneBytes(in.TaprootInternalKey),
		TaprootMerkleRoot:  cloneBytes(in.TaprootMerkleRoot),
		Unknowns:           copyUnknowns(in.Unknowns),
	}
	if in.WitnessUtxo != nil {
		c.WitnessUtxo = &wire.TxOut{
			Value:    in.WitnessUtxo.Value,
			PkScript: cloneBytes(in.WitnessUtxo.PkScript),
		}
	}
	if in.NonWitnessUtxo != nil {
		c.NonWitnessUtxo = in.NonWitnessUtxo.Copy()
	}
	for _, ps := range in.PartialSigs {
		c.PartialSigs = append(c.PartialSigs, &psbt.PartialSig{
			PubKey: cloneBytes(ps.PubKey), Signature: cloneBytes(ps.Signature),
		})
	}
	for _, d := range in.Bip32Derivation {
		c.Bip32Derivation = append(c.Bip32Derivation, &psbt.Bip32Derivation{
			PubKey:               cloneBytes(d.PubKey),
			MasterKeyFingerprint: d.MasterKeyFingerprint,
			Bip32Path:            clonePath(d.Bip32Path),
		})
	}
	c.TaprootScriptSpendSig = append(c.TaprootScriptSpendSig, in.TaprootScriptSpendSig...)
	c.TaprootLeafScript = append(c.TaprootLeafScript, in.TaprootLeafScript...)
	c.TaprootBip32Derivation = append(c.TaprootBip32Derivation, in.TaprootBip32Derivation...)
	return c
}

func copyOutput(o psbt.POutput) psbt.POutput {
	c := psbt.POutput{
		RedeemScript:       cloneBytes(o.RedeemScript),
		WitnessScript:      cloneBytes(o.WitnessScript),
		TaprootInternalKey: cloneBytes(o.TaprootInternalKey),
		TaprootTapTree:     cloneBytes(o.TaprootTapTree),
		Unknowns:           copyUnknowns(o.Unknowns),
	}
	for _, d := range o.Bip32Derivation {
		c.Bip32Derivation = append(c.Bip32Derivation, &psbt.Bip32Derivation{
			PubKey:               cloneBytes(d.PubKey),
			MasterKeyFingerprint: d.MasterKeyFingerprint,
			Bip32Path:            clonePath(d.Bip32Path),
		})
	}
	c.TaprootBip32Derivation = append(c.TaprootBip32Derivation, o.TaprootBip32Derivation...)
	return c
}

func copyUnknowns(us []*psbt.Unknown) []*psbt.Unknown {
	if len(us) == 0 {
		return nil
	}
	out := make([]*psbt.Unknown, 0, len(us))
	for _, u := range us {
		out = append(out, &psbt.Unknown{Key: cloneBytes(u.Key), Value: cloneBytes(u.Value)})
	}
	return out
}

func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

func clonePath(p []uint32) []uint32 {
	if p == nil {
		return nil
	}
	out := make([]uint32, len(p))
	copy(out, p)
	return out
}

func samePath(a, b []uint32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// origin renders a key origin the way a descriptor does, for a refusal an
// operator can compare against their own wallet.
func origin(fingerprint uint32, path []uint32) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "[%08x", fingerprint)
	for _, step := range path {
		if step >= hardened {
			fmt.Fprintf(&b, "/%dh", step-hardened)
			continue
		}
		fmt.Fprintf(&b, "/%d", step)
	}
	b.WriteByte(']')
	return b.String()
}

const hardened uint32 = 0x80000000

// requiredSignatures reads m out of an m-of-n bare multisig witness script.
//
// The same reading internal/plan/size.go does for sizing, for a different
// purpose: there it is how big the witness will be, here it is how many
// signatures btcd's finalizer will accept. Both go through
// txscript.CalcMultiSigStats, which is what makes them agree.
func requiredSignatures(witnessScript []byte) (int, bool) {
	if txscript.GetScriptClass(witnessScript) != txscript.MultiSigTy {
		return 0, false
	}
	_, sigs, err := txscript.CalcMultiSigStats(witnessScript)
	if err != nil || sigs <= 0 {
		return 0, false
	}
	return sigs, true
}
