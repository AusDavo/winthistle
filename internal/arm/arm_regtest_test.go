package arm_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/AusDavo/winthistle/internal/abort"
	"github.com/AusDavo/winthistle/internal/arm"
	"github.com/AusDavo/winthistle/internal/combine"
	"github.com/AusDavo/winthistle/internal/journal"
	"github.com/AusDavo/winthistle/internal/lnd"
	"github.com/AusDavo/winthistle/internal/plan"
	"github.com/AusDavo/winthistle/internal/prose"
	"github.com/AusDavo/winthistle/internal/regtestenv"
	"github.com/AusDavo/winthistle/internal/regtestenv/coldwallet"
	"github.com/AusDavo/winthistle/internal/reserve"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnrpc"
)

// fixtureChannelSat matches the other packages' fixtures, so a peer that accepts
// one accepts the others.
const fixtureChannelSat = 250_000

// fixtureFeeRate is what these fixtures target, in sat/vB.
const fixtureFeeRate = 10.0

func harnessCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

func openJournal(t *testing.T) *journal.Journal {
	t.Helper()
	j, err := journal.Open(harnessCtx(t), filepath.Join(t.TempDir(), "runs.db"))
	if err != nil {
		t.Fatalf("opening the journal: %v", err)
	}
	t.Cleanup(func() { j.Close() })
	return j
}

// window is one armed window driven as far as a test wants it.
type window struct {
	env      *regtestenv.Env
	j        *journal.Journal
	runID    string
	streams  *arm.Streams
	plan     *plan.Plan
	built    coldwallet.Built
	verified *arm.Verified
	armed    *arm.Armed
	final    *combine.Finalized
}

// drive runs steps 2 through 7 of the sequence for n channels, exactly as the
// app does, stopping before the publish.
//
// Everything in here is app code: arm.Open for the streams, coldwallet.Build for
// the transaction, plan for the verification, arm.Verify for LND's with
// skip_finalize, arm.Receipts for the gate, the cold wallet's two halves for the
// signatures, and internal/combine for the merge. The only fixture-shaped parts
// are the peer choice and the journal's location.
//
// The order is the inversion: the gate opens at arm.Receipts, before anything is
// signed. After the replan the cold wallet is standing in for Sparrow — the
// harness playing the operator's part — and the thing that matters about it here
// is only that it returns a transaction with the same txid.
func drive(t *testing.T, n int) *window {
	t.Helper()
	env := regtestenv.Start(t)
	ctx := harnessCtx(t)

	peers := env.Peers(t)
	if len(peers) < n {
		t.Skipf("this test needs %d peers, alice has %d", n, len(peers))
	}

	chans := make([]arm.Channel, 0, n)
	for _, p := range peers[:n] {
		chans = append(chans, arm.Channel{Peer: p, AmountSat: fixtureChannelSat})
	}

	// Phase 0's last act, and the one that decides whether step 5 can succeed at
	// all: does alice's own hot wallet clear the anchor reserve?
	finding, err := reserve.Check(ctx, env.Alice.WalletKit, reserve.Batch{Public: n})
	if err != nil {
		t.Fatalf("the anchor-reserve pre-flight: %v", err)
	}
	t.Logf("reserve pre-flight: %s", finding.Summary())

	w := &window{env: env, j: openJournal(t), runID: "arm-test-" + t.Name()}

	streams, err := arm.Open(ctx, env.Alice.Lightning, "regtest", chans)
	if err != nil {
		t.Fatalf("opening the batch's funding streams: %v", err)
	}
	w.streams = streams
	t.Cleanup(streams.Close)

	// The pending channel ids are the only handles that can cancel these streams,
	// so they go on disk before anything else happens.
	if err := w.j.Begin(ctx, w.runID, streams.NewChannels()); err != nil {
		t.Fatalf("journalling the run: %v", err)
	}
	abortAtCleanup(t, w)

	coins, err := coldwallet.SelectCoins(ctx, env.Cold, 1)
	if err != nil {
		t.Fatalf("selecting the cold wallet's coins: %v", err)
	}
	fenced, err := coldwallet.FenceOff(ctx, env.Cold, coins)
	if err != nil {
		t.Fatalf("fencing off the coins the batch may not spend: %v", err)
	}
	env.ReleaseLocksAtCleanup(t, env.Cold, fenced)

	change, err := coldwallet.ChangeAddress(ctx, env.Cold)
	if err != nil {
		t.Fatalf("asking the cold wallet for a change address: %v", err)
	}

	topUp, err := plan.ReserveTopUp(ctx, env.Alice.Lightning, finding)
	if err != nil {
		t.Fatalf("building the reserve top-up: %v", err)
	}

	w.plan, err = streams.Plan(arm.Blueprint{
		Chain:  "regtest",
		TopUp:  topUp,
		Change: plan.Change{Address: change},
		Inputs: plan.Inputs{MinConfirmations: 1, Allowed: outpointsOf(coins)},
	})
	if err != nil {
		t.Fatalf("assembling the plan: %v", err)
	}
	t.Logf("\n%s", w.plan.Document())

	outputs := fundingOutputs(streams)
	if topUp != nil {
		outputs = append(outputs, coldwallet.Output{
			Address: topUp.Address, AmountSat: topUp.AmountSat,
		})
	}
	w.built, err = coldwallet.Build(ctx, env.Cold, coldwallet.BuildRequest{
		Outputs:          outputs,
		ChangeAddress:    change,
		FeeRateSatPerVB:  fixtureFeeRate,
		MinConfirmations: 1,
	})
	if err != nil {
		t.Fatalf("building the batch transaction: %v", err)
	}
	env.ReleaseLocksAtCleanup(t, env.Cold, w.built.Inputs)

	// Ours first. LND's psbt_verify looks for its own funding output and stops,
	// so an output nobody named would pass all n of its checks.
	v, err := w.plan.Verify(w.built.Raw)
	if err != nil {
		t.Fatalf("verifying the transaction Core built: %v", err)
	}
	if !v.OK() {
		t.Fatalf("the verifier refused the transaction Core built to the plan:\n%s",
			v.Report())
	}

	// Then LND's, for every stream, with skip_finalize — and then the receipts,
	// with the mempool watched throughout. Nothing is signed in either step, and
	// nothing may reach the network in either: that is I-1 and it is now the whole
	// of what happens inside the peers' ten minutes.
	verified, armed := armWatchingTheMempool(t, w)
	w.verified, w.armed = verified, armed

	if run, err := w.j.Load(ctx, w.runID); err != nil {
		t.Fatalf("reading the run back: %v", err)
	} else if run.State != journal.StateArmed {
		t.Fatalf("after n of n chan_pending the run is %s, not %s", run.State,
			journal.StateArmed)
	} else if run.RawTx != "" {
		t.Fatal("the run is armed and the journal already holds a raw transaction. " +
			"The gate opens over an unsigned transaction; nothing has been signed yet")
	}
	if len(armed.Channels) != n {
		t.Fatalf("armed %d channels of %d", len(armed.Channels), n)
	}
	t.Logf("%d of %d channels recoverable by force-close at %s, nothing signed",
		len(armed.Channels), n, verified.TxID)

	// Step 7, with clock A already stopped. The journal refuses this write unless
	// the batch is armed, so it is also the assertion that it is.
	if err := w.j.MarkSigning(ctx, w.runID); err != nil {
		t.Fatalf("MarkSigning: %v", err)
	}
	if run, err := w.j.Load(ctx, w.runID); err != nil {
		t.Fatalf("reading the run back: %v", err)
	} else if run.State != journal.StateSigning {
		t.Fatalf("the run is %s, not %s", run.State, journal.StateSigning)
	}

	// The signers. Two halves of a 2-of-2, neither of which can complete the
	// transaction — see internal/regtestenv.SignPartial.
	for _, label := range regtestenv.ColdSigners() {
		if err := w.j.RecordSigner(ctx, w.runID, label, journal.SignerAwaiting); err != nil {
			t.Fatalf("journalling signer %s: %v", label, err)
		}
	}
	parts := env.SignWithColdWallet(t, w.built.PSBT)
	for _, p := range parts {
		if err := w.j.RecordSigner(ctx, w.runID, p.Label, journal.SignerPartial); err != nil {
			t.Fatalf("journalling signer %s: %v", p.Label, err)
		}
	}

	final, recheck, err := combine.Complete(w.plan, w.built.Raw, parts)
	if err != nil {
		t.Fatalf("combining and finalizing in-app: %v", err)
	}
	if !recheck.OK() {
		t.Fatalf("the plan refused the finalized transaction:\n%s", recheck.Report())
	}
	w.final = final

	if final.TxID != verified.TxID {
		t.Fatalf("I-3: the txid LND pinned was %s and the signing round returned %s",
			verified.TxID, final.TxID)
	}
	if ok, why := env.AcceptsToMempool(t, hex.EncodeToString(final.RawTx)); !ok {
		t.Fatalf("testmempoolaccept refused the batch: %s", why)
	}
	assertMempoolEmpty(t, env, final.TxID, "after combining and finalizing in-app")

	return w
}

// armWatchingTheMempool runs steps 5 and 6 with the mempool polled throughout.
//
// This is where the old fixture watched psbt_finalize, and the assertion has
// moved with the step rather than been dropped: psbt_verify with skip_finalize is
// now the call that completes LND's funding flow, so it is the call at which
// "all but the last" would have broadcast. Nothing may be in the mempool while it
// runs — and nothing could be, because the transaction has no witnesses yet,
// which is a second reason on top of no_publish rather than a replacement for it.
//
// Polling rather than hooking, because there is nothing to hook: both calls are
// deliberately straight lines.
func armWatchingTheMempool(t *testing.T, w *window) (*arm.Verified, *arm.Armed) {
	t.Helper()
	ctx := harnessCtx(t)

	stop := make(chan struct{})
	watched := make(chan error, 1)
	go func() {
		for {
			select {
			case <-stop:
				watched <- nil
				return
			default:
			}
			// Not InMempool: t.Fatalf from a goroutine that is not the test's own
			// runs runtime.Goexit on the wrong one.
			present, err := w.env.TryInMempool(w.built.TxID)
			switch {
			case err != nil:
				watched <- fmt.Errorf("polling the mempool: %w", err)
				return
			case present:
				watched <- fmt.Errorf("%s reached the mempool while the batch was "+
					"being armed. no_publish is set on every stream precisely so "+
					"that nothing broadcasts here", w.built.TxID)
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()

	verified, verr := arm.Verify(ctx, w.env.Alice.Lightning, w.j, w.runID,
		w.streams, w.built.Raw)
	var armed *arm.Armed
	var rerr error
	if verr == nil {
		armed, rerr = arm.Receipts(ctx, w.env.Alice.Lightning, w.j, w.streams, verified)
	}
	close(stop)
	if watchErr := <-watched; watchErr != nil {
		t.Fatal(watchErr)
	}
	if verr != nil {
		t.Fatalf("psbt_verify with skip_finalize: %v", verr)
	}
	if rerr != nil {
		t.Fatalf("collecting the receipts: %v", rerr)
	}
	return verified, armed
}

// abortAtCleanup takes whatever the run left behind apart, through the journal's
// own recovery path.
//
// Journal.Recover refuses a run in publishing or published — ErrMayBePublished —
// so a test that published is expected to be left alone here, and this asserts
// that rather than tolerating it.
func abortAtCleanup(t *testing.T, w *window) {
	t.Helper()
	t.Cleanup(func() {
		ctx, done := context.WithTimeout(context.Background(), 2*time.Minute)
		defer done()

		run, err := w.j.Load(ctx, w.runID)
		if err != nil {
			t.Errorf("reading run %s back for cleanup: %v", w.runID, err)
			return
		}
		switch run.State {
		case journal.StatePublishing, journal.StatePublished:
			if _, err := w.j.Recover(ctx, w.env.Alice.Lightning,
				w.runID, blunt(t)); !errors.Is(err, journal.ErrMayBePublished) {
				t.Errorf("the journal allowed a published run to be aborted: %v", err)
			}
			return
		}
		rep, err := w.j.Recover(ctx, w.env.Alice.Lightning, w.runID, blunt(t))
		if err != nil {
			t.Errorf("aborting run %s (%s): %v", w.runID, run.State, err)
			return
		}
		t.Logf("cleanup: abandoned %d, cancelled %d",
			len(rep.Abandoned), len(rep.Cancelled))
	})
}

// blunt authorises i_know_what_i_am_doing, which is the standard route rather
// than an edge case: LND's safe flag infers "shim funded" from ThawHeight > 0 and
// a plain PSBT open sets none. AbandonPending re-establishes the pending check
// itself before asking.
func blunt(t *testing.T) abort.Confirmation {
	return func(ctx context.Context, req abort.BluntRequest) (bool, error) {
		t.Logf("authorising the blunt abandon of %s (lnd said: %s)",
			req.Channel, req.Rejection)
		return true, nil
	}
}

func outpointsOf(c coldwallet.Coins) []plan.Outpoint {
	out := make([]plan.Outpoint, 0, len(c.Eligible))
	for _, u := range c.Eligible {
		out = append(out, plan.Outpoint{TxID: u.TxID, Vout: u.Vout})
	}
	return out
}

func assertMempoolEmpty(t *testing.T, env *regtestenv.Env, txid, when string) {
	t.Helper()
	if env.InMempool(t, txid) {
		t.Fatalf("%s is in the mempool %s. Nothing may reach the network before "+
			"every channel in the batch is recoverable (I-1)", txid, when)
	}
}

// TestTheBatchPublishesExactlyOnceAndOnlyAfterEveryChannelIsRecoverable is the
// whole of I-1, end to end, against a live node, in the inverted order.
//
// The assertion that matters is not that the publish works. It is *where* the
// mempool is checked: after every psbt_verify, including the last one, which is
// precisely where LND's own "all but the last" idiom would have broadcast — and
// again with the batch fully armed and signed. The transaction reaches the
// network on exactly one line of this test, and that line is one of the two calls
// to WalletKit.PublishTransaction in the repository.
//
// What is new is the order. Every channel reaches chan_pending over an unsigned
// transaction, the signing round happens afterwards with no clock A running, and
// only then does anything go out. drive() has already checked the journal at each
// of those points; this test is what happens after them.
func TestTheBatchPublishesExactlyOnceAndOnlyAfterEveryChannelIsRecoverable(t *testing.T) {
	w := drive(t, 3)
	env, ctx := w.env, harnessCtx(t)
	armed := w.armed

	if len(armed.Channels) != 3 {
		t.Fatalf("armed with %d channels, expected 3", len(armed.Channels))
	}

	// One transaction, three distinct outputs in it.
	seen := map[uint32]bool{}
	for _, cp := range armed.Channels {
		if cp.TxID != w.final.TxID {
			t.Errorf("channel %s is not in the batch transaction %s", cp, w.final.TxID)
		}
		if seen[cp.Index] {
			t.Errorf("two channels share output %d", cp.Index)
		}
		seen[cp.Index] = true
	}

	if n := len(armed.Backup.GetMultiChanBackup().GetMultiChanBackup()); n == 0 {
		t.Error("no multi-channel backup was exported")
	} else {
		t.Logf("exported a %d-byte multi-channel backup covering %d singles", n,
			len(armed.Backup.GetSingleChanBackups().GetChanBackups()))
	}

	// The last check before the line that cannot be undone.
	assertMempoolEmpty(t, env, w.final.TxID, "with the batch fully armed, signed "+
		"and the backups exported")

	if err := arm.Publish(ctx, env.Alice.WalletKit, w.j, armed, w.final.RawTx); err != nil {
		t.Fatalf("publishing: %v", err)
	}
	if !env.InMempool(t, w.final.TxID) {
		t.Fatalf("%s is not in the mempool after the publish call returned", w.final.TxID)
	}
	run, err := w.j.Load(ctx, w.runID)
	if err != nil {
		t.Fatalf("reading the run back: %v", err)
	}
	if run.State != journal.StatePublished {
		t.Fatalf("after publishing the run is %s, not %s", run.State, journal.StatePublished)
	}
	if run.RawTx != hex.EncodeToString(w.final.RawTx) {
		t.Error("the journal's raw transaction is not the one that was published, " +
			"so a rebroadcast after a crash would send different bytes")
	}
	t.Logf("published %s: %s in %d channels, %s fee",
		w.final.TxID, prose.Sats(3*fixtureChannelSat), 3, prose.Sats(w.final.FeeSat))

	// And it is a real transaction, not merely an accepted one.
	env.Mine(t, 6)
	deadline := time.Now().Add(2 * time.Minute)
	for {
		open := 0
		for _, cp := range armed.Channels {
			if env.HasOpenChannel(t, cp) {
				open++
			}
		}
		if open == len(armed.Channels) {
			t.Logf("all %d channels confirmed and open", open)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d channels opened within two minutes", open,
				len(armed.Channels))
		}
		time.Sleep(time.Second)
	}
}

// TestPublishRefusesABatchTheJournalDoesNotCallArmed.
//
// The journal is the component that decides whether every chan_pending arrived,
// and this is the check that would still hold if internal/arm were wrong about
// everything else. Reproduced by taking an armed batch, taking one channel's row
// back out of the pending state, and asking to publish.
func TestPublishRefusesABatchTheJournalDoesNotCallArmed(t *testing.T) {
	w := drive(t, 2)
	env, ctx := w.env, harnessCtx(t)

	// A second run, sharing nothing but this journal, that never armed: the same
	// Armed value — receipts and all, because a struct copy carries unexported
	// fields — pointed at a run the journal will not agree about.
	stale := &arm.Armed{}
	*stale = *w.armed
	stale.RunID = w.runID + "-never-armed"
	if err := w.j.Begin(ctx, stale.RunID, w.streams.NewChannels()); err != nil {
		t.Fatalf("journalling the second run: %v", err)
	}

	err := arm.Publish(ctx, env.Alice.WalletKit, w.j, stale, w.final.RawTx)
	if !errors.Is(err, journal.ErrNotArmed) {
		t.Fatalf("a run in %s was published, or refused for another reason: %v",
			journal.StateArming, err)
	}
	t.Logf("refused: %v", err)

	assertMempoolEmpty(t, env, w.final.TxID, "after a refused publish")

	// The real run is untouched — signing, because drive() took it through the
	// gate and out to the wallet — and still abortable, which is what the
	// cleanup then does. A refusal aimed at one run must not move another.
	if run, err := w.j.Load(ctx, w.runID); err != nil {
		t.Fatalf("reading the run back: %v", err)
	} else if run.State != journal.StateSigning {
		t.Fatalf("the armed run is now %s, want %s", run.State, journal.StateSigning)
	} else if _, err := run.AbortTarget(); err != nil {
		t.Fatalf("the armed run is no longer abortable: %v", err)
	}
}

// TestAnArmedValueFromNowhereCarriesNoTransaction.
//
// Publish takes an *arm.Armed and nothing else that proves the gate opened, and
// the receipts live in an unexported map. So the type system already says that a
// caller outside this package cannot assemble something publishable — this is
// that statement as a test, because it is the kind of property a later refactor
// could quietly remove by exporting one field.
//
// The bytes are real and the txid matches, so nothing else is standing in the
// way: what refuses is the count of receipts against the count of channels.
func TestAnArmedValueFromNowhereCarriesNoTransaction(t *testing.T) {
	env := regtestenv.Start(t)
	ctx := harnessCtx(t)
	j := openJournal(t)

	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: 0},
		Sequence:         wire.MaxTxInSequenceNum - 1,
	})
	tx.AddTxOut(wire.NewTxOut(250_000, bytes.Repeat([]byte{0x51}, 22)))
	var raw bytes.Buffer
	if err := tx.Serialize(&raw); err != nil {
		t.Fatal(err)
	}

	forged := &arm.Armed{
		RunID:    "forged",
		TxID:     tx.TxHash().String(),
		Channels: []lnd.ChannelPoint{{TxID: tx.TxHash().String(), Index: 0}},
		Backup:   &lnrpc.ChanBackupSnapshot{},
	}
	if err := arm.Publish(ctx, env.Alice.WalletKit, j, forged, raw.Bytes()); err == nil {
		t.Fatal("an Armed built outside the gate was accepted")
	} else {
		t.Logf("refused: %v", err)
	}
}

// fundingOutputs is the harness reading the recipients off the open streams, the
// way an operator reads them off the terminal at step 4.
func fundingOutputs(streams *arm.Streams) []coldwallet.Output {
	addrs := make([]string, 0, len(streams.All))
	amounts := make([]int64, 0, len(streams.All))
	for _, st := range streams.All {
		addrs = append(addrs, st.FundingAddress)
		amounts = append(amounts, st.FundingAmount)
	}
	return coldwallet.FundingOutputsOf(addrs, amounts)
}
