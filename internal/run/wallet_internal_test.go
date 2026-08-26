package run

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// readWhole's own rules, close up. The tests through FileWallet say what the
// transport does with the answer; these say what the answer is.

// TestReadWholeSaysNothingIsThereWhenNothingIsThere. present is what tells the
// caller apart "the wallet has not saved yet" from "the wallet is part-way
// through saving", and only the second is worth printing a line about.
func TestReadWholeSaysNothingIsThereWhenNothingIsThere(t *testing.T) {
	body, present, err := readWhole(filepath.Join(t.TempDir(), "not-there.psbt"))
	if err != nil {
		t.Fatalf("readWhole on an absent path: %v", err)
	}
	if body != nil || present {
		t.Errorf("an absent path reported present=%v with %d bytes", present, len(body))
	}
}

// TestReadWholeNeverReadsAnEmptyFile is the wide half of issue #8.
//
// A wallet that opens the file and then writes it leaves it at zero bytes for the
// whole gap between those two, and that is where the flake landed. No valid
// transaction is empty in either encoding, so there is nothing to weigh up here:
// the file is skipped without being read, whatever it does next.
func TestReadWholeNeverReadsAnEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "batch.psbt")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	body, present, err := readWhole(path)
	if err != nil {
		t.Fatalf("readWhole on an empty file: %v", err)
	}
	if !present {
		t.Error("the file is there and readWhole says it is not, so the wait would " +
			"never say it is holding off on it")
	}
	if body != nil {
		t.Errorf("an empty file was read as %d bytes", len(body))
	}
}

// TestReadWholeReadsAFileThatIsNotMoving, which is every real one.
func TestReadWholeReadsAFileThatIsNotMoving(t *testing.T) {
	path := filepath.Join(t.TempDir(), "batch.psbt")
	want := bytes.Repeat([]byte{0x42}, 1100)
	if err := os.WriteFile(path, want, 0o600); err != nil {
		t.Fatal(err)
	}
	body, present, err := readWhole(path)
	if err != nil {
		t.Fatalf("readWhole: %v", err)
	}
	if !present || !bytes.Equal(body, want) {
		t.Errorf("read %d bytes (present=%v), want the whole %d", len(body), present,
			len(want))
	}
}

// TestReadWholeDiscardsAReadTheWriterMovedUnderneath is the narrow half of issue
// #8: a prefix, which is what a writer that writes in more than one go leaves
// between two of them.
//
// Constructed rather than hoped for. The file is large — sparse, so it costs no
// disk — which makes the read take milliseconds, and the writer extends it in a
// loop with nothing in it, so the size cannot fail to move while the read is in
// flight. That is the whole of what two stats around a read can detect, and
// readWhole's doc comment is where the rest of that sentence lives.
func TestReadWholeDiscardsAReadTheWriterMovedUnderneath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "batch.psbt")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	const size = 32 << 20
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for n := int64(size + 1); ; n++ {
			select {
			case <-stop:
				return
			default:
			}
			if err := f.Truncate(n); err != nil {
				return
			}
			runtime.Gosched()
		}
	}()

	body, present, err := readWhole(path)
	close(stop)
	<-done

	if err != nil {
		t.Fatalf("readWhole on a growing file: %v", err)
	}
	if !present {
		t.Error("the file is there and readWhole says it is not")
	}
	if body != nil {
		t.Errorf("readWhole handed over %d bytes of a file that was still growing "+
			"under it, which is a prefix of a transaction and decodes as garbage",
			len(body))
	}
}
