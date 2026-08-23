package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AusDavo/winthistle/internal/coldwallet"
)

// tpubs, always. CLAUDE.md forbids a mainnet xpub in a fixture, and .gitignore
// blocks descriptor *files* rather than inline strings, so an xpub pasted into a
// test would sail straight through.
const (
	testReceive = "wsh(sortedmulti(2," +
		"[1b51e4f1/84h/1h/0h]tpubDDcpACxNfy7FEgFAGDz9NXiH1VW4Tci2hFTaYkBf6uCWHXhGgNfKbx6tEQMRnBHt8n3FK8r3dtMeqMW92xmdvjfGL9iS8zNJysCvxraG7A2/0/*," +
		"[4cf33624/84h/1h/0h]tpubDCfUM4YyLJvf8BStcHGDoj7L9c6eXWqtyaK9LYsxCXNrmAxBvN3wHQ2UM77qv2Sb8hskEDDHkEvhDrgvqEu2we3HRMVrsSECvRsQUhS7eMb/0/*))"
	testChange = "wsh(sortedmulti(2," +
		"[1b51e4f1/84h/1h/0h]tpubDDcpACxNfy7FEgFAGDz9NXiH1VW4Tci2hFTaYkBf6uCWHXhGgNfKbx6tEQMRnBHt8n3FK8r3dtMeqMW92xmdvjfGL9iS8zNJysCvxraG7A2/1/*," +
		"[4cf33624/84h/1h/0h]tpubDCfUM4YyLJvf8BStcHGDoj7L9c6eXWqtyaK9LYsxCXNrmAxBvN3wHQ2UM77qv2Sb8hskEDDHkEvhDrgvqEu2we3HRMVrsSECvRsQUhS7eMb/1/*))"
)

func writeDescriptors(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cold.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}

func goodFile() string {
	return "[cold]\nreceive  = \"" + testReceive + "\"\nchange   = \"" + testChange +
		"\"\nbirthday = \"2023-11-14\"\n"
}

func TestADescriptorFileReadsBackWhatItSays(t *testing.T) {
	d, err := LoadDescriptors(writeDescriptors(t, goodFile()))
	if err != nil {
		t.Fatalf("reading a good file: %v", err)
	}
	if d.Receive != testReceive || d.Change != testChange {
		t.Error("the descriptors did not survive the round trip")
	}
	want := time.Date(2023, 11, 14, 0, 0, 0, 0, time.UTC)
	if !d.Birthday.Equal(want) {
		t.Errorf("birthday %s, want %s", d.Birthday, want)
	}
}

// TestTheExampleFileIsReadableIsNotADetail: the example is what an operator
// starts from, and one that does not parse is worse than none.
func TestTheExampleFileIsReadable(t *testing.T) {
	if _, err := LoadDescriptors(writeDescriptors(t, ExampleDescriptors)); err != nil {
		t.Fatalf("`winthistle example-descriptors` does not parse: %v", err)
	}
}

// TestEveryKeyIsRequired. None of the three has a default and none may get one:
// a missing descriptor cannot be guessed, and a missing birthday defaulted to
// "now" produces a wallet that imports cleanly and reports a zero balance
// because it never scanned the blocks the coins arrived in.
func TestEveryKeyIsRequired(t *testing.T) {
	for _, tc := range []struct{ name, key, says string }{
		{"no receive", "receive", "external descriptor"},
		{"no change", "change", "internal descriptor"},
		{"no birthday", "birthday", "where Core starts its rescan"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var kept []string
			for _, line := range strings.Split(goodFile(), "\n") {
				if !strings.HasPrefix(line, tc.key) {
					kept = append(kept, line)
				}
			}
			_, err := LoadDescriptors(writeDescriptors(t, strings.Join(kept, "\n")))
			if err == nil {
				t.Fatalf("a file with no %s was accepted", tc.key)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("the refusal does not say what the key is for: %v", err)
			}
		})
	}
}

// TestGenesisIsSpelledOut. Scanning the whole chain is a legitimate choice and a
// multi-hour one, so it has to be something somebody typed rather than what a
// blank field means.
func TestGenesisIsSpelledOut(t *testing.T) {
	body := strings.Replace(goodFile(), `"2023-11-14"`, `"genesis"`, 1)
	d, err := LoadDescriptors(writeDescriptors(t, body))
	if err != nil {
		t.Fatalf("reading %q as a birthday: %v", GenesisWord, err)
	}
	if !d.Birthday.Equal(coldwallet.Genesis) {
		t.Errorf("birthday %s, want the genesis block", d.Birthday)
	}
}

// TestOneDateFormat. 03/04 is two different days depending on where it was
// written, and a rescan from the wrong one of them finds part of the wallet and
// reports it as all of it. So anything but YYYY-MM-DD is refused rather than
// guessed at.
func TestOneDateFormat(t *testing.T) {
	for _, bad := range []string{"14/11/2023", "11/14/2023", "Nov 2023", "1699920000", "now"} {
		body := strings.Replace(goodFile(), `"2023-11-14"`, `"`+bad+`"`, 1)
		_, err := LoadDescriptors(writeDescriptors(t, body))
		if err == nil {
			t.Errorf("birthday %q was accepted", bad)
			continue
		}
		if !strings.Contains(err.Error(), "YYYY-MM-DD") {
			t.Errorf("the refusal of %q does not say what to write instead: %v", bad, err)
		}
	}
}

// TestAnUnknownKeyIsAnError, for the same reason winthistle.toml's are: this
// file is read once, by one command, and the failure worth engineering against
// is not the malformed file but the well-formed one with a key slightly
// misspelled.
func TestAnUnknownKeyIsAnError(t *testing.T) {
	body := goodFile() + "recieve = \"typo\"\n"
	_, err := LoadDescriptors(writeDescriptors(t, body))
	if err == nil {
		t.Fatal("a misspelled key was ignored")
	}
	if !strings.Contains(err.Error(), "recieve") {
		t.Errorf("the refusal does not name the key: %v", err)
	}
}

func TestAWrongSectionIsNamed(t *testing.T) {
	body := strings.Replace(goodFile(), "[cold]", "[coldwallet]", 1)
	_, err := LoadDescriptors(writeDescriptors(t, body))
	if err == nil {
		t.Fatal("[coldwallet] was accepted as [cold]")
	}
	if !strings.Contains(err.Error(), "coldwallet") {
		t.Errorf("the refusal does not name the section: %v", err)
	}
}
