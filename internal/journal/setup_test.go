package journal_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/AusDavo/winthistle/internal/journal"
)

// Regtest descriptors and regtest addresses. Nothing here is mainnet, and
// CLAUDE.md's rule about xpubs applies to a test as much as to a fixture file.
const (
	setupWallet  = "winthistle-cold"
	setupReceive = "wsh(sortedmulti(2,[1b51e4f1/84h/1h/0h]tpubA/0/*,[4cf33624/84h/1h/0h]tpubB/0/*))#aaaaaaaa"
	setupChange  = "wsh(sortedmulti(2,[1b51e4f1/84h/1h/0h]tpubA/1/*,[4cf33624/84h/1h/0h]tpubB/1/*))#bbbbbbbb"
	setupAddr0   = "bcrt1qxqaa0r9vce8ll9q76jqxp7s0u8dz904kdv28aqkd4sl0zj457n7sehqtsu"
	setupChange0 = "bcrt1qlyj3uce4l6zns44nqpz2t0yvx6m2el0lkd74qqrzjk9juzyu34tqv6jpq2"
)

func setupRecord() journal.Setup {
	return journal.Setup{
		Wallet:        setupWallet,
		Outcome:       journal.SetupConfirmed,
		Receive:       setupReceive,
		Change:        setupChange,
		SampleSize:    5,
		FirstReceive:  setupAddr0,
		FirstChange:   setupChange0,
		RescannedFrom: 1699920000,
	}
}

func TestAnUnansweredWalletSaysSo(t *testing.T) {
	j := open(t)
	_, err := j.LatestSetup(context.Background(), setupWallet)
	if !errors.Is(err, journal.ErrNoSetup) {
		t.Fatalf("LatestSetup on a fresh journal: %v, want ErrNoSetup", err)
	}
}

func TestTheAnswerToTheAddressCheckSurvivesTheRoundTrip(t *testing.T) {
	ctx := context.Background()
	j := open(t)

	seq, err := j.RecordSetup(ctx, setupRecord())
	if err != nil {
		t.Fatalf("RecordSetup: %v", err)
	}
	if seq != 1 {
		t.Errorf("first answer got seq %d, want 1", seq)
	}

	got, err := j.LatestSetup(ctx, setupWallet)
	if err != nil {
		t.Fatalf("LatestSetup: %v", err)
	}
	if got.Outcome != journal.SetupConfirmed {
		t.Errorf("outcome %q", got.Outcome)
	}
	if got.Receive != setupReceive || got.Change != setupChange {
		t.Error("the descriptors the answer was about did not survive")
	}
	if got.SampleSize != 5 || got.FirstReceive != setupAddr0 {
		t.Errorf("what was compared did not survive: %+v", got)
	}
	if got.AnsweredAt.IsZero() {
		t.Error("the answer has no timestamp")
	}
}

// TestTheAnswerIsAboutTheDescriptorsItNames is the property the whole table
// exists for. A confirmation that did not say what it was about would age into a
// claim about a wallet it no longer describes — and it would, because Core cannot
// remove a descriptor, so a corrected setup leaves the old pair in place and only
// the recorded strings can tell the two apart.
func TestTheAnswerIsAboutTheDescriptorsItNames(t *testing.T) {
	ctx := context.Background()
	j := open(t)
	if _, err := j.RecordSetup(ctx, setupRecord()); err != nil {
		t.Fatal(err)
	}
	got, err := j.LatestSetup(ctx, setupWallet)
	if err != nil {
		t.Fatal(err)
	}

	if !got.Describes(setupReceive, setupChange) {
		t.Error("the answer does not describe the pair it was recorded against")
	}
	// The same keys, written in the other order. sortedmulti sorts the derived
	// pubkeys, so this derives identical addresses and is a different descriptor
	// string — measured on Core 29, which does not normalise the order. Two
	// records, deliberately: the wallet holds one of them and not the other.
	swapped := strings.Replace(setupReceive, "tpubA/0/*,[4cf33624/84h/1h/0h]tpubB",
		"tpubB/0/*,[4cf33624/84h/1h/0h]tpubA", 1)
	if got.Describes(swapped, setupChange) {
		t.Error("the answer claims to describe a descriptor it was not about")
	}
	if got.Describes(setupReceive, setupReceive) {
		t.Error("the answer ignores the change descriptor")
	}
	var nothing *journal.Setup
	if nothing.Describes(setupReceive, setupChange) {
		t.Error("no answer at all claims to describe a pair")
	}
}

// TestTheLatestAnswerWinsWhateverItIs. A reader that searched for the newest
// *confirmation* would find one from before the descriptors were corrected and
// report a wallet as checked when the last thing a human said about it was no.
func TestTheLatestAnswerWinsWhateverItIs(t *testing.T) {
	ctx := context.Background()
	j := open(t)

	if _, err := j.RecordSetup(ctx, setupRecord()); err != nil {
		t.Fatal(err)
	}
	rejected := setupRecord()
	rejected.Outcome = journal.SetupRejected
	seq, err := j.RecordSetup(ctx, rejected)
	if err != nil {
		t.Fatal(err)
	}
	if seq != 2 {
		t.Errorf("second answer got seq %d, want 2", seq)
	}

	got, err := j.LatestSetup(ctx, setupWallet)
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != journal.SetupRejected {
		t.Errorf("outcome %q, want the later answer", got.Outcome)
	}
	if got.Seq != 2 {
		t.Errorf("seq %d, want 2", got.Seq)
	}

	// And another wallet's answers are its own.
	other := setupRecord()
	other.Wallet = "somebody-elses-wallet"
	if _, err := j.RecordSetup(ctx, other); err != nil {
		t.Fatal(err)
	}
	back, err := j.LatestSetup(ctx, setupWallet)
	if err != nil {
		t.Fatal(err)
	}
	if back.Outcome != journal.SetupRejected {
		t.Error("a different wallet's answer leaked into this one")
	}
}

// TestAnEmptyAnswerIsRefused. Every field here is evidence, and a row missing any
// of it is a record of nothing dressed as a confirmation.
func TestAnEmptyAnswerIsRefused(t *testing.T) {
	ctx := context.Background()
	j := open(t)

	for _, tc := range []struct {
		name   string
		broken func(*journal.Setup)
	}{
		{"no wallet", func(s *journal.Setup) { s.Wallet = "" }},
		{"no outcome", func(s *journal.Setup) { s.Outcome = "" }},
		{"invented outcome", func(s *journal.Setup) { s.Outcome = "probably fine" }},
		{"no receive descriptor", func(s *journal.Setup) { s.Receive = "" }},
		{"no change descriptor", func(s *journal.Setup) { s.Change = "" }},
		{"no addresses", func(s *journal.Setup) { s.FirstReceive = "" }},
		{"no sample", func(s *journal.Setup) { s.SampleSize = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := setupRecord()
			tc.broken(&rec)
			if _, err := j.RecordSetup(ctx, rec); err == nil {
				t.Fatalf("a record with %s was accepted", tc.name)
			}
		})
	}
}

// TestSetupsAreNotRuns. The table has no foreign key to runs on purpose: a setup
// is not on the I-1 state machine, and a recovery path that had to special-case
// it would be a recovery path with three shapes instead of two.
func TestSetupsAreNotRuns(t *testing.T) {
	ctx := context.Background()
	j := open(t)
	if _, err := j.RecordSetup(ctx, setupRecord()); err != nil {
		t.Fatal(err)
	}

	runs, err := j.Unfinished(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Errorf("a setup answer showed up as %d unfinished run(s)", len(runs))
	}
	bumps, err := j.UnfinishedBumps(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(bumps) != 0 {
		t.Errorf("a setup answer showed up as %d unfinished bump(s)", len(bumps))
	}
}
