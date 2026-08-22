// Package coldwallet is directed mode's half of the setup: the watch-only Core
// wallet that holds the cold storage descriptors.
//
// One private-key-less descriptor wallet in Core is what gives the app coin
// selection, change derivation, exact fee rates and PSBT construction with the
// bip32 derivations signers need, for no bespoke cryptography. Nothing here
// holds or handles a key, and the wallet it builds is structurally incapable of
// signing — see bitcoind.CreateWatchOnlyWallet.
//
// # Why this is not a thin wrapper around importdescriptors
//
// Both of the mistakes the design names produce a wallet that imports cleanly
// and then lies:
//
//  1. The timestamp is the cold wallet's birthday, not now. Pass "now" and Core
//     reports a zero balance, because it never scanned the blocks the coins
//     arrived in. bitcoind.ImportRequest types Timestamp as an int64 so the
//     string "now" cannot be expressed at all, and Config refuses a birthday it
//     was not given.
//
//  2. sortedmulti and multi produce entirely different addresses from identical
//     keys. The wallet imports, shows nothing, and explains nothing.
//
// Neither is detectable from the import succeeding, and neither is detectable
// from a balance either — a correct wallet whose coins have not moved yet also
// shows zero. So Install ends in a round-trip address check rather than in a
// success message: the first few receive and change addresses, derived from the
// descriptors that actually landed in the wallet, for the operator to compare
// against their own wallet software. That one comparison catches wrong
// derivation paths, multi-versus-sortedmulti, transposed keys and truncated
// xpubs — the whole family, at the only moment when catching them is free.
//
// # What regtest cannot prove here
//
// The rescan. Regtest has no history to rescan, so a wrong birthday on regtest
// is indistinguishable from a right one, and the pruning check has nothing to
// fire on. Those need signet — see CLAUDE.md. The regtest tests cover the
// lifecycle, the round trip, and the refusals; they do not cover the rescan, and
// they say so.
package coldwallet

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/AusDavo/winthistle/internal/bitcoind"
)

// Genesis is the birthday to give when the cold wallet's own is unknown and the
// operator has decided to scan the whole chain. Core clamps any earlier
// timestamp to the genesis block.
//
// It is spelled out rather than being the zero value on purpose: a zero
// time.Time reaching Core as "scan everything" would turn a forgotten field into
// a multi-hour rescan on mainnet, and a forgotten field should be an error.
var Genesis = time.Unix(1231006505, 0).UTC()

// Defaults for the two knobs that have a sensible one.
const (
	// DefaultGapLimit matches the design's range of [0,999]. Core derives the
	// whole range at import, so this is also the cost of the import.
	DefaultGapLimit = 999

	// DefaultSampleSize is how many receive and change addresses the round-trip
	// check shows.
	//
	// Five rather than one, and the reason is measured rather than tidy.
	// sortedmulti and multi agree at every index where the derived keys already
	// happen to be in ascending order — for two keys, about half of them. On the
	// harness's own 2-of-2 the two descriptors agree on 13 of the first 20
	// addresses, index 0 among them, so an operator who compared a single
	// address would have passed a wrong descriptor. Five leaves roughly a three
	// per cent chance of the whole sample agreeing for a 2-of-2, and less for
	// larger quorums.
	DefaultSampleSize = 5

	// RecentBirthday is how close to now a birthday has to be before the report
	// says out loud that it looks like "now" wearing a date.
	RecentBirthday = 7 * 24 * time.Hour
)

// Config is what the operator brings: a wallet name, the two descriptors, and
// the birthday.
type Config struct {
	// WalletName is the Core wallet to create. It becomes a directory name, so
	// it may not contain a path separator.
	WalletName string

	// Receive and Change are the cold wallet's external and internal
	// descriptors, exactly as exported from the operator's own wallet software.
	// A checksum is optional — Core computes one either way — but full
	// key-origin prefixes are what let a hardware signer recognise its own key.
	Receive string
	Change  string

	// Birthday is the cold wallet's, and Core rescans from it. Required: see
	// Genesis for the deliberate way to say "scan everything".
	Birthday time.Time

	// GapLimit is the top of the descriptor range; zero means DefaultGapLimit.
	GapLimit int

	// SampleSize is how many addresses the round-trip check derives on each
	// branch; zero means DefaultSampleSize.
	SampleSize int
}

func (c Config) gapLimit() int {
	if c.GapLimit <= 0 {
		return DefaultGapLimit
	}
	return c.GapLimit
}

func (c Config) sampleSize() int {
	if c.SampleSize <= 0 {
		return DefaultSampleSize
	}
	return c.SampleSize
}

// timestamp is what goes into importdescriptors: the birthday in Unix seconds,
// floored at zero, which Core reads as the genesis block.
func (c Config) timestamp() int64 {
	if c.Birthday.Before(Genesis) {
		return 0
	}
	return c.Birthday.Unix()
}

// Concern is something worth saying about a setup, and whether it stops it.
//
// Fatal is the distinction that matters: a wrong descriptor *shape* can be
// refused outright, but multi-versus-sortedmulti cannot — both are valid
// descriptors and only the operator knows which their wallet uses. So the
// sniffable traps are warnings that the address check then settles, and only
// the things that are wrong on their face are fatal.
type Concern struct {
	Fatal    bool
	Subject  string // "the receive descriptor", "the birthday"
	Headline string
	Detail   string
}

func (c Concern) String() string {
	if c.Detail == "" {
		return c.Headline
	}
	return c.Headline + " " + c.Detail
}

// AnyFatal reports whether any concern in the list stops the setup.
func AnyFatal(cs []Concern) bool {
	for _, c := range cs {
		if c.Fatal {
			return true
		}
	}
	return false
}

// firstFatal returns the first blocking concern, for an error message.
func firstFatal(cs []Concern) (Concern, bool) {
	for _, c := range cs {
		if c.Fatal {
			return c, true
		}
	}
	return Concern{}, false
}

// ---------------------------------------------------------------------------
// Reading a descriptor without a node
// ---------------------------------------------------------------------------

var (
	// extendedKey matches an xpub/tpub/xprv/tprv and its base58 body. Loose on
	// length on purpose: a truncated key should be caught and named, not missed.
	extendedKey = regexp.MustCompile(`\b[xyzYZtuUvV](?:pub|prv)[1-9A-HJ-NP-Za-km-z]{20,}`)

	// hexKey matches a raw compressed or x-only pubkey.
	hexKey = regexp.MustCompile(`\b(?:0[23][0-9a-fA-F]{64}|[0-9a-fA-F]{64})\b`)

	// origin matches a key-origin prefix: [fingerprint/path].
	origin = regexp.MustCompile(`\[[0-9a-fA-F]{8}(?:/[0-9]+h?'?)*\]`)

	// branch matches the trailing /N/* of a ranged key expression.
	branch = regexp.MustCompile(`/([0-9]+)/\*`)
)

// Shape is what can be read off a descriptor string without asking anything.
//
// Local on purpose: a descriptor carrying a private key must be refused before
// it is sent anywhere, including to the operator's own node.
type Shape struct {
	Ranged      bool
	SortedMulti bool
	PlainMulti  bool
	Keys        int
	Origins     int
	HasPrivate  bool

	// Branches are the /N/* indices found, deduplicated. A well-formed pair has
	// exactly one each, and they differ.
	Branches []int
}

// Inspect reads a descriptor string.
func Inspect(desc string) Shape {
	body := desc
	if i := strings.LastIndex(body, "#"); i > 0 {
		body = body[:i]
	}
	sorted := strings.Count(body, "sortedmulti(")
	all := strings.Count(body, "multi(")

	s := Shape{
		Ranged:      strings.Contains(body, "*"),
		SortedMulti: sorted > 0,
		PlainMulti:  all-sorted > 0,
		Origins:     len(origin.FindAllString(body, -1)),
		HasPrivate:  strings.Contains(body, "prv"),
	}

	// Count keys as extended keys plus raw pubkeys that are not part of one.
	withoutExtended := extendedKey.ReplaceAllString(body, "")
	s.Keys = len(extendedKey.FindAllString(body, -1)) +
		len(hexKey.FindAllString(withoutExtended, -1))

	seen := map[int]bool{}
	for _, m := range branch.FindAllStringSubmatch(body, -1) {
		var n int
		if _, err := fmt.Sscanf(m[1], "%d", &n); err != nil {
			continue
		}
		if !seen[n] {
			seen[n] = true
			s.Branches = append(s.Branches, n)
		}
	}
	return s
}

// SoleBranch returns the descriptor's derivation branch when every key agrees on
// one, which is the only case anything is inferred from.
func (s Shape) SoleBranch() (int, bool) {
	if len(s.Branches) != 1 {
		return 0, false
	}
	return s.Branches[0], true
}

// Validate checks the config against everything that can be known locally.
//
// It never contacts a node, so it is the first thing setup runs and the thing a
// UI can run on every keystroke.
func (c Config) Validate() []Concern {
	var out []Concern
	add := func(fatal bool, subject, headline, detail string) {
		out = append(out, Concern{Fatal: fatal, Subject: subject,
			Headline: headline, Detail: detail})
	}

	switch {
	case strings.TrimSpace(c.WalletName) == "":
		add(true, "the wallet name", "No wallet name was given.",
			"Core needs one to create the watch-only wallet.")
	case strings.ContainsAny(c.WalletName, `/\`):
		add(true, "the wallet name",
			fmt.Sprintf("The wallet name %q contains a path separator.", c.WalletName),
			"Core reads that as a directory under its wallet directory, which is "+
				"almost never what was meant.")
	}

	pairs := []struct {
		subject string
		desc    string
		want    int // conventional branch
	}{
		{"the receive descriptor", c.Receive, 0},
		{"the change descriptor", c.Change, 1},
	}
	shapes := make([]Shape, len(pairs))
	for i, p := range pairs {
		if strings.TrimSpace(p.desc) == "" {
			add(true, p.subject, fmt.Sprintf("No %s was given.", strings.TrimPrefix(p.subject, "the ")),
				"Directed mode needs both branches: without the change descriptor "+
					"Core cannot derive change, and a batch with no change output "+
					"cannot be fee-bumped by CPFP (I-4).")
			continue
		}
		s := Inspect(p.desc)
		shapes[i] = s

		if s.HasPrivate {
			add(true, p.subject, "That descriptor contains a private key.",
				"It has not been sent anywhere. This wallet is watch-only by "+
					"construction — the signers hold the keys and the app holds "+
					"the last signature (I-2). Export the public descriptor instead.")
			continue
		}
		if !s.Ranged {
			add(true, p.subject, "That descriptor is not ranged.",
				"It has no /* at the end, so it describes one address rather than a "+
					"branch. Core would import exactly that address and nothing else.")
		}
		if s.PlainMulti && !s.SortedMulti {
			add(false, p.subject, "That descriptor uses multi(), not sortedmulti().",
				"They produce entirely different addresses from identical keys, and "+
					"the wrong one imports cleanly, shows a zero balance and explains "+
					"nothing. Most wallet software — Sparrow, Specter, Electrum, and "+
					"anything following BIP-67 — exports sortedmulti(). The address "+
					"check at the end is what settles this; do not skip it.")
		}
		if s.Keys > 0 && s.Origins < s.Keys {
			add(false, p.subject,
				fmt.Sprintf("%d of the %d keys carry no [fingerprint/path] origin prefix.",
					s.Keys-s.Origins, s.Keys),
				"Origin info is how a hardware signer recognises its own key. "+
					"Without it, devices refuse to sign — which would be discovered "+
					"during the rehearsal at the earliest, and in the window at worst.")
		}
		if b, ok := s.SoleBranch(); ok && b != p.want {
			add(false, p.subject,
				fmt.Sprintf("That descriptor derives from branch /%d/*, not /%d/*.", b, p.want),
				"Not wrong on its own — some wallets use other branches — but a "+
					"receive and change pair the wrong way round is the ordinary way "+
					"this happens.")
		}
	}

	if c.Receive != "" && c.Receive == c.Change {
		add(true, "the descriptors", "The receive and change descriptors are identical.",
			"Change would then be paid to the same branch the funding addresses come "+
				"from, and the wallet would have no internal branch at all.")
	}

	switch {
	case c.Birthday.IsZero():
		add(true, "the birthday", "No birthday was given.",
			"Core rescans from this timestamp, and the design names getting it wrong "+
				"as one of the two traps: a birthday of \"now\" produces a wallet that "+
				"imports cleanly and reports a zero balance, because it never scanned "+
				"the blocks the coins arrived in. Give the cold wallet's real birthday, "+
				"or coldwallet.Genesis to scan the whole chain deliberately.")
	case c.Birthday.After(time.Now()):
		add(true, "the birthday",
			fmt.Sprintf("The birthday %s is in the future.",
				c.Birthday.UTC().Format("2006-01-02")),
			"Core would scan nothing at all and the wallet would report a zero "+
				"balance whatever it holds.")
	case time.Since(c.Birthday) < RecentBirthday:
		add(false, "the birthday",
			fmt.Sprintf("The birthday %s is within the last week.",
				c.Birthday.UTC().Format("2006-01-02")),
			"Correct for a cold wallet created this week, and the trap the design "+
				"names for one that is not: the rescan will cover almost nothing, and "+
				"a wallet whose coins arrived earlier will report a zero balance "+
				"without saying why.")
	}

	if c.GapLimit < 0 {
		add(true, "the gap limit", fmt.Sprintf("A gap limit of %d is not a range.", c.GapLimit), "")
	}
	if c.SampleSize < 0 {
		add(true, "the address check",
			fmt.Sprintf("A sample size of %d is not a number of addresses.", c.SampleSize), "")
	}
	return out
}

// ---------------------------------------------------------------------------
// The lifecycle
// ---------------------------------------------------------------------------

// Prepared is the checksummed pair Core agreed to parse, before anything is
// imported.
type Prepared struct {
	Receive bitcoind.DescriptorInfo
	Change  bitcoind.DescriptorInfo
}

// Prepare hands both descriptors to getdescriptorinfo.
//
// Node-level and side-effect free: nothing is created and nothing is imported.
// It is worth doing separately because it is where a malformed descriptor, a bad
// key, a truncated xpub or a wrong hand-written checksum fails — all of them
// before a wallet exists to be half-configured.
//
// The checksum Core returns is the one that gets imported. Writing one by hand
// is possible and pointless: Core computes it from the descriptor, so the only
// thing a hand-written checksum can do is disagree.
func Prepare(ctx context.Context, node *bitcoind.Client, cfg Config) (Prepared, error) {
	if c, ok := firstFatal(cfg.Validate()); ok {
		return Prepared{}, fmt.Errorf("%s: %s", c.Subject, c)
	}
	recv, err := node.DescribeDescriptor(ctx, cfg.Receive)
	if err != nil {
		return Prepared{}, fmt.Errorf("Core could not parse the receive descriptor: %w", err)
	}
	chg, err := node.DescribeDescriptor(ctx, cfg.Change)
	if err != nil {
		return Prepared{}, fmt.Errorf("Core could not parse the change descriptor: %w", err)
	}
	for _, d := range []struct {
		what string
		info bitcoind.DescriptorInfo
	}{{"receive", recv}, {"change", chg}} {
		if d.info.HasPrivateKeys {
			return Prepared{}, fmt.Errorf("the %s descriptor contains a private key; "+
				"this wallet is watch-only by construction", d.what)
		}
		if !d.info.IsRange {
			return Prepared{}, fmt.Errorf("Core reports the %s descriptor is not ranged, "+
				"so it describes one address rather than a branch", d.what)
		}
		if !d.info.IsSolvable {
			return Prepared{}, fmt.Errorf("Core reports the %s descriptor is not solvable: "+
				"it cannot work out how to spend from it, so it cannot build a PSBT "+
				"a signer could complete", d.what)
		}
	}
	if recv.Descriptor == chg.Descriptor {
		return Prepared{}, fmt.Errorf("Core normalised both descriptors to the same thing, " +
			"so the wallet would have no internal branch")
	}
	return Prepared{Receive: recv, Change: chg}, nil
}

// requests renders the two importdescriptors entries.
func (p Prepared) requests(cfg Config) []bitcoind.ImportRequest {
	ts, top := cfg.timestamp(), cfg.gapLimit()
	// A range slice each. Sharing one would make widenToExisting raise both
	// branches whenever either had grown, which Core tolerates and nobody asked
	// for.
	return []bitcoind.ImportRequest{
		{Desc: p.Receive.Descriptor, Active: true, Internal: false,
			Range: []int{0, top}, Timestamp: ts},
		{Desc: p.Change.Descriptor, Active: true, Internal: true,
			Range: []int{0, top}, Timestamp: ts},
	}
}

// Import creates the descriptors in the wallet and rescans from the birthday.
//
// The call blocks for the whole rescan. On mainnet that is minutes to hours, so
// the client must have been built with a Timeout that covers it and the context
// must carry a deadline that does too — see bitcoind.Config.Timeout. The design's
// rule is the other half of this: import during setup, never during a batch.
//
// Core reports per-entry failures inside a successful response, so both entries
// are checked. A half-import — receive in, change refused — is precisely the
// state that yields a wallet which looks healthy right up to the moment it has
// to derive change.
func Import(ctx context.Context, wallet *bitcoind.Client, cfg Config, p Prepared) ([]string, error) {
	reqs := p.requests(cfg)

	// Core grows an active descriptor's range by itself, topping the keypool up
	// as addresses are handed out, and then refuses a re-import that would
	// shrink it: "new range must include current range = [0,1003]". Observed on
	// Core 29 against a wallet imported with a range of [0,50].
	//
	// So the gap limit is a floor rather than a setting, and a second Install —
	// the resume this whole path is idempotent for — has to widen to whatever
	// Core has grown to rather than re-assert what it was told the first time.
	if existing, err := wallet.ListDescriptors(ctx); err == nil {
		widenToExisting(reqs, existing)
	}

	results, err := wallet.ImportDescriptors(ctx, reqs)
	if err != nil {
		return nil, fmt.Errorf("importing the descriptors: %w", err)
	}
	var warnings []string
	names := []string{"receive", "change"}
	for i, r := range results {
		what := names[i]
		for _, w := range r.Warnings {
			warnings = append(warnings, fmt.Sprintf("%s descriptor: %s", what, w))
		}
		if r.Success {
			continue
		}
		if r.Error != nil {
			return warnings, fmt.Errorf("Core refused the %s descriptor: %w", what, r.Error)
		}
		return warnings, fmt.Errorf("Core refused the %s descriptor and said why to no one", what)
	}
	return warnings, nil
}

// widenToExisting raises each request's range top to whatever the wallet already
// holds for that descriptor.
func widenToExisting(reqs []bitcoind.ImportRequest, existing []bitcoind.WalletDescriptor) {
	top := make(map[string]int, len(existing))
	for _, d := range existing {
		if len(d.Range) == 2 {
			top[d.Desc] = d.Range[1]
		}
	}
	for i := range reqs {
		if have, ok := top[reqs[i].Desc]; ok && len(reqs[i].Range) == 2 &&
			have > reqs[i].Range[1] {

			reqs[i].Range[1] = have
		}
	}
}

// Landed is what the wallet actually holds, read back rather than assumed.
type Landed struct {
	Info    bitcoind.WalletInfo
	Receive bitcoind.WalletDescriptor
	Change  bitcoind.WalletDescriptor
	Others  []bitcoind.WalletDescriptor
}

// Confirm reads the wallet back and checks it is the wallet we meant to build.
//
// Everything here is a property the import succeeding does not establish. The
// wallet may have existed already, with different descriptors, made by a
// different tool, with private keys enabled; createwallet is idempotent so that
// a half-finished setup can be resumed, and this is the price of that.
func Confirm(ctx context.Context, wallet *bitcoind.Client, cfg Config, p Prepared) (Landed, error) {
	info, err := wallet.GetWalletInfo(ctx)
	if err != nil {
		return Landed{}, fmt.Errorf("reading the wallet back: %w", err)
	}
	if info.PrivateKeysEnabled {
		return Landed{}, fmt.Errorf("the Core wallet %q has private keys enabled. "+
			"This app must be structurally incapable of signing (I-2); the wallet it "+
			"uses has to be created with disable_private_keys", info.Name)
	}
	if !info.Descriptors {
		return Landed{}, fmt.Errorf("the Core wallet %q is a legacy wallet, which cannot "+
			"hold an imported ranged descriptor", info.Name)
	}

	descs, err := wallet.ListDescriptors(ctx)
	if err != nil {
		return Landed{}, fmt.Errorf("listing the wallet's descriptors: %w", err)
	}

	l := Landed{Info: info}
	for _, d := range descs {
		switch d.Desc {
		case p.Receive.Descriptor:
			l.Receive = d
		case p.Change.Descriptor:
			l.Change = d
		default:
			l.Others = append(l.Others, d)
		}
	}
	if l.Receive.Desc == "" {
		return l, fmt.Errorf("the receive descriptor is not in the wallet after importing it.\n"+
			"  asked for: %s\n  wallet holds: %s", p.Receive.Descriptor, describeAll(descs))
	}
	if l.Change.Desc == "" {
		return l, fmt.Errorf("the change descriptor is not in the wallet after importing it.\n"+
			"  asked for: %s\n  wallet holds: %s", p.Change.Descriptor, describeAll(descs))
	}
	if !l.Receive.Active || l.Receive.Internal {
		return l, fmt.Errorf("the receive descriptor landed as active=%v internal=%v, "+
			"not active=true internal=false", l.Receive.Active, l.Receive.Internal)
	}
	if !l.Change.Active || !l.Change.Internal {
		return l, fmt.Errorf("the change descriptor landed as active=%v internal=%v, "+
			"not active=true internal=true", l.Change.Active, l.Change.Internal)
	}
	return l, nil
}

func describeAll(descs []bitcoind.WalletDescriptor) string {
	if len(descs) == 0 {
		return "(nothing)"
	}
	out := make([]string, 0, len(descs))
	for _, d := range descs {
		out = append(out, d.Desc)
	}
	return "\n    " + strings.Join(out, "\n    ")
}

// Result is everything Install learned. It is deliberately not a success value:
// see Verdict.
type Result struct {
	Config    Config
	Concerns  []Concern
	Preflight Preflight
	Prepared  Prepared
	Warnings  []string
	Landed    Landed
	Check     AddressCheck
}

// Verdict is how far setup got.
type Verdict int

const (
	// AwaitingAddressCheck is the only outcome a completed Install returns. The
	// wallet is built and self-consistent, and nothing about that rules out the
	// two traps — only the operator comparing the derived addresses against
	// their own wallet software does.
	AwaitingAddressCheck Verdict = iota

	// Unavailable: Core cannot serve directed mode. See Preflight.
	Unavailable

	// Refused: the configuration is wrong on its face and nothing was created.
	Refused
)

// Verdict reads the result.
func (r *Result) Verdict() Verdict {
	switch {
	case AnyFatal(r.Concerns):
		return Refused
	case !r.Preflight.Available():
		return Unavailable
	default:
		return AwaitingAddressCheck
	}
}

// Install runs the whole setup: pre-flight, prepare, create, import, confirm,
// and derive the addresses for the round-trip check.
//
// node must be a client with no wallet scope; wallet must be one bound to
// cfg.WalletName. It does not have to exist yet.
//
// Install never reports success. Its last act is to produce a question — see
// Result.Report — and the caller must not treat directed mode as configured
// until the operator has answered it.
func Install(ctx context.Context, node, wallet *bitcoind.Client, cfg Config) (*Result, error) {
	r := &Result{Config: cfg, Concerns: cfg.Validate()}
	if c, ok := firstFatal(r.Concerns); ok {
		return r, fmt.Errorf("%s: %s", c.Subject, c)
	}

	pf, err := RunPreflight(ctx, node, cfg)
	if err != nil {
		return r, err
	}
	r.Preflight = pf
	if !pf.Available() {
		return r, fmt.Errorf("this Core node cannot serve directed mode: %s", pf.Summary())
	}

	if r.Prepared, err = Prepare(ctx, node, cfg); err != nil {
		return r, err
	}
	if err := node.CreateWatchOnlyWallet(ctx, cfg.WalletName); err != nil {
		return r, fmt.Errorf("creating the watch-only wallet %q: %w", cfg.WalletName, err)
	}
	if r.Warnings, err = Import(ctx, wallet, cfg, r.Prepared); err != nil {
		return r, err
	}
	if r.Landed, err = Confirm(ctx, wallet, cfg, r.Prepared); err != nil {
		return r, err
	}
	if r.Check, err = DeriveCheck(ctx, node, wallet, r.Landed, cfg.sampleSize()); err != nil {
		return r, err
	}
	return r, nil
}
