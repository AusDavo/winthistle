package signers

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

	"github.com/AusDavo/winthistle/internal/config"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

// samplePSBT is a real, minimal PSBT: one input, one output, unsigned.
//
// Real rather than a string of bytes, because what these transports have to get
// right is that a PSBT survives the round trip byte for byte — combine refuses
// a packet whose unsigned transaction differs from the base, and a transport
// that trimmed or re-encoded would produce exactly that refusal, blamed on the
// device.
func samplePSBT(t *testing.T) (b64 string, raw []byte) {
	t.Helper()

	var prev chainhash.Hash
	prev[0] = 0x11
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: *wire.NewOutPoint(&prev, 0),
		Sequence:         0xfffffffe,
	})
	tx.AddTxOut(wire.NewTxOut(100_000, bytes.Repeat([]byte{0x00, 0x14}, 1)))

	packet, err := psbt.NewFromUnsignedTx(tx)
	if err != nil {
		t.Fatalf("building a sample PSBT: %v", err)
	}
	b64, err = packet.B64Encode()
	if err != nil {
		t.Fatalf("encoding a sample PSBT: %v", err)
	}
	raw, err = base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("decoding a sample PSBT: %v", err)
	}
	return b64, raw
}

func TestACommandSignerHandsThePSBTOverAndBack(t *testing.T) {
	b64, raw := samplePSBT(t)

	set, err := New([]config.Signer{{Label: "cold1", Command: "cat"}}, Options{
		Out: new(bytes.Buffer),
	})
	if err != nil {
		t.Fatalf("building the set: %v", err)
	}
	devices := set.Round("batch")
	if len(devices) != 1 || devices[0].Label != "cold1" {
		t.Fatalf("round produced %+v", devices)
	}

	part, err := devices[0].Sign(context.Background(), b64)
	if err != nil {
		t.Fatalf("signing: %v", err)
	}
	if part.Label != "cold1" {
		t.Errorf("the part is labelled %q, so a refusal could not name the device",
			part.Label)
	}
	if !bytes.Equal(part.PSBT, raw) {
		t.Error("the PSBT did not survive the round trip byte for byte")
	}
}

// TestAFailedCommandNamesTheDeviceAndSaysWhat. With m devices in a room, "a
// signer failed" is a hunt and "cold2 did, and it said this" is a fix.
func TestAFailedCommandNamesTheDeviceAndSaysWhat(t *testing.T) {
	b64, _ := samplePSBT(t)

	set, err := New([]config.Signer{
		{Label: "cold2", Command: "echo 'no card inserted' >&2; exit 3"},
	}, Options{Out: new(bytes.Buffer)})
	if err != nil {
		t.Fatalf("building the set: %v", err)
	}
	_, err = set.Round("batch")[0].Sign(context.Background(), b64)
	if err == nil {
		t.Fatal("a command that exited 3 was treated as a signature")
	}
	for _, want := range []string{"cold2", "no card inserted"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure does not mention %q: %v", want, err)
		}
	}
}

func TestACommandThatReturnsRubbishIsNotASignature(t *testing.T) {
	b64, _ := samplePSBT(t)

	set, err := New([]config.Signer{
		{Label: "cold1", Command: "echo 'Signed successfully!'"},
	}, Options{Out: new(bytes.Buffer)})
	if err != nil {
		t.Fatalf("building the set: %v", err)
	}
	if _, err := set.Round("batch")[0].Sign(context.Background(), b64); err == nil {
		t.Fatal("a cheerful message was accepted as a PSBT")
	}
}

// TestTheFileHandshakeReadsEitherEncoding: Sparrow writes binary .psbt files
// and Core writes base64, and an operator moving files by hand should not have
// to know which this tool wanted.
func TestTheFileHandshakeReadsEitherEncoding(t *testing.T) {
	b64, raw := samplePSBT(t)

	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"base64, as Core writes it", []byte(b64 + "\n")},
		{"binary, as Sparrow writes it", raw},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			out := new(bytes.Buffer)
			set, err := New([]config.Signer{{Label: "cold1"}}, Options{
				Dir: dir, Out: out, Poll: 10 * time.Millisecond,
			})
			if err != nil {
				t.Fatalf("building the set: %v", err)
			}

			done := make(chan error, 1)
			go func() {
				part, err := set.Round("batch")[0].Sign(context.Background(), b64)
				if err == nil && !bytes.Equal(part.PSBT, raw) {
					err = errors.New("the PSBT did not survive the round trip")
				}
				done <- err
			}()

			req := filepath.Join(dir, "batch-cold1.psbt")
			waitForFile(t, req)
			if got, err := os.ReadFile(req); err != nil {
				t.Fatalf("reading what was written for the device: %v", err)
			} else if strings.TrimSpace(string(got)) != b64 {
				t.Error("the file written for the device is not the PSBT it was given")
			}
			if !strings.Contains(out.String(), req) {
				t.Errorf("the operator was not told where the file is:\n%s", out)
			}
			if !strings.Contains(out.String(), "finalize") {
				t.Errorf("the operator was not warned off finalizing:\n%s", out)
			}

			signed := filepath.Join(dir, "batch-cold1-signed.psbt")
			if err := os.WriteFile(signed, tc.body, 0o600); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("the handshake: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the handshake never noticed the signed file")
			}
		})
	}
}

// TestAStaleSignedFileIsNotMistakenForThisRound.
//
// The dress rehearsal and the real batch are two rounds minutes apart. A signed
// file left over from the first, picked up by the second, is a signature over
// the decoy — which internal/combine would refuse at the worst possible moment,
// with a message about a moved txid rather than about a leftover file.
func TestAStaleSignedFileIsNotMistakenForThisRound(t *testing.T) {
	b64, raw := samplePSBT(t)
	dir := t.TempDir()

	stale := filepath.Join(dir, "batch-cold1-signed.psbt")
	if err := os.WriteFile(stale, []byte("not a psbt at all"), 0o600); err != nil {
		t.Fatal(err)
	}

	set, err := New([]config.Signer{{Label: "cold1"}}, Options{
		Dir: dir, Out: new(bytes.Buffer), Poll: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("building the set: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := set.Round("batch")[0].Sign(context.Background(), b64)
		done <- err
	}()

	waitForFile(t, filepath.Join(dir, "batch-cold1.psbt"))
	select {
	case err := <-done:
		t.Fatalf("the stale file was read as this round's answer: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	if err := os.WriteFile(stale, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the handshake: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the handshake never noticed the signed file")
	}
}

func TestTheHandshakeGivesUpWithTheContext(t *testing.T) {
	b64, _ := samplePSBT(t)
	set, err := New([]config.Signer{{Label: "cold1"}}, Options{
		Dir: t.TempDir(), Out: new(bytes.Buffer), Poll: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("building the set: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if _, err := set.Round("batch")[0].Sign(ctx, b64); err == nil {
		t.Fatal("the handshake outlived its context")
	}
}

func TestASetNeedsSignersAndADirectoryToHandThemFiles(t *testing.T) {
	if _, err := New(nil, Options{}); err == nil {
		t.Error("a set with no signers was accepted")
	}
	_, err := New([]config.Signer{{Label: "cold1"}}, Options{})
	if err == nil || !strings.Contains(err.Error(), "no directory") {
		t.Errorf("a file-handshake signer with nowhere to write was accepted: %v", err)
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never appeared", path)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
