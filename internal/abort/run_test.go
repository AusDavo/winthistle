package abort

import (
	"context"
	"errors"
	"testing"

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

func target(t *testing.T) Target {
	t.Helper()
	id, err := lnd.NewPendingChanID()
	if err != nil {
		t.Fatal(err)
	}
	return Target{
		Channels: []lnd.ChannelPoint{{TxID: "aa" + zeros(62), Index: 0}},
		Shims:    []lnd.PendingChanID{id},
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
	ln := &fakeLN{}
	rep, err := Run(context.Background(), ln, target(t), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !rep.Clean() {
		t.Fatalf("report not clean: %v", rep.Failures)
	}
	if len(rep.Abandoned) != 1 || len(rep.Cancelled) != 1 {
		t.Fatalf("report incomplete: %+v", rep)
	}
}

// The property that matters most: a failing abandon must not strand the step
// behind it. This used to be about Core's coin locks, which were released last
// and which an operator would find as a wallet silently refusing to spend its
// own money. There are no coin locks now, and the ordering rule they motivated
// is unchanged and still worth holding: every step is attempted, every failure
// is collected, and nothing returns early.
func TestRunCancelsShimsEvenWhenAbandonFails(t *testing.T) {
	ln := &fakeLN{abandonErr: errors.New("rpc exploded")}

	rep, err := Run(context.Background(), ln, target(t), nil)
	if err == nil {
		t.Fatal("expected the abandon failure to be reported")
	}
	if len(rep.Failures) != 1 {
		t.Fatalf("want 1 failure, got %v", rep.Failures)
	}
	if ln.cancels != 1 {
		t.Fatal("shim cancel was skipped after the abandon failed")
	}
	if len(rep.Cancelled) != 1 {
		t.Fatal("the cancelled shim was not reported")
	}
}

// An already-cancelled shim is the end state an abort wants, not a failure.
func TestRunTreatsMissingShimAsAlreadyClean(t *testing.T) {
	ln := &fakeLN{cancelErr: errors.New("no funding intent found for pendingChannelID(ab)")}

	rep, err := Run(context.Background(), ln, target(t), nil)
	if err != nil {
		t.Fatalf("a missing shim should not fail the abort: %v", err)
	}
	if len(rep.Cancelled) != 1 || !rep.Cancelled[0].AlreadyGone {
		t.Fatalf("want one already-gone shim, got %+v", rep.Cancelled)
	}
}
