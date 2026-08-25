package run_test

import (
	"bytes"
	"context"
	"encoding/base64"
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
// itself, so clearing it is the app's to do — the same rule internal/signers'
// file handshake has always followed.
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
