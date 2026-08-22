package abort

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/AusDavo/winthistle/internal/lnd"
	"github.com/lightningnetwork/lnd/lnrpc"
)

// The exact rejection LND returns when the safe flag declines. From
// rpcserver.go's AbandonChannel:
//
//	return nil, fmt.Errorf("channel %v is not externally "+
//	        "funded or not pending", chanPoint)
//
// Matched on text because it is a bare fmt.Errorf with no code behind it. The
// match is narrow so that a reworded LND error becomes a loud failure rather
// than a silent escalation to the blunt flag.
const safeFlagRejection = "is not externally funded or not pending"

var (
	// ErrNotPending means LND declined the safe flag and our own check of
	// PendingChannels agrees the channel is not pending. The blunt flag would
	// still remove it, which is exactly what must not happen: a confirmed
	// channel abandoned this way is a channel whose funds are stranded with no
	// force-close path. We stop instead.
	ErrNotPending = errors.New("channel is not pending — refusing to abandon it")

	// ErrBluntNotConfirmed means the fallback to i_know_what_i_am_doing was
	// available and was not authorised. Not an error condition in LND; a
	// deliberate stop here.
	ErrBluntNotConfirmed = errors.New("abandon requires the blunt flag and it was not confirmed")
)

// BluntRequest is what the operator is being asked to authorise. It carries
// LND's own words rather than a paraphrase, because the decision turns on
// which half of "not externally funded or not pending" actually applied.
type BluntRequest struct {
	Channel   lnd.ChannelPoint
	Rejection string // LND's verbatim rejection of the safe flag
}

// Confirmation authorises one use of i_know_what_i_am_doing, for one channel.
//
// A func rather than a bool so it cannot be set once and forgotten: the caller
// is asked per channel, at the moment of the rejection, and a nil Confirmation
// means "never fall back" rather than "fall back silently".
type Confirmation func(ctx context.Context, req BluntRequest) (bool, error)

// AbandonOutcome records what actually happened, for the journal and for the
// recovery screen. UsedBlunt is the field worth surfacing to a human.
type AbandonOutcome struct {
	Channel   lnd.ChannelPoint
	Status    string // LND's status string
	UsedBlunt bool
}

// AbandonPending removes one pending, shim-funded channel from LND.
//
// It always tries pending_funding_shim_only first. That flag makes LND check
// that the channel is both shim-funded and still pending, so it structurally
// cannot touch a confirmed channel.
//
// The flag has a known weakness, and handling it is most of this function.
// rpcserver.go infers "shim funded" from ThawHeight > 0, with a standing TODO
// about storing the funding type properly — so a plain PSBT open with no thaw
// height, which is exactly what this app produces, is not recognised as shim
// funded and the safe flag rejects it. The rejection is therefore expected in
// normal operation, not a sign of trouble.
//
// Falling back to i_know_what_i_am_doing gives up *every* protection in that
// call, including the one that matters: it would remove a confirmed channel just
// as readily. So before asking for the fallback we re-establish the missing half
// of the check ourselves, by asking PendingChannels whether the channel is
// pending. If it is not, we refuse outright and no confirmation is offered —
// there is nothing a human could usefully authorise there.
func AbandonPending(ctx context.Context, cli lnrpc.LightningClient,
	cp lnd.ChannelPoint, confirm Confirmation) (AbandonOutcome, error) {

	out := AbandonOutcome{Channel: cp}

	resp, err := cli.AbandonChannel(ctx, &lnrpc.AbandonChannelRequest{
		ChannelPoint:           cp.RPC(),
		PendingFundingShimOnly: true,
	})
	if err == nil {
		out.Status = resp.GetStatus()
		return out, nil
	}
	if !strings.Contains(err.Error(), safeFlagRejection) {
		return out, fmt.Errorf("abandoning %s: %w", cp, err)
	}
	rejection := err.Error()

	// LND declined. Establish independently whether the channel is pending,
	// since that is the protection we would be giving up.
	pending, err := isPendingOpen(ctx, cli, cp)
	if err != nil {
		return out, fmt.Errorf("abandoning %s: safe flag was declined (%s) and "+
			"the pending check could not confirm the channel's state: %w",
			cp, rejection, err)
	}
	if !pending {
		return out, fmt.Errorf("abandoning %s: %w (lnd said: %s)", cp, ErrNotPending, rejection)
	}

	if confirm == nil {
		return out, fmt.Errorf("abandoning %s: %w (lnd said: %s)",
			cp, ErrBluntNotConfirmed, rejection)
	}
	ok, err := confirm(ctx, BluntRequest{Channel: cp, Rejection: rejection})
	if err != nil {
		return out, fmt.Errorf("abandoning %s: confirming the blunt flag: %w", cp, err)
	}
	if !ok {
		return out, fmt.Errorf("abandoning %s: %w", cp, ErrBluntNotConfirmed)
	}

	// PendingFundingShimOnly stays set. LND skips its check as soon as
	// IKnowWhatIAmDoing is true, so it changes nothing on the wire — but it
	// records in the call itself that the safe path was the one we wanted.
	resp, err = cli.AbandonChannel(ctx, &lnrpc.AbandonChannelRequest{
		ChannelPoint:           cp.RPC(),
		PendingFundingShimOnly: true,
		IKnowWhatIAmDoing:      true,
	})
	if err != nil {
		return out, fmt.Errorf("abandoning %s with the blunt flag: %w", cp, err)
	}
	out.Status = resp.GetStatus()
	out.UsedBlunt = true
	return out, nil
}

// isPendingOpen reports whether the channel appears among LND's pending opens.
//
// Deliberately narrow: only pending_open_channels counts. A channel that is
// pending *close* is not something an abort should be removing, and neither is
// one that has already vanished.
func isPendingOpen(ctx context.Context, cli lnrpc.LightningClient, cp lnd.ChannelPoint) (bool, error) {
	resp, err := cli.PendingChannels(ctx, &lnrpc.PendingChannelsRequest{})
	if err != nil {
		return false, fmt.Errorf("listing pending channels: %w", err)
	}
	want := cp.String()
	for _, p := range resp.GetPendingOpenChannels() {
		if p.GetChannel().GetChannelPoint() == want {
			return true, nil
		}
	}
	return false, nil
}
