package setup_test

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AusDavo/winthistle/internal/bitcoind"
	"github.com/AusDavo/winthistle/internal/coldwallet"
	"github.com/AusDavo/winthistle/internal/config"
	"github.com/AusDavo/winthistle/internal/journal"
	"github.com/AusDavo/winthistle/internal/regtestenv"
	"github.com/AusDavo/winthistle/internal/setup"
)

// What regtest cannot prove here, said once. Every test in this file exercises
// the descriptor lifecycle and the answer that is recorded about it. None of
// them exercises the rescan: regtest has no chain history, so a wallet imported
// with the right birthday and one imported with a wrong one find exactly the same
// nothing.
//
// That is covered against the signet/ harness, one layer down, in
// internal/coldwallet/coldwallet_signet_test.go — nothing in this package sits
// between setup and the rescan, so there is nothing here for a second copy to
// prove.
const notProvedHere = "regtest has no chain history, so the rescan is not proved " +
	"here — it is in internal/coldwallet, against signet/"

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// pair reads the harness's own descriptors, so no fixture can drift from the
// cold wallet the coins are actually in.
func pair(t *testing.T, env *regtestenv.Env) (receive, change string) {
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
		t.Fatalf("%s has no active descriptor pair — run: make harness",
			regtestenv.ColdWallet)
	}
	return receive, change
}

// wrongPair is the design's second setup trap: multi() where the wallet uses
// sortedmulti(). Same keys, different addresses at rather more than half the
// indices, and it imports without a complaint.
func wrongPair(t *testing.T, env *regtestenv.Env) (receive, change string) {
	t.Helper()
	r, c := pair(t, env)
	swap := func(d string) string {
		if i := strings.LastIndex(d, "#"); i > 0 {
			d = d[:i]
		}
		out := strings.Replace(d, "sortedmulti(", "multi(", 1)
		if out == d {
			t.Fatalf("the harness descriptor is not a sortedmulti: %s", d)
		}
		return out
	}
	return swap(r), swap(c)
}

// wallet builds a client for a wallet that may not exist yet, and unloads it
// afterwards so a re-run starts from Core's on-disk copy.
func wallet(t *testing.T, env *regtestenv.Env, name string) *bitcoind.Client {
	t.Helper()
	c := env.WalletClient(t, name, 5*time.Minute)
	t.Cleanup(func() {
		ctx, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		_ = env.Node.Call(ctx, "unloadwallet", []any{name}, nil)
	})
	return c
}

func openJournal(t *testing.T) *journal.Journal {
	t.Helper()
	j, err := journal.Open(context.Background(), filepath.Join(t.TempDir(), "runs.db"))
	if err != nil {
		t.Fatalf("opening the journal: %v", err)
	}
	t.Cleanup(func() { j.Close() })
	return j
}

func descriptors(receive, change string) *config.Descriptors {
	return &config.Descriptors{
		Receive: receive, Change: change, Birthday: coldwallet.Genesis,
	}
}

func answering(a setup.Answer) func(context.Context, coldwallet.AddressCheck) (setup.Answer, error) {
	return func(context.Context, coldwallet.AddressCheck) (setup.Answer, error) {
		return a, nil
	}
}

// TestSetupRecordsNothingUntilSomebodyAnswers.
//
// The property the whole command turns on. Building the wallet establishes
// nothing — a wrong descriptor builds just as cleanly — so a run that nobody
// answered must leave the journal exactly as it found it, and the next run must
// be able to ask the same question again.
func TestSetupRecordsNothingUntilSomebodyAnswers(t *testing.T) {
	env := regtestenv.Start(t)
	ctx := testCtx(t)
	receive, change := pair(t, env)

	const name = "winthistle-setup-unanswered"
	w := wallet(t, env, name)
	j := openJournal(t)

	// Ask is nil: a pipe, a script, a terminal at end of input.
	d := setup.Deps{Node: env.Node, Wallet: w, Journal: j, Out: io.Discard}
	res, err := setup.Do(ctx, d, setup.Options{
		WalletName: name, Descriptors: descriptors(receive, change), GapLimit: 50,
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	t.Log(notProvedHere)

	if res.Recorded != nil {
		t.Error("a comparison nobody made was recorded")
	}
	if _, err := j.LatestSetup(ctx, name); !errors.Is(err, journal.ErrNoSetup) {
		t.Errorf("the journal holds an answer after nobody gave one: %v", err)
	}
	if !res.Check.Recognised() {
		t.Fatalf("the wallet does not recognise its own addresses:\n%s",
			res.Check.Report())
	}

	// The addresses have to be the harness cold wallet's, or the rest of this
	// file is testing arithmetic rather than a setup.
	for i, got := range res.Check.Receive {
		info, err := env.Cold.GetAddressInfo(ctx, got.Address)
		if err != nil {
			t.Fatalf("asking %s about receive address %d: %v", regtestenv.ColdWallet, i, err)
		}
		if !info.IsMine {
			t.Errorf("receive address %d (%s) is not %s's", i, got.Address,
				regtestenv.ColdWallet)
		}
	}

	// And the resume path asks again, from the wallet rather than from the file.
	res2, err := setup.Do(ctx, setup.Deps{
		Node: env.Node, Wallet: w, Journal: j, Out: io.Discard,
		Ask: answering(setup.Matched),
	}, setup.Options{WalletName: name})
	if err != nil {
		t.Fatalf("resuming: %v", err)
	}
	if res2.Recorded == nil {
		t.Fatal("an answer was given and nothing was recorded")
	}
	if res2.Check.Receive[0].Address != res.Check.Receive[0].Address {
		t.Error("the resume path derived different addresses from the install path")
	}

	rec, err := j.LatestSetup(ctx, name)
	if err != nil {
		t.Fatalf("reading the answer back: %v", err)
	}
	if rec.Outcome != journal.SetupConfirmed {
		t.Errorf("outcome %q", rec.Outcome)
	}
	if !rec.Describes(res2.Landed.Receive.Desc, res2.Landed.Change.Desc) {
		t.Error("the record does not describe the descriptors that are in the wallet")
	}
}

// TestAWrongDescriptorIsRecordedAsWrong.
//
// The multi() pair imports without a complaint, is internally self-consistent,
// and finds a plausible part of the cold wallet's balance. Nothing here can tell
// it apart from a correct one — so what the command has to get right is recording
// the human's answer, and refusing afterwards rather than warning.
func TestAWrongDescriptorIsRecordedAsWrong(t *testing.T) {
	env := regtestenv.Start(t)
	ctx := testCtx(t)
	receive, change := wrongPair(t, env)

	const name = "winthistle-setup-rejected"
	w := wallet(t, env, name)
	j := openJournal(t)

	res, err := setup.Do(ctx, setup.Deps{
		Node: env.Node, Wallet: w, Journal: j, Out: io.Discard,
		Ask: answering(setup.Differed),
	}, setup.Options{
		WalletName: name, Descriptors: descriptors(receive, change), GapLimit: 50,
	})
	if !errors.Is(err, setup.ErrRejected) {
		t.Fatalf("setup returned %v, want ErrRejected — a shell that ran `setup && "+
			"run` has to stop here", err)
	}
	if res.Recorded == nil || res.Recorded.Outcome != journal.SetupRejected {
		t.Fatalf("the rejection was not recorded: %+v", res.Recorded)
	}

	// The wrong wallet was built without a complaint and is entirely
	// self-consistent, which is the point.
	if !res.Check.Consistent() {
		t.Errorf("the wrong wallet is internally inconsistent, which would be a "+
			"different bug from the one this test is about:\n%s", res.Check.Report())
	}
	var balances struct {
		Mine struct {
			Trusted float64 `json:"trusted"`
		} `json:"mine"`
	}
	if err := w.Call(ctx, "getbalances", nil, &balances); err != nil {
		t.Fatalf("getbalances: %v", err)
	}
	t.Logf("the multi() wallet imported cleanly, recognises all %d of its own "+
		"derived addresses, and holds %v BTC. Only the operator's answer separated "+
		"it from a correct one.", len(res.Check.Receive), balances.Mine.Trusted)
}

// TestACorrectedSetupCanStillBeConfirmed is the failure that would make an
// operator distrust a working setup, demonstrated and then not happening.
//
// The path is the ordinary one: import a pair, be told the addresses do not
// match, fix the descriptor, run setup again. Core cannot remove the rejected
// descriptor, so the wallet now holds two that derive the same address at every
// index where the sorted and unsorted key orders agree — and getaddressinfo's
// parent_desc is singular, so Core credits those addresses to one of them. On
// Core 29 that is the key manager created first, which is the rejected one.
//
// So on a pair that is entirely correct, several addresses report as belonging to
// a different descriptor. Gating the question on that would refuse to ask about
// the corrected wallet. The gate is Recognised — does this wallet hold these
// addresses — and attribution is reported instead.
func TestACorrectedSetupCanStillBeConfirmed(t *testing.T) {
	env := regtestenv.Start(t)
	ctx := testCtx(t)
	wrongR, wrongC := wrongPair(t, env)
	rightR, rightC := pair(t, env)

	const name = "winthistle-setup-corrected"
	w := wallet(t, env, name)
	j := openJournal(t)
	d := setup.Deps{Node: env.Node, Wallet: w, Journal: j, Out: io.Discard}

	// 1. The wrong pair, rejected.
	d.Ask = answering(setup.Differed)
	first, err := setup.Do(ctx, d, setup.Options{
		WalletName: name, Descriptors: descriptors(wrongR, wrongC), GapLimit: 50,
	})
	if !errors.Is(err, setup.ErrRejected) {
		t.Fatalf("the wrong pair was not rejected: %v", err)
	}

	// 2. The corrected pair, into the same wallet.
	d.Ask = answering(setup.Matched)
	second, err := setup.Do(ctx, d, setup.Options{
		WalletName: name, Descriptors: descriptors(rightR, rightC), GapLimit: 50,
	})
	if err != nil {
		t.Fatalf("the corrected pair could not be confirmed: %v\n%s", err,
			second.Check.Report())
	}

	// The measurement this test exists for.
	if !second.Check.Recognised() {
		t.Fatalf("the wallet disowns its own corrected addresses:\n%s",
			second.Check.Report())
	}
	if second.Check.Attributed() {
		t.Log("Core credited every address to the corrected descriptor on this " +
			"fixture, so the misattribution did not arise here. The gate is still " +
			"Recognised rather than Attributed; see the doc comment.")
	} else {
		elsewhere := second.Check.Elsewhere()
		t.Logf("as expected: Core credits some of the corrected pair's addresses to "+
			"%d other descriptor(s) in this wallet, and the setup was confirmable "+
			"anyway", len(elsewhere))
		if len(elsewhere) == 0 {
			t.Error("attribution is broken and no other descriptor is named, so the " +
				"operator would be told nothing about why")
		}
	}

	// The rejected pair is still in the wallet, inactive, and setup says so.
	if len(second.Landed.Stale()) == 0 {
		t.Error("the rejected descriptor is gone, which Core has no RPC for")
	}
	if !strings.Contains(coldwallet.StaleWarning(second.Landed), "no RPC that removes") {
		t.Error("the stale-descriptor warning does not say the old pair cannot be removed")
	}

	// And the journal's two rows are about two different pairs.
	if first.Recorded.Describes(rightR, rightC) {
		t.Error("the rejection claims to be about the corrected pair")
	}
	latest, err := j.LatestSetup(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	if latest.Seq != 2 || latest.Outcome != journal.SetupConfirmed {
		t.Errorf("the latest answer is seq %d, %q", latest.Seq, latest.Outcome)
	}
	if !latest.Describes(second.Landed.Receive.Desc, second.Landed.Change.Desc) {
		t.Error("the confirmation does not describe what is in the wallet")
	}
}

// TestADeactivatedDescriptorsCoinsAreStillSpendable is the claim the corrected
// setup's copy makes, measured rather than asserted.
//
// It is the reason the advice is "use a new wallet name" and not "import the
// right one over the top". A rejected descriptor's coins do not go away when it
// is deactivated, and coin selection has no idea which descriptor found what.
func TestADeactivatedDescriptorsCoinsAreStillSpendable(t *testing.T) {
	env := regtestenv.Start(t)
	ctx := testCtx(t)
	receive, change := pair(t, env)

	const name = "winthistle-setup-stale-coins"
	w := wallet(t, env, name)
	j := openJournal(t)
	d := setup.Deps{Node: env.Node, Wallet: w, Journal: j, Out: io.Discard}

	if _, err := setup.Do(ctx, d, setup.Options{
		WalletName: name, Descriptors: descriptors(receive, change), GapLimit: 50,
	}); err != nil {
		t.Fatalf("importing the funded pair: %v", err)
	}
	before, err := w.ListUnspent(ctx, 0, 9_999_999)
	if err != nil {
		t.Fatalf("listing coins: %v", err)
	}
	if len(before) == 0 {
		t.Skip("the harness cold wallet has no coins, so there is nothing to " +
			"measure — run: make harness")
	}

	// A pair over the same keys on a branch nothing has ever used. Importing it
	// active deactivates the funded pair without removing it.
	other := func(desc string, branch string) string {
		if i := strings.LastIndex(desc, "#"); i > 0 {
			desc = desc[:i]
		}
		return strings.ReplaceAll(desc, "/0/*", branch)
	}
	if _, err := setup.Do(ctx, d, setup.Options{
		WalletName: name, GapLimit: 50,
		Descriptors: descriptors(other(receive, "/7/*"), other(receive, "/8/*")),
	}); err != nil {
		t.Fatalf("importing the second pair: %v", err)
	}

	landed, err := coldwallet.Read(ctx, w)
	if err != nil {
		t.Fatalf("reading the wallet back: %v", err)
	}
	if len(landed.Stale()) == 0 {
		t.Fatal("the funded pair was not deactivated, so this test measures nothing")
	}

	after, err := w.ListUnspent(ctx, 0, 9_999_999)
	if err != nil {
		t.Fatalf("listing coins after the deactivation: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("%d coins before the deactivation and %d after — if Core did start "+
			"hiding a deactivated descriptor's coins, the corrected-setup copy is "+
			"wrong and should be corrected rather than kept",
			len(before), len(after))
	}
	t.Logf("%d coins still selectable after their descriptor was deactivated, "+
		"which is why the advice after a rejection is a new wallet name rather "+
		"than a second import", len(after))
}
