package lnd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/macaroon.v2"
)

// aMacaroon is the same shape LND writes: one V2 macaroon, marshalled binary.
func aMacaroon(t *testing.T) []byte {
	t.Helper()
	m, err := macaroon.New([]byte("root-key"), []byte("winthistle"),
		"lnd", macaroon.V2)
	if err != nil {
		t.Fatalf("building a macaroon: %v", err)
	}
	b, err := m.MarshalBinary()
	if err != nil {
		t.Fatalf("marshalling a macaroon: %v", err)
	}
	return b
}

// TestAnUnreadableMacaroonIsNamedAsAFile.
//
// The defect this guards is not the failure but the sentence: a short read
// hex-encodes into a well-formed credential carrying the wrong bytes, LND
// refuses it on authentication, and the operator is sent to re-bake — which is
// the one action that re-opens the window the torn read came through.
//
// No node is involved, which is the point of the test. Every case here is
// decided from the bytes on disk, before anything is presented to anybody.
func TestAnUnreadableMacaroonIsNamedAsAFile(t *testing.T) {
	good := aMacaroon(t)

	cases := map[string]struct {
		body []byte
		ok   bool
	}{
		// The wide window: the file sits at zero bytes for the whole gap
		// between the writer's open and its write.
		"empty, which is a bake caught at its start": {body: []byte{}},
		"one byte":                              {body: good[:1]},
		"a truncated prefix of a real macaroon": {body: good[:len(good)/2]},
		"a whole macaroon":                      {body: good, ok: true},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "winthistle.macaroon")
			if err := os.WriteFile(path, tc.body, 0o600); err != nil {
				t.Fatalf("writing the fixture: %v", err)
			}

			got, err := ReadMacaroon(path)
			if tc.ok {
				if err != nil {
					t.Fatalf("a whole macaroon was refused: %v", err)
				}
				if string(got) != string(tc.body) {
					t.Fatalf("the bytes changed on the way through: %d in, %d out",
						len(tc.body), len(got))
				}
				return
			}

			if err == nil {
				t.Fatalf("accepted %d bytes that are not a macaroon", len(tc.body))
			}
			if !errors.Is(err, ErrMacaroonFile) {
				t.Errorf("not marked as a file failure, so a caller cannot tell "+
					"it from an auth failure: %v", err)
			}
			if !strings.Contains(err.Error(), path) {
				t.Errorf("does not name the file, which is the whole point: %v", err)
			}
			// The sentence must not send the operator at the node or at the
			// permission list, because neither has been consulted yet.
			for _, wrong := range []string{"permission denied", "not authorised",
				"too narrow", "re-bake"} {
				if strings.Contains(strings.ToLower(err.Error()), wrong) {
					t.Errorf("asserts a cause it has not established (%q): %v", wrong, err)
				}
			}
		})
	}
}

// TestAMissingMacaroonIsStillNamedAsAFile. The path that already worked, kept
// working: os.ReadFile's own error is wrapped rather than replaced.
func TestAMissingMacaroonIsStillNamedAsAFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nothing-here.macaroon")
	_, err := ReadMacaroon(path)
	if err == nil {
		t.Fatal("a missing file was accepted")
	}
	if !errors.Is(err, ErrMacaroonFile) || !errors.Is(err, os.ErrNotExist) {
		t.Errorf("want both a file failure and os.ErrNotExist, got %v", err)
	}
}
