package bitcoind

import (
	"context"
	"fmt"
	"math"
	"strings"
)

// FeeEstimate is Core's answer to estimatesmartfee.
//
// Core returns the rate in BTC per kilovirtualbyte and this keeps it that way,
// because converting in the client would put a unit change between the RPC and
// the code that reasons about it. SatPerVB does the conversion, once.
//
// Errors is Core's own list, and it is not an RPC failure: estimatesmartfee
// succeeds and reports "Insufficient data or no feerate found" when the node has
// not seen enough blocks to estimate. That is the ordinary state of a regtest
// node and of a freshly synced one, so it has to be a value rather than an error.
type FeeEstimate struct {
	// FeeRateBTCPerKvB is zero when Core could not estimate.
	FeeRateBTCPerKvB float64 `json:"feerate"`

	// Blocks is the target Core actually answered for, which can be larger than
	// the one asked for.
	Blocks int `json:"blocks"`

	Errors []string `json:"errors"`
}

// Answered reports whether Core produced a rate.
func (e FeeEstimate) Answered() bool { return e.FeeRateBTCPerKvB > 0 }

// SatPerVB converts Core's BTC/kvB into the unit everything else here uses.
//
// One BTC per kvB is 1e8 sat per 1000 vB, so the factor is 1e5. Rounded up: a
// fee rate that rounds down is a transaction that might not relay, and I-4
// leaves no way to replace it.
func (e FeeEstimate) SatPerVB() float64 {
	if !e.Answered() {
		return 0
	}
	return e.FeeRateBTCPerKvB * 1e5
}

// Why joins Core's reasons into one line.
func (e FeeEstimate) Why() string {
	if len(e.Errors) == 0 {
		return ""
	}
	return strings.Join(e.Errors, "; ")
}

// EstimateSmartFee asks Core for a fee rate.
//
// mode is "CONSERVATIVE", "ECONOMICAL" or "UNSET". The caller picks; see
// internal/fees for why this build's default is the conservative one.
//
// This is the only fee source in the tool. CLAUDE.md forbids a third-party fee
// API and the reason is in the rule: a fee API sees the amounts, the timing and
// — through the size of what is being built — the shape of the batch.
func (c *Client) EstimateSmartFee(ctx context.Context, blocks int, mode string) (FeeEstimate, error) {
	if blocks < 1 {
		return FeeEstimate{}, fmt.Errorf("a confirmation target of %d blocks is not a target", blocks)
	}
	params := []any{blocks}
	if mode != "" {
		params = append(params, mode)
	}
	var out FeeEstimate
	if err := c.Call(ctx, "estimatesmartfee", params, &out); err != nil {
		return FeeEstimate{}, fmt.Errorf("asking Core for a fee estimate: %w", err)
	}
	return out, nil
}

// MempoolInfo carries the two floors below which a transaction will not relay.
type MempoolInfo struct {
	// MinFeeBTCPerKvB is the current mempool minimum — it rises above the relay
	// floor when the mempool is full, and a transaction below it is evicted
	// rather than merely slow.
	MinFeeBTCPerKvB float64 `json:"mempoolminfee"`

	// MinRelayBTCPerKvB is the node's static relay floor.
	MinRelayBTCPerKvB float64 `json:"minrelaytxfee"`

	Size int64 `json:"size"`
}

// FloorSatPerVB is the higher of the two floors, in sat/vB, rounded up.
//
// Rounded up because this is a floor: a rate that rounds down to it is a rate
// the node's own mempool may refuse, and there is no RBF to fix that with.
func (m MempoolInfo) FloorSatPerVB() float64 {
	floor := m.MinFeeBTCPerKvB
	if m.MinRelayBTCPerKvB > floor {
		floor = m.MinRelayBTCPerKvB
	}
	sat := floor * 1e5
	// Two decimal places, rounded up: Core's floors are exact multiples of
	// 0.00001 BTC/kvB in practice and the ceiling keeps a floor a floor.
	return math.Ceil(sat*100) / 100
}

// GetMempoolInfo reads the node's relay floors.
func (c *Client) GetMempoolInfo(ctx context.Context) (MempoolInfo, error) {
	var out MempoolInfo
	if err := c.Call(ctx, "getmempoolinfo", nil, &out); err != nil {
		return MempoolInfo{}, fmt.Errorf("asking Core for its mempool floors: %w", err)
	}
	return out, nil
}

// Core's own error codes, for the cases where the code decides what to do next
// and the message is only for the operator.
const (
	// ErrNotFound is RPC_INVALID_ADDRESS_OR_KEY, which is what gettransaction
	// and getrawtransaction both return for a transaction Core does not have.
	ErrNotFound = -5

	// ErrWalletNotSpecified is RPC_WALLET_NOT_SPECIFIED: "Multiple wallets are
	// loaded. Please select which wallet to use...". A client with no wallet
	// scope gets it for every wallet-scoped call on a multi-wallet node, which
	// this harness and any real setup both are.
	ErrWalletNotSpecified = -19
)

// Confirmations reports how deep a transaction is: 0 for one that is in the
// mempool and unconfirmed, and a negative number for one Core believes has been
// conflicted out.
//
// Two RPCs, in this order, and the order is the point:
//
//   - gettransaction, which is wallet-scoped and answers for any transaction the
//     wallet has a stake in. The batch's change output pays the cold wallet, so
//     the funding transaction is always one of those — and this route needs no
//     txindex, which a real node may well not run.
//   - getrawtransaction, for a client with no wallet scope or a transaction the
//     wallet does not own. This one does need txindex, or the transaction still
//     in the mempool.
//
// present is false when Core has never heard of the transaction, which for a
// batch we published ourselves is a finding rather than an error: it means the
// broadcast did not stick.
func (c *Client) Confirmations(ctx context.Context, txid string) (confs int64, present bool, err error) {
	var wallet struct {
		Confirmations int64 `json:"confirmations"`
	}
	err = c.Call(ctx, "gettransaction", []any{txid}, &wallet)
	if err == nil {
		return wallet.Confirmations, true, nil
	}
	// Three ways gettransaction can decline without anything being wrong: the
	// wallet has no stake in the transaction, this client has no wallet scope at
	// all, or the node was built without wallet support. All three mean "ask
	// the node instead".
	switch {
	case IsRPCError(err, ErrNotFound), IsRPCError(err, ErrWalletNotSpecified),
		strings.Contains(err.Error(), "Method not found"):
	default:
		return 0, false, fmt.Errorf("asking Core how deep %s is: %w", txid, err)
	}

	var raw struct {
		Confirmations int64 `json:"confirmations"`
	}
	if err := c.Call(ctx, "getrawtransaction", []any{txid, true}, &raw); err != nil {
		if IsRPCError(err, ErrNotFound) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("asking Core how deep %s is: %w\n"+
			"Neither this node's wallet nor its transaction index has it. Without "+
			"-txindex, only a transaction the wallet has a stake in can be looked "+
			"up by txid", txid, err)
	}
	return raw.Confirmations, true, nil
}

// TestMempoolAccept asks Core whether a raw transaction would be accepted,
// without relaying it.
//
// This is the only pre-flight there is and it is specifically not a broadcast:
// Core validates against the mempool's rules and its own policy and answers,
// and the transaction goes nowhere. CLAUDE.md's rule that there must be exactly
// one publish call site is why this lives here rather than being written out at
// each of the three places that want it — a second function that takes a raw
// transaction and talks to Core is a second thing to check when reading for
// broadcast paths.
func (c *Client) TestMempoolAccept(ctx context.Context, rawTxHex string) (
	allowed bool, reason string, vsize int64, err error) {

	var out []struct {
		Allowed      bool   `json:"allowed"`
		TxID         string `json:"txid"`
		Vsize        int64  `json:"vsize"`
		RejectReason string `json:"reject-reason"`
	}
	if err := c.Call(ctx, "testmempoolaccept",
		[]any{[]string{rawTxHex}}, &out); err != nil {

		return false, "", 0, fmt.Errorf("testmempoolaccept: %w", err)
	}
	if len(out) != 1 {
		return false, "", 0, fmt.Errorf("testmempoolaccept answered about %d "+
			"transactions", len(out))
	}
	return out[0].Allowed, out[0].RejectReason, out[0].Vsize, nil
}
