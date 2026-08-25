package run_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AusDavo/winthistle/internal/abort"
	"github.com/AusDavo/winthistle/internal/config"
	"github.com/AusDavo/winthistle/internal/journal"
	"github.com/AusDavo/winthistle/internal/regtestenv"
	"github.com/AusDavo/winthistle/internal/regtestenv/coldwallet"
	"github.com/AusDavo/winthistle/internal/run"
)

// fixtureChannelSat matches the other packages' fixtures, so a peer that
// accepts one accepts the others.
const fixtureChannelSat = 250_000

// sparrow is the harness playing the wallet at steps 4 and 7.
//
// Two Core wallets and no hardware, standing in for the one thing this app no
// longer does: it builds the transaction from the recipients it is handed, and it
// signs it and finalizes it before handing it back. The combining happens *here*,
// outside the app, which is the whole difference from the fixture this replaced —
// combine.Accept has to meet the input it will actually get, which is one packet
// with complete witnesses, not m partials.
//
// coldwallet and internal/bitcoind survive item 5 exactly for this. The
// application drops Core; a regtest test still needs something to build and sign
// a funding transaction, and this is that something rather than a back door.
type sparrow struct {
	t   *testing.T
	env *regtestenv.Env

	// feeRate is the rate the app's own plan will judge the transaction against,
	// read off the same configuration the run reads. A fixture that picked its own
	// would be testing the fee finding rather than the sequence.
	feeRate float64

	// signedWith records the packet handed back at step 7, so a test can prove
	// the bytes that reached the publish call are these.
	signed []byte

	// beforeSign runs just before the wallet answers step 7. It is where a test
	// that wants to interrupt the run puts the interruption, because step 7 is
	// where an operator's Ctrl-C is most likely to land: n peers hold
	// reservations, every channel is at chan_pending, and the teardown has real
	// work to do.
	beforeSign func(ctx context.Context) error
}

func (s *sparrow) Built(_ context.Context, pay []run.Recipient) ([]byte, error) {
	outputs := make([]coldwallet.Output, 0, len(pay))
	for _, r := range pay {
		outputs = append(outputs, coldwallet.Output{
			Address: r.Address, AmountSat: r.AmountSat,
		})
	}
	funded := s.env.BuildPSBTPaying(s.t, s.env.Cold, outputs, s.feeRate)
	return funded.Raw, nil
}

func (s *sparrow) Signed(ctx context.Context, unsigned []byte) ([]byte, error) {
	if s.beforeSign != nil {
		if err := s.beforeSign(ctx); err != nil {
			return nil, err
		}
	}
	s.signed = s.env.SignLikeSparrow(s.t, unsigned)
	return s.signed, nil
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
		LND:     env.Alice,
		Journal: j,
		Signing: &sparrow{t: t, env: env, feeRate: cfg.Fees.TargetSatPerVB},
		Out:     out, Confirm: blunt(t),
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

// TestARejectedWalletStopsTheRunBeforeAnythingIsAsked lived here, and it went
// with the gate it was about.
//
// setup.Check read back a human's verdict on the cold wallet's descriptor pair,
// first, before LND was asked anything, and the test's real subject was that
// placement rather than the refusal — that the peer section had not printed when
// it fired. `run` no longer selects coins, derives addresses or asks Core for
// change, so it never reads those descriptors and a refusal about them would have
// no subject. The gate itself is unchanged and still tested, in internal/setup and
// through `winthistle doctor`, which is where somebody setting a wallet up meets
// it. Item 5 of docs/replan-2026-08.md deletes the package.

// cancelDuringSigning cancels the run at step 7, which is where an operator's
// Ctrl-C is most likely to land and where it costs the most.
//
// It replaced a fixture that cancelled between two devices of the batch's signing
// round. There is no such round any more — one wallet is asked once — so the
// equivalent moment is the wallet being asked and the operator walking away. By
// then arm.Open has returned, n peers hold reservations and every channel has
// reached chan_pending holding a commitment signature, so the teardown has real
// work to do rather than nothing.
func cancelDuringSigning(t *testing.T, env *regtestenv.Env, cancel func(),
	rate float64, cancelled chan struct{}) *sparrow {

	return &sparrow{
		t: t, env: env, feeRate: rate,
		beforeSign: func(ctx context.Context) error {
			close(cancelled)
			cancel()
			// Return the context's error rather than a signature, which is what a
			// real transport does when the operator interrupts it.
			<-ctx.Done()
			return ctx.Err()
		},
	}
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

	cancelled := make(chan struct{})
	d.Signing = cancelDuringSigning(t, env, cancel, o.Config.Fees.TargetSatPerVB,
		cancelled)

	res, err := run.Do(ctx, d, o)
	t.Logf("\n%s", out.String())

	select {
	case <-cancelled:
	default:
		t.Fatal("the run never reached step 7, so nothing was cancelled and this " +
			"test proved nothing")
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

// syncWriter is the transcript, safe to read while the run is writing it.
//
// The operator half of the test below runs on the test's own goroutine and the
// run on another, which is the right way round — the harness helpers call
// t.Fatalf, and t.Fatalf from a goroutine that is not the test's abandons that
// goroutine silently and hangs the run instead of failing it.
type syncWriter struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

// waitFor blocks until the transcript says something, or the test gives up.
func waitFor(t *testing.T, out *syncWriter, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		if strings.Contains(out.String(), want) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the run never said %q:\n%s", want, out.String())
}

// TestTheFilePathDrivesTheWholeSequence is the --psbt path end to end.
//
// Everything above it drives the SigningWallet seam directly. This one goes
// through the transport an operator actually uses: the app prints a table, waits
// for a file, verifies it, arms the batch over it, waits for a signed file, and
// checks what came back. The harness plays Sparrow on the test's own goroutine —
// coldwallet.Build for the unsigned transaction and both halves combined
// *outside* the app for a complete witness, which is the input combine.Accept
// exists for and the one a partial-signature fixture would never produce.
//
// The publish is withheld, so this costs one peer a pending-channel slot and
// nothing else.
func TestTheFilePathDrivesTheWholeSequence(t *testing.T) {
	env := regtestenv.Start(t)
	peers := env.Peers(t)
	if len(peers) < 1 {
		t.Skip("this test needs one peer")
	}
	d, o, _, env := setup(t, peers[:1], []int64{fixtureChannelSat})
	o.StopBeforePublish = true

	out := new(syncWriter)
	d.Out = out

	dir := t.TempDir()
	wallet, err := run.NewFileWallet(filepath.Join(dir, "batch.psbt"), out)
	if err != nil {
		t.Fatalf("the file transport: %v", err)
	}
	wallet.Poll = 100 * time.Millisecond
	d.Signing = wallet

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	type outcome struct {
		res *run.Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := run.Do(ctx, d, o)
		done <- outcome{res, err}
	}()

	// Step 4, as the operator does it: read the table, build the transaction,
	// save it where the app said.
	waitFor(t, out, "Step 4")
	pay := regtestenv.RecipientsIn(t, out.String())
	funded := env.BuildPSBTPaying(t, env.Cold, pay, o.Config.Fees.TargetSatPerVB)
	if err := os.WriteFile(wallet.Unsigned, funded.Raw, 0o600); err != nil {
		t.Fatalf("saving the unsigned transaction: %v", err)
	}

	// Step 7, after the gate has opened. Nothing was signed before this line, and
	// the transcript above it is the evidence.
	waitFor(t, out, "Step 7")
	if !strings.Contains(out.String(), "with nothing signed") {
		t.Errorf("the transcript does not say the gate opened over an unsigned "+
			"transaction:\n%s", out.String())
	}
	signed := env.SignLikeSparrow(t, funded.Raw)
	if err := os.WriteFile(wallet.SignedPath(), signed, 0o600); err != nil {
		t.Fatalf("saving the signed transaction: %v", err)
	}

	got := <-done
	t.Logf("\n%s", out.String())
	if got.err != nil {
		t.Fatalf("the run: %v", got.err)
	}
	if got.res.Armed == nil {
		t.Fatal("the batch never reached the gate")
	}
	if got.res.Armed.TxID != funded.TxID {
		t.Errorf("the batch armed at %s and the transaction the harness built is "+
			"%s. I-3 says these are the same string or the batch is lost",
			got.res.Armed.TxID, funded.TxID)
	}
	if got.res.Published {
		t.Fatal("--stop-before-publish published")
	}
	if env.InMempool(t, funded.TxID) {
		t.Fatalf("%s reached the mempool", funded.TxID)
	}
	if got.res.Aborted == nil || !got.res.Aborted.Clean() {
		t.Errorf("the teardown did not finish cleanly: %+v", got.res.Aborted)
	}

	// The journal's one signer row is the wallet, and it signed.
	jr, err := d.Journal.Load(ctx, o.RunID)
	if err != nil {
		t.Fatalf("reading the run back: %v", err)
	}
	if len(jr.Signers) != 1 {
		t.Fatalf("the journal holds %d signer rows for a batch signed by one "+
			"wallet: %+v", len(jr.Signers), jr.Signers)
	}
	if jr.Signers[0].State != journal.SignerSigned {
		t.Errorf("the wallet is recorded as %q, want %q. \"partial\" is what an "+
			"earlier build wrote and it is not what one file with complete witnesses "+
			"is", jr.Signers[0].State, journal.SignerSigned)
	}
}
