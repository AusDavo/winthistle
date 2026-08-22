package plan

import (
	"fmt"
	"math"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// Sizing constants. Every one of them is an upper bound, deliberately, so the
// estimated vsize is an upper bound and the fee rate computed from it is a lower
// bound. The direction matters: I-4 forbids RBF, so a batch that turns out to be
// paying less than it looks like can only be rescued by CPFP, while one paying
// more has merely overpaid.
const (
	// maxDERSignature is a DER-encoded ECDSA signature plus its sighash byte, at
	// its largest. btcwallet's txsizes uses the same figure.
	maxDERSignature = 73

	// maxSchnorrSignature is a BIP-340 signature plus an explicit sighash byte.
	// A SIGHASH_DEFAULT spend is 64 and this is 65, so this is the bound.
	maxSchnorrSignature = 65

	// compressedPubKey is 33 bytes.
	compressedPubKey = 33

	// witnessScaleFactor and the marker/flag pair, from BIP-141.
	witnessScaleFactor = 4
	segwitMarkerFlag   = 2

	// DustSat is the floor below which an output is not relayed. Core's
	// dustRelayFee of 3000 sat/kvB puts a P2WSH or P2TR output at 330 sat and a
	// P2WPKH one at 294; 330 is used throughout because it is the larger, and
	// the change output on this path is a cold-wallet script.
	DustSat = 330
)

// varIntSize is the serialized size of a CompactSize integer.
func varIntSize(n uint64) int {
	switch {
	case n < 0xfd:
		return 1
	case n <= math.MaxUint16:
		return 3
	case n <= math.MaxUint32:
		return 5
	default:
		return 9
	}
}

// pushSize is the serialized size of a length-prefixed byte string: a witness
// stack item, or a scriptSig inside a transaction input.
func pushSize(n int) int { return varIntSize(uint64(n)) + n }

// spendSize is what spending one input costs, split the way BIP-141 weighs it.
type spendSize struct {
	ScriptSig int // bytes, counted at four weight units each
	Witness   int // bytes, counted at one
}

// estimateSpend sizes one input.
//
// It recognises exactly the spend types this tool supports, and returns an error
// for anything else rather than a plausible number. An unrecognised input in a
// cold-wallet batch is a finding in itself: I-3 already refuses anything that is
// not a SegWit spend, and a SegWit spend whose shape we cannot read is one whose
// fee we cannot check either.
func estimateSpend(in psbt.PInput, prevScript []byte) (spendSize, error) {
	// An input that is already signed needs no estimate at all.
	if len(in.FinalScriptWitness) > 0 || len(in.FinalScriptSig) > 0 {
		return spendSize{
			ScriptSig: len(in.FinalScriptSig),
			Witness:   len(in.FinalScriptWitness),
		}, nil
	}

	script := prevScript
	scriptSig := 0

	// A P2SH output spends as SegWit only by pushing a witness program as its
	// redeem script; the push is the whole scriptSig.
	if txscript.IsPayToScriptHash(script) {
		if len(in.RedeemScript) == 0 {
			return spendSize{}, fmt.Errorf("P2SH input with no redeem script")
		}
		if !txscript.IsWitnessProgram(in.RedeemScript) {
			return spendSize{}, fmt.Errorf("P2SH input whose redeem script is not a " +
				"witness program, so it is a legacy spend")
		}
		scriptSig = pushSize(len(in.RedeemScript))
		script = in.RedeemScript
	}

	switch {
	case txscript.IsPayToWitnessPubKeyHash(script):
		// stack: <sig> <pubkey>
		return spendSize{
			ScriptSig: scriptSig,
			Witness: varIntSize(2) + pushSize(maxDERSignature) +
				pushSize(compressedPubKey),
		}, nil

	case txscript.IsPayToWitnessScriptHash(script):
		if len(in.WitnessScript) == 0 {
			return spendSize{}, fmt.Errorf("P2WSH input with no witness script")
		}
		sigs, err := requiredSignatures(in.WitnessScript)
		if err != nil {
			return spendSize{}, err
		}
		// stack: <empty> <sig>*m <witnessScript>. The leading empty item is
		// OP_CHECKMULTISIG's off-by-one.
		items := 1 + sigs + 1
		size := varIntSize(uint64(items)) + pushSize(0) +
			sigs*pushSize(maxDERSignature) + pushSize(len(in.WitnessScript))
		return spendSize{ScriptSig: scriptSig, Witness: size}, nil

	case txscript.IsPayToTaproot(script):
		if len(in.TaprootLeafScript) > 0 {
			return spendSize{}, fmt.Errorf("taproot script-path input: the leaf and " +
				"control block make the witness size unknowable in advance")
		}
		// stack: <schnorr sig>
		return spendSize{
			ScriptSig: scriptSig,
			Witness:   varIntSize(1) + pushSize(maxSchnorrSignature),
		}, nil

	default:
		return spendSize{}, fmt.Errorf("%s input, which is not a SegWit spend",
			txscript.GetScriptClass(script))
	}
}

// requiredSignatures reads m out of an m-of-n bare multisig witness script.
func requiredSignatures(witnessScript []byte) (int, error) {
	_, sigs, err := txscript.CalcMultiSigStats(witnessScript)
	if err != nil {
		return 0, fmt.Errorf("witness script is not a bare multisig, so the number "+
			"of signatures it needs cannot be read: %w", err)
	}
	if sigs <= 0 {
		return 0, fmt.Errorf("witness script requires %d signatures", sigs)
	}
	return sigs, nil
}

// txBaseSize is the serialized size of a transaction with no witness data,
// given each input's scriptSig length.
func txBaseSize(tx *wire.MsgTx, scriptSigs []int) int {
	size := 4 + 4 // version, locktime
	size += varIntSize(uint64(len(tx.TxIn)))
	for i := range tx.TxIn {
		// outpoint(36) + varint(scriptSig) + scriptSig + sequence(4)
		size += 36 + 4 + pushSize(scriptSigs[i])
	}
	size += varIntSize(uint64(len(tx.TxOut)))
	for _, out := range tx.TxOut {
		size += 8 + varIntSize(uint64(len(out.PkScript))) + len(out.PkScript)
	}
	return size
}

// Size is the estimated size of a transaction, and how it was arrived at.
type Size struct {
	BaseBytes    int
	WitnessBytes int
	Weight       int
	Vsize        int64

	// Estimated is false when every input was already signed, in which case the
	// figures above are exact rather than upper bounds.
	Estimated bool
}

// estimateSize sizes a whole packet.
func estimateSize(packet *psbt.Packet, prevScripts [][]byte) (Size, error) {
	tx := packet.UnsignedTx
	scriptSigs := make([]int, len(tx.TxIn))
	witness, estimated := 0, false

	for i := range packet.Inputs {
		s, err := estimateSpend(packet.Inputs[i], prevScripts[i])
		if err != nil {
			return Size{}, fmt.Errorf("input %d: %w", i, err)
		}
		scriptSigs[i] = s.ScriptSig
		witness += s.Witness
		if len(packet.Inputs[i].FinalScriptWitness) == 0 {
			estimated = true
		}
	}

	base := txBaseSize(tx, scriptSigs)
	weight := base * witnessScaleFactor
	if witness > 0 {
		weight += segwitMarkerFlag + witness
	}
	return Size{
		BaseBytes:    base,
		WitnessBytes: witness,
		Weight:       weight,
		Vsize:        int64((weight + witnessScaleFactor - 1) / witnessScaleFactor),
		Estimated:    estimated,
	}, nil
}

// childVsize is the size of the CPFP child the change output has to be able to
// fund: one input spending that script, one output paying it back to the same
// kind of script.
//
// A guess with a reason, rather than a guess. The child's own destination is not
// known when the parent is planned, and paying back to the same script type is
// both the likely choice and the conservative one for a multisig cold wallet,
// whose scripts are the largest of the segwit family.
func childVsize(changeScript []byte, witnessScript []byte) (int64, error) {
	in := psbt.PInput{WitnessScript: witnessScript}
	s, err := estimateSpend(in, changeScript)
	if err != nil {
		return 0, err
	}
	base := 4 + 4 + varIntSize(1) + 36 + 4 + pushSize(s.ScriptSig) +
		varIntSize(1) + 8 + varIntSize(uint64(len(changeScript))) + len(changeScript)
	weight := base*witnessScaleFactor + segwitMarkerFlag + s.Witness
	return int64((weight + witnessScaleFactor - 1) / witnessScaleFactor), nil
}

// ChangeFloor is the smallest change amount that leaves a viable CPFP child.
//
// I-4 says the batch can never be replaced, so the change output is the only
// lever left if the fee turns out to be too low. To lift the parent and child
// together to bumpTo sat/vB the child has to pay
//
//	(parentVsize + childVsize) * bumpTo - parentFee
//
// and still leave an output above the dust limit. A change output smaller than
// that is a change output that cannot rescue the batch, which — given no RBF —
// means a batch with nothing to rescue it.
func ChangeFloor(parentVsize, parentFeeSat int64, bumpTo float64, childVsize int64) int64 {
	packageFee := int64(math.Ceil(bumpTo * float64(parentVsize+childVsize)))
	need := packageFee - parentFeeSat
	if need < 0 {
		need = 0
	}
	return need + DustSat
}
