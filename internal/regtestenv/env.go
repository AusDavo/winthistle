// Package regtestenv wires tests up to the regtest/ harness.
//
// Test support only. If the harness is not running, every helper here skips the
// test rather than failing it, so `go test ./...` stays useful on a machine with
// no Docker — but a skip is reported with a reason, so a harness that is down is
// never mistaken for a suite that passed.
//
// Start the harness with `make -C regtest reset`.
package regtestenv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AusDavo/winthistle/internal/bitcoind"
	"github.com/AusDavo/winthistle/internal/lnd"
)

// Harness addresses, fixed by regtest/docker-compose.yml's port mappings.
const (
	coreAddr  = "127.0.0.1:18443"
	aliceAddr = "127.0.0.1:10009"

	// ColdWallet is the watch-only 2-of-2 wallet — the stand-in for cold
	// storage, holding no private keys.
	ColdWallet = "cold-watch"

	// MinerWallet holds keys and mines. Tests use it to fund fixtures and to
	// advance the chain.
	MinerWallet = "miner"
)

// Env is a live harness.
type Env struct {
	Root  string // repo root
	Alice *lnd.Client
	Node  *bitcoind.Client // no wallet scope, for node-level calls
	Cold  *bitcoind.Client // cold-watch, watch-only
	Miner *bitcoind.Client // miner, holds keys

	rpcUser, rpcPass string
}

// WalletClient builds a Core client bound to a wallet by name.
//
// The wallet does not have to exist: bitcoind.New only assembles a URL, so this
// is how a test drives a wallet it is about to create. timeout may be zero for
// bitcoind.DefaultTimeout; a descriptor import wants far more than that on a
// real chain, and nothing else does.
func (e *Env) WalletClient(t *testing.T, name string, timeout time.Duration) *bitcoind.Client {
	t.Helper()
	c, err := bitcoind.New(bitcoind.Config{
		Address: coreAddr, User: e.rpcUser, Pass: e.rpcPass, Wallet: name,
		Timeout: timeout,
	})
	if err != nil {
		t.Fatalf("building a core client for %s: %v", name, err)
	}
	return c
}

// repoRoot walks up from the test's working directory looking for go.mod.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod above %s", dir)
		}
		dir = parent
	}
}

// rpcCreds reads the harness's deliberately-fake regtest credentials out of
// regtest/.env rather than hardcoding them here, so there is one place to change.
func rpcCreds(root string) (user, pass string, err error) {
	raw, err := os.ReadFile(filepath.Join(root, "regtest", ".env"))
	if err != nil {
		return "", "", err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "RPCUSER":
			user = v
		case "RPCPASS":
			pass = v
		}
	}
	if user == "" || pass == "" {
		return "", "", fmt.Errorf("regtest/.env has no RPCUSER/RPCPASS")
	}
	return user, pass, nil
}

// Start connects to the harness, or skips the test explaining how to bring it up.
func Start(t *testing.T) *Env {
	t.Helper()

	if testing.Short() {
		t.Skip("harness-backed test; -short was given")
	}

	root, err := repoRoot()
	if err != nil {
		t.Fatalf("locating repo root: %v", err)
	}

	certPath := filepath.Join(root, "regtest", "creds", "alice", "tls.cert")
	macPath := filepath.Join(root, "regtest", "creds", "alice", "admin.macaroon")
	for _, p := range []string{certPath, macPath} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("harness credentials missing (%s) — run: make -C regtest reset", p)
		}
	}

	user, pass, err := rpcCreds(root)
	if err != nil {
		t.Skipf("harness not configured: %v — run: make -C regtest reset", err)
	}

	newCore := func(wallet string) *bitcoind.Client {
		c, err := bitcoind.New(bitcoind.Config{
			Address: coreAddr, User: user, Pass: pass, Wallet: wallet,
		})
		if err != nil {
			t.Fatalf("building core client for %s: %v", wallet, err)
		}
		return c
	}
	node := newCore("")
	cold, miner := newCore(ColdWallet), newCore(MinerWallet)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var height int64
	if err := node.Call(ctx, "getblockcount", nil, &height); err != nil {
		t.Skipf("bitcoind at %s not answering: %v — run: make -C regtest reset", coreAddr, err)
	}

	// Core does not auto-load non-default wallets, so a plain `docker compose
	// start` — or any Core restart — leaves them on disk but unloaded, and every
	// wallet-scoped call then fails with "Requested wallet does not exist or is
	// not loaded". Loading here keeps a restarted harness usable without a full
	// reset. Worth knowing beyond the tests: the same is true of a real node,
	// and it is one of the states the abort path has to be able to run in.
	for _, w := range []string{MinerWallet, ColdWallet} {
		if err := ensureWalletLoaded(ctx, node, w); err != nil {
			t.Skipf("wallet %q unavailable: %v — run: make -C regtest reset", w, err)
		}
	}

	// The harness's own admin.macaroon: fine for regtest. Production uses the
	// narrow baked credential — see `print-macaroon-command`.
	alice, err := lnd.Dial(ctx, lnd.Config{
		Address: aliceAddr, TLSCert: certPath, Macaroon: macPath,
	})
	if err != nil {
		t.Skipf("alice at %s not answering: %v — run: make -C regtest reset", aliceAddr, err)
	}
	t.Cleanup(func() { alice.Close() })

	return &Env{Root: root, Alice: alice, Node: node, Cold: cold, Miner: miner,
		rpcUser: user, rpcPass: pass}
}

// ensureWalletLoaded loads a wallet if Core does not already have it open.
func ensureWalletLoaded(ctx context.Context, node *bitcoind.Client, name string) error {
	err := node.Call(ctx, "loadwallet", []any{name}, nil)
	if err == nil {
		return nil
	}
	// Already loaded is the state we wanted. Core reports it as RPC_WALLET_ALREADY_LOADED.
	var rpcErr *bitcoind.Error
	if errors.As(err, &rpcErr) && rpcErr.Code == -35 {
		return nil
	}
	if strings.Contains(err.Error(), "already loaded") {
		return nil
	}
	return err
}

// Mine advances the chain by n blocks, paying the miner wallet.
func (e *Env) Mine(t *testing.T, n int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var addr string
	if err := e.Miner.Call(ctx, "getnewaddress", nil, &addr); err != nil {
		t.Fatalf("miner getnewaddress: %v", err)
	}
	if err := e.Node.Call(ctx, "generatetoaddress", []any{n, addr}, nil); err != nil {
		t.Fatalf("generatetoaddress %d: %v", n, err)
	}
}

// InMempool reports whether Core has that txid in its mempool.
//
// This is how the tests check that nothing was broadcast. getmempoolentry errors
// for an absent transaction, so a "not in mempool" error is the expected answer
// and is distinguished from a transport failure by its message.
func (e *Env) InMempool(t *testing.T, txid string) bool {
	t.Helper()
	present, err := e.TryInMempool(txid)
	if err != nil {
		t.Fatalf("getmempoolentry %s: %v", txid, err)
	}
	return present
}

// TryInMempool is InMempool for a caller that must not fail the test itself.
//
// A polling goroutine is the case: t.Fatalf there would run runtime.Goexit on
// the wrong goroutine, so a transport failure has to come back as a value and be
// asserted on by whoever owns the test.
func (e *Env) TryInMempool(txid string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var raw json.RawMessage
	err := e.Node.Call(ctx, "getmempoolentry", []any{txid}, &raw)
	if err == nil {
		return true, nil
	}
	if strings.Contains(err.Error(), "not in mempool") ||
		strings.Contains(err.Error(), "Transaction not in mempool") {
		return false, nil
	}
	return false, err
}
