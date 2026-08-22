package journal_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/AusDavo/winthistle/internal/abort"
	"github.com/AusDavo/winthistle/internal/bitcoind"
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

func locks(n int) []bitcoind.Outpoint {
	out := make([]bitcoind.Outpoint, 0, n)
	for i := range n {
		out = append(out, bitcoind.Outpoint{TxID: fixtureTxID, Vout: uint32(i)})
	}
	return out
}

// The whole healthy sequence, in order, with the states the journal is supposed
// to derive for itself checked at each step.
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

	if err := j.RecordLocks(ctx, runID, locks(2)); err != nil {
		t.Fatalf("RecordLocks: %v", err)
	}

	// Verifying all but one must NOT move the run on: the state means "every
	// stream has committed to the outpoint", and two out of three has not.
	for _, id := range chanIDs[:2] {
		if err := j.MarkVerified(ctx, runID, id); err != nil {
			t.Fatalf("MarkVerified: %v", err)
		}
	}
	if got := load(t, j, runID).State; got != journal.StateArming {
		t.Fatalf("run is %s with one channel unverified, want %s", got, journal.StateArming)
	}
	if err := j.MarkVerified(ctx, runID, chanIDs[2]); err != nil {
		t.Fatalf("MarkVerified: %v", err)
	}
	if got := load(t, j, runID).State; got != journal.StateSigning {
		t.Fatalf("run is %s with every channel verified, want %s", got, journal.StateSigning)
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

	if err := j.RecordFinalizedTx(ctx, runID, fixtureTxID, "0200000000ffffffff"); err != nil {
		t.Fatalf("RecordFinalizedTx: %v", err)
	}

	for i, id := range chanIDs {
		cp := lnd.ChannelPoint{TxID: fixtureTxID, Index: uint32(i)}
		if err := j.MarkPending(ctx, runID, id, cp); err != nil {
			t.Fatalf("MarkPending: %v", err)
		}
	}

	r := load(t, j, runID)
	if r.State != journal.StateArmed {
		t.Fatalf("run is %s with every channel pending, want %s", r.State, journal.StateArmed)
	}
	if r.TxID != fixtureTxID || r.RawTx == "" {
		t.Fatalf("the finalized transaction did not survive: txid=%q raw=%q", r.TxID, r.RawTx)
	}
	if len(r.Channels) != 3 || len(r.Locks) != 2 || len(r.Signers) != 2 {
		t.Fatalf("run reads back as %d channels, %d locks, %d signers",
			len(r.Channels), len(r.Locks), len(r.Signers))
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
		t.Fatalf("MarkPublishing on an armed run: %v", err)
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
	if err := j.RecordLocks(ctx, runID, locks(2)); err != nil {
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
	if len(target.Locks) != 2 {
		t.Fatalf("want 2 coin locks to free, got %v", target.Locks)
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
			ln, core := &fakeLN{}, &fakeCore{}
			_, err := j.Recover(ctx, ln, core, runID, alwaysConfirm)
			if !errors.Is(err, journal.ErrMayBePublished) {
				t.Fatalf("Recover: want ErrMayBePublished, got %v", err)
			}
			if ln.calls != 0 || core.calls != 0 {
				t.Fatalf("Recover touched the node: %d lnd calls, %d core calls",
					ln.calls, core.calls)
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
	if err := j.RecordLocks(ctx, runID, locks(2)); err != nil {
		t.Fatal(err)
	}
	armed := lnd.ChannelPoint{TxID: fixtureTxID, Index: 0}
	if err := j.MarkPending(ctx, runID, chanIDs[0], armed); err != nil {
		t.Fatal(err)
	}

	ln, core := &fakeLN{}, &fakeCore{}
	rep, err := j.Recover(ctx, ln, core, runID, alwaysConfirm)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if !rep.Clean() {
		t.Fatalf("Recover left failures: %v", rep.Failures)
	}
	if len(rep.Abandoned) != 1 || len(rep.Cancelled) != 1 {
		t.Fatalf("report is %+v", rep)
	}
	if len(rep.LocksFreed) != 2 {
		t.Fatalf("freed %d of 2 coin locks", len(rep.LocksFreed))
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
	for _, l := range r.Locks {
		if !l.Released {
			t.Errorf("coin lock %s is still journalled as held", l.Outpoint)
		}
	}

	// Second time round: nothing left, and nothing asked of the node.
	ln2, core2 := &fakeLN{}, &fakeCore{}
	rep2, err := j.Recover(ctx, ln2, core2, runID, alwaysConfirm)
	if err != nil {
		t.Fatalf("second Recover should be a no-op, got: %v", err)
	}
	if len(rep2.Abandoned) != 0 || len(rep2.Cancelled) != 0 || len(rep2.LocksFreed) != 0 {
		t.Fatalf("second Recover did work that was already done: %+v", rep2)
	}
	if core2.calls != 0 {
		t.Fatalf("second Recover asked Core to unlock %d times — with an empty list "+
			"that is a wallet-wide unlock", core2.calls)
	}
}

// An abort that fails partway must leave the run in aborting, not aborted, and
// must record the half that worked.
func TestRecoverLeavesAFailedAbortOpen(t *testing.T) {
	ctx := context.Background()
	j := open(t)
	chanIDs := ids(t, 1)
	const runID = "run-stuck"

	if err := j.Begin(ctx, runID, batch(t, chanIDs)); err != nil {
		t.Fatal(err)
	}
	if err := j.RecordLocks(ctx, runID, locks(1)); err != nil {
		t.Fatal(err)
	}

	ln := &fakeLN{}
	core := &fakeCore{err: errors.New("bitcoind is not answering")}
	rep, err := j.Recover(ctx, ln, core, runID, alwaysConfirm)
	if err == nil {
		t.Fatal("Recover reported success despite a failed lock release")
	}
	if len(rep.Cancelled) != 1 {
		t.Fatalf("the shim cancel that did work was not reported: %+v", rep)
	}

	r := load(t, j, runID)
	if r.State != journal.StateAborting {
		t.Fatalf("run is %s, want %s so a retry can find it", r.State, journal.StateAborting)
	}
	if r.Channels[0].State != journal.ChanCancelled {
		t.Errorf("the cancelled shim was not recorded: %s", r.Channels[0].State)
	}
	if r.Locks[0].Released {
		t.Error("the coin lock is journalled as freed, but Core refused")
	}
	if len(mustUnfinished(t, j)) != 1 {
		t.Error("a half-finished abort is not listed for recovery")
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
	if err := j.RecordLocks(ctx, "nope", locks(1)); !errors.Is(err, journal.ErrNoRun) {
		t.Fatalf("RecordLocks: want ErrNoRun, got %v", err)
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
	calls   int
	cancels int
}

func (f *fakeLN) AbandonChannel(context.Context, *lnrpc.AbandonChannelRequest,
	...grpc.CallOption) (*lnrpc.AbandonChannelResponse, error) {
	f.calls++
	return &lnrpc.AbandonChannelResponse{Status: "abandoned"}, nil
}

func (f *fakeLN) FundingStateStep(context.Context, *lnrpc.FundingTransitionMsg,
	...grpc.CallOption) (*lnrpc.FundingStateStepResp, error) {
	f.calls++
	f.cancels++
	if f.cancels > 1 {
		// LND's wording for an intent that is already gone, which is what a
		// second cancel of the same shim gets.
		return nil, errors.New("no funding intent found for pendingChannelID(...)")
	}
	return &lnrpc.FundingStateStepResp{}, nil
}

type fakeCore struct {
	calls int
	err   error
}

func (f *fakeCore) ReleaseLocks(_ context.Context, ops []bitcoind.Outpoint) ([]bitcoind.Outpoint, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return ops, nil
}
