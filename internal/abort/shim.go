// Package abort holds the paths that take a batch apart again.
//
// These ship before the happy path on purpose. The mainnet cold probe runs the
// real production flow and *terminates through this package*, so nothing else
// can be exercised safely until it works.
//
// Every function here is written to be safe to call twice. An abort runs when
// something has already gone wrong, often against state the operator can only
// partly see, and a cleanup step that fails because the thing was already clean
// is worse than useless — it stops the steps behind it from running.
package abort

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/AusDavo/winthistle/internal/lnd"
	"github.com/lightningnetwork/lnd/lnrpc"
	"google.golang.org/grpc"
)

// ShimCanceller is the slice of LND a shim cancel needs: one method.
//
// Narrow rather than lnrpc.LightningClient, because cancelling a shim is
// something the peer pre-flight has to do too — internal/peers probes with
// arm.Client and must be able to take its own probe down — and a client that
// can open a stream should not have to be a client that can abandon a channel.
// lnrpc.LightningClient satisfies it, so abort.Run is unchanged.
type ShimCanceller interface {
	FundingStateStep(ctx context.Context, in *lnrpc.FundingTransitionMsg,
		opts ...grpc.CallOption) (*lnrpc.FundingStateStepResp, error)
}

// ErrNoShim reports that LND holds no funding intent for that pending channel
// id — it was never registered, or a previous cancel already took it.
//
// From lnwallet/wallet.go, CancelFundingIntent errors with "no funding intent
// found for pendingChannelID(...)" when the map has no entry. For an abort that
// is the desired end state, not a failure, so it is a distinguishable sentinel
// rather than an opaque error: CancelShim reports it, and Run treats it as
// already-clean.
var ErrNoShim = errors.New("lnd holds no funding intent for that pending channel id")

// CancelShim releases the funding intent for one pending channel id.
//
// This is the cheap half of the abort. Cancelling gives the intent a chance to
// clean up after itself — CancelFundingIntent calls intent.Cancel(), which
// releases LND's own coin locks — and it is what makes re-arming after a lapsed
// ten-minute window free: reopen the streams for fresh addresses and re-issue
// the same transaction. Nothing has been broadcast, so nothing is lost.
//
// It returns ErrNoShim if there was no intent to cancel.
func CancelShim(ctx context.Context, cli ShimCanceller, id lnd.PendingChanID) error {
	ctx, done := call(ctx)
	defer done()
	_, err := cli.FundingStateStep(ctx, &lnrpc.FundingTransitionMsg{
		Trigger: &lnrpc.FundingTransitionMsg_ShimCancel{
			ShimCancel: &lnrpc.FundingShimCancel{
				PendingChanId: id.Bytes(),
			},
		},
	})
	if err == nil {
		return nil
	}
	// Matched on text because LND returns a plain fmt.Errorf here, with no code
	// and no typed error to key off. Narrow enough to be specific to this case,
	// and the id is included in LND's message so a change in wording shows up as
	// a hard failure rather than a silently swallowed one.
	if strings.Contains(err.Error(), "no funding intent found") {
		return fmt.Errorf("%w (%s)", ErrNoShim, id)
	}
	return fmt.Errorf("cancelling shim %s: %w", id, err)
}
