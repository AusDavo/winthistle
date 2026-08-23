package doctor_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/AusDavo/winthistle/internal/config"
	"github.com/AusDavo/winthistle/internal/doctor"
	"github.com/AusDavo/winthistle/internal/journal"
	"github.com/AusDavo/winthistle/internal/methods"
	"github.com/AusDavo/winthistle/internal/regtestenv"
)

func ctxFor(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// TestDoctorReadsTheWholeSetup runs every check against the live harness.
//
// The assertion that matters is not that it passes — it deliberately does not,
// because the harness's credential is a copy of admin.macaroon. It is that the
// credential check *works*: CheckMacaroonPermissions answers about the macaroon
// in the request rather than about the caller's, no handler is invoked, and
// both halves of the promise are tested — sufficient for what the app calls,
// and insufficient for what it must never do.
//
// That second half is the one nothing else in the repo checks. The registry's
// never-list is a claim about the credential in the operator's config file, and
// an operator pointing this tool at admin.macaroon has broken every part of it
// while the tool works perfectly.
func TestDoctorReadsTheWholeSetup(t *testing.T) {
	env := regtestenv.Start(t)
	ctx := ctxFor(t)

	cfg, err := config.Load(env.ConfigFile(t, t.TempDir()))
	if err != nil {
		t.Fatalf("the harness config file: %v", err)
	}

	report := doctor.Run(ctx, cfg, doctor.Options{})
	t.Logf("\n%s", report.Report())

	byName := map[string]doctor.Check{}
	for _, c := range report.Checks {
		byName[c.Name] = c
	}
	for _, name := range []string{
		"winthistle.toml", "LND", "the macaroon", "Bitcoin Core",
		"the cold wallet", "the coins", "the anchor reserve", "the fee rate",
		"the peers", "the run journal",
	} {
		if _, ok := byName[name]; !ok {
			t.Errorf("no check called %q ran", name)
		}
	}

	// The setup itself is sound: everything except the credential should be
	// usable against a harness that other packages drive successfully.
	for _, name := range []string{"LND", "Bitcoin Core", "the cold wallet",
		"the coins", "the anchor reserve", "the fee rate", "the run journal"} {

		if c := byName[name]; c.Status == doctor.Fail {
			t.Errorf("%q failed against a working harness:\n%s", name,
				strings.Join(c.Lines, "\n"))
		}
	}

	mac := byName["the macaroon"]
	if mac.Status != doctor.Fail {
		t.Fatalf("the macaroon check passed on a copy of admin.macaroon, so the "+
			"never-list is not being checked against the file at all:\n%s",
			strings.Join(mac.Lines, "\n"))
	}
	joined := strings.Join(mac.Lines, "\n")

	// Admin holds the coarse entity permissions, so every never-list entry
	// should have come back as something this credential can do.
	for _, want := range []string{"send coins on-chain", "close a channel",
		"bake itself a wider credential"} {

		if !strings.Contains(joined, want) {
			t.Errorf("the never-list check did not notice that admin.macaroon can "+
				"%s:\n%s", want, joined)
		}
	}
	// And the other half: admin authorises everything the app calls, so nothing
	// should be reported missing.
	if strings.Contains(joined, "not authorised") {
		t.Errorf("admin.macaroon was reported as missing a permission this build "+
			"calls, which means the probe itself is wrong:\n%s", joined)
	}
	if strings.Contains(joined, "cannot even run the permission check") {
		t.Errorf("the probe could not run at all:\n%s", joined)
	}
	if len(mac.Fix) == 0 {
		t.Error("the macaroon failure printed no command to fix it")
	}
	if !strings.Contains(strings.Join(mac.Fix, " "), "print-macaroon-command") {
		t.Errorf("the fix is not the generated bake command: %v", mac.Fix)
	}

	if report.OK() {
		t.Error("the report says this setup is ready, with a credential that can " +
			"send coins")
	}
	t.Logf("%d app methods and %d never-list entries were checked against the "+
		"live node", len(methods.App()), len(methods.Forbidden()))
}

// TestDoctorWithABatchChecksThatBatch: with a batch file the peer and reserve
// checks are about the channels the operator means to open rather than about a
// hypothetical single one.
func TestDoctorWithABatchChecksThatBatch(t *testing.T) {
	env := regtestenv.Start(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	cfg, err := config.Load(env.ConfigFile(t, dir))
	if err != nil {
		t.Fatalf("the harness config file: %v", err)
	}
	batch := regtestenv.BatchFile(t, dir, env.Peers(t), 250_000)

	loaded, err := config.LoadBatch(batch)
	if err != nil {
		t.Fatalf("the harness batch file: %v", err)
	}
	report := doctor.Run(ctx, cfg, doctor.Options{Batch: loaded})
	t.Logf("\n%s", report.Report())

	var peersCheck doctor.Check
	for _, c := range report.Checks {
		if c.Name == "the peers" {
			peersCheck = c
		}
	}
	if peersCheck.Status == doctor.Skip {
		t.Fatal("the peer check skipped even though a batch was given")
	}
	joined := strings.Join(peersCheck.Lines, "\n")
	// One line per peer, plus the closing note. The lines say aliases rather
	// than pubkeys — peers.Facts.Summary is written for a human reading a batch
	// — so this counts them rather than matching keys.
	if len(peersCheck.Lines) < len(loaded.Channels)+1 {
		t.Errorf("%d channels in the batch but only %d lines in the peer check:\n%s",
			len(loaded.Channels), len(peersCheck.Lines), joined)
	}
	if !strings.Contains(joined, "Nothing here is a verdict") {
		t.Error("the peer check does not say that none of it is authoritative. " +
			"The peer's minimum is enforced conversationally and published nowhere.")
	}
}

// TestTheColdWalletCheckReportsTheAddressCheck runs the four verdicts doctor can
// reach about the only check whose evidence is a human.
//
// Nothing a node can be asked separates a correct cold-storage descriptor from a
// plausible wrong one: Core parses both, the import succeeds for both, the
// read-back is self-consistent for both, and the balance does not separate them
// either. So the answer is written down, keyed on the descriptors it was about —
// and each of these four is a different thing to tell an operator.
func TestTheColdWalletCheckReportsTheAddressCheck(t *testing.T) {
	env := regtestenv.Start(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	cfg, err := config.Load(env.ConfigFile(t, dir))
	if err != nil {
		t.Fatalf("the harness config file: %v", err)
	}

	// The harness's cold wallet, as it actually is.
	descs, err := env.Cold.ListDescriptors(ctx)
	if err != nil {
		t.Fatalf("listing %s's descriptors: %v", regtestenv.ColdWallet, err)
	}
	var receive, change string
	for _, d := range descs {
		switch {
		case d.Active && !d.Internal:
			receive = d.Desc
		case d.Active && d.Internal:
			change = d.Desc
		}
	}
	if receive == "" || change == "" {
		t.Fatalf("%s has no active pair — run: make harness", regtestenv.ColdWallet)
	}

	addrs, err := env.Node.DeriveAddresses(ctx, receive, 0, 0)
	if err != nil {
		t.Fatalf("deriving the first receive address: %v", err)
	}
	changeAddrs, err := env.Node.DeriveAddresses(ctx, change, 0, 0)
	if err != nil {
		t.Fatalf("deriving the first change address: %v", err)
	}

	record := func(t *testing.T, s journal.Setup) {
		t.Helper()
		j, err := journal.Open(ctx, cfg.Server.Journal)
		if err != nil {
			t.Fatalf("opening the journal: %v", err)
		}
		defer j.Close()
		if _, err := j.RecordSetup(ctx, s); err != nil {
			t.Fatalf("recording the answer: %v", err)
		}
	}
	coldCheck := func(t *testing.T) doctor.Check {
		t.Helper()
		report := doctor.Run(ctx, cfg, doctor.Options{})
		for _, c := range report.Checks {
			if c.Name == "the cold wallet" {
				return c
			}
		}
		t.Fatal("no cold wallet check ran")
		return doctor.Check{}
	}

	// 1. Nobody has answered. A warning rather than a failure: a wallet imported
	// by hand before this command existed is a working wallet that has not been
	// checked.
	c := coldCheck(t)
	if c.Status != doctor.Warn {
		t.Errorf("an unchecked wallet is %v, want warn:\n%s", c.Status,
			strings.Join(c.Lines, "\n"))
	}
	if !strings.Contains(strings.Join(c.Lines, " "), "Nobody has compared") &&
		!strings.Contains(strings.Join(c.Lines, " "), "nobody has compared") {

		t.Errorf("the check does not say the comparison has not been made:\n%s",
			strings.Join(c.Lines, "\n"))
	}
	if len(c.Fix) == 0 || !strings.Contains(strings.Join(c.Fix, " "), "winthistle setup") {
		t.Errorf("the fix is not `winthistle setup`: %v", c.Fix)
	}

	answered := journal.Setup{
		Wallet: cfg.Bitcoind.Wallet, Outcome: journal.SetupConfirmed,
		Receive: receive, Change: change, SampleSize: 5,
		FirstReceive: addrs[0], FirstChange: changeAddrs[0],
	}

	// 2. Confirmed, for these exact descriptors.
	record(t, answered)
	c = coldCheck(t)
	if c.Status == doctor.Fail {
		t.Errorf("a confirmed wallet failed:\n%s", strings.Join(c.Lines, "\n"))
	}
	if !strings.Contains(strings.Join(c.Lines, " "), "addresses confirmed on") {
		t.Errorf("the confirmation is not reported:\n%s", strings.Join(c.Lines, "\n"))
	}

	// 3. Confirmed, but about a different pair — so it says nothing about this
	// one. This is what stops a record ageing into a claim, and it is reachable
	// because Core cannot remove a descriptor.
	elsewhere := answered
	elsewhere.Receive = strings.Replace(receive, "sortedmulti(", "multi(", 1)
	record(t, elsewhere)
	c = coldCheck(t)
	if c.Status != doctor.Warn {
		t.Errorf("an answer about other descriptors is %v, want warn:\n%s",
			c.Status, strings.Join(c.Lines, "\n"))
	}
	if !strings.Contains(strings.Join(c.Lines, " "), "different descriptor pair") {
		t.Errorf("the check does not say the answer was about something else:\n%s",
			strings.Join(c.Lines, "\n"))
	}

	// 4. Rejected. The one state where doctor refuses rather than warns: a human
	// looked at these exact descriptors and said they are not the cold wallet's.
	rejected := answered
	rejected.Outcome = journal.SetupRejected
	record(t, rejected)
	c = coldCheck(t)
	if c.Status != doctor.Fail {
		t.Fatalf("a rejected wallet is %v, want FAIL:\n%s", c.Status,
			strings.Join(c.Lines, "\n"))
	}
	if !strings.Contains(strings.Join(c.Lines, " "), "must not fund a batch") {
		t.Errorf("the refusal does not say what it means:\n%s",
			strings.Join(c.Lines, "\n"))
	}
	if !strings.Contains(strings.Join(c.Fix, " "), "new wallet name") &&
		!strings.Contains(strings.Join(c.Lines, " "), "new wallet name") {

		t.Errorf("the refusal does not say to use a new wallet name — importing a "+
			"corrected pair here leaves the rejected one's coins selectable:\n%s",
			strings.Join(c.Lines, "\n"))
	}
}
