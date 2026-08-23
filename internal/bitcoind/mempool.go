package bitcoind

import (
	"context"
	"fmt"
	"math"
)

// MempoolEntry is what Core knows about a transaction sitting in its mempool.
//
// It is the authoritative answer for a CPFP child's arithmetic and the reason
// nothing here sums prevouts to work out a parent's fee: a transaction that is
// being bumped is by definition unconfirmed, so Core already holds its exact
// virtual size and its exact fee, and re-deriving either from the inputs would
// be a second opinion that could disagree with the one the fee market actually
// uses.
type MempoolEntry struct {
	// VsizeVB and FeeSat are this transaction's own.
	VsizeVB int64
	FeeSat  int64

	// AncestorCount, AncestorVsizeVB and AncestorFeeSat cover this transaction
	// *and* every unconfirmed transaction it depends on — Core's own wording is
	// "including this one", so a transaction whose inputs are all confirmed
	// reports a count of 1 and ancestor figures equal to its own.
	//
	// These are the figures a CPFP child has to be sized against, not the ones
	// above. walletcreatefundedpsbt charges the fee that lifts the whole
	// unconfirmed ancestor package to the rate it is given, and "the ancestor
	// package" means all of it. A batch built from confirmed coins makes the two
	// pairs identical, which is the ordinary case and is exactly why the
	// difference is easy to miss.
	AncestorCount   int64
	AncestorVsizeVB int64
	AncestorFeeSat  int64

	// DescendantCount includes this transaction too, so anything above 1 means
	// something already spends one of its outputs. For a batch that means a
	// child already exists, and a second one cannot spend the same change.
	DescendantCount int64

	// SpentBy are the unconfirmed transactions spending this one's outputs.
	SpentBy []string
}

// AncestorRate is what the whole unconfirmed package this transaction sits on
// currently pays.
func (e MempoolEntry) AncestorRate() float64 {
	if e.AncestorVsizeVB <= 0 {
		return 0
	}
	return float64(e.AncestorFeeSat) / float64(e.AncestorVsizeVB)
}

// HasUnconfirmedAncestors reports whether this transaction is not the bottom of
// its own package.
func (e MempoolEntry) HasUnconfirmedAncestors() bool { return e.AncestorCount > 1 }

// MempoolEntry asks Core about one unconfirmed transaction.
//
// The bool is whether Core has it. A transaction that is not in the mempool is
// the ordinary answer to a reasonable question — it confirmed, or it never went
// out — so it is not an error, and Core's own -5 for it is translated rather
// than passed up. Every other failure is a failure.
func (c *Client) MempoolEntry(ctx context.Context, txid string) (MempoolEntry, bool, error) {
	var raw struct {
		Vsize           int64 `json:"vsize"`
		AncestorCount   int64 `json:"ancestorcount"`
		AncestorSize    int64 `json:"ancestorsize"`
		DescendantCount int64 `json:"descendantcount"`
		Fees            struct {
			Base     float64 `json:"base"`
			Ancestor float64 `json:"ancestor"`
		} `json:"fees"`
		SpentBy []string `json:"spentby"`
	}
	err := c.Call(ctx, "getmempoolentry", []any{txid}, &raw)
	if IsRPCError(err, ErrInvalidAddressOrKey) {
		return MempoolEntry{}, false, nil
	}
	if err != nil {
		return MempoolEntry{}, false, fmt.Errorf("asking Core about %s in its mempool: %w",
			txid, err)
	}
	return MempoolEntry{
		VsizeVB:         raw.Vsize,
		FeeSat:          sats(raw.Fees.Base),
		AncestorCount:   raw.AncestorCount,
		AncestorVsizeVB: raw.AncestorSize,
		AncestorFeeSat:  sats(raw.Fees.Ancestor),
		DescendantCount: raw.DescendantCount,
		SpentBy:         raw.SpentBy,
	}, true, nil
}

// TxOut is one output of a transaction, as Core decodes it.
type TxOut struct {
	Vout      uint32
	AmountSat int64
	Address   string
	ScriptHex string
}

// TxOutputs reads a transaction's outputs back out of the node.
//
// It exists for one case: finding a batch's change output after something has
// already spent it. listunspent is the ordinary route and Core drops an output
// from it the moment an unconfirmed transaction spends it, so a second CPFP lift
// — which by definition spends what the first one spent — cannot see the coin it
// is about to replace a spend of. The transaction itself still has the output,
// and while the parent is unconfirmed it is in the mempool, so getrawtransaction
// answers without txindex.
//
// verbose=true, so this needs no wallet scope: it is a node-level call about a
// transaction, not a wallet's view of one.
func (c *Client) TxOutputs(ctx context.Context, txid string) ([]TxOut, error) {
	var raw struct {
		Vout []struct {
			Value        float64 `json:"value"`
			N            uint32  `json:"n"`
			ScriptPubKey struct {
				Hex     string `json:"hex"`
				Address string `json:"address"`
			} `json:"scriptPubKey"`
		} `json:"vout"`
	}
	if err := c.Call(ctx, "getrawtransaction", []any{txid, true}, &raw); err != nil {
		return nil, fmt.Errorf("reading %s back out of the node: %w", txid, err)
	}
	out := make([]TxOut, 0, len(raw.Vout))
	for _, v := range raw.Vout {
		out = append(out, TxOut{
			Vout:      v.N,
			AmountSat: sats(v.Value),
			Address:   v.ScriptPubKey.Address,
			ScriptHex: v.ScriptPubKey.Hex,
		})
	}
	return out, nil
}

// sats converts one of Core's BTC amounts to satoshis.
//
// Rounded rather than truncated: Core renders these as JSON numbers and a
// float64 holding 0.00010000 can arrive as 0.00009999999, which truncation
// turns into a fee one satoshi light. Every figure this touches is compared
// against an exact integer somewhere.
func sats(btc float64) int64 { return int64(math.Round(btc * 1e8)) }
