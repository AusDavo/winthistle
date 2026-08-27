package journal_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/AusDavo/winthistle/internal/abort"
	"github.com/AusDavo/winthistle/internal/journal"
	"github.com/AusDavo/winthistle/internal/lnd"
	"github.com/lightningnetwork/lnd/lnrpc"
	"google.golang.org/grpc"
)

// A regtest txid, so nothing here can be mistaken for mainnet state.
const fixtureTxID = "8f52b022b79c198551d965f6985ef85d879820fa1a3a4899b1349e32fe4063d9"

func open(t *testing.T) *journal.Journal {
	t.Helper()
	j, err := journal.Open(context.Background(), filepath.Join(t.TempDir(), "runs.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { j.Close() })
	return j
}

func ids(t *testing.T, n int) []lnd.PendingChanID {
	t.Helper()
	out := make([]lnd.PendingChanID, 0, n)
	for range n {
		id, err := lnd.NewPendingChanID()
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, id)
	}
	return out
}

func batch(t *testing.T, chanIDs []lnd.PendingChanID) []journal.NewChannel {
	t.Helper()
	out := make([]journal.NewChannel, 0, len(chanIDs))
	for i, id := range chanIDs {
		out = append(out, journal.NewChannel{
			PendingChanID: id,
			PeerPubkey:    fmt.Sprintf("02%062x", i+1),
			AmountSat:     250_000,
		})
	}
	return out
}

// The whole healthy sequence, in order, with the states the journal is supposed
// to derive for itself checked at each step.
//
// The order is the inversion's: arming through every psbt_verify, armed at n of n
// chan_pending, signing while the transaction is out with a wallet, then
// publishing. It used to run arming → signing → armed, because the old sequence
// signed inside the peers' ten minutes and collected the receipts afterwards.
// Nothing about the writes changed except which one moves the run and when.
func TestJournalWalksARunThroughToPublished(t *testing.T) {
	ctx := context.Background()
	j := open(t)
	chanIDs := ids(t, 3)
	const runID = "run-1"

	if err := j.Begin(ctx, runID, batch(t, chanIDs)); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if got := load(t, j, runID).State; got != journal.StateArming {
		t.Fatalf("a fresh run is %s, want %s", got, journal.StateArming)
	}

	// The txid is pinned before the first psbt_verify, because that call does not
	// pause LND's funding flow — it completes it.
	if err := j.RecordPinnedTxID(ctx, runID, fixtureTxID); err != nil {
		t.Fatalf("RecordPinnedTxID: %v", err)
	}

	// Verifying moves no channel's run on any more, at any count. The next thing
	// that happens to a verified channel is its own chan_pending, arriving over
	// the unsigned transaction, and until every one of them is in there is no
	// gate and nothing to say.
	for _, id := range chanIDs {
		if err := j.MarkVerified(ctx, runID, id); err != nil {
			t.Fatalf("MarkVerified: %v", err)
		}
		if got := load(t, j, runID).State; got != journal.StateArming {
			t.Fatalf("run is %s partway through the verifies, want %s",
				got, journal.StateArming)
		}
	}

	// Two of three receipts is exactly the state LND's own "all but the last"
	// idiom would publish in, and the gate is a count.
	for i, id := range chanIDs[:2] {
		cp := lnd.ChannelPoint{TxID: fixtureTxID, Index: uint32(i)}
		if err := j.MarkPending(ctx, runID, id, cp); err != nil {
			t.Fatalf("MarkPending: %v", err)
		}
	}
	if got := load(t, j, runID).State; got != journal.StateArming {
		t.Fatalf("run is %s with one receipt missing, want %s", got, journal.StateArming)
	}
	if err := j.MarkPending(ctx, runID, chanIDs[2],
		lnd.ChannelPoint{TxID: fixtureTxID, Index: 2}); err != nil {
		t.Fatalf("MarkPending: %v", err)
	}
	if got := load(t, j, runID).State; got != journal.StateArmed {
		t.Fatalf("run is %s with every channel pending, want %s", got, journal.StateArmed)
	}

	// Only now does anything go out to be signed, and the journal refuses that
	// write from any state but armed.
	if err := j.MarkSigning(ctx, runID); err != nil {
		t.Fatalf("MarkSigning on an armed run: %v", err)
	}
	if got := load(t, j, runID).State; got != journal.StateSigning {
		t.Fatalf("run is %s after the transaction went out to be signed, want %s",
			got, journal.StateSigning)
	}

	for _, s := range []struct {
		label string
		state journal.SignerState
	}{
		{"coldcard", journal.SignerAwaiting},
		{"coldcard", journal.SignerPartial},
		{"seedsigner", journal.SignerPartial},
	} {
		if err := j.RecordSigner(ctx, runID, s.label, s.state); err != nil {
			t.Fatalf("RecordSigner %s: %v", s.label, err)
		}
	}

	// The bytes exist for the first time here, and they have to hash to the pin.
	if err := j.RecordFinalizedTx(ctx, runID, fixtureTxID, "0200000000ffffffff"); err != nil {
		t.Fatalf("RecordFinalizedTx: %v", err)
	}

	r := load(t, j, runID)
	if r.TxID != fixtureTxID || r.RawTx == "" {
		t.Fatalf("the signed transaction did not survive: txid=%q raw=%q", r.TxID, r.RawTx)
	}
	if len(r.Channels) != 3 || len(r.Signers) != 2 {
		t.Fatalf("run reads back as %d channels, %d signers",
			len(r.Channels), len(r.Signers))
	}
	// Two rows for one signer label, not three: the second write updates.
	for _, s := range r.Signers {
		if s.State != journal.SignerPartial {
			t.Errorf("signer %s is %s, want %s", s.Label, s.State, journal.SignerPartial)
		}
	}
	for _, c := range r.Channels {
		if c.State != journal.ChanPending {
			t.Errorf("channel %s is %s, want %s", c.PendingChanID, c.State, journal.ChanPending)
		}
		if c.Outpoint.TxID != fixtureTxID {
			t.Errorf("channel %s has outpoint %s, want it on %s",
				c.PendingChanID, c.Outpoint, fixtureTxID)
		}
	}

	if err := j.MarkPublishing(ctx, runID); err != nil {
		t.Fatalf("MarkPublishing on a signed run: %v", err)
	}
	if err := j.MarkPublished(ctx, runID); err != nil {
		t.Fatalf("MarkPublished: %v", err)
	}
	if got := load(t, j, runID).State; got != journal.StatePublished {
		t.Fatalf("run is %s, want %s", got, journal.StatePublished)
	}

	// Nothing is left for a recovery to pick up.
	unfinished, err := j.Unfinished(ctx)
	if err != nil {
		t.Fatalf("Unfinished: %v", err)
	}
	if len(unfinished) != 0 {
		t.Fatalf("a published run is still listed as unfinished: %+v", unfinished)
	}
}

// I-1, enforced by the journal rather than by the loop that calls it. Two of
// three chan_pending is exactly the state LND's own "all but the last" idiom
// would publish in.
func TestMarkPublishingRefusesAPartiallyArmedBatch(t *testing.T) {
	ctx := context.Background()
	j := open(t)
	chanIDs := ids(t, 3)
	const runID = "run-partial"

	if err := j.Begin(ctx, runID, batch(t, chanIDs)); err != nil {
		t.Fatal(err)
	}
	if err := j.RecordFinalizedTx(ctx, runID, fixtureTxID, "0200000000ffffffff"); err != nil {
		t.Fatal(err)
	}
	for i, id := range chanIDs[:2] {
		cp := lnd.ChannelPoint{TxID: fixtureTxID, Index: uint32(i)}
		if err := j.MarkPending(ctx, runID, id, cp); err != nil {
			t.Fatal(err)
		}
	}

	err := j.MarkPublishing(ctx, runID)
	if !errors.Is(err, journal.ErrNotArmed) {
		t.Fatalf("want ErrNotArmed with 2 of 3 channels pending, got %v", err)
	}
	if got := load(t, j, runID).State; got == journal.StatePublishing {
		t.Fatal("the run was moved to publishing despite the refusal")
	}
}

// Armed but with no transaction on disk is not publishable either: I-1 leaves
// rebroadcast to us, and we cannot rebroadcast what we did not write down.
func TestMarkPublishingRefusesWithNothingToRebroadcast(t *testing.T) {
	ctx := context.Background()
	j := open(t)
	chanIDs := ids(t, 1)
	const runID = "run-no-tx"

	if err := j.Begin(ctx, runID, batch(t, chanIDs)); err != nil {
		t.Fatal(err)
	}
	cp := lnd.ChannelPoint{TxID: fixtureTxID, Index: 0}
	if err := j.MarkPending(ctx, runID, chanIDs[0], cp); err != nil {
		t.Fatal(err)
	}
	if got := load(t, j, runID).State; got != journal.StateArmed {
		t.Fatalf("run is %s, want %s", got, journal.StateArmed)
	}

	if err := j.MarkPublishing(ctx, runID); !errors.Is(err, journal.ErrNotArmed) {
		t.Fatalf("want ErrNotArmed with no finalized tx journalled, got %v", err)
	}
}

// The shape a mid-window failure actually leaves: some channels armed, some
// streams never finalized, and coin locks behind both. That is what abort.Target
// models, and the journal is what decides which side each channel falls on.
func TestAbortTargetSplitsArmedChannelsFromUnfinishedShims(t *testing.T) {
	ctx := context.Background()
	j := open(t)
	chanIDs := ids(t, 3)
	const runID = "run-partial"

	if err := j.Begin(ctx, runID, batch(t, chanIDs)); err != nil {
		t.Fatal(err)
	}
	if err := j.MarkVerified(ctx, runID, chanIDs[1]); err != nil {
		t.Fatal(err)
	}
	armed := lnd.ChannelPoint{TxID: fixtureTxID, Index: 0}
	if err := j.MarkPending(ctx, runID, chanIDs[0], armed); err != nil {
		t.Fatal(err)
	}

	target, err := load(t, j, runID).AbortTarget()
	if err != nil {
		t.Fatalf("AbortTarget: %v", err)
	}
	if len(target.Channels) != 1 || target.Channels[0] != armed {
		t.Fatalf("want the one armed channel to abandon, got %v", target.Channels)
	}
	if len(target.Shims) != 2 {
		t.Fatalf("want 2 shims to cancel, got %v", target.Shims)
	}
}

// The refusal that matters. A run that reached the publish call may have its
// funding transaction in a mempool or a block, and abandoning a pending channel
// whose funding transaction then confirms strands the funds with no force-close
// path. No abort is offered — the operator has to look at the chain.
func TestAbortTargetRefusesARunThatMayBePublic(t *testing.T) {
	ctx := context.Background()
	chanIDs := ids(t, 1)

	for _, tc := range []struct {
		name  string
		state journal.State
	}{
		{"mid-publish", journal.StatePublishing},
		{"published", journal.StatePublished},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := open(t)
			const runID = "run-public"
			if err := j.Begin(ctx, runID, batch(t, chanIDs)); err != nil {
				t.Fatal(err)
			}
			if err := j.RecordFinalizedTx(ctx, runID, fixtureTxID, "0200000000ffffffff"); err != nil {
				t.Fatal(err)
			}
			cp := lnd.ChannelPoint{TxID: fixtureTxID, Index: 0}
			if err := j.MarkPending(ctx, runID, chanIDs[0], cp); err != nil {
				t.Fatal(err)
			}
			if err := j.MarkPublishing(ctx, runID); err != nil {
				t.Fatal(err)
			}
			if tc.state == journal.StatePublished {
				if err := j.MarkPublished(ctx, runID); err != nil {
					t.Fatal(err)
				}
			}

			r := load(t, j, runID)
			if r.State != tc.state {
				t.Fatalf("run is %s, want %s", r.State, tc.state)
			}
			if _, err := r.AbortTarget(); !errors.Is(err, journal.ErrMayBePublished) {
				t.Fatalf("want ErrMayBePublished, got %v", err)
			}

			// And Recover must not reach the RPCs at all.
			ln := &fakeLN{}
			_, err := j.Recover(ctx, ln, runID, alwaysConfirm)
			if !errors.Is(err, journal.ErrMayBePublished) {
				t.Fatalf("Recover: want ErrMayBePublished, got %v", err)
			}
			if ln.calls != 0 {
				t.Fatalf("Recover touched the node: %d lnd calls", ln.calls)
			}
		})
	}
}

// A crash mid-publish must leave artifacts, not mystery. Reopen the file and the
// run is still there, still in publishing, still holding the transaction that
// n channels depend on.
func TestACrashMidPublishLeavesTheTransactionOnDisk(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "runs.db")
	chanIDs := ids(t, 1)
	const runID = "run-crash"
	const rawTx = "0200000000ffffffff"

	j, err := journal.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Begin(ctx, runID, batch(t, chanIDs)); err != nil {
		t.Fatal(err)
	}
	if err := j.RecordFinalizedTx(ctx, runID, fixtureTxID, rawTx); err != nil {
		t.Fatal(err)
	}
	cp := lnd.ChannelPoint{TxID: fixtureTxID, Index: 0}
	if err := j.MarkPending(ctx, runID, chanIDs[0], cp); err != nil {
		t.Fatal(err)
	}
	if err := j.MarkPublishing(ctx, runID); err != nil {
		t.Fatal(err)
	}
	// Everything after this point is what the crash ate.
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := journal.Open(ctx, path)
	if err != nil {
		t.Fatalf("reopening the journal: %v", err)
	}
	defer reopened.Close()

	unfinished, err := reopened.Unfinished(ctx)
	if err != nil {
		t.Fatalf("Unfinished: %v", err)
	}
	if len(unfinished) != 1 {
		t.Fatalf("want the crashed run listed for recovery, got %d runs", len(unfinished))
	}
	r := unfinished[0]
	if r.State != journal.StatePublishing {
		t.Fatalf("run is %s, want %s", r.State, journal.StatePublishing)
	}
	if r.RawTx != rawTx {
		t.Fatalf("raw tx read back as %q, want %q", r.RawTx, rawTx)
	}
	if len(r.Channels) != 1 || r.Channels[0].Outpoint != cp {
		t.Fatalf("the channel's funding outpoint did not survive: %+v", r.Channels)
	}
}

// Recover takes a crashed run apart and writes down what it did, so a second
// Recover has nothing left to do — which is what a resumed recovery looks like.
func TestRecoverExecutesTheAbortAndSettlesTheRun(t *testing.T) {
	ctx := context.Background()
	j := open(t)
	chanIDs := ids(t, 2)
	const runID = "run-recover"

	if err := j.Begin(ctx, runID, batch(t, chanIDs)); err != nil {
		t.Fatal(err)
	}
	armed := lnd.ChannelPoint{TxID: fixtureTxID, Index: 0}
	if err := j.MarkPending(ctx, runID, chanIDs[0], armed); err != nil {
		t.Fatal(err)
	}

	ln := &fakeLN{}
	rep, err := j.Recover(ctx, ln, runID, alwaysConfirm)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if !rep.Clean() {
		t.Fatalf("Recover left failures: %v", rep.Failures)
	}
	if len(rep.Abandoned) != 1 || len(rep.Cancelled) != 1 {
		t.Fatalf("report is %+v", rep)
	}

	r := load(t, j, runID)
	if r.State != journal.StateAborted {
		t.Fatalf("run is %s, want %s", r.State, journal.StateAborted)
	}
	want := map[journal.ChannelState]int{journal.ChanAbandoned: 1, journal.ChanCancelled: 1}
	got := map[journal.ChannelState]int{}
	for _, c := range r.Channels {
		got[c.State]++
	}
	for st, n := range want {
		if got[st] != n {
			t.Errorf("%d channels are %s, want %d", got[st], st, n)
		}
	}

	// Second time round: nothing left, and nothing asked of the node.
	ln2 := &fakeLN{}
	rep2, err := j.Recover(ctx, ln2, runID, alwaysConfirm)
	if err != nil {
		t.Fatalf("second Recover should be a no-op, got: %v", err)
	}
	if len(rep2.Abandoned) != 0 || len(rep2.Cancelled) != 0 {
		t.Fatalf("second Recover did work that was already done: %+v", rep2)
	}
	if ln2.calls != 0 {
		t.Fatalf("second Recover made %d calls for work already recorded", ln2.calls)
	}
}

// An abort that fails partway must leave the run in aborting, not aborted, and
// must record the half that worked.
//
// The failing step used to be Core's lock release, which ran last. There are no
// coin locks, so the specimen is the abandon, which runs first — and that is the
// harder half of the same property: the shim cancel behind it must still happen
// and must still be recorded, rather than being skipped because the step in
// front of it failed.
func TestRecoverLeavesAFailedAbortOpen(t *testing.T) {
	ctx := context.Background()
	j := open(t)
	chanIDs := ids(t, 2)
	const runID = "run-stuck"

	if err := j.Begin(ctx, runID, batch(t, chanIDs)); err != nil {
		t.Fatal(err)
	}
	// One channel reached chan_pending, so the abort has an abandon to attempt;
	// the other never did, so it has a shim to cancel behind it.
	armed := lnd.ChannelPoint{TxID: fixtureTxID, Index: 0}
	if err := j.MarkPending(ctx, runID, chanIDs[0], armed); err != nil {
		t.Fatal(err)
	}

	ln := &fakeLN{abandonErr: errors.New("lnd is not answering")}
	rep, err := j.Recover(ctx, ln, runID, alwaysConfirm)
	if err == nil {
		t.Fatal("Recover reported success despite a failed abandon")
	}
	if len(rep.Cancelled) != 1 {
		t.Fatalf("the shim cancel that did work was not reported: %+v", rep)
	}

	r := load(t, j, runID)
	if r.State != journal.StateAborting {
		t.Fatalf("run is %s, want %s so a retry can find it", r.State, journal.StateAborting)
	}
	cancelled := 0
	for _, c := range r.Channels {
		if c.State == journal.ChanCancelled {
			cancelled++
		}
	}
	if cancelled != 1 {
		t.Errorf("%d channels are cancelled, want the one shim: %+v", cancelled, r.Channels)
	}
	if len(mustUnfinished(t, j)) != 1 {
		t.Error("a half-finished abort is not listed for recovery")
	}
}

// A shim that was already gone when the abort asked is not a shim this run
// cancelled, and over a verified row it is not even evidence that the channel is
// gone. #32 item 1.
//
// The row is left exactly as it stood. ChanCancelled says "its shim was
// cancelled before it ever reached pending", which is a cause with an actor in
// it and an ordering claim on top, and abort.CancelShim established neither —
// abort.ErrNoShim is matched off LND's own text and says only that LND holds no
// funding intent under that id. psbt_verify carries skip_finalize, so a verified
// channel is one whose funding flow LND completed; the intent being gone is
// exactly what that looks like from here.
//
// So the run also stays out of StateAborted: "the abort completed with nothing
// left behind" is not established by nothing having failed.
func TestAnAlreadyGoneShimOverAVerifiedRowIsLeftAlone(t *testing.T) {
	ctx := context.Background()
	j := open(t)
	chanIDs := ids(t, 1)
	const runID = "run-gone-verified"

	if err := j.Begin(ctx, runID, batch(t, chanIDs)); err != nil {
		t.Fatal(err)
	}
	if err := j.MarkVerified(ctx, runID, chanIDs[0]); err != nil {
		t.Fatal(err)
	}

	rep, err := j.Recover(ctx, &fakeLN{shimGone: true}, runID, alwaysConfirm)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(rep.Cancelled) != 1 || !rep.Cancelled[0].AlreadyGone {
		t.Fatalf("want one already-gone shim, got %+v", rep)
	}
	if !rep.Clean() {
		t.Fatalf("an already-gone shim is not a failure: %v", rep.Failures)
	}

	r := load(t, j, runID)
	if got := r.Channels[0].State; got != journal.ChanVerified {
		t.Errorf("the channel is journalled as %s; the abort established only that "+
			"LND holds no intent under that id, so the row must still be %s",
			got, journal.ChanVerified)
	}
	if r.State != journal.StateAborting {
		t.Errorf("run is %s, want %s: a channel this journal cannot abandon is "+
			"still standing, so the abort did not complete with nothing left behind",
			r.State, journal.StateAborting)
	}
	if len(mustUnfinished(t, j)) != 1 {
		t.Error("the run is not listed for recovery, so nothing will ever bring " +
			"the operator back to a channel the peer is still holding")
	}
	target, err := r.AbortTarget()
	if err != nil {
		t.Fatalf("AbortTarget: %v", err)
	}
	if len(target.Shims) != 1 || target.Shims[0] != chanIDs[0] {
		t.Errorf("a second recovery does not pick the channel up: %+v", target)
	}
}

// The other side of the same read, and the one place the absence is terminal.
//
// A channel this run never verified is one LND never created — psbt_verify is
// what starts the funding flow — so a missing intent leaves nothing to abandon.
// That pair is ChanShimGone, and it is what lets the run settle rather than
// nagging forever over a shim nothing can do anything about. It is still not
// ChanCancelled: nothing here cancelled anything.
func TestAnAlreadyGoneShimOverAnUnverifiedRowSettlesTheRun(t *testing.T) {
	ctx := context.Background()
	j := open(t)
	chanIDs := ids(t, 1)
	const runID = "run-gone-unverified"

	if err := j.Begin(ctx, runID, batch(t, chanIDs)); err != nil {
		t.Fatal(err)
	}

	rep, err := j.Recover(ctx, &fakeLN{shimGone: true}, runID, alwaysConfirm)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(rep.Cancelled) != 1 || !rep.Cancelled[0].AlreadyGone {
		t.Fatalf("want one already-gone shim, got %+v", rep)
	}

	r := load(t, j, runID)
	if got := r.Channels[0].State; got != journal.ChanShimGone {
		t.Errorf("the channel is journalled as %s, want %s", got, journal.ChanShimGone)
	}
	if r.State != journal.StateAborted {
		t.Errorf("run is %s, want %s: nothing was created under this shim and "+
			"nothing is left of it", r.State, journal.StateAborted)
	}
	if len(mustUnfinished(t, j)) != 0 {
		t.Error("a run with nothing left of it is still listed for recovery")
	}
}

func TestBeginRefusesAnEmptyBatch(t *testing.T) {
	ctx := context.Background()
	j := open(t)
	if err := j.Begin(ctx, "run-empty", nil); err == nil {
		t.Fatal("a batch with no channels was accepted")
	}
}

func TestWritesToAnUnknownRunAreRefused(t *testing.T) {
	ctx := context.Background()
	j := open(t)
	id := ids(t, 1)[0]

	if _, err := j.Load(ctx, "nope"); !errors.Is(err, journal.ErrNoRun) {
		t.Fatalf("Load: want ErrNoRun, got %v", err)
	}
	if err := j.RecordSigner(ctx, "nope", "coldcard", journal.SignerPartial); !errors.Is(err, journal.ErrNoRun) {
		t.Fatalf("RecordSigner: want ErrNoRun, got %v", err)
	}
	if err := j.RecordFinalizedTx(ctx, "nope", fixtureTxID, "00"); !errors.Is(err, journal.ErrNoRun) {
		t.Fatalf("RecordFinalizedTx: want ErrNoRun, got %v", err)
	}
	if err := j.MarkPublishing(ctx, "nope"); !errors.Is(err, journal.ErrNoRun) {
		t.Fatalf("MarkPublishing: want ErrNoRun, got %v", err)
	}

	// And a channel this run does not own is refused too, rather than silently
	// updating nothing.
	if err := j.Begin(ctx, "run-1", batch(t, ids(t, 1))); err != nil {
		t.Fatal(err)
	}
	cp := lnd.ChannelPoint{TxID: fixtureTxID, Index: 0}
	if err := j.MarkPending(ctx, "run-1", id, cp); !errors.Is(err, journal.ErrNoRun) {
		t.Fatalf("MarkPending on a foreign channel: want ErrNoRun, got %v", err)
	}
}

// ---- test doubles ----

func load(t *testing.T, j *journal.Journal, runID string) *journal.Run {
	t.Helper()
	r, err := j.Load(context.Background(), runID)
	if err != nil {
		t.Fatalf("Load %s: %v", runID, err)
	}
	return r
}

func mustUnfinished(t *testing.T, j *journal.Journal) []*journal.Run {
	t.Helper()
	out, err := j.Unfinished(context.Background())
	if err != nil {
		t.Fatalf("Unfinished: %v", err)
	}
	return out
}

func alwaysConfirm(context.Context, abort.BluntRequest) (bool, error) { return true, nil }

// fakeLN accepts the abort's three calls and counts them. Embedding the
// interface means any other call panics rather than returning a plausible zero.
type fakeLN struct {
	lnrpc.LightningClient
	calls      int
	cancels    int
	abandonErr error

	// shimGone makes the *first* cancel report an intent that is already gone,
	// which is what a crashed run's shim cancel gets: CompleteReservation
	// consumed the intent before the process ever asked.
	shimGone bool
}

func (f *fakeLN) AbandonChannel(context.Context, *lnrpc.AbandonChannelRequest,
	...grpc.CallOption) (*lnrpc.AbandonChannelResponse, error) {
	f.calls++
	if f.abandonErr != nil {
		return nil, f.abandonErr
	}
	return &lnrpc.AbandonChannelResponse{Status: "abandoned"}, nil
}

func (f *fakeLN) FundingStateStep(context.Context, *lnrpc.FundingTransitionMsg,
	...grpc.CallOption) (*lnrpc.FundingStateStepResp, error) {
	f.calls++
	f.cancels++
	if f.shimGone || f.cancels > 1 {
		// LND's wording for an intent that is already gone, which is what a
		// second cancel of the same shim gets.
		return nil, errors.New("no funding intent found for pendingChannelID(...)")
	}
	return &lnrpc.FundingStateStepResp{}, nil
}
