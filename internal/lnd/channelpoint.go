package lnd

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/lightningnetwork/lnd/lnrpc"
)

// ChannelPoint is a channel's funding outpoint.
//
// TxID is hex in RPC/display byte order — the same orientation Core prints and
// the same one LND accepts in funding_txid_str. Keeping it as text rather than a
// hash avoids a class of reversed-txid bug at every boundary this crosses: the
// journal, Core's RPC, and LND's.
type ChannelPoint struct {
	TxID  string
	Index uint32
}

// ParseChannelPoint reads the "txid:index" form LND returns from
// PendingChannels and ListChannels.
func ParseChannelPoint(s string) (ChannelPoint, error) {
	txid, idx, ok := strings.Cut(s, ":")
	if !ok {
		return ChannelPoint{}, fmt.Errorf("channel point %q is not txid:index", s)
	}
	n, err := strconv.ParseUint(idx, 10, 32)
	if err != nil {
		return ChannelPoint{}, fmt.Errorf("channel point %q has a bad output index: %w", s, err)
	}
	if len(txid) != 64 {
		return ChannelPoint{}, fmt.Errorf("channel point %q: txid is %d hex chars, want 64", s, len(txid))
	}
	return ChannelPoint{TxID: txid, Index: uint32(n)}, nil
}

func (cp ChannelPoint) String() string { return fmt.Sprintf("%s:%d", cp.TxID, cp.Index) }

// RPC renders the protobuf form. funding_txid_str is used rather than the bytes
// variant so the value LND parses is the same string we journalled.
func (cp ChannelPoint) RPC() *lnrpc.ChannelPoint {
	return &lnrpc.ChannelPoint{
		FundingTxid: &lnrpc.ChannelPoint_FundingTxidStr{FundingTxidStr: cp.TxID},
		OutputIndex: cp.Index,
	}
}

// ChannelPointFromPending converts a chan_pending update into a ChannelPoint.
//
// The reversal is not cosmetic. LND fills PendingUpdate.Txid from
// fundingPoint.Hash[:] — a chainhash.Hash, which is internal byte order — while
// every txid a human or Core ever sees is that same hash reversed. Hex-encoding
// the bytes as they arrive yields a plausible-looking txid that matches nothing,
// and the journal is the worst possible place to discover that.
func ChannelPointFromPending(txid []byte, index uint32) (ChannelPoint, error) {
	if len(txid) != 32 {
		return ChannelPoint{}, fmt.Errorf("chan_pending txid is %d bytes, want 32", len(txid))
	}
	reversed := make([]byte, 32)
	for i, b := range txid {
		reversed[31-i] = b
	}
	return ChannelPoint{TxID: hex.EncodeToString(reversed), Index: index}, nil
}
