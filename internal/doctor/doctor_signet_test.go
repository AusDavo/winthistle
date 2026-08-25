package doctor_test

import (
	"strings"
	"testing"

	"github.com/AusDavo/winthistle/internal/config"
	"github.com/AusDavo/winthistle/internal/doctor"
	"github.com/AusDavo/winthistle/internal/signetenv"
)

// TestDoctorReportsAPruneHorizon is the other reader of a prune horizon.
//
// coldwallet.RunPreflight compares the horizon against a birthday it has been
// given; doctor has no birthday to compare against — it runs before there is
// necessarily a cold wallet at all — so its job is to state the date and let the
// operator do the comparison. That branch had never run: regtest cannot make a
// node meaningfully pruned, so until the signet harness existed the only thing
// proving this code path was that it compiled.
//
// Only the Bitcoin Core check is asserted. There is no signet LND, so the LND
// and macaroon checks fail, and that is the honest shape of this harness rather
// than a defect — see signetenv.Env.PrunedConfigFile.
func TestDoctorReportsAPruneHorizon(t *testing.T) {
	env := signetenv.Start(t)
	ctx := ctxFor(t)

	cfg, err := config.Load(env.PrunedConfigFile(t, t.TempDir()))
	if err != nil {
		t.Fatalf("the signet config file: %v", err)
	}

	report := doctor.Run(ctx, cfg, doctor.Options{})

	var core doctor.Check
	for _, c := range report.Checks {
		if c.Name == "Bitcoin Core" {
			core = c
		}
	}
	if core.Name == "" {
		t.Fatalf("no Bitcoin Core check ran:\n%s", report.Report())
	}
	body := strings.Join(core.Lines, "\n")
	t.Logf("\n%s", body)

	if core.Status == doctor.Fail {
		t.Fatalf("the Core check failed against a synced pruned node:\n%s", body)
	}
	if core.Status != doctor.Warn {
		t.Errorf("a node pruned past most of the chain is reported as %v. Pruning "+
			"is not a failure — a wallet born after the horizon is served fine — "+
			"but it is the thing that stops an otherwise well-run node doing "+
			"directed mode, so it has to be said out loud.", core.Status)
	}
	if !strings.Contains(body, "pruned to block") {
		t.Errorf("the report does not name the prune height:\n%s", body)
	}
	// The height alone is useless to an operator, who knows their cold wallet's
	// birthday as a date and not as a block number. Dating it is the whole
	// contribution of this branch over `getblockchaininfo`.
	if !strings.Contains(body, "birthday") {
		t.Errorf("the report names a prune height but never connects it to the "+
			"cold wallet's birthday, which is the only reason an operator cares:\n%s",
			body)
	}
	if strings.Contains(body, "initial block download") {
		t.Errorf("the pruned node is reported as still syncing; signetenv.Start "+
			"should have skipped this test:\n%s", body)
	}
}
