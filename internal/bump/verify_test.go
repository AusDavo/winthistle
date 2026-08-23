package bump_test

import (
	"bytes"
	"testing"

	"github.com/AusDavo/winthistle/internal/bump"
	"github.com/AusDavo/winthistle/internal/combine"
	"github.com/AusDavo/winthistle/internal/plan"
	"github.com/AusDavo/winthistle/internal/settle"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// These tests need no harness, no Core and no LND. The verifier's whole job is
// to read bytes and compare them against figures that were read somewhere else,
// so a fixture built out of generated keys exercises the real thing rather than
// a stand-in — including the part that matters most, which is that a signed
// child coming back out of internal/combine still passes.
//
// The parent here is not a real batch. It does not need to be: what the child
// verifier checks about a parent is its size, its fee and which outpoint is its
// change, and all three arrive as numbers from Core and the cold wallet. The
// live arithmetic against a genuinely unconfirmed parent is in
// bump_regtest_test.go.

const (
	// A parent shaped like a real three-channel batch: about seven thousand
	// virtual bytes paying one sat/vB, which is the case internal/settle's own
	// CPFP measurements were taken against.
	parentVsize = 7_007
	parentFee   = 7_007
	changeSat   = 400_000
)

// coldWallet is a simulated 2-of-2 P2WSH cold wallet — the shape the harness
// uses and the shape a real multisig cold wallet takes.
type coldWallet struct {
	keys          []*btcec.PrivateKey
	witnessScript []byte
	pkScript      []byte
	address       string
}

func newColdWallet(t *testing.T) *coldWallet {
	t.Helper()
	w := &coldWallet{}
	for i := 0; i < 2; i++ {
		key, err := btcec.NewPrivateKey()
		if err != nil {
			t.Fatalf("generating key %d: %v", i, err)
		}
		w.keys = append(w.keys, key)
	}
	// Sorted by pubkey, the way sortedmulti derives its key order, so the
	// finalizer's signature ordering is exercised rather than accidentally
	// matching the order the devices answered in.
	if bytes.Compare(w.keys[0].PubKey().SerializeCompressed(),
		w.keys[1].PubKey().SerializeCompressed()) > 0 {
		w.keys[0], w.keys[1] = w.keys[1], w.keys[0]
	}

	builder := txscript.NewScriptBuilder().AddInt64(2)
	for _, k := range w.keys {
		builder.AddData(k.PubKey().SerializeCompressed())
	}
	builder.AddInt64(2).AddOp(txscript.OP_CHECKMULTISIG)
	script, err := builder.Script()
	if err != nil {
		t.Fatalf("building the witness script: %v", err)
	}
	w.witnessScript = script

	addr, err := btcutil.NewAddressWitnessScriptHash(
		chainhash.HashB(script), &chaincfg.RegressionNetParams)
	if err != nil {
		t.Fatalf("deriving the p2wsh address: %v", err)
	}
	w.address = addr.EncodeAddress()
	if w.pkScript, err = txscript.PayToAddrScript(addr); err != nil {
		t.Fatalf("building the p2wsh script: %v", err)
	}
	return w
}

// childSpec is one CPFP child to build, with every knob a test might want to
// turn away from correct.
type childSpec struct {
	spends     wire.OutPoint
	sequence   uint32
	outputSat  int64
	paysTo     []byte
	extraInput bool
	extraOut   bool
}

// fixture is a cold wallet, a parent change outpoint, and the expectation a
// correct child would be checked against.
type fixture struct {
	w      *coldWallet
	change wire.OutPoint
	exp    bump.Expectation
}

func newFixture(t *testing.T, target float64) *fixture {
	t.Helper()
	w := newColdWallet(t)
	change := wire.OutPoint{Hash: chainhash.Hash{0xab, 0xcd, 0xef}, Index: 3}

	return &fixture{
		w:      w,
		change: change,
		exp: bump.Expectation{
			Chain: "regtest",
			Parent: settle.Parent{
				TxID:      change.Hash.String(),
				VsizeVB:   parentVsize,
				FeeSat:    parentFee,
				Change:    plan.Outpoint{TxID: change.Hash.String(), Vout: change.Index},
				ChangeSat: changeSat,
			},
			TargetSatPerVB: target,
			PaysTo:         w.address,
		},
	}
}

// feeFor is what a child of childVsize has to pay to lift the fixture's parent
// to the target — the same expression plan.ChangeFloor is built out of, which is
// the point: the verifier is checking that Core arrived at this number, so the
// test must not arrive at it a different way.
func (f *fixture) feeFor(t *testing.T, childVsize int64) int64 {
	t.Helper()
	return plan.ChildFeeSat(f.exp.Parent.VsizeVB, f.exp.Parent.FeeSat,
		f.exp.TargetSatPerVB, childVsize)
}

// childVsize is what a one-in one-out child of this wallet comes to, by the same
// estimate internal/plan uses.
func (f *fixture) childVsize(t *testing.T) int64 {
	t.Helper()
	n, err := plan.ChildVsize(f.w.pkScript, f.w.witnessScript)
	if err != nil {
		t.Fatalf("sizing the child: %v", err)
	}
	return n
}

// build assembles a child PSBT to spec and returns it with the expectation to
// check it against.
func (f *fixture) build(t *testing.T, s childSpec) ([]byte, bump.Expectation) {
	t.Helper()

	if s.spends == (wire.OutPoint{}) {
		s.spends = f.change
	}
	if s.sequence == 0 {
		s.sequence = plan.MaxNonReplaceableSequence
	}
	if s.paysTo == nil {
		s.paysTo = f.w.pkScript
	}

	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{PreviousOutPoint: s.spends, Sequence: s.sequence})
	if s.extraInput {
		tx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: wire.OutPoint{Hash: chainhash.Hash{0x99}, Index: 1},
			Sequence:         plan.MaxNonReplaceableSequence,
		})
	}
	tx.AddTxOut(&wire.TxOut{Value: s.outputSat, PkScript: s.paysTo})
	if s.extraOut {
		tx.AddTxOut(&wire.TxOut{Value: 1000, PkScript: f.w.pkScript})
	}

	packet, err := psbt.NewFromUnsignedTx(tx)
	if err != nil {
		t.Fatalf("building the child packet: %v", err)
	}
	for i := range packet.Inputs {
		packet.Inputs[i].WitnessUtxo = &wire.TxOut{
			Value: changeSat, PkScript: f.w.pkScript,
		}
		packet.Inputs[i].WitnessScript = f.w.witnessScript
		packet.Inputs[i].SighashType = txscript.SigHashAll
	}

	var buf bytes.Buffer
	if err := packet.Serialize(&buf); err != nil {
		t.Fatalf("serialising the child: %v", err)
	}
	raw := buf.Bytes()

	exp := f.exp
	exp.FeeSat = changeSat - s.outputSat
	if s.extraInput {
		exp.FeeSat = 2*changeSat - s.outputSat
	}
	return raw, exp
}

// correct builds the child a correct build would produce: one in, one out, the
// fee the package arithmetic calls for.
func (f *fixture) correct(t *testing.T) ([]byte, bump.Expectation, int64) {
	t.Helper()
	vsize := f.childVsize(t)
	fee := f.feeFor(t, vsize)
	raw, exp := f.build(t, childSpec{outputSat: changeSat - fee})
	return raw, exp, vsize
}

// TestACorrectChildVerifiesAndStillDoesAfterSigning is the property the whole
// verifier exists for, and it is checked on both sides of the signing round
// because those are two different artifacts.
//
// The second half is the one worth having. It runs internal/combine's real merge
// and finalizer over the child, takes the view those produce, and puts it back
// through the same verifier — so a clean first pass is a prediction about the
// bytes that would be broadcast rather than an opinion about the packet that
// went out.
func TestACorrectChildVerifiesAndStillDoesAfterSigning(t *testing.T) {
	f := newFixture(t, 20)
	raw, exp, vsize := f.correct(t)

	v, err := bump.Verify(raw, exp)
	if err != nil {
		t.Fatalf("verifying the child: %v", err)
	}
	if !v.OK() {
		t.Fatalf("a correct child was refused:\n%s", v.Report("what was built"))
	}
	if v.Signed {
		t.Error("an unsigned child reported itself signed, so its size would have " +
			"been treated as a measurement")
	}
	if v.PackageRate < exp.TargetSatPerVB {
		t.Errorf("the pair pays %.2f sat/vB against a target of %.2f",
			v.PackageRate, exp.TargetSatPerVB)
	}
	t.Logf("built: %d vB child, %d sat, package %.2f sat/vB",
		v.VsizeVB, v.FeeSat, v.PackageRate)

	// Now sign it for real and put what comes back through Recheck.
	final := signChild(t, f, raw)
	view, err := final.View()
	if err != nil {
		t.Fatalf("taking the verifier's view of the signed child: %v", err)
	}
	rv, err := bump.Recheck(view, exp, vsize)
	if err != nil {
		t.Fatalf("rechecking the signed child: %v", err)
	}
	if !rv.OK() {
		t.Fatalf("the signed child was refused:\n%s", rv.Report("what came back"))
	}
	if !rv.Signed {
		t.Error("a finalized child did not report itself signed, so its size was " +
			"still being treated as an upper bound")
	}
	if rv.VsizeVB > vsize {
		t.Errorf("the signed child is %d vB and the estimate was %d. The estimate "+
			"uses upper bounds on every signature, so it must not be exceeded",
			rv.VsizeVB, vsize)
	}
	if rv.FeeSat != v.FeeSat {
		t.Errorf("the fee changed during signing: %d, was %d", rv.FeeSat, v.FeeSat)
	}
	t.Logf("signed: %d vB measured against %d estimated, %.0f sat/vB for the child",
		rv.VsizeVB, vsize, rv.ChildRate)
}

// TestASecondInputIsRefused is the one refusal that is about CPFP itself rather
// than about the cold wallet.
//
// A second, already-confirmed input makes a cheaper child — and it also lets a
// miner take the child without the parent, at which point the fee buys the batch
// nothing at all. add_inputs is off at build time for that reason and this is the
// check that the returned transaction still honours it.
func TestASecondInputIsRefused(t *testing.T) {
	f := newFixture(t, 20)
	vsize := f.childVsize(t)
	raw, exp := f.build(t, childSpec{
		outputSat:  changeSat - f.feeFor(t, vsize),
		extraInput: true,
	})
	mustRefuse(t, raw, exp, bump.WrongInputCount)
}

// A child that does not spend the parent accelerates nothing.
func TestSpendingSomethingOtherThanTheChangeIsRefused(t *testing.T) {
	f := newFixture(t, 20)
	vsize := f.childVsize(t)
	raw, exp := f.build(t, childSpec{
		spends:    wire.OutPoint{Hash: chainhash.Hash{0x77}, Index: 0},
		outputSat: changeSat - f.feeFor(t, vsize),
	})
	mustRefuse(t, raw, exp, bump.WrongInput)
}

// The output is named exactly rather than recognised, because the app asked Core
// for the address. Anything else is the batch's change leaving cold storage.
func TestPayingSomewhereElseIsRefused(t *testing.T) {
	f := newFixture(t, 20)
	vsize := f.childVsize(t)
	_, elsewhere := freshDestination(t)
	raw, exp := f.build(t, childSpec{
		outputSat: changeSat - f.feeFor(t, vsize),
		paysTo:    elsewhere,
	})
	mustRefuse(t, raw, exp, bump.WrongOutput)
}

// A second output is Core having done something other than what it was asked, or
// somebody having edited the transaction.
func TestASecondOutputIsRefused(t *testing.T) {
	f := newFixture(t, 20)
	vsize := f.childVsize(t)
	raw, exp := f.build(t, childSpec{
		outputSat: changeSat - f.feeFor(t, vsize) - 1000,
		extraOut:  true,
	})
	mustRefuse(t, raw, exp, bump.WrongOutputCount)
}

// The child is built non-replaceable, so a replaceable one is a transaction
// somebody changed.
func TestAReplaceableChildIsRefused(t *testing.T) {
	f := newFixture(t, 20)
	vsize := f.childVsize(t)
	raw, exp := f.build(t, childSpec{
		outputSat: changeSat - f.feeFor(t, vsize),
		sequence:  plan.MaxNonReplaceableSequence - 1,
	})
	mustRefuse(t, raw, exp, bump.Replaceable)
}

// The fee is the figure the package arithmetic called for, and the verifier holds
// both numbers, so this is an equality rather than a tolerance.
func TestAFeeThatIsNotTheOneWorkedOutIsRefused(t *testing.T) {
	f := newFixture(t, 20)
	vsize := f.childVsize(t)
	raw, exp := f.build(t, childSpec{outputSat: changeSat - f.feeFor(t, vsize)})
	exp.FeeSat -= 5_000
	mustRefuse(t, raw, exp, bump.WrongFee)
}

// A child that pays a fee but does not lift the pair to the target is the
// failure a bump exists to avoid: another cold-wallet session for nothing.
func TestAChildThatDoesNotLiftThePairIsRefused(t *testing.T) {
	f := newFixture(t, 20)
	// A tenth of what the arithmetic asks for.
	fee := f.feeFor(t, f.childVsize(t)) / 10
	raw, exp := f.build(t, childSpec{outputSat: changeSat - fee})
	mustRefuse(t, raw, exp, bump.NotBumped)
}

// Below the dust floor the output would not relay, so the lift is unaffordable
// however the arithmetic comes out.
func TestAnOutputBelowDustIsRefused(t *testing.T) {
	f := newFixture(t, 20)
	raw, exp := f.build(t, childSpec{outputSat: plan.DustSat - 1})
	mustRefuse(t, raw, exp, bump.DustOutput)
}

// TestALiftAboveTheRelayCeilingIsRefused is the ceiling nothing else in this
// build has met, and it is refused before a device is asked for anything.
//
// A child concentrates the whole package's lift into about 150 virtual bytes, so
// its own fee rate is roughly (parentVsize / childVsize) times the package
// target — a multiplier near fifty on a batch this size. LND hands the child to
// btcwallet, which calls sendrawtransaction with maxfeerate fixed at 0.10
// BTC/kvB and no WalletKit parameter reaching it, so above 10,000 sat/vB the
// node declines to relay a perfectly valid transaction.
//
// The number this test picks is deliberately a package rate an operator could
// reasonably ask for on a busy day, which is the whole reason the check has to
// exist rather than being a theoretical bound.
func TestALiftAboveTheRelayCeilingIsRefused(t *testing.T) {
	const target = 250.0
	f := newFixture(t, target)
	vsize := f.childVsize(t)
	fee := f.feeFor(t, vsize)

	// Sanity: this is meant to be a plausible ask that produces an implausible
	// child rate, not an absurd ask.
	rate := float64(fee) / float64(vsize)
	if rate <= bump.MaxChildFeeRateSatPerVB {
		t.Fatalf("a %g sat/vB package target on a %d vB parent gives the child "+
			"%.0f sat/vB, which is under the %d ceiling — this test is no longer "+
			"testing what it says", target, parentVsize, rate,
			bump.MaxChildFeeRateSatPerVB)
	}
	t.Logf("a %g sat/vB package target needs a %d vB child paying %d sat: "+
		"%.0f sat/vB of its own", target, vsize, fee, rate)

	// The change has to be big enough to pay it, or dust would be the finding.
	f.exp.Parent.ChangeSat = fee + 100_000
	raw, exp := f.build(t, childSpec{outputSat: 100_000})
	exp.Parent.ChangeSat = f.exp.Parent.ChangeSat
	exp.FeeSat = fee

	// The fixture's input value has to match the enlarged change.
	raw = withInputValue(t, raw, f.exp.Parent.ChangeSat, f.w.pkScript)

	mustRefuse(t, raw, exp, bump.AboveRelayCeiling)
}

// ---- helpers ----

// mustRefuse asserts that the child is refused, and that one of the reasons is
// the code the test is about.
func mustRefuse(t *testing.T, raw []byte, exp bump.Expectation, want bump.Code) {
	t.Helper()
	v, err := bump.Verify(raw, exp)
	if err != nil {
		t.Fatalf("verifying: %v", err)
	}
	if v.OK() {
		t.Fatalf("the child was accepted; expected %s", want)
	}
	for _, p := range v.Problems {
		if p.Code == want {
			t.Logf("refused, correctly: %s", p)
			return
		}
	}
	t.Fatalf("refused, but not for %s:\n%s", want, v.Report("what was built"))
}

// withInputValue rewrites every input's witness UTXO value, for the tests that
// need a change output of a different size than the fixture's default.
func withInputValue(t *testing.T, raw []byte, value int64, script []byte) []byte {
	t.Helper()
	packet, err := psbt.NewFromRawBytes(bytes.NewReader(raw), false)
	if err != nil {
		t.Fatalf("reparsing the child: %v", err)
	}
	for i := range packet.Inputs {
		packet.Inputs[i].WitnessUtxo = &wire.TxOut{Value: value, PkScript: script}
	}
	var buf bytes.Buffer
	if err := packet.Serialize(&buf); err != nil {
		t.Fatalf("reserialising the child: %v", err)
	}
	return buf.Bytes()
}

// signChild runs the child through the real signing path: both halves of the
// cold wallet return a partial, and internal/combine merges and finalizes.
func signChild(t *testing.T, f *fixture, raw []byte) *combine.Finalized {
	t.Helper()

	parts := make([]combine.Part, 0, len(f.w.keys))
	for i, key := range f.w.keys {
		packet, err := psbt.NewFromRawBytes(bytes.NewReader(raw), false)
		if err != nil {
			t.Fatalf("reparsing the child for signer %d: %v", i, err)
		}
		fetcher := txscript.NewCannedPrevOutputFetcher(f.w.pkScript, changeSat)
		hashes := txscript.NewTxSigHashes(packet.UnsignedTx, fetcher)
		sig, err := txscript.RawTxInWitnessSignature(packet.UnsignedTx, hashes, 0,
			changeSat, f.w.witnessScript, txscript.SigHashAll, key)
		if err != nil {
			t.Fatalf("signing the child with key %d: %v", i, err)
		}
		packet.Inputs[0].PartialSigs = []*psbt.PartialSig{{
			PubKey: key.PubKey().SerializeCompressed(), Signature: sig,
		}}
		var buf bytes.Buffer
		if err := packet.Serialize(&buf); err != nil {
			t.Fatalf("serialising signer %d's answer: %v", i, err)
		}
		parts = append(parts, combine.Part{
			Label: [...]string{"cold1", "cold2"}[i], PSBT: buf.Bytes(),
		})
	}

	merged, err := combine.Merge(raw, parts)
	if err != nil {
		t.Fatalf("merging the child's partials: %v", err)
	}
	final, err := combine.Finalize(merged)
	if err != nil {
		t.Fatalf("finalizing the child: %v", err)
	}
	return final
}

func freshDestination(t *testing.T) (string, []byte) {
	t.Helper()
	key, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatalf("generating a destination key: %v", err)
	}
	addr, err := btcutil.NewAddressWitnessPubKeyHash(
		btcutil.Hash160(key.PubKey().SerializeCompressed()),
		&chaincfg.RegressionNetParams)
	if err != nil {
		t.Fatalf("deriving a destination address: %v", err)
	}
	script, err := txscript.PayToAddrScript(addr)
	if err != nil {
		t.Fatalf("building a destination script: %v", err)
	}
	return addr.EncodeAddress(), script
}
