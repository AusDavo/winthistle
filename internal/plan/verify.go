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
	LegacyInput
	NoUTXOInfo
	MismatchedUTXO
	InputNotAllowed
	DuplicateInput
	NoFee
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
	case NoFee:
		return "no fee"
	case Unsizable:
		return "size cannot be estimated"
	default:
		return "unknown"
	}
}

// Problem is one reason to refuse the returned transaction.
//
// Every problem is a refusal. There is no severity here on purpose: a verifier
// that graded its own findings would be a verifier whose refusals could be
// argued down, and the whole product is the check that every output is
// accounted for.
//
// What the verifier establishes and does not refuse over is a Finding, in a
// different list and a deliberately different type. The split is which list a
// thing lands in, never a field somebody reads afterwards, so OK() cannot be
// made to depend on a grade that was set wrong.
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

// Finding is something the verifier established and does not refuse over.
//
// One code lives here: ChangeMissing. Your change arrangements are yours. This
// app does not build the transaction and does not select the coins, so all it
// can do is say what yours came out as, and it says so rather than blocking a
// batch over it.
//
// There were four. FeeTooLow, FeeTooHigh and ChangeTooSmall left with the
// declared fee rate, because each of them judged the transaction against a
// number the operator had typed twice — and the app that does not choose the fee
// has no standing to hold them to it. The type stays for ChangeMissing, and
// because the split between refusing and reporting is worth keeping expressible.
//
// It carries the same fields as a Problem because a report wants the same
// numbers a refusal did: the Where a finding attaches to, and the Detail that
// says what the arithmetic was.
type Finding struct {
	Code     Code
	Where    string // "output 3", "input 0", ""
	Headline string
	Detail   string
}

func (f Finding) String() string {
	if f.Where == "" {
		return f.Headline
	}
	return f.Where + ": " + f.Headline
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

	// ChangeSat is the change output's amount, which is attribution rather than
	// judgement: the report says what it came out as and nothing grades it.
	ChangeSat int64

	Problems []Problem

	// Reports is what the verifier established and does not refuse over. It has
	// no bearing on OK(): a batch whose only findings are here arms with no
	// further prompt, which is the point of them being here.
	Reports []Finding

	// Unchecked names what this verification could not establish, so that a
	// clean result is not read as a broader guarantee than it is.
	//
	// Reports is the other list, and the two are not interchangeable. "Your
	// change is 4,000 sat and the floor is 21,000" is something this verifier
	// did establish, and filing it under a heading that says otherwise would
	// cost that heading the only thing it is for.
	Unchecked []string
}

// OK reports whether the transaction may go on to psbt_verify.
func (v *Verification) OK() bool { return len(v.Problems) == 0 }

// refuse records a reason not to proceed. See Problem.
func (v *Verification) refuse(code Code, where, headline, detail string) {
	v.Problems = append(v.Problems, Problem{Code: code, Where: where,
		Headline: headline, Detail: detail})
}

// note records something established and not refused over. See Finding.
func (v *Verification) note(code Code, where, headline, detail string) {
	v.Reports = append(v.Reports, Finding{Code: code, Where: where,
		Headline: headline, Detail: detail})
}

// VerifyBase64 verifies a PSBT that is already in base64.
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
	prevScripts, inputsKnown := p.checkInputs(packet, v)
	p.checkOutputs(packet, named, params, v)
	if inputsKnown {
		p.checkFee(packet, prevScripts, v)
	} else {
		// Without every input's value there is no fee, and without every input's
		// script there is no size. Reporting either from a partial sum would put
		// a confident wrong number next to a real problem.
		v.Unchecked = append(v.Unchecked, "the fee and the fee rate: at least one "+
			"input does not say what it spends, so neither the input total nor the "+
			"transaction's size can be worked out")
	}

	v.Unchecked = append(v.Unchecked,
		"whether the inputs are confirmed — a PSBT carries no chain height, and "+
			"this app selects no coins, so it is stated as a constraint in the plan "+
			"and enforced by the wallet that picked them",
		"node policy: min relay fee, standardness and ancestor limits. "+
			"Core's testmempoolaccept answers those and this build does not run it: "+
			"it dials no Bitcoin node at all. Each input's witness is executed "+
			"against its own script before the transaction is published, which is "+
			"narrower and is checked here")

	sort.SliceStable(v.Problems, func(i, j int) bool {
		return v.Problems[i].Code < v.Problems[j].Code
	})
	sort.SliceStable(v.Reports, func(i, j int) bool {
		return v.Reports[i].Code < v.Reports[j].Code
	})
	return v, nil
}

// checkInputs walks the inputs and returns each one's prevout script, which the
// sizing pass needs.
func (p *Plan) checkInputs(packet *psbt.Packet, v *Verification) ([][]byte, bool) {

	tx := packet.UnsignedTx
	prevScripts := make([][]byte, len(tx.TxIn))

	if len(tx.TxIn) == 0 {
		v.refuse(NoInputs, "", "The transaction spends nothing.",
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
			v.refuse(DuplicateInput, where,
				fmt.Sprintf("%s is already spent by input %d.", op, first), "")
		}
		seen[txIn.PreviousOutPoint] = i

		// Nothing judges the sequence number, and that is deliberate. A refusal
		// stood here until item 6: any input below 0xfffffffe signals BIP-125
		// opt-in replaceability, and I-4 says replacing the funding transaction
		// moves every outpoint in it and destroys every channel in the batch.
		//
		// The refusal was a lint wearing an invariant's clothes. Core 29 relays a
		// higher-fee conflict whatever the sequence numbers signal — full-RBF is
		// unconditional there, verified against a running node: mempoolfullrbf
		// does not exist even as a hidden debug option, and getmempoolinfo
		// reports "fullrbf": true with no way to turn it off. So a transaction
		// that signals nothing is no harder to replace than one that does, and
		// "replaceable": false is a statement of intent rather than a defence.
		//
		// What holds I-4 is authorship, which is all that ever held it: only we
		// can sign our inputs, and there is no code path in this repository that
		// replaces a funding transaction. Do not put the refusal back — the
		// sequence is still on InputView, so the fact is recorded and simply not
		// judged.

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
				v.refuse(MismatchedUTXO, where,
					fmt.Sprintf("The attached previous transaction is %s, but this "+
						"input spends %s.", h, txIn.PreviousOutPoint.Hash), "")
				break
			}
			idx := int(txIn.PreviousOutPoint.Index)
			if idx >= len(in.NonWitnessUtxo.TxOut) {
				v.refuse(MismatchedUTXO, where,
					fmt.Sprintf("The attached previous transaction has %d outputs, "+
						"so it has no output %d.", len(in.NonWitnessUtxo.TxOut), idx), "")
				break
			}
			prevScripts[i] = in.NonWitnessUtxo.TxOut[idx].PkScript
			view.AmountSat = in.NonWitnessUtxo.TxOut[idx].Value
		default:
			v.refuse(NoUTXOInfo, where,
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
				v.refuse(LegacyInput, where,
					fmt.Sprintf("%s is not a SegWit spend: %s.", op, why),
					"LND refuses this outright — verifyAllInputsSegWit, \"risk of "+
						"malleability\" — and I-3 is the same fact from the other side: "+
						"a malleable input is a TXID that can move after psbt_verify has "+
						"committed to it.")
			}
		}

		if len(allowed) > 0 && !allowed[op] {
			v.refuse(InputNotAllowed, where,
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
	params *chaincfg.Params, v *Verification) {

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
				v.refuse(WrongAmount, where,
					fmt.Sprintf("%s pays %d sat; the plan says %d sat.",
						n.Label, out.Value, n.AmountSat),
					"LND compares its own funding output with psbt.TxOutsEqual, which "+
						"compares the value as well as the script, so this channel would "+
						"not verify.")
			}
			if !n.Exact && n.AmountSat > 0 && out.Value < n.AmountSat {
				v.refuse(WrongAmount, where,
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

		v.refuse(UnnamedOutput, where,
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
			v.refuse(MissingOutput, "",
				fmt.Sprintf("%s is not in the transaction at all (%s, %d sat).",
					n.Label, n.Address, n.AmountSat), "")
		case c > 1:
			v.refuse(DuplicateOutput, "",
				fmt.Sprintf("%s appears %d times. The plan names it once.", n.Label, c),
				"LND would be satisfied — psbt_verify sets a found flag and does not "+
					"count — but the batch would pay twice.")
		}
	}

	p.checkChange(v)
}

// checkChange finds the change output and reports if there is not exactly one.
func (p *Plan) checkChange(v *Verification) {
	var found []Attribution
	for _, a := range v.Outputs {
		if a.Named && a.Kind == ChangeOut {
			found = append(found, a)
		}
	}
	switch len(found) {
	case 0:
		v.note(ChangeMissing, "", "The transaction has no change output.",
			"Nothing is at risk in that: the coins are yours, unspent, in a "+
				"transaction only you can sign. What it costs you is the lever. I-4 "+
				"forbids replacing this transaction, so a change output is the only "+
				"thing a CPFP child could ever spend, and a batch without one that "+
				"goes out too cheap can only be waited out — or double-spent out of "+
				"band, in your own wallet, which is yours to do and not this app's. "+
				"Your change arrangements are your own; this is said rather than "+
				"refused over.")
	case 1:
		v.ChangeSat = found[0].AmountSat
	default:
		idx := make([]int, 0, len(found))
		for _, a := range found {
			idx = append(idx, a.Index)
		}
		v.refuse(ChangeAmbiguous, "",
			fmt.Sprintf("%d outputs look like change (%v).", len(found), idx),
			"The plan expects one, and which one is the CPFP lever cannot be "+
				"guessed.")
	}
}

// checkFee does the arithmetic and the I-4 change sizing.
func (p *Plan) checkFee(packet *psbt.Packet, prevScripts [][]byte,
	v *Verification) {

	v.FeeSat = v.InputSat - v.OutputSat

	// LND's own rule, from PsbtIntent.Verify: "input amount sum must be larger
	// than output amount sum". It does no fee estimation beyond that, which is
	// why the rate check below is ours.
	if v.InputSat > 0 && v.FeeSat <= 0 {
		v.refuse(NoFee, "",
			fmt.Sprintf("The inputs total %d sat and the outputs %d sat, so the fee is %d.",
				v.InputSat, v.OutputSat, v.FeeSat),
			"LND refuses this at psbt_verify: the input sum must exceed the output sum.")
		return
	}

	size, err := estimateSize(packet, prevScripts)
	if err != nil {
		v.refuse(Unsizable, "", "The transaction's size cannot be estimated: "+err.Error(),
			"Without a size there is no fee rate to check, and an input whose spend "+
				"shape cannot be read is one the plan did not anticipate.")
		return
	}
	v.Size = size
	if size.Vsize <= 0 || v.FeeSat <= 0 {
		return
	}
	// The rate is computed and reported, and nothing judges it. There is no
	// target to judge it against: the app does not build the transaction and does
	// not choose the fee, so the only number it could compare against was one the
	// operator typed after their wallet had already shown them the real one.
	v.FeeRate = float64(v.FeeSat) / float64(size.Vsize)
}

// matches reports whether a PSBT output's key-origin information says it belongs
// to the cold wallet's change branch.
//
// This is the same evidence a hardware signer uses to decide an output is its own
// change rather than a payment, and it is strictly weaker than naming the script:
// it proves the keys are the cold wallet's, not that the amount or the index is
// the one intended. Every fingerprint has to be one of the wallet's, all of them
// have to be present, and the derivation has to be on the change branch.
// DefaultChangeBranch is the derivation branch a change address comes from. It
// is 1 in every ordinary wallet, single-sig or multisig, BIP-44 through BIP-48.
const DefaultChangeBranch uint32 = 1

// RecogniseChangeIn reads a Recognition out of the transaction's own inputs.
//
// This exists because the app no longer builds the transaction and therefore no
// longer knows the change address. The recipients are ours — LND issued them —
// but the change output is one the operator's wallet chose after we had finished
// talking, and an output nobody names is an UnnamedOutput refusal. Without a way
// to recognise change, the verifier would refuse every transaction Sparrow
// builds, which is the whole happy path.
//
// The evidence is the key origins the wallet wrote into the packet: the master
// fingerprints on the inputs are the wallet that is about to sign, and an output
// carrying those same fingerprints on the change branch is that wallet paying
// itself. It is the same evidence a hardware signer uses to decide an output is
// its own change rather than a payment, and Recognition already existed for it.
//
// # It is weaker than naming the script, and it is meant to be read that way
//
// The packet gets to nominate its own change output, which is circular, and the
// circle is deliberately small. The funding outputs are checked by script and to
// the satoshi against addresses LND issued, so nothing here can move a channel's
// money. What a lying packet could do is put the *change* somewhere that is not
// the operator's — and a wallet that lies about its own change address has
// already taken the coins it is about to sign for, whatever this verifier says.
// The report marks a recognised output as "recognised by key origin rather than
// by address" for exactly this reason, and --change names the script instead when
// an operator wants the stronger check.
//
// It returns nil, and no error, when the packet says nothing usable: a wallet
// that writes no output derivations cannot be recognised, and the caller's answer
// to that is the UnnamedOutput refusal plus the copy that says to use --change.
func RecogniseChangeIn(raw []byte) (*Recognition, error) {
	packet, err := psbt.NewFromRawBytes(bytes.NewReader(raw), false)
	if err != nil {
		return nil, fmt.Errorf("that is not a PSBT: %w", err)
	}

	seen := map[uint32]bool{}
	var fingerprints []uint32
	note := func(fp uint32) {
		if seen[fp] {
			return
		}
		seen[fp] = true
		fingerprints = append(fingerprints, fp)
	}
	for _, in := range packet.Inputs {
		for _, d := range in.Bip32Derivation {
			note(d.MasterKeyFingerprint)
		}
		for _, d := range in.TaprootBip32Derivation {
			note(d.MasterKeyFingerprint)
		}
	}
	if len(fingerprints) == 0 {
		return nil, nil
	}
	sort.Slice(fingerprints, func(i, j int) bool { return fingerprints[i] < fingerprints[j] })
	return &Recognition{Fingerprints: fingerprints, Branch: DefaultChangeBranch}, nil
}

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
