package coldwallet_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/AusDavo/winthistle/internal/bitcoind"
	"github.com/AusDavo/winthistle/internal/coldwallet"
	"github.com/AusDavo/winthistle/internal/signetenv"
)

// The two things regtest cannot reach, on a chain that has history.
//
// coldwallet_regtest_test.go's notProvedHere constant names this file. Signet
// has years of other people's blocks, so a birthday before the cold wallet's
// first coin and a birthday after it find different things, and a node started
// with -prune has a horizon RunPreflight can date and refuse against.
//
// Everything here needs WINTHISTLE_SIGNET=1 and the signet/ harness. See
// signetenv.Start for what it skips on and why, and signet/README.md for the
// faucet step, which has a human in it.

func signetCtx(t *testing.T) context.Context {
	t.Helper()
	// Generous: a rescan over a real chain is the thing being measured, and a
	// deadline shorter than one would turn the measurement into a timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// signetWallet builds a client for a wallet name that no previous run has used.
//
// Unique per run, and that is the point rather than tidiness. A rescan test
// against a wallet that already found the coin on an earlier run would pass with
// the rescan removed entirely — the coin would still be sitting in it. A fresh
// wallet is the only way the assertion means what it says.
//
// The cost is a wallet directory left on the node per run. This harness is
// disposable; `make -C signet sweep` removes them.
func signetWallet(t *testing.T, env *signetenv.Env, what string) (*bitcoind.Client, string) {
	t.Helper()
	name := fmt.Sprintf("winthistle-%s-test-%d", what, time.Now().UnixNano())
	// A descriptor import blocks for the whole rescan.
	c := env.WalletClient(t, name, 30*time.Minute)
	t.Cleanup(func() {
		ctx, done := context.WithTimeout(context.Background(), 60*time.Second)
		defer done()
		_ = env.Node.Call(ctx, "unloadwallet", []any{name}, nil)
	})
	return c, name
}

// TestARescanFromTheRightBirthdayFindsTheCoins is the first thing an operator
// does on mainnet with their real descriptors, done here against a real chain.
//
// Install blocks for the whole rescan, which is why bitcoind.Config has a
// Timeout at all. What is asserted is not that the call returned: it is that the
// wallet afterwards holds the coin the harness's cold wallet holds, found by
// scanning back to a birthday thirty days before it landed.
func TestARescanFromTheRightBirthdayFindsTheCoins(t *testing.T) {
	env := signetenv.Start(t)
	ctx := signetCtx(t)
	receive, change := env.Descriptors(t, ctx)
	coin := env.OldestCoin(t, ctx)

	wallet, name := signetWallet(t, env, "rescan")
	birthday := coin.Time.AddDate(0, 0, -30)

	cfg := coldwallet.Config{
		WalletName: name,
		Receive:    receive,
		Change:     change,
		Birthday:   birthday,
	}
	if coldwallet.AnyFatal(cfg.Validate()) {
		t.Fatalf("the fixture config is refused before anything is created: %v",
			cfg.Validate())
	}

	started := time.Now()
	result, err := coldwallet.Install(ctx, env.Node, wallet, cfg)
	took := time.Since(started)
	if err != nil {
		t.Fatalf("installing the watch-only wallet: %v", err)
	}
	if got := result.Verdict(); got != coldwallet.AwaitingAddressCheck {
		t.Fatalf("Install reported %v; the only outcome it may report is a question", got)
	}

	// The measurement, logged rather than asserted. There is no right number
	// here and a threshold would only ever fail on somebody's slower disk — but
	// an operator asking "how long does this take" deserves a real figure from a
	// real chain rather than a guess, and this is where one comes from.
	t.Logf("rescanned from a birthday of %s to the tip in %s; the coin under test "+
		"is at height %d, dated %s", birthday.Format("2006-01-02"),
		took.Round(time.Second), coin.Height, coin.Time.Format("2006-01-02"))

	utxos, err := wallet.ListUnspent(ctx, 1, 9_999_999)
	if err != nil {
		t.Fatalf("listing the new wallet's coins: %v", err)
	}
	found := false
	for _, u := range utxos {
		if u.TxID == coin.TxID && u.Vout == coin.Vout {
			found = true
		}
	}
	if !found {
		t.Fatalf("the rescan did not find %s:%d, which confirmed at height %d on "+
			"%s — a month after the birthday of %s it was given. The wallet holds "+
			"%d coins. This is the failure the whole birthday mechanism exists to "+
			"prevent: a wallet that imports cleanly and reports nothing.",
			coin.TxID, coin.Vout, coin.Height, coin.Time.Format("2006-01-02"),
			birthday.Format("2006-01-02"), len(utxos))
	}

	// The same round trip the regtest test makes, against a wallet whose coins
	// are real: these addresses have to be the ones cold-watch already uses.
	if !result.Check.Consistent() {
		t.Errorf("Core does not recognise its own derived addresses:\n%s",
			result.Check.Report())
	}
	for i, d := range result.Check.Receive {
		info, err := env.Cold.GetAddressInfo(ctx, d.Address)
		if err != nil {
			t.Fatalf("asking %s about receive address %d: %v", signetenv.ColdWallet, i, err)
		}
		if !info.Ours() {
			t.Errorf("receive address %d (%s) is not an address of %s — the import "+
				"landed a different wallet from the one the coin belongs to",
				i, d.Address, signetenv.ColdWallet)
		}
	}
}

// TestATooLateBirthdayFindsNothingAndLooksExactlyTheSame pins the hole, on
// purpose. Read the next four paragraphs before changing anything here.
//
// A birthday later than the wallet's coins produces a wallet that imports
// cleanly, passes every check this build makes, derives the correct addresses,
// reports no error, no warning that could be called a refusal — and holds
// nothing. It is byte-for-byte the same outcome as a correct setup on a cold
// wallet that has simply never been paid. That is asserted below rather than
// worked around.
//
// **It is not a bug, and it is not fixable.** Core cannot know the wallet's real
// birthday; only the operator does. A zero balance cannot be read as evidence,
// because a correct wallet whose coins have not arrived shows the same zero. The
// only refusals available are the ones already there — Config.Validate refuses a
// birthday it was not given and one in the future, and warns about one inside
// the last week — and none of them can distinguish "too late" from "new wallet".
//
// **This is what the round-trip address check is for.** The addresses are the
// one thing that is checkable without knowing the history: they come out of the
// descriptors that actually landed, and the operator compares them against their
// own wallet software. That comparison is why Install ends in a question rather
// than a success — see coldwallet's package comment.
//
// **So if you are here because this test looks like it is asserting a defect:**
// the defect is real, it is named in docs/design.html, and the mitigation is the
// address check. What must not happen is a later slice turning the birthday into
// a promise — refusing a "suspicious" birthday, warning on an empty rescan,
// calling a zero balance a failure. Every one of those would refuse correct
// setups on new wallets, and none of them can catch this. If this test starts
// failing, the question to ask is which of those was added.
func TestATooLateBirthdayFindsNothingAndLooksExactlyTheSame(t *testing.T) {
	env := signetenv.Start(t)
	ctx := signetCtx(t)
	receive, change := env.Descriptors(t, ctx)
	env.RequireClearOfTheTimestampWindow(t, ctx)
	coin := env.OldestCoin(t, ctx)

	wallet, name := signetWallet(t, env, "toolate")

	// Today. This is the trap the design names by its own name: a birthday of
	// "now" wearing a date. OldestCoin has already refused to run this against a
	// coin young enough for Core's import-timestamp window to reach back over —
	// without that clearance the rescan would cover the coin anyway and this
	// test would prove the opposite of what it says.
	birthday := time.Now()
	if !birthday.After(coin.Time.Add(signetenv.TimestampWindow())) {
		t.Fatalf("the coin at height %d confirmed at %s, which is not clear of "+
			"Core's %s import window before now", coin.Height, coin.Time,
			signetenv.TimestampWindow())
	}

	cfg := coldwallet.Config{
		WalletName: name,
		Receive:    receive,
		Change:     change,
		Birthday:   birthday,
	}

	// The only thing in this build that says anything at all about this
	// birthday, and it is a warning rather than a refusal — because for a cold
	// wallet genuinely created today it is correct.
	concerns := cfg.Validate()
	if coldwallet.AnyFatal(concerns) {
		t.Fatalf("a birthday of today is refused outright, which would refuse a "+
			"cold wallet created today: %v", concerns)
	}
	var warned bool
	for _, c := range concerns {
		if c.Subject == "the birthday" {
			warned = true
			t.Logf("the one signal there is: %s", c)
		}
	}
	if !warned {
		t.Error("nothing at all is said about a birthday of today. That warning is " +
			"one half of the cover for this hole; the address check is the other.")
	}

	result, err := coldwallet.Install(ctx, env.Node, wallet, cfg)
	if err != nil {
		t.Fatalf("Install refused a too-late birthday: %v.\n"+
			"That refusal cannot be right — it would refuse every cold wallet "+
			"created this week. Read this test's comment.", err)
	}

	// Indistinguishable, item by item, from the correct setup above.
	if got := result.Verdict(); got != coldwallet.AwaitingAddressCheck {
		t.Errorf("Install reported %v on a too-late birthday and %v on a correct "+
			"one; they are supposed to be the same outcome",
			got, coldwallet.AwaitingAddressCheck)
	}
	if !result.Check.Consistent() {
		t.Errorf("the address check fails on a too-late birthday, which would make "+
			"it a birthday check rather than a descriptor check:\n%s",
			result.Check.Report())
	}
	for i, d := range result.Check.Receive {
		info, err := env.Cold.GetAddressInfo(ctx, d.Address)
		if err != nil {
			t.Fatalf("asking %s about receive address %d: %v", signetenv.ColdWallet, i, err)
		}
		if !info.Ours() {
			t.Errorf("receive address %d (%s) is not an address of %s. The "+
				"descriptors are right; only the birthday is wrong, and the "+
				"addresses must not know the difference", i, d.Address,
				signetenv.ColdWallet)
		}
	}

	// And the difference, which nothing in the result reports.
	utxos, err := wallet.ListUnspent(ctx, 0, 9_999_999)
	if err != nil {
		t.Fatalf("listing the new wallet's coins: %v", err)
	}
	if len(utxos) != 0 {
		t.Fatalf("the wallet found %d coins with a birthday of %s, later than the "+
			"only coin it could have found (height %d, %s). Either Core's import "+
			"timestamp is no longer being honoured, or this test is being run "+
			"against a wallet an earlier run already filled.",
			len(utxos), birthday.Format("2006-01-02"), coin.Height,
			coin.Time.Format("2006-01-02"))
	}

	report := result.Report()
	if strings.Contains(strings.ToLower(report), "no coins") ||
		strings.Contains(strings.ToLower(report), "found nothing") {

		t.Errorf("the report treats an empty rescan as a finding. It cannot: a "+
			"correct wallet that has never been paid produces the same emptiness. "+
			"Report:\n%s", report)
	}
	t.Logf("a correct-looking setup holding nothing:\n%s", report)
}

// TestPrunedPastBirthdayFiresOnAPrunedNode is the first time this refusal has
// ever fired. Regtest cannot make a node meaningfully pruned, so until now it
// was written and never proved — the same class of unknown as a bool that meant
// two things.
//
// Both directions are asserted, because "pruned" on its own is not the refusal.
// A pruned node serving a wallet born after its horizon is fine, and a check
// that refused every pruned node would be wrong about the ordinary Umbrel case
// the design cares about.
func TestPrunedPastBirthdayFiresOnAPrunedNode(t *testing.T) {
	env := signetenv.Start(t)
	ctx := signetCtx(t)

	// A birthday older than the horizon: the blocks are gone.
	old := coldwallet.Config{
		WalletName: "winthistle-prune-test",
		Receive:    "wsh(sortedmulti(2,A/0/*,B/0/*))", // never parsed: RunPreflight asks Core nothing about them
		Change:     "wsh(sortedmulti(2,A/1/*,B/1/*))",
		Birthday:   coldwallet.Genesis,
	}
	p, err := coldwallet.RunPreflight(ctx, env.Pruned, old)
	if err != nil {
		t.Fatalf("pre-flighting the pruned node: %v", err)
	}
	if !p.Pruned {
		t.Fatalf("the pruned node reports Pruned=false, so there is nothing here "+
			"to test: %+v", p)
	}
	if p.PruneHeight <= 0 {
		t.Errorf("Core reports pruned but a pruneheight of %d", p.PruneHeight)
	}
	if p.PruneHorizon.IsZero() {
		t.Error("the horizon was not dated. Core keeps every header, so " +
			"getblockheader answers for a pruned block and this should never be zero")
	}
	if p.Reason != coldwallet.PrunedPastBirthday {
		t.Fatalf("a birthday of %s against a horizon of %s (block %d) gave %v, "+
			"not PrunedPastBirthday", old.Birthday.Format("2006-01-02"),
			p.PruneHorizon.Format("2006-01-02"), p.PruneHeight, p.Reason)
	}
	if p.Available() {
		t.Error("the node is reported available for directed mode with the blocks " +
			"a rescan needs already deleted")
	}
	t.Logf("pruned to block %d (%s), %d blocks behind the tip\n%s",
		p.PruneHeight, p.PruneHorizon.Format("2006-01-02"),
		p.Height-p.PruneHeight, p.Summary())

	// The other direction, which is what makes it a comparison rather than a
	// blanket refusal: a wallet born after the horizon is servable by this same
	// pruned node.
	young := old
	young.Birthday = time.Now()
	q, err := coldwallet.RunPreflight(ctx, env.Pruned, young)
	if err != nil {
		t.Fatalf("pre-flighting the pruned node for a new wallet: %v", err)
	}
	if q.Reason != coldwallet.Ready {
		t.Errorf("a wallet born today is refused by a node pruned to %s: %v — the "+
			"check would then be refusing pruning rather than comparing dates",
			q.PruneHorizon.Format("2006-01-02"), q.Reason)
	}

	// And the unpruned node, which has no horizon at all.
	u, err := coldwallet.RunPreflight(ctx, env.Node, old)
	if err != nil {
		t.Fatalf("pre-flighting the unpruned node: %v", err)
	}
	if u.Pruned {
		t.Fatalf("the node the rescan tests use is pruned: %+v", u)
	}
	if u.Reason != coldwallet.Ready {
		t.Errorf("the unpruned node is not ready for a birthday of %s: %v",
			old.Birthday.Format("2006-01-02"), u.Reason)
	}
}
