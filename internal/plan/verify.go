package plan

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"sort"

	"github.com/AusDavo/winthistle/internal/prose"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// MaxNonReplaceableSequence is the largest input sequence that still signals
// BIP-125 opt-in replaceability. A transaction is replaceable if *any* input is
// below 0xfffffffe, so every input has to be at or above it.
//
// I-4: replacing the funding transaction changes every outpoint and destroys
// every channel in the batch. This is not adjustable and there is no mode in
// which the app accepts a replaceable funding transaction.
const MaxNonReplaceableSequence = wire.MaxTxInSequenceNum - 1

// Code identifies a finding, so a UI can react to one without matching prose.
type Code int

const (
	Unparseable Code = iota
	NoInputs
	UnnamedOutput
	MissingOutput
	DuplicateOutput
	WrongAmount
	ChangeMissing
	ChangeAmbiguous
	ChangeTooSmall
	LegacyInput
	NoUTXOInfo
	MismatchedUTXO
	InputNotAllowed
	DuplicateInput
	Replaceable
	NoFee
	FeeTooLow
	FeeTooHigh
	Unsizable
)

func (c Code) String() string {
	switch c {
	case Unparseable:
		return "unparseable"
	case NoInputs:
		return "no inputs"
	case UnnamedOutput:
		return "output the plan does not name"
	case MissingOutput:
		return "output missing"
	case DuplicateOutput:
		return "output present more than once"
	case WrongAmount:
		return "wrong amount"
	case ChangeMissing:
		return "no change output"
	case ChangeAmbiguous:
		return "change output ambiguous"
	case ChangeTooSmall:
		return "change too small to fund a CPFP child"
	case LegacyInput:
		return "input is not a segwit spend"
	case NoUTXOInfo:
		return "input carries no utxo information"
	case MismatchedUTXO:
		return "input's utxo does not belong to it"
	case InputNotAllowed:
		return "input is not in the planned coin set"
	case DuplicateInput:
		return "input spent twice"
	case Replaceable:
		return "replaceable"
	case NoFee:
		return "no fee"
	case FeeTooLow:
		return "fee rate below the plan"
	case FeeTooHigh:
		return "fee rate above the plan"
	case Unsizable:
		return "size cannot be estimated"
	default:
		return "unknown"
	}
}

// Problem is one reason to refuse the returned transaction.
//
// Every problem is a refusal. There is no severity here on purpose: this
// verifier runs between the operator building a transaction and the app asking
// n peers to commit to it, and a finding worth printing at that moment is worth
// stopping for.
type Problem struct {
	Code     Code
	Where    string // "output 3", "input 0", ""
	Headline string
	Detail   string
}

func (p Problem) String() string {
	if p.Where == "" {
		return p.Headline
	}
	return p.Where + ": " + p.Headline
}

// Attribution is one output of the returned transaction, and what the plan says
// it is.
type Attribution struct {
	Index     int
	AmountSat int64
	Script    []byte
	Address   string

	Named bool
	Kind  Kind
	Label string

	// Recognised marks a change output identified by key origin rather than by
	// script. See Recognition — it is a weaker claim and the report says so.
	Recognised bool
}

// InputView is one input of the returned transaction.
type InputView struct {
	Index     int
	Outpoint  Outpoint
	AmountSat int64
	Script    []byte
	Segwit    bool
	Sequence  uint32
}

// Verification is everything the verifier learned.
type Verification struct {
	Chain string

	// UnsignedTxID is what I-3 pins. LND commits to the funding outpoint at
	// psbt_verify — "no inputs or outputs can change, only signatures can be
	// added" — so this is the value every later PSBT has to still hash to.
	UnsignedTxID string

	Inputs  []InputView
	Outputs []Attribution

	InputSat  int64
	OutputSat int64
	FeeSat    int64

	Size    Size
	FeeRate float64

	// ChangeSat and ChangeFloorSat are the I-4 arithmetic, when it could be done.
	ChangeSat      int64
	ChangeFloorSat int64

	Problems []Problem

	// Unchecked names what this verification could not establish, so that a
	// clean result is not read as a broader guarantee than it is.
	Unchecked []string
}

// OK reports whether the transaction may go on to psbt_verify.
func (v *Verification) OK() bool { return len(v.Problems) == 0 }

// VerifyBase64 verifies a PSBT in the encoding a browser upload carries.
func (p *Plan) VerifyBase64(s string) (*Verification, error) {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("that is not a base64 PSBT: %w", err)
	}
	return p.Verify(raw)
}

// Verify checks a returned PSBT against the plan.
//
// It returns an error only when the plan itself is unusable or the bytes are not
// a PSBT. Everything the transaction does wrong is a Problem on the returned
// Verification, because the operator has to see all of them at once — a verifier
// that stops at the first fault turns one signing round into several.
func (p *Plan) Verify(raw []byte) (*Verification, error) {
	named, err := p.Outputs()
	if err != nil {
		return nil, err
	}
	params, err := Params(p.Chain)
	if err != nil {
		return nil, err
	}

	packet, err := psbt.NewFromRawBytes(bytes.NewReader(raw), false)
	if err != nil {
		return nil, fmt.Errorf("that is not a PSBT: %w", err)
	}
	if err := packet.SanityCheck(); err != nil {
		return nil, fmt.Errorf("the PSBT is malformed: %w", err)
	}

	v := &Verification{
		Chain:        p.Chain,
		UnsignedTxID: packet.UnsignedTx.TxHash().String(),
	}
	add := func(code Code, where, headline, detail string) {
		v.Problems = append(v.Problems, Problem{Code: code, Where: where,
			Headline: headline, Detail: detail})
	}

	prevScripts, inputsKnown := p.checkInputs(packet, v, add)
	p.checkOutputs(packet, named, params, v, add)
	if inputsKnown {
		p.checkFee(packet, prevScripts, v, add)
	} else {
		// Without every input's value there is no fee, and without every input's
		// script there is no size. Reporting either from a partial sum would put
		// a confident wrong number next to a real problem.
		v.Unchecked = append(v.Unchecked, "the fee and the fee rate: at least one "+
			"input does not say what it spends, so neither the input total nor the "+
			"transaction's size can be worked out")
	}

	v.Unchecked = append(v.Unchecked,
		"whether the inputs are confirmed — a PSBT carries no chain height, so "+
			"this is enforced during coin selection in directed mode and stated as a "+
			"constraint in the plan otherwise",
		"node policy: min relay fee, standardness and ancestor limits. "+
			"testmempoolaccept answers those and needs Core")

	sort.SliceStable(v.Problems, func(i, j int) bool {
		return v.Problems[i].Code < v.Problems[j].Code
	})
	return v, nil
}

// checkInputs walks the inputs and returns each one's prevout script, which the
// sizing pass needs.
func (p *Plan) checkInputs(packet *psbt.Packet, v *Verification,
	add func(Code, string, string, string)) ([][]byte, bool) {

	tx := packet.UnsignedTx
	prevScripts := make([][]byte, len(tx.TxIn))

	if len(tx.TxIn) == 0 {
		add(NoInputs, "", "The transaction spends nothing.",
			"LND refuses this too, at psbt_verify.")
		return prevScripts, false
	}

	allowed := map[Outpoint]bool{}
	for _, op := range p.Inputs.Allowed {
		allowed[op] = true
	}
	seen := map[wire.OutPoint]int{}

	for i, txIn := range tx.TxIn {
		where := fmt.Sprintf("input %d", i)
		op := Outpoint{TxID: txIn.PreviousOutPoint.Hash.String(),
			Vout: txIn.PreviousOutPoint.Index}
		view := InputView{Index: i, Outpoint: op, Sequence: txIn.Sequence}

		if first, dup := seen[txIn.PreviousOutPoint]; dup {
			add(DuplicateInput, where,
				fmt.Sprintf("%s is already spent by input %d.", op, first), "")
		}
		seen[txIn.PreviousOutPoint] = i

		// I-4. A transaction is replaceable if any single input signals it.
		if txIn.Sequence < MaxNonReplaceableSequence {
			add(Replaceable, where,
				fmt.Sprintf("Sequence is %#x, which signals BIP-125 replaceability.", txIn.Sequence),
				"Replacing the funding transaction changes every outpoint in it and "+
					"destroys every channel in the batch. Rebuild with replaceability "+
					"off — in Sparrow that is the RBF toggle on the transaction; in "+
					"Core it is walletcreatefundedpsbt's \"replaceable\": false.")
		}

		in := packet.Inputs[i]
		switch {
		case in.WitnessUtxo != nil:
			prevScripts[i] = in.WitnessUtxo.PkScript
			view.AmountSat = in.WitnessUtxo.Value
		case in.NonWitnessUtxo != nil:
			// LND's psbt.SumUtxoInputValues trusts this field to belong to the
			// input. Nothing checks that it does, so we do: a non-witness UTXO
			// from a different transaction makes the input sum — and therefore
			// the fee — a fiction.
			if h := in.NonWitnessUtxo.TxHash(); h != txIn.PreviousOutPoint.Hash {
				add(MismatchedUTXO, where,
					fmt.Sprintf("The attached previous transaction is %s, but this "+
						"input spends %s.", h, txIn.PreviousOutPoint.Hash), "")
				break
			}
			idx := int(txIn.PreviousOutPoint.Index)
			if idx >= len(in.NonWitnessUtxo.TxOut) {
				add(MismatchedUTXO, where,
					fmt.Sprintf("The attached previous transaction has %d outputs, "+
						"so it has no output %d.", len(in.NonWitnessUtxo.TxOut), idx), "")
				break
			}
			prevScripts[i] = in.NonWitnessUtxo.TxOut[idx].PkScript
			view.AmountSat = in.NonWitnessUtxo.TxOut[idx].Value
		default:
			add(NoUTXOInfo, where,
				fmt.Sprintf("%s carries neither a witness UTXO nor the transaction it "+
					"came from.", op),
				"Without it neither the amount nor the script is knowable, so the fee "+
					"cannot be computed and the input cannot be shown to be SegWit.")
		}

		if prevScripts[i] != nil {
			view.Script = prevScripts[i]
			segwit, why := IsSegwitSpend(prevScripts[i], in.RedeemScript)
			view.Segwit = segwit
			if !segwit {
				add(LegacyInput, where,
					fmt.Sprintf("%s is not a SegWit spend: %s.", op, why),
					"LND refuses this outright — verifyAllInputsSegWit, \"risk of "+
						"malleability\" — and I-3 is the same fact from the other side: "+
						"a malleable input is a TXID that can move after psbt_verify has "+
						"committed to it.")
			}
		}

		if len(allowed) > 0 && !allowed[op] {
			add(InputNotAllowed, where,
				fmt.Sprintf("%s is not one of the %d coins the plan named.", op, len(allowed)),
				"The plan locked a coin set during Phase 0. A coin outside it may be "+
					"unconfirmed, legacy, or reserved for something else.")
		}

		v.Inputs = append(v.Inputs, view)
		v.InputSat += view.AmountSat
	}

	for _, s := range prevScripts {
		if s == nil {
			return prevScripts, false
		}
	}
	return prevScripts, true
}

// checkOutputs is the part LND does not do.
func (p *Plan) checkOutputs(packet *psbt.Packet, named []Named,
	params *chaincfg.Params, v *Verification, add func(Code, string, string, string)) {

	byScript := make(map[string]*Named, len(named))
	count := make(map[string]int, len(named))
	for i := range named {
		byScript[string(named[i].Script)] = &named[i]
	}

	tx := packet.UnsignedTx
	for i, out := range tx.TxOut {
		where := fmt.Sprintf("output %d", i)
		a := Attribution{Index: i, AmountSat: out.Value, Script: out.PkScript,
			Address: addressOf(out.PkScript, params)}
		v.OutputSat += out.Value

		n, ok := byScript[string(out.PkScript)]
		if ok {
			count[string(out.PkScript)]++
			a.Named, a.Kind, a.Label = true, n.Kind, n.Label
			if n.Exact && out.Value != n.AmountSat {
				add(WrongAmount, where,
					fmt.Sprintf("%s pays %d sat; the plan says %d sat.",
						n.Label, out.Value, n.AmountSat),
					"LND compares its own funding output with psbt.TxOutsEqual, which "+
						"compares the value as well as the script, so this channel would "+
						"not verify.")
			}
			if !n.Exact && n.AmountSat > 0 && out.Value < n.AmountSat {
				add(WrongAmount, where,
					fmt.Sprintf("%s pays %d sat; the plan asks for at least %d sat.",
						n.Label, out.Value, n.AmountSat), "")
			}
			v.Outputs = append(v.Outputs, a)
			continue
		}

		// Unnamed. In assisted mode Sparrow chooses its own change address, so
		// this is where an output is allowed to be change on the strength of its
		// key origin rather than its script.
		if p.Change.Address == "" && p.Change.Recognise != nil &&
			p.Change.Recognise.matches(packet.Outputs[i]) {

			a.Named, a.Recognised = true, true
			a.Kind, a.Label = ChangeOut, "the change output"
			v.Outputs = append(v.Outputs, a)
			continue
		}

		add(UnnamedOutput, where,
			fmt.Sprintf("%s to %s, which the plan does not name.",
				prose.Sats(out.Value), a.Address),
			"This is the check nothing else makes. LND's psbt_verify looks for its "+
				"own funding output and stops — it never asserts that its output is "+
				"the only one, which is what lets n channels share one transaction. "+
				"So an output nobody named passes every psbt_verify in the batch.")
		v.Outputs = append(v.Outputs, a)
	}

	for _, n := range named {
		switch c := count[string(n.Script)]; {
		case c == 0 && n.Kind == ChangeOut:
			// checkChange reports this, in copy that says why it matters.
		case c == 0:
			add(MissingOutput, "",
				fmt.Sprintf("%s is not in the transaction at all (%s, %d sat).",
					n.Label, n.Address, n.AmountSat), "")
		case c > 1:
			add(DuplicateOutput, "",
				fmt.Sprintf("%s appears %d times. The plan names it once.", n.Label, c),
				"LND would be satisfied — psbt_verify sets a found flag and does not "+
					"count — but the batch would pay twice.")
		}
	}

	p.checkChange(v, add)
}

// checkChange finds the change output and reports if there is not exactly one.
func (p *Plan) checkChange(v *Verification, add func(Code, string, string, string)) {
	var found []Attribution
	for _, a := range v.Outputs {
		if a.Named && a.Kind == ChangeOut {
			found = append(found, a)
		}
	}
	switch len(found) {
	case 0:
		add(ChangeMissing, "", "The transaction has no change output.",
			"I-4 forbids replacing this transaction, so its change output is the "+
				"only thing that can ever accelerate it. A batch without one is a "+
				"batch that can only be waited out.")
	case 1:
		v.ChangeSat = found[0].AmountSat
	default:
		idx := make([]int, 0, len(found))
		for _, a := range found {
			idx = append(idx, a.Index)
		}
		add(ChangeAmbiguous, "",
			fmt.Sprintf("%d outputs look like change (%v).", len(found), idx),
			"The plan expects one, and which one is the CPFP lever cannot be "+
				"guessed.")
	}
}

// checkFee does the arithmetic and the I-4 change sizing.
func (p *Plan) checkFee(packet *psbt.Packet, prevScripts [][]byte,
	v *Verification, add func(Code, string, string, string)) {

	v.FeeSat = v.InputSat - v.OutputSat

	// LND's own rule, from PsbtIntent.Verify: "input amount sum must be larger
	// than output amount sum". It does no fee estimation beyond that, which is
	// why the rate check below is ours.
	if v.InputSat > 0 && v.FeeSat <= 0 {
		add(NoFee, "",
			fmt.Sprintf("The inputs total %d sat and the outputs %d sat, so the fee is %d.",
				v.InputSat, v.OutputSat, v.FeeSat),
			"LND refuses this at psbt_verify: the input sum must exceed the output sum.")
		return
	}

	size, err := estimateSize(packet, prevScripts)
	if err != nil {
		add(Unsizable, "", "The transaction's size cannot be estimated: "+err.Error(),
			"Without a size there is no fee rate to check, and an input whose spend "+
				"shape cannot be read is one the plan did not anticipate.")
		return
	}
	v.Size = size
	if size.Vsize <= 0 || v.FeeSat <= 0 {
		return
	}
	v.FeeRate = float64(v.FeeSat) / float64(size.Vsize)

	switch {
	case v.FeeRate < p.Fee.Low():
		add(FeeTooLow, "",
			fmt.Sprintf("The fee rate is %.2f sat/vB; the plan targets %.2f.",
				v.FeeRate, p.Fee.TargetSatPerVB),
			"There is no RBF available here (I-4), so a batch that goes out too "+
				"cheap can only be pushed by a CPFP child or waited out.")
	case v.FeeRate > p.Fee.High():
		add(FeeTooHigh, "",
			fmt.Sprintf("The fee rate is %.2f sat/vB; the plan targets %.2f.",
				v.FeeRate, p.Fee.TargetSatPerVB),
			"Not dangerous, but it is not what was approved, and the difference is "+
				"paid out of the change.")
	}

	p.checkChangeSize(packet, v, add)
}

// checkChangeSize is I-4's arithmetic: can the change output still buy a child
// that lifts this transaction?
func (p *Plan) checkChangeSize(packet *psbt.Packet, v *Verification,
	add func(Code, string, string, string)) {

	var change *Attribution
	for i := range v.Outputs {
		if v.Outputs[i].Named && v.Outputs[i].Kind == ChangeOut {
			change = &v.Outputs[i]
			break
		}
	}
	if change == nil {
		return
	}
	if p.Change.MinimumSat > 0 && change.AmountSat < p.Change.MinimumSat {
		add(ChangeTooSmall, fmt.Sprintf("output %d", change.Index),
			fmt.Sprintf("Change is %d sat; the plan set a floor of %d sat.",
				change.AmountSat, p.Change.MinimumSat), "")
	}

	// The child's own witness script, when the PSBT carries it. Core and Sparrow
	// both attach it for an output of their own wallet, and without it a
	// multisig child cannot be sized.
	witnessScript := packet.Outputs[change.Index].WitnessScript
	child, err := ChildVsize(change.Script, witnessScript)
	if err != nil {
		note := fmt.Sprintf("whether the change output could fund a CPFP child: %s", err)
		if p.Change.MinimumSat > 0 {
			note += fmt.Sprintf(". The plan's own floor of %s was still applied",
				prose.Sats(p.Change.MinimumSat))
		}
		v.Unchecked = append(v.Unchecked, note)
		return
	}
	floor := ChangeFloor(v.Size.Vsize, v.FeeSat, p.Fee.cpfpTarget(), child)
	v.ChangeFloorSat = floor
	if change.AmountSat < floor {
		add(ChangeTooSmall, fmt.Sprintf("output %d", change.Index),
			fmt.Sprintf("Change is %d sat, which cannot fund a child big enough to "+
				"lift this batch to %.2f sat/vB.", change.AmountSat, p.Fee.cpfpTarget()),
			fmt.Sprintf("A %d vB child on top of a %d vB parent needs %d sat of "+
				"change to leave anything above the %d sat dust limit. Reduce a "+
				"funding amount or add a coin.",
				child, v.Size.Vsize, floor, DustSat))
	}
}

// matches reports whether a PSBT output's key-origin information says it belongs
// to the cold wallet's change branch.
//
// This is the same evidence a hardware signer uses to decide an output is its own
// change rather than a payment, and it is strictly weaker than naming the script:
// it proves the keys are the cold wallet's, not that the amount or the index is
// the one intended. Every fingerprint has to be one of the wallet's, all of them
// have to be present, and the derivation has to be on the change branch.
func (r *Recognition) matches(out psbt.POutput) bool {
	want := make(map[uint32]bool, len(r.Fingerprints))
	for _, fp := range r.Fingerprints {
		want[fp] = true
	}
	got := map[uint32]bool{}

	check := func(fp uint32, path []uint32) bool {
		if !want[fp] {
			return false
		}
		if len(path) < 2 || path[len(path)-2] != r.Branch {
			return false
		}
		got[fp] = true
		return true
	}

	var seen int
	for _, d := range out.Bip32Derivation {
		if !check(d.MasterKeyFingerprint, d.Bip32Path) {
			return false
		}
		seen++
	}
	for _, d := range out.TaprootBip32Derivation {
		if !check(d.MasterKeyFingerprint, d.Bip32Path) {
			return false
		}
		seen++
	}
	return seen > 0 && len(got) == len(want)
}

// IsSegwitSpend reports whether spending an output with this scriptPubKey is a
// SegWit spend.
//
// It reads the script rather than trusting a field, which is where it differs
// from LND. verifyAllInputsSegWit accepts an input the moment a WitnessUtxo is
// present:
//
//	case in.WitnessUtxo != nil:
//
// with no look at what that UTXO's pkScript actually is. A PSBT that attaches a
// WitnessUtxo to a P2PKH input therefore satisfies LND's malleability check
// while remaining malleable. Only the NonWitnessUtxo branch inspects the script.
//
// The two cases below are the ones LND's NonWitnessUtxo branch tests for, applied
// to every input: a witness program spends natively, and a P2SH output spends as
// SegWit only if its redeem script is itself a witness program.
func IsSegwitSpend(pkScript, redeemScript []byte) (bool, string) {
	if txscript.IsWitnessProgram(pkScript) {
		return true, ""
	}
	if !txscript.IsPayToScriptHash(pkScript) {
		return false, fmt.Sprintf("it pays a %s script, which spends without a witness",
			txscript.GetScriptClass(pkScript))
	}
	if len(redeemScript) == 0 {
		return false, "it is P2SH and the PSBT carries no redeem script, so a " +
			"wrapped SegWit spend cannot be told from a legacy one"
	}
	if !txscript.IsWitnessProgram(redeemScript) {
		return false, "it is P2SH and its redeem script is not a witness program, " +
			"so it is a legacy spend"
	}
	return true, ""
}

// addressOf renders a script as an address, or as hex when it has none. An
// output the plan does not name has to be shown to the operator somehow, and a
// non-standard script is exactly the case where that matters most.
func addressOf(script []byte, params *chaincfg.Params) string {
	_, addrs, _, err := txscript.ExtractPkScriptAddrs(script, params)
	if err != nil || len(addrs) == 0 {
		return "a non-standard script (" + hex.EncodeToString(script) + ")"
	}
	return addrs[0].EncodeAddress()
}
