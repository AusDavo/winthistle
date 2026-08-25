// Package signetenv wires tests up to the signet/ harness.
//
// Test support only, and it exists for exactly two things regtest cannot reach,
// both of which are Core's rather than LND's:
//
//   - coldwallet.Import's rescan. Regtest has no history, so a wallet imported
//     with the right birthday and one imported with a wrong one find precisely
//     the same nothing. Signet has six years of it.
//   - coldwallet.PrunedPastBirthday, and doctor's prune-horizon warning. Neither
//     can fire on a node that has never thrown a block away.
//
// So there is no LND here — setup.Deps has no LND field — and no channels, no
// peers and no batch. Two bitcoinds.
//
// Every helper skips rather than fails, the way regtestenv does, and there is
// one skip more: this harness costs a real block download, so it is off unless
// WINTHISTLE_SIGNET=1 is in the environment. `make test` on a machine that has
// never run signet/ stays green, and a skip always says why.
//
// Start the harness with `make -C signet up`, wait for `make -C signet sync` to
// report ibd=False on both nodes, then `make -C signet cold` and fund the
// address it prints from a faucet.
package signetenv

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AusDavo/winthistle/internal/bitcoind"
)

// Harness addresses, fixed by signet/docker-compose.yml's port mappings.
const (
	unprunedAddr = "127.0.0.1:38332"
	prunedAddr   = "127.0.0.1:38352"

	// ColdWallet is the watch-only 2-of-2 wallet signet/cold-wallet.py builds —
	// the stand-in for cold storage, holding no private keys. It is the only
	// wallet in this harness that matters, and it lives on the unpruned node.
	ColdWallet = "cold-watch"

	// EnvVar switches these tests on. Off by default because the harness costs
	// a signet initial block download, which no other test in this repository
	// asks anyone to pay for.
	EnvVar = "WINTHISTLE_SIGNET"
)

// Env is a live signet harness.
type Env struct {
	Root string // repo root

	// Node is the unpruned node with no wallet scope. This is the one a rescan
	// runs against, because it is the only one that still holds the blocks.
	Node *bitcoind.Client

	// Cold is ColdWallet on the unpruned node: the descriptors and the coin.
	Cold *bitcoind.Client

	// Pruned is the pruned node, no wallet scope. It holds no coins and needs
	// none — all it has to do is report a pruneheight.
	Pruned *bitcoind.Client

	rpcUser, rpcPass string
}

// WalletClient builds a Core client on the unpruned node bound to a wallet by
// name. The wallet does not have to exist: this is how a test drives a wallet it
// is about to create.
//
// timeout may be zero for bitcoind.DefaultTimeout. A descriptor import blocks
// for the whole rescan, and on this chain that is the point — give it minutes.
func (e *Env) WalletClient(t *testing.T, name string, timeout time.Duration) *bitcoind.Client {
	t.Helper()
	c, err := bitcoind.New(bitcoind.Config{
		Address: unprunedAddr, User: e.rpcUser, Pass: e.rpcPass, Wallet: name,
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

// rpcCreds reads the harness's deliberately-fake credentials out of signet/.env
// rather than hardcoding them, so there is one place to change.
func rpcCreds(root string) (user, pass string, err error) {
	raw, err := os.ReadFile(filepath.Join(root, "signet", ".env"))
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
		return "", "", fmt.Errorf("signet/.env has no RPCUSER/RPCPASS")
	}
	return user, pass, nil
}

// Start connects to the harness, or skips the test explaining how to bring it up.
//
// It refuses a node that is still in initial block download rather than working
// around it, and the reason is the thing under test: a rescan against a chain
// that is still arriving finds whatever has arrived, silently. A birthday test
// run against a half-downloaded chain would be measuring the download.
func Start(t *testing.T) *Env {
	t.Helper()

	if testing.Short() {
		t.Skip("harness-backed test; -short was given")
	}
	if os.Getenv(EnvVar) == "" {
		t.Skipf("signet-backed test; set %s=1 to run it. The harness is a real "+
			"block download — see signet/README.md", EnvVar)
	}

	root, err := repoRoot()
	if err != nil {
		t.Fatalf("locating repo root: %v", err)
	}
	user, pass, err := rpcCreds(root)
	if err != nil {
		t.Skipf("signet harness not configured: %v", err)
	}

	e := &Env{Root: root, rpcUser: user, rpcPass: pass}
	newCore := func(addr, wallet string) *bitcoind.Client {
		c, err := bitcoind.New(bitcoind.Config{
			Address: addr, User: user, Pass: pass, Wallet: wallet,
			Timeout: 10 * time.Minute,
		})
		if err != nil {
			t.Fatalf("building a core client for %s/%s: %v", addr, wallet, err)
		}
		return c
	}
	e.Node = newCore(unprunedAddr, "")
	e.Cold = newCore(unprunedAddr, ColdWallet)
	e.Pruned = newCore(prunedAddr, "")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	for _, n := range []struct {
		what   string
		client *bitcoind.Client
		pruned bool
	}{
		{"the unpruned node", e.Node, false},
		{"the pruned node", e.Pruned, true},
	} {
		info, err := n.client.GetBlockchainInfo(ctx)
		if err != nil {
			t.Skipf("%s is not answering: %v — run: make -C signet up", n.what, err)
		}
		if info.Chain != "signet" {
			t.Skipf("%s is on %s, not signet", n.what, info.Chain)
		}
		if info.InitialBlockDownload {
			t.Skipf("%s is still downloading the chain (%.2f%% verified, height %d). "+
				"A rescan against a chain that is still arriving finds whatever has "+
				"arrived, silently — watch it with: make -C signet sync",
				n.what, info.VerificationProgress*100, info.Blocks)
		}
		if n.pruned && !info.Pruned {
			t.Skipf("%s reports it is not pruned, so there is no horizon to fire on "+
				"— check -prune in signet/docker-compose.yml", n.what)
		}
	}

	// Core does not auto-load non-default wallets, so any restart leaves this one
	// on disk but unloaded and every wallet-scoped call fails with "Requested
	// wallet does not exist or is not loaded".
	if err := e.Node.EnsureWalletLoaded(ctx, ColdWallet); err != nil {
		t.Skipf("the %s wallet is unavailable: %v — run: make -C signet cold",
			ColdWallet, err)
	}
	return e
}

// Descriptors reads the pair signet/cold-wallet.py built, off the wallet rather
// than out of a fixture, so the two cannot drift.
func (e *Env) Descriptors(t *testing.T, ctx context.Context) (receive, change string) {
	t.Helper()
	descs, err := e.Cold.ListDescriptors(ctx)
	if err != nil {
		t.Fatalf("listing %s's descriptors: %v", ColdWallet, err)
	}
	for _, d := range descs {
		switch {
		case d.Active && !d.Internal:
			receive = d.Desc
		case d.Active && d.Internal:
			change = d.Desc
		}
	}
	if receive == "" || change == "" {
		t.Fatalf("%s has no active descriptor pair — run: make -C signet cold", ColdWallet)
	}
	return receive, change
}

// Coin is the faucet payment the cold wallet holds, and the height it landed at.
//
// That height is what this whole harness is for. It is the pivot both birthday
// tests turn on: one birthday before this block, which finds the coin, and one
// after it, which finds nothing and looks identical.
type Coin struct {
	TxID      string
	Vout      uint32
	AmountSat int64
	Height    int64

	// Time is the block header time, not the wall clock. Core compares an import
	// timestamp against header times, so this is the only number a birthday can
	// honestly be placed either side of.
	Time time.Time
}

// coreTimestampWindow is the slack Core allows itself around an import
// timestamp. importdescriptors does not start scanning at the first block later
// than the timestamp — it winds back TIMESTAMP_WINDOW first, so that a wallet
// whose clock disagreed with the chain's still finds its coins.
//
// It is 2 hours in Core 29 (TIMESTAMP_WINDOW, src/wallet/wallet.h). Any test
// placing a birthday "after" a block has to clear it, or the rescan reaches back
// over the block anyway and the test proves the opposite of what it says.
const coreTimestampWindow = 2 * time.Hour

// coins returns every confirmed UTXO in the cold wallet, dated.
func (e *Env) coins(t *testing.T, ctx context.Context) []Coin {
	t.Helper()

	utxos, err := e.Cold.ListUnspent(ctx, 1, 9_999_999)
	if err != nil {
		t.Fatalf("listing %s's coins: %v", ColdWallet, err)
	}
	if len(utxos) == 0 {
		t.Skipf("%s holds no confirmed coins. This harness needs one faucet "+
			"payment: run `make -C signet address` and send signet coins to it",
			ColdWallet)
	}

	var out []Coin
	for _, u := range utxos {
		var tx struct {
			BlockHeight int64 `json:"blockheight"`
			BlockTime   int64 `json:"blocktime"`
		}
		if err := e.Cold.Call(ctx, "gettransaction", []any{u.TxID}, &tx); err != nil {
			t.Fatalf("dating %s: %v", u.TxID, err)
		}
		if tx.BlockHeight == 0 {
			continue
		}
		out = append(out, Coin{
			TxID: u.TxID, Vout: u.Vout, AmountSat: int64(math.Round(u.Amount * 1e8)),
			Height: tx.BlockHeight, Time: time.Unix(tx.BlockTime, 0).UTC(),
		})
	}
	if len(out) == 0 {
		t.Skipf("%s's coins are all unconfirmed — wait for a block", ColdWallet)
	}
	return out
}

// OldestCoin returns the earliest-confirmed UTXO in the cold wallet.
//
// It carries no requirement about the coin's age, because the rescan test does
// not have one: a birthday thirty days before a coin that confirmed ten minutes
// ago is still a birthday thirty days before it. Only the too-late test needs
// the coin to be old, and it says so itself — see
// RequireClearOfTheTimestampWindow.
func (e *Env) OldestCoin(t *testing.T, ctx context.Context) Coin {
	t.Helper()
	all := e.coins(t, ctx)
	oldest := all[0]
	for _, c := range all[1:] {
		if c.Height < oldest.Height {
			oldest = c
		}
	}
	return oldest
}

// RequireClearOfTheTimestampWindow skips unless every coin in the cold wallet is
// old enough that a birthday of *today* lands cleanly after it.
//
// Only the too-late-birthday test needs this, and it needs it absolutely: Core
// winds a rescan back coreTimestampWindow from the import timestamp, so a coin
// that confirmed an hour ago is found by a birthday of today and the test proves
// the opposite of what it says.
//
// It checks every coin rather than the oldest, which is the part that is easy to
// get wrong. A second faucet payment is an ordinary thing for somebody to do to
// this harness, and one made an hour ago would be found while the original coin
// sat safely in the past.
//
// The skip is not a flake and not a defect. While a fresh coin is sitting there,
// this fixture genuinely cannot tell a too-late birthday from a correct one, and
// it is a one-off: once the coins are a few hours old they stay that way.
func (e *Env) RequireClearOfTheTimestampWindow(t *testing.T, ctx context.Context) {
	t.Helper()
	for _, c := range e.coins(t, ctx) {
		if age := time.Since(c.Time); age < coreTimestampWindow+time.Hour {
			t.Skipf("%s holds a coin that confirmed %s ago (height %d), inside "+
				"Core's %s import timestamp window. A birthday of today would be "+
				"wound back over it and find it, so this test could not tell a "+
				"too-late birthday from a correct one. Wait it out — nothing is "+
				"wrong, and it only happens once per coin", ColdWallet,
				age.Round(time.Minute), c.Height, coreTimestampWindow)
		}
	}
}

// TimestampWindow is Core's import-timestamp slack, exported so a test can say
// out loud how much clearance it is giving itself.
func TimestampWindow() time.Duration { return coreTimestampWindow }

// PrunedConfigFile writes a winthistle.toml pointing Core at the *pruned* node
// and returns its path. It is for `winthistle doctor`, which is the other reader
// of a prune horizon and has its own copy of the reasoning.
//
// The LND section names files that do not exist. That is deliberate and it is
// the honest shape of this harness: there is no signet LND, so doctor's LND and
// macaroon checks are expected to fail and a test using this must assert on the
// Bitcoin Core check alone. Writing a config that pretended otherwise would make
// the failure look like a defect rather than the absence it is.
func (e *Env) PrunedConfigFile(t *testing.T, dir string) string {
	t.Helper()

	body := fmt.Sprintf(`[lnd]
address  = "127.0.0.1:10009"
tls_cert = %q
macaroon = %q

[bitcoind]
address = %q
user    = %q
pass    = %q
wallet  = %q

[server]
bind    = "127.0.0.1:7420"
journal = %q

[limits]
abort_after_signing_seconds = 300

[fees]
floor_sat_per_vb = 1.0

[[signer]]
label = "cold1"

[[signer]]
label = "cold2"
`,
		filepath.Join(dir, "there-is-no-signet-lnd.cert"),
		filepath.Join(dir, "there-is-no-signet-lnd.macaroon"),
		prunedAddr, e.rpcUser, e.rpcPass, ColdWallet,
		filepath.Join(dir, "runs.db"))

	path := filepath.Join(dir, "winthistle.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}
