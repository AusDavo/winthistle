package plan

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnrpc"
	"google.golang.org/grpc"

	"github.com/AusDavo/winthistle/internal/policy"
	"github.com/AusDavo/winthistle/internal/prose"
	"github.com/AusDavo/winthistle/internal/reserve"
)

var params = &chaincfg.RegressionNetParams

// ---------------------------------------------------------------------------
// Fixtures. No keys anywhere: every script here is built from a fixed byte
// pattern, which is enough for a verifier that reasons about scripts and
// amounts. CLAUDE.md's rule about xpubs is easiest to keep by having none.
// ---------------------------------------------------------------------------

func filled(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = seed + byte(i)
	}
	return b
}

// p2wsh returns a witness-script-hash address and its script.
func p2wsh(t *testing.T, seed byte) (string, []byte) {
	t.Helper()
	addr, err := btcutil.NewAddressWitnessScriptHash(filled(32, seed), params)
	if err != nil {
		t.Fatalf("building a p2wsh address: %v", err)
	}
	return addr.EncodeAddress(), mustScript(t, addr)
}

func p2wpkh(t *testing.T, seed byte) (string, []byte) {
	t.Helper()
	addr, err := btcutil.NewAddressWitnessPubKeyHash(filled(20, seed), params)
	if err != nil {
		t.Fatalf("building a p2wpkh address: %v", err)
	}
	return addr.EncodeAddress(), mustScript(t, addr)
}

func p2tr(t *testing.T, seed byte) (string, []byte) {
	t.Helper()
	addr, err := btcutil.NewAddressTaproot(filled(32, seed), params)
	if err != nil {
		t.Fatalf("building a p2tr address: %v", err)
	}
	return addr.EncodeAddress(), mustScript(t, addr)
}

func p2pkh(t *testing.T, seed byte) (string, []byte) {
	t.Helper()
	addr, err := btcutil.NewAddressPubKeyHash(filled(20, seed), params)
	if err != nil {
		t.Fatalf("building a p2pkh address: %v", err)
	}
	return addr.EncodeAddress(), mustScript(t, addr)
}

func mustScript(t *testing.T, addr btcutil.Address) []byte {
	t.Helper()
	s, err := txscript.PayToAddrScript(addr)
	if err != nil {
		t.Fatalf("scripting %s: %v", addr, err)
	}
	return s
}

// pubkey is a real secp256k1 point derived from a fixed byte pattern. It has to
// be a real one: the PSBT parser runs every key it reads through
// btcec.ParsePubKey, so a fabricated 33 bytes fails to deserialise. Nothing here
// is a wallet key — the scalar is the test's own fixed pattern, and no xpub of
// any network appears in this file. See CLAUDE.md.
func pubkey(seed byte) []byte {
	priv, _ := btcec.PrivKeyFromBytes(filled(32, seed))
	return priv.PubKey().SerializeCompressed()
}

// multisig2of2 is a plausible witness script for the simulated cold wallet.
func multisig2of2(t *testing.T) []byte {
	t.Helper()
	a, b := pubkey(0x11), pubkey(0x44)
	script, err := txscript.NewScriptBuilder().
		AddOp(txscript.OP_2).AddData(a).AddData(b).
		AddOp(txscript.OP_2).AddOp(txscript.OP_CHECKMULTISIG).Script()
	if err != nil {
		t.Fatalf("building a 2-of-2 witness script: %v", err)
	}
	return script
}

func outpoint(seed byte, vout uint32) *wire.OutPoint {
	var h chainhash.Hash
	copy(h[:], filled(32, seed))
	return wire.NewOutPoint(&h, vout)
}

// txBuilder assembles a PSBT the way Core or Sparrow would hand one back.
type txBuilder struct {
	t         *testing.T
	ins       []*wire.OutPoint
	sequences []uint32
	prevouts  []*wire.TxOut
	witScript [][]byte
	inMeta    []psbt.PInput
	outs      []*wire.TxOut
	outMeta   []psbt.POutput
}

func newTx(t *testing.T) *txBuilder { return &txBuilder{t: t} }

func (b *txBuilder) in(seed byte, vout uint32, value int64, script []byte) *txBuilder {
	return b.inSeq(seed, vout, value, script, wire.MaxTxInSequenceNum)
}

func (b *txBuilder) inSeq(seed byte, vout uint32, value int64, script []byte,
	seq uint32) *txBuilder {

	b.ins = append(b.ins, outpoint(seed, vout))
	b.sequences = append(b.sequences, seq)
	b.prevouts = append(b.prevouts, wire.NewTxOut(value, script))
	b.witScript = append(b.witScript, nil)
	b.inMeta = append(b.inMeta, psbt.PInput{})
	return b
}

// coldIn is an input spending the simulated 2-of-2 cold wallet.
func (b *txBuilder) coldIn(seed byte, value int64) *txBuilder {
	_, script := p2wsh(b.t, seed)
	b.in(seed, 0, value, script)
	b.witScript[len(b.witScript)-1] = multisig2of2(b.t)
	return b
}

func (b *txBuilder) out(value int64, script []byte) *txBuilder {
	b.outs = append(b.outs, wire.NewTxOut(value, script))
	b.outMeta = append(b.outMeta, psbt.POutput{})
	return b
}

func (b *txBuilder) outMetaLast(meta psbt.POutput) *txBuilder {
	b.outMeta[len(b.outMeta)-1] = meta
	return b
}

func (b *txBuilder) packet() *psbt.Packet {
	b.t.Helper()
	p, err := psbt.New(b.ins, b.outs, 2, 0, b.sequences)
	if err != nil {
		b.t.Fatalf("building the psbt: %v", err)
	}
	for i := range p.Inputs {
		p.Inputs[i].WitnessUtxo = b.prevouts[i]
		p.Inputs[i].WitnessScript = b.witScript[i]
		// Key origins only. The UTXO and the witness script are the builder's, and
		// a fixture that could overwrite them would be a fixture that could hide the
		// checks those two fields exist for.
		p.Inputs[i].Bip32Derivation = b.inMeta[i].Bip32Derivation
		p.Inputs[i].TaprootBip32Derivation = b.inMeta[i].TaprootBip32Derivation
	}
	copy(p.Outputs, b.outMeta)
	return p
}

func (b *txBuilder) bytes() []byte {
	b.t.Helper()
	var buf bytes.Buffer
	if err := b.packet().Serialize(&buf); err != nil {
		b.t.Fatalf("serialising the psbt: %v", err)
	}
	return buf.Bytes()
}

// ---------------------------------------------------------------------------
// A plan, and the transaction that satisfies it.
// ---------------------------------------------------------------------------

type fixture struct {
	plan        *Plan
	fundingA    []byte
	fundingB    []byte
	topUp       []byte
	change      []byte
	changeAddr  string
	inputValue  int64
	changeValue int64
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	addrA, scriptA := p2wsh(t, 0x20)
	addrB, scriptB := p2wsh(t, 0x30)
	topUpAddr, topUpScript := p2tr(t, 0x40)
	changeAddr, changeScript := p2wsh(t, 0x50)

	p := &Plan{
		Chain: "regtest",
		Channels: []Channel{
			{Peer: strings.Repeat("aa", 33), PendingChanID: "01", Address: addrA, AmountSat: 1_000_000},
			{Peer: strings.Repeat("bb", 33), PendingChanID: "02", Address: addrB, AmountSat: 2_000_000},
		},
		TopUp:  &TopUp{Address: topUpAddr, AmountSat: 20_000, Shortfall: 20_000},
		Change: Change{Address: changeAddr},
		Inputs: Inputs{MinConfirmations: 1},
	}
	return fixture{
		plan: p, fundingA: scriptA, fundingB: scriptB, topUp: topUpScript,
		change: changeScript, changeAddr: changeAddr,
		inputValue: 5_000_000, changeValue: 1_976_000,
	}
}

// good builds the transaction the plan asks for. Change is what is left after a
// fee of roughly 10 sat/vB on a two-input, four-output 2-of-2 spend.
func (f fixture) good(t *testing.T) *txBuilder {
	t.Helper()
	return newTx(t).
		coldIn(0x60, 3_000_000).
		coldIn(0x70, 2_000_000).
		out(1_000_000, f.fundingA).
		out(2_000_000, f.fundingB).
		out(20_000, f.topUp).
		out(f.changeValue, f.change)
}

func verify(t *testing.T, p *Plan, b *txBuilder) *Verification {
	t.Helper()
	v, err := p.Verify(b.bytes())
	if err != nil {
		t.Fatalf("verifying: %v", err)
	}
	return v
}

func codes(v *Verification) []Code {
	out := make([]Code, 0, len(v.Problems))
	for _, p := range v.Problems {
		out = append(out, p.Code)
	}
	return out
}

func has(v *Verification, want Code) bool {
	for _, c := range codes(v) {
		if c == want {
			return true
		}
	}
	return false
}

// reportCodes is the other list. Since item 6 four findings are established and
// not refused over, and they live in Reports rather than Problems — so a test
// that only ever looked at codes() would read that demotion as a disappearance.
func reportCodes(v *Verification) []Code {
	out := make([]Code, 0, len(v.Reports))
	for _, r := range v.Reports {
		out = append(out, r.Code)
	}
	return out
}

func reported(v *Verification, want Code) bool {
	for _, c := range reportCodes(v) {
		if c == want {
			return true
		}
	}
	return false
}

// found looks in both lists, for a test whose subject is whether the verifier
// noticed something at all rather than what it did about it.
func found(v *Verification, want Code) bool {
	return has(v, want) || reported(v, want)
}

func wantOnly(t *testing.T, v *Verification, want Code) {
	t.Helper()
	if !has(v, want) {
		t.Fatalf("expected %v; got %v\n%s", want, codes(v), v.Report())
	}
	for _, c := range codes(v) {
		if c != want {
			t.Errorf("also found %v, which this case is not about:\n%s", c, v.Report())
		}
	}
}

// ---------------------------------------------------------------------------

func TestThePlannedTransactionPasses(t *testing.T) {
	f := newFixture(t)
	v := verify(t, f.plan, f.good(t))
	if !v.OK() {
		t.Fatalf("the transaction the plan asks for was refused:\n%s", v.Report())
	}
	if v.FeeSat != 5_000_000-1_000_000-2_000_000-20_000-f.changeValue {
		t.Errorf("fee arithmetic is wrong: %d", v.FeeSat)
	}
	if v.FeeRate < 8 || v.FeeRate > 12 {
		t.Errorf("fee rate %.2f is nowhere near the 10 sat/vB the fixture aims at", v.FeeRate)
	}
	if v.UnsignedTxID == "" {
		t.Error("no txid recorded — I-3 has nothing to pin")
	}
	// The verification must not read as a broader guarantee than it is.
	if len(v.Unchecked) == 0 {
		t.Error("a clean verification claims to have checked everything")
	}
}

// TestAnOutputThePlanDoesNotNameIsRefused is the one this package exists for.
//
// LND cannot catch this. PsbtIntent.Verify locates its own output with
// psbt.TxOutsEqual and never asserts that its output is the only one — which is
// precisely what lets n funding streams share one transaction. So a returned
// PSBT that quietly pays itself an extra output satisfies every psbt_verify in
// the batch.
func TestAnOutputThePlanDoesNotNameIsRefused(t *testing.T) {
	f := newFixture(t)
	_, stranger := p2wpkh(t, 0x80)

	b := newTx(t).
		coldIn(0x60, 3_000_000).
		coldIn(0x70, 2_000_000).
		out(1_000_000, f.fundingA).
		out(2_000_000, f.fundingB).
		out(20_000, f.topUp).
		out(500_000, stranger).
		out(f.changeValue-500_000, f.change)

	v := verify(t, f.plan, b)
	wantOnly(t, v, UnnamedOutput)

	// Every named output is still present and correct, which is exactly why
	// nothing else would have noticed.
	for _, want := range [][]byte{f.fundingA, f.fundingB, f.topUp} {
		var found bool
		for _, a := range v.Outputs {
			if bytes.Equal(a.Script, want) && a.Named {
				found = true
			}
		}
		if !found {
			t.Fatalf("the fixture is wrong: a named output is missing")
		}
	}
	if !strings.Contains(v.Report(), "NOT IN THE PLAN") {
		t.Errorf("the report does not point at the offending output:\n%s", v.Report())
	}
}

func TestAFundingAmountOffByOneSatoshiIsRefused(t *testing.T) {
	f := newFixture(t)
	b := newTx(t).
		coldIn(0x60, 3_000_000).
		coldIn(0x70, 2_000_000).
		out(999_999, f.fundingA).
		out(2_000_000, f.fundingB).
		out(20_000, f.topUp).
		out(f.changeValue+1, f.change)

	wantOnly(t, verify(t, f.plan, b), WrongAmount)
}

// TestAFundingOutputPaidTwiceIsRefused. LND sets a found flag and does not
// count, so both channels would verify and the batch would pay twice.
func TestAFundingOutputPaidTwiceIsRefused(t *testing.T) {
	f := newFixture(t)
	b := newTx(t).
		coldIn(0x60, 3_000_000).
		coldIn(0x70, 2_000_000).
		out(1_000_000, f.fundingA).
		out(1_000_000, f.fundingA).
		out(2_000_000, f.fundingB).
		out(20_000, f.topUp).
		out(f.changeValue-1_000_000, f.change)

	v := verify(t, f.plan, b)
	if !has(v, DuplicateOutput) {
		t.Fatalf("a doubled funding output passed: %v\n%s", codes(v), v.Report())
	}
}

func TestAMissingFundingOutputIsRefused(t *testing.T) {
	f := newFixture(t)
	b := newTx(t).
		coldIn(0x60, 3_000_000).
		coldIn(0x70, 2_000_000).
		out(1_000_000, f.fundingA).
		out(20_000, f.topUp).
		out(f.changeValue+2_000_000, f.change)

	wantOnly(t, verify(t, f.plan, b), MissingOutput)
}

func TestAMissingTopUpIsRefused(t *testing.T) {
	f := newFixture(t)
	b := newTx(t).
		coldIn(0x60, 3_000_000).
		coldIn(0x70, 2_000_000).
		out(1_000_000, f.fundingA).
		out(2_000_000, f.fundingB).
		out(f.changeValue+20_000, f.change)

	wantOnly(t, verify(t, f.plan, b), MissingOutput)
}

// TestALegacyInputIsRefusedEvenWithAWitnessUtxo.
//
// This is the second gap. verifyAllInputsSegWit's first case is
//
//	case in.WitnessUtxo != nil:
//
// with no look at the pkScript inside it, so attaching a WitnessUtxo to a P2PKH
// input satisfies LND's malleability check while leaving the input malleable.
// The verifier here reads the script.
func TestALegacyInputIsRefusedEvenWithAWitnessUtxo(t *testing.T) {
	f := newFixture(t)
	_, legacy := p2pkh(t, 0x90)

	b := newTx(t).
		coldIn(0x60, 3_000_000).
		in(0x70, 0, 2_000_000, legacy).
		out(1_000_000, f.fundingA).
		out(2_000_000, f.fundingB).
		out(20_000, f.topUp).
		out(f.changeValue, f.change)

	packet := b.packet()
	if packet.Inputs[1].WitnessUtxo == nil {
		t.Fatal("the fixture does not reproduce LND's condition")
	}
	v := verify(t, f.plan, b)
	if !has(v, LegacyInput) {
		t.Fatalf("a P2PKH input with a WitnessUtxo attached passed: %v\n%s",
			codes(v), v.Report())
	}
}

// TestTheSequenceNumberIsRecordedAndNotJudged.
//
// A refusal stood here until item 6: any input below 0xfffffffe signals BIP-125
// opt-in replaceability, and I-4 says replacing the funding transaction moves
// every outpoint in it and destroys every channel in the batch.
//
// It was a lint wearing an invariant's clothes. Core 29 relays a higher-fee
// conflict whatever the sequence numbers signal — full-RBF is unconditional
// there, verified against a running node — so a transaction that signals nothing
// is no harder to replace than one that does. What holds I-4 is authorship: only
// we can sign our inputs, and no code path in this repository replaces a funding
// transaction.
//
// So this asserts the opposite of what it used to, on the same fixtures: the
// sequence is on InputView, where the report can show it, and nothing refuses
// over it. 0xfffffffd is BIP-125 opt-in; 0xfffffffe is what a wallet uses when
// it wants nLockTime honoured without opting in; 0xffffffff is neither.
func TestTheSequenceNumberIsRecordedAndNotJudged(t *testing.T) {
	for _, seq := range []uint32{0xfffffffd, 0xfffffffe, 0xffffffff} {
		f := newFixture(t)
		b := newTx(t).
			coldIn(0x60, 3_000_000).
			inSeq(0x70, 0, 2_000_000, mustP2WSH(t, 0x70), seq).
			out(1_000_000, f.fundingA).
			out(2_000_000, f.fundingB).
			out(20_000, f.topUp).
			out(f.changeValue, f.change)
		b.witScript[1] = multisig2of2(t)

		v := verify(t, f.plan, b)
		if !v.OK() {
			t.Errorf("sequence %#x was refused: %v\n%s", seq, codes(v), v.Report())
		}
		if len(v.Reports) > 0 {
			t.Errorf("sequence %#x was reported on: %v", seq, reportCodes(v))
		}
		if got := v.Inputs[1].Sequence; got != seq {
			t.Errorf("the report records sequence %#x, not the %#x in the "+
				"transaction", got, seq)
		}
	}
}

func mustP2WSH(t *testing.T, seed byte) []byte {
	t.Helper()
	_, s := p2wsh(t, seed)
	return s
}

// TestNoFeeAtAllIsRefused reproduces LND's own rule: the input sum must exceed
// the output sum, strictly.
func TestNoFeeAtAllIsRefused(t *testing.T) {
	f := newFixture(t)
	b := newTx(t).
		coldIn(0x60, 3_000_000).
		coldIn(0x70, 2_000_000).
		out(1_000_000, f.fundingA).
		out(2_000_000, f.fundingB).
		out(20_000, f.topUp).
		out(1_980_000, f.change)

	if v := verify(t, f.plan, b); !has(v, NoFee) {
		t.Errorf("a zero-fee transaction passed: %v", codes(v))
	}
}

// TestNoChangeOutputIsReportedAndTheBatchStillArms.
//
// Item 6's whole point, and the assertion is on the second half as much as the
// first. A batch with no change output has no CPFP lever and therefore no exit
// but an out-of-band double-spend the operator performs themselves — worth
// saying, and not worth refusing over. Nothing is at risk in it: the coins are
// theirs, unspent, in a transaction only they can sign.
//
// A tool that refused this would be claiming an authority it gave up at step 4,
// where it stopped building the transaction. So OK() stays true and there is no
// further prompt.
func TestNoChangeOutputIsReportedAndTheBatchStillArms(t *testing.T) {
	f := newFixture(t)
	b := newTx(t).
		coldIn(0x60, 3_000_000).
		coldIn(0x70, 24_000).
		out(1_000_000, f.fundingA).
		out(2_000_000, f.fundingB).
		out(20_000, f.topUp)

	v := verify(t, f.plan, b)
	if !reported(v, ChangeMissing) {
		t.Fatalf("a batch with no CPFP lever went unremarked: %v / %v\n%s",
			codes(v), reportCodes(v), v.Report())
	}
	if !v.OK() {
		t.Fatalf("a missing change output refused the batch: %v\n%s",
			codes(v), v.Report())
	}
	if !strings.Contains(v.Report(), "Reported, not refused") {
		t.Errorf("the finding is not under a heading of its own:\n%s", v.Report())
	}
	if strings.Contains(v.Report(), "Do not sign this") {
		t.Errorf("the report tells the operator not to sign over their own change "+
			"arrangements:\n%s", v.Report())
	}
}

func TestAnInputOutsideThePlannedCoinSetIsRefused(t *testing.T) {
	f := newFixture(t)
	f.plan.Inputs.Allowed = []Outpoint{
		{TxID: outpoint(0x60, 0).Hash.String(), Vout: 0},
	}
	v := verify(t, f.plan, f.good(t))
	if !has(v, InputNotAllowed) {
		t.Fatalf("a coin outside the locked set passed: %v\n%s", codes(v), v.Report())
	}
}

func TestTheSameCoinSpentTwiceIsRefused(t *testing.T) {
	f := newFixture(t)
	b := newTx(t).
		coldIn(0x60, 3_000_000).
		coldIn(0x60, 3_000_000).
		out(1_000_000, f.fundingA).
		out(2_000_000, f.fundingB).
		out(20_000, f.topUp).
		out(f.changeValue, f.change)

	if v := verify(t, f.plan, b); !has(v, DuplicateInput) {
		t.Errorf("a doubled input passed: %v", codes(v))
	}
}

// TestANonWitnessUtxoFromTheWrongTransactionIsRefused.
//
// psbt.SumUtxoInputValues — the function LND uses to total the inputs — reads
// the attached previous transaction without checking that it is the one the
// input spends. A wrong one makes the input sum, and therefore the fee, a
// fiction that LND would accept.
func TestANonWitnessUtxoFromTheWrongTransactionIsRefused(t *testing.T) {
	f := newFixture(t)
	b := f.good(t)
	packet := b.packet()

	unrelated := wire.NewMsgTx(2)
	unrelated.AddTxIn(wire.NewTxIn(outpoint(0xf0, 0), nil, nil))
	unrelated.AddTxOut(wire.NewTxOut(9_000_000, mustP2WSH(t, 0x60)))

	packet.Inputs[0].WitnessUtxo = nil
	packet.Inputs[0].NonWitnessUtxo = unrelated

	var buf bytes.Buffer
	if err := packet.Serialize(&buf); err != nil {
		t.Fatalf("serialising: %v", err)
	}
	v, err := f.plan.Verify(buf.Bytes())
	if err != nil {
		t.Fatalf("verifying: %v", err)
	}
	if !has(v, MismatchedUTXO) {
		t.Fatalf("a previous transaction that is not the input's passed: %v\n%s",
			codes(v), v.Report())
	}
}

// TestAnInputWithNoUTXOInformationIsRefusedAndNoFeeIsGuessed.
//
// The PSBT spec does not require either UTXO field, and LND's own input sum —
// psbt.SumUtxoInputValues — errors rather than guessing. Neither should we: a
// confident fee rate computed from a partial input total would sit next to the
// real problem and look like the answer.
func TestAnInputWithNoUTXOInformationIsRefusedAndNoFeeIsGuessed(t *testing.T) {
	f := newFixture(t)
	packet := f.good(t).packet()
	packet.Inputs[1].WitnessUtxo = nil
	packet.Inputs[1].WitnessScript = nil

	var buf bytes.Buffer
	if err := packet.Serialize(&buf); err != nil {
		t.Fatalf("serialising: %v", err)
	}
	v, err := f.plan.Verify(buf.Bytes())
	if err != nil {
		t.Fatalf("verifying: %v", err)
	}
	if !has(v, NoUTXOInfo) {
		t.Fatalf("an input with no UTXO information passed: %v\n%s", codes(v), v.Report())
	}
	if v.FeeRate != 0 || v.FeeSat != 0 {
		t.Errorf("a fee of %d sat at %.2f sat/vB was reported from a partial input "+
			"total", v.FeeSat, v.FeeRate)
	}
	if found(v, NoFee) {
		t.Errorf("a fee finding was made up out of a partial input total: %v / %v",
			codes(v), reportCodes(v))
	}
	var said bool
	for _, u := range v.Unchecked {
		if strings.Contains(u, "fee rate") {
			said = true
		}
	}
	if !said {
		t.Errorf("the report does not say the fee went unchecked: %v", v.Unchecked)
	}
}

// ---------------------------------------------------------------------------
// Assisted mode: change recognised by key origin, because Sparrow picks its own
// change address and the plan cannot name it.
// ---------------------------------------------------------------------------

func changeDerivation(fp uint32, branch, index uint32) psbt.POutput {
	return psbt.POutput{
		Bip32Derivation: []*psbt.Bip32Derivation{{
			PubKey:               pubkey(byte(fp)),
			MasterKeyFingerprint: fp,
			Bip32Path:            []uint32{84 + 0x80000000, 1 + 0x80000000, 0x80000000, branch, index},
		}},
	}
}

func mergeDerivations(outs ...psbt.POutput) psbt.POutput {
	var merged psbt.POutput
	for _, o := range outs {
		merged.Bip32Derivation = append(merged.Bip32Derivation, o.Bip32Derivation...)
	}
	return merged
}

func assistedFixture(t *testing.T) fixture {
	f := newFixture(t)
	f.plan.Change = Change{Recognise: &Recognition{
		Fingerprints: []uint32{0x1b51e4f1, 0x4cf33624},
		Branch:       1,
	}}
	return f
}

func TestChangeIsRecognisedByKeyOrigin(t *testing.T) {
	f := assistedFixture(t)
	b := f.good(t)
	b.outMeta[3] = mergeDerivations(
		changeDerivation(0x1b51e4f1, 1, 7),
		changeDerivation(0x4cf33624, 1, 7),
	)
	v := verify(t, f.plan, b)
	if !v.OK() {
		t.Fatalf("a change output carrying the cold wallet's own derivations was "+
			"refused:\n%s", v.Report())
	}
	var recognised int
	for _, a := range v.Outputs {
		if a.Recognised {
			recognised++
		}
	}
	if recognised != 1 {
		t.Errorf("expected exactly one recognised change output, got %d", recognised)
	}
	if !strings.Contains(v.Report(), "recognised by key origin") {
		t.Errorf("the report does not say the change claim is the weaker one:\n%s", v.Report())
	}
}

func TestAStrangersOutputIsNotMistakenForChange(t *testing.T) {
	f := assistedFixture(t)
	b := f.good(t)
	b.outMeta[3] = changeDerivation(0xdeadbeef, 1, 7)

	v := verify(t, f.plan, b)
	if !has(v, UnnamedOutput) {
		t.Fatalf("an output with a stranger's fingerprint was taken for change: %v\n%s",
			codes(v), v.Report())
	}
	// Demoted in item 6, so it is in the other list. The subject of this test is
	// the fingerprint, not the severity, but a stranger's output that swallowed
	// the change silently would be exactly the failure worth catching.
	if !reported(v, ChangeMissing) {
		t.Errorf("the missing change output was not reported: %v / %v",
			codes(v), reportCodes(v))
	}
}

// TestTheReceiveBranchIsNotChange. An output derived on /0/* is a payment to
// the cold wallet's receive branch, not change, and accepting it would let a
// returned transaction move money between the operator's own branches without
// anyone naming it.
func TestTheReceiveBranchIsNotChange(t *testing.T) {
	f := assistedFixture(t)
	b := f.good(t)
	b.outMeta[3] = mergeDerivations(
		changeDerivation(0x1b51e4f1, 0, 7),
		changeDerivation(0x4cf33624, 0, 7),
	)
	if v := verify(t, f.plan, b); !has(v, UnnamedOutput) {
		t.Errorf("a receive-branch output was taken for change: %v", codes(v))
	}
}

// TestPartialKeyOriginIsNotEnough. A 2-of-2 change output should carry both
// keys; one is what a transaction paying a different wallet that happens to
// share a signer would look like.
func TestPartialKeyOriginIsNotEnough(t *testing.T) {
	f := assistedFixture(t)
	b := f.good(t)
	b.outMeta[3] = changeDerivation(0x1b51e4f1, 1, 7)

	if v := verify(t, f.plan, b); !has(v, UnnamedOutput) {
		t.Errorf("a change output carrying one of two keys passed: %v", codes(v))
	}
}

// ---------------------------------------------------------------------------
// The plan itself
// ---------------------------------------------------------------------------

// TestAPlanWithNoChangeArrangementIsRefused, and it is not the demoted finding.
//
// Item 6 made a transaction's change arrangements the operator's business. This
// is the other thing: a plan that cannot tell change from an output nobody named
// cannot make the attribution check that the whole program is, so it refuses —
// the same family as UnnamedOutput, and --change ADDRESS is the answer.
func TestAPlanWithNoChangeArrangementIsRefused(t *testing.T) {
	f := newFixture(t)
	f.plan.Change = Change{}
	_, err := f.plan.Outputs()
	if err == nil {
		t.Fatal("a plan that cannot identify its change output was accepted")
	}
	if !strings.Contains(err.Error(), "identify the change output") {
		t.Errorf("the refusal reads as a judgement about change rather than about "+
			"attribution: %v", err)
	}
}

func TestAPlanThatNamesOneScriptTwiceIsRefused(t *testing.T) {
	f := newFixture(t)
	f.plan.Channels[1].Address = f.plan.Channels[0].Address
	_, err := f.plan.Outputs()
	if err == nil {
		t.Fatal("a plan naming the same script for two channels was accepted")
	}
	if !strings.Contains(err.Error(), "same script") {
		t.Errorf("unhelpful error: %v", err)
	}
}

func TestAnAddressForTheWrongNetworkIsRefused(t *testing.T) {
	f := newFixture(t)
	mainnet, err := btcutil.NewAddressWitnessScriptHash(filled(32, 0x20), &chaincfg.MainNetParams)
	if err != nil {
		t.Fatalf("building a mainnet address: %v", err)
	}
	f.plan.Channels[0].Address = mainnet.EncodeAddress()
	if _, err := f.plan.Outputs(); err == nil {
		t.Fatal("a mainnet address was accepted into a regtest plan")
	}
}

func TestTheDocumentNamesEveryOutput(t *testing.T) {
	f := newFixture(t)
	doc := f.plan.Document()
	for _, want := range []string{
		f.plan.Channels[0].Address,
		f.plan.Channels[1].Address,
		f.plan.TopUp.Address,
		f.changeAddr,
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("the plan document does not name %s:\n%s", want, doc)
		}
	}
	if !strings.Contains(doc, "Replace-by-fee off") {
		t.Errorf("the plan document does not forbid RBF:\n%s", doc)
	}
}

func TestReportsStayInThePane(t *testing.T) {
	f := newFixture(t)
	_, stranger := p2wpkh(t, 0x80)
	b := f.good(t).out(1, stranger)

	texts := []string{f.plan.Document(), verify(t, f.plan, b).Report(),
		verify(t, f.plan, f.good(t)).Report()}
	for _, text := range texts {
		for i, line := range strings.Split(text, "\n") {
			if n := len([]rune(line)); n > 80 {
				t.Errorf("line %d is %d columns wide:\n%s", i+1, n, line)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Sizing
// ---------------------------------------------------------------------------

// TestVsizeIsAnUpperBound checks the estimator against a shape whose size is
// known: one P2WPKH input, one P2WPKH output. Bitcoin Core's own figure for
// that transaction is 110 vB with a 72-byte signature and 111 with a 73-byte
// one, and the estimator deliberately assumes the larger.
func TestVsizeIsAnUpperBound(t *testing.T) {
	_, script := p2wpkh(t, 0x10)
	b := newTx(t).in(0x60, 0, 100_000, script).out(90_000, script)

	size, err := estimateSize(b.packet(), [][]byte{script})
	if err != nil {
		t.Fatalf("estimating: %v", err)
	}
	if size.Vsize < 110 || size.Vsize > 112 {
		t.Errorf("a 1-in 1-out P2WPKH spend estimated at %d vB; expected 110-112", size.Vsize)
	}
	if !size.Estimated {
		t.Error("an unsigned transaction was reported as exactly sized")
	}
}

func TestAnUnrecognisedInputShapeIsNotGuessedAt(t *testing.T) {
	f := newFixture(t)
	_, legacy := p2pkh(t, 0x90)
	b := newTx(t).
		coldIn(0x60, 3_000_000).
		in(0x70, 0, 2_000_000, legacy).
		out(1_000_000, f.fundingA).
		out(2_000_000, f.fundingB).
		out(20_000, f.topUp).
		out(f.changeValue, f.change)

	v := verify(t, f.plan, b)
	if !has(v, Unsizable) {
		t.Errorf("a legacy input produced a confident fee rate: %v\n%s", codes(v), v.Report())
	}
	if v.FeeRate != 0 {
		t.Errorf("a fee rate of %.2f was reported for a transaction that cannot be sized",
			v.FeeRate)
	}
}

// fakeLND answers the one call the plan makes.
type fakeLND struct {
	addr  string
	calls int
	err   error
}

func (f *fakeLND) NewAddress(context.Context, *lnrpc.NewAddressRequest,
	...grpc.CallOption) (*lnrpc.NewAddressResponse, error) {

	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &lnrpc.NewAddressResponse{Address: f.addr}, nil
}

func TestReserveTopUpAimsAtTheLargerFigure(t *testing.T) {
	cli := &fakeLND{addr: "bcrt1qexample"}

	// Clear: nothing to do, and no address burned asking.
	clear := reserve.Finding{Batch: reserve.Batch{Public: 3}, Available: 1_000_000,
		AtVerify: 30_000, AfterBatch: 50_000}
	top, err := ReserveTopUp(context.Background(), cli, clear)
	if err != nil {
		t.Fatalf("ReserveTopUp: %v", err)
	}
	if top != nil {
		t.Errorf("a node that clears the reserve was given a %d sat top-up", top.AmountSat)
	}
	if cli.calls != 0 {
		t.Errorf("an address was minted for a top-up that is not needed")
	}

	// Short after the batch but not at verify: the batch would pass, and the
	// cheapest moment to fix the node is now.
	short := reserve.Finding{Batch: reserve.Batch{Public: 3}, Available: 40_000,
		AtVerify: 30_000, AfterBatch: 50_000}
	top, err = ReserveTopUp(context.Background(), cli, short)
	if err != nil {
		t.Fatalf("ReserveTopUp: %v", err)
	}
	if top == nil || top.AmountSat != 10_000 {
		t.Fatalf("expected a 10,000 sat top-up, got %+v", top)
	}

	// Refused at verify: the larger of the two figures, not the blocking one.
	refused := reserve.Finding{Batch: reserve.Batch{Public: 3}, Available: 0,
		AtVerify: 30_000, AfterBatch: 50_000}
	top, err = ReserveTopUp(context.Background(), cli, refused)
	if err != nil {
		t.Fatalf("ReserveTopUp: %v", err)
	}
	if top == nil || top.AmountSat != 50_000 {
		t.Fatalf("expected a 50,000 sat top-up, got %+v", top)
	}
	if top.Address != cli.addr {
		t.Errorf("the top-up pays %q, not the address LND minted", top.Address)
	}
}

// TestAnAllPrivateBatchNeedsNoTopUp. enforceNewReservedValue returns early for
// an unannounced channel, so the check LND runs at verify does not run at all.
func TestAnAllPrivateBatchNeedsNoTopUp(t *testing.T) {
	cli := &fakeLND{addr: "bcrt1qexample"}
	private := reserve.Finding{Batch: reserve.Batch{Private: 2}, Available: 0,
		NowRequired: 20_000, AtVerify: 30_000, AfterBatch: 20_000}
	top, err := ReserveTopUp(context.Background(), cli, private)
	if err != nil {
		t.Fatalf("ReserveTopUp: %v", err)
	}
	if top != nil {
		t.Errorf("an all-private batch was given a %d sat top-up, but LND never "+
			"checks the reserve for one", top.AmountSat)
	}
}

func TestParamsRejectsAnUnknownChain(t *testing.T) {
	if _, err := Params("liquid"); err == nil {
		t.Fatal("an unknown chain was accepted")
	}
	for _, name := range []string{"mainnet", "regtest", "signet", "testnet"} {
		if _, err := Params(name); err != nil {
			t.Errorf("Params(%q): %v", name, err)
		}
	}
}

// TestThePolicySitsBesideTheAmount.
//
// The forwarding policy is in the plan document because it is the same
// decision: a channel's capacity and what it charges to route are chosen
// together, or the second one is not chosen at all. LND opens every channel at
// 1000 msat base and 1 ppm, and on a large channel that is close to free
// routing for whoever notices first — so a plan that showed only the amounts
// would have the operator approve half of it.
func TestThePolicySitsBesideTheAmount(t *testing.T) {
	f := newFixture(t)
	chosen := policy.Policy{BaseFeeMsat: 0, FeeRatePPM: 250, TimeLockDelta: 144}
	f.plan.Channels[0].Policy = &chosen

	doc := f.plan.Document()
	if !strings.Contains(doc, chosen.Summary()) {
		t.Errorf("the plan document does not show the policy:\n%s", doc)
	}
	// The channel with no policy is the hazard, so it has to be louder than the
	// one that has one, not quieter.
	if !strings.Contains(doc, "No forwarding policy chosen") {
		t.Errorf("a channel with no policy is not called out:\n%s", doc)
	}
	if !strings.Contains(doc, "close\nto free routing") &&
		!strings.Contains(doc, "close to free routing") {

		t.Errorf("the document does not say what LND's defaults cost:\n%s", doc)
	}

	// And the policy is checked here rather than in Phase 2, where
	// UpdateChannelPolicy reports an invalid parameter inside a *successful*
	// response and the transaction is already public.
	bad := policy.Policy{TimeLockDelta: policy.MinTimeLockDelta - 1}
	f.plan.Channels[0].Policy = &bad
	if _, err := f.plan.Outputs(); err == nil {
		t.Fatal("a plan carrying a CLTV delta LND refuses was accepted")
	}
}

// TestTheReportsFitThePane closes the gap that let a 79-character line ship.
//
// internal/doctor and internal/settle measure their reports against
// prose.PaneWidth. This package did not, and it renders the two operator-facing
// screens that matter most: the plan document an operator approves before the
// cold wallet comes out, and the verification an operator reads when something
// does not match. A line one character over the pane in the second of those was found
// by rendering the screen and reading it, which is a slower way to find it than a
// test.
//
// Runes rather than bytes, deliberately. This copy is full of em dashes, and a
// byte count would report a line as over the pane when it is not — which is worse
// than no test, because it teaches you to widen the pane.
func TestTheReportsFitThePane(t *testing.T) {
	f := newFixture(t)

	// A batch with no change output and a fee well under the plan's, so the
	// "Reported, not refused" heading and both of its longest findings render.
	// The demoted copy is the newest in this package and the least measured.
	reports := newTx(t).
		coldIn(0x60, 3_000_000).
		coldIn(0x70, 24_000).
		out(1_000_000, f.fundingA).
		out(2_000_000, f.fundingB).
		out(20_000, f.topUp)

	for _, tc := range []struct {
		name string
		text string
	}{
		{"the plan document", f.plan.Document()},
		{"a clean verification", verify(t, f.plan, f.good(t)).Report()},
		{"a verification with reports", verify(t, f.plan, reports).Report()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if strings.TrimSpace(tc.text) == "" {
				t.Fatal("the report is empty, so this measures nothing")
			}
			for i, line := range strings.Split(tc.text, "\n") {
				if n := len([]rune(line)); n > prose.PaneWidth {
					t.Errorf("line %d is %d runes, past the %d-column pane:\n%s",
						i+1, n, prose.PaneWidth, line)
				}
			}
		})
	}
}

// TestTheEstimatedSizeNoteFitsWhateverTheSizeIs. The note used to trail the
// vsize on the same line, which made the width depend on how many digits the
// vsize had — so a batch large enough to need six digits would have pushed it
// over even after the text was shortened. It is on its own line now, and this is
// the assertion that keeps it there.
func TestTheEstimatedSizeNoteFitsWhateverTheSizeIs(t *testing.T) {
	for _, vsize := range []int64{1, 236, 99_999, 1_000_000} {
		v := &Verification{
			UnsignedTxID: strings.Repeat("a", 64),
			Size:         Size{Vsize: vsize, Estimated: true},
		}
		for i, line := range strings.Split(v.Report(), "\n") {
			if n := len([]rune(line)); n > prose.PaneWidth {
				t.Errorf("vsize %d: line %d is %d runes, past the %d-column pane:\n%s",
					vsize, i+1, n, prose.PaneWidth, line)
			}
		}
	}
}

// TestChangeIsRecognisedFromTheTransactionsOwnInputs.
//
// The app no longer builds the transaction, so it no longer knows the change
// address, so the plan cannot name it. RecogniseChangeIn is what keeps that from
// making every transaction Sparrow builds an UnnamedOutput refusal: the master
// fingerprints on the inputs are the wallet about to sign, and an output carrying
// those same fingerprints on branch 1 is that wallet paying itself.
//
// The assertion is end to end rather than on the returned struct, because the
// struct is only interesting if it makes a real transaction verify.
func TestChangeIsRecognisedFromTheTransactionsOwnInputs(t *testing.T) {
	const one, two uint32 = 0x1b51e4f1, 0x4cf33624
	// Distinct seeds, so the two derivations are not two records for one key —
	// which mergeDerivations would collapse and matches() would see as one.
	const seedOne, seedTwo byte = 0xf1, 0x24

	f := newFixture(t)
	b := f.good(t)
	// The cold wallet's own key origins, on the input, which is what a wallet
	// writes when it hands over a packet it can sign.
	b.inMeta[0] = psbt.PInput{Bip32Derivation: []*psbt.Bip32Derivation{
		{PubKey: pubkey(seedOne), MasterKeyFingerprint: one,
			Bip32Path: []uint32{84 + 0x80000000, 1 + 0x80000000, 0x80000000, 0, 3}},
		{PubKey: pubkey(seedTwo), MasterKeyFingerprint: two,
			Bip32Path: []uint32{84 + 0x80000000, 1 + 0x80000000, 0x80000000, 0, 3}},
	}}
	b.outMeta[3] = mergeDerivations(
		changeDerivation(one, 1, 7),
		changeDerivation(two, 1, 7),
	)

	raw := b.bytes()
	rec, err := RecogniseChangeIn(raw)
	if err != nil {
		t.Fatalf("reading the recognition out of the packet: %v", err)
	}
	if rec == nil {
		t.Fatal("no recognition was read from a packet whose inputs carry key origins")
	}
	if len(rec.Fingerprints) != 2 {
		t.Fatalf("read %d fingerprints from the inputs, want the wallet's 2: %v",
			len(rec.Fingerprints), rec.Fingerprints)
	}

	// The plan names no change address at all, which is the new shape.
	f.plan.Change = Change{Recognise: rec}
	v, err := f.plan.Verify(raw)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !v.OK() {
		t.Fatalf("a transaction whose change output carries the spending wallet's "+
			"own key origins was refused:\n%s", v.Report())
	}
	if !strings.Contains(v.Report(), "recognised by key origin") {
		t.Errorf("the report does not say this change claim is the weaker one:\n%s",
			v.Report())
	}
}

// TestAPacketWithNoInputKeyOriginsCannotRecogniseChange.
//
// A wallet that writes no key origins cannot be recognised, and the honest answer
// is nil rather than a Recognition that matches nothing — because a Recognition
// with no fingerprints is refused by Outputs, which would report the plan as
// broken instead of the packet as unreadable. The caller's answer is the
// UnnamedOutput refusal and the copy that says to name the address.
func TestAPacketWithNoInputKeyOriginsCannotRecogniseChange(t *testing.T) {
	f := newFixture(t)
	rec, err := RecogniseChangeIn(f.good(t).bytes())
	if err != nil {
		t.Fatalf("reading the recognition: %v", err)
	}
	if rec != nil {
		t.Fatalf("a packet with no input key origins produced %+v", rec)
	}
}
