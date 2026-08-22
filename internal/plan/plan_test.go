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
		Fee:    Fee{TargetSatPerVB: 10},
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

func TestAReplaceableTransactionIsRefused(t *testing.T) {
	f := newFixture(t)
	b := newTx(t).
		coldIn(0x60, 3_000_000).
		inSeq(0x70, 0, 2_000_000, mustP2WSH(t, 0x70), 0xfffffffd).
		out(1_000_000, f.fundingA).
		out(2_000_000, f.fundingB).
		out(20_000, f.topUp).
		out(f.changeValue, f.change)
	b.witScript[1] = multisig2of2(t)

	v := verify(t, f.plan, b)
	if !has(v, Replaceable) {
		t.Fatalf("an RBF-signalling input passed: %v\n%s", codes(v), v.Report())
	}
}

// TestSequenceFFFFFFFEIsNotReplaceable. BIP-125 opt-in is strictly below
// 0xfffffffe, and 0xfffffffe is the value a wallet uses when it wants nLockTime
// honoured without opting in to replacement.
func TestSequenceFFFFFFFEIsNotReplaceable(t *testing.T) {
	f := newFixture(t)
	b := newTx(t).
		coldIn(0x60, 3_000_000).
		inSeq(0x70, 0, 2_000_000, mustP2WSH(t, 0x70), 0xfffffffe).
		out(1_000_000, f.fundingA).
		out(2_000_000, f.fundingB).
		out(20_000, f.topUp).
		out(f.changeValue, f.change)
	b.witScript[1] = multisig2of2(t)

	if v := verify(t, f.plan, b); has(v, Replaceable) {
		t.Errorf("0xfffffffe was read as replaceable:\n%s", v.Report())
	}
}

func mustP2WSH(t *testing.T, seed byte) []byte {
	t.Helper()
	_, s := p2wsh(t, seed)
	return s
}

func TestAFeeRateOutsideToleranceIsRefused(t *testing.T) {
	f := newFixture(t)

	low := newTx(t).
		coldIn(0x60, 3_000_000).
		coldIn(0x70, 2_000_000).
		out(1_000_000, f.fundingA).
		out(2_000_000, f.fundingB).
		out(20_000, f.topUp).
		out(1_979_800, f.change) // ~1 sat/vB
	if v := verify(t, f.plan, low); !has(v, FeeTooLow) {
		t.Errorf("a 1 sat/vB transaction passed a 10 sat/vB plan: %v", codes(v))
	}

	high := newTx(t).
		coldIn(0x60, 3_000_000).
		coldIn(0x70, 2_000_000).
		out(1_000_000, f.fundingA).
		out(2_000_000, f.fundingB).
		out(20_000, f.topUp).
		out(1_900_000, f.change) // ~200 sat/vB
	if v := verify(t, f.plan, high); !has(v, FeeTooHigh) {
		t.Errorf("a wildly overpaying transaction passed: %v", codes(v))
	}
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

func TestNoChangeOutputIsRefused(t *testing.T) {
	f := newFixture(t)
	b := newTx(t).
		coldIn(0x60, 3_000_000).
		coldIn(0x70, 24_000).
		out(1_000_000, f.fundingA).
		out(2_000_000, f.fundingB).
		out(20_000, f.topUp)

	v := verify(t, f.plan, b)
	if !has(v, ChangeMissing) {
		t.Fatalf("a batch with no CPFP lever passed: %v\n%s", codes(v), v.Report())
	}
}

// TestChangeTooSmallForACPFPChildIsRefused. I-4 leaves the change output as the
// only way to accelerate a stuck batch, so change that cannot buy a child is
// change that is not doing its job.
func TestChangeTooSmallForACPFPChildIsRefused(t *testing.T) {
	f := newFixture(t)
	// The fee is left at roughly the plan's 10 sat/vB, so the only thing wrong
	// with this transaction is that its change output is too small to rescue it.
	b := newTx(t).
		coldIn(0x60, 3_000_000).
		coldIn(0x70, 24_360).
		out(1_000_000, f.fundingA).
		out(2_000_000, f.fundingB).
		out(20_000, f.topUp).
		out(600, f.change)
	// Give the change output the witness script, so the child can be sized the
	// way Core and Sparrow would let us size it.
	b.outMeta[3] = psbt.POutput{WitnessScript: multisig2of2(t)}

	v := verify(t, f.plan, b)
	if !has(v, ChangeTooSmall) {
		t.Fatalf("change too small to fund a child passed: %v\n%s", codes(v), v.Report())
	}
	if v.ChangeFloorSat <= 600 {
		t.Errorf("the CPFP floor came out as %d, which cannot be right", v.ChangeFloorSat)
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
	if has(v, NoFee) || has(v, FeeTooLow) || has(v, FeeTooHigh) {
		t.Errorf("a fee finding was made up out of a partial input total: %v", codes(v))
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
	if !has(v, ChangeMissing) {
		t.Errorf("the missing change output was not reported: %v", codes(v))
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

func TestAPlanWithNoChangeArrangementIsRefused(t *testing.T) {
	f := newFixture(t)
	f.plan.Change = Change{}
	if _, err := f.plan.Outputs(); err == nil {
		t.Fatal("a plan with no change output was accepted; I-4 has no lever without one")
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

func TestChangeFloorCoversTheChildAndTheParentDeficit(t *testing.T) {
	// A 200 vB parent paying 200 sat (1 sat/vB), lifted to 10 sat/vB by a
	// 150 vB child: the package needs 3,500 sat, the parent has paid 200, so
	// the child owes 3,300 plus the dust it must leave behind.
	got := ChangeFloor(200, 200, 10, 150)
	if want := int64(3_300 + DustSat); got != want {
		t.Errorf("ChangeFloor = %d, want %d", got, want)
	}
	// A parent that already overpays needs nothing from the child but dust.
	if got := ChangeFloor(200, 100_000, 10, 150); got != DustSat {
		t.Errorf("ChangeFloor on an overpaying parent = %d, want %d", got, DustSat)
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
