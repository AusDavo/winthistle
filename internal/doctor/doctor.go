// Package doctor is the pre-flight the operator runs before anything else.
//
// Every check in here already existed somewhere — the method registry, the
// anchor-reserve pre-flight, the coldwallet pre-flight, the segwit coin filter,
// the fee source, the journal. What was missing is the one place that runs them
// in order and, for each failure, prints the command that fixes it rather than
// a paragraph describing the fix. docs/design.html is specific about that: "a
// written guide is necessary and will rot, so the guide is short and the tool
// carries the checks".
//
// # How the credential is checked, and why not by calling things
//
// The obvious way to find out whether a macaroon authorises a method is to call
// the method. For most of this build's list that is a bad idea: the macaroon
// interceptor runs before the handler, so a *refused* method costs nothing, but
// an *allowed* one runs — and AbandonChannel, FundingStateStep, OpenChannel and
// PublishTransaction are not things to invoke with junk arguments to see what
// happens.
//
// lnrpc.CheckMacaroonPermissions answers the question directly. It takes the
// macaroon to examine in the request rather than using the caller's, and
// macaroons.Service.CheckMacAuth then runs exactly the check the interceptor
// would: the method's coarse ops first, then the uri:<full_method> form that
// this tool's baked credential actually carries. No handler is reached.
//
// It answers about the never-list too, which is the half nothing else could
// check. The registry promises the operator that the baked credential cannot
// send coins, close a channel, sign a message or widen itself; that promise is
// about the credential in the config file, not about this build's intentions,
// and an operator pointing the tool at admin.macaroon has broken every part of
// it. Asking LND whether the credential holds onchain:write is how that becomes
// a line in a report.
//
// # Two refusals that look alike
//
// Both arrive with bakery's plain "permission denied" text, and they mean
// opposite things:
//
//   - codes.InvalidArgument from CheckMacaroonPermissions is the answer: the
//     credential being examined does not authorise that method.
//   - codes.Unknown with the same text is the interceptor refusing *this* call.
//     bakery.ErrPermissionDenied is an errgo error with no gRPC status attached,
//     so it arrives untyped. It means the configured credential is too narrow to
//     run the check at all, which is itself the diagnosis: it was baked before
//     this build existed.
//
// Matching on the code alone would confuse them, and matching on the text alone
// would too. Both are matched.
package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/AusDavo/winthistle/internal/arm"
	"github.com/AusDavo/winthistle/internal/bitcoind"
	"github.com/AusDavo/winthistle/internal/coldwallet"
	"github.com/AusDavo/winthistle/internal/config"
	"github.com/AusDavo/winthistle/internal/fees"
	"github.com/AusDavo/winthistle/internal/journal"
	"github.com/AusDavo/winthistle/internal/lnd"
	"github.com/AusDavo/winthistle/internal/methods"
	"github.com/AusDavo/winthistle/internal/peers"
	"github.com/AusDavo/winthistle/internal/prose"
	"github.com/AusDavo/winthistle/internal/reserve"
	"github.com/lightningnetwork/lnd/lnrpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Status is what one check decided.
type Status int

const (
	// OK: nothing to do.
	OK Status = iota

	// Warn: usable, and the operator should know. A warning never stops a run;
	// if something should stop a run it is a Fail.
	Warn

	// Fail: this has to be fixed before a batch can be opened.
	Fail

	// Skip: the check could not be made, usually because an earlier one failed.
	// Deliberately not OK — a check that did not run has not passed.
	Skip
)

func (s Status) String() string {
	switch s {
	case OK:
		return "ok"
	case Warn:
		return "warn"
	case Fail:
		return "FAIL"
	default:
		return "skip"
	}
}

// Check is one thing that was looked at.
type Check struct {
	Name   string
	Status Status

	// Lines are what was found, one fact per line.
	Lines []string

	// Fix is the command that resolves it, verbatim and runnable. Prose about
	// what to do belongs in Lines; this is the thing to paste.
	Fix []string
}

func (c *Check) say(format string, args ...any) {
	c.Lines = append(c.Lines, fmt.Sprintf(format, args...))
}

func (c *Check) fix(format string, args ...any) {
	c.Fix = append(c.Fix, fmt.Sprintf(format, args...))
}

// fail records a failure and its reason in one call, so the two cannot drift.
func (c *Check) fail(format string, args ...any) {
	c.Status = Fail
	c.say(format, args...)
}

func (c *Check) warn(format string, args ...any) {
	if c.Status != Fail {
		c.Status = Warn
	}
	c.say(format, args...)
}

// Report is every check, in the order they were made.
type Report struct {
	Checks []Check
}

// OK reports whether a batch could be opened on this setup.
func (r *Report) OK() bool {
	for _, c := range r.Checks {
		if c.Status == Fail {
			return false
		}
	}
	return true
}

func (r *Report) add(c Check) *Check {
	r.Checks = append(r.Checks, c)
	return &r.Checks[len(r.Checks)-1]
}

// Options tunes what doctor can check.
type Options struct {
	// Batch, when given, lets the peer and reserve checks be about the batch the
	// operator actually means to open rather than about a hypothetical one.
	Batch *config.Batch

	// Connect allows the peer check to connect to peers that are not connected.
	// Off by default: doctor is a diagnostic and connecting is a change to the
	// node, however small.
	Connect bool
}

// Run makes every check, in order, and never stops at the first failure.
//
// A doctor that reported one problem per run would turn an evening's setup into
// several. Where a failure makes a later check impossible the later one is
// Skip, which is not OK.
func Run(ctx context.Context, cfg *config.Config, opts Options) *Report {
	r := &Report{}
	checkConfig(r, cfg)

	cli := checkLND(ctx, r, cfg)
	if cli != nil {
		defer cli.Close()
	}
	checkMacaroon(ctx, r, cfg, cli)

	// One journal handle for the whole report. The cold-wallet check needs it —
	// the answer to the round-trip address check is the only thing that
	// distinguishes a correct descriptor from a plausible wrong one, and it is
	// written down there — and opening it twice in one run would report the same
	// failure in two places.
	j, journalErr := openJournal(ctx, cfg)
	if j != nil {
		defer j.Close()
	}

	node, wallet := checkCore(ctx, r, cfg)
	checkColdWallet(ctx, r, cfg, node, wallet, j)
	checkCoins(ctx, r, cfg, wallet)
	checkReserve(ctx, r, cli, opts)
	checkFees(ctx, r, cfg, node)
	checkPeers(ctx, r, cli, opts)
	checkJournal(ctx, r, cfg, wallet, j, journalErr)
	return r
}

// openJournal makes the directory and opens the file, which is the one thing
// doctor creates: a journal that does not exist yet is not a fault.
func openJournal(ctx context.Context, cfg *config.Config) (*journal.Journal, error) {
	if dir := filepath.Dir(cfg.Server.Journal); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("making %s: %w", dir, err)
		}
	}
	j, err := journal.Open(ctx, cfg.Server.Journal)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", cfg.Server.Journal, err)
	}
	return j, nil
}

func checkConfig(r *Report, cfg *config.Config) {
	c := r.add(Check{Name: "winthistle.toml"})
	// Its own line, at an indent: a path is read character by character and a
	// long one wrapped mid-word is unreadable. Same rule as txids and addresses
	// everywhere else in this tool's copy.
	c.say("read:")
	c.say("    %s", cfg.Path)
	c.say("lnd %s, core %s, wallet %q", cfg.LND.Address, cfg.Bitcoind.Address,
		cfg.Bitcoind.Wallet)
	c.say("abort gate %s of the peers' 10m", cfg.Limits.AbortAfterSigning)
	if !cfg.Loopback() {
		c.warn("[server] bind is %s, which is not loopback. A bind is not an "+
			"authentication boundary either way — the token and the Origin checks "+
			"are — but this one is reachable from off the machine.", cfg.Server.Bind)
	}
	if len(cfg.Signers) == 0 {
		c.fail("no [[signer]] blocks, so there is nothing to sign with. There must " +
			"be exactly as many as the descriptor requires: btcd's finalizer wants " +
			"exactly m signatures, so a 2-of-3 carrying three partials does not " +
			"finalize at all.")
		c.fix("# add to %s:\n[[signer]]\nlabel = \"cold1\"", cfg.Path)
	} else {
		c.say("signers: %s", strings.Join(labels(cfg.Signers), ", "))
	}
}

func labels(sigs []config.Signer) []string {
	out := make([]string, 0, len(sigs))
	for _, s := range sigs {
		if s.Command == "" {
			out = append(out, s.Label+" (by file)")
			continue
		}
		out = append(out, s.Label)
	}
	return out
}

func checkLND(ctx context.Context, r *Report, cfg *config.Config) *lnd.Client {
	c := r.add(Check{Name: "LND"})

	for _, f := range []struct{ what, path string }{
		{"tls_cert", cfg.LND.TLSCert},
		{"macaroon", cfg.LND.Macaroon},
	} {
		if _, err := os.Stat(f.path); err != nil {
			c.fail("%s: %v", f.what, err)
		}
	}
	if c.Status == Fail {
		c.fix("winthistle print-macaroon-command --save-to %s", cfg.LND.Macaroon)
		return nil
	}

	dialCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	cli, err := lnd.Dial(dialCtx, cfg.LND)
	if err != nil {
		c.fail("cannot reach %s: %v", cfg.LND.Address, err)
		if tooNarrow(err) {
			c.say("That is the credential being refused rather than the node being " +
				"down: lnd.Dial's probe calls GetInfo, and this macaroon does not " +
				"carry it. Re-bake it from this build.")
			c.fix("winthistle print-macaroon-command --save-to %s | sh", cfg.LND.Macaroon)
			return nil
		}
		c.fix("# is it listening?\nss -ltnp | grep %s", port(cfg.LND.Address))
		return nil
	}

	info, err := cli.Lightning.GetInfo(ctx, &lnrpc.GetInfoRequest{})
	if err != nil {
		c.fail("GetInfo: %v", err)
		return cli
	}
	c.say("%s, lnd %s, %s", chainOf(info), info.GetVersion(), info.GetIdentityPubkey())
	c.say("%d peer(s), %d active channel(s), block %d", info.GetNumPeers(),
		info.GetNumActiveChannels(), info.GetBlockHeight())
	if !info.GetSyncedToChain() {
		c.fail("the node is not synced to chain. LND refuses to open a channel " +
			"while its wallet is behind — \"channels cannot be created before the " +
			"wallet is fully synced\" — and one block is enough to trigger it.")
	}
	if !info.GetSyncedToGraph() {
		c.warn("the node is not synced to the graph, so the peer pre-flight's " +
			"facts are whatever has arrived so far.")
	}
	return cli
}

func chainOf(info *lnrpc.GetInfoResponse) string {
	for _, ch := range info.GetChains() {
		return ch.GetChain() + " " + ch.GetNetwork()
	}
	return "unknown chain"
}

// checkMacaroon asks LND what the configured credential can do — both halves.
func checkMacaroon(ctx context.Context, r *Report, cfg *config.Config, cli *lnd.Client) {
	c := r.add(Check{Name: "the macaroon"})
	if cli == nil {
		c.Status = Skip
		c.say("not checked: LND could not be reached")
		return
	}
	mac, err := os.ReadFile(cfg.LND.Macaroon)
	if err != nil {
		c.fail("reading %s: %v", cfg.LND.Macaroon, err)
		return
	}

	var missing []string
	for _, m := range methods.App() {
		ok, err := authorises(ctx, cli, mac, m.Name, m.Ops)
		switch {
		case errors.Is(err, errProbeRefused):
			c.fail("this credential cannot even run the permission check, which "+
				"means it was baked before this build: %v", err)
			c.fix("winthistle print-macaroon-command --save-to %s | sh", cfg.LND.Macaroon)
			return
		case err != nil:
			c.fail("asking LND about %s: %v", m.Short(), err)
			return
		case !ok:
			missing = append(missing, m.Name)
		}
	}

	if len(missing) > 0 {
		c.fail("%d of the %d methods this build calls %s not authorised:",
			len(missing), len(methods.App()), prose.IsAre(len(missing)))
		for _, m := range missing {
			c.say("    %s", m)
		}
		c.say("The permission list is a property of the code, so it is generated " +
			"rather than written down. Re-bake and the list comes out right by " +
			"construction.")
		c.fix("winthistle print-macaroon-command --save-to %s | sh", cfg.LND.Macaroon)
	} else {
		c.say("authorises all %d methods this build calls", len(methods.App()))
	}

	// The other half of the promise, and the half nothing else can check: what
	// this credential must not be able to do.
	var can []methods.Capability
	for _, f := range methods.Forbidden() {
		ok, err := authorises(ctx, cli, mac, f.Method, f.Ops)
		if err != nil {
			c.warn("could not check %s: %v", f.Method, err)
			continue
		}
		if ok {
			can = append(can, f)
		}
	}
	if len(can) > 0 {
		c.fail("this credential can also do %d thing%s the tool promises it cannot:",
			len(can), prose.Plural(len(can)))
		for _, f := range can {
			c.say("    %s — %s", f.Does, f.Method)
		}
		c.say("That is what admin.macaroon looks like from here. The baked " +
			"credential holds one uri permission per method and no coarse entity " +
			"at all, so it cannot spend, sign, close or widen itself even if this " +
			"build were wrong about everything else.")
		c.fix("winthistle print-macaroon-command --save-to %s | sh", cfg.LND.Macaroon)
	} else {
		c.say("cannot spend, sign, close a channel or widen itself — checked "+
			"against all %d entries on the never-list", len(methods.Forbidden()))
	}
}

// errProbeRefused means our own credential lacks CheckMacaroonPermissions.
var errProbeRefused = errors.New("the configured credential does not carry " +
	"CheckMacaroonPermissions")

// authorises asks LND whether mac would be allowed to call method.
func authorises(ctx context.Context, cli *lnd.Client, mac []byte, method string,
	ops []methods.Op) (bool, error) {

	perms := make([]*lnrpc.MacaroonPermission, 0, len(ops))
	for _, op := range ops {
		perms = append(perms, &lnrpc.MacaroonPermission{
			Entity: op.Entity, Action: op.Action,
		})
	}
	_, err := cli.Lightning.CheckMacaroonPermissions(ctx, &lnrpc.CheckMacPermRequest{
		Macaroon:    mac,
		Permissions: perms,
		FullMethod:  method,
	})
	switch {
	case err == nil:
		return true, nil
	case status.Code(err) == codes.InvalidArgument:
		// The answer, not a failure: CheckMacaroonPermissions reports a macaroon
		// that does not authorise the method by returning InvalidArgument with
		// bakery's own text inside it.
		return false, nil
	case tooNarrow(err):
		return false, fmt.Errorf("%w: %v", errProbeRefused, err)
	default:
		return false, err
	}
}

// tooNarrow reports whether an error is LND's interceptor refusing *our* call
// over the macaroon.
//
// The text, not the code. bakery.ErrPermissionDenied is errgo.New("permission
// denied") with no gRPC status attached, so LND's interceptor passes it through
// as codes.Unknown; matching on codes.PermissionDenied would never fire. If LND
// ever starts typing it, the code check below starts carrying the weight and
// the text check becomes redundant rather than wrong.
func tooNarrow(err error) bool {
	if err == nil {
		return false
	}
	if status.Code(err) == codes.PermissionDenied {
		return true
	}
	return status.Code(err) != codes.InvalidArgument &&
		strings.Contains(err.Error(), "permission denied")
}

func checkCore(ctx context.Context, r *Report, cfg *config.Config) (node, wallet *bitcoind.Client) {
	c := r.add(Check{Name: "Bitcoin Core"})

	nodeCfg := cfg.Bitcoind
	nodeCfg.Wallet = ""
	node, err := bitcoind.New(nodeCfg)
	if err != nil {
		c.fail("%v", err)
		return nil, nil
	}
	wallet, err = bitcoind.New(cfg.Bitcoind)
	if err != nil {
		c.fail("%v", err)
		return node, nil
	}

	net, err := node.GetNetworkInfo(ctx)
	if err != nil {
		c.fail("cannot reach %s: %v", cfg.Bitcoind.Address, err)
		c.say("Directed mode builds the batch with Core — coin selection, change " +
			"derivation, the exact fee rate and the bip32 derivations the signers " +
			"need. There is no other builder in this build.")
		c.fix("# is it listening, and is the cookie readable?\nss -ltnp | grep %s\nls -l %s",
			port(cfg.Bitcoind.Address), cfg.Bitcoind.Cookie)
		return node, wallet
	}
	c.say("core %s (%d)", net.SubVersion, net.Version)
	if net.Version < coldwallet.MinCoreVersion {
		c.fail("core %d is below the %d this build expects", net.Version,
			coldwallet.MinCoreVersion)
	}

	chain, err := node.GetBlockchainInfo(ctx)
	if err != nil {
		c.fail("getblockchaininfo: %v", err)
		return node, wallet
	}
	c.say("%s, block %d of %d", chain.Chain, chain.Blocks, chain.Headers)
	if chain.InitialBlockDownload {
		c.fail("core is still in initial block download (%.2f%%). A rescan against "+
			"a chain that is still arriving finds whatever has arrived, silently.",
			chain.VerificationProgress*100)
	}
	if chain.Pruned {
		when, err := node.BlockTime(ctx, chain.PruneHeight)
		if err == nil {
			c.warn("pruned to block %d (%s). A descriptor import rescans from the "+
				"cold wallet's birthday, and the blocks before this one are gone — "+
				"if the wallet is older than that date, its history cannot be found "+
				"here.", chain.PruneHeight, when.Format("2006-01-02"))
		} else {
			c.warn("pruned to block %d", chain.PruneHeight)
		}
	}
	return node, wallet
}

func checkColdWallet(ctx context.Context, r *Report, cfg *config.Config,
	node, wallet *bitcoind.Client, j *journal.Journal) {

	c := r.add(Check{Name: "the cold wallet"})
	if wallet == nil {
		c.Status = Skip
		c.say("not checked: Core could not be reached")
		return
	}

	info, err := wallet.GetWalletInfo(ctx)
	if err != nil {
		c.fail("wallet %q: %v", cfg.Bitcoind.Wallet, err)
		c.say("Core does not auto-load non-default wallets, so this is what every " +
			"wallet call says after a bitcoind restart. It reads like data loss and " +
			"it is not.")
		c.fix("bitcoin-cli loadwallet %q", cfg.Bitcoind.Wallet)
		return
	}
	c.say("wallet %q is loaded", info.Name)

	if info.PrivateKeysEnabled {
		c.fail("this wallet has private keys. The watch-only wallet is structurally " +
			"incapable of signing, which is the property that makes it safe to point " +
			"a batch builder at it — this one is not that wallet.")
		c.fix("bitcoin-cli createwallet %q true true \"\" false true true",
			cfg.Bitcoind.Wallet+"-watch")
	}
	if !info.Descriptors {
		c.fail("this is a legacy wallet, not a descriptor wallet. The whole setup " +
			"path is importdescriptors.")
		c.fix("bitcoin-cli createwallet %q true true \"\" false true true",
			cfg.Bitcoind.Wallet+"-watch")
	}
	if info.Scanning.Running {
		c.warn("a rescan is running (%.1f%%). Until it finishes the balance and the "+
			"coin list are whatever has been scanned so far.", info.Scanning.Progress*100)
	}

	descs, err := wallet.ListDescriptors(ctx)
	if err != nil {
		c.fail("listdescriptors: %v", err)
		return
	}
	active := 0
	for _, d := range descs {
		if !d.Active {
			continue
		}
		active++
		branch := "receive"
		if d.Internal {
			branch = "change"
		}
		c.say("%s: %s", branch, describe(d))
	}
	switch active {
	case 0:
		c.fail("no active descriptors, so this wallet knows about no coins at all.")
		c.say("The descriptors and the birthday are the one part of setup no " +
			"program can supply: they come out of your own wallet software, and " +
			"nothing here can guess either. `winthistle setup` does the rest — " +
			"creates the wallet, checksums and imports the pair, reads back what " +
			"landed, and ends by asking you to compare addresses.")
		c.fix("winthistle example-descriptors > cold.toml\n" +
			"# fill in the two descriptors and the birthday, then:\n" +
			"winthistle setup --descriptors cold.toml")
		return
	case 2:
		c.say("two active descriptors, which is a receive branch and a change branch")
	default:
		c.warn("%d active descriptors. A cold wallet is normally two: receive and "+
			"change.", active)
	}
	if stale := len(descs) - active; stale > 0 {
		c.warn("%d descriptor%s in this wallet %s inactive, and their coins are "+
			"still in this wallet's coin list. Core keeps one active receive branch "+
			"and one active change branch, so an import replaces an earlier pair by "+
			"deactivating it — there is no RPC that removes a descriptor. A batch "+
			"built here can still spend those coins.",
			stale, prose.Plural(stale), prose.IsAre(stale))
	}
	checkAddressCheck(ctx, c, cfg, node, j, descs)
}

// checkAddressCheck reports whether anybody has ever compared this wallet's
// addresses against the wallet software that holds the keys.
//
// This is the only check on this screen whose evidence is a human rather than a
// node, and it is here because nothing a node can be asked separates a correct
// descriptor from a plausible wrong one. Core parses both, the import succeeds
// for both, the read-back is self-consistent for both, and the balance does not
// separate them either — a multi()-where-you-wanted-sortedmulti() wallet finds
// most of the harness cold wallet's balance, because sortedmulti sorts the
// derived keys and the two agree wherever they were already in order. A
// plausible partial balance is a better disguise than zero.
//
// The record is keyed on the descriptors it was about, so it invalidates itself:
// a confirmation from before a re-import describes descriptors this wallet no
// longer derives from, and saying so is more useful than either believing it or
// ignoring it.
func checkAddressCheck(ctx context.Context, c *Check, cfg *config.Config,
	node *bitcoind.Client, j *journal.Journal,
	descs []bitcoind.WalletDescriptor) {

	if j == nil {
		c.warn("the address check cannot be read: the journal is not open, and that " +
			"is where the answer to it lives.")
		return
	}
	var receive, change string
	for _, d := range descs {
		switch {
		case d.Active && !d.Internal:
			receive = d.Desc
		case d.Active && d.Internal:
			change = d.Desc
		}
	}

	prev, err := j.LatestSetup(ctx, cfg.Bitcoind.Wallet)
	if errors.Is(err, journal.ErrNoSetup) {
		c.warn("nobody has compared this wallet's addresses against your own wallet " +
			"software, and that comparison is the only thing that can tell a correct " +
			"descriptor from a plausible wrong one. It is a warning rather than a " +
			"failure because a wallet imported by hand before this command existed " +
			"is a working wallet — but it has not been checked.")
		c.fix("winthistle setup")
		return
	}
	if err != nil {
		c.warn("reading the address check: %v", err)
		return
	}

	when := prev.AnsweredAt.UTC().Format("2006-01-02")
	if !prev.Describes(receive, change) {
		c.warn("the addresses were answered about on %s, and the answer was %q — "+
			"but about a different descriptor pair from the one in this wallet now, "+
			"so it says nothing about the addresses a batch would be built against. "+
			"The record names the pair it was about, which is what stops it ageing "+
			"into a claim about this one.", when, prev.Outcome)
		c.fix("winthistle setup")
		return
	}
	if prev.Outcome == journal.SetupRejected {
		c.fail("on %s these exact descriptors were compared against your own wallet "+
			"software and they did not match. This wallet must not fund a batch. "+
			"Export the descriptors again from the software that holds the keys, and "+
			"set up under a new wallet name — importing a corrected pair here would "+
			"leave the rejected one's coins selectable.", when)
		c.fix("winthistle example-descriptors > cold.toml\n"+
			"# then change [bitcoind] wallet in %s and:\n"+
			"winthistle setup --descriptors cold.toml", cfg.Path)
		return
	}

	c.say("addresses confirmed on %s, %d per branch, against these exact descriptors",
		when, prev.SampleSize)

	// The tripwire. The descriptor strings match, so derivation is deterministic
	// and this should never differ — which is exactly why it is worth asking:
	// a mismatch here means the recorded address did not come from the recorded
	// descriptor, and no amount of reasoning about descriptors would find that.
	if node == nil {
		return
	}
	addrs, err := node.DeriveAddresses(ctx, receive, 0, 0)
	if err != nil {
		c.warn("re-deriving the confirmed address: %v", err)
		return
	}
	if len(addrs) == 0 || addrs[0] != prev.FirstReceive {
		c.fail("the confirmed receive address is not the one this wallet's " +
			"descriptor derives today.")
		c.say("    confirmed: %s", prev.FirstReceive)
		c.say("    derives:   %s", first(addrs))
		c.say("The descriptor string in the record matches the one in the wallet, " +
			"so these two cannot disagree unless the record and the wallet are " +
			"describing different things. Do not open a batch here.")
		c.fix("winthistle setup")
	}
}

func first(addrs []string) string {
	if len(addrs) == 0 {
		return "(nothing)"
	}
	return addrs[0]
}

func checkCoins(ctx context.Context, r *Report, cfg *config.Config, wallet *bitcoind.Client) {
	c := r.add(Check{Name: "the coins"})
	if wallet == nil {
		c.Status = Skip
		c.say("not checked: Core could not be reached")
		return
	}
	coins, err := coldwallet.SelectCoins(ctx, wallet, cfg.Limits.MinConfirmations())
	if err != nil {
		c.fail("listing the cold wallet's coins: %v", err)
		return
	}
	c.say("%d spendable coin%s, %s", len(coins.Eligible), prose.Plural(len(coins.Eligible)),
		prose.Sats(coins.EligibleSat))
	if len(coins.Excluded) > 0 {
		c.warn("%d coin%s excluded, %s, so this wallet's own balance and anything "+
			"this tool builds will disagree:", len(coins.Excluded),
			prose.Plural(len(coins.Excluded)), prose.Sats(coins.ExcludedSat))
		for _, e := range coins.Excluded {
			c.say("    %s  %s — %s", e.Coin.Outpoint(), e.Why, e.Detail)
		}
		c.say("A legacy input is refused by LND outright — verifyAllInputsSegWit, " +
			"\"risk of malleability\" — so these are fenced off rather than " +
			"selected and then rejected.")
	}
	if len(coins.Eligible) == 0 {
		c.fail("nothing to fund a batch with.")
	}
}

func checkReserve(ctx context.Context, r *Report, cli *lnd.Client, opts Options) {
	c := r.add(Check{Name: "the anchor reserve"})
	if cli == nil {
		c.Status = Skip
		c.say("not checked: LND could not be reached")
		return
	}

	batch := reserve.Batch{Public: 1}
	if opts.Batch != nil {
		batch = arm.BatchOf(opts.Batch.ArmChannels())
	} else {
		c.say("no batch file given, so this is the answer for one announced channel")
	}

	f, err := reserve.Check(ctx, cli.WalletKit, batch)
	if err != nil {
		c.fail("%v", err)
		return
	}
	c.say("%s", f.Summary())
	switch f.Verdict() {
	case reserve.WouldBeRefused:
		c.fail("psbt_verify would refuse this batch — step 5, with the cold wallet " +
			"already out and the peers' clocks running. The refusal is about this " +
			"node's own on-chain wallet; it says nothing about the cold wallet, " +
			"which is fine, and LND's own wording does not mention either.")
		c.say("The plan adds a top-up output for the %s shortfall automatically, "+
			"and it counts at verify — CheckReservedValue credits an output paying "+
			"an address the node's wallet owns, so there is no second transaction "+
			"and no wait. What it cannot do is find the money: it comes out of this "+
			"batch, so the cold wallet pays for it.",
			prose.Sats(f.ShortfallAtVerify()))
	case reserve.ShortAfterBatch:
		c.warn("the batch will verify, but publishing it leaves the node %s under "+
			"the reserve LND wants for the channels it will then have. Below it LND "+
			"declines further on-chain spends and public channel opens.",
			prose.Sats(f.ShortfallAfterBatch()))
	case reserve.NotApplicable:
		c.say("every channel in this batch is unannounced, so LND's check does not " +
			"run at all: enforceNewReservedValue returns before it counts anything " +
			"for an unannounced channel.")
	}
}

func checkFees(ctx context.Context, r *Report, cfg *config.Config, node *bitcoind.Client) {
	c := r.add(Check{Name: "the fee rate"})
	if node == nil {
		c.Status = Skip
		c.say("not checked: Core could not be reached")
		return
	}
	rate, err := fees.Estimate(ctx, node, fees.Request{
		TargetBlocks:  cfg.Fees.TargetBlocks,
		Mode:          cfg.Fees.Mode,
		FloorSatPerVB: cfg.Fees.FloorSatPerVB,
	})
	if err != nil {
		c.fail("%v", err)
		c.say("Core is entitled to answer \"insufficient data\", and does so on " +
			"every regtest node, on a freshly synced one, and on any node that has " +
			"been offline for a while. That is not a fault — but a rate has to come " +
			"from somewhere, and I-4 means a rate chosen badly cannot be corrected " +
			"by replacing the transaction.")
		c.fix("# add to %s:\n[fees]\nfloor_sat_per_vb = 2.0", cfg.Path)
		return
	}
	c.say("%s", rate.Summary())
	if !rate.Estimated() {
		c.warn("Core had no estimate, so the batch would be built at a floor rather " +
			"than at a market rate. Worth knowing before the evening rather than " +
			"during it.")
	}
	if cfg.Fees.FloorSatPerVB == 0 {
		c.warn("[fees] floor_sat_per_vb is not set. It has no default on purpose: a " +
			"node with no estimate and no floor is an error rather than a guess, " +
			"and this node happens to have an estimate today.")
		c.fix("# add to %s:\n[fees]\nfloor_sat_per_vb = 2.0", cfg.Path)
	}
}

func checkPeers(ctx context.Context, r *Report, cli *lnd.Client, opts Options) {
	c := r.add(Check{Name: "the peers"})
	switch {
	case cli == nil:
		c.Status = Skip
		c.say("not checked: LND could not be reached")
		return
	case opts.Batch == nil:
		c.Status = Skip
		c.say("not checked: no batch file given. Pass one with --batch to have the " +
			"peers resolved in the local graph.")
		return
	}

	wants := opts.Batch.Wants()
	if !opts.Connect {
		// Do not connect from a diagnostic. ConnectPeer is a change to the node,
		// and doctor is the one command that should be safe to run at any moment.
		for i := range wants {
			wants[i].Host = ""
		}
	}
	facts, err := peers.Check(ctx, cli.Lightning, wants)
	if err != nil {
		c.fail("%v", err)
		return
	}
	for _, f := range facts {
		c.say("%s", f.Summary())
		switch {
		case !f.KeyOK:
			c.fail("    %s", f.KeyProblem)
		case !f.InGraph:
			c.warn("    not in the local gossip graph. A brand new node, or one " +
				"with only private channels, looks exactly like this — but nothing " +
				"below that line can be said about it.")
		case f.BelowSmallest():
			c.warn("    this batch asks for less than the smallest channel this " +
				"peer already has. That is a proxy and not a limit: there is no " +
				"gossip field for a minimum channel size, and the only authoritative " +
				"answer costs one of the peer's pending-channel slots for eleven " +
				"minutes.")
		}
	}
	c.say("Nothing here is a verdict. The peer's minimum, its reserve and its " +
		"accepted commitment type are enforced conversationally and published " +
		"nowhere, so the only authoritative answer is accept_channel.")
}

func checkJournal(ctx context.Context, r *Report, cfg *config.Config,
	wallet *bitcoind.Client, j *journal.Journal, openErr error) {

	c := r.add(Check{Name: "the run journal"})
	if j == nil {
		c.fail("%v", openErr)
		return
	}
	c.say("    %s", cfg.Server.Journal)

	unfinished, err := j.Unfinished(ctx)
	if err != nil {
		c.fail("reading the journal: %v", err)
		return
	}
	claimed := map[bitcoind.Outpoint]string{}
	for _, run := range unfinished {
		for _, l := range run.Locks {
			if !l.Released {
				claimed[l.Outpoint] = run.ID
			}
		}
	}

	// A CPFP child's lock has an owner too, and its owner is not in the list
	// above: the run a child accelerates is published, and a published run is
	// not unfinished. Without this every bump in progress makes the batch's
	// change output read as an orphan — which is the one diagnosis on this
	// screen that tells an operator to unlock a coin something is using.
	bumps, err := j.UnfinishedBumps(ctx)
	if err != nil {
		c.fail("reading the journal's CPFP children: %v", err)
		return
	}
	for _, b := range bumps {
		for _, l := range b.Locks {
			if !l.Released {
				claimed[l.Outpoint] = fmt.Sprintf("%s bump %d", b.RunID, b.Seq)
			}
		}
	}
	if len(unfinished) > 0 {
		c.fail("%d run%s stopped somewhere %s should not have:", len(unfinished),
			prose.Plural(len(unfinished)), prose.IsAre(len(unfinished)))
		for _, run := range unfinished {
			c.say("    %s — %s", run.ID, run.State)
		}
		c.fix("winthistle recover")
	} else {
		c.say("no unfinished runs")
	}

	// A warning rather than a failure. An unfinished child costs a coin lock and
	// nothing else — there is no peer holding a reservation and no channel in a
	// half-open state — so it is a thing to tidy up rather than a thing that
	// stops the next batch. The exception says itself: one that may be public.
	if len(bumps) > 0 {
		c.warn("%d CPFP child%s unfinished:", len(bumps),
			pluralES(len(bumps)))
		for _, b := range bumps {
			note := ""
			if b.MayBePublic() {
				note = " — may already be in a mempool, so do not assume it is not"
			}
			c.say("    %s bump %d — %s%s", b.RunID, b.Seq, b.State, note)
		}
		c.fix("winthistle bump <run-id> --abandon")
	}

	if wallet == nil {
		return
	}
	locks, err := wallet.ListLocks(ctx)
	if err != nil {
		c.warn("could not list Core's coin locks: %v", err)
		return
	}
	var orphans []bitcoind.Outpoint
	for _, op := range locks {
		if _, ours := claimed[op]; !ours {
			orphans = append(orphans, op)
		}
	}
	sort.Slice(orphans, func(i, j int) bool { return orphans[i].String() < orphans[j].String() })
	switch {
	case len(orphans) == 0 && len(locks) > 0:
		c.say("Core holds %d coin lock%s, all of them claimed by a run above",
			len(locks), prose.Plural(len(locks)))
	case len(orphans) > 0:
		c.warn("Core holds %d locked coin%s that no run in this journal claims. A "+
			"wallet that will not spend its own money, with nothing on disk saying "+
			"why, is exactly what a crash between walletcreatefundedpsbt and the "+
			"journal write leaves behind.", len(orphans), prose.Plural(len(orphans)))
		for _, op := range orphans {
			c.say("    %s", op.String())
		}
		c.fix("bitcoin-cli -rpcwallet=%q lockunspent true '%s'",
			cfg.Bitcoind.Wallet, lockJSON(orphans))
		c.say("Core validates the whole list before applying any of it, and " +
			"refuses an entry already in the requested state, so one stale outpoint " +
			"frees nothing. The locks are memory-only: a Core restart clears them " +
			"all at once.")
	}
}

// pluralES is prose.Plural for a word that takes -es. "child" takes neither, so
// this one carries the "ren" it needs.
func pluralES(n int) string {
	if n == 1 {
		return ""
	}
	return "ren"
}

func lockJSON(ops []bitcoind.Outpoint) string {
	parts := make([]string, 0, len(ops))
	for _, op := range ops {
		parts = append(parts, fmt.Sprintf(`{"txid":"%s","vout":%d}`, op.TxID, op.Vout))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func port(addr string) string {
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		return addr[i+1:]
	}
	return addr
}
