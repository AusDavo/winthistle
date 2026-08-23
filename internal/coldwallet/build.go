package coldwallet

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"

	"github.com/AusDavo/winthistle/internal/bitcoind"
	"github.com/AusDavo/winthistle/internal/prose"
	"github.com/btcsuite/btcd/btcutil/psbt"
)

// Output is one thing the funding transaction pays.
type Output struct {
	Address   string
	AmountSat int64
}

// BuildRequest is step 4 of the sequence, directed mode: everything Core needs
// to turn the plan's outputs into one unsigned PSBT.
//
// It carries no defaults worth guessing at. A fee rate of zero would fall back
// to Core's own estimator, which is a different number from the one the plan was
// approved against, and an empty change address would let Core choose one the
// plan cannot name — so both are required.
type BuildRequest struct {
	// Outputs are the funding outputs and the reserve top-up, in the order the
	// plan lists them. Change is not here: Core derives it.
	Outputs []Output

	// ChangeAddress is where the change goes, and it is required.
	//
	// Directed mode asks the cold wallet for this — getrawchangeaddress on the
	// watch-only wallet, which derives from the internal descriptor — and passes
	// it in, so the plan can name the exact change script instead of having to
	// recognise one by key origin. That is the stronger of the two checks
	// internal/plan can make about change, and it is available here only because
	// we are the one building.
	ChangeAddress string

	// FeeRateSatPerVB comes from Core's estimatesmartfee, never a fee API.
	FeeRateSatPerVB float64

	// MinConfirmations is the floor Core applies to its own coin selection.
	//
	// SelectCoins has already judged every coin, so this is belt and braces
	// rather than the check — but the two must agree, and it is cheap to say so
	// twice. I-4 is why the floor exists: an unconfirmed parent can be replaced,
	// which moves our input, which moves our TXID, which destroys every channel
	// in the batch.
	MinConfirmations int
}

// Built is the unsigned transaction, and what Core did while making it.
type Built struct {
	// PSBT is base64, as Core returns it and as a browser download carries it.
	PSBT string

	// Raw is the same packet in bytes. LND parses raw bytes at psbt_verify —
	// psbt.NewFromRawBytes(r, false) — so both forms are kept rather than
	// converted twice at separate call sites.
	Raw []byte

	// TxID is the unsigned transaction's txid. I-3 pins this: from psbt_verify
	// onwards only signatures may be added, so any later PSBT has to still hash
	// to it.
	TxID string

	// Inputs are the coins Core chose, and therefore the coins Core has now
	// locked. They go in the run journal before the transaction goes anywhere,
	// because a crash here leaves a wallet that silently refuses to spend its own
	// money and nothing on disk saying why.
	Inputs []bitcoind.Outpoint

	// ChangeIndex is where Core put the change output. Core randomises the
	// position by default and nothing here asks it not to: the verifier
	// attributes outputs by script, so the position is information rather than
	// a dependency.
	ChangeIndex int

	// FeeSat is what Core says the fee is.
	FeeSat int64
}

// Build creates and funds the batch transaction from the watch-only cold wallet.
//
// The three options that are not negotiable:
//
//   - replaceable: false. I-4 at construction. Replacing the funding transaction
//     changes every outpoint in it and destroys every channel in the batch, so
//     there is no mode in which this is true and no operator control over it.
//   - lockUnspents: true. Core then holds the chosen coins unspendable for the
//     duration of the run, which is what stops a second process — or a second
//     tab — spending an input this transaction depends on. The locks are
//     memory-only and releasing them is internal/abort's job.
//   - bip32derivs: true. The key-origin information is what lets a hardware
//     signer recognise its own key; without it devices refuse to sign, and
//     assisted mode's change recognition has nothing to read.
//
// It does not choose the coins. SelectCoins judges them and FenceOff locks the
// ones the batch may not use, because Core has no "do not spend these" option
// and does skip locked outputs. So the caller's coin policy arrives here as the
// absence of the coins it excluded.
func Build(ctx context.Context, wallet *bitcoind.Client, req BuildRequest) (Built, error) {
	switch {
	case len(req.Outputs) == 0:
		return Built{}, fmt.Errorf("a funding transaction with no outputs is not a batch")
	case req.ChangeAddress == "":
		return Built{}, fmt.Errorf("no change address. I-4 forbids replacing this " +
			"transaction, so its change output is the only thing that could ever " +
			"accelerate it — and directed mode is the mode that gets to name the script")
	case req.FeeRateSatPerVB <= 0:
		return Built{}, fmt.Errorf("a fee rate of %g sat/vB is not a fee rate",
			req.FeeRateSatPerVB)
	case req.MinConfirmations < 0:
		return Built{}, fmt.Errorf("a confirmation floor of %d is not a number of blocks",
			req.MinConfirmations)
	}

	// Core takes the outputs as a list of single-entry objects. A list rather
	// than one object because "each key may only appear once": two outputs paying
	// the same address would silently become one, and the plan refuses a
	// duplicated script for exactly that reason.
	outputs := make([]map[string]any, 0, len(req.Outputs))
	seen := make(map[string]bool, len(req.Outputs))
	for _, o := range req.Outputs {
		if o.Address == "" {
			return Built{}, fmt.Errorf("an output of %s names no address", prose.Sats(o.AmountSat))
		}
		if o.AmountSat <= 0 {
			return Built{}, fmt.Errorf("the output to %s is for %d sat", o.Address, o.AmountSat)
		}
		if seen[o.Address] {
			return Built{}, fmt.Errorf("two outputs both pay %s. Core would collapse "+
				"them into one, so the batch would be a satoshi short of what LND "+
				"expects for one of them", o.Address)
		}
		seen[o.Address] = true
		// As text, not a float: prose.BTC is the one way this codebase renders a
		// satoshi as a BTC amount, and the funding amount is exact — LND compares
		// its own output with psbt.TxOutsEqual, which compares the value.
		outputs = append(outputs, map[string]any{o.Address: prose.BTC(o.AmountSat)})
	}
	if seen[req.ChangeAddress] {
		return Built{}, fmt.Errorf("the change address %s is also a payment in this "+
			"batch. The verifier attributes outputs by script, so it could not tell "+
			"the change output from the payment", req.ChangeAddress)
	}

	opts := map[string]any{
		"fee_rate":      req.FeeRateSatPerVB,
		"lockUnspents":  true,
		"replaceable":   false,
		"changeAddress": req.ChangeAddress,
		"minconf":       req.MinConfirmations,
		// Core's default for a watch-only wallet is already true; stated because
		// a wallet that turns out to hold a key must not silently start behaving
		// differently.
		"includeWatching": true,
		"add_inputs":      true,
	}

	var created struct {
		PSBT      string  `json:"psbt"`
		Fee       float64 `json:"fee"`
		ChangePos int     `json:"changepos"`
	}
	// The trailing true is bip32derivs.
	err := wallet.Call(ctx, "walletcreatefundedpsbt",
		[]any{[]any{}, outputs, 0, opts, true}, &created)
	if err != nil {
		return Built{}, fmt.Errorf("building the funding transaction: %w", err)
	}
	if created.ChangePos < 0 {
		return Built{}, fmt.Errorf("Core funded the batch without a change output. " +
			"I-4 means a batch with no change output is a batch that can only be " +
			"waited out: reduce a funding amount, or add a coin")
	}

	raw, err := base64.StdEncoding.DecodeString(created.PSBT)
	if err != nil {
		return Built{}, fmt.Errorf("Core returned a PSBT that is not base64: %w", err)
	}
	// Parsed here as well as by Core, and the two txids compared below, because
	// every later step reasons about this transaction through btcd's parser while
	// Core is the one that built it. A disagreement about these bytes is worth
	// finding now rather than at psbt_verify.
	parsed, err := psbt.NewFromRawBytes(bytes.NewReader(raw), false)
	if err != nil {
		return Built{}, fmt.Errorf("Core returned a PSBT this build cannot read: %w", err)
	}

	inputs, txid, err := decodeInputs(ctx, wallet, created.PSBT)
	if err != nil {
		return Built{}, err
	}
	if ours := parsed.UnsignedTx.TxHash().String(); ours != txid {
		return Built{}, fmt.Errorf("Core says the transaction it built is %s and this "+
			"build reads it as %s. I-3 pins one of those and the other would be the "+
			"one published", txid, ours)
	}

	return Built{
		PSBT:        created.PSBT,
		Raw:         raw,
		TxID:        txid,
		Inputs:      inputs,
		ChangeIndex: created.ChangePos,
		FeeSat:      satFromBTC(created.Fee),
	}, nil
}

// ChangeAddress asks the watch-only wallet for one of its own change addresses.
//
// It derives from the internal descriptor, so the address belongs to the cold
// wallet rather than to Core: the change comes home. This is the whole reason
// directed mode can name the exact change script in the plan.
func ChangeAddress(ctx context.Context, wallet *bitcoind.Client) (string, error) {
	var addr string
	if err := wallet.Call(ctx, "getrawchangeaddress", nil, &addr); err != nil {
		return "", fmt.Errorf("asking the cold wallet for a change address: %w", err)
	}
	if addr == "" {
		return "", fmt.Errorf("the cold wallet returned an empty change address")
	}
	return addr, nil
}

// decodeInputs reads the coins the PSBT spends, and its txid, from Core.
//
// Asked of Core rather than parsed here, deliberately: these are the outpoints
// Core just locked, and the point is to record what Core thinks it locked. The
// txid comes back from the same call, which is one fewer place for the two to
// disagree.
func decodeInputs(ctx context.Context, wallet *bitcoind.Client, psbtB64 string) (
	[]bitcoind.Outpoint, string, error) {

	var decoded struct {
		Tx struct {
			TxID string `json:"txid"`
			Vin  []struct {
				TxID string `json:"txid"`
				Vout uint32 `json:"vout"`
			} `json:"vin"`
		} `json:"tx"`
	}
	if err := wallet.Call(ctx, "decodepsbt", []any{psbtB64}, &decoded); err != nil {
		return nil, "", fmt.Errorf("reading back the transaction Core built: %w", err)
	}
	if len(decoded.Tx.Vin) == 0 {
		return nil, "", fmt.Errorf("Core built a transaction that spends nothing")
	}
	out := make([]bitcoind.Outpoint, 0, len(decoded.Tx.Vin))
	for _, in := range decoded.Tx.Vin {
		out = append(out, bitcoind.Outpoint{TxID: in.TxID, Vout: in.Vout})
	}
	return out, decoded.Tx.TxID, nil
}
