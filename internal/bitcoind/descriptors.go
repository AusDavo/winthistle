package bitcoind

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Core's JSON-RPC error codes, named where this package reacts to one rather
// than merely reporting it. Values from src/rpc/protocol.h.
const (
	ErrWalletNotFound      = -18 // RPC_WALLET_NOT_FOUND
	ErrWalletAlreadyLoaded = -35 // RPC_WALLET_ALREADY_LOADED
	ErrWalletError         = -4  // RPC_WALLET_ERROR — also "already exists"
	ErrMethodNotFound      = -32601

	// ErrInvalidAddressOrKey is what getmempoolentry returns for a transaction
	// Core does not have: "Transaction not in mempool". It is the ordinary
	// answer rather than a fault — the transaction confirmed, or it never went
	// out — so MempoolEntry turns it into a bool.
	ErrInvalidAddressOrKey = -5 // RPC_INVALID_ADDRESS_OR_KEY
)

// IsRPCError reports whether err is a Core JSON-RPC error with the given code.
func IsRPCError(err error, code int) bool {
	var rpcErr *Error
	return errors.As(err, &rpcErr) && rpcErr.Code == code
}

// ---------------------------------------------------------------------------
// Node-level calls. These do not need a wallet, and a client built with no
// Wallet in its Config can make them.
// ---------------------------------------------------------------------------

// ChainInfo is the part of getblockchaininfo the setup path reads.
type ChainInfo struct {
	Chain                string  `json:"chain"`
	Blocks               int64   `json:"blocks"`
	Headers              int64   `json:"headers"`
	VerificationProgress float64 `json:"verificationprogress"`
	InitialBlockDownload bool    `json:"initialblockdownload"`

	// Pruned and PruneHeight are the whole reason this call is here. A
	// descriptor import rescans from the wallet's birthday, and a pruned node
	// has thrown away the blocks it would have to read.
	Pruned      bool  `json:"pruned"`
	PruneHeight int64 `json:"pruneheight"`
}

// GetBlockchainInfo asks Core about the chain it is on and how much of it it
// still holds.
func (c *Client) GetBlockchainInfo(ctx context.Context) (ChainInfo, error) {
	var out ChainInfo
	if err := c.Call(ctx, "getblockchaininfo", nil, &out); err != nil {
		return ChainInfo{}, err
	}
	return out, nil
}

// NetworkInfo is the part of getnetworkinfo the setup path reads.
type NetworkInfo struct {
	Version         int    `json:"version"`
	SubVersion      string `json:"subversion"`
	ProtocolVersion int    `json:"protocolversion"`
}

// GetNetworkInfo reports Core's own version, as an integer of the form
// MMmmrr00 — 250100 is 25.1.0, 290000 is 29.0.
func (c *Client) GetNetworkInfo(ctx context.Context) (NetworkInfo, error) {
	var out NetworkInfo
	if err := c.Call(ctx, "getnetworkinfo", nil, &out); err != nil {
		return NetworkInfo{}, err
	}
	return out, nil
}

// BlockTime returns the timestamp in the header of the block at that height.
//
// This is how a prune horizon is compared against a wallet birthday. Core
// reports pruneheight as a height and the operator knows their birthday as a
// date, and the only honest way to put the two side by side is to read the
// header.
func (c *Client) BlockTime(ctx context.Context, height int64) (time.Time, error) {
	var hash string
	if err := c.Call(ctx, "getblockhash", []any{height}, &hash); err != nil {
		return time.Time{}, fmt.Errorf("getting the hash of block %d: %w", height, err)
	}
	var header struct {
		Time int64 `json:"time"`
	}
	if err := c.Call(ctx, "getblockheader", []any{hash}, &header); err != nil {
		return time.Time{}, fmt.Errorf("reading the header of block %d: %w", height, err)
	}
	return time.Unix(header.Time, 0).UTC(), nil
}

// DescriptorInfo is getdescriptorinfo's answer.
type DescriptorInfo struct {
	// Descriptor is the input with a checksum attached — Core computes it, so
	// this is the one value in the setup path we never have to write ourselves.
	Descriptor string `json:"descriptor"`

	Checksum       string `json:"checksum"`
	IsRange        bool   `json:"isrange"`
	IsSolvable     bool   `json:"issolvable"`
	HasPrivateKeys bool   `json:"hasprivatekeys"`
}

// DescribeDescriptor parses a descriptor and attaches its checksum.
//
// Node-level, not wallet-level: nothing is imported and no wallet is touched.
// A malformed descriptor, a bad key, or a wrong existing checksum all fail here,
// which is the cheapest place for them to fail.
func (c *Client) DescribeDescriptor(ctx context.Context, desc string) (DescriptorInfo, error) {
	var out DescriptorInfo
	if err := c.Call(ctx, "getdescriptorinfo", []any{desc}, &out); err != nil {
		return DescriptorInfo{}, err
	}
	return out, nil
}

// DeriveAddresses expands a ranged descriptor over [from, to] inclusive.
//
// Node-level and side-effect free, which is what makes it the right call for the
// round-trip check: the addresses come out of the descriptor arithmetic itself,
// not out of wallet state, and asking twice cannot advance a keypool.
func (c *Client) DeriveAddresses(ctx context.Context, desc string, from, to int) ([]string, error) {
	if from < 0 || to < from {
		return nil, fmt.Errorf("deriveaddresses: bad range [%d,%d]", from, to)
	}
	var out []string
	if err := c.Call(ctx, "deriveaddresses", []any{desc, []int{from, to}}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListWallets returns the wallets Core currently has loaded.
//
// Doubles as the wallet-support probe: a Core built with --disable-wallet does
// not register the method at all, so this fails with -32601 rather than
// returning an empty list.
func (c *Client) ListWallets(ctx context.Context) ([]string, error) {
	var out []string
	if err := c.Call(ctx, "listwallets", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListWalletDir returns every wallet on disk, loaded or not.
func (c *Client) ListWalletDir(ctx context.Context) ([]string, error) {
	var out struct {
		Wallets []struct {
			Name string `json:"name"`
		} `json:"wallets"`
	}
	if err := c.Call(ctx, "listwalletdir", nil, &out); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(out.Wallets))
	for _, w := range out.Wallets {
		names = append(names, w.Name)
	}
	return names, nil
}

// CreateWatchOnlyWallet creates a private-key-less, blank, descriptor wallet.
//
// Every one of those three matters and none of them is Core's default for a
// positional createwallet:
//
//   - disable_private_keys: the app must be structurally incapable of signing.
//     I-2 says the signers hold the keys and the app holds the last signature;
//     a Core wallet that could sign would put a broadcastable transaction one
//     RPC away.
//   - blank: no seed and no automatically generated descriptors, so the only
//     descriptors in the wallet are the ones we import. A non-blank wallet
//     would carry active descriptors of its own and derive change from them.
//   - descriptors: legacy wallets cannot hold an imported ranged multisig
//     descriptor at all.
//
// load_on_startup is set so a Core restart does not silently leave the wallet
// unloaded, which is a state the abort path has already had to be taught about.
//
// An existing wallet of that name is not an error: this is idempotent so that a
// half-finished setup can be resumed. What the wallet actually is gets read back
// through GetWalletInfo rather than inferred from the creation succeeding.
func (c *Client) CreateWatchOnlyWallet(ctx context.Context, name string) error {
	if name == "" {
		return fmt.Errorf("createwallet: no wallet name given")
	}
	// Positional, matching the design doc's documented invocation:
	// createwallet name disable_private_keys blank passphrase avoid_reuse
	//               descriptors load_on_startup
	params := []any{name, true, true, "", false, true, true}

	var out struct {
		Name    string `json:"name"`
		Warning string `json:"warning"`
	}
	err := c.Call(ctx, "createwallet", params, &out)
	if err == nil {
		return nil
	}
	if !isAlreadyExists(err) {
		return err
	}
	return c.EnsureWalletLoaded(ctx, name)
}

func isAlreadyExists(err error) bool {
	s := err.Error()
	return strings.Contains(s, "already exists") ||
		strings.Contains(s, "Database already exists")
}

// EnsureWalletLoaded loads a wallet unless Core already has it open.
//
// Core does not auto-load non-default wallets, so after any bitcoind restart
// every wallet-scoped call fails with "Requested wallet does not exist or is not
// loaded" — which reads like data loss and is not.
func (c *Client) EnsureWalletLoaded(ctx context.Context, name string) error {
	err := c.Call(ctx, "loadwallet", []any{name}, nil)
	switch {
	case err == nil:
		return nil
	case IsRPCError(err, ErrWalletAlreadyLoaded):
		return nil
	case strings.Contains(err.Error(), "already loaded"):
		return nil
	default:
		return err
	}
}

// ---------------------------------------------------------------------------
// Wallet-scoped calls. The client must have been built with a Wallet.
// ---------------------------------------------------------------------------

// WalletInfo is the part of getwalletinfo the setup path reads back.
type WalletInfo struct {
	Name string `json:"walletname"`

	// PrivateKeysEnabled must be false and Descriptors must be true. These are
	// read back rather than assumed, because createwallet is idempotent here and
	// the wallet that already existed may not be the wallet we would have made.
	PrivateKeysEnabled bool `json:"private_keys_enabled"`
	Descriptors        bool `json:"descriptors"`

	// Scanning is Core's rescan progress: literal false when idle, an object
	// while a rescan is running. See ScanProgress.
	Scanning ScanProgress `json:"scanning"`
}

// ScanProgress is getwalletinfo's scanning field, which Core types as either
// the boolean false or an object.
type ScanProgress struct {
	Running  bool
	Duration int64   // seconds so far
	Progress float64 // 0..1
}

// UnmarshalJSON copes with Core's two shapes for one field.
func (s *ScanProgress) UnmarshalJSON(b []byte) error {
	var flag bool
	if err := json.Unmarshal(b, &flag); err == nil {
		*s = ScanProgress{Running: flag}
		return nil
	}
	var obj struct {
		Duration int64   `json:"duration"`
		Progress float64 `json:"progress"`
	}
	if err := json.Unmarshal(b, &obj); err != nil {
		return fmt.Errorf("getwalletinfo scanning field is neither false nor an object: %w", err)
	}
	*s = ScanProgress{Running: true, Duration: obj.Duration, Progress: obj.Progress}
	return nil
}

// GetWalletInfo reads back what the wallet actually is.
func (c *Client) GetWalletInfo(ctx context.Context) (WalletInfo, error) {
	var out WalletInfo
	if err := c.Call(ctx, "getwalletinfo", nil, &out); err != nil {
		return WalletInfo{}, err
	}
	return out, nil
}

// ImportRequest is one entry of importdescriptors' argument.
type ImportRequest struct {
	Desc     string `json:"desc"`
	Active   bool   `json:"active"`
	Internal bool   `json:"internal"`
	Range    []int  `json:"range,omitempty"`

	// Timestamp is the wallet's birthday in Unix seconds, and Core rescans from
	// there. It is an int64 and not an any, so this type cannot express Core's
	// other accepted value, the string "now" — see coldwallet for why that is
	// deliberate.
	Timestamp int64 `json:"timestamp"`

	Label string `json:"label,omitempty"`
}

// ImportResult is one entry of importdescriptors' reply.
type ImportResult struct {
	Success  bool     `json:"success"`
	Warnings []string `json:"warnings"`
	Error    *Error   `json:"error"`
}

// ImportDescriptors imports descriptors into the wallet and rescans from each
// one's timestamp.
//
// The call blocks for the whole rescan, which on mainnet is minutes to hours.
// The context deadline and the client's HTTP timeout both have to cover it; the
// design's answer is to import during setup and never during a batch.
//
// Core reports per-entry failures inside a 200 response rather than as an RPC
// error, so the results are returned whole and the caller decides. A partial
// import — receive in, change refused — is exactly the state that produces a
// wallet which looks healthy and cannot derive change.
func (c *Client) ImportDescriptors(ctx context.Context, reqs []ImportRequest) ([]ImportResult, error) {
	if len(reqs) == 0 {
		return nil, fmt.Errorf("importdescriptors: nothing to import")
	}
	var out []ImportResult
	if err := c.Call(ctx, "importdescriptors", []any{reqs}, &out); err != nil {
		return nil, err
	}
	if len(out) != len(reqs) {
		return out, fmt.Errorf("importdescriptors: asked for %d, Core answered for %d",
			len(reqs), len(out))
	}
	return out, nil
}

// WalletDescriptor is one entry of listdescriptors' reply: what the wallet
// actually holds, as opposed to what we asked it to hold.
type WalletDescriptor struct {
	Desc      string `json:"desc"`
	Timestamp int64  `json:"timestamp"`
	Active    bool   `json:"active"`
	Internal  bool   `json:"internal"`
	Range     []int  `json:"range"`
	Next      int    `json:"next"`
}

// ListDescriptors reads back the wallet's descriptors.
//
// Never with the private=true argument. This wallet has no private keys by
// construction, and a call that would print them if it did is not one this
// package should be able to make.
func (c *Client) ListDescriptors(ctx context.Context) ([]WalletDescriptor, error) {
	var out struct {
		WalletName  string             `json:"wallet_name"`
		Descriptors []WalletDescriptor `json:"descriptors"`
	}
	if err := c.Call(ctx, "listdescriptors", nil, &out); err != nil {
		return nil, err
	}
	return out.Descriptors, nil
}

// AddressInfo is the part of getaddressinfo the round-trip check reads.
type AddressInfo struct {
	Address      string `json:"address"`
	ScriptPubKey string `json:"scriptPubKey"`
	IsMine       bool   `json:"ismine"`
	Solvable     bool   `json:"solvable"`
	IsWatchOnly  bool   `json:"iswatchonly"`
	Descriptor   string `json:"desc"`

	// IsChange is Core's ischange, and it does not mean what its name suggests.
	// Core's IsChange is "an output of ours that has no address-book entry", so
	// it is true for any address that has never received — external branch
	// included — and false for an internal address that has. It is not the
	// read-back of importdescriptors' internal flag; listdescriptors is.
	IsChange bool `json:"ischange"`

	// ParentDesc names the imported descriptor the address came from, which is
	// what turns "the wallet recognises this" into "the wallet recognises this as
	// coming from the descriptor we imported".
	//
	// Both spellings are here because Core uses both: getaddressinfo answers with
	// the singular parent_desc and listunspent with the plural parent_descs.
	ParentDesc  string   `json:"parent_desc"`
	ParentDescs []string `json:"parent_descs"`

	// Hex is the redeem or witness script behind a P2SH or P2WSH address, which
	// Core reports under the unhelpful name "hex". It is what a multisig spend
	// has to be sized against, and it is the reason this field exists: once an
	// output is spent by an unconfirmed transaction Core drops it from
	// listunspent, so listunspent's witnessScript is no longer reachable and this
	// is the remaining route to the same bytes. See internal/bump, which needs it
	// to size a replacement of a standing CPFP child.
	Hex string `json:"hex"`
}

// Ours reports whether the wallet considers this address its own, under either
// of the two flags a descriptor wallet may use.
//
// Both, because a watch-only descriptor wallet reports ismine true and
// iswatchonly false — observed on the harness's cold-watch — which is not what
// the names suggest. Matching only iswatchonly would decide that a watch-only
// wallet does not own its own addresses.
func (a AddressInfo) Ours() bool { return a.IsMine || a.IsWatchOnly }

// Parents returns the parent descriptors under either of Core's two spellings.
func (a AddressInfo) Parents() []string {
	if a.ParentDesc != "" {
		return append([]string{a.ParentDesc}, a.ParentDescs...)
	}
	return a.ParentDescs
}

// GetAddressInfo asks the wallet what it knows about an address.
func (c *Client) GetAddressInfo(ctx context.Context, addr string) (AddressInfo, error) {
	var out AddressInfo
	if err := c.Call(ctx, "getaddressinfo", []any{addr}, &out); err != nil {
		return AddressInfo{}, err
	}
	return out, nil
}

// UTXO is one entry of listunspent.
type UTXO struct {
	TxID          string  `json:"txid"`
	Vout          uint32  `json:"vout"`
	Address       string  `json:"address"`
	ScriptPubKey  string  `json:"scriptPubKey"`
	WitnessScript string  `json:"witnessScript"`
	RedeemScript  string  `json:"redeemScript"`
	Amount        float64 `json:"amount"`
	Confirmations int64   `json:"confirmations"`
	Spendable     bool    `json:"spendable"`
	Solvable      bool    `json:"solvable"`
	Safe          bool    `json:"safe"`
	Descriptor    string  `json:"desc"`
}

// Outpoint returns the UTXO's outpoint, so a coin can be handed straight to
// LockForRun.
func (u UTXO) Outpoint() Outpoint { return Outpoint{TxID: u.TxID, Vout: u.Vout} }

// ListUnspent lists the wallet's unspent outputs at or above minConf
// confirmations.
//
// Locked outputs are absent — Core filters them — which is what makes locking
// the legacy coins an exclusion mechanism rather than a bookkeeping one.
func (c *Client) ListUnspent(ctx context.Context, minConf, maxConf int) ([]UTXO, error) {
	var out []UTXO
	if err := c.Call(ctx, "listunspent", []any{minConf, maxConf}, &out); err != nil {
		return nil, err
	}
	return out, nil
}
