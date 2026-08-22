package bitcoind

import (
	"context"
	"strings"
	"testing"
)

// The empty-list guard has to hold without a node behind it: Core reads
// `lockunspent true` with no outpoints as unlock-everything, so the refusal must
// happen before the call is ever made.
func TestReleaseLocksRefusesEmptyList(t *testing.T) {
	// Deliberately unreachable: if the guard leaks, this becomes a connection
	// error instead of the refusal, and the test fails either way.
	c, err := New(Config{Address: "127.0.0.1:1", User: "u", Pass: "p", Wallet: "w"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for _, ops := range [][]Outpoint{nil, {}} {
		_, err := c.ReleaseLocks(context.Background(), ops)
		if err == nil {
			t.Fatal("ReleaseLocks with no outpoints must refuse")
		}
		if !strings.Contains(err.Error(), "unlock-everything") {
			t.Fatalf("refused for the wrong reason: %v", err)
		}
	}

	if _, err := c.LockForRun(context.Background(), nil); err == nil {
		t.Fatal("LockForRun with no outpoints must refuse")
	}
}
