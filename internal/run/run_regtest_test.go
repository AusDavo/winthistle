package run_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/AusDavo/winthistle/internal/abort"
	"github.com/AusDavo/winthistle/internal/combine"
	"github.com/AusDavo/winthistle/internal/config"
	"github.com/AusDavo/winthistle/internal/journal"
	"github.com/AusDavo/winthistle/internal/regtestenv"
	"github.com/AusDavo/winthistle/internal/rehearsal"
	"github.com/AusDavo/winthistle/internal/run"
)

// fixtureChannelSat matches the other packages' fixtures, so a peer that
// accepts one accepts the others.
const fixtureChannelSat = 250_000

// coldSigners is the harness's simulated 2-of-2, behind the interface the run
// uses.
//
// The two halves are Core wallets rather than a command, which is the point:
// the composition takes a Signers, so what the transport is — a subprocess, a
// file handshake, a browser upload — is not the run's business. What matters
// is that each device returns a partial signature and none of them can complete
// the transaction alone, which is the property I-2 rests on.
type coldSigners struct {
	t   *testing.T
	env *regtestenv.Env
}

func (c coldSigners) Labels() []string { return regtestenv.ColdSigners() }

func (c coldSigners) Round(string) []rehearsal.Device {
	var out []rehearsal.Device
	for _, label := range regtestenv.ColdSigners() {
		label := label
		out = append(out, rehearsal.Device{
			Label: label,
			Sign: func(_ context.Context, psbtB64 string) (combine.Part, error) {
				return c.env.SignPartial(c.t, label, psbtB64), nil
			},
		})
	}
	return out
}

// blunt authorises i_know_what_i_am_doing, which is the standard route rather
// than an edge case: LND's safe flag infers "shim funded" from ThawHeight > 0
// and a plain PSBT open sets none.
func blunt(t *testing.T) abort.Confirmation {
	return func(_ context.Context, req abort.BluntRequest) (bool, error) {
		t.Logf("authorising the blunt abandon of %s (lnd said: %s)",
			req.Channel, req.Rejection)
		return true, nil
	}
}

// setup builds everything a run needs against the harness, out of the same
// config and batch files an operator would write.
func setup(t *testing.T, peers []string, amounts []int64) (
	run.Deps, run.Options, *bytes.Buffer, *regtestenv.Env) {

	t.Helper()
	env := regtestenv.Start(t)
	dir := t.TempDir()

	cfg, err := config.Load(env.ConfigFile(t, dir))
	if err != nil {
		t.Fatalf("the harness config file: %v", err)
	}
	batch, err := config.LoadBatch(batchFile(t, dir, peers, amounts))
	if err != nil {
		t.Fatalf("the harness batch file: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)

	j, err := journal.Open(ctx, filepath.Join(dir, "runs.db"))
	if err != nil {
		t.Fatalf("opening the journal: %v", err)
	}
	t.Cleanup(func() { j.Close() })

	out := new(bytes.Buffer)
	d := run.Deps{
		LND: env.Alice, Node: env.Node, Wallet: env.Cold,
		Journal: j, Signers: coldSigners{t: t, env: env},
		Out: out, Confirm: blunt(t),
	}
	o := run.Options{Config: cfg, Batch: batch, RunID: "run-test-" + t.Name()}
	return d, o, out, env
}

// batchFile writes one [[channel]] per peer, at the amount given for it.
func batchFile(t *testing.T, dir string, peers []string, amounts []int64) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("[policy]\nbase_fee_msat = 0\nfee_rate_ppm = 250\n" +
		"time_lock_delta = 144\n")
	for i, p := range peers {
		b.WriteString("\n[[channel]]\npeer       = \"" + p + "\"\n")
		b.WriteString("amount_sat = " + itoa(amounts[i]) + "\n")
	}
	return writeFile(t, filepath.Join(dir, "batch.toml"), b.String())
}

// TestTheColdProbeRunsTheRealPathAndWithholdsStepNine.
//
// This is the mainnet cold probe, on regtest: the whole production sequence,
// with the last call not made. What it has to prove is that the flag is a
// withheld call rather than a second, gentler route — because a second route to
// the same place would be a way around the I-1 gate rather than a rehearsal of
// it. So the assertions are about the state the real run passes through:
//
//   - every channel reached chan_pending, which is the gate opening;
//   - the journal called the run armed, which is the only thing that can;
//   - nothing is in the mempool, checked afterwards;
//   - and the run terminated through the abort path, which is the other half of
//     what the probe is for.
func TestTheColdProbeRunsTheRealPathAndWithholdsStepNine(t *testing.T) {
	env := regtestenv.Start(t)
	peers := env.Peers(t)
	if len(peers) < 2 {
		t.Skipf("this test needs 2 peers, alice has %d", len(peers))
	}
	d, o, out, env := setup(t, peers[:2],
		[]int64{fixtureChannelSat, fixtureChannelSat})
	o.StopBeforePublish = true

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	res, err := run.Do(ctx, d, o)
	t.Logf("\n%s", out.String())
	if err != nil {
		t.Fatalf("the cold probe: %v", err)
	}

	if res.Armed == nil {
		t.Fatal("the run returned no Armed, so the batch never reached the gate")
	}
	if n := len(res.Armed.Channels); n != 2 {
		t.Fatalf("armed with %d channels, expected 2", n)
	}
	seen := map[uint32]bool{}
	for _, cp := range res.Armed.Channels {
		if cp.TxID != res.Armed.TxID {
			t.Errorf("channel %s is not in the batch transaction %s", cp, res.Armed.TxID)
		}
		if seen[cp.Index] {
			t.Errorf("two channels share output %d", cp.Index)
		}
		seen[cp.Index] = true
	}
	if res.Published {
		t.Fatal("--stop-before-publish published")
	}
	if env.InMempool(t, res.Armed.TxID) {
		t.Fatalf("%s reached the mempool. The whole claim of this mode is that "+
			"step 9 is the one call it does not make", res.Armed.TxID)
	}
	// ExportAllChannelBackups is node-wide, not batch-scoped: the snapshot
	// covers every channel this node has, pending ones included. So the
	// assertion is that it covers at least this batch and that the multi-backup
	// is real — a count equal to n would be asserting something LND does not do.
	if n := len(res.Armed.Backup.GetSingleChanBackups().GetChanBackups()); n < 2 {
		t.Errorf("step 8 exported backups for %d channels, expected at least the "+
			"2 in this batch", n)
	}
	if len(res.Armed.Backup.GetMultiChanBackup().GetMultiChanBackup()) == 0 {
		t.Error("step 8 exported an empty multi-channel backup")
	}

	// The probe says so in the report, because the operator has to be able to
	// tell "it worked and I stopped it" from "it worked".
	if !strings.Contains(out.String(), "Step 9 was not made") {
		t.Error("the report does not say the publish was withheld")
	}

	// And it terminated through the abort path.
	if res.Aborted == nil {
		t.Fatal("the probe left the batch armed rather than taking it apart")
	}
	if !res.Aborted.Clean() {
		t.Errorf("the abort left something behind: %+v", res.Aborted.Failures)
	}
	run, err := d.Journal.Load(ctx, o.RunID)
	if err != nil {
		t.Fatalf("reading the run back: %v", err)
	}
	if run.State != journal.StateAborted {
		t.Errorf("the run ended in %s, not %s", run.State, journal.StateAborted)
	}
	if run.TxID != res.Armed.TxID || run.RawTx == "" {
		t.Error("the journal does not hold the finalized transaction, which is " +
			"what a rebroadcast after a crash would need")
	}
}

// TestAFailureInsideTheArmedWindowIsTakenApart.
//
// The second channel asks for less than LND's minimum channel size, so the peer
// refuses at accept_channel and arm.Open returns partway. That is not an error
// anybody can walk away from: the first stream is a reservation a peer is
// holding, and its pending channel id is the only handle that can release it.
//
// So the run journals what opened before deciding Open failed, and then takes
// it apart through journal.Recover — the same call winthistle recover makes.
func TestAFailureInsideTheArmedWindowIsTakenApart(t *testing.T) {
	env := regtestenv.Start(t)
	peers := env.Peers(t)
	if len(peers) < 2 {
		t.Skipf("this test needs 2 peers, alice has %d", len(peers))
	}
	// 1,000 sat is below LND's MinChanFundingSize of 20,000, and the peer's
	// refusal reaches us: failFundingFlow forwards a lnwallet.ReservationError
	// verbatim.
	d, o, out, _ := setup(t, peers[:2], []int64{fixtureChannelSat, 1_000})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	res, err := run.Do(ctx, d, o)
	t.Logf("\n%s", out.String())
	if err == nil {
		t.Fatal("a batch with a channel below the minimum size was opened")
	}
	if res.Armed != nil || res.Published {
		t.Fatal("the run got past the armed window")
	}

	run, jerr := d.Journal.Load(ctx, o.RunID)
	if errors.Is(jerr, journal.ErrNoRun) {
		t.Fatal("nothing was journalled, so the stream that did open has no handle " +
			"on disk that could cancel it")
	}
	if jerr != nil {
		t.Fatalf("reading the run back: %v", jerr)
	}
	if run.State != journal.StateAborted {
		t.Errorf("the run ended in %s, not %s — the shims are still open at the "+
			"peers", run.State, journal.StateAborted)
	}
	if res.Aborted == nil || !res.Aborted.Clean() {
		t.Errorf("the teardown did not finish cleanly: %+v", res.Aborted)
	}
	if len(res.Aborted.Cancelled) == 0 {
		t.Error("no shim was cancelled, so the stream that opened was dropped on " +
			"the floor")
	}
}

func writeFile(t *testing.T, path, body string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
