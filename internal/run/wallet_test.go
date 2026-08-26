package run_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AusDavo/winthistle/internal/combine"
	"github.com/AusDavo/winthistle/internal/run"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// The file transport, with no harness and no Core. What it has to get right is
// which file it reads, when it refuses one, and that it never guesses about
// binary-or-base64 on its own.

// unsignedPacket is a minimal PSBT: one segwit input, one output, no signatures.
func unsignedPacket(t *testing.T) *psbt.Packet {
	t.Helper()
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Hash: chainhash.Hash{0x0a}, Index: 0},
		Sequence:         wire.MaxTxInSequenceNum,
	})
	script, err := txscript.NewScriptBuilder().AddOp(txscript.OP_0).
		AddData(bytes.Repeat([]byte{0x11}, 20)).Script()
	if err != nil {
		t.Fatalf("building a script: %v", err)
	}
	tx.AddTxOut(wire.NewTxOut(90_000, script))

	p, err := psbt.NewFromUnsignedTx(tx)
	if err != nil {
		t.Fatalf("building a packet: %v", err)
	}
	p.Inputs[0].WitnessUtxo = wire.NewTxOut(100_000, script)
	return p
}

func packetBytes(t *testing.T, p *psbt.Packet) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := p.Serialize(&buf); err != nil {
		t.Fatalf("serialising: %v", err)
	}
	return buf.Bytes()
}

// TestTheFileTransportRefusesAPathThatAlreadyExists.
//
// A rule rather than caution. The funding addresses this transaction pays to did
// not exist before the run — LND issued them at step 2 — so a file that predates
// the run cannot be this batch's transaction, and reading one would arm a batch
// against somebody else's outputs and find out at step 5. The refusal is made
// before LND is dialled, which is why it costs nothing.
func TestTheFileTransportRefusesAPathThatAlreadyExists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "batch.psbt")
	if err := os.WriteFile(path, []byte("whatever was here before"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := run.NewFileWallet(path, new(bytes.Buffer))
	if err == nil {
		t.Fatal("a path that already held a file was accepted")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}

func TestTheFileTransportNeedsAPath(t *testing.T) {
	if _, err := run.NewFileWallet("   ", new(bytes.Buffer)); err == nil {
		t.Fatal("an empty --psbt was accepted")
	}
}

// TestTheSignedFileSitsBesideTheUnsignedOne, derived rather than asked for, so
// the two paths cannot be given the same value.
func TestTheSignedFileSitsBesideTheUnsignedOne(t *testing.T) {
	dir := t.TempDir()
	w, err := run.NewFileWallet(filepath.Join(dir, "batch.psbt"), new(bytes.Buffer))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := w.SignedPath(), filepath.Join(dir, "batch-signed.psbt"); got != want {
		t.Errorf("SignedPath is %q, want %q", got, want)
	}
	if w.SignedPath() == w.Unsigned {
		t.Error("the signed path is the unsigned one, so there would be nothing " +
			"left to compare the returned transaction against")
	}
}

// TestStepFourRefusesASignedPacket is I-1 at the one place it can still be
// defeated from outside.
//
// Step 4 is before the gate. A wallet that signs there leaves the operator
// holding a broadcastable funding transaction while nothing has reached
// chan_pending — and Sparrow's broadcast button is two clicks from its signing
// one. A transaction that reached the network then would confirm one 2-of-2
// output per channel with no channel behind any of them.
func TestStepFourRefusesASignedPacket(t *testing.T) {
	dir := t.TempDir()
	out := new(bytes.Buffer)
	w, err := run.NewFileWallet(filepath.Join(dir, "batch.psbt"), out)
	if err != nil {
		t.Fatal(err)
	}
	w.Poll = 5 * time.Millisecond

	// The likeliest way this happens: the operator hit Sign in the same visit and
	// the wallet finalized, which is what Sparrow does. A complete witness, which
	// is exactly what step 7 wants and step 4 must not have.
	p := unsignedPacket(t)
	var wit bytes.Buffer
	if err := psbt.WriteTxWitness(&wit, [][]byte{{0x01}, {0x02}}); err != nil {
		t.Fatal(err)
	}
	p.Inputs[0].FinalScriptWitness = wit.Bytes()
	if err := os.WriteFile(w.Unsigned, packetBytes(t, p), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := w.Built(ctx, nil); !errors.Is(err, combine.ErrAlreadySigned) {
		t.Fatalf("a signed packet was accepted at step 4, or refused for another "+
			"reason: %v", err)
	}
}

// TestTheFileTransportReadsBothEncodings.
//
// Sparrow writes binary .psbt files and Core writes base64, and an operator
// moving a file by hand should not have to know which one this build wanted. One
// sniffer settles it — combine.Parse, on BIP174's five magic bytes — because two
// sniffers in two packages could disagree about one file, and that surfaces as a
// wallet being blamed for something a transport did.
func TestTheFileTransportReadsBothEncodings(t *testing.T) {
	raw := packetBytes(t, unsignedPacket(t))

	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"binary", raw},
		{"base64", []byte(base64.StdEncoding.EncodeToString(raw) + "\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			w, err := run.NewFileWallet(filepath.Join(dir, "batch.psbt"),
				new(bytes.Buffer))
			if err != nil {
				t.Fatal(err)
			}
			w.Poll = 5 * time.Millisecond
			if err := os.WriteFile(w.Unsigned, tc.body, 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			got, err := w.Built(ctx, nil)
			if err != nil {
				t.Fatalf("Built: %v", err)
			}
			if !bytes.Equal(got, raw) {
				t.Error("the packet did not survive the transport")
			}
		})
	}
}

// TestStepSevenClearsTheAnswerBeforeAskingTheQuestion.
//
// A signed file left by an earlier attempt must not be read as this one's. The
// unsigned path is guaranteed fresh by NewFileWallet; this one the app names
// itself, so clearing it is the app's to do.
func TestStepSevenClearsTheAnswerBeforeAskingTheQuestion(t *testing.T) {
	dir := t.TempDir()
	out := new(bytes.Buffer)
	w, err := run.NewFileWallet(filepath.Join(dir, "batch.psbt"), out)
	if err != nil {
		t.Fatal(err)
	}
	w.Poll = 5 * time.Millisecond

	stale := packetBytes(t, unsignedPacket(t))
	if err := os.WriteFile(w.SignedPath(), stale, 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	// With the stale file cleared there is nothing to read, so this has to time
	// out rather than return the stale packet.
	got, err := w.Signed(ctx, stale)
	if err == nil {
		t.Fatalf("a file left over from an earlier attempt was read as this run's "+
			"signed transaction (%d bytes)", len(got))
	}
	if _, statErr := os.Stat(w.SignedPath()); !os.IsNotExist(statErr) {
		t.Error("the stale signed file is still there, so the next attempt would " +
			"read it too")
	}
	if !strings.Contains(out.String(), w.SignedPath()) {
		t.Error("the instructions do not say where to put the signed transaction")
	}
}

// signedRawTX is the finished transaction an operator has in hand at step 7: the
// packet's own transaction with a witness on it. Sparrow's View Final
// Transaction shows this as hex, and lncli is fed exactly that at the equivalent
// prompt in the manual workflow this tool replaces.
//
// The witness is not a real signature. Nothing on this path executes one — the
// transport lifts witnesses and combine.Accept is what runs them against their
// scripts, and internal/combine's own tests do that with real keys.
func signedRawTX(t *testing.T, p *psbt.Packet) []byte {
	t.Helper()
	tx := p.UnsignedTx.Copy()
	tx.TxIn[0].Witness = wire.TxWitness{{0x30, 0x44, 0x01}, {0x02, 0x03}}
	var buf bytes.Buffer
	if err := tx.Serialize(&buf); err != nil {
		t.Fatalf("serialising the signed transaction: %v", err)
	}
	return buf.Bytes()
}

// TestStepFourStillRefusesARawSignedTransaction is the regression guard for
// issue #3's design caution, and it is the important one.
//
// Step 7 reads a raw transaction now. combine.Parse is the sniffer step 4 and
// step 7 used to share, so teaching *it* about raw transactions would have
// taught step 4 about them too — and step 4 is before the gate. A raw signed
// transaction landing there is precisely the packet combine.Unsigned exists to
// refuse, and combine.Unsigned reads a PSBT's partial signatures, which a raw
// transaction does not have: it would have been accepted in silence, by a check
// that had nothing to look at.
//
// So the acceptance lives at step 7's call site and this asserts the other half:
// whatever step 7 learns, step 4 does not.
func TestStepFourStillRefusesARawSignedTransaction(t *testing.T) {
	p := unsignedPacket(t)
	raw := signedRawTX(t, p)

	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"binary", raw},
		{"hex", []byte(hex.EncodeToString(raw) + "\n")},
		// The unsigned transaction too. It is just as useless at step 4 — no
		// witness UTXOs and no key origins, so the verifier could neither check the
		// inputs nor account for the change output — and if this one were read, the
		// signed one two lines above would have been read as well.
		{"unsigned", func() []byte {
			var buf bytes.Buffer
			if err := p.UnsignedTx.Serialize(&buf); err != nil {
				t.Fatal(err)
			}
			return buf.Bytes()
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			w, err := run.NewFileWallet(filepath.Join(dir, "batch.psbt"),
				new(bytes.Buffer))
			if err != nil {
				t.Fatal(err)
			}
			w.Poll = 5 * time.Millisecond
			if err := os.WriteFile(w.Unsigned, tc.body, 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			got, err := w.Built(ctx, nil)
			if err == nil {
				t.Fatalf("step 4 accepted a raw transaction (%d bytes). The gate is "+
					"not open yet, so a wallet that signed here leaves the operator "+
					"holding broadcastable bytes with no channel recoverable", len(got))
			}
			if !strings.Contains(err.Error(), "not a PSBT") {
				t.Errorf("the refusal does not say what step 4 wanted: %v", err)
			}
		})
	}
}

// writeAfterClear puts the signed file in place once Signed has cleared the
// path, which Signed does before it starts polling. Racing that clear would let
// the run read the file and then delete it, or delete the file it was about to
// read; waiting for the path to be gone makes the order a fact rather than a
// timing.
func writeAfterClear(t *testing.T, w *run.FileWallet, body []byte) {
	t.Helper()
	if err := os.WriteFile(w.SignedPath(), []byte("cleared before this is read"),
		0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Written aside and renamed into place, so the poller cannot catch the
		// file between create-and-truncate and the bytes landing in it and read a
		// wallet's export as an empty one.
		staged := w.SignedPath() + ".staging"
		if err := os.WriteFile(staged, body, 0o600); err != nil {
			return
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(w.SignedPath()); os.IsNotExist(err) {
				_ = os.Rename(staged, w.SignedPath())
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	t.Cleanup(func() { <-done })
}

// TestStepSevenReadsARawTransaction is issue #3 at the transport, which is where
// the mainnet cold probe hit it: the operator had signed correctly, exported via
// Sparrow's View Final Transaction, and the run died on the wrapper.
//
// What comes back is the packet combine.Accept takes — the base's own
// transaction with the witnesses written onto it — so nothing downstream of this
// call learns a second encoding.
func TestStepSevenReadsARawTransaction(t *testing.T) {
	p := unsignedPacket(t)
	base := packetBytes(t, p)
	raw := signedRawTX(t, p)

	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"binary", raw},
		{"hex", []byte(hex.EncodeToString(raw) + "\n")},
		{"psbt", func() []byte {
			signed := unsignedPacket(t)
			var wit bytes.Buffer
			if err := psbt.WriteTxWitness(&wit, [][]byte{{0x30, 0x44, 0x01}, {0x02, 0x03}}); err != nil {
				t.Fatal(err)
			}
			signed.Inputs[0].FinalScriptWitness = wit.Bytes()
			return packetBytes(t, signed)
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			out := new(bytes.Buffer)
			w, err := run.NewFileWallet(filepath.Join(dir, "batch.psbt"), out)
			if err != nil {
				t.Fatal(err)
			}
			w.Poll = 5 * time.Millisecond
			writeAfterClear(t, w, tc.body)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			got, err := w.Signed(ctx, base)
			if err != nil {
				t.Fatalf("Signed: %v", err)
			}
			back, err := psbt.NewFromRawBytes(bytes.NewReader(got), false)
			if err != nil {
				t.Fatalf("step 7 returned something that is not a PSBT: %v", err)
			}
			if back.UnsignedTx.TxHash() != p.UnsignedTx.TxHash() {
				t.Errorf("the txid moved through the transport: %s, want %s",
					back.UnsignedTx.TxHash(), p.UnsignedTx.TxHash())
			}
			if len(back.Inputs[0].FinalScriptWitness) == 0 {
				t.Error("the witness did not survive the transport, so the one thing " +
					"the signed file carries was dropped")
			}
			if !strings.Contains(out.String(), "raw transaction") {
				t.Error("the instructions do not say a raw transaction is read, which " +
					"is the ergonomics half of the fix")
			}
		})
	}
}

// TestStepSevenRefusesATransactionThatIsNotThePinnedOne. I-3 reaching the
// operator through the transport, rather than being noticed later by Accept.
func TestStepSevenRefusesATransactionThatIsNotThePinnedOne(t *testing.T) {
	p := unsignedPacket(t)
	base := packetBytes(t, p)

	tx := p.UnsignedTx.Copy()
	tx.TxIn[0].Witness = wire.TxWitness{{0x30, 0x44, 0x01}}
	tx.TxIn[0].Sequence = wire.MaxTxInSequenceNum - 2 // a changed txid
	var buf bytes.Buffer
	if err := tx.Serialize(&buf); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	w, err := run.NewFileWallet(filepath.Join(dir, "batch.psbt"), new(bytes.Buffer))
	if err != nil {
		t.Fatal(err)
	}
	w.Poll = 5 * time.Millisecond
	writeAfterClear(t, w, buf.Bytes())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := w.Signed(ctx, base); !errors.Is(err, combine.ErrTXIDMoved) {
		t.Fatalf("a transaction that is not the pinned one was accepted at step 7, "+
			"or refused for another reason: %v", err)
	}
}
