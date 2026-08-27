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

// TestReadWholeSaysNothingIsThereWhenNothingIsThere. The state is what tells
// "the wallet has not saved yet" apart from the two shapes of "not finished",
// and only the latter are worth printing a line about — in the words of whichever
// one it was, since fileEmpty has seen no writer at all.
func TestReadWholeSaysNothingIsThereWhenNothingIsThere(t *testing.T) {
	body, state, err := readWhole(filepath.Join(t.TempDir(), "not-there.psbt"))
	if err != nil {
		t.Fatalf("readWhole on an absent path: %v", err)
	}
	if body != nil || state != fileAbsent {
		t.Errorf("an absent path reported state=%v with %d bytes", state, len(body))
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
	body, state, err := readWhole(path)
	if err != nil {
		t.Fatalf("readWhole on an empty file: %v", err)
	}
	if state != fileEmpty {
		t.Errorf("an empty file reported state=%v; the wait would either never say "+
			"it is holding off on it, or say it grew, which nothing observed", state)
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
	body, state, err := readWhole(path)
	if err != nil {
		t.Fatalf("readWhole: %v", err)
	}
	if state != fileWhole || !bytes.Equal(body, want) {
		t.Errorf("read %d bytes (state=%v), want the whole %d", len(body), state,
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

	body, state, err := readWhole(path)
	close(stop)
	<-done

	if err != nil {
		t.Fatalf("readWhole on a growing file: %v", err)
	}
	if state != fileGrew {
		t.Errorf("a growing file reported state=%v, so the screen would not say "+
			"the read came back as a prefix", state)
	}
	if body != nil {
		t.Errorf("readWhole handed over %d bytes of a file that was still growing "+
			"under it, which is a prefix of a transaction and decodes as garbage",
			len(body))
	}
}
