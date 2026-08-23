package coldwallet

import (
	"strings"
	"testing"
	"time"

	"github.com/AusDavo/winthistle/internal/bitcoind"
)

// A captured sample of what regtest/cold-wallet.py produces: a 2-of-2
// wsh(sortedmulti) over two tpubs. The script generates fresh keys on every
// `make harness`, so these are not the live harness's — they do not need to be,
// since nothing in this file talks to a node. The live pair is read back from
// cold-watch in coldwallet_regtest_test.go.
//
// Regtest keys, deliberately. CLAUDE.md forbids a mainnet xpub in a fixture, and
// a pasted one would sail straight past .gitignore, which blocks descriptor
// *files* and not inline strings.
const (
	harnessReceive = "wsh(sortedmulti(2," +
		"[1b51e4f1/84h/1h/0h]tpubDDcpACxNfy7FEgFAGDz9NXiH1VW4Tci2hFTaYkBf6uCWHXhGgNfKbx6tEQMRnBHt8n3FK8r3dtMeqMW92xmdvjfGL9iS8zNJysCvxraG7A2/0/*," +
		"[4cf33624/84h/1h/0h]tpubDCfUM4YyLJvf8BStcHGDoj7L9c6eXWqtyaK9LYsxCXNrmAxBvN3wHQ2UM77qv2Sb8hskEDDHkEvhDrgvqEu2we3HRMVrsSECvRsQUhS7eMb/0/*))"
	harnessChange = "wsh(sortedmulti(2," +
		"[1b51e4f1/84h/1h/0h]tpubDDcpACxNfy7FEgFAGDz9NXiH1VW4Tci2hFTaYkBf6uCWHXhGgNfKbx6tEQMRnBHt8n3FK8r3dtMeqMW92xmdvjfGL9iS8zNJysCvxraG7A2/1/*," +
		"[4cf33624/84h/1h/0h]tpubDCfUM4YyLJvf8BStcHGDoj7L9c6eXWqtyaK9LYsxCXNrmAxBvN3wHQ2UM77qv2Sb8hskEDDHkEvhDrgvqEu2we3HRMVrsSECvRsQUhS7eMb/1/*))"
)

var testBirthday = time.Date(2023, 11, 14, 0, 0, 0, 0, time.UTC)

func goodConfig() Config {
	return Config{
		WalletName: "winthistle-cold",
		Receive:    harnessReceive,
		Change:     harnessChange,
		Birthday:   testBirthday,
	}
}

func fatalConcerns(cs []Concern) []Concern {
	var out []Concern
	for _, c := range cs {
		if c.Fatal {
			out = append(out, c)
		}
	}
	return out
}

func mentions(cs []Concern, substr string) bool {
	for _, c := range cs {
		if strings.Contains(c.Headline+" "+c.Detail, substr) {
			return true
		}
	}
	return false
}

func TestAGoodConfigRaisesNothing(t *testing.T) {
	if cs := goodConfig().Validate(); len(cs) != 0 {
		t.Fatalf("the harness's own descriptors raised %d concern(s): %v", len(cs), cs)
	}
}

// TestABirthdayOfNowIsTheTrap. The design names two setup traps and this is the
// first: importdescriptors accepts "now", the wallet imports cleanly, and it
// reports a zero balance because Core never scanned the blocks the coins arrived
// in. bitcoind.ImportRequest cannot express the string at all, so what is left
// to catch is a caller passing today's date for a wallet that is older.
func TestABirthdayOfNowIsTheTrap(t *testing.T) {
	cfg := goodConfig()
	cfg.Birthday = time.Now()
	cs := cfg.Validate()
	if len(fatalConcerns(cs)) != 0 {
		t.Errorf("today's date was refused outright; a cold wallet created today "+
			"has a birthday of today: %v", fatalConcerns(cs))
	}
	if !mentions(cs, "within the last week") {
		t.Errorf("a birthday of now went unremarked: %v", cs)
	}

	cfg.Birthday = time.Time{}
	if len(fatalConcerns(cfg.Validate())) == 0 {
		t.Error("a config with no birthday at all was accepted")
	}

	cfg.Birthday = time.Now().Add(48 * time.Hour)
	if len(fatalConcerns(cfg.Validate())) == 0 {
		t.Error("a birthday in the future was accepted; it would scan nothing")
	}
}

func TestGenesisIsHowYouSayScanEverything(t *testing.T) {
	cfg := goodConfig()
	cfg.Birthday = Genesis
	if cs := fatalConcerns(cfg.Validate()); len(cs) != 0 {
		t.Fatalf("Genesis was refused: %v", cs)
	}
	if ts := cfg.timestamp(); ts != Genesis.Unix() {
		t.Errorf("Genesis rendered as timestamp %d, want %d", ts, Genesis.Unix())
	}
	cfg.Birthday = Genesis.Add(-time.Hour)
	if ts := cfg.timestamp(); ts != 0 {
		t.Errorf("a pre-genesis birthday rendered as %d, want 0 — Core reads 0 as "+
			"the genesis block", ts)
	}
}

// TestPlainMultiIsFlaggedButNotRefused. The second trap. Both are valid
// descriptors and only the operator knows which their wallet uses, so this can
// be said loudly and cannot be decided here — which is exactly why setup ends in
// the address check.
func TestPlainMultiIsFlaggedButNotRefused(t *testing.T) {
	cfg := goodConfig()
	cfg.Receive = strings.Replace(cfg.Receive, "sortedmulti(", "multi(", 1)
	cfg.Change = strings.Replace(cfg.Change, "sortedmulti(", "multi(", 1)

	cs := cfg.Validate()
	if len(fatalConcerns(cs)) != 0 {
		t.Errorf("multi() was refused outright, but it is a legitimate descriptor: %v",
			fatalConcerns(cs))
	}
	if !mentions(cs, "sortedmulti()") {
		t.Errorf("multi() went unremarked: %v", cs)
	}
}

func TestAPrivateKeyIsRefusedBeforeItLeavesTheProcess(t *testing.T) {
	cfg := goodConfig()
	cfg.Receive = strings.Replace(cfg.Receive, "tpubDDcpACxNfy7", "tprvDDcpACxNfy7", 1)
	cs := fatalConcerns(cfg.Validate())
	if len(cs) == 0 {
		t.Fatal("a descriptor carrying a private key was accepted")
	}
	if !mentions(cs, "not been sent anywhere") {
		t.Errorf("the refusal does not say the key stayed local: %v", cs)
	}
}

func TestAnUnrangedDescriptorIsRefused(t *testing.T) {
	cfg := goodConfig()
	cfg.Receive = strings.ReplaceAll(cfg.Receive, "/0/*", "/0/0")
	if len(fatalConcerns(cfg.Validate())) == 0 {
		t.Error("a descriptor describing one address was accepted as a branch")
	}
}

func TestIdenticalReceiveAndChangeAreRefused(t *testing.T) {
	cfg := goodConfig()
	cfg.Change = cfg.Receive
	if len(fatalConcerns(cfg.Validate())) == 0 {
		t.Error("a wallet with no internal branch was accepted")
	}
}

func TestSwappedBranchesAreFlagged(t *testing.T) {
	cfg := goodConfig()
	cfg.Receive, cfg.Change = harnessChange, harnessReceive
	cs := cfg.Validate()
	if len(fatalConcerns(cs)) != 0 {
		t.Errorf("a swap was refused outright; some wallets do use other branches: %v",
			fatalConcerns(cs))
	}
	if !mentions(cs, "branch /1/*") || !mentions(cs, "branch /0/*") {
		t.Errorf("a receive/change swap went unremarked: %v", cs)
	}
}

func TestMissingKeyOriginIsFlagged(t *testing.T) {
	cfg := goodConfig()
	cfg.Receive = strings.Replace(cfg.Receive, "[1b51e4f1/84h/1h/0h]", "", 1)
	if !mentions(cfg.Validate(), "origin prefix") {
		t.Errorf("a key with no origin went unremarked: %v", cfg.Validate())
	}
}

func TestAWalletNameWithAPathSeparatorIsRefused(t *testing.T) {
	cfg := goodConfig()
	cfg.WalletName = "../winthistle"
	if len(fatalConcerns(cfg.Validate())) == 0 {
		t.Error("a wallet name Core would read as a directory was accepted")
	}
}

func TestInspectReadsTheHarnessDescriptor(t *testing.T) {
	s := Inspect(harnessReceive)
	if !s.SortedMulti || s.PlainMulti {
		t.Errorf("sortedmulti misread: %+v", s)
	}
	if !s.Ranged || s.HasPrivate {
		t.Errorf("shape misread: %+v", s)
	}
	if s.Keys != 2 || s.Origins != 2 {
		t.Errorf("expected 2 keys with 2 origins, got %d and %d", s.Keys, s.Origins)
	}
	if b, ok := s.SoleBranch(); !ok || b != 0 {
		t.Errorf("branch read as %d (%v), want 0", b, ok)
	}
	if b, ok := Inspect(harnessChange).SoleBranch(); !ok || b != 1 {
		t.Errorf("change branch read as %d (%v), want 1", b, ok)
	}
}

// TestInspectCountsARawPubkeyDescriptor. Core hands descriptors back with keys
// expanded to raw pubkeys — see listunspent's "desc" field — so the shape reader
// has to count those too, or a read-back descriptor looks like it has no keys.
func TestInspectCountsARawPubkeyDescriptor(t *testing.T) {
	desc := "wsh(multi(2," +
		"[4cf33624/84h/1h/0h/0/2]023ba01b91e4e51b1c53dfe03d464d840e60f30c63dd543dcc8373383c26379683," +
		"[1b51e4f1/84h/1h/0h/0/2]034660a86a8d82e2901f325b55ddc2db187248851ffab984386dcb93f8e4c801b4))#pq2v4gzz"
	s := Inspect(desc)
	if s.Keys != 2 {
		t.Errorf("counted %d keys in an expanded descriptor, want 2", s.Keys)
	}
	if s.Origins != 2 {
		t.Errorf("counted %d origins, want 2", s.Origins)
	}
	if !s.PlainMulti || s.SortedMulti {
		t.Errorf("expanded multi() misread: %+v", s)
	}
}

// ---------------------------------------------------------------------------
// Coin selection
// ---------------------------------------------------------------------------

func TestLegacyCoinsAreLeftOutAndNamed(t *testing.T) {
	utxos := map[string]bitcoind.UTXO{
		// wsh: the cold wallet's own.
		"native segwit": {ScriptPubKey: "0020" + strings.Repeat("11", 32),
			Confirmations: 6, Solvable: true, Safe: true, Amount: 1},
		// p2pkh: legacy, and the reason I-3 exists.
		"p2pkh": {ScriptPubKey: "76a914" + strings.Repeat("22", 20) + "88ac",
			Confirmations: 6, Solvable: true, Safe: true, Amount: 2},
		// p2sh wrapping a witness program: a SegWit spend after all.
		"wrapped segwit": {ScriptPubKey: "a914" + strings.Repeat("33", 20) + "87",
			RedeemScript:  "0014" + strings.Repeat("44", 20),
			Confirmations: 6, Solvable: true, Safe: true, Amount: 3},
		// p2sh with a legacy redeem script.
		"wrapped legacy": {ScriptPubKey: "a914" + strings.Repeat("55", 20) + "87",
			RedeemScript:  "5121" + strings.Repeat("66", 33) + "51ae",
			Confirmations: 6, Solvable: true, Safe: true, Amount: 4},
		// taproot.
		"taproot": {ScriptPubKey: "5120" + strings.Repeat("77", 32),
			Confirmations: 6, Solvable: true, Safe: true, Amount: 5},
	}
	want := map[string]bool{
		"native segwit": true, "p2pkh": false, "wrapped segwit": true,
		"wrapped legacy": false, "taproot": true,
	}
	for name, u := range utxos {
		got, detail := segwitSpend(u.ScriptPubKey, u.RedeemScript)
		if got != want[name] {
			t.Errorf("%s: segwit=%v, want %v (%s)", name, got, want[name], detail)
		}
		if !got && detail == "" {
			t.Errorf("%s was excluded without a reason", name)
		}
	}
}

func TestAP2SHCoinWithNoRedeemScriptIsNotAssumedSegwit(t *testing.T) {
	ok, detail := segwitSpend("a914"+strings.Repeat("33", 20)+"87", "")
	if ok {
		t.Fatal("a P2SH coin with no redeem script was taken for a wrapped SegWit spend")
	}
	if !strings.Contains(detail, "redeem script") {
		t.Errorf("unhelpful reason: %s", detail)
	}
}

func TestExclusionReasonsAreDistinguished(t *testing.T) {
	segwit := "0020" + strings.Repeat("11", 32)
	cases := []struct {
		name string
		u    bitcoind.UTXO
		why  Why
	}{
		{"legacy", bitcoind.UTXO{ScriptPubKey: "76a914" + strings.Repeat("22", 20) + "88ac",
			Confirmations: 6, Solvable: true, Safe: true}, Legacy},
		{"unsolvable", bitcoind.UTXO{ScriptPubKey: segwit,
			Confirmations: 6, Solvable: false, Safe: true}, Unsolvable},
		{"unconfirmed", bitcoind.UTXO{ScriptPubKey: segwit,
			Confirmations: 0, Solvable: true, Safe: true}, Unconfirmed},
		{"unsafe", bitcoind.UTXO{ScriptPubKey: segwit,
			Confirmations: 6, Solvable: true, Safe: false}, Unsafe},
	}
	for _, c := range cases {
		why, _, excluded := excludes(c.u, 1)
		if !excluded {
			t.Errorf("%s was not excluded", c.name)
			continue
		}
		if why != c.why {
			t.Errorf("%s excluded as %v, want %v", c.name, why, c.why)
		}
	}
	fine := bitcoind.UTXO{ScriptPubKey: segwit, Confirmations: 1, Solvable: true, Safe: true}
	if _, _, excluded := excludes(fine, 1); excluded {
		t.Error("a confirmed, solvable, safe segwit coin was excluded")
	}
}

func TestSatFromBTCRoundsAtTheSatoshi(t *testing.T) {
	cases := map[float64]int64{
		0.1: 10_000_000, 2: 200_000_000, 0.00000001: 1, 1.23456789: 123_456_789,
	}
	for in, want := range cases {
		if got := satFromBTC(in); got != want {
			t.Errorf("satFromBTC(%v) = %d, want %d", in, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Pre-flight and copy
// ---------------------------------------------------------------------------

func TestVersionRendersCoresInteger(t *testing.T) {
	cases := map[int]string{290000: "29.0", 250000: "25.0", 250100: "25.1", 240001: "24.0.1"}
	for in, want := range cases {
		if got := version(in); got != want {
			t.Errorf("version(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestAPrunedNodeIsToldToUseAssistedMode(t *testing.T) {
	p := Preflight{
		Reason: PrunedPastBirthday, Version: 290000, Chain: "main", Height: 870_000,
		Pruned: true, PruneHeight: 800_000,
		PruneHorizon: time.Date(2023, 12, 1, 0, 0, 0, 0, time.UTC),
		Birthday:     testBirthday,
	}
	if p.Available() {
		t.Fatal("a node pruned past the birthday reported itself available")
	}
	report := p.Report()
	if !strings.Contains(report, "assisted mode") {
		t.Errorf("the operator is not pointed at the mode that works:\n%s", report)
	}
	if !strings.Contains(report, "800000") && !strings.Contains(report, "800,000") {
		t.Errorf("the report does not say where the node is pruned to:\n%s", report)
	}
}

func TestPreflightReportsStayInThePane(t *testing.T) {
	reasons := []Reason{PrunedPastBirthday, NoWalletSupport, TooOld, StillSyncing, Ready}
	for _, r := range reasons {
		p := Preflight{Reason: r, Version: 240000, Chain: "main", Height: 870_000,
			Pruned: true, PruneHeight: 800_000, VerificationProgress: 0.93,
			PruneHorizon: time.Now(), Birthday: testBirthday}
		for i, line := range strings.Split(p.Report(), "\n") {
			if n := len([]rune(line)); n > 80 {
				t.Errorf("%v report, line %d is %d columns:\n%s", r, i+1, n, line)
			}
		}
	}
}

func TestTheAddressCheckSaysWhatItCannotSettle(t *testing.T) {
	a := AddressCheck{
		WalletName:  "winthistle-cold",
		ReceiveDesc: harnessReceive,
		ChangeDesc:  harnessChange,
		Receive: []Derived{
			{Index: 0, Address: "bcrt1qexamplereceive0", Mine: true, Solvable: true, FromImported: true},
		},
		Change: []Derived{
			{Index: 0, Address: "bcrt1qexamplechange0", Mine: true, Solvable: true, FromImported: true},
		},
	}
	if !a.Consistent() {
		t.Fatal("a wholly consistent check reported itself inconsistent")
	}
	report := a.Report()
	for _, want := range []string{"bcrt1qexamplereceive0", "bcrt1qexamplechange0",
		"your own wallet software", "sortedmulti()"} {
		if !strings.Contains(report, want) {
			t.Errorf("the address check does not mention %q:\n%s", want, report)
		}
	}
	if strings.Contains(strings.ToLower(report), "success") {
		t.Errorf("the address check reports success; it is supposed to ask a "+
			"question:\n%s", report)
	}
	for i, line := range strings.Split(report, "\n") {
		if n := len([]rune(line)); n > 80 {
			t.Errorf("line %d is %d columns:\n%s", i+1, n, line)
		}
	}
}

func TestAnInconsistentCheckSaysSo(t *testing.T) {
	a := AddressCheck{
		Receive: []Derived{{Index: 0, Address: "bcrt1qa", Mine: false}},
		Change:  []Derived{{Index: 0, Address: "bcrt1qb", Mine: true, Solvable: true, FromImported: true}},
	}
	if a.Consistent() {
		t.Fatal("an address the wallet does not recognise passed")
	}
	if !strings.Contains(a.Report(), "does not recognise") {
		t.Errorf("the report does not flag it:\n%s", a.Report())
	}
}

// TestInstallNeverReportsSuccess. The verdict of a completed install is a
// question, and nothing in this package returns anything stronger.
func TestInstallNeverReportsSuccess(t *testing.T) {
	r := &Result{Config: goodConfig(), Preflight: Preflight{Reason: Ready}}
	if r.Verdict() != AwaitingAddressCheck {
		t.Errorf("a clean install reported %v", r.Verdict())
	}
}

func TestScanProgressCopesWithBothOfCoresShapes(t *testing.T) {
	var s bitcoind.ScanProgress
	if err := s.UnmarshalJSON([]byte("false")); err != nil || s.Running {
		t.Errorf("scanning=false: %+v, %v", s, err)
	}
	if err := s.UnmarshalJSON([]byte(`{"duration":12,"progress":0.5}`)); err != nil {
		t.Fatalf("scanning={...}: %v", err)
	}
	if !s.Running || s.Duration != 12 || s.Progress != 0.5 {
		t.Errorf("scanning object misread: %+v", s)
	}
}

// TestAMultipathDescriptorIsRefused.
//
// This is the modern shape of the design's second setup trap. Most wallet
// software now exports one descriptor with a <0;1> step in it, and Core parses
// it — getdescriptorinfo answers with a multipath_expansion array and a
// checksum for the whole thing, but its `descriptor` field is the *first branch
// alone*. So the obvious use of it imports the receive branch twice: once where
// it belongs and once as the wallet's change branch, which would derive change
// to the addresses the batch is also funded from.
//
// Measured on Core 29 against the harness's own pair. It is fatal rather than a
// warning because there is nothing to weigh: the descriptor cannot be imported
// as given (importdescriptors refuses a multipath entry that also specifies
// `internal`), and the thing Core would silently accept instead is wrong.
func TestAMultipathDescriptorIsRefused(t *testing.T) {
	multi := strings.ReplaceAll(harnessReceive, "/0/*", "/<0;1>/*")
	cfg := goodConfig()
	cfg.Receive = multi

	cs := cfg.Validate()
	if len(fatalConcerns(cs)) == 0 {
		t.Fatalf("a <0;1> descriptor was accepted:\n%+v", cs)
	}
	if !mentions(cs, "more than one branch") {
		t.Errorf("the refusal does not say what is wrong:\n%+v", cs)
	}
	if !mentions(cs, "keeps only the first branch") {
		t.Errorf("the refusal does not say what Core would do with it:\n%+v", cs)
	}
	if !Inspect(multi).Multipath {
		t.Error("Inspect does not see the multipath step")
	}
	if Inspect(harnessReceive).Multipath {
		t.Error("Inspect sees a multipath step in an ordinary descriptor")
	}
}

// TestAGapLimitBelowTheSampleIsRefused is the one way this build could make the
// round-trip check fail on a wallet that is entirely correct, so it is refused
// before anything is created.
//
// The import covers [0, gap limit] and the check derives 0 to sample-1. An
// address outside the imported range is not in the wallet at all: measured on the
// harness, getaddressinfo for index 5000 of a wallet imported to [0,1132]
// answers ismine false, solvable false and no parent descriptor — the same three
// flags a wrong descriptor produces, on the same screen, with nothing to tell the
// operator which one they are looking at.
func TestAGapLimitBelowTheSampleIsRefused(t *testing.T) {
	cfg := goodConfig()
	cfg.GapLimit = 3
	cfg.SampleSize = 5

	cs := cfg.Validate()
	if len(fatalConcerns(cs)) == 0 {
		t.Fatalf("a gap limit of 3 with a sample of 5 was accepted:\n%+v", cs)
	}
	if !mentions(cs, "does not reach") {
		t.Errorf("the refusal does not say what will not reach what:\n%+v", cs)
	}

	// The boundary: [0,4] is exactly five addresses.
	cfg.GapLimit = 4
	if cs := fatalConcerns(cfg.Validate()); len(cs) != 0 {
		t.Errorf("a gap limit of 4 with a sample of 5 was refused:\n%+v", cs)
	}

	// And the defaults must never collide, whichever way they move.
	if DefaultGapLimit+1 < DefaultSampleSize {
		t.Errorf("DefaultGapLimit %d cannot cover DefaultSampleSize %d",
			DefaultGapLimit, DefaultSampleSize)
	}
}
