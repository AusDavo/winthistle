// Package methods is the registry of LND RPCs this build calls.
//
// It exists so the baked macaroon cannot drift from the code. CLAUDE.md forbids
// hardcoding the permission list in documentation, and the design doc makes the
// stronger promise: the list the tool prints is authoritative. That only holds
// if there is exactly one place the list lives, and if adding a call site
// without touching that place fails.
//
// Three things keep it honest, in increasing order of how much they can be
// trusted:
//
//  1. `winthistle print-macaroon-command` renders the bake command from this
//     registry, so the credential is a function of the code.
//  2. Guard, a gRPC client interceptor installed by lnd.Dial, refuses any call
//     to a method not listed here. A call site that bypassed the registry fails
//     on its first RPC rather than working with a wide macaroon and breaking
//     with a narrow one.
//  3. TestEveryLNDCallSiteIsRegistered type-checks the whole module, finds every
//     call on an lnrpc/walletrpc client interface, and requires that the
//     registry list exactly those methods — no missing entries, and no spare
//     ones either, since a spare entry means the baked macaroon is wider than
//     the code needs. That test runs in `make check` and needs no harness.
//
// Only (3) catches a new call site before it ships, which is why it is the one
// wired into `make check`.
package methods

import (
	"fmt"
	"sort"
	"strings"
)

// Use says who calls a method, and therefore whether it belongs in the
// credential the operator bakes.
//
// The distinction is checked, not merely declared: the call-site test requires
// every InApp method to have a call site outside the tests, and every InHarness
// method to have none.
type Use string

const (
	// InApp: the app itself calls this in production. It goes into the
	// baked macaroon.
	InApp Use = "app"

	// InHarness: only the regtest fixtures call it. The harness uses alice's
	// admin.macaroon, so these are deliberately left out of the baked
	// credential — a fixture's needs are not the operator's.
	InHarness Use = "harness"
)

// Op is one of LND's own coarse macaroon permissions.
type Op struct {
	Entity string
	Action string
}

func (o Op) String() string { return o.Entity + ":" + o.Action }

// Method is one LND RPC this build calls.
type Method struct {
	// Name is the full gRPC method path, exactly as LND's permission map
	// keys it: "/lnrpc.Lightning/AbandonChannel".
	Name string

	Use Use

	// Ops is what LND's own permission map demands for this method. We do
	// not bake these — see URI — but they are recorded because they are the
	// argument for not baking them: the coarse entity that would cover this
	// call also covers calls we must never be able to make.
	//
	// Read out of rpcserver.go's MainRPCServerPermissions and
	// walletkit_server.go's macPermissions at v0.19.3-beta. Not load-bearing:
	// nothing decides anything on these, they only explain the choice.
	Ops []Op

	// Why is the call site, in one line. It is the thing a reviewer checks
	// the entry against.
	Why string
}

// URI is the permission actually baked: the "uri" entity, whose action is a
// literal gRPC method path.
//
// LND validates it in BakeMacaroon against interceptorChain.Permissions(), the
// union of every registered method — so an unknown or misspelled path is
// refused at bake time rather than producing a credential that fails later.
// macaroons.PermissionEntityCustomURI is the entity name; "uri" is its value.
func (m Method) URI() Op { return Op{Entity: "uri", Action: m.Name} }

// Service returns the proto service name, e.g. "lnrpc.Lightning".
func (m Method) Service() string {
	trimmed := strings.TrimPrefix(m.Name, "/")
	svc, _, _ := strings.Cut(trimmed, "/")
	return svc
}

// Short returns just the method name, e.g. "AbandonChannel".
func (m Method) Short() string {
	i := strings.LastIndex(m.Name, "/")
	return m.Name[i+1:]
}

// Capability is one thing the credential must never be able to do, and the LND
// method that would let it.
type Capability struct {
	Method string
	Does   string
}

// forbidden is the never-list. The design promises the operator that the baked
// credential cannot do these things, and this is what makes that a property of
// the code rather than a sentence in a document: validate refuses to register any
// of them, Explain derives its "cannot" claim from this list rather than
// restating it, and the guard names the list when it refuses a call.
//
// PublishTransaction is deliberately absent, and now that the funding flow
// exists that choice is load-bearing rather than pending. The promise the
// never-list makes is about spending and signing, and broadcasting is neither.
// WalletKit.PublishTransaction deserializes the bytes it is given, hands them to
// the wallet, and returns — walletkit_server.go, v0.19.3-beta; there is no
// signing step in it. So a credential holding it can relay a transaction that is
// already fully signed and nothing else, and producing a fully signed transaction
// needs a signature this credential cannot obtain: SendCoins, SendMany,
// SendOutputs and SignPsbt are all below, and FundPsbt with them.
//
// I-1 is the reason it has to be here at all. no_publish leaves the single
// publish to the app, and WalletKit is the route that also puts the transaction
// in LND's wallet-level rebroadcaster.
var forbidden = []Capability{
	{"/lnrpc.Lightning/SendCoins", "send coins on-chain"},
	{"/lnrpc.Lightning/SendMany", "send coins on-chain"},
	{"/lnrpc.Lightning/SendPaymentSync", "send a payment"},
	{"/lnrpc.Lightning/SendToRouteSync", "send a payment"},
	{"/lnrpc.Lightning/CloseChannel", "close a channel"},
	{"/lnrpc.Lightning/SignMessage", "sign a message"},
	{"/lnrpc.Lightning/BakeMacaroon", "bake itself a wider credential"},
	{"/walletrpc.WalletKit/SendOutputs", "send coins on-chain"},
	{"/walletrpc.WalletKit/SignPsbt", "sign a transaction"},
	{"/walletrpc.WalletKit/FundPsbt", "spend the node's own coins"},
}

// Forbidden returns the never-list.
func Forbidden() []Capability {
	out := make([]Capability, len(forbidden))
	copy(out, forbidden)
	return out
}

// isForbidden reports whether a method is on the never-list.
func isForbidden(name string) bool {
	for _, c := range forbidden {
		if c.Method == name {
			return true
		}
	}
	return false
}

// registry is the single source. Adding a call site without adding an entry here
// fails TestEveryLNDCallSiteIsRegistered; leaving an entry here with no call
// site fails it too.
//
// Sorted by name, so the printed command is stable and a diff to it is readable.
var registry = []Method{
	{
		Name: "/lnrpc.Lightning/AbandonChannel",
		Use:  InApp,
		Ops:  []Op{{"offchain", "write"}},
		Why:  "abort.AbandonPending: remove a pending channel from LND.",
	},
	{
		Name: "/lnrpc.Lightning/ExportAllChannelBackups",
		Use:  InApp,
		Ops:  []Op{{"offchain", "read"}},
		Why: "arm.Finalize: step 8, taken while every channel is pending and before " +
			"anything is broadcast. It works on pending channels because " +
			"chanbackup.FetchStaticChanBackups reads ChannelStateDB.FetchAllChannels, " +
			"which includes pending opens.",
	},
	{
		Name: "/lnrpc.Lightning/FundingStateStep",
		Use:  InApp,
		Ops:  []Op{{"onchain", "write"}, {"offchain", "write"}},
		Why: "abort.CancelShim (shim_cancel); arm.Verify (psbt_verify) and arm.Finalize " +
			"(psbt_finalize).",
	},
	{
		Name: "/lnrpc.Lightning/GetInfo",
		Use:  InApp,
		Ops:  []Op{{"info", "read"}},
		Why:  "lnd.Dial's probe: proves address, certificate and macaroon work together.",
	},
	{
		Name: "/lnrpc.Lightning/ListChannels",
		Use:  InHarness,
		Ops:  []Op{{"offchain", "read"}},
		Why:  "regtestenv.HasOpenChannel, waiting for a fixture channel to confirm.",
	},
	{
		Name: "/lnrpc.Lightning/ListPeers",
		Use:  InHarness,
		Ops:  []Op{{"peers", "read"}},
		Why:  "regtestenv.Peers, choosing fixture peers. The app takes its peers from the plan.",
	},
	{
		Name: "/lnrpc.Lightning/NewAddress",
		Use:  InApp,
		Ops:  []Op{{"address", "write"}},
		Why: "plan.TopUpAddress: a fresh address of the node's own wallet for the reserve " +
			"top-up output. What makes the verifier's \"the top-up pays an address we " +
			"control\" a fact rather than a hope is that the node minted it.",
	},
	{
		Name: "/lnrpc.Lightning/OpenChannel",
		Use:  InApp,
		Ops:  []Op{{"onchain", "write"}, {"offchain", "write"}},
		Why: "arm.Open: step 2 of the sequence, one PSBT-shim stream per channel with " +
			"no_publish set on every one of them. It was InHarness until the funding " +
			"flow existed, because a credential that could open a channel for a build " +
			"that could not finish one is a permission granted for nothing.",
	},
	{
		Name: "/lnrpc.Lightning/OpenChannelSync",
		Use:  InHarness,
		Ops:  []Op{{"onchain", "write"}, {"offchain", "write"}},
		Why: "regtestenv.OpenAndConfirmPlainChannel: a plain, broadcast-immediately open, " +
			"which is the opposite of what the app does and exists only as the channel " +
			"the abort path must refuse to touch.",
	},
	{
		Name: "/lnrpc.Lightning/PendingChannels",
		Use:  InApp,
		Ops:  []Op{{"offchain", "read"}},
		Why: "abort.AbandonPending's own pending check — the protection it re-establishes " +
			"before falling back to i_know_what_i_am_doing.",
	},
	{
		Name: "/walletrpc.WalletKit/LeaseOutput",
		Use:  InHarness,
		Ops:  []Op{{"onchain", "write"}},
		Why:  "regtestenv.LeaseAllUnspent, which reproduces the reserved-value refusal on purpose.",
	},
	{
		Name: "/walletrpc.WalletKit/ListLeases",
		Use:  InApp,
		Ops:  []Op{{"onchain", "read"}},
		Why: "reserve.Check: the difference between an empty hot wallet and a full one " +
			"whose coins are all leased. LND's own error names neither.",
	},
	{
		Name: "/walletrpc.WalletKit/ListUnspent",
		Use:  InApp,
		Ops:  []Op{{"onchain", "read"}},
		Why: "reserve.Check: the exact balance CheckReservedValue judges — unlocked, " +
			"default account, witness outputs, zero confirmations.",
	},
	{
		Name: "/walletrpc.WalletKit/PublishTransaction",
		Use:  InApp,
		Ops:  []Op{{"onchain", "write"}},
		Why: "arm.Publish: step 9, the single publish call, gated on n of n " +
			"chan_pending. Deliberately not on the never-list — see forbidden.",
	},
	{
		Name: "/walletrpc.WalletKit/ReleaseOutput",
		Use:  InHarness,
		Ops:  []Op{{"onchain", "write"}},
		Why:  "regtestenv.LeaseAllUnspent's cleanup.",
	},
	{
		Name: "/walletrpc.WalletKit/RequiredReserve",
		Use:  InApp,
		Ops:  []Op{{"onchain", "read"}},
		Why: "reserve.Check: ask LND for the figure rather than counting anchor channels " +
			"ourselves, which is CurrentNumAnchorChans's job and not reproducible from outside.",
	},
}

// All returns every registered method, sorted by name.
func All() []Method {
	out := make([]Method, len(registry))
	copy(out, registry)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// App returns the methods that go into the baked macaroon.
func App() []Method {
	var out []Method
	for _, m := range All() {
		if m.Use == InApp {
			out = append(out, m)
		}
	}
	return out
}

// Lookup finds a method by its full gRPC path.
func Lookup(name string) (Method, bool) {
	for _, m := range registry {
		if m.Name == name {
			return m, true
		}
	}
	return Method{}, false
}

// validate reports the registry's own inconsistencies. Called from init so a
// malformed entry cannot reach a running binary — the registry is small enough
// that the cost is nothing, and a typo in a method path would otherwise surface
// as a bake-time rejection from LND with no clue which entry caused it.
func validate() error {
	seen := make(map[string]bool, len(registry))
	for _, m := range registry {
		switch {
		case !strings.HasPrefix(m.Name, "/"):
			return fmt.Errorf("method %q must start with '/'", m.Name)
		case strings.Count(m.Name, "/") != 2:
			return fmt.Errorf("method %q is not /package.Service/MethodName", m.Name)
		case !strings.Contains(m.Service(), "."):
			return fmt.Errorf("method %q has no package in its service name", m.Name)
		case m.Short() == "":
			return fmt.Errorf("method %q has no method name", m.Name)
		case m.Use != InApp && m.Use != InHarness:
			return fmt.Errorf("method %s has use %q", m.Name, m.Use)
		case len(m.Ops) == 0:
			return fmt.Errorf("method %s records no LND permission", m.Name)
		case m.Why == "":
			return fmt.Errorf("method %s says nothing about where it is called", m.Name)
		case seen[m.Name]:
			return fmt.Errorf("method %s is listed twice", m.Name)
		case isForbidden(m.Name):
			return fmt.Errorf("method %s is on the never-list: the credential "+
				"this tool bakes must not be able to %s. If that promise has "+
				"genuinely changed, change it in forbidden and in the design "+
				"doc, deliberately, rather than around it",
				m.Name, capabilityOf(m.Name))
		}
		seen[m.Name] = true
	}
	return nil
}

// capabilityOf names what a forbidden method would let the credential do.
func capabilityOf(name string) string {
	for _, c := range forbidden {
		if c.Method == name {
			return c.Does
		}
	}
	return "do something it must not"
}

func init() {
	if err := validate(); err != nil {
		panic("methods: " + err.Error())
	}
}
