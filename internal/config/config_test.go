package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AusDavo/winthistle/internal/policy"
)

// write puts a file in a temp directory and returns its path.
func write(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}

const good = `# a comment
[lnd]
address  = "127.0.0.1:10009"
tls_cert = "/creds/tls.cert"
macaroon = "/creds/winthistle.macaroon"

[bitcoind]
address = "127.0.0.1:8332"
cookie  = "/core/.cookie"
wallet  = "winthistle-cold"   # trailing comment

[server]
journal = "/state/runs.db"

[limits]
abort_after_signing_seconds = 240
require_confirmed_inputs    = true

[fees]
floor_sat_per_vb = 2.5
target_blocks    = 12
mode             = "ECONOMICAL"

[[signer]]
label   = "cold1"
command = "sign-with cold1"

[[signer]]
label = "cold2"
`

func TestATypicalFileReadsBack(t *testing.T) {
	cfg, err := Load(write(t, "winthistle.toml", good))
	if err != nil {
		t.Fatalf("reading a well-formed file: %v", err)
	}
	if cfg.LND.Address != "127.0.0.1:10009" {
		t.Errorf("lnd address is %q", cfg.LND.Address)
	}
	if cfg.Bitcoind.Wallet != "winthistle-cold" {
		t.Errorf("a trailing comment leaked into the wallet name: %q", cfg.Bitcoind.Wallet)
	}
	if cfg.Limits.AbortAfterSigning != 4*time.Minute {
		t.Errorf("the abort gate is %s", cfg.Limits.AbortAfterSigning)
	}
	if cfg.Limits.MinConfirmations() != 1 {
		t.Error("require_confirmed_inputs did not become a confirmation floor")
	}
	if cfg.Fees.FloorSatPerVB != 2.5 || cfg.Fees.TargetBlocks != 12 {
		t.Errorf("fees read back as %+v", cfg.Fees)
	}
	if len(cfg.Signers) != 2 || cfg.Signers[0].Label != "cold1" ||
		cfg.Signers[1].Command != "" {

		t.Errorf("signers read back as %+v", cfg.Signers)
	}
}

// TestTheDefaultsAreTheOnesTheCodeAlreadyHas.
//
// The abort gate in particular: docs/design.html puts 300 in the config block
// and rehearsal.DefaultAbortAfterSigning is the constant the gate compares
// against. Two numbers that must agree are one number, and this is where a
// second one would show up.
func TestTheDefaultsAreTheOnesTheCodeAlreadyHas(t *testing.T) {
	cfg, err := Load(write(t, "winthistle.toml", `
[lnd]
address  = "127.0.0.1:10009"
tls_cert = "/creds/tls.cert"
macaroon = "/creds/m.macaroon"

[bitcoind]
address = "127.0.0.1:8332"
cookie  = "/core/.cookie"
wallet  = "cold"

[server]
journal = "/state/runs.db"
`))
	if err != nil {
		t.Fatalf("reading a minimal file: %v", err)
	}
	if cfg.Limits.AbortAfterSigning != DefaultAbortAfter {
		t.Errorf("the abort gate defaulted to %s, not %s",
			cfg.Limits.AbortAfterSigning, DefaultAbortAfter)
	}
	if !cfg.Limits.RequireConfirmedInputs {
		t.Error("require_confirmed_inputs defaulted to false. An unconfirmed " +
			"parent can be replaced, which moves our input, which moves the txid")
	}
	// The one key with no default, on purpose.
	if cfg.Fees.FloorSatPerVB != 0 {
		t.Errorf("the fee floor defaulted to %g. It must not: a node with no "+
			"estimate and no floor is an error rather than a guess",
			cfg.Fees.FloorSatPerVB)
	}
}

// TestAllowRBFIsRefusedRatherThanIgnored.
//
// It is in docs/design.html's example block, so an operator will paste it. I-4
// means nothing reads a preference about replaceability, and the two ways to
// deal with a key nothing reads are both worse than refusing it: honouring it
// would be a lie, and ignoring it silently would let somebody write
// allow_rbf = true, watch the run proceed, and conclude it had been honoured.
func TestAllowRBFIsRefusedRatherThanIgnored(t *testing.T) {
	for _, value := range []string{"true", "false"} {
		_, err := Load(write(t, "winthistle.toml", `
[lnd]
address  = "127.0.0.1:10009"
tls_cert = "/creds/tls.cert"
macaroon = "/creds/m.macaroon"

[bitcoind]
address = "127.0.0.1:8332"
cookie  = "/core/.cookie"
wallet  = "cold"

[server]
journal = "/state/runs.db"

[limits]
allow_rbf = `+value+`
`))
		if err == nil {
			t.Fatalf("allow_rbf = %s was accepted", value)
		}
		if !strings.Contains(err.Error(), "allow_rbf is not a setting") {
			t.Errorf("allow_rbf = %s was refused for the wrong reason: %v", value, err)
		}
		if !strings.Contains(err.Error(), "I-4") {
			t.Errorf("the refusal does not say why: %v", err)
		}
	}
}

// TestAMisspelledKeyIsAnErrorRatherThanADefault is the reason this reader
// exists at all. The gate is read once, before a cold wallet comes out, and a
// file that silently defaults it has told the operator nothing.
func TestAMisspelledKeyIsAnErrorRatherThanADefault(t *testing.T) {
	_, err := Load(write(t, "winthistle.toml", strings.Replace(good,
		"abort_after_signing_seconds", "abort_after_signing_second", 1)))
	if err == nil {
		t.Fatal("a misspelled key was accepted, and the gate silently defaulted")
	}
	if !strings.Contains(err.Error(), "abort_after_signing_second") {
		t.Errorf("the refusal does not name the key: %v", err)
	}
}

func TestWhatElseIsRefused(t *testing.T) {
	cases := map[string]struct{ body, want string }{
		"an unknown section": {
			body: good + "\n[wallet]\nname = \"x\"\n", want: "not a section",
		},
		"a key that was retired": {
			body: strings.Replace(good, "[server]",
				"[server]\nbind = \"127.0.0.1:7420\"", 1),
			want: "there is no socket left for this to name",
		},
		"admin.macaroon": {
			body: strings.Replace(good, "/creds/winthistle.macaroon",
				"/creds/admin.macaroon", 1),
			want: "admin.macaroon",
		},
		"no credentials for core": {
			body: strings.Replace(good, `cookie  = "/core/.cookie"`, "", 1),
			want: "cookie, or user and pass",
		},
		"a gate longer than the peers' window": {
			body: strings.Replace(good, "abort_after_signing_seconds = 240",
				"abort_after_signing_seconds = 900", 1),
			want: "the whole of the peers'",
		},
		"a mode Core does not have": {
			body: strings.Replace(good, `"ECONOMICAL"`, `"CHEAPEST"`, 1),
			want: "Core has two",
		},
		"a key set twice": {
			body: strings.Replace(good, "[server]",
				"[server]\njournal = \"/a.db\"", 1),
			want: "set twice",
		},
		"two signers with one name": {
			body: strings.Replace(good, `label = "cold2"`, `label = "cold1"`, 1),
			want: "two signers are called",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Load(write(t, "winthistle.toml", tc.body))
			if err == nil {
				t.Fatalf("%s was accepted", name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refused for the wrong reason: %v", err)
			}
		})
	}
}

// TestEveryProblemIsReportedAtOnce. A config file is edited in a text editor
// and re-run, and a validator that reports one problem per attempt turns one
// edit into five.
func TestEveryProblemIsReportedAtOnce(t *testing.T) {
	_, err := Load(write(t, "winthistle.toml", `
[lnd]
address = "127.0.0.1:10009"

[bitcoind]
address = "127.0.0.1:8332"

[server]
journal = "/state/runs.db"
`))
	if err == nil {
		t.Fatal("a file missing four required keys was accepted")
	}
	var invalid *Invalid
	if !asInvalid(err, &invalid) {
		t.Fatalf("the error is not an *Invalid: %T", err)
	}
	if len(invalid.Problems) < 4 {
		t.Errorf("reported %d problems, expected at least 4:\n%v",
			len(invalid.Problems), invalid.Problems)
	}
}

func asInvalid(err error, target **Invalid) bool {
	if v, ok := err.(*Invalid); ok {
		*target = v
		return true
	}
	return false
}

// TestPathsResolveAgainstTheFile, not against the working directory. Otherwise
// `winthistle doctor` and `cd /; winthistle doctor` read different credentials.
func TestPathsResolveAgainstTheFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "winthistle.toml")
	body := strings.Replace(good, `tls_cert = "/creds/tls.cert"`,
		`tls_cert = "creds/tls.cert"`, 1)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if want := filepath.Join(dir, "creds", "tls.cert"); cfg.LND.TLSCert != want {
		t.Errorf("tls_cert resolved to %q, expected %q", cfg.LND.TLSCert, want)
	}
}

const goodBatch = `[policy]
base_fee_msat   = 0
fee_rate_ppm    = 250
time_lock_delta = 144

[[channel]]
peer       = "0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798"
amount_sat = 5_000_000

[[channel]]
peer         = "02c6047f9441ed7d6d3045406e95c07cd85c778e4b8cef3ca7abac09b95c709ee5"
amount_sat   = 250000
private      = true
fee_rate_ppm = 900
`

func TestABatchInheritsThePolicyAndOverridesIt(t *testing.T) {
	b, err := LoadBatch(write(t, "batch.toml", goodBatch))
	if err != nil {
		t.Fatalf("reading a well-formed batch: %v", err)
	}
	if len(b.Channels) != 2 {
		t.Fatalf("read %d channels", len(b.Channels))
	}
	if got := b.Channels[0].Policy.FeeRatePPM; got != 250 {
		t.Errorf("channel 1 inherited %d ppm, expected the default 250", got)
	}
	if got := b.Channels[1].Policy.FeeRatePPM; got != 900 {
		t.Errorf("channel 2's override did not apply: %d ppm", got)
	}
	if got := b.Channels[1].Policy.TimeLockDelta; got != 144 {
		t.Errorf("channel 2 lost the inherited CLTV delta: %d", got)
	}
	if !b.Channels[1].Private {
		t.Error("private did not read back")
	}
	if b.TotalSat() != 5_250_000 {
		t.Errorf("total is %d", b.TotalSat())
	}
	if got := b.AmountsSat(); len(got) != 2 || got[0] != 5_000_000 {
		t.Errorf("amounts are %v", got)
	}
}

// TestAPolicyLNDWouldRefuseIsRefusedHere. UpdateChannelPolicy reports an
// invalid parameter inside a *successful* response, so by Phase 2 the
// transaction is public and the channel routes at 1 ppm until a human notices.
func TestAPolicyLNDWouldRefuseIsRefusedHere(t *testing.T) {
	body := strings.Replace(goodBatch, "time_lock_delta = 144",
		"time_lock_delta = 4", 1)
	_, err := LoadBatch(write(t, "batch.toml", body))
	if err == nil {
		t.Fatal("a CLTV delta of 4 was accepted")
	}
	if !strings.Contains(err.Error(), "below LND's minimum") {
		t.Errorf("refused for the wrong reason: %v", err)
	}
	if policy.MinTimeLockDelta != 18 {
		t.Errorf("routing.MinCLTVDelta is recorded as %d", policy.MinTimeLockDelta)
	}
}

func TestABatchWithNothingInItIsRefused(t *testing.T) {
	_, err := LoadBatch(write(t, "batch.toml", "[policy]\nfee_rate_ppm = 1\n"))
	if err == nil || !strings.Contains(err.Error(), "no [[channel]]") {
		t.Errorf("an empty batch was accepted, or refused oddly: %v", err)
	}
}

func TestABadPubkeyIsCaughtBeforeAnythingOpens(t *testing.T) {
	// A well-formed 33 bytes that is not a point on the curve — the residue
	// after hex and length have both passed, and the one a length check misses.
	body := strings.Replace(goodBatch,
		"0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798",
		"02ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff", 1)
	_, err := LoadBatch(write(t, "batch.toml", body))
	if err == nil {
		t.Fatal("a pubkey that is not on the curve was accepted")
	}
}
