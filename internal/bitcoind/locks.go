package bitcoind

import (
	"context"
	"fmt"
)

// Outpoint is a Core-shaped outpoint: txid as hex in RPC byte order, plus index.
type Outpoint struct {
	TxID string `json:"txid"`
	Vout uint32 `json:"vout"`
}

func (o Outpoint) String() string { return fmt.Sprintf("%s:%d", o.TxID, o.Vout) }

// ListLocks returns every currently locked outpoint in the wallet.
//
// Worth exposing beyond the abort path: a crash between locking coins and
// writing the journal leaves locks with no record of who owns them, and this is
// how `winthistle doctor` shows the operator what is being held.
func (c *Client) ListLocks(ctx context.Context) ([]Outpoint, error) {
	var out []Outpoint
	if err := c.Call(ctx, "listlockunspent", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// heldLocks returns the current lock set as a lookup.
func (c *Client) heldLocks(ctx context.Context) (map[Outpoint]bool, error) {
	locked, err := c.ListLocks(ctx)
	if err != nil {
		return nil, fmt.Errorf("checking which coins are locked: %w", err)
	}
	held := make(map[Outpoint]bool, len(locked))
	for _, l := range locked {
		held[l] = true
	}
	return held, nil
}

// Two Core behaviours shape both functions below, and both were confirmed
// against the regtest harness rather than inferred:
//
//  1. `lockunspent true` with an empty outpoint list means unlock *everything*
//     in the wallet.
//  2. lockunspent validates the entire list before applying any of it. An entry
//     already in the requested state fails the whole call — "Invalid parameter,
//     expected locked output" when unlocking, "output already locked" when
//     locking — and nothing is changed. One stale entry therefore frees nothing
//     at all.
//
// Together those make the naive implementation actively dangerous for an abort
// path. Core's locks are memory-only, so a Core restart mid-run turns every
// entry in the journal stale at once; without filtering, the release meant to
// clean up after that restart fails on the first stale entry and leaves every
// genuinely-locked coin held. Filtering against the live lock set is what makes
// these safe to call twice, and safe to call after a restart.

// LockForRun marks the batch's chosen inputs unspendable so nothing else in the
// wallet selects them mid-run, and reports which ones it actually locked.
// Inputs already locked are left alone.
//
// The lock is deliberately non-persistent. A lock that dies with the Core
// process means a crashed run self-heals on the operator's next restart, rather
// than leaving a wallet that quietly refuses to spend its own coins with no
// surviving record of why. The run journal is what makes the lock releasable
// deliberately; process exit is the backstop for when it is not.
func (c *Client) LockForRun(ctx context.Context, ops []Outpoint) ([]Outpoint, error) {
	if len(ops) == 0 {
		return nil, fmt.Errorf("lock: no outpoints given")
	}
	held, err := c.heldLocks(ctx)
	if err != nil {
		return nil, err
	}
	var todo []Outpoint
	for _, op := range ops {
		if !held[op] {
			todo = append(todo, op)
			held[op] = true // also guards a duplicate within ops
		}
	}
	if len(todo) == 0 {
		return nil, nil
	}
	if err := c.lockunspent(ctx, false, todo); err != nil {
		return nil, err
	}
	return todo, nil
}

// ReleaseLocks unlocks those of ops that Core currently holds, and reports which
// ones it actually freed.
//
// It refuses an empty list outright rather than passing it through, because Core
// would read that as unlock-everything and release locks belonging to the
// operator or to another tool. A nil slice here is a bug in the caller, not an
// instruction to unlock the wallet. Naming outpoints that are not locked is
// fine and frees nothing — that is the already-clean case, and it is not an error.
func (c *Client) ReleaseLocks(ctx context.Context, ops []Outpoint) ([]Outpoint, error) {
	if len(ops) == 0 {
		return nil, fmt.Errorf("release: no outpoints given — refusing, because " +
			"Core would read that as unlock-everything")
	}
	held, err := c.heldLocks(ctx)
	if err != nil {
		return nil, err
	}
	var todo []Outpoint
	for _, op := range ops {
		if held[op] {
			todo = append(todo, op)
			held[op] = false
		}
	}
	if len(todo) == 0 {
		return nil, nil
	}
	if err := c.lockunspent(ctx, true, todo); err != nil {
		return nil, err
	}
	return todo, nil
}

func (c *Client) lockunspent(ctx context.Context, unlock bool, ops []Outpoint) error {
	var ok bool
	if err := c.Call(ctx, "lockunspent", []any{unlock, ops}, &ok); err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("lockunspent(unlock=%v) returned false for %d outpoint(s)",
			unlock, len(ops))
	}
	return nil
}
