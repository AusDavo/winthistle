package coldwallet_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/AusDavo/winthistle/internal/bitcoind"
	"github.com/AusDavo/winthistle/internal/coldwallet"
	"github.com/AusDavo/winthistle/internal/regtestenv"
)

// What regtest cannot prove here, said once rather than implied by omission.
//
// Every test in this file exercises the descriptor lifecycle: create, checksum,
// import, read back, derive, compare. None of them exercises the rescan, because
// regtest has no history to rescan — a wallet imported with the right birthday
// and one imported with a wrong one find exactly the same nothing. The pruning
// pre-flight is in the same position: this node is not pruned and cannot be made
// meaningfully pruned. Both need signet, per CLAUDE.md.
const notProvedHere = "regtest has no chain history, so the rescan and the prune " +
	"horizon are untested here — that needs signet"

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// harnessDescriptors reads the descriptors regtest/cold-wallet.py built, so the
// fixture cannot drift from the harness.
func harnessDescriptors(t *testing.T, env *regtestenv.Env) (receive, change string) {
	t.Helper()
	descs, err := env.Cold.ListDescriptors(testCtx(t))
	if err != nil {
		t.Fatalf("listing %s's descriptors: %v", regtestenv.ColdWallet, err)
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
		t.Fatalf("%s has no active descriptor pair — run: make harness", regtestenv.ColdWallet)
	}
	return receive, change
}

// newWallet builds a client for a wallet that may not exist yet, and unloads it
// afterwards so a re-run starts from Core's on-disk copy rather than from a
// wallet this process left open.
func newWallet(t *testing.T, env *regtestenv.Env, name string) *bitcoind.Client {
	t.Helper()
	// A descriptor import blocks for the whole rescan. On regtest that is
	// instant; on mainnet it is hours, which is the reason the knob exists.
	c := env.WalletClient(t, name, 10*time.Minute)
	t.Cleanup(func() {
		c, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		_ = env.Node.Call(c, "unloadwallet", []any{name}, nil)
	})
	return c
}

// TestInstallBuildsTheWalletAndEndsInAQuestion walks the whole lifecycle against
// a live Core and checks what actually landed, rather than that the calls
// returned.
func TestInstallBuildsTheWalletAndEndsInAQuestion(t *testing.T) {
	env := regtestenv.Start(t)
	ctx := testCtx(t)
	receive, change := harnessDescriptors(t, env)

	const name = "winthistle-install-test"
	wallet := newWallet(t, env, name)

	cfg := coldwallet.Config{
		WalletName: name,
		Receive:    receive,
		Change:     change,
		Birthday:   coldwallet.Genesis,
		GapLimit:   50,
	}
	result, err := coldwallet.Install(ctx, env.Node, wallet, cfg)
	if err != nil {
		t.Fatalf("installing the watch-only wallet: %v", err)
	}
	t.Log(notProvedHere)

	if got := result.Verdict(); got != coldwallet.AwaitingAddressCheck {
		t.Fatalf("Install reported %v; the only outcome it may report is a question", got)
	}

	// What landed, not what was asked for.
	if result.Landed.Info.PrivateKeysEnabled {
		t.Error("the wallet has private keys enabled — I-2 says this process must " +
			"be structurally incapable of signing")
	}
	if !result.Landed.Info.Descriptors {
		t.Error("the wallet is not a descriptor wallet")
	}
	if result.Landed.Receive.Internal || !result.Landed.Receive.Active {
		t.Errorf("the receive descriptor landed as %+v", result.Landed.Receive)
	}
	if !result.Landed.Change.Internal || !result.Landed.Change.Active {
		t.Errorf("the change descriptor landed as %+v", result.Landed.Change)
	}
	if got, want := len(result.Check.Receive), coldwallet.DefaultSampleSize; got != want {
		t.Errorf("derived %d receive addresses, want %d", got, want)
	}
	if !result.Check.Consistent() {
		t.Errorf("Core does not recognise its own derived addresses:\n%s",
			result.Check.Report())
	}

	// The round trip that matters: this wallet's addresses have to be the same
	// ones the harness's cold wallet already uses. cold-watch was built by
	// regtest/cold-wallet.py from the private keys, and holds real coins.
	for i, d := range result.Check.Receive {
		info, err := env.Cold.GetAddressInfo(ctx, d.Address)
		if err != nil {
			t.Fatalf("asking %s about receive address %d: %v", regtestenv.ColdWallet, i, err)
		}
		if !info.IsMine {
			t.Errorf("receive address %d (%s) is not an address of %s. The import "+
				"landed a different wallet from the one the keys belong to.",
				i, d.Address, regtestenv.ColdWallet)
		}
	}
	for i, d := range result.Check.Change {
		info, err := env.Cold.GetAddressInfo(ctx, d.Address)
		if err != nil {
			t.Fatalf("asking %s about change address %d: %v", regtestenv.ColdWallet, i, err)
		}
		if !info.IsMine {
			t.Errorf("change address %d (%s) is not an address of %s", i, d.Address,
				regtestenv.ColdWallet)
		}
	}

	report := result.Report()
	if strings.Contains(strings.ToLower(report), "success") {
		t.Errorf("setup reports success rather than asking the operator to compare "+
			"addresses:\n%s", report)
	}
	if !strings.Contains(report, "your own wallet software") {
		t.Errorf("setup does not ask for the comparison:\n%s", report)
	}
	t.Logf("\n%s", report)
}

// TestSortedMultiAndMultiAgreeOftenEnoughToFoolYou is the second setup trap,
// demonstrated rather than described — and it turns out to be worse than the
// design describes.
//
// sortedmulti sorts the derived pubkeys; multi keeps the order they were written
// in. At any index where the derived keys already happen to be in ascending
// order the two agree, which for two keys is about half the indices. So the two
// descriptors do not simply produce different wallets: they produce wallets that
// agree at some indices and not others, and a wrong one can show a correct first
// address.
//
// That is why the address check derives several addresses rather than one.
func TestSortedMultiAndMultiAgreeOftenEnoughToFoolYou(t *testing.T) {
	env := regtestenv.Start(t)
	ctx := testCtx(t)
	receive, _ := harnessDescriptors(t, env)

	// Strip the checksum, swap the function, let Core recompute.
	body := receive
	if i := strings.LastIndex(body, "#"); i > 0 {
		body = body[:i]
	}
	wrong := strings.Replace(body, "sortedmulti(", "multi(", 1)
	if wrong == body {
		t.Fatalf("the harness descriptor is not a sortedmulti: %s", body)
	}

	right, err := env.Node.DescribeDescriptor(ctx, body)
	if err != nil {
		t.Fatalf("Core would not parse the sortedmulti descriptor: %v", err)
	}
	other, err := env.Node.DescribeDescriptor(ctx, wrong)
	if err != nil {
		t.Fatalf("Core refused the multi() descriptor, so the trap does not exist "+
			"in this form: %v", err)
	}

	const sample = 20
	rightAddrs, err := env.Node.DeriveAddresses(ctx, right.Descriptor, 0, sample-1)
	if err != nil {
		t.Fatalf("deriving from sortedmulti: %v", err)
	}
	wrongAddrs, err := env.Node.DeriveAddresses(ctx, other.Descriptor, 0, sample-1)
	if err != nil {
		t.Fatalf("deriving from multi: %v", err)
	}

	var agreed, differed int
	firstDifference := -1
	for i := range rightAddrs {
		if rightAddrs[i] == wrongAddrs[i] {
			agreed++
			continue
		}
		differed++
		if firstDifference < 0 {
			firstDifference = i
		}
	}
	if differed == 0 {
		t.Fatalf("sortedmulti and multi agreed on all %d addresses. If that were "+
			"generally true the trap would not exist; check the fixture.", sample)
	}
	t.Logf("over %d indices the two descriptors agreed %d times and differed %d; "+
		"the first disagreement is at index %d:\n  sortedmulti  %s\n  multi        %s",
		sample, agreed, differed, firstDifference,
		rightAddrs[firstDifference], wrongAddrs[firstDifference])
	if agreed > 0 && firstDifference > 0 {
		t.Logf("note that index 0 agrees, so an operator who compared one address "+
			"would have passed a wrong descriptor. DefaultSampleSize is %d for "+
			"this reason.", coldwallet.DefaultSampleSize)
	}

	// And the whole setup path completes happily on the wrong descriptor.
	wrongChange := strings.Replace(changeBody(t, env), "sortedmulti(", "multi(", 1)
	const name = "winthistle-multi-trap-test"
	wallet := newWallet(t, env, name)

	result, err := coldwallet.Install(ctx, env.Node, wallet, coldwallet.Config{
		WalletName: name,
		Receive:    wrong,
		Change:     wrongChange,
		Birthday:   coldwallet.Genesis,
		GapLimit:   50,
	})
	if err != nil {
		t.Fatalf("Install refused the multi() descriptors: %v", err)
	}
	if result.Verdict() != coldwallet.AwaitingAddressCheck {
		t.Fatalf("Install reported %v on the wrong descriptors", result.Verdict())
	}
	if !result.Check.Consistent() {
		t.Errorf("the wrong wallet is internally inconsistent, which would be a "+
			"different bug from the one this test is about:\n%s", result.Check.Report())
	}

	var balances struct {
		Mine struct {
			Trusted float64 `json:"trusted"`
		} `json:"mine"`
	}
	if err := wallet.Call(ctx, "getbalances", nil, &balances); err != nil {
		t.Fatalf("getbalances: %v", err)
	}
	t.Logf("the multi() wallet was built without a single complaint, is entirely "+
		"self-consistent, and holds %v BTC — and a correct wallet whose coins have "+
		"not moved yet looks exactly the same. Only the address comparison "+
		"separates them.", balances.Mine.Trusted)

	// The one thing that does separate them: some of its addresses are not the
	// cold wallet's. Checked over the same 20 indices as the comparison above,
	// so the assertion is as deterministic as that one — asserting it over the
	// five the round-trip check samples would be a coin toss dressed as a test.
	var mismatches, sampled int
	for i, addr := range wrongAddrs {
		info, err := env.Cold.GetAddressInfo(ctx, addr)
		if err != nil {
			t.Fatalf("asking %s about %s: %v", regtestenv.ColdWallet, addr, err)
		}
		if info.IsMine {
			continue
		}
		mismatches++
		if i < coldwallet.DefaultSampleSize {
			sampled++
		}
	}
	if mismatches == 0 {
		t.Fatalf("every address of the wrong wallet is also the cold wallet's, so " +
			"the address check could not tell them apart at any index")
	}
	t.Logf("%d of the first %d addresses of the wrong wallet are not the cold "+
		"wallet's, of which %d fall inside the %d the round-trip check shows",
		mismatches, len(wrongAddrs), sampled, coldwallet.DefaultSampleSize)
	if sampled == 0 {
		t.Logf("none of them did on this fixture, which is the case " +
			"DefaultSampleSize is sized against — raise it if this recurs")
	}
}

// changeBody returns the harness's change descriptor with its checksum stripped,
// ready to be mangled.
func changeBody(t *testing.T, env *regtestenv.Env) string {
	t.Helper()
	_, change := harnessDescriptors(t, env)
	if i := strings.LastIndex(change, "#"); i > 0 {
		return change[:i]
	}
	return change
}

// TestConfirmCatchesAWalletThatIsNotTheOneWeMeantToBuild.
//
// createwallet is idempotent so a half-finished setup can be resumed, and the
// price of that is that the wallet may not be ours. This proves the read-back
// notices.
func TestConfirmCatchesAWalletThatIsNotTheOneWeMeantToBuild(t *testing.T) {
	env := regtestenv.Start(t)
	ctx := testCtx(t)
	receive, change := harnessDescriptors(t, env)

	const name = "winthistle-keyed-test"
	wallet := newWallet(t, env, name)

	// A wallet with private keys enabled — everything Install refuses to build.
	err := env.Node.Call(ctx, "createwallet",
		[]any{name, false, false, "", false, true, true}, nil)
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("creating a keyed wallet: %v", err)
	}
	if err := env.Node.EnsureWalletLoaded(ctx, name); err != nil {
		t.Fatalf("loading %s: %v", name, err)
	}

	cfg := coldwallet.Config{WalletName: name, Receive: receive, Change: change,
		Birthday: coldwallet.Genesis}
	prepared, err := coldwallet.Prepare(ctx, env.Node, cfg)
	if err != nil {
		t.Fatalf("preparing: %v", err)
	}
	_, err = coldwallet.Confirm(ctx, wallet, cfg, prepared)
	if err == nil {
		t.Fatal("a wallet with private keys enabled was accepted")
	}
	if !strings.Contains(err.Error(), "private keys") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}

// TestPreflightSaysDirectedModeIsAvailableHere.
func TestPreflightSaysDirectedModeIsAvailableHere(t *testing.T) {
	env := regtestenv.Start(t)
	ctx := testCtx(t)

	p, err := coldwallet.RunPreflight(ctx, env.Node, coldwallet.Config{
		WalletName: "winthistle-cold", Receive: "x", Change: "y",
		Birthday: coldwallet.Genesis,
	})
	if err != nil {
		t.Fatalf("pre-flight: %v", err)
	}
	if !p.Available() {
		t.Fatalf("directed mode reported unavailable on the harness: %s", p.Summary())
	}
	if p.Chain != "regtest" {
		t.Errorf("chain reported as %q", p.Chain)
	}
	if p.Version < coldwallet.MinCoreVersion {
		t.Errorf("the harness runs Core %d, below the %d floor", p.Version,
			coldwallet.MinCoreVersion)
	}
	if p.Pruned {
		t.Errorf("the harness node reports itself pruned, which it should not be")
	}
	t.Logf("%s — %s", p.Summary(), notProvedHere)
}

// TestLegacyCoinsAreExcludedAndFencedOff proves the I-3 filter against Core's
// real listunspent output rather than against a hand-written fixture, and proves
// the exclusion mechanism: Core has no "do not spend these" option on
// walletcreatefundedpsbt, but it does skip locked outputs.
func TestLegacyCoinsAreExcludedAndFencedOff(t *testing.T) {
	env := regtestenv.Start(t)
	ctx := testCtx(t)

	// A keyed fixture wallet, so the addresses can be minted here. It stands in
	// for a cold wallet whose descriptors include a legacy branch; the app's own
	// wallet is never keyed.
	const name = "winthistle-coins-test"
	wallet := newWallet(t, env, name)
	err := env.Node.Call(ctx, "createwallet",
		[]any{name, false, false, "", false, true, true}, nil)
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("creating the fixture wallet: %v", err)
	}
	if err := env.Node.EnsureWalletLoaded(ctx, name); err != nil {
		t.Fatalf("loading %s: %v", name, err)
	}

	addrOf := func(kind string) string {
		var a string
		if err := wallet.Call(ctx, "getnewaddress", []any{"", kind}, &a); err != nil {
			t.Fatalf("getnewaddress %s: %v", kind, err)
		}
		return a
	}
	legacyAddr := addrOf("legacy")
	fundingAddr := addrOf("bech32")
	segwitAddr := addrOf("bech32")

	// Fund from the miner to the fixture's own bech32 address, then let the
	// fixture pay itself at the legacy one.
	//
	// The indirection matters. Core derives change of the same type as the
	// payment, so sending from the miner straight to a legacy address leaves the
	// *miner* holding a legacy change output — and the next batch built from the
	// miner wallet then fails at psbt_verify with "not all inputs are SegWit
	// spends", in a test that has nothing to do with this one. Keeping the legacy
	// coins inside the fixture wallet keeps the poison in the bottle.
	var txid string
	if err := env.Miner.Call(ctx, "sendtoaddress", []any{fundingAddr, "2.0"}, &txid); err != nil {
		t.Fatalf("funding the fixture wallet: %v", err)
	}
	env.Mine(t, 2)
	if err := wallet.Call(ctx, "sendtoaddress", []any{legacyAddr, "0.5"}, &txid); err != nil {
		t.Fatalf("paying the fixture's own legacy address: %v", err)
	}
	// That send spent the funding coin and, because Core derives change of the
	// same type as the payment, left the fixture's change legacy as well. So the
	// segwit coin the batch is allowed to spend has to arrive separately.
	if err := env.Miner.Call(ctx, "sendtoaddress", []any{segwitAddr, "1.0"}, &txid); err != nil {
		t.Fatalf("funding the fixture's segwit coin: %v", err)
	}
	env.Mine(t, 2)

	coins, err := coldwallet.SelectCoins(ctx, wallet, 1)
	if err != nil {
		t.Fatalf("selecting coins: %v", err)
	}
	t.Logf("\n%s", coins.Report())

	if len(coins.Eligible) == 0 {
		t.Fatalf("the fixture left no spendable coin at all:\n%s", coins.Report())
	}

	var sawLegacy bool
	for _, e := range coins.Excluded {
		if e.Coin.Address == legacyAddr {
			sawLegacy = true
			if e.Why != coldwallet.Legacy {
				t.Errorf("the legacy coin was excluded as %v, not as a legacy spend", e.Why)
			}
		}
	}
	if !sawLegacy {
		t.Fatalf("the P2PKH coin at %s was not excluded. LND refuses a funding "+
			"transaction with any non-SegWit input outright.", legacyAddr)
	}
	var sawSegwit bool
	for _, u := range coins.Eligible {
		if u.Address == segwitAddr {
			sawSegwit = true
		}
	}
	if !sawSegwit {
		t.Errorf("the bech32 coin at %s was left out of the batch", segwitAddr)
	}

	// Fencing off: Core must then refuse to select the legacy coin.
	locked, err := coldwallet.FenceOff(ctx, wallet, coins)
	if err != nil {
		t.Fatalf("fencing off the excluded coins: %v", err)
	}
	t.Cleanup(func() {
		c, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		if len(locked) > 0 {
			_, _ = wallet.ReleaseLocks(c, locked)
		}
	})
	if len(locked) == 0 {
		t.Fatal("nothing was locked, so nothing stops Core selecting the legacy coin")
	}

	after, err := wallet.ListUnspent(ctx, 0, 9_999_999)
	if err != nil {
		t.Fatalf("listing coins after the fence: %v", err)
	}
	for _, u := range after {
		if u.Address == legacyAddr {
			t.Errorf("the legacy coin %s is still selectable after FenceOff", u.Outpoint())
		}
	}
}
