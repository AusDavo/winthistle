package bitcoind_test

import (
	"context"
	"testing"
	"time"

	"github.com/AusDavo/winthistle/internal/bitcoind"
	"github.com/AusDavo/winthistle/internal/regtestenv"
)

// External test package: internal/regtestenv imports internal/bitcoind, so an
// in-package test here would be an import cycle.

func has(locks []bitcoind.Outpoint, want bitcoind.Outpoint) bool {
	for _, l := range locks {
		if l == want {
			return true
		}
	}
	return false
}

func TestLockAndReleaseAgainstCore(t *testing.T) {
	env := regtestenv.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var unspent []struct {
		TxID string `json:"txid"`
		Vout uint32 `json:"vout"`
	}
	if err := env.Cold.Call(ctx, "listunspent", nil, &unspent); err != nil {
		t.Fatalf("listunspent: %v", err)
	}
	if len(unspent) < 2 {
		t.Skipf("cold-watch has %d utxos, need 2 — run: make -C regtest reset", len(unspent))
	}

	ops := []bitcoind.Outpoint{
		{TxID: unspent[0].TxID, Vout: unspent[0].Vout},
		{TxID: unspent[1].TxID, Vout: unspent[1].Vout},
	}

	// Whatever this test does, leave the wallet as we found it.
	t.Cleanup(func() {
		cleanupCtx, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		_, _ = env.Cold.ReleaseLocks(cleanupCtx, ops)
	})

	locked0, err := env.Cold.LockForRun(ctx, ops)
	if err != nil {
		t.Fatalf("LockForRun: %v", err)
	}
	if len(locked0) != len(ops) {
		t.Fatalf("LockForRun reported %d locked, want %d", len(locked0), len(ops))
	}

	locked, err := env.Cold.ListLocks(ctx)
	if err != nil {
		t.Fatalf("ListLocks: %v", err)
	}
	for _, op := range ops {
		if !has(locked, op) {
			t.Fatalf("%s was not locked; Core reports %v", op, locked)
		}
	}

	freed, err := env.Cold.ReleaseLocks(ctx, ops)
	if err != nil {
		t.Fatalf("ReleaseLocks: %v", err)
	}
	if len(freed) != len(ops) {
		t.Fatalf("ReleaseLocks freed %d, want %d", len(freed), len(ops))
	}

	locked, err = env.Cold.ListLocks(ctx)
	if err != nil {
		t.Fatalf("ListLocks after release: %v", err)
	}
	for _, op := range ops {
		if has(locked, op) {
			t.Fatalf("%s is still locked after release", op)
		}
	}
}

// Releasing an outpoint that was never locked is a no-op, not an error. An abort
// path has to be safe to run twice, and the second run will find nothing to free.
func TestReleaseIsIdempotentAgainstCore(t *testing.T) {
	env := regtestenv.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var unspent []struct {
		TxID string `json:"txid"`
		Vout uint32 `json:"vout"`
	}
	if err := env.Cold.Call(ctx, "listunspent", nil, &unspent); err != nil {
		t.Fatalf("listunspent: %v", err)
	}
	if len(unspent) == 0 {
		t.Skip("cold-watch has no utxos — run: make -C regtest reset")
	}
	op := bitcoind.Outpoint{TxID: unspent[0].TxID, Vout: unspent[0].Vout}

	// Core rejects unlocking an output it does not hold, and validates the whole
	// list before applying any of it — so this only passes because ReleaseLocks
	// filters against the live lock set first.
	for i := 0; i < 2; i++ {
		freed, err := env.Cold.ReleaseLocks(ctx, []bitcoind.Outpoint{op})
		if err != nil {
			t.Fatalf("release attempt %d: %v", i+1, err)
		}
		if len(freed) != 0 {
			t.Fatalf("release attempt %d freed %v, but nothing was locked", i+1, freed)
		}
	}
}

// The regression that motivated the filtering. Core validates the whole list
// before applying any of it, so asking it to release two outpoints when only one
// is locked frees neither — leaving coins held by an abort path that reported
// nothing wrong. This is also what a Core restart mid-run looks like, since
// locks are memory-only.
func TestReleaseFreesWhatItCanWhenSomeLocksAreStale(t *testing.T) {
	env := regtestenv.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var unspent []struct {
		TxID string `json:"txid"`
		Vout uint32 `json:"vout"`
	}
	if err := env.Cold.Call(ctx, "listunspent", nil, &unspent); err != nil {
		t.Fatalf("listunspent: %v", err)
	}
	if len(unspent) < 2 {
		t.Skipf("cold-watch has %d utxos, need 2 — run: make -C regtest reset", len(unspent))
	}
	live := bitcoind.Outpoint{TxID: unspent[0].TxID, Vout: unspent[0].Vout}
	stale := bitcoind.Outpoint{TxID: unspent[1].TxID, Vout: unspent[1].Vout}

	t.Cleanup(func() {
		c, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		_, _ = env.Cold.ReleaseLocks(c, []bitcoind.Outpoint{live, stale})
	})

	if _, err := env.Cold.LockForRun(ctx, []bitcoind.Outpoint{live}); err != nil {
		t.Fatalf("locking the live outpoint: %v", err)
	}

	// stale was never locked. The journal would name both.
	freed, err := env.Cold.ReleaseLocks(ctx, []bitcoind.Outpoint{live, stale})
	if err != nil {
		t.Fatalf("release with one stale entry failed outright: %v", err)
	}
	if len(freed) != 1 || freed[0] != live {
		t.Fatalf("freed %v, want exactly %s", freed, live)
	}

	locked, err := env.Cold.ListLocks(ctx)
	if err != nil {
		t.Fatalf("ListLocks: %v", err)
	}
	if has(locked, live) {
		t.Fatalf("%s is still locked; Core reports %v", live, locked)
	}
}

// Locking is filtered the same way, and for the same reason: Core rejects a list
// containing an already-locked output, which a re-armed batch would produce.
func TestLockSkipsWhatIsAlreadyLocked(t *testing.T) {
	env := regtestenv.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var unspent []struct {
		TxID string `json:"txid"`
		Vout uint32 `json:"vout"`
	}
	if err := env.Cold.Call(ctx, "listunspent", nil, &unspent); err != nil {
		t.Fatalf("listunspent: %v", err)
	}
	if len(unspent) == 0 {
		t.Skip("cold-watch has no utxos — run: make -C regtest reset")
	}
	op := bitcoind.Outpoint{TxID: unspent[0].TxID, Vout: unspent[0].Vout}
	ops := []bitcoind.Outpoint{op}

	t.Cleanup(func() {
		c, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		_, _ = env.Cold.ReleaseLocks(c, ops)
	})

	if _, err := env.Cold.LockForRun(ctx, ops); err != nil {
		t.Fatalf("first lock: %v", err)
	}
	again, err := env.Cold.LockForRun(ctx, ops)
	if err != nil {
		t.Fatalf("second lock should be a no-op, got: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("second lock reported %v as newly locked", again)
	}
}
