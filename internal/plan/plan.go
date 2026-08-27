// Package plan is the batch plan and the verifier that checks what comes back.
//
// In assisted mode the app has no Bitcoin Core, so it cannot build the funding
// transaction. It emits a plan instead — every funding address with its exact
// amount, the reserve top-up, a target fee rate and the input constraints — the
// operator builds that transaction in Sparrow, and this package verifies what
// returns, output by output. Directed mode runs the same verifier over the
// transaction Core built, because a check worth doing on a stranger's work is
// worth doing on our own.
//
// # Why this check is ours and nobody else's
//
// It is tempting to think LND's psbt_verify already does this. It does not, and
// the gap is not an oversight — it is what makes the batch possible.
// PsbtIntent.Verify, in lnwallet/chanfunding/psbt_assembler.go at v0.21.2-beta:
//
//	outputFound := false
//	outputSum := int64(0)
//	for _, out := range packet.UnsignedTx.TxOut {
//	        outputSum += out.Value
//	        if psbt.TxOutsEqual(out, expectedOutput) {
//	                outputFound = true
//	        }
//	}
//
// It looks for its own output and stops caring. It never asserts that its output
// is the only one — which is exactly why n funding streams can each verify the
// same n-output transaction, and therefore exactly why "no outputs to scripts the
// plan does not name" is a check only we can make. Each stream vouches for its
// own funding output. Nobody vouches for the rest of the transaction.
//
// The other two things Verify does are just as narrow. It requires the input sum
// to exceed the *total* output sum — "we don't want to dive into fee estimation
// here" — so any fee above zero passes, and a returned PSBT paying itself a
// hundred times the intended fee satisfies it. And it runs verifyAllInputsSegWit,
// which is satisfied by the mere presence of a WitnessUtxo field rather than by
// the prevout script actually being a witness program. This package reads the
// script.
//
// So a returned PSBT can pass all n psbt_verify calls while quietly paying an
// output the plan never named. That is the failure this package exists to stop.
package plan

import (
	"context"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/AusDavo/winthistle/internal/policy"
	"github.com/AusDavo/winthistle/internal/reserve"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightningnetwork/lnd/lnrpc"
	"google.golang.org/grpc"
)

// Params resolves a chain name to the parameters address decoding needs.
//
// Both LND and Core are asked for their chain name rather than it being
// configured, so this accepts what either of them says.
func Params(chain string) (*chaincfg.Params, error) {
	switch strings.ToLower(strings.TrimSpace(chain)) {
	case "mainnet", "main", "bitcoin":
		return &chaincfg.MainNetParams, nil
	case "testnet", "testnet3", "test":
		return &chaincfg.TestNet3Params, nil
	case "testnet4":
		return &chaincfg.TestNet4Params, nil
	case "signet":
		return &chaincfg.SigNetParams, nil
	case "regtest":
		return &chaincfg.RegressionNetParams, nil
	default:
		return nil, fmt.Errorf("unknown chain %q", chain)
	}
}

// Channel is one funding output the plan names.
type Channel struct {
	// Peer is the peer's pubkey, and PendingChanID the stream that produced this
	// address. Neither is checked against the transaction — a transaction knows
	// nothing about either — but a finding that cannot name the channel it
	// concerns is a finding the operator cannot act on.
	Peer          string
	PendingChanID string

	// Alias is the peer's self-declared name from LND's gossip graph, carried
	// for the operator's eyes and nothing else.
	//
	// It never replaces the pubkey in any label, and that is not a style choice.
	// An alias is whatever a node announces about itself: it is not unique, not
	// verified by anybody, and two nodes may claim the same one. A sheet that
	// said only "channel 1 to bitrefill" would be asking the operator to check
	// an amount against a name that any node on the network can adopt. So the
	// key stays, and the alias sits beside it as the thing that makes the key
	// legible.
	//
	// Empty is ordinary: a peer the graph has not heard of, or one that has
	// never set an alias, looks exactly like this.
	Alias string

	// Address and AmountSat are LND's own psbt_fund answer, and both are exact.
	// LND compares its expected output with psbt.TxOutsEqual, which compares the
	// value as well as the script, so a satoshi of rounding is a channel that
	// never opens.
	Address   string
	AmountSat int64

	// Private is whether the channel will be unannounced. Not a constraint on
	// the transaction — an announced and an unannounced channel produce the same
	// output — but it changes two things the operator is reviewing here: an
	// unannounced channel is invisible to LND's anchor reserve, and it will not
	// route for anybody, which makes its forwarding policy moot.
	Private bool

	// Policy is what this channel will charge to route once Phase 2 has applied
	// it, and nil means it will be left at LND's own defaults.
	//
	// It is in the plan because it is part of the same decision: the operator
	// reviewing where the money goes is the last person who can change what it
	// will earn, and after this document is approved the policy is applied by a
	// loop with no human in it. The verifier says nothing about it — a
	// transaction carries no forwarding policy — so Document renders it and
	// Verify ignores it.
	Policy *policy.Policy
}

// TopUp is the reserve top-up: an output paying the node's own wallet, so that
// LND's anchor reserve is met at psbt_verify.
//
// It belongs in the batch rather than in a separate transaction because
// CheckReservedValue credits transaction outputs paying an address the node's
// wallet owns, so a top-up inside the batch counts at step 5 with no second
// transaction and no wait. See internal/reserve.
type TopUp struct {
	// Address must have come from the node's own wallet — lnrpc NewAddress. The
	// verifier checks the returned transaction pays this exact script; what makes
	// that a check of "an address we control" rather than of "an address the plan
	// happens to name" is that the app minted it. See TopUpAddress.
	Address   string
	AmountSat int64

	// Shortfall is what internal/reserve said was missing, for the report.
	Shortfall int64
}

// Change is the cold wallet's change output.
//
// A batch that cannot be replaced can only be accelerated by spending its
// change, so a batch without one has no lever on it at all. That is worth
// saying and is not worth refusing over — the verifier reports it, and its size
// against a CPFP child that could actually lift it.
//
// What this type is for is telling the verifier which output is the change. That
// is an attribution question rather than a fee-and-change one, and it is refused
// when it cannot be answered: an output nobody can account for is the thing this
// program exists to catch.
type Change struct {
	// Address is the exact change script, when it is known. Directed mode always
	// knows it, because Core derives it. In assisted mode Sparrow chooses its own
	// change address and this is empty — see Recognise.
	Address string

	// Recognise is how an unnamed output is accepted as change when Address is
	// empty. It is strictly weaker than naming the script and the report says so.
	Recognise *Recognition
}

// Recognition identifies the cold wallet's own change output from the key-origin
// information in the PSBT, which is the same evidence a hardware signer uses to
// decide an output is its own change rather than a payment.
type Recognition struct {
	// Fingerprints are the master key fingerprints of every key in the cold
	// wallet's descriptor, read out of the [xxxxxxxx/...] origin prefixes.
	Fingerprints []uint32

	// Branch is the change descriptor's derivation branch — 1 in every ordinary
	// wallet. A derivation on the receive branch is not change.
	Branch uint32
}

// Outpoint is a coin, in the byte order humans and Core use.
type Outpoint struct {
	TxID string
	Vout uint32
}

func (o Outpoint) String() string { return fmt.Sprintf("%s:%d", o.TxID, o.Vout) }

// Inputs is what the plan requires of the coins.
type Inputs struct {
	// MinConfirmations is the floor the plan asks for. The verifier cannot check
	// it from a PSBT — a PSBT carries no chain height — so it is stated here,
	// checked in directed mode during coin selection, and reported as unchecked
	// in assisted mode rather than silently assumed.
	MinConfirmations int

	// Allowed, when non-empty, is the exact set of coins the builder may spend.
	// Directed mode fills it in; assisted mode usually cannot.
	Allowed []Outpoint

	// Excluded is what coin selection left out and why, for the plan document.
	// Purely informational: an excluded coin that turns up as an input is caught
	// by the segwit check or by Allowed, not by this.
	Excluded []string
}

// Plan is the reviewable artifact. It is written during Phase 0 and it is what
// the armed window executes.
type Plan struct {
	Chain    string
	Channels []Channel
	TopUp    *TopUp
	Change   Change
	Inputs   Inputs
}

// Named is one output the plan names, resolved to a script.
type Named struct {
	Kind      Kind
	Label     string
	Script    []byte
	Address   string
	AmountSat int64

	// Exact is whether the amount must match to the satoshi. Funding outputs and
	// the change floor differ here: LND compares its funding output with
	// TxOutsEqual, so a funding amount is exact, while a top-up or a change
	// output that is larger than planned is not a problem.
	Exact bool
}

// Kind says what an output is for.
type Kind int

const (
	Funding Kind = iota
	Reserve
	ChangeOut
)

func (k Kind) String() string {
	switch k {
	case Funding:
		return "funding"
	case Reserve:
		return "reserve top-up"
	case ChangeOut:
		return "change"
	default:
		return "unknown"
	}
}

// Outputs resolves everything the plan names into scripts.
//
// This is also the plan's validation: a plan whose outputs cannot be resolved,
// or which names one script twice, is a plan the verifier could not reason
// about, so it is refused here rather than producing confident nonsense later.
func (p *Plan) Outputs() ([]Named, error) {
	params, err := Params(p.Chain)
	if err != nil {
		return nil, err
	}
	if len(p.Channels) == 0 {
		return nil, fmt.Errorf("a plan with no channels in it is not a batch")
	}

	var out []Named
	seen := map[string]string{} // script hex -> label

	add := func(n Named) error {
		key := hex.EncodeToString(n.Script)
		if prev, dup := seen[key]; dup {
			return fmt.Errorf("%s and %s are the same script (%s). The verifier "+
				"attributes outputs by script, so it could not tell them apart",
				prev, n.Label, n.Address)
		}
		seen[key] = n.Label
		out = append(out, n)
		return nil
	}

	for i, ch := range p.Channels {
		label := fmt.Sprintf("channel %d", i+1)
		if ch.Peer != "" {
			label = fmt.Sprintf("channel %d to %s", i+1, ch.who())
		}
		script, err := ScriptFor(ch.Address, params)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", label, err)
		}
		if ch.AmountSat <= 0 {
			return nil, fmt.Errorf("%s: a funding amount of %d is not an amount",
				label, ch.AmountSat)
		}
		// A policy LND will refuse is refused here, where it costs nothing.
		// The alternative is Phase 2 discovering it after the transaction is
		// public: UpdateChannelPolicy reports an invalid parameter inside a
		// successful response, polling cannot make it valid, and the channel
		// routes at 1 ppm until somebody notices.
		if ch.Policy != nil {
			if err := ch.Policy.Validate(); err != nil {
				return nil, fmt.Errorf("%s: %w", label, err)
			}
		}
		if err := add(Named{Kind: Funding, Label: label, Script: script,
			Address: ch.Address, AmountSat: ch.AmountSat, Exact: true}); err != nil {
			return nil, err
		}
	}

	if p.TopUp != nil {
		if p.TopUp.Address == "" {
			return nil, fmt.Errorf("the reserve top-up names no address")
		}
		script, err := ScriptFor(p.TopUp.Address, params)
		if err != nil {
			return nil, fmt.Errorf("the reserve top-up: %w", err)
		}
		if p.TopUp.AmountSat <= 0 {
			return nil, fmt.Errorf("the reserve top-up is for %d sat", p.TopUp.AmountSat)
		}
		if err := add(Named{Kind: Reserve, Label: "the reserve top-up", Script: script,
			Address: p.TopUp.Address, AmountSat: p.TopUp.AmountSat}); err != nil {
			return nil, err
		}
	}

	switch {
	case p.Change.Address != "":
		script, err := ScriptFor(p.Change.Address, params)
		if err != nil {
			return nil, fmt.Errorf("the change output: %w", err)
		}
		if err := add(Named{Kind: ChangeOut, Label: "the change output", Script: script,
			Address: p.Change.Address}); err != nil {
			return nil, err
		}
	case p.Change.Recognise != nil:
		if len(p.Change.Recognise.Fingerprints) == 0 {
			return nil, fmt.Errorf("the change output is to be recognised by key " +
				"origin, but no master key fingerprints were given")
		}
	default:
		// Not a judgement about the transaction's change arrangements — those are
		// reported, not refused. This is the plan being unable to tell change from
		// an output nobody named, which is the check the program is for.
		return nil, fmt.Errorf("the plan has no way to identify the change output, " +
			"so an output your own wallet paid itself cannot be told from one paying " +
			"a stranger: name a change address, or say how to recognise one")
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out, nil
}

// ScriptFor decodes an address and renders its scriptPubKey.
func ScriptFor(addr string, params *chaincfg.Params) ([]byte, error) {
	decoded, err := btcutil.DecodeAddress(addr, params)
	if err != nil {
		return nil, fmt.Errorf("%q is not an address: %w", addr, err)
	}
	// DecodeAddress is lenient about networks for some formats, so this is not
	// redundant: a testnet address on mainnet would otherwise resolve to a
	// script that pays a stranger.
	if !decoded.IsForNet(params) {
		return nil, fmt.Errorf("%q is not a %s address", addr, params.Name)
	}
	script, err := txscript.PayToAddrScript(decoded)
	if err != nil {
		return nil, fmt.Errorf("building the script for %q: %w", addr, err)
	}
	return script, nil
}

// TotalOutSat is what the plan intends to pay out, change aside.
func (p *Plan) TotalOutSat() int64 {
	var total int64
	for _, ch := range p.Channels {
		total += ch.AmountSat
	}
	if p.TopUp != nil {
		total += p.TopUp.AmountSat
	}
	return total
}

// who names the peer as the operator should read it: the alias for legibility,
// the key because the alias is not an identifier.
func (c Channel) who() string {
	if c.Alias == "" {
		return shortPeer(c.Peer)
	}
	return fmt.Sprintf("%s (%s)", c.Alias, shortPeer(c.Peer))
}

func shortPeer(pubkey string) string {
	if len(pubkey) <= 16 {
		return pubkey
	}
	return pubkey[:16] + "…"
}

// ---------------------------------------------------------------------------
// The one thing the plan needs LND for
// ---------------------------------------------------------------------------

// Client is the slice of LND this package needs: one call, to mint the top-up
// address.
//
// Narrow for the same reason internal/reserve's is. Building a plan happens
// before the operator has agreed to anything, and the type says that a plan
// cannot open a stream, move a coin or publish anything.
type Client interface {
	NewAddress(context.Context, *lnrpc.NewAddressRequest,
		...grpc.CallOption) (*lnrpc.NewAddressResponse, error)
}

// TopUpAddress asks the node for a fresh address of its own wallet, for the
// reserve top-up output.
//
// This is what makes "the top-up really pays an address we control" a fact
// rather than a hope. CheckReservedValue credits an output only when
// IsOurAddress says the node's wallet owns it — l.IsOurAddress(addr) in
// lnwallet/wallet.go, which is btcwallet's HaveAddress — and an address the node
// just minted is one it owns by construction. The verifier then only has to
// check that the returned transaction pays this exact script.
//
// P2TR because the reserve exists to fee-bump anchor closes and the cheapest
// input to spend later is the right one to create now. The address type is
// immaterial to CheckReservedValue: ExtractPkScriptAddrs handles v1 witness
// programs, so a taproot output is credited like any other.
func TopUpAddress(ctx context.Context, cli Client) (string, error) {
	resp, err := cli.NewAddress(ctx, &lnrpc.NewAddressRequest{
		Type: lnrpc.AddressType_TAPROOT_PUBKEY,
	})
	if err != nil {
		return "", fmt.Errorf("asking LND for an address to top its own reserve up "+
			"into: %w", err)
	}
	if resp.GetAddress() == "" {
		return "", fmt.Errorf("LND returned an empty address for the reserve top-up")
	}
	return resp.GetAddress(), nil
}

// ReserveTopUp turns the anchor-reserve pre-flight's finding into a top-up
// output, or into nothing when the node already clears the reserve.
//
// It aims at the larger of the two figures internal/reserve reports, and that is
// the load-bearing choice rather than an economy. The smaller one,
// ShortfallAtVerify, is only what the batch's FIRST psbt_verify demands: an
// earlier channel's CompleteReservation can put it in the channel database before
// a later channel's verify, so the nth verify can be judged against existing + n,
// which is ShortfallAfterBatch's figure. Measured at n = 3 by
// TestALaterVerifyCountsAnEarlierChannelInTheBatch, where the figure moved from
// 10,000 to 20,000 sat mid-batch. Aiming smaller would leave a refused verify
// inside clock A with every earlier peer's window already open.
//
// It is also what the node needs once all n are pending, below which LND declines
// further on-chain spends and public channel opens. One output covers both.
func ReserveTopUp(ctx context.Context, cli Client, f reserve.Finding) (*TopUp, error) {
	// An all-private batch is never judged: enforceNewReservedValue returns
	// before it counts anything for an unannounced channel, and a private channel
	// is not counted when the reserve is worked out either. Topping the node up
	// anyway would be paying a fee to satisfy a check that does not run.
	if f.Verdict() == reserve.NotApplicable {
		return nil, nil
	}
	shortfall := f.ShortfallAtVerify()
	if after := f.ShortfallAfterBatch(); after > shortfall {
		shortfall = after
	}
	if shortfall == 0 {
		return nil, nil
	}
	addr, err := TopUpAddress(ctx, cli)
	if err != nil {
		return nil, err
	}
	return &TopUp{Address: addr, AmountSat: shortfall, Shortfall: shortfall}, nil
}
