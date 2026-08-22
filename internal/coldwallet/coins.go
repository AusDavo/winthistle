package coldwallet

import (
	"context"
	"encoding/hex"
	"fmt"
	"math"
	"sort"

	"github.com/AusDavo/winthistle/internal/bitcoind"
	"github.com/AusDavo/winthistle/internal/plan"
)

// Why a coin cannot go into the batch.
type Why int

const (
	// Legacy: spending it is not a SegWit spend, so the transaction would be
	// malleable and LND refuses it outright — verifyAllInputsSegWit in
	// lnwallet/chanfunding/psbt_assembler.go, "risk of malleability". I-3 is the
	// same fact from the other side: a malleable input is a TXID that can move
	// after psbt_verify has committed to it.
	Legacy Why = iota

	// Unconfirmed: below the batch's confirmation floor.
	Unconfirmed

	// Unsolvable: Core cannot work out how to spend it, so it cannot put the
	// derivations in the PSBT that a signer needs to recognise its own key.
	Unsolvable

	// Unsafe: Core's own "safe" flag is false — an unconfirmed output from a
	// transaction this wallet did not send, which may still be replaced.
	Unsafe
)

func (w Why) String() string {
	switch w {
	case Legacy:
		return "not a segwit spend"
	case Unconfirmed:
		return "not confirmed"
	case Unsolvable:
		return "not solvable"
	case Unsafe:
		return "not safe to spend"
	default:
		return "unknown"
	}
}

// Excluded is one coin the batch may not use, and why.
type Excluded struct {
	Coin   bitcoind.UTXO
	Why    Why
	Detail string
}

// Coins is the cold wallet's spendable set, split.
//
// The excluded half is carried rather than discarded because the operator is
// entitled to know why their balance is smaller than their wallet software says
// it is. A coin selection that silently drops a third of the wallet and then
// reports "insufficient funds" is the worst version of this.
type Coins struct {
	MinConfirmations int
	Eligible         []bitcoind.UTXO
	EligibleSat      int64
	Excluded         []Excluded
	ExcludedSat      int64
}

// ExcludedOutpoints returns the excluded coins' outpoints, ready for LockForRun.
func (c Coins) ExcludedOutpoints() []bitcoind.Outpoint {
	out := make([]bitcoind.Outpoint, 0, len(c.Excluded))
	for _, e := range c.Excluded {
		out = append(out, e.Coin.Outpoint())
	}
	return out
}

// SelectCoins reads the cold wallet's unspent outputs and splits them.
//
// minConf is the confirmation floor. The design's prerequisite is confirmed
// coins, and the reason is I-4 rather than fastidiousness: an unconfirmed parent
// can be replaced, which moves our input, which moves our TXID, which destroys
// every channel in the batch — and we cannot RBF our way out because replacing
// the funding transaction does the same thing.
func SelectCoins(ctx context.Context, wallet *bitcoind.Client, minConf int) (Coins, error) {
	if minConf < 0 {
		return Coins{}, fmt.Errorf("a confirmation floor of %d is not a number of blocks", minConf)
	}
	// Ask for everything and judge it here rather than letting Core filter by
	// confirmations, so the coins that were left out can be named.
	utxos, err := wallet.ListUnspent(ctx, 0, math.MaxInt32)
	if err != nil {
		return Coins{}, fmt.Errorf("listing the cold wallet's coins: %w", err)
	}

	c := Coins{MinConfirmations: minConf}
	for _, u := range utxos {
		sat := satFromBTC(u.Amount)
		if why, detail, ok := excludes(u, minConf); ok {
			c.Excluded = append(c.Excluded, Excluded{Coin: u, Why: why, Detail: detail})
			c.ExcludedSat += sat
			continue
		}
		c.Eligible = append(c.Eligible, u)
		c.EligibleSat += sat
	}

	// Largest first, so the operator reads the set in the order coin selection
	// will most likely consume it.
	sort.SliceStable(c.Eligible, func(i, j int) bool {
		return c.Eligible[i].Amount > c.Eligible[j].Amount
	})
	sort.SliceStable(c.Excluded, func(i, j int) bool {
		if c.Excluded[i].Why != c.Excluded[j].Why {
			return c.Excluded[i].Why < c.Excluded[j].Why
		}
		return c.Excluded[i].Coin.Amount > c.Excluded[j].Coin.Amount
	})
	return c, nil
}

// excludes judges one coin.
func excludes(u bitcoind.UTXO, minConf int) (Why, string, bool) {
	if segwit, detail := segwitSpend(u.ScriptPubKey, u.RedeemScript); !segwit {
		return Legacy, detail, true
	}
	if !u.Solvable {
		return Unsolvable, "Core does not have the script or the derivations for it, " +
			"so it cannot build a PSBT a signer could complete", true
	}
	if u.Confirmations < int64(minConf) {
		return Unconfirmed, fmt.Sprintf("%d confirmation%s, floor is %d",
			u.Confirmations, plural(u.Confirmations), minConf), true
	}
	if !u.Safe {
		return Unsafe, "Core marks it unsafe: an unconfirmed output from a transaction " +
			"this wallet did not send, which its sender may still replace", true
	}
	return 0, "", false
}

func plural(n int64) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// segwitSpend judges one coin's scriptPubKey.
//
// The predicate itself lives in internal/plan, next to the reading of LND's
// verifyAllInputsSegWit that it mirrors, so that coin selection and the batch
// verifier cannot come to different conclusions about the same coin. This is
// only the hex-decoding around it: Core hands scripts out as hex and LND
// reasons about them as bytes.
func segwitSpend(pkScriptHex, redeemScriptHex string) (bool, string) {
	pkScript, err := hex.DecodeString(pkScriptHex)
	if err != nil {
		return false, fmt.Sprintf("its scriptPubKey is not hex (%q)", pkScriptHex)
	}
	var redeem []byte
	if redeemScriptHex != "" {
		if redeem, err = hex.DecodeString(redeemScriptHex); err != nil {
			return false, fmt.Sprintf("its redeem script is not hex (%q)", redeemScriptHex)
		}
	}
	return plan.IsSegwitSpend(pkScript, redeem)
}

// FenceOff locks the excluded coins so Core's own coin selection cannot reach
// them, and reports which ones it actually locked.
//
// Core has no "exclude these" option on walletcreatefundedpsbt, and it does skip
// locked outputs — so a lock is the mechanism, and it is the same mechanism the
// run journal and the abort path already know how to release. Locks are
// memory-only, so the worst case if this is never undone is that a Core restart
// undoes it.
//
// It is not an error for there to be nothing to fence off.
func FenceOff(ctx context.Context, wallet *bitcoind.Client, c Coins) ([]bitcoind.Outpoint, error) {
	ops := c.ExcludedOutpoints()
	if len(ops) == 0 {
		return nil, nil
	}
	locked, err := wallet.LockForRun(ctx, ops)
	if err != nil {
		return nil, fmt.Errorf("locking the %d coin%s the batch may not spend: %w",
			len(ops), plural(int64(len(ops))), err)
	}
	return locked, nil
}

// satFromBTC converts Core's BTC float to satoshis.
//
// Core hands out amounts as JSON numbers and there is no way around parsing one;
// rounding at the satoshi is what keeps 0.1 from arriving as 9999999.
func satFromBTC(btc float64) int64 { return int64(math.Round(btc * 1e8)) }
