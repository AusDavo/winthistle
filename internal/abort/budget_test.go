package abort

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/AusDavo/winthistle/internal/lnd"
	"github.com/lightningnetwork/lnd/lnrpc"
	"google.golang.org/grpc"
)

// blowsTheSafeFlag refuses pending_funding_shim_only the way LND does, answers
// PendingChannels with the channel present, and records the deadline it was
// handed on each AbandonChannel call.
type blowsTheSafeFlag struct {
	lnrpc.LightningClient
	cp lnd.ChannelPoint

	calls     int
	deadlines []time.Time
	hadNone   []bool
}

func (f *blowsTheSafeFlag) AbandonChannel(ctx context.Context,
	req *lnrpc.AbandonChannelRequest, _ ...grpc.CallOption) (
	*lnrpc.AbandonChannelResponse, error) {

	f.calls++
	dl, ok := ctx.Deadline()
	f.deadlines = append(f.deadlines, dl)
	f.hadNone = append(f.hadNone, !ok)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !req.GetIKnowWhatIAmDoing() {
		return nil, errors.New("channel " + f.cp.String() +
			" is not externally funded or not pending")
	}
	return &lnrpc.AbandonChannelResponse{Status: "abandoned"}, nil
}

func (f *blowsTheSafeFlag) PendingChannels(ctx context.Context,
	_ *lnrpc.PendingChannelsRequest, _ ...grpc.CallOption) (
	*lnrpc.PendingChannelsResponse, error) {

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &lnrpc.PendingChannelsResponse{
		PendingOpenChannels: []*lnrpc.PendingChannelsResponse_PendingOpenChannel{{
			Channel: &lnrpc.PendingChannelsResponse_PendingChannel{
				ChannelPoint: f.cp.String(),
			},
		}},
	}, nil
}

// TestTheOperatorIsNotOnTheTeardownClock is the regression test for the defect
// that retired run.TeardownBudget.
//
// The blunt confirmation sits between two LND calls. While one deadline covered
// the whole teardown, an operator who took longer than it deciding got the
// second call refused with "context deadline exceeded" *after* answering yes —
// which leaves somebody believing they authorised something that did not happen.
//
// Two assertions, and the first is the one that matters: the context handed to
// the Confirmation carries no deadline at all, so re-adding a blanket deadline
// upstream fails here rather than in front of an operator at 2am. The second is
// that the call after the confirmation gets a *fresh* CallBudget rather than
// whatever was left of a shared one.
func TestTheOperatorIsNotOnTheTeardownClock(t *testing.T) {
	cp := lnd.ChannelPoint{TxID: "bb" + zeros(62), Index: 1}
	cli := &blowsTheSafeFlag{cp: cp}

	// A deliberately slow operator. Real deliberation, not a mock of it.
	const thinking = 150 * time.Millisecond

	var confirmHadDeadline bool
	confirm := func(ctx context.Context, _ BluntRequest) (bool, error) {
		_, confirmHadDeadline = ctx.Deadline()
		time.Sleep(thinking)
		return true, nil
	}

	// A parent that has ALREADY expired by the time the operator answers. This
	// is the old TeardownBudget reproduced exactly: one clock over the whole
	// teardown, spent during the thinking. Under the old code the blunt call
	// below failed with "context deadline exceeded" after the yes.
	parent, cancelParent := context.WithTimeout(context.Background(), thinking/3)
	defer cancelParent()

	before := time.Now()
	out, err := AbandonPending(parent, cli, cp, confirm)
	if err != nil {
		t.Fatalf("a slow operator lost the abandon: %v", err)
	}
	if !out.UsedBlunt {
		t.Fatal("the blunt flag was not the one that worked")
	}

	if confirmHadDeadline {
		t.Error("the confirmation was handed a context with a deadline. Nothing " +
			"may put the operator on a clock: a deadline here refuses the abandon " +
			"after the answer, which is the defect TeardownBudget was retired for")
	}
	if parent.Err() == nil {
		t.Fatal("the parent context did not expire, so this test did not " +
			"reproduce the thing it is about")
	}

	if cli.calls != 2 {
		t.Fatalf("expected the safe attempt and the blunt one, got %d calls", cli.calls)
	}
	// The blunt call's budget must start after the thinking, not before it.
	blunt := cli.deadlines[1]
	if cli.hadNone[1] {
		t.Fatal("the blunt AbandonChannel had no deadline; CallBudget is not applied")
	}
	if spent := blunt.Sub(before); spent <= CallBudget {
		t.Errorf("the blunt call's deadline is %v from the start of the abandon, "+
			"which is inside one CallBudget (%v) — it inherited a clock that had "+
			"already been running through the confirmation", spent, CallBudget)
	}
}
