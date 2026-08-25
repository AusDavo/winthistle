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
	// Aliased because this file already has a local helper called setup().
	setuppkg "github.com/AusDavo/winthistle/internal/setup"
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
	if !strings.Contains(out.String(), "Step 8 was not made") {
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
	if run.TxID != res.Armed.TxID {
		t.Errorf("the journal pinned %q and the batch armed at %s. The txid goes in "+
			"before the first psbt_verify, which is the call that starts a peer "+
			"storing a commitment signature against it", run.TxID, res.Armed.TxID)
	}
	// And no raw transaction, which is the assertion rather than an omission.
	// The bytes go to disk immediately before the publish RPC and nowhere else,
	// because that write is what "we may owe a rebroadcast" means. A probe that
	// withheld the publish owes nothing: nothing was broadcast, and the abort
	// this run terminated through needs the outpoints rather than the bytes.
	if run.RawTx != "" {
		t.Error("the journal holds a raw transaction for a run that never reached " +
			"the publish call, so raw_tx no longer means what MarkPublishing reads it " +
			"to mean")
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

// TestARejectedWalletStopsTheRunBeforeAnythingIsAsked.
//
// The gate that was deliberately absent until it was not. `winthistle doctor`
// refuses a wallet whose exact descriptors a human compared and rejected, and
// nothing stopped `run` opening a batch against the same wallet — the two
// commands shared a journal and not a gate.
//
// What matters here is not only that it refuses but *where*: first, before LND
// is asked anything. A refusal at that point has cost nothing — no stream, no
// reservation, no coin lock, and no peer has been told a channel is coming. So
// the assertions are that the peer section never printed and that the failure is
// the sentinel rather than something that happens to have gone wrong.
func TestARejectedWalletStopsTheRunBeforeAnythingIsAsked(t *testing.T) {
	env := regtestenv.Start(t)
	peers := env.Peers(t)
	if len(peers) < 1 {
		t.Skip("this test needs a peer to prove one was not asked")
	}
	d, o, out, env := setup(t, peers[:1], []int64{fixtureChannelSat})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// The harness cold wallet, as it is — and a human answer saying it is not
	// theirs. The journal is this test's own temp file, so nothing leaks into
	// another run.
	descs, err := env.Cold.ListDescriptors(ctx)
	if err != nil {
		t.Fatalf("listing %s's descriptors: %v", regtestenv.ColdWallet, err)
	}
	var receive, change string
	for _, dd := range descs {
		switch {
		case dd.Active && !dd.Internal:
			receive = dd.Desc
		case dd.Active && dd.Internal:
			change = dd.Desc
		}
	}
	if receive == "" || change == "" {
		t.Fatalf("%s has no active pair — run: make harness", regtestenv.ColdWallet)
	}
	addrs, err := env.Node.DeriveAddresses(ctx, receive, 0, 0)
	if err != nil {
		t.Fatalf("deriving the first address: %v", err)
	}
	changeAddrs, err := env.Node.DeriveAddresses(ctx, change, 0, 0)
	if err != nil {
		t.Fatalf("deriving the first change address: %v", err)
	}

	// First: the same wallet with no answer recorded gets past the gate. Without
	// this the test below would pass on a gate that refuses everything.
	before, err := setuppkg.Check(ctx, env.Cold, d.Journal, o.Config.Bitcoind.Wallet)
	if err != nil {
		t.Fatalf("an unanswered wallet was refused: %v", err)
	}
	if before.Rejected() || before.Confirmed() {
		t.Fatalf("the harness wallet already carries an answer in this journal: %+v",
			before.Record)
	}

	if _, err := d.Journal.RecordSetup(ctx, journal.Setup{
		Wallet: o.Config.Bitcoind.Wallet, Outcome: journal.SetupRejected,
		Receive: receive, Change: change, SampleSize: 5,
		FirstReceive: addrs[0], FirstChange: changeAddrs[0],
	}); err != nil {
		t.Fatalf("recording the rejection: %v", err)
	}

	res, err := run.Do(ctx, d, o)
	t.Logf("\n%s", out.String())

	if !errors.Is(err, setuppkg.ErrRejectedWallet) {
		t.Fatalf("run returned %v, want ErrRejectedWallet", err)
	}
	if res != nil && res.Armed != nil {
		t.Fatal("a rejected wallet armed a batch")
	}
	if strings.Contains(out.String(), "Phase 0 — the peers") {
		t.Error("the run reached the peer pre-flight before refusing. The point of " +
			"this gate is that it costs nothing: a peer that has been asked about a " +
			"channel holds a pending-channel slot for about eleven minutes.")
	}
	screen := strings.Join(strings.Fields(out.String()), " ")
	for _, want := range []string{
		"did not match",
		"Nothing was opened and nothing was asked of any peer",
		"new wallet name",
		"no RPC that removes a descriptor",
	} {
		if !strings.Contains(screen, want) {
			t.Errorf("the refusal screen does not say %q:\n%s", want, out.String())
		}
	}
}

// cancelDuringBatchRound is a Signers that cancels the run partway through the
// real signing round: the first device signs, and then the context is cancelled
// while the second is being asked.
//
// That is where Ctrl-C is most likely to land and where it costs the most. By
// then arm.Open has returned, n peers hold reservations, and every channel that
// reached chan_pending is holding a commitment signature — so the teardown has
// real work to do rather than nothing.
type cancelDuringBatchRound struct {
	t      *testing.T
	env    *regtestenv.Env
	cancel func()

	// cancelled is closed once the cancel has been made, so the test can tell
	// "the run stopped because we cancelled it" from "the run stopped for some
	// other reason and this fixture never fired".
	cancelled chan struct{}
}

func (c *cancelDuringBatchRound) Labels() []string { return regtestenv.ColdSigners() }

func (c *cancelDuringBatchRound) Round(name string) []rehearsal.Device {
	labels := regtestenv.ColdSigners()
	out := make([]rehearsal.Device, 0, len(labels))
	for i, label := range labels {
		label, i := label, i
		out = append(out, rehearsal.Device{
			Label: label,
			Sign: func(ctx context.Context, psbtB64 string) (combine.Part, error) {
				// The rehearsal has to succeed: the gate is what decides whether
				// the batch is armed at all, and a run that never arms leaves
				// nothing for the teardown to take apart.
				if name != "batch" || i == 0 {
					return c.env.SignPartial(c.t, label, psbtB64), nil
				}
				close(c.cancelled)
				c.cancel()
				// Return the context's error rather than a signature, which is
				// what a real transport does when the operator interrupts it.
				<-ctx.Done()
				return combine.Part{}, ctx.Err()
			},
		})
	}
	return out
}

// TestCancellingMidRunStillTakesTheBatchApart is the test that was missing when
// the abort path shipped broken.
//
// recoverRun ran on the run's own context, so on Ctrl-C every call in the
// teardown failed at once: the journal read is database/sql, the shim cancels and
// the abandons are gRPC, and Core's lock release is JSON-RPC. An abort triggered
// by a cancellation would have reported "context canceled" and taken nothing
// apart — which is the exact opposite of what the deferred teardown is for.
//
// Nothing caught it because every test that exercised the abort path did so
// through a *failure*, which leaves a live context. This one cancels instead, and
// it is the only test in the repository that does. Its subject is not the error
// the run returns — that is uninteresting, and it is a cancellation — but that
// the teardown afterwards actually ran on a context of its own.
func TestCancellingMidRunStillTakesTheBatchApart(t *testing.T) {
	env := regtestenv.Start(t)
	peers := env.Peers(t)
	if len(peers) < 2 {
		t.Skipf("this test needs 2 peers, alice has %d", len(peers))
	}
	d, o, out, env := setup(t, peers[:2],
		[]int64{fixtureChannelSat, fixtureChannelSat})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	signers := &cancelDuringBatchRound{
		t: t, env: env, cancel: cancel, cancelled: make(chan struct{}),
	}
	d.Signers = signers

	res, err := run.Do(ctx, d, o)
	t.Logf("\n%s", out.String())

	select {
	case <-signers.cancelled:
	default:
		t.Fatal("the run never reached the batch's second device, so nothing was " +
			"cancelled and this test proved nothing")
	}
	if err == nil {
		t.Fatal("a cancelled run reported success")
	}

	// The point of the test. A teardown on a cancelled context fails at its first
	// call, so a report with something in it is the evidence that it ran on a
	// context of its own.
	if res.Aborted == nil {
		t.Fatalf("the run was cancelled and nothing was taken apart. That is the "+
			"defect this test exists for: the teardown has to run on a context "+
			"that survives the cancellation that triggered it.\nrun returned: %v", err)
	}
	if !res.Aborted.Clean() {
		t.Errorf("the teardown left something behind: %+v", res.Aborted.Failures)
	}
	if n := len(res.Aborted.Cancelled) + len(res.Aborted.Abandoned); n == 0 {
		t.Error("the teardown reported no shims cancelled and no channels " +
			"abandoned, so it had nothing to do — which means arm.Open never " +
			"opened a stream and the cancellation landed somewhere harmless")
	}

	// And it must not have reported the cancellation as the reason it could not
	// clean up, which is what the broken version said.
	if strings.Contains(out.String(), "could not read run") {
		t.Errorf("the teardown could not read its own journal row:\n%s", out.String())
	}

	// The journal agrees, which is what `winthistle recover` would read next.
	run, jerr := d.Journal.Load(ctx, o.RunID)
	if jerr != nil {
		// The context is cancelled by now, so use a fresh one — the same thing
		// recoverRun has to do.
		fresh, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		if run, jerr = d.Journal.Load(fresh, o.RunID); jerr != nil {
			t.Fatalf("reading the run back: %v", jerr)
		}
	}
	if run.State != journal.StateAborted {
		t.Errorf("the run ended in %s, not %s", run.State, journal.StateAborted)
	}
}
