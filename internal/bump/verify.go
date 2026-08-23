package bump

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"sort"

	"github.com/AusDavo/winthistle/internal/plan"
	"github.com/AusDavo/winthistle/internal/prose"
	"github.com/AusDavo/winthistle/internal/settle"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
)

// Code identifies a finding, so a UI can react to one without matching prose.
type Code int

const (
	Unparseable Code = iota
	WrongInputCount
	WrongInput
	NoUTXOInfo
	MismatchedUTXO
	WrongInputValue
	LegacyInput
	Replaceable
	WrongOutputCount
	WrongOutput
	DustOutput
	WrongFee
	Unsizable
	SizeGrew
	NotBumped
	AboveRelayCeiling
)

func (c Code) String() string {
	switch c {
	case Unparseable:
		return "unparseable"
	case WrongInputCount:
		return "wrong number of inputs"
	case WrongInput:
		return "spends something other than the parent's change"
	case NoUTXOInfo:
		return "input carries no utxo information"
	case MismatchedUTXO:
		return "input's utxo does not belong to it"
	case WrongInputValue:
		return "input is not worth what the parent's change is worth"
	case LegacyInput:
		return "input is not a segwit spend"
	case Replaceable:
		return "replaceable"
	case WrongOutputCount:
		return "wrong number of outputs"
	case WrongOutput:
		return "pays a script that was not asked for"
	case DustOutput:
		return "output below the dust floor"
	case WrongFee:
		return "fee is not the one that was worked out"
	case Unsizable:
		return "size cannot be estimated"
	case SizeGrew:
		return "the signed transaction is larger than the estimate"
	case NotBumped:
		return "does not lift the parent to the target"
	case AboveRelayCeiling:
		return "fee rate above what the node will broadcast"
	}
	return "unknown"
}

// Problem is one reason to refuse the child.
//
// The same four fields internal/plan's Problem carries, so a caller rendering
// findings does not have to care which verifier produced them — and a separate
// enum, because the two transactions are refused for different reasons. There
// are sixteen codes here against internal/plan's nineteen and only six overlap.
type Problem struct {
	Code     Code
	Where    string // "input 0", "output 0", ""
	Headline string
	Detail   string
}

func (p Problem) String() string {
	if p.Where == "" {
		return p.Headline
	}
	return p.Where + ": " + p.Headline
}

// Expectation is what the child is supposed to be.
//
// Every field is something that was read rather than assumed. The parent's size
// and fee come out of Core's mempool entry, the change outpoint and its value out
// of the cold wallet's listunspent, the address out of getrawchangeaddress, and
// the fee out of what Core charged when it built the transaction. Verifying is
// then a comparison rather than a second derivation, which is the only kind of
// check worth making before a PSBT goes to a cold-storage device.
type Expectation struct {
	Chain string

	// Parent is the batch being accelerated, with the figures the arithmetic used.
	Parent settle.Parent

	// TargetSatPerVB is the rate the pair has to reach together.
	TargetSatPerVB float64

	// PaysTo is the cold-wallet address the remainder goes back to, named
	// exactly. Naming the script is the strongest form of this check — stronger
	// than internal/plan's assisted-mode change recognition, which has to settle
	// for key-origin evidence because Sparrow picks its own address. Here the app
	// asked Core for the address, so there is nothing to recognise.
	PaysTo string

	// FeeSat is the fee this child is expected to pay: what Core charged at build
	// time, which BuildChild already checked against plan.ChildFeeSat.
	FeeSat int64
}

// Verification is everything the verifier learned about one child.
type Verification struct {
	TxID string

	// Signed is whether every input carried a witness, which makes Vsize exact
	// rather than an upper bound.
	Signed bool

	InputSat  int64
	OutputSat int64
	FeeSat    int64

	VsizeVB     int64
	ChildRate   float64
	PackageVB   int64
	PackageFee  int64
	PackageRate float64

	Problems []Problem
}

// OK reports whether the child may go to the signers, or to the network.
func (v *Verification) OK() bool { return len(v.Problems) == 0 }

// Verify checks a CPFP child against the parent it claims to accelerate.
//
// # Why this is not internal/plan
//
// A one-in one-out child cannot go through plan.Plan at all: Outputs refuses a
// plan with no channels in it, and that refusal is load-bearing rather than
// incidental. Relaxing it would be the wrong repair even if it were free,
// because four of the batch verifier's checks mean something different here or
// nothing at all — the fee tolerance is about the parent's own rate where a
// child's is about the package's, ChangeFloor has no meaning for a transaction
// that *is* the change being spent, the assisted-mode change recognition does
// not apply because the app named the address, and the exact-amount rule inverts.
// A verifier with four switches in it is worse than two verifiers.
//
// What is shared is the floor, and it is shared as code rather than as a
// convention: plan.IsSegwitSpend reads the prevout script rather than trusting a
// field, plan.MaxNonReplaceableSequence is the same constant, plan.SizeOf uses
// the same upper bounds, and plan.DustSat is the same floor. So the two
// verifiers cannot drift on the facts they both depend on.
//
// # Why it runs twice
//
// Once on what goes out and once on what comes back, which is the same
// discipline internal/combine applies to the batch. The first pass is what stops
// an unverified PSBT reaching a cold-storage device. The second is the one that
// matters more: the packet that went out and the bytes that will be broadcast are
// not the same artifact, and only the second can be checked with every witness
// present — which turns the size from an upper bound into a measurement, and the
// fee rate from a floor into the rate the transaction actually pays.
func Verify(raw []byte, exp Expectation) (*Verification, error) {
	params, err := plan.Params(exp.Chain)
	if err != nil {
		return nil, err
	}
	if exp.PaysTo == "" {
		return nil, fmt.Errorf("nothing says where the child is supposed to pay, so " +
			"there is nothing to check it against")
	}
	want, err := plan.ScriptFor(exp.PaysTo, params)
	if err != nil {
		return nil, fmt.Errorf("the address the child should pay back to: %w", err)
	}
	if exp.Parent.Change.TxID == "" {
		return nil, fmt.Errorf("nothing says which outpoint the child is supposed " +
			"to spend")
	}

	packet, err := psbt.NewFromRawBytes(bytes.NewReader(raw), false)
	if err != nil {
		return nil, fmt.Errorf("that is not a PSBT: %w", err)
	}
	if err := packet.SanityCheck(); err != nil {
		return nil, fmt.Errorf("the PSBT is malformed: %w", err)
	}

	v := &Verification{TxID: packet.UnsignedTx.TxHash().String()}
	add := func(code Code, where, headline, detail string) {
		v.Problems = append(v.Problems, Problem{Code: code, Where: where,
			Headline: headline, Detail: detail})
	}

	prevScripts, known := checkInputs(packet, exp, v, add)
	checkOutputs(packet, want, exp, params, v, add)
	if known {
		checkArithmetic(packet, prevScripts, exp, v, add)
	}

	sort.SliceStable(v.Problems, func(i, j int) bool {
		return v.Problems[i].Code < v.Problems[j].Code
	})
	return v, nil
}

// checkInputs is the heart of it: a CPFP child spends the parent's change and
// nothing else.
func checkInputs(packet *psbt.Packet, exp Expectation, v *Verification,
	add func(Code, string, string, string)) ([][]byte, bool) {

	tx := packet.UnsignedTx
	prevScripts := make([][]byte, len(tx.TxIn))

	if n := len(tx.TxIn); n != 1 {
		add(WrongInputCount, "",
			fmt.Sprintf("The child spends %d inputs. It has to spend exactly one.", n),
			"A second input would make a cheaper child, and it would also let a "+
				"miner take the child without the parent — which is the one thing a "+
				"CPFP child must not allow, because then the fee buys nothing for "+
				"the batch. add_inputs is off at build time for the same reason.")
		if n == 0 {
			return prevScripts, false
		}
	}

	for i, txIn := range tx.TxIn {
		where := fmt.Sprintf("input %d", i)
		op := plan.Outpoint{TxID: txIn.PreviousOutPoint.Hash.String(),
			Vout: txIn.PreviousOutPoint.Index}

		if op != exp.Parent.Change {
			add(WrongInput, where,
				fmt.Sprintf("It spends %s, not the batch's change output %s.",
					op, exp.Parent.Change),
				"Only a transaction spending an output of the parent accelerates the "+
					"parent. A child of anything else is a fee paid for nothing, and "+
					"under I-4 the batch would still have no remedy.")
		}

		// I-4's habit rather than I-4 itself. Replacing the child would move no
		// funding outpoint and would be safe, and the build sets
		// replaceable: false anyway; this is the check that the returned packet
		// still says so.
		if txIn.Sequence < plan.MaxNonReplaceableSequence {
			add(Replaceable, where,
				fmt.Sprintf("Sequence is %#x, which signals BIP-125 replaceability.",
					txIn.Sequence),
				"The child is built non-replaceable, so this is a change somebody "+
					"made to it. Replacing the child would not by itself touch a "+
					"funding outpoint — I-4 is about the parent — but a returned "+
					"transaction that is not the one that was built is not one to sign.")
		}

		in := packet.Inputs[i]
		var value int64
		switch {
		case in.WitnessUtxo != nil:
			prevScripts[i] = in.WitnessUtxo.PkScript
			value = in.WitnessUtxo.Value
		case in.NonWitnessUtxo != nil:
			// Checked to belong to the input, for the same reason internal/plan
			// checks it: psbt.SumUtxoInputValues reads this field without
			// verifying it is the input's own previous transaction, so a wrong
			// one makes the input total — and therefore the fee — a fiction.
			if h := in.NonWitnessUtxo.TxHash(); h != txIn.PreviousOutPoint.Hash {
				add(MismatchedUTXO, where,
					fmt.Sprintf("The attached previous transaction is %s, but this "+
						"input spends %s.", h, txIn.PreviousOutPoint.Hash), "")
				continue
			}
			idx := int(txIn.PreviousOutPoint.Index)
			if idx >= len(in.NonWitnessUtxo.TxOut) {
				add(MismatchedUTXO, where,
					fmt.Sprintf("The attached previous transaction has %d outputs, "+
						"so it has no output %d.", len(in.NonWitnessUtxo.TxOut), idx), "")
				continue
			}
			prevScripts[i] = in.NonWitnessUtxo.TxOut[idx].PkScript
			value = in.NonWitnessUtxo.TxOut[idx].Value
		default:
			add(NoUTXOInfo, where,
				fmt.Sprintf("%s carries neither a witness UTXO nor the transaction it "+
					"came from.", op),
				"Without it neither the amount nor the script is knowable, so the fee "+
					"cannot be worked out and the input cannot be shown to be SegWit.")
			continue
		}

		v.InputSat += value
		if op == exp.Parent.Change && value != exp.Parent.ChangeSat {
			add(WrongInputValue, where,
				fmt.Sprintf("The PSBT says this coin is worth %s; the cold wallet "+
					"says %s.", prose.Sats(value), prose.Sats(exp.Parent.ChangeSat)),
				"The fee is the input less the output, so a wrong input value makes "+
					"every figure below it wrong — including the rate this child was "+
					"built to reach.")
		}

		segwit, why := plan.IsSegwitSpend(prevScripts[i], in.RedeemScript)
		if !segwit {
			add(LegacyInput, where,
				fmt.Sprintf("%s is not a SegWit spend: %s.", op, why),
				"A legacy spend is malleable, so the child's own txid could move "+
					"after it was signed. Nothing else depends on that txid the way "+
					"n channels depend on the parent's, but a transaction whose "+
					"identity can change is one whose size and fee cannot be "+
					"promised either.")
		}
		if len(in.FinalScriptWitness) == 0 && len(in.FinalScriptSig) == 0 {
			continue
		}
		v.Signed = true
	}

	for _, s := range prevScripts {
		if s == nil {
			return prevScripts, false
		}
	}
	return prevScripts, true
}

// checkOutputs: one output, to the script the app asked Core for.
func checkOutputs(packet *psbt.Packet, want []byte, exp Expectation,
	params *chaincfg.Params, v *Verification, add func(Code, string, string, string)) {

	tx := packet.UnsignedTx
	if n := len(tx.TxOut); n != 1 {
		add(WrongOutputCount, "",
			fmt.Sprintf("The child pays %d outputs. It has to pay exactly one.", n),
			"The only input is the batch's change and the output claims all of it "+
				"less the fee — subtractFeeFromOutputs is what makes that work, and "+
				"it is also what stops Core adding a change output of its own. A "+
				"second output is either Core having done something else or somebody "+
				"having edited the transaction.")
	}

	for i, out := range tx.TxOut {
		where := fmt.Sprintf("output %d", i)
		v.OutputSat += out.Value

		if !bytes.Equal(out.PkScript, want) {
			add(WrongOutput, where,
				fmt.Sprintf("%s to %s, and the child was built to pay %s.",
					prose.Sats(out.Value), addressOf(out.PkScript, params), exp.PaysTo),
				"This address came from the cold wallet's own getrawchangeaddress, so "+
					"the script is named exactly rather than recognised. An output to "+
					"anything else is the batch's change leaving cold storage.")
		}
		if out.Value < plan.DustSat {
			add(DustOutput, where,
				fmt.Sprintf("It returns %s, below the %s dust floor, so it would not "+
					"relay.", prose.Sats(out.Value), prose.Sats(plan.DustSat)),
				"The lift asked for costs more than the change can pay and still "+
					"leave a spendable output. A smaller lift is the remaining option.")
		}
	}
}

// checkArithmetic is the part that decides whether this child is worth signing:
// the fee is the one that was worked out, and the pair reaches the target.
func checkArithmetic(packet *psbt.Packet, prevScripts [][]byte, exp Expectation,
	v *Verification, add func(Code, string, string, string)) {

	v.FeeSat = v.InputSat - v.OutputSat

	if exp.FeeSat > 0 && v.FeeSat != exp.FeeSat {
		add(WrongFee, "",
			fmt.Sprintf("The child pays %s and the arithmetic called for %s.",
				prose.Sats(v.FeeSat), prose.Sats(exp.FeeSat)),
			"Core worked the fee out from the package rate and this build checked "+
				"that answer against plan.ChildFeeSat before anything was signed. A "+
				"different figure here means the transaction is not the one that was "+
				"checked.")
	}

	var buf bytes.Buffer
	if err := packet.Serialize(&buf); err != nil {
		add(Unsizable, "", "The child could not be re-serialised to be sized: "+err.Error(), "")
		return
	}
	size, err := plan.SizeOf(buf.Bytes())
	if err != nil {
		add(Unsizable, "", "The child's size cannot be worked out: "+err.Error(),
			"Without a size there is no fee rate, and the whole purpose of this "+
				"transaction is a fee rate.")
		return
	}
	v.VsizeVB = size.Vsize
	if size.Vsize <= 0 || v.FeeSat <= 0 {
		return
	}

	v.ChildRate = float64(v.FeeSat) / float64(size.Vsize)
	v.PackageVB = exp.Parent.VsizeVB + size.Vsize
	v.PackageFee = exp.Parent.FeeSat + v.FeeSat
	v.PackageRate = float64(v.PackageFee) / float64(v.PackageVB)

	if v.PackageRate < exp.TargetSatPerVB*(1-settle.RateTolerance) {
		add(NotBumped, "",
			fmt.Sprintf("The pair pays %.2f sat/vB and the target is %.2f.",
				v.PackageRate, exp.TargetSatPerVB),
			fmt.Sprintf("A %d vB child at %s on top of a %d vB parent at %s is what "+
				"that comes to. The usual cause is that the parent is no longer "+
				"unconfirmed: Core only charges for ancestors still in the mempool, "+
				"and a confirmed parent needs no child.",
				size.Vsize, prose.Sats(v.FeeSat), exp.Parent.VsizeVB,
				prose.Sats(exp.Parent.FeeSat)))
	}

	// The ceiling this build cannot raise. See MaxChildFeeRateSatPerVB.
	if v.ChildRate > MaxChildFeeRateSatPerVB {
		add(AboveRelayCeiling, "",
			fmt.Sprintf("The child's own fee rate is %.0f sat/vB, above the %d sat/vB "+
				"ceiling the node will broadcast.", v.ChildRate,
				MaxChildFeeRateSatPerVB),
			ceilingDetail(v.ChildRate))
	}
}

// Recheck runs the verifier over the finalized child, and adds the one check
// that only makes sense once every witness is present.
//
// The size the first pass used was an upper bound — 73-byte signatures, the
// largest DER can be — which made the rate it reported a floor. With the
// witnesses in hand the size is a measurement, so the real transaction can be
// smaller but must not be larger. Larger would mean the fee rate the operator
// approved was not a floor after all, and the arithmetic behind the whole
// transaction would be wrong in the one direction that matters: I-4 leaves no
// second attempt on the parent, so a child that under-delivers costs another
// cold-wallet session.
func Recheck(raw []byte, exp Expectation, estimatedVsizeVB int64) (*Verification, error) {
	v, err := Verify(raw, exp)
	if err != nil {
		return nil, err
	}
	if !v.Signed {
		v.Problems = append(v.Problems, Problem{
			Code:     Unsizable,
			Headline: "The child that came back carries no witnesses.",
			Detail: "Recheck runs on the transaction the merge produced, which is " +
				"fully signed by construction. A packet with no witnesses here means " +
				"the wrong bytes were handed to it.",
		})
		return v, nil
	}
	if estimatedVsizeVB > 0 && v.VsizeVB > estimatedVsizeVB {
		v.Problems = append(v.Problems, Problem{
			Code: SizeGrew,
			Headline: fmt.Sprintf("The signed child is %d vB and was estimated at %d.",
				v.VsizeVB, estimatedVsizeVB),
			Detail: "The estimate uses upper bounds on every signature, so the real " +
				"transaction is meant to be the same size or smaller. Larger means " +
				"the rate that was approved was not the floor it was presented as.",
		})
	}
	sort.SliceStable(v.Problems, func(i, j int) bool {
		return v.Problems[i].Code < v.Problems[j].Code
	})
	return v, nil
}

// addressOf renders a script as an address, or as hex when it has none — an
// output the child was not supposed to pay has to be showable either way, and a
// non-standard script is exactly where that matters most.
func addressOf(script []byte, params *chaincfg.Params) string {
	_, addrs, _, err := txscript.ExtractPkScriptAddrs(script, params)
	if err != nil || len(addrs) == 0 {
		return "a non-standard script (" + hex.EncodeToString(script) + ")"
	}
	return addrs[0].EncodeAddress()
}
