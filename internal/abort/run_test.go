package abort

import (
	"context"
	"errors"
	"testing"

	"github.com/AusDavo/winthistle/internal/bitcoind"
	"github.com/AusDavo/winthistle/internal/lnd"
	"github.com/lightningnetwork/lnd/lnrpc"
	"google.golang.org/grpc"
)

// fakeLN overrides only the two methods the abort calls. Embedding the interface
// means any other call panics loudly rather than returning a plausible zero
// value, which is what we want from a stub in this package.
type fakeLN struct {
	lnrpc.LightningClient
	abandonErr error
	cancelErr  error
	abandons   int
	cancels    int
}

func (f *fakeLN) AbandonChannel(_ context.Context, _ *lnrpc.AbandonChannelRequest,
	_ ...grpc.CallOption) (*lnrpc.AbandonChannelResponse, error) {
	f.abandons++
	if f.abandonErr != nil {
		return nil, f.abandonErr
	}
	return &lnrpc.AbandonChannelResponse{Status: "abandoned"}, nil
}

func (f *fakeLN) FundingStateStep(_ context.Context, _ *lnrpc.FundingTransitionMsg,
	_ ...grpc.CallOption) (*lnrpc.FundingStateStepResp, error) {
	f.cancels++
	if f.cancelErr != nil {
		return nil, f.cancelErr
	}
	return &lnrpc.FundingStateStepResp{}, nil
}

type fakeCore struct {
	released []bitcoind.Outpoint
	err      error
	calls    int
}

func (f *fakeCore) ReleaseLocks(_ context.Context, ops []bitcoind.Outpoint) ([]bitcoind.Outpoint, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	f.released = append(f.released, ops...)
	return ops, nil
}

func target(t *testing.T) Target {
	t.Helper()
	id, err := lnd.NewPendingChanID()
	if err != nil {
		t.Fatal(err)
	}
	return Target{
		Channels: []lnd.ChannelPoint{{TxID: "aa" + zeros(62), Index: 0}},
		Shims:    []lnd.PendingChanID{id},
		Locks:    []bitcoind.Outpoint{{TxID: "bb" + zeros(62), Vout: 1}},
	}
}

func zeros(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = '0'
	}
	return string(b)
}

func TestRunCompletesEveryStep(t *testing.T) {
	ln, core := &fakeLN{}, &fakeCore{}
	rep, err := Run(context.Background(), ln, core, target(t), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !rep.Clean() {
		t.Fatalf("report not clean: %v", rep.Failures)
	}
	if len(rep.Abandoned) != 1 || len(rep.Cancelled) != 1 || len(rep.LocksFreed) != 1 {
		t.Fatalf("report incomplete: %+v", rep)
	}
}

// The property that matters most: a failing abandon must not strand the coin
// locks behind it. An operator whose wallet silently refuses to spend its own
// coins, because the release was queued behind a step that failed, is the exact
// outcome the best-effort ordering exists to prevent.
func TestRunReleasesLocksEvenWhenAbandonFails(t *testing.T) {
	ln := &fakeLN{abandonErr: errors.New("rpc exploded")}
	core := &fakeCore{}

	rep, err := Run(context.Background(), ln, core, target(t), nil)
	if err == nil {
		t.Fatal("expected the abandon failure to be reported")
	}
	if len(rep.Failures) != 1 {
		t.Fatalf("want 1 failure, got %v", rep.Failures)
	}
	if len(rep.LocksFreed) != 1 {
		t.Fatal("coin locks were not released after the abandon failed")
	}
	if ln.cancels != 1 {
		t.Fatal("shim cancel was skipped after the abandon failed")
	}
}

// A run that locked no coins must not turn into a wallet-wide unlock.
func TestRunWithNoLocksDoesNotCallCore(t *testing.T) {
	ln, core := &fakeLN{}, &fakeCore{}
	tgt := target(t)
	tgt.Locks = nil

	if _, err := Run(context.Background(), ln, core, tgt, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if core.calls != 0 {
		t.Fatal("Run called ReleaseLocks with an empty list")
	}
}

// An already-cancelled shim is the end state an abort wants, not a failure.
func TestRunTreatsMissingShimAsAlreadyClean(t *testing.T) {
	ln := &fakeLN{cancelErr: errors.New("no funding intent found for pendingChannelID(ab)")}
	core := &fakeCore{}

	rep, err := Run(context.Background(), ln, core, target(t), nil)
	if err != nil {
		t.Fatalf("a missing shim should not fail the abort: %v", err)
	}
	if len(rep.Cancelled) != 1 || !rep.Cancelled[0].AlreadyGone {
		t.Fatalf("want one already-gone shim, got %+v", rep.Cancelled)
	}
}
