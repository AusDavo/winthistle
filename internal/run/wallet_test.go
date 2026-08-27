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
	"sync"
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
		// Written aside and renamed into place, so this test is about the encoding
		// it names and nothing else. The transport does not depend on the rename any
		// more — readWhole declines a file it caught mid-write, and the tests at the
		// bottom of this file are where that is asserted — but a fixture that hands
		// over the whole file at once keeps the other cases single-subject.
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

// TestStepSevenReadsARawTransactionSavedAsTXN is issue #5.
//
// Step 7 takes a raw transaction, and a wallet asked to save one names it .txn —
// Sparrow does. SignedPath inherits its extension from --psbt, so it asserts
// .psbt, and before this the .txn beside it was never looked at. That failed as
// silence rather than as a refusal: step 7 has no deadline, by design, so the run
// waited while the operator watched their wallet report that it had saved the
// file.
func TestStepSevenReadsARawTransactionSavedAsTXN(t *testing.T) {
	p := unsignedPacket(t)
	base := packetBytes(t, p)
	raw := signedRawTX(t, p)

	read := func(t *testing.T, which int) []byte {
		t.Helper()
		dir := t.TempDir()
		w, err := run.NewFileWallet(filepath.Join(dir, "batch.psbt"), new(bytes.Buffer))
		if err != nil {
			t.Fatal(err)
		}
		w.Poll = 5 * time.Millisecond

		paths := w.SignedPaths()
		if len(paths) != 2 {
			t.Fatalf("SignedPaths is %v, want the .psbt and the .txn", paths)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		done := make(chan struct{})
		go func() {
			defer close(done)
			// Written after Signed has cleared the candidates, which is the order a
			// real operator produces: the instructions print, then they save.
			time.Sleep(20 * time.Millisecond)
			_ = os.WriteFile(paths[which], raw, 0o600)
		}()
		got, err := w.Signed(ctx, base)
		<-done
		if err != nil {
			t.Fatalf("saved as %s: %v", filepath.Ext(paths[which]), err)
		}
		return got
	}

	viaPSBTName := read(t, 0)
	viaTXNName := read(t, 1)

	if !bytes.Equal(viaPSBTName, viaTXNName) {
		t.Errorf("the same transaction read back differently depending on the name "+
			"it was saved under: %d bytes via .psbt, %d via .txn",
			len(viaPSBTName), len(viaTXNName))
	}
}

// TestTheSignedNamesNeverCollide keeps SignedPath's own rule while widening the
// search around it.
//
// A wallet that wrote the signed transaction over the unsigned one would leave
// nothing to compare it against, and adding a second name must not create that
// case by another route.
func TestTheSignedNamesNeverCollide(t *testing.T) {
	for _, name := range []string{
		"batch.psbt", "batch.txn", "batch", "batch-signed.psbt", "batch.PSBT",
	} {
		dir := t.TempDir()
		w, err := run.NewFileWallet(filepath.Join(dir, name), new(bytes.Buffer))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		paths := w.SignedPaths()
		if len(paths) == 0 {
			t.Fatalf("%s: no signed path at all", name)
		}
		seen := map[string]bool{}
		for _, p := range paths {
			if p == w.Unsigned {
				t.Errorf("--psbt %s: signed path %q is the unsigned path, so the "+
					"wallet would overwrite what we compare against", name, p)
			}
			if seen[p] {
				t.Errorf("--psbt %s: %q is listed twice", name, p)
			}
			seen[p] = true
		}
		if paths[0] != w.SignedPath() {
			t.Errorf("--psbt %s: SignedPath %q is not tried first (%v)",
				name, w.SignedPath(), paths)
		}
	}
}

// TestStepSevenNamesEveryPathItWatches is the copy half of issue #5.
//
// Watching a name the operator is never told about is the same defect as not
// watching it: they still save to the one they were shown.
func TestStepSevenNamesEveryPathItWatches(t *testing.T) {
	dir := t.TempDir()
	out := new(bytes.Buffer)
	w, err := run.NewFileWallet(filepath.Join(dir, "batch.psbt"), out)
	if err != nil {
		t.Fatal(err)
	}
	w.Poll = 5 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	_, _ = w.Signed(ctx, packetBytes(t, unsignedPacket(t)))

	for _, p := range w.SignedPaths() {
		if !strings.Contains(out.String(), p) {
			t.Errorf("step 7 watches %s and never says so", p)
		}
	}
}

// TestStepSevenClearsEveryCandidate extends the stale-file rule to the name that
// was added, rather than leaving one of the two watched paths unswept.
func TestStepSevenClearsEveryCandidate(t *testing.T) {
	dir := t.TempDir()
	w, err := run.NewFileWallet(filepath.Join(dir, "batch.psbt"), new(bytes.Buffer))
	if err != nil {
		t.Fatal(err)
	}
	w.Poll = 5 * time.Millisecond

	stale := packetBytes(t, unsignedPacket(t))
	for _, p := range w.SignedPaths() {
		if err := os.WriteFile(p, stale, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if got, err := w.Signed(ctx, stale); err == nil {
		t.Fatalf("a stale file was read as this run's signed transaction (%d bytes)",
			len(got))
	}
	for _, p := range w.SignedPaths() {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s survived, so the next attempt would read it", p)
		}
	}
}

// Issue #8: a file that is there is not yet a file that is finished.
//
// Both waits poll a file the wallet is actively writing, and os.ReadFile on one
// caught mid-write returns a prefix and a nil error. The tests below construct
// that rather than hoping for it: a file that never finishes is waited on by
// construction, and a file that does finish is finished only after the transport
// has said it looked and turned the partial one down.
//
// What the wide window actually is, because it decides what is testable here. A
// wallet saving 1,100 bytes opens the file and then writes it, and the gap
// between those two is the whole of the exposure: the file is *empty* for all of
// it, and then goes to its full length in one write. That is the case the flake
// was, and it is closed here absolutely — an empty file is never read, whatever
// it does next. A prefix is what a writer that writes in several chunks leaves
// between two of them; readWhole catches one that moves while it is being read,
// and readWhole's own doc comment says plainly what that does not cover.

// syncBuffer is the transcript, readable from the goroutine staging the writes
// while the transport is writing to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// notRead is the half of that sentence that both unfinished shapes share. The
// tests use it as a synchronisation point — it is printed only after the poller
// has looked at the unfinished file and declined to read it — which is what makes
// the two-stage writes below a constructed race rather than a hopeful one.
//
// Only the shared half, because the rest of the sentence names what was actually
// observed and the two observations are different: a file at zero bytes has seen
// no writer at all. isEmpty and grew are those halves, asserted where each one
// is the one produced.
const (
	notRead = "so it has not been read"
	isEmpty = "has nothing in it yet"
	grew    = "grew while it was being read"
)

// awaitPolled blocks until the transport has looked at an unfinished file and
// turned it down. It reports rather than fails, because it is called from the
// goroutine doing the staging and only the test goroutine may call Fatal — and it
// gives up when stop closes, so a test that has already failed does not then sit
// out the deadline of a stage that will never come.
func awaitPolled(out *syncBuffer, stop <-chan struct{}) bool {
	return awaitCondition(stop, func() bool {
		return strings.Contains(out.String(), notRead)
	})
}

// awaitGone blocks until path is not there, which is how a step-7 fixture waits
// for Signed to have cleared the candidates before it writes one.
func awaitGone(path string, stop <-chan struct{}) bool {
	return awaitCondition(stop, func() bool {
		_, err := os.Stat(path)
		return os.IsNotExist(err)
	})
}

func awaitCondition(stop <-chan struct{}, ok func() bool) bool {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return true
		}
		select {
		case <-stop:
			return false
		case <-time.After(time.Millisecond):
		}
	}
	return false
}

// TestAFileTheWalletHasNotWrittenYetIsNotRead is the defect, at both steps.
//
// The file is created and left empty, which is what a poll landing between the
// wallet's open and its write sees, and it is put there before the call so there
// is no timing to win or lose. An implementation that returns any read which did
// not error hands nothing to combine.Parse and gets "it is empty"; a correct one
// waits. At step 4 that difference is a failed run inside clock A, and every
// peer's reservation with it.
func TestAFileTheWalletHasNotWrittenYetIsNotRead(t *testing.T) {
	whole := packetBytes(t, unsignedPacket(t))

	t.Run("step 4", func(t *testing.T) {
		dir := t.TempDir()
		w, err := run.NewFileWallet(filepath.Join(dir, "batch.psbt"), new(bytes.Buffer))
		if err != nil {
			t.Fatal(err)
		}
		w.Poll = 5 * time.Millisecond
		if err := os.WriteFile(w.Unsigned, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()

		got, err := w.Built(ctx, nil)
		if err == nil {
			t.Fatalf("step 4 read %d bytes out of a file the wallet had not written "+
				"yet. Inside clock A that fails the run, and every peer's reservation "+
				"with it", len(got))
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("step 4 did not wait for the wallet to write the file; it failed "+
				"on what it read: %v", err)
		}
	})

	t.Run("step 7", func(t *testing.T) {
		dir := t.TempDir()
		w, err := run.NewFileWallet(filepath.Join(dir, "batch.psbt"), new(bytes.Buffer))
		if err != nil {
			t.Fatal(err)
		}
		w.Poll = 5 * time.Millisecond

		// Written after the clear, which is where a real one lands too.
		stop := make(chan struct{})
		defer close(stop)
		done := make(chan struct{})
		go func() {
			defer close(done)
			if awaitGone(w.SignedPath(), stop) {
				_ = os.WriteFile(w.SignedPath(), nil, 0o600)
			}
		}()
		t.Cleanup(func() { <-done })

		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()

		got, err := w.Signed(ctx, whole)
		if err == nil {
			t.Fatalf("step 7 read %d bytes out of a file the wallet had not written "+
				"yet", len(got))
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("step 7 did not wait for the wallet to write the file; it failed "+
				"on what it read: %v", err)
		}
	})
}

// TestAFileThatArrivesUnfinishedIsWaitedForAndThenReadWhole is the same race with
// the wallet finishing, which is what actually happens: the file is correct a
// moment later, and the run this used to fail was not wrong about anything.
//
// The second stage is written only once the transport has said it found the file
// unfinished, so the poll between the two stages is an observed fact rather than
// a sleep. An implementation that returns any successful read fails this on the
// bytes and not on a timeout: it has already handed the empty file to the decoder
// by then.
func TestAFileThatArrivesUnfinishedIsWaitedForAndThenReadWhole(t *testing.T) {
	t.Run("step 4", func(t *testing.T) {
		whole := packetBytes(t, unsignedPacket(t))

		dir := t.TempDir()
		out := &syncBuffer{}
		w, err := run.NewFileWallet(filepath.Join(dir, "batch.psbt"), out)
		if err != nil {
			t.Fatal(err)
		}
		w.Poll = 5 * time.Millisecond
		if err := os.WriteFile(w.Unsigned, nil, 0o600); err != nil {
			t.Fatal(err)
		}

		stop := make(chan struct{})
		defer close(stop)
		done := make(chan struct{})
		go func() {
			defer close(done)
			if awaitPolled(out, stop) {
				_ = os.WriteFile(w.Unsigned, whole, 0o600)
			}
		}()
		t.Cleanup(func() { <-done })

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		got, err := w.Built(ctx, nil)
		if err != nil {
			t.Fatalf("Built: %v", err)
		}
		if !bytes.Equal(got, whole) {
			t.Errorf("step 4 came back with %d bytes, want the whole %d-byte packet",
				len(got), len(whole))
		}
	})

	t.Run("step 7", func(t *testing.T) {
		// The .txn name, because that is where this surfaced: issue #5's own test,
		// under load.
		p := unsignedPacket(t)
		base := packetBytes(t, p)
		raw := signedRawTX(t, p)

		dir := t.TempDir()
		out := &syncBuffer{}
		w, err := run.NewFileWallet(filepath.Join(dir, "batch.psbt"), out)
		if err != nil {
			t.Fatal(err)
		}
		w.Poll = 5 * time.Millisecond
		paths := w.SignedPaths()
		txn := paths[len(paths)-1]

		stop := make(chan struct{})
		defer close(stop)
		done := make(chan struct{})
		go func() {
			defer close(done)
			if !awaitGone(txn, stop) {
				return
			}
			if err := os.WriteFile(txn, nil, 0o600); err != nil {
				return
			}
			if awaitPolled(out, stop) {
				_ = os.WriteFile(txn, raw, 0o600)
			}
		}()
		t.Cleanup(func() { <-done })

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
			t.Error("the witness did not survive the transport")
		}
	})
}

// TestTheWaitSaysWhenItIsHoldingOffOnAFile.
//
// Declining to read an unfinished file replaces a decode failure with a wait, and
// a wait nobody can see is the failure issue #5 was: the operator's wallet has
// told them it saved, and the screen would say nothing at all. Step 7 has no
// deadline by design, so silence there lasts until somebody gives up.
func TestTheWaitSaysWhenItIsHoldingOffOnAFile(t *testing.T) {
	dir := t.TempDir()
	out := &syncBuffer{}
	w, err := run.NewFileWallet(filepath.Join(dir, "batch.psbt"), out)
	if err != nil {
		t.Fatal(err)
	}
	w.Poll = 5 * time.Millisecond
	if err := os.WriteFile(w.Unsigned, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, _ = w.Built(ctx, nil)

	if !strings.Contains(out.String(), w.Unsigned) ||
		!strings.Contains(out.String(), notRead) {
		t.Errorf("the wait never said it was holding off on %s, so the operator "+
			"watches nothing happen:\n%s", w.Unsigned, out.String())
	}
	if n := strings.Count(out.String(), notRead); n != 1 {
		t.Errorf("said it %d times; saying it once is the whole point of saying it", n)
	}
	// And it says what it saw. This file is zero bytes and stays zero bytes,
	// which is the state the old wording named least well: it asserted a writer,
	// and nothing here has one.
	if !strings.Contains(out.String(), isEmpty) {
		t.Errorf("the wait did not say the file is empty, and an empty file is "+
			"what it looked at:\n%s", out.String())
	}
	if strings.Contains(out.String(), grew) {
		t.Errorf("the wait said the file grew, and nothing wrote to it:\n%s",
			out.String())
	}
}

// TestAFileCompleteOnTheFirstPollIsReadOnTheFirstPoll.
//
// The other shape for this fix — require the size to be unchanged across two
// consecutive polls — is stronger against a slow writer and costs a whole poll
// interval on every run, which at DefaultPoll is two seconds added to a step
// inside clock A for a file that was finished before anybody looked. Two stats
// around the read cost two syscalls and cost the common case nothing. This is the
// assertion that keeps that true, so the poll interval here is long enough that an
// extra tick could not hide inside it.
func TestAFileCompleteOnTheFirstPollIsReadOnTheFirstPoll(t *testing.T) {
	dir := t.TempDir()
	w, err := run.NewFileWallet(filepath.Join(dir, "batch.psbt"), new(bytes.Buffer))
	if err != nil {
		t.Fatal(err)
	}
	w.Poll = 3 * time.Second

	whole := packetBytes(t, unsignedPacket(t))
	if err := os.WriteFile(w.Unsigned, whole, 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	got, err := w.Built(ctx, nil)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Built: %v", err)
	}
	if !bytes.Equal(got, whole) {
		t.Error("the packet did not survive the transport")
	}
	if elapsed >= w.Poll {
		t.Errorf("a file that was already complete took %s to read, which is a whole "+
			"poll interval of latency added to every run", elapsed)
	}
}

// TestAnInvalidFileStillFailsLoudly is the rejected fix, asserted against.
//
// Retrying on a decode failure would close this race too, and it would make a
// genuinely wrong file — the operator saved the wrong transaction, or signed at
// step 4 — indistinguishable from a slow one. Step 4's refusal of a signed packet
// is I-1's last gate, and a gate that waits instead of refusing is not one. So a
// finished file that does not decode fails on the first look and not on the
// context.
func TestAnInvalidFileStillFailsLoudly(t *testing.T) {
	junk := bytes.Repeat([]byte{0x7f}, 512)

	t.Run("step 4", func(t *testing.T) {
		dir := t.TempDir()
		w, err := run.NewFileWallet(filepath.Join(dir, "batch.psbt"), new(bytes.Buffer))
		if err != nil {
			t.Fatal(err)
		}
		w.Poll = 5 * time.Millisecond
		if err := os.WriteFile(w.Unsigned, junk, 0o600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		_, err = w.Built(ctx, nil)
		if err == nil {
			t.Fatal("step 4 accepted 512 bytes of nothing in particular")
		}
		if errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("step 4 waited out a file that will never decode: %v", err)
		}
		if !strings.Contains(err.Error(), "not a PSBT") {
			t.Errorf("the refusal does not say what step 4 wanted: %v", err)
		}
	})

	t.Run("step 7", func(t *testing.T) {
		dir := t.TempDir()
		w, err := run.NewFileWallet(filepath.Join(dir, "batch.psbt"), new(bytes.Buffer))
		if err != nil {
			t.Fatal(err)
		}
		w.Poll = 5 * time.Millisecond
		writeAfterClear(t, w, junk)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		_, err = w.Signed(ctx, packetBytes(t, unsignedPacket(t)))
		if err == nil {
			t.Fatal("step 7 accepted 512 bytes of nothing in particular")
		}
		if errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("step 7 waited out a file that will never decode: %v", err)
		}
		if !strings.Contains(err.Error(), "neither a PSBT nor a raw transaction") {
			t.Errorf("the refusal does not name both encodings it tried: %v", err)
		}
	})
}

// TestTheRecipientsFileSitsBesideTheUnsignedOne, derived from --psbt the way the
// signed path is, and carrying its own extension so no --psbt value can make it
// collide with a path this transport reads.
func TestTheRecipientsFileSitsBesideTheUnsignedOne(t *testing.T) {
	dir := t.TempDir()
	for _, psbtName := range []string{"batch.psbt", "batch", "batch-recipients.csv"} {
		w, err := run.NewFileWallet(filepath.Join(dir, psbtName), new(bytes.Buffer))
		if err != nil {
			t.Fatalf("--psbt %s: %v", psbtName, err)
		}
		csv := w.RecipientsPath()
		if !strings.HasSuffix(csv, run.RecipientsSuffix) {
			t.Errorf("--psbt %s derives %q, which does not end in %q",
				psbtName, csv, run.RecipientsSuffix)
		}
		// The one that must never happen: writing the recipients over a path this
		// transport is going to read a transaction back from.
		if csv == w.Unsigned {
			t.Errorf("--psbt %s derives its own path for the recipients file", psbtName)
		}
		for _, p := range w.SignedPaths() {
			if csv == p {
				t.Errorf("--psbt %s derives the recipients file onto %q", psbtName, p)
			}
		}
	}
}

// TestTheFileTransportRefusesAnExistingRecipientsFile.
//
// The same rule as the unsigned path, arrived at from the other side. A
// batch-recipients.csv already on disk was written for funding addresses that are
// not this batch's — LND had not issued this batch's yet — so an operator who
// loads it builds a transaction paying somebody else's outputs. Step 5 refuses
// that transaction, but it refuses it inside clock A with n peers holding
// reservations, and this refusal is made before LND is dialled.
//
// Refused rather than overwritten, which is where it parts from SignedPaths: this
// is a name invented out of the operator's own --psbt stem, in the operator's own
// directory, and truncating a file we did not create is not something to do
// silently.
func TestTheFileTransportRefusesAnExistingRecipientsFile(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "batch"+run.RecipientsSuffix)
	if err := os.WriteFile(stale, []byte("address,amount_btc,label\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The PSBT path itself is clear, so the existing refusal cannot be what fires.
	_, err := run.NewFileWallet(filepath.Join(dir, "batch.psbt"), new(bytes.Buffer))
	if err == nil {
		t.Fatal("a run was allowed to start over a recipients file from another batch")
	}
	if !strings.Contains(err.Error(), stale) {
		t.Errorf("the refusal does not name the file it is about: %v", err)
	}
	if _, err := os.ReadFile(stale); err != nil {
		t.Errorf("the refused run touched the file anyway: %v", err)
	}
}

// TestStepFourWritesTheRecipientsAndSaysWhere.
//
// The file is the convenience and the printed path is what makes it one: a file
// under a name the operator is never told is the same defect as no file, which is
// issue #5's standing lesson. The table stays on screen either way — it is the
// attribution, and the CSV does not replace it.
func TestStepFourWritesTheRecipientsAndSaysWhere(t *testing.T) {
	dir := t.TempDir()
	out := new(bytes.Buffer)
	w, err := run.NewFileWallet(filepath.Join(dir, "batch.psbt"), out)
	if err != nil {
		t.Fatal(err)
	}
	w.Poll = 5 * time.Millisecond

	pay := []run.Recipient{
		{Label: "channel 1  ACINQ, Inc.", Address: "bcrt1qexample", AmountSat: 250_000},
		{Label: "anchor reserve", Address: "bcrt1qreserve", AmountSat: 50_000},
	}

	if err := os.WriteFile(w.Unsigned, packetBytes(t, unsignedPacket(t)), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := w.Built(ctx, pay); err != nil {
		t.Fatalf("step 4: %v", err)
	}

	body, err := os.ReadFile(w.RecipientsPath())
	if err != nil {
		t.Fatalf("the recipients file was not written: %v", err)
	}
	want := "address,amount_btc,label\n" +
		"bcrt1qexample,0.00250000,\"channel 1  ACINQ, Inc.\"\n" +
		"bcrt1qreserve,0.00050000,\"anchor reserve\"\n"
	if string(body) != want {
		t.Errorf("the recipients file is\n%q\nwant\n%q", body, want)
	}
	if !strings.Contains(out.String(), w.RecipientsPath()) {
		t.Errorf("step 4 wrote a file it never named:\n%s", out.String())
	}
	// The unit cannot be read off the file, so the copy has to carry it, and the
	// trust boundary has to stay said out loud.
	for _, phrase := range []string{"BTC", "step 5"} {
		if !strings.Contains(out.String(), phrase) {
			t.Errorf("step 4's copy never says %q:\n%s", phrase, out.String())
		}
	}
}
