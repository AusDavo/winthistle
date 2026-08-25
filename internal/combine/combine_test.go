package combine_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/AusDavo/winthistle/internal/combine"
	"github.com/AusDavo/winthistle/internal/plan"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// These tests need no harness and no Core, which is the point: the combining and
// finalizing this package does is exactly what the design left to Core's
// finalizepsbt, and it is now testable with nothing but keys.
//
// The fixture is a real m-of-n P2WSH spend with real signatures. A mocked one
// would prove that the merge moves bytes around; this one proves the witness it
// assembles actually satisfies the script, which is the claim that matters.

const (
	fundedSat = 1_000_000
	// The batch shape: two "funding" outputs and a change output, so the plan
	// re-verification has something to attribute.
	channelSat = 400_000
	changeSat  = 197_880
	feeSat     = fundedSat - 2*channelSat - changeSat
)

// wallet is a simulated m-of-n cold wallet: n keys, of which m sign.
type wallet struct {
	t        *testing.T
	keys     []*btcec.PrivateKey
	pubKeys  [][]byte
	required int

	witnessScript []byte
	pkScript      []byte
}

// newWallet builds an m-of-n bare multisig, keys sorted by public key the way
// sortedmulti does.
func newWallet(t *testing.T, required, total int) *wallet {
	t.Helper()
	w := &wallet{t: t, required: required}
	for i := 0; i < total; i++ {
		key, err := btcec.NewPrivateKey()
		if err != nil {
			t.Fatalf("generating key %d: %v", i, err)
		}
		w.keys = append(w.keys, key)
	}
	// Sorted, so the witness script's key order is deterministic and the
	// signature ordering the finalizer derives from the script is exercised
	// rather than accidentally matching the order the devices came back in.
	for i := 0; i < len(w.keys); i++ {
		for j := i + 1; j < len(w.keys); j++ {
			a := w.keys[i].PubKey().SerializeCompressed()
			b := w.keys[j].PubKey().SerializeCompressed()
			if bytes.Compare(a, b) > 0 {
				w.keys[i], w.keys[j] = w.keys[j], w.keys[i]
			}
		}
	}
	builder := txscript.NewScriptBuilder().AddInt64(int64(required))
	for _, key := range w.keys {
		pub := key.PubKey().SerializeCompressed()
		w.pubKeys = append(w.pubKeys, pub)
		builder.AddData(pub)
	}
	builder.AddInt64(int64(total)).AddOp(txscript.OP_CHECKMULTISIG)

	script, err := builder.Script()
	if err != nil {
		t.Fatalf("building the witness script: %v", err)
	}
	w.witnessScript = script

	addr, err := btcutil.NewAddressWitnessScriptHash(hash256(script), &chaincfg.RegressionNetParams)
	if err != nil {
		t.Fatalf("deriving the p2wsh address: %v", err)
	}
	if w.pkScript, err = txscript.PayToAddrScript(addr); err != nil {
		t.Fatalf("building the p2wsh script: %v", err)
	}
	return w
}

func hash256(b []byte) []byte {
	sum := chainhash.HashB(b) // sha256, once — which is what p2wsh commits to
	return sum
}

// batch is one unsigned batch transaction and the plan that names its outputs.
type batch struct {
	w      *wallet
	base   []byte
	packet *psbt.Packet
	plan   *plan.Plan
}

// newBatch builds a transaction shaped like a real batch: n funding outputs, a
// change output, one P2WSH input from the cold wallet.
func newBatch(t *testing.T, w *wallet) *batch {
	t.Helper()

	prev := wire.OutPoint{Hash: chainhash.Hash{0x11, 0x22, 0x33}, Index: 0}
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: prev,
		// I-4: not replaceable. 0xffffffff, which is what a wallet that does not
		// want nLockTime honoured uses.
		Sequence: wire.MaxTxInSequenceNum,
	})

	p := &plan.Plan{
		Chain: "regtest",
		// Ten sat/vB, which is what changeSat above was chosen to leave. The
		// tolerance is wide because the witness script's size — and therefore the
		// rate — differs between a 2-of-2 and a 2-of-3 fixture.
		Fee: plan.Fee{TargetSatPerVB: 10, Tolerance: 0.5},
		// The floor is left at zero and the CPFP arithmetic does the work, the
		// same way it does on a real batch.
		Inputs: plan.Inputs{MinConfirmations: 1},
	}

	for i := 0; i < 2; i++ {
		addr, script := freshP2WPKH(t)
		tx.AddTxOut(&wire.TxOut{Value: channelSat, PkScript: script})
		p.Channels = append(p.Channels, plan.Channel{
			Peer:      strings.Repeat("0", 63) + string(rune('1'+i)),
			Address:   addr,
			AmountSat: channelSat,
		})
	}

	// Change back to the cold wallet itself, which is what a real batch does and
	// what makes the CPFP sizing realistic.
	changeAddr, err := btcutil.NewAddressWitnessScriptHash(hash256(w.witnessScript),
		&chaincfg.RegressionNetParams)
	if err != nil {
		t.Fatalf("change address: %v", err)
	}
	changeScript, err := txscript.PayToAddrScript(changeAddr)
	if err != nil {
		t.Fatalf("change script: %v", err)
	}
	tx.AddTxOut(&wire.TxOut{Value: changeSat, PkScript: changeScript})
	p.Change = plan.Change{Address: changeAddr.EncodeAddress()}

	packet, err := psbt.NewFromUnsignedTx(tx)
	if err != nil {
		t.Fatalf("building the base packet: %v", err)
	}
	packet.Inputs[0].WitnessUtxo = &wire.TxOut{Value: fundedSat, PkScript: w.pkScript}
	packet.Inputs[0].WitnessScript = w.witnessScript
	packet.Inputs[0].SighashType = txscript.SigHashAll

	return &batch{w: w, base: serialize(t, packet), packet: packet, plan: p}
}

// freshP2WPKH mints a throwaway destination.
func freshP2WPKH(t *testing.T) (address string, pkScript []byte) {
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

func serialize(t *testing.T, p *psbt.Packet) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := p.Serialize(&buf); err != nil {
		t.Fatalf("serialising a packet: %v", err)
	}
	return buf.Bytes()
}

func parse(t *testing.T, raw []byte) *psbt.Packet {
	t.Helper()
	p, err := psbt.NewFromRawBytes(bytes.NewReader(raw), false)
	if err != nil {
		t.Fatalf("parsing a packet: %v", err)
	}
	return p
}

// sign is one device: it returns the base packet with its own partial signature
// attached, and nothing else changed. This is what walletprocesspsbt with
// finalize=false does, and what a hardware signer does.
func (b *batch) sign(t *testing.T, keyIndex int) combine.Part {
	t.Helper()
	packet := parse(t, b.base)

	fetcher := txscript.NewCannedPrevOutputFetcher(b.w.pkScript, fundedSat)
	sigHashes := txscript.NewTxSigHashes(packet.UnsignedTx, fetcher)
	sig, err := txscript.RawTxInWitnessSignature(packet.UnsignedTx, sigHashes, 0,
		fundedSat, b.w.witnessScript, txscript.SigHashAll, b.w.keys[keyIndex])
	if err != nil {
		t.Fatalf("signing with key %d: %v", keyIndex, err)
	}
	packet.Inputs[0].PartialSigs = []*psbt.PartialSig{{
		PubKey: b.w.keys[keyIndex].PubKey().SerializeCompressed(), Signature: sig,
	}}
	return combine.Part{Label: labelFor(keyIndex), PSBT: serialize(t, packet)}
}

func labelFor(i int) string { return [...]string{"cold1", "cold2", "cold3"}[i] }

// TestNeitherSignerAloneCompletesAndTheMergeDoes is the property the whole
// package exists for, and it is I-2 stated as a test.
//
// A single signer's returned packet must not be finalizable, because a signer
// that could finalize could publish — and then I-1's gate could be defeated from
// outside, by a device or a wallet, before any channel was recoverable.
func TestNeitherSignerAloneCompletesAndTheMergeDoes(t *testing.T) {
	w := newWallet(t, 2, 2)
	b := newBatch(t, w)

	first, second := b.sign(t, 0), b.sign(t, 1)

	for _, alone := range []combine.Part{first, second} {
		merged, err := combine.Merge(b.base, []combine.Part{alone})
		if err != nil {
			t.Fatalf("%s alone did not even merge: %v", alone.Label, err)
		}
		if _, err := combine.Finalize(merged); !errors.Is(err, combine.ErrNotEnoughSignatures) {
			t.Fatalf("%s alone finalized (or failed for the wrong reason): %v",
				alone.Label, err)
		}
	}

	final, v, err := combine.Complete(b.plan, b.base, []combine.Part{first, second})
	if err != nil {
		t.Fatalf("the two partials together did not complete: %v", err)
	}
	if !v.OK() {
		t.Fatalf("the plan refused the transaction the merge produced:\n%s", v.Report())
	}
	if got, want := final.TxID, b.packet.UnsignedTx.TxHash().String(); got != want {
		t.Errorf("txid moved: %s, want %s", got, want)
	}
	if final.FeeSat != feeSat {
		t.Errorf("fee is %d sat, want %d", final.FeeSat, feeSat)
	}
	if len(final.Signers) != 2 {
		t.Errorf("Signers = %v, want both devices", final.Signers)
	}
	if final.Vsize != v.Size.Vsize {
		t.Errorf("Recheck should have caught this: %d vB vs %d", final.Vsize, v.Size.Vsize)
	}
	if v.Size.Estimated {
		t.Error("the re-verification treated a fully-signed transaction as an estimate, " +
			"so its fee rate is an upper bound rather than the real one")
	}
}

// TestTheOrderTheDevicesComeBackInDoesNotMatter. The witness order is taken from
// the script, not from the order the partials arrived in, and the merged packet
// is sorted so the bytes are reproducible either way.
func TestTheOrderTheDevicesComeBackInDoesNotMatter(t *testing.T) {
	w := newWallet(t, 2, 2)
	b := newBatch(t, w)
	first, second := b.sign(t, 0), b.sign(t, 1)

	forward, _, err := combine.Complete(b.plan, b.base, []combine.Part{first, second})
	if err != nil {
		t.Fatalf("forward: %v", err)
	}
	backward, _, err := combine.Complete(b.plan, b.base, []combine.Part{second, first})
	if err != nil {
		t.Fatalf("reversed: %v", err)
	}
	if !bytes.Equal(forward.RawTx, backward.RawTx) {
		t.Error("the transaction depends on the order the devices came back in, so " +
			"the journal would record different bytes for the same signatures")
	}
}

// TestMergeLeavesTheBaseAlone. A refused merge must not have consumed the
// packet: the operator's next move is to re-send the same base to the same
// devices.
func TestMergeLeavesTheBaseAlone(t *testing.T) {
	w := newWallet(t, 2, 2)
	b := newBatch(t, w)
	before := append([]byte(nil), b.base...)

	merged, err := combine.Merge(b.base, []combine.Part{b.sign(t, 0), b.sign(t, 1)})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if _, err := combine.Finalize(merged); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if !bytes.Equal(before, b.base) {
		t.Error("the base packet was mutated")
	}
	if isFinal(merged.Packet) {
		t.Error("Finalize mutated the merged packet it was given, so a caller could " +
			"not merge once and finalize twice")
	}
}

func isFinal(p *psbt.Packet) bool {
	for i := range p.Inputs {
		if p.Inputs[i].FinalScriptSig != nil || p.Inputs[i].FinalScriptWitness != nil {
			return true
		}
	}
	return false
}

// TestADeviceThatChangedTheTransactionIsNamed is I-3 at the merge, and the
// "naming" half of it is the part that matters operationally: with m devices in
// a room, a refusal that does not say which one is a hunt.
func TestADeviceThatChangedTheTransactionIsNamed(t *testing.T) {
	w := newWallet(t, 2, 2)
	b := newBatch(t, w)

	good := b.sign(t, 0)

	// cold2 re-derives change, one satoshi lower. LND compares its own funding
	// output with psbt.TxOutsEqual and would still find it, so this is not a
	// change LND's psbt_verify would object to on the way in.
	tampered := parse(t, b.sign(t, 1).PSBT)
	tampered.UnsignedTx.TxOut[2].Value--
	bad := combine.Part{Label: "cold2", PSBT: serialize(t, tampered)}

	_, err := combine.Merge(b.base, []combine.Part{good, bad})
	if !errors.Is(err, combine.ErrDifferentTransaction) {
		t.Fatalf("a changed transaction was accepted, or refused for another reason: %v", err)
	}
	var de *combine.DeviceError
	if !errors.As(err, &de) || de.Label != "cold2" {
		t.Fatalf("the refusal does not name cold2: %v", err)
	}
}

// TestASignerThatMadeTheTransactionReplaceableIsRefused is the case LND would
// not catch.
//
// PsbtIntent.FinalizeRawTX compares the outputs and the input previous outpoints
// and nothing else — "the fields in the PSBT part are allowed to change" —
// and a sequence number is neither. Then CompileFundingTx takes the channel
// point from the transaction it was handed. So a returned transaction that is
// BIP-125 replaceable would be adopted by LND, and I-4 says replacing this
// transaction destroys every channel in the batch.
func TestASignerThatMadeTheTransactionReplaceableIsRefused(t *testing.T) {
	w := newWallet(t, 2, 2)
	b := newBatch(t, w)

	good := b.sign(t, 0)
	tampered := parse(t, b.sign(t, 1).PSBT)
	tampered.UnsignedTx.TxIn[0].Sequence = wire.MaxTxInSequenceNum - 2 // 0xfffffffd
	bad := combine.Part{Label: "cold2", PSBT: serialize(t, tampered)}

	_, err := combine.Merge(b.base, []combine.Part{good, bad})
	if !errors.Is(err, combine.ErrDifferentTransaction) {
		t.Fatalf("a replaceable transaction was accepted, or refused for another "+
			"reason: %v", err)
	}

	// And the second line of defence, in case a future merge ever stopped
	// comparing the whole transaction: the plan refuses the sequence itself.
	replaceable := parse(t, b.base)
	replaceable.UnsignedTx.TxIn[0].Sequence = wire.MaxTxInSequenceNum - 2
	rebased := serialize(t, replaceable)
	v, err := b.plan.Verify(rebased)
	if err != nil {
		t.Fatalf("verifying: %v", err)
	}
	var sawIt bool
	for _, p := range v.Problems {
		if p.Code == plan.Replaceable {
			sawIt = true
		}
	}
	if !sawIt {
		t.Errorf("the plan did not object to a replaceable input: %s", v.Summary())
	}
}

// TestTwoSignaturesForOneKeyAreRefusedRatherThanChosenBetween.
//
// Either signature would work. That is the reason to refuse: two signatures from
// one key means one device signed twice or two devices claim the same key, and
// neither is a thing to resolve silently while assembling a transaction n
// channels are about to depend on.
func TestTwoSignaturesForOneKeyAreRefusedRatherThanChosenBetween(t *testing.T) {
	w := newWallet(t, 2, 2)
	b := newBatch(t, w)

	first := b.sign(t, 0)
	twin := parse(t, first.PSBT)
	sig := twin.Inputs[0].PartialSigs[0].Signature
	altered := append([]byte(nil), sig...)
	altered[10] ^= 0xff // still the right length and the right sighash byte
	twin.Inputs[0].PartialSigs[0].Signature = altered

	_, err := combine.Merge(b.base, []combine.Part{
		first,
		{Label: "cold1-again", PSBT: serialize(t, twin)},
	})
	if !errors.Is(err, combine.ErrConflictingSignature) {
		t.Fatalf("two signatures for one key were resolved rather than refused: %v", err)
	}
}

// TestAnIdenticalRepeatIsNotAConflict. A device asked twice, or a packet
// uploaded twice, is an ordinary thing and must not look like an attack.
func TestAnIdenticalRepeatIsNotAConflict(t *testing.T) {
	w := newWallet(t, 2, 2)
	b := newBatch(t, w)

	first := b.sign(t, 0)
	again := combine.Part{Label: "cold1-again", PSBT: first.PSBT}

	merged, err := combine.Merge(b.base, []combine.Part{first, again, b.sign(t, 1)})
	if err != nil {
		t.Fatalf("an identical repeat was refused: %v", err)
	}
	if merged.Added["cold1-again"] != 0 {
		t.Errorf("the repeat was counted as a contribution: %v", merged.Added)
	}
	if _, err := combine.Finalize(merged); err != nil {
		t.Fatalf("finalize: %v", err)
	}
}

// TestADeviceInAMultiDeviceRoundMayNotFinalize.
//
// This used to be I-2 as a rail: a finalized input carries a complete witness, so
// the device that produced it held a broadcastable transaction, and the app had
// to be the only party ever in that position. I-2 is dissolved — see Accept and
// the package comment — and the refusal survives it for a mechanical reason.
// Finalization discards the partial signatures, so a device that finalizes on its
// own has ended a round the other devices were still in, and there is nothing
// left for theirs to be unioned with.
//
// The same packet through Accept is the happy path, and the test below it is that.
func TestADeviceInAMultiDeviceRoundMayNotFinalize(t *testing.T) {
	w := newWallet(t, 2, 2)
	b := newBatch(t, w)

	// A device that combined and finalized on its own, which is exactly what
	// happens if the operator lets a wallet do the last step.
	merged, err := combine.Merge(b.base, []combine.Part{b.sign(t, 0), b.sign(t, 1)})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if err := psbt.MaybeFinalizeAll(merged.Packet); err != nil {
		t.Fatalf("finalizing the fixture: %v", err)
	}

	_, err = combine.Merge(b.base, []combine.Part{
		{Label: "sparrow", PSBT: serialize(t, merged.Packet)},
	})
	if !errors.Is(err, combine.ErrAlreadyFinalized) {
		t.Fatalf("a finalized packet was accepted, or refused for another reason: %v", err)
	}
	var de *combine.DeviceError
	if !errors.As(err, &de) || de.Label != "sparrow" || de.Where != "input 0" {
		t.Fatalf("the refusal does not name the device and the input: %v", err)
	}
}

// TestADeviceCannotRewriteWhatAnInputSpends.
//
// LND reads the input amount with psbt.SumUtxoInputValues and never checks that
// the attached UTXO belongs to the input, so this field is the fee. A device that
// could rewrite it could make the fee a fiction LND would accept.
func TestADeviceCannotRewriteWhatAnInputSpends(t *testing.T) {
	w := newWallet(t, 2, 2)
	b := newBatch(t, w)

	tampered := parse(t, b.sign(t, 1).PSBT)
	tampered.Inputs[0].WitnessUtxo.Value = fundedSat * 10

	_, err := combine.Merge(b.base, []combine.Part{
		b.sign(t, 0),
		{Label: "cold2", PSBT: serialize(t, tampered)},
	})
	if !errors.Is(err, combine.ErrConflictingField) {
		t.Fatalf("a rewritten input amount was accepted: %v", err)
	}
}

// TestALaterPacketCannotOverwriteAnEarliersWitnessScript.
func TestALaterPacketCannotOverwriteAnEarliersWitnessScript(t *testing.T) {
	w := newWallet(t, 2, 2)
	other := newWallet(t, 2, 2)
	b := newBatch(t, w)

	tampered := parse(t, b.sign(t, 1).PSBT)
	tampered.Inputs[0].WitnessScript = other.witnessScript

	_, err := combine.Merge(b.base, []combine.Part{
		b.sign(t, 0),
		{Label: "cold2", PSBT: serialize(t, tampered)},
	})
	if !errors.Is(err, combine.ErrConflictingField) {
		t.Fatalf("a rewritten witness script was accepted: %v", err)
	}
}

// TestOverSigningIsRefusedWithNumbersRatherThanBtcdsWording.
//
// btcd's checkIsMultiSigScript insists the number of signatures equal the number
// the script demands, so a 2-of-3 carrying three partials fails — with
// "Unsupported script type", which tells an operator nothing. This is the same
// refusal with the counts in it.
func TestOverSigningIsRefusedWithNumbersRatherThanBtcdsWording(t *testing.T) {
	w := newWallet(t, 2, 3)
	b := newBatch(t, w)

	// Two is right.
	if _, _, err := combine.Complete(b.plan, b.base,
		[]combine.Part{b.sign(t, 0), b.sign(t, 1)}); err != nil {
		t.Fatalf("2 of 3 did not complete: %v", err)
	}

	// Three is not, and the message has to say so.
	merged, err := combine.Merge(b.base,
		[]combine.Part{b.sign(t, 0), b.sign(t, 1), b.sign(t, 2)})
	if err != nil {
		t.Fatalf("merging three partials: %v", err)
	}
	_, err = combine.Finalize(merged)
	if !errors.Is(err, combine.ErrTooManySignatures) {
		t.Fatalf("three partials on a 2-of-3 were accepted, or refused for another "+
			"reason: %v", err)
	}
	for _, want := range []string{"3 signatures", "takes 2", "cold1", "cold3"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

// TestAWitnessThatDoesNotSatisfyItsScriptIsCaughtHere.
//
// LND will not catch this. verifyInputsSigned only asserts that each input has
// something attached, and psbt_verify never sees a witness at all — so in
// assisted mode, where there is no Core to run testmempoolaccept, executing the
// script locally is the only check between a bad signature and n peers
// committing to the transaction.
func TestAWitnessThatDoesNotSatisfyItsScriptIsCaughtHere(t *testing.T) {
	w := newWallet(t, 2, 2)
	b := newBatch(t, w)

	broken := parse(t, b.sign(t, 1).PSBT)
	sig := broken.Inputs[0].PartialSigs[0].Signature
	// Corrupt the signature body while leaving its length and its trailing
	// sighash byte intact, so nothing before the script engine objects.
	sig[len(sig)/2] ^= 0x01

	merged, err := combine.Merge(b.base, []combine.Part{
		b.sign(t, 0),
		{Label: "cold2", PSBT: serialize(t, broken)},
	})
	if err != nil {
		t.Fatalf("merging a badly-signed packet should be fine — the signature is "+
			"not checked at the merge: %v", err)
	}
	if _, err := combine.Finalize(merged); !errors.Is(err, combine.ErrWitnessInvalid) {
		t.Fatalf("a witness that does not satisfy its script was accepted, or "+
			"refused for another reason: %v", err)
	}
}

// TestSingleSigCompletesToo. Single-sig funding is in scope — it is the case
// where I-2 degrades to needing an air-gapped signer — so the finalizer has to
// handle a P2WPKH input as well as a multisig one.
func TestSingleSigCompletesToo(t *testing.T) {
	key, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	pub := key.PubKey().SerializeCompressed()
	addr, err := btcutil.NewAddressWitnessPubKeyHash(btcutil.Hash160(pub),
		&chaincfg.RegressionNetParams)
	if err != nil {
		t.Fatalf("address: %v", err)
	}
	pkScript, err := txscript.PayToAddrScript(addr)
	if err != nil {
		t.Fatalf("script: %v", err)
	}

	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Hash: chainhash.Hash{0x44}, Index: 1},
		Sequence:         wire.MaxTxInSequenceNum,
	})
	destAddr, destScript := freshP2WPKH(t)
	tx.AddTxOut(&wire.TxOut{Value: channelSat, PkScript: destScript})
	tx.AddTxOut(&wire.TxOut{Value: fundedSat - channelSat - 500, PkScript: pkScript})

	packet, err := psbt.NewFromUnsignedTx(tx)
	if err != nil {
		t.Fatalf("packet: %v", err)
	}
	packet.Inputs[0].WitnessUtxo = &wire.TxOut{Value: fundedSat, PkScript: pkScript}
	base := serialize(t, packet)

	signed := parse(t, base)
	fetcher := txscript.NewCannedPrevOutputFetcher(pkScript, fundedSat)
	sigHashes := txscript.NewTxSigHashes(signed.UnsignedTx, fetcher)
	sig, err := txscript.RawTxInWitnessSignature(signed.UnsignedTx, sigHashes, 0,
		fundedSat, pkScript, txscript.SigHashAll, key)
	if err != nil {
		t.Fatalf("signing: %v", err)
	}
	signed.Inputs[0].PartialSigs = []*psbt.PartialSig{{PubKey: pub, Signature: sig}}

	p := &plan.Plan{
		Chain: "regtest",
		Channels: []plan.Channel{{
			Peer: strings.Repeat("0", 64), Address: destAddr, AmountSat: channelSat,
		}},
		Change: plan.Change{Address: addr.EncodeAddress()},
		Fee:    plan.Fee{TargetSatPerVB: 3, Tolerance: 0.9},
	}

	final, v, err := combine.Complete(p, base, []combine.Part{
		{Label: "the air-gapped one", PSBT: serialize(t, signed)},
	})
	if err != nil {
		t.Fatalf("a single-sig spend did not complete: %v\n%s", err, report(v))
	}
	if final.TxID != packet.UnsignedTx.TxHash().String() {
		t.Error("txid moved")
	}
}

// TestTheVerifierStillGetsTheLastWord: a transaction that completes cleanly but
// pays an output the plan does not name must not get past this package.
func TestTheVerifierStillGetsTheLastWord(t *testing.T) {
	w := newWallet(t, 2, 2)
	b := newBatch(t, w)

	// The plan forgets the second channel — which stands in for a builder that
	// paid an output nobody named, since the verifier's view of the two is the
	// same.
	forgetful := *b.plan
	forgetful.Channels = b.plan.Channels[:1]

	_, v, err := combine.Complete(&forgetful, b.base,
		[]combine.Part{b.sign(t, 0), b.sign(t, 1)})
	if !errors.Is(err, combine.ErrPlanBroken) {
		t.Fatalf("an unnamed output got through: %v", err)
	}
	var sawIt bool
	for _, p := range v.Problems {
		if p.Code == plan.UnnamedOutput {
			sawIt = true
		}
	}
	if !sawIt {
		t.Errorf("the objection was not the unnamed output: %s", v.Summary())
	}
}

// TestBtcdRefusesAMalformedPartialSignatureBeforeTheMergeSeesIt.
//
// The merge checks partial signatures for a length it can rely on, because
// btcd's finalizer reads sig[len(sig)-1] with no length check of its own. This
// test records why that check has never fired: btcd's *deserializer* runs
// PartialSig.checkValid — ParsePubKey plus ParseDERSignature — on every record it
// reads, so a malformed partial signature does not survive NewFromRawBytes. If
// that ever stops being true, the guard in the merge is what stands between a
// hostile packet and a panic in the armed window, and this test is where the
// change will show up.
func TestBtcdRefusesAMalformedPartialSignatureBeforeTheMergeSeesIt(t *testing.T) {
	w := newWallet(t, 2, 2)
	b := newBatch(t, w)

	signed := parse(t, b.sign(t, 1).PSBT)
	sig := signed.Inputs[0].PartialSigs[0].Signature
	// Break the DER structure rather than the signature's value: overwrite the
	// leading 0x30 sequence tag.
	sig[0] = 0x31

	if _, err := psbt.NewFromRawBytes(bytes.NewReader(serialize(t, signed)), false); err == nil {
		t.Fatal("btcd parsed a partial signature that is not DER; the merge's own " +
			"length guard is now the only thing between that and the finalizer")
	}
}

// TestEveryDeviceSayingNoIsNotAnError worth reporting as a crash. It is one more
// signing round, and the message should say so.
func TestEveryDeviceSayingNoIsNotAnError(t *testing.T) {
	w := newWallet(t, 2, 2)
	b := newBatch(t, w)

	_, err := combine.Merge(b.base, []combine.Part{
		{Label: "cold1", PSBT: b.base},
		{Label: "cold2", PSBT: b.base},
	})
	if !errors.Is(err, combine.ErrNoSignatures) {
		t.Fatalf("unsigned packets were accepted: %v", err)
	}
	if !strings.Contains(err.Error(), "one more signing round") {
		t.Errorf("the message does not say what it costs: %v", err)
	}
}

// TestUnlabelledAndDuplicateLabelsAreRefused. The label is the whole mechanism
// by which a refusal names a device.
func TestUnlabelledAndDuplicateLabelsAreRefused(t *testing.T) {
	w := newWallet(t, 2, 2)
	b := newBatch(t, w)

	unlabelled := b.sign(t, 0)
	unlabelled.Label = ""
	if _, err := combine.Merge(b.base, []combine.Part{unlabelled}); err == nil {
		t.Error("an unlabelled packet was accepted")
	}

	same := b.sign(t, 1)
	same.Label = "cold1"
	if _, err := combine.Merge(b.base, []combine.Part{b.sign(t, 0), same}); err == nil {
		t.Error("two packets with the same label were accepted")
	}
}

func report(v *plan.Verification) string {
	if v == nil {
		return ""
	}
	return v.Report()
}

// TestParseReadsEitherEncoding is the tolerance three transports share.
//
// It moved here from internal/signers when the browser upload needed it: the file
// handshake reads a file a wallet wrote, and so does a browser upload, and two
// sniffers in two packages could disagree about one file. A disagreement there
// surfaces as a device being blamed for something a transport did, which is the
// worst place in this product to be wrong about who is at fault.
//
// The fixture is the real one — a serialised P2WSH packet, not a five-byte
// prefix — so "it parsed" means the round trip preserved a packet rather than
// that the magic matched.
func TestParseReadsEitherEncoding(t *testing.T) {
	w := newWallet(t, 2, 2)
	b := newBatch(t, w)

	b64, err := parse(t, b.base).B64Encode()
	if err != nil {
		t.Fatalf("B64Encode: %v", err)
	}

	for _, tc := range []struct {
		name string
		in   []byte
	}{
		{"binary, as Sparrow writes it", b.base},
		{"base64, as Core writes it", []byte(b64)},
		{"base64 with the trailing newline a file has", []byte(b64 + "\n")},
		{"base64 with leading whitespace, as a paste carries", []byte("  " + b64 + "  ")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := combine.Parse(tc.in)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if !bytes.Equal(got, b.base) {
				t.Errorf("Parse returned %d bytes, not the packet's %d",
					len(got), len(b.base))
			}
		})
	}
}

// TestParseSaysWhatWasActuallyThere.
//
// The refusal an operator reads has to name the file's contents, not base64's
// complaint about padding — "illegal base64 data at input byte 3" tells nobody
// whether they picked the wrong file, exported the wrong format, or hit a
// truncated write. This is the message internal/signers' file handshake already
// gave, and it is now what a browser upload gives too.
func TestParseSaysWhatWasActuallyThere(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{"empty", "", "it is empty"},
		{"whitespace only", "  \n\t ", "it is empty"},
		{"not base64 at all", "not base64 at all", "neither base64 nor a PSBT"},
		{"a wallet's error page", "<html>Sign in</html>", "neither base64 nor a PSBT"},
		// Valid base64 that is not a PSBT: the message must not claim it is not
		// base64, because it is — the packet is what is wrong with it.
		{"base64 of something else", "aGVsbG8gd29ybGQ=", "PSBT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := combine.Parse([]byte(tc.in))
			if err == nil {
				t.Fatal("it parsed")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Parse(%q) said %q, which does not say %q",
					tc.in, err, tc.want)
			}
		})
	}

	// The one that would be most misleading if it were wrong: valid base64 of
	// something else must not be reported as bad base64.
	_, err := combine.Parse([]byte("aGVsbG8gd29ybGQ="))
	if err == nil {
		t.Fatal("base64 of a non-PSBT parsed")
	}
	if strings.Contains(err.Error(), "neither base64 nor a PSBT") {
		t.Errorf("valid base64 of a non-PSBT is reported as not being base64: %v", err)
	}
}

// TestOneWalletsFullySignedPacketIsAcceptedAndTheSameOneIsRefusedByTheMerge is
// the collision the inversion left behind, settled.
//
// Step 7 is "sign in Sparrow", and what Sparrow hands back is a finalized PSBT —
// the exact input Merge refuses. The two entry points want opposite answers about
// the same bytes and both are right for their own caller, so the assertion is
// that one file goes both ways: accepted by Accept, refused by Merge, with the
// refusal still naming the reason.
func TestOneWalletsFullySignedPacketIsAcceptedAndTheSameOneIsRefusedByTheMerge(t *testing.T) {
	w := newWallet(t, 2, 2)
	b := newBatch(t, w)

	// The harness playing Sparrow: both halves sign and the combining happens
	// *outside* the app, which is what makes this a complete witness rather than
	// two partials. Nothing in the production path does this any more.
	merged, err := combine.Merge(b.base, []combine.Part{b.sign(t, 0), b.sign(t, 1)})
	if err != nil {
		t.Fatalf("the fixture's own merge: %v", err)
	}
	if err := psbt.MaybeFinalizeAll(merged.Packet); err != nil {
		t.Fatalf("finalizing the fixture: %v", err)
	}
	signed := serialize(t, merged.Packet)

	final, v, err := combine.Accept(b.plan, b.base, signed)
	if err != nil {
		t.Fatalf("Accept refused the input step 7 actually produces: %v", err)
	}
	if !v.OK() {
		t.Fatalf("the plan re-check failed: %v", v.Problems)
	}
	if final.TxID != merged.TxID {
		t.Errorf("Accept extracted %s, and the base's unsigned txid is %s. Adding "+
			"witnesses cannot move a txid, so these must be equal", final.TxID, merged.TxID)
	}
	if len(final.RawTx) == 0 {
		t.Error("Accept produced no network serialization, which is the artifact")
	}
	// The witnesses were executed rather than counted: that is what Finalize does
	// with an already-final input, and it is the only check left on it.
	if len(final.Signers) != 1 || final.Signers[0] != combine.SigningWalletLabel {
		t.Errorf("Accept credited %v, want just %q", final.Signers,
			combine.SigningWalletLabel)
	}

	// And the same bytes through the multi-device path are still refused.
	if _, err := combine.Merge(b.base, []combine.Part{
		{Label: "cold1", PSBT: signed},
	}); !errors.Is(err, combine.ErrAlreadyFinalized) {
		t.Fatalf("Merge accepted a finalized packet, or refused it for another "+
			"reason: %v", err)
	}
}

// TestAcceptTakesACompleteSetOfPartialsToo.
//
// A wallet that holds every key but does not finalize — Core's walletprocesspsbt
// with finalize=false, and some hardware wallets' default — hands back partial
// signatures rather than witnesses. That is still one packet from one wallet, so
// Accept finalizes it here. Without this, "sign in Sparrow" would work and "sign
// in the thing that behaves slightly differently" would not.
func TestAcceptTakesACompleteSetOfPartialsToo(t *testing.T) {
	w := newWallet(t, 2, 2)
	b := newBatch(t, w)

	both := parse(t, b.sign(t, 0).PSBT)
	other := parse(t, b.sign(t, 1).PSBT)
	both.Inputs[0].PartialSigs = append(both.Inputs[0].PartialSigs,
		other.Inputs[0].PartialSigs...)

	final, v, err := combine.Accept(b.plan, b.base, serialize(t, both))
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if !v.OK() {
		t.Fatalf("the plan re-check failed: %v", v.Problems)
	}
	if len(final.RawTx) == 0 {
		t.Error("no network serialization")
	}
}

// TestAcceptStillRefusesAMovedTXID is I-3, which now carries the weight I-2 used
// to: it is the only load-bearing check on what comes back from step 7.
func TestAcceptStillRefusesAMovedTXID(t *testing.T) {
	w := newWallet(t, 2, 2)
	b := newBatch(t, w)

	// A wallet that re-selected coins, re-derived change, or simply flipped a
	// sequence number. Any of the three moves the txid, and LND has already
	// committed to the old one at psbt_verify.
	moved := parse(t, b.base)
	moved.UnsignedTx.TxIn[0].Sequence = 0xfffffffd
	moved.Inputs[0].PartialSigs = nil

	_, _, err := combine.Accept(b.plan, b.base, serialize(t, moved))
	if !errors.Is(err, combine.ErrDifferentTransaction) {
		t.Fatalf("a returned packet with a different unsigned transaction was "+
			"accepted, or refused for another reason: %v", err)
	}
}

// TestAcceptRefusesAWalletThatSignedNothing.
//
// The likeliest step-7 mistake is handing back the unsigned file — the operator
// saved to the wrong path, or Sparrow was never asked to sign. That has to read as
// "nothing was signed" rather than as a batch that cannot be finalized.
func TestAcceptRefusesAWalletThatSignedNothing(t *testing.T) {
	w := newWallet(t, 2, 2)
	b := newBatch(t, w)

	_, _, err := combine.Accept(b.plan, b.base, b.base)
	if !errors.Is(err, combine.ErrNoSignatures) {
		t.Fatalf("the unsigned packet handed back as the signed one was not "+
			"reported as unsigned: %v", err)
	}
}
