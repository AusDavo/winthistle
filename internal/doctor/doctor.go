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
	"strings"
	"time"

	"github.com/AusDavo/winthistle/internal/arm"
	"github.com/AusDavo/winthistle/internal/config"
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

	j, journalErr := openJournal(ctx, cfg)
	if j != nil {
		defer j.Close()
	}

	checkReserve(ctx, r, cli, opts)
	checkPeers(ctx, r, cli, opts)
	checkJournal(ctx, r, cfg, j, journalErr)
	return r
}

// openJournal makes the directory and opens the file, which is the one thing
// doctor creates: a journal that does not exist yet is not a fault.
func openJournal(ctx context.Context, cfg *config.Config) (*journal.Journal, error) {
	if dir := filepath.Dir(cfg.Journal.Path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("making %s: %w", dir, err)
		}
	}
	j, err := journal.Open(ctx, cfg.Journal.Path)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", cfg.Journal.Path, err)
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
	c.say("lnd %s", cfg.LND.Address)
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
		// Not in the switch above: a competing pending open is orthogonal to
		// every case in it, and a peer can easily be both absent from the graph
		// and already holding a channel from us.
		if f.HasCompetingOpen() {
			c.warn("    %d channel%s already pending open with this peer, before "+
				"the batch adds one. Against a peer running LND's default of one "+
				"pending channel there is no room left, and the refusal arrives at "+
				"step 2 with the cold wallet out. Not a verdict: "+
				"--maxpendingchannels is the peer's own and is published nowhere.",
				len(f.Pending), prose.Plural(len(f.Pending)))
			for _, po := range f.Pending {
				c.say("      %s", po.ChannelPoint)
			}
			if anyOurs(f.Pending) {
				c.fix("winthistle recover")
			}
		}
	}
	c.say("Nothing here is a verdict. The peer's minimum, its reserve and its " +
		"accepted commitment type are enforced conversationally and published " +
		"nowhere, so the only authoritative answer is accept_channel.")
}

// anyOurs reports whether this node opened any of these pending channels, which
// is what makes `winthistle recover` the right thing to suggest: a channel we
// opened and did not finish is one this tool may be able to take apart, and one
// the peer opened is not ours to touch.
func anyOurs(pending []peers.PendingOpen) bool {
	for _, po := range pending {
		if po.Ours {
			return true
		}
	}
	return false
}

// checkJournal is the runs that stopped.
//
// It reconciled Core's coin locks against the journal's owners until item 5. A
// run takes no coin locks — this app selects no coins — and there is no Core
// client to ask, so what is left is the half that was always about this tool's
// own state.
func checkJournal(ctx context.Context, r *Report, cfg *config.Config,
	j *journal.Journal, openErr error) {

	c := r.add(Check{Name: "the run journal"})
	if j == nil {
		c.fail("%v", openErr)
		return
	}
	c.say("    %s", cfg.Journal.Path)

	unfinished, err := j.Unfinished(ctx)
	if err != nil {
		c.fail("reading the journal: %v", err)
		return
	}
	if len(unfinished) == 0 {
		c.say("no unfinished runs")
		return
	}
	c.fail("%d run%s stopped somewhere %s should not have:", len(unfinished),
		prose.Plural(len(unfinished)), prose.IsAre(len(unfinished)))
	for _, run := range unfinished {
		c.say("    %s — %s", run.ID, run.State)
	}
	c.fix("winthistle recover")
}

func port(addr string) string {
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		return addr[i+1:]
	}
	return addr
}
