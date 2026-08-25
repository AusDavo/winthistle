package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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

[journal]
path = "/state/runs.db"   # trailing comment

[limits]
require_confirmed_inputs = true

[fees]
target_sat_per_vb = 2.5

`

func TestATypicalFileReadsBack(t *testing.T) {
	cfg, err := Load(write(t, "winthistle.toml", good))
	if err != nil {
		t.Fatalf("reading a well-formed file: %v", err)
	}
	if cfg.LND.Address != "127.0.0.1:10009" {
		t.Errorf("lnd address is %q", cfg.LND.Address)
	}
	if cfg.Journal.Path != "/state/runs.db" {
		t.Errorf("a trailing comment leaked into the journal path: %q", cfg.Journal.Path)
	}
	if cfg.Limits.MinConfirmations() != 1 {
		t.Error("require_confirmed_inputs did not become a confirmation floor")
	}
	if cfg.Fees.TargetSatPerVB != 2.5 {
		t.Errorf("fees read back as %+v", cfg.Fees)
	}
}

// TestTheDefaultsAreTheOnesTheCodeAlreadyHas.
//
// Two numbers that must agree are one number, and this is where a second one
// would show up. It used to guard the abort gate, whose default was written both
// in docs/design.html's config block and in the constant the gate compared
// against; that key is retired, and what is left to guard is the confirmation
// floor and the journal's path.
func TestTheDefaultsAreTheOnesTheCodeAlreadyHas(t *testing.T) {
	cfg, err := Load(write(t, "winthistle.toml", `
[lnd]
address  = "127.0.0.1:10009"
tls_cert = "/creds/tls.cert"
macaroon = "/creds/m.macaroon"

[journal]
path = "/state/runs.db"

[fees]
target_sat_per_vb = 12.0
`))
	if err != nil {
		t.Fatalf("reading a minimal file: %v", err)
	}
	if !cfg.Limits.RequireConfirmedInputs {
		t.Error("require_confirmed_inputs defaulted to false. An unconfirmed " +
			"parent can be replaced, which moves our input, which moves the txid")
	}
	if cfg.Journal.Path != "/state/runs.db" {
		t.Errorf("the journal path read back as %q", cfg.Journal.Path)
	}
}

// TestAMissingFeeRateIsRefusedRatherThanDefaultedToZero.
//
// The one key with no default, and the reason is the whole of item 5's first
// decision. Core's estimatesmartfee answered this until Core was removed, and
// CLAUDE.md forbids the obvious substitute, so the number is declared. A
// declared number that quietly becomes zero is the worst default this tool
// could ship: the fee is the one figure in a batch with no right answer, and
// I-4 means a batch built at the wrong one cannot be corrected by replacing it.
//
// Two independent refusals stand between a missing number and a batch built
// against nothing — this one, and plan.Build's on a non-positive target. This
// is the one that fires before LND is dialled.
func TestAMissingFeeRateIsRefusedRatherThanDefaultedToZero(t *testing.T) {
	body := strings.Replace(good, "target_sat_per_vb = 2.5", "", 1)
	_, err := Load(write(t, "winthistle.toml", body))
	if err == nil {
		t.Fatal("a file with no fee rate loaded, and the rate is now zero")
	}
	if !strings.Contains(err.Error(), "target_sat_per_vb is required") {
		t.Errorf("the refusal does not name the key: %v", err)
	}
	if !strings.Contains(err.Error(), "I-4") {
		t.Errorf("the refusal does not say why it cannot be corrected later: %v", err)
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

[journal]
path = "/state/runs.db"

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
// exists at all. The file is read once, at the start of a run, and a file that
// silently defaults a key has told the operator nothing.
//
// The specimen used to be abort_after_signing_seconds, which was the gate and is
// now retired. require_confirmed_inputs is the right replacement rather than an
// arbitrary one: it is the key whose silent default would be worst. Misspell it
// and the floor stays at one confirmation, which is the safe direction — but a
// batch built on unconfirmed inputs is a batch whose parent can be replaced,
// which moves an input, which moves the txid, which destroys every channel in
// it. A key that can only fail safe today is one nobody checks tomorrow.
func TestAMisspelledKeyIsAnErrorRatherThanADefault(t *testing.T) {
	_, err := Load(write(t, "winthistle.toml", strings.Replace(good,
		"require_confirmed_inputs", "require_confirmed_input", 1)))
	if err == nil {
		t.Fatal("a misspelled key was accepted, and the floor silently defaulted")
	}
	if !strings.Contains(err.Error(), "require_confirmed_input") {
		t.Errorf("the refusal does not name the key: %v", err)
	}
}

func TestWhatElseIsRefused(t *testing.T) {
	cases := map[string]struct{ body, want string }{
		"an unknown section": {
			body: good + "\n[wallet]\nname = \"x\"\n", want: "not a section",
		},
		"a section that was renamed": {
			body: good + "\n[server]\njournal = \"/state/runs.db\"\n",
			want: "is [journal] path now",
		},
		"admin.macaroon": {
			body: strings.Replace(good, "/creds/winthistle.macaroon",
				"/creds/admin.macaroon", 1),
			want: "admin.macaroon",
		},
		"a fourth section that was retired": {
			body: good + "\n[bitcoind]\naddress = \"127.0.0.1:8332\"\n",
			want: "dials no Bitcoin node",
		},
		"a second key that was retired": {
			body: strings.Replace(good, "[limits]",
				"[limits]\nabort_after_signing_seconds = 300", 1),
			want: "no longer contains a signing round",
		},
		"a third key that was retired": {
			body: strings.Replace(good, "[fees]", "[fees]\nmode = \"ECONOMICAL\"", 1),
			want: "nothing estimates now",
		},
		"a key set twice": {
			body: strings.Replace(good, "[journal]",
				"[journal]\npath = \"/a.db\"", 1),
			want: "set twice",
		},
		"a section that was retired": {
			body: good + "\n[[signer]]\nlabel = \"cold1\"\n",
			want: "there is no such round left",
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

[journal]
path = "/state/runs.db"
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
