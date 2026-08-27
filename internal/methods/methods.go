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
//
// # Counting, for the one method where the count is the point
//
// (3) asks whether a method is called, not how often, and for nineteen of the
// twenty entries here that is the right question. For
// WalletKit.PublishTransaction it is not: the design's claim is that the
// network is reached on a known, small number of lines, each of them typed so
// it can only carry one kind of transaction. That is a claim about a count, and
// a count is checked by counting. Method.CallSites is where the number lives
// and the call-site test is what enforces it.
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
	// walletkit_server.go's macPermissions at v0.21.2-beta. Not load-bearing:
	// nothing decides anything on these, they only explain the choice.
	Ops []Op

	// Why is the call site, in one line. It is the thing a reviewer checks
	// the entry against.
	Why string

	// CallSites is the exact number of production call sites this method is
	// allowed to have, when the count itself is the property being protected.
	// Zero means unconstrained, which is the right default: most methods are
	// called from wherever they are needed, and a test that failed when a
	// function was split in two would be a refactoring tripwire rather than a
	// safety check.
	//
	// It is not the default for PublishTransaction. "The transaction reaches
	// the network on exactly these lines" is a claim about how many lines there
	// are, and until this field existed it was made in five prose comments and
	// checked nowhere — a second caller passed TestEveryLNDCallSiteIsRegistered
	// silently, because that test groups by method and asserts non-empty in both
	// directions without ever counting. The count is 1 now, and the field matters
	// more rather than less for that: with two lines there was a type system
	// keeping them apart as well, and with one there is only the number.
	CallSites int
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

	// Ops is what LND's own permission map demands for this method, read out of
	// MainRPCServerPermissions and walletkit_server.go's macPermissions at
	// v0.21.2-beta.
	//
	// Unlike Method.Ops these are load-bearing: `winthistle doctor` asks LND
	// whether the configured credential holds them, through
	// CheckMacaroonPermissions, and reports a credential that can do any of
	// these as a failure. That is the never-list checked against the actual
	// file rather than against this build's intentions — an operator running
	// with admin.macaroon has every one of them, and nothing else in the tool
	// would have said so.
	Ops []Op
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
// the wallet, and returns — walletkit_server.go, v0.21.2-beta; there is no
// signing step in it. So a credential holding it can relay a transaction that is
// already fully signed and nothing else, and producing a fully signed transaction
// needs a signature this credential cannot obtain: SendCoins, SendMany,
// SendOutputs and SignPsbt are all below, and FundPsbt with them.
//
// I-1 is the reason it has to be here at all. no_publish leaves the single
// publish to the app, and WalletKit is the route that also puts the transaction
// in LND's wallet-level rebroadcaster.
//
// WalletKit.SubmitPackage, new at v0.21.2-beta, is absent for the same reason
// PublishTransaction is: it broadcasts, and broadcasting is not what this list
// promises about. It is also not registered, so the guard refuses it and the
// baked credential never carries it — which is the only reason it needs no entry
// of its own. If a call site is ever added for it, that is a decision about I-4
// and the CPFP child, not a registry edit.
var forbidden = []Capability{
	{"/lnrpc.Lightning/SendCoins", "send coins on-chain",
		[]Op{{"onchain", "write"}}},
	{"/lnrpc.Lightning/SendMany", "send coins on-chain",
		[]Op{{"onchain", "write"}}},
	// These two were /lnrpc.Lightning/SendPaymentSync and SendToRouteSync until
	// the v0.21.2-beta bump, which removed both from lightning.proto. A
	// never-list entry naming a method LND no longer serves promises nothing, so
	// they are restated at the RPCs that actually carry a payment now.
	{"/routerrpc.Router/SendPaymentV2", "send a payment",
		[]Op{{"offchain", "write"}}},
	{"/routerrpc.Router/SendToRouteV2", "send a payment",
		[]Op{{"offchain", "write"}}},
	{"/lnrpc.Lightning/CloseChannel", "close a channel",
		[]Op{{"onchain", "write"}, {"offchain", "write"}}},
	{"/lnrpc.Lightning/SignMessage", "sign a message",
		[]Op{{"message", "write"}}},
	{"/lnrpc.Lightning/BakeMacaroon", "bake itself a wider credential",
		[]Op{{"macaroon", "generate"}}},
	{"/walletrpc.WalletKit/SendOutputs", "send coins on-chain",
		[]Op{{"onchain", "write"}}},
	{"/walletrpc.WalletKit/SignPsbt", "sign a transaction",
		[]Op{{"onchain", "write"}}},
	{"/walletrpc.WalletKit/FundPsbt", "spend the node's own coins",
		[]Op{{"onchain", "write"}}},
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
		Name: "/lnrpc.Lightning/CheckMacaroonPermissions",
		Use:  InApp,
		Ops:  []Op{{"macaroon", "read"}},
		Why: "doctor.Run: ask LND whether the configured credential authorises each " +
			"method this build calls, and whether it authorises anything on the " +
			"never-list. It answers about the macaroon in the request rather than " +
			"about the caller's, and it invokes no handler — CheckMacAuth checks " +
			"the ops and then the uri:<full_method> form, and returns. The " +
			"alternative was calling the methods themselves with junk arguments, " +
			"which for AbandonChannel or PublishTransaction is not a diagnostic.",
	},
	{
		Name: "/lnrpc.Lightning/ConnectPeer",
		Use:  InApp,
		Ops:  []Op{{"peers", "write"}},
		Why: "peers.Check: Phase 0's peer pre-flight connects to each peer before " +
			"anything else is asked of it. perm is false — a permanent connection " +
			"is a lasting change to the node made by a pre-flight, and Phase 0 has " +
			"to leave nothing behind.",
	},
	{
		Name: "/lnrpc.Lightning/ExportAllChannelBackups",
		Use:  InApp,
		Ops:  []Op{{"offchain", "read"}},
		Why: "arm.Receipts: taken while every channel is pending, before anything is " +
			"signed and before anything is broadcast. It works on pending channels " +
			"because chanbackup.FetchStaticChanBackups reads " +
			"ChannelStateDB.FetchAllChannels, which includes pending opens.",
	},
	{
		Name: "/lnrpc.Lightning/FundingStateStep",
		Use:  InApp,
		Ops:  []Op{{"onchain", "write"}, {"offchain", "write"}},
		Why: "abort.CancelShim (shim_cancel); arm.Verify (psbt_verify with " +
			"skip_finalize, which completes the funding flow rather than pausing it, " +
			"so there is no psbt_finalize call anywhere in this build).",
	},
	{
		Name: "/lnrpc.Lightning/GetInfo",
		Use:  InApp,
		Ops:  []Op{{"info", "read"}},
		Why:  "lnd.Dial's probe: proves address, certificate and macaroon work together.",
	},
	{
		Name: "/lnrpc.Lightning/GetNodeInfo",
		Use:  InApp,
		Ops:  []Op{{"info", "read"}},
		Why: "peers.Check: the peer's alias, addresses and existing channel " +
			"capacities, out of LND's own local gossip graph. Never a Lightning " +
			"explorer — CLAUDE.md forbids one, and the graph is already on the node.",
	},
	{
		Name: "/lnrpc.Lightning/ListChannels",
		Use:  InApp,
		Ops:  []Op{{"offchain", "read"}},
		Why: "settle.Tick: whether each member of a published batch is open yet, " +
			"and whether its peer is online. Also regtestenv.HasOpenChannel. It was " +
			"InHarness until Phase 2 existed, because a channel leaving " +
			"pending_open_channels is the authoritative signal that the peer " +
			"considers it confirmed to its own minimum_depth.",
	},
	{
		Name: "/lnrpc.Lightning/ListPeers",
		Use:  InApp,
		Ops:  []Op{{"peers", "read"}},
		Why: "peers.Check: which peers LND is already connected to, asked once for " +
			"the whole batch and asked before connecting, so \"already connected\" " +
			"is a fact about the node as the operator found it. Also " +
			"regtestenv.Peers, choosing fixture peers.",
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
			"before falling back to i_know_what_i_am_doing. And peers.Check, which " +
			"reads it for the opposite reason: a channel already pending with a " +
			"batch peer spends the pending-channel slot step 2 needs, and a peer at " +
			"LND's default of one has none left. Free to ask, and the alternative " +
			"is learning it from accept_channel with the cold wallet out. Also " +
			"settle.Tick, for two figures on the same message: funding_expiry_blocks, " +
			"which is the funding horizon, and confirmations_until_active, which is " +
			"the peer's own minimum_depth while the funding transaction is " +
			"unconfirmed.",
	},
	{
		Name: "/lnrpc.Lightning/UpdateChannelPolicy",
		Use:  InApp,
		Ops:  []Op{{"offchain", "write"}},
		Why: "settle.ApplyPolicy: Phase 2 closes the fee-policy race, applying the " +
			"per-peer policy chosen in Phase 0 the moment each channel goes open. " +
			"Its failures arrive inside a successful response — failed_updates — " +
			"rather than as an error, which is why that call has a function of its " +
			"own.",
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
		Name:      "/walletrpc.WalletKit/PublishTransaction",
		Use:       InApp,
		Ops:       []Op{{"onchain", "write"}},
		CallSites: 1,
		Why: "arm.Publish: step 8, the funding transaction's single publish, gated " +
			"on n of n chan_pending. One call site and no more — CallSites is what " +
			"enforces that. It was two until the CPFP child went, and the child was " +
			"kept apart from this one by the type system rather than by care: " +
			"arm.Publish takes an *arm.Armed, which carries the chan_pending " +
			"receipts in an unexported map only arm.Receipts fills, and it " +
			"re-derives the txid from the bytes it is handed and refuses any that " +
			"do not hash to the one LND pinned. With one line left there is nothing " +
			"to keep apart, and the count is now the whole claim. Deliberately not " +
			"on the never-list — see forbidden.",
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
		case m.CallSites < 0:
			return fmt.Errorf("method %s declares %d call sites", m.Name, m.CallSites)
		case m.CallSites > 0 && m.Use != InApp:
			return fmt.Errorf("method %s is %s but pins its production call-site "+
				"count at %d. An InHarness method is required to have none, so the "+
				"two rules would contradict each other", m.Name, m.Use, m.CallSites)
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
