package methods_test

import (
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/AusDavo/winthistle/internal/peers"
	"github.com/AusDavo/winthistle/internal/policy"
	"github.com/AusDavo/winthistle/internal/reserve"
	"github.com/AusDavo/winthistle/internal/settle"
	"golang.org/x/tools/go/packages"
)

// This file is the mechanical half of "re-cite before assuming".
//
// A version bump moves line numbers loudly — the citation says :2718 and the
// function is somewhere else, and a reader chasing it notices immediately. Two
// other things move silently, and both of them are what the bump to
// v0.21.2-beta actually broke. A symbol that was renamed: the fundee's
// open_channel handler was cited eight times, twice in copy an operator reads,
// under a name that has never existed at any version this build has pinned, and
// its real name is fundeeProcessOpenChannel. And a constant that was
// transcribed and then changed at the other end: policy.MinTimeLockDelta said
// 18 where LND's minimum is routing.MinCLTVDelta, 24 — which arms and publishes
// a batch whose whole policy pass LND then refuses.
//
// Neither needs an operator, a node or a sweep to find. The first test below
// catches the first class and the second the second, both against the module
// cache at the version go.mod pins, so the next bump fails the build rather
// than waiting to be read.
//
// No dead name is written down anywhere in this file. An allowlist entry for one
// would be a hole in the check aimed at exactly the name that motivated it; the
// account of what was wrong belongs in the commit that fixed it.

// citationDirs are the trees whose comments and printed strings are scanned.
var citationDirs = []string{"internal", "cmd", "regtest"}

// thisFile is the one file whose comments and strings are not scanned.
//
// It holds citationExceptions, whose keys and reasons are a list of names that
// resolve nowhere — that is what the list is — so scanning it would report every
// entry as a finding. Skipping one named file is a visible, bounded hole;
// excusing those names globally would not be, because an entry in the list
// excuses that name everywhere in the tree. Its declarations still count as this
// module's, so a comment elsewhere may name the tests below.
const thisFile = "citations_test.go"

// citedModulePrefixes are the module paths whose source is the authority on
// whether a cited symbol exists.
//
// LND and the btcsuite tree first, because those are the codebases this
// repository makes claims about. grpc and the macaroon bakery are here because
// the two things this build hands LND — a connection and a credential — are
// theirs, and their symbols are cited in the same breath as LND's. Everything
// else a comment might name — a stdlib call, a Bitcoin Core RPC argument, a
// Sparrow class — is absorbed by the ownership rule below or is on the
// allowlist.
var citedModulePrefixes = []string{
	"github.com/lightningnetwork/lnd",
	"github.com/btcsuite/",
	"google.golang.org/grpc",
	"gopkg.in/macaroon",
}

// citationExceptions are the identifier-shaped words this tree writes down that
// resolve nowhere, with the reason each one is not a defect.
//
// Two kinds, and the distinction is the whole discipline of this list. Words
// that are not Go symbols at all — a Bitcoin Core RPC argument, a Java class
// from Sparrow, a fragment of somebody else's error text, prose that happens to
// be camel-cased. And symbols that genuinely existed and genuinely do not any
// more, named on purpose in a sentence that records their removal.
//
// Do not add a name here to quiet a real miss. The question is whether the word
// is a symbol that is supposed to exist. If it is, the citation is wrong and
// correcting the citation is the point of the check. If a dead name is still
// load-bearing enough to write down, it belongs in the second group with what
// removed it — and never a name that could plausibly be reintroduced, because an
// entry here is a hole in the check for exactly that name.
var citationExceptions = map[string]string{
	// Not a Go symbol: Bitcoin Core RPC names, RPC argument names and
	// descriptor syntax. The harness dials Core and no Core source is in the
	// module graph.
	"lockUnspents":      "bitcoind argument to walletcreatefundedpsbt",
	"includeWatching":   "bitcoind argument to walletcreatefundedpsbt",
	"scriptPubKey":      "bitcoind field, and the Bitcoin term",
	"scriptSig":         "the Bitcoin term",
	"nLockTime":         "the Bitcoin transaction field",
	"nSequence":         "the Bitcoin transaction field",
	"nVersion":          "the Bitcoin transaction field",
	"MMmmrr00":          "a descriptor checksum placeholder",
	"testmempoolaccept": "a bitcoind RPC name",
	"submitpackage":     "a bitcoind RPC name",
	"walletprocesspsbt": "a bitcoind RPC name",
	"combinepsbt":       "a bitcoind RPC name",
	"sortedmulti":       "descriptor syntax",

	// Not a Go symbol: Sparrow, which is Java and is not vendored anywhere.
	// Cited in readWhole's measurement of how Sparrow writes a file, and in the
	// CSV reasoning.
	"FileOutputStream":      "a Java class, on Sparrow's save path",
	"OutputStreamWriter":    "a Java class, on Sparrow's save path",
	"NumberFormatException": "a Java class, from Sparrow's CSV reader",
	"parseLong":             "a Java method, from Sparrow's CSV reader",
	"SendToManyDialog":      "a Sparrow class",

	// Not a Go symbol: prose, abbreviations, and fragments of encodings that
	// survive the identifier regex.
	"gRPC":            "the protocol",
	"RPCs":            "the plural of RPC",
	"PSBTs":           "the plural of PSBT",
	"UTXOs":           "the plural of UTXO",
	"SQLite":          "the database",
	"SegWit":          "the soft fork",
	"TxID":            "the abbreviation, prose spelling",
	"CompactSize":     "the Bitcoin serialization term",
	"mSAT":            "LND's own spelling of the unit, in its error text",
	"cHNidP8":         "the base64 prefix of a PSBT's magic bytes",
	"aGVsbG8gd29ybGQ": "base64 in a test fixture",

	// Not a Go symbol: somebody else's text or somebody else's example.
	"pendingChannelID": "a fragment of LND's own error text, " +
		"\"no funding intent found for pendingChannelID(%x)\"",
	"expectedOutput": "an invented variable in an illustrative snippet in " +
		"internal/plan's package doc",
	"FormatFloat": "strconv.FormatFloat, named in a comment about what LND's " +
		"amount formatting does, and not called here",

	// Removed, and named on purpose by the sentence that records the removal.
	"FeeTooLow":       "a plan.Code, removed with the declared fee rate by issue #2",
	"FeeTooHigh":      "a plan.Code, removed with the declared fee rate by issue #2",
	"ChangeTooSmall":  "a plan.Code, removed with ChangeFloor by issue #2",
	"FundingOutputs":  "an arm.Streams field, moved into internal/regtestenv by item 4",
	"SignWithMiner":   "a harness helper the multisig cold wallet replaced",
	"TeardownBudget":  "a run constant retired when abort took its own deadline",
	"releaseFence":    "a run helper deleted with Bitcoin Core's coin locks in item 5",
	"tooNarrow":       "doctor's predicate, renamed refusedOverMacaroon by issue #13",
	"SendPaymentSync": "an lnrpc method LND removed at the v0.21.2-beta bump",
	"SendToRouteSync": "an lnrpc method LND removed at the v0.21.2-beta bump",
	"TestARejectedWalletStopsTheRunBeforeAnythingIsAsked": "a test deleted with " +
		"the cold-wallet gate it was about",
}

var (
	identRe     = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)
	camelHumpRe = regexp.MustCompile(`[a-z][A-Z]`)
	acronymRe   = regexp.MustCompile(`[A-Z]{2,}[a-z]`)
)

// identifierShaped reports whether a word in prose could be a Go identifier
// somebody meant as a citation.
//
// The test is a case transition inside the word, which is what separates a
// camel-cased symbol from an English one. It is deliberately blind to
// single-word identifiers (Verify, Recv, close): those are indistinguishable
// from prose, and a check that flagged them would need an allowlist longer than
// the thing it was checking. The renamed handler this check was written for has
// a case transition, which is the bar it has to clear.
func identifierShaped(w string) bool {
	if len(w) < 4 || strings.Contains(w, "_") {
		return false
	}
	return camelHumpRe.MatchString(w) || acronymRe.MatchString(w)
}

// citation is one place a name is written down.
type citation struct {
	Pos     string
	Printed bool // in a string literal rather than a comment
}

// TestEveryCitedLNDSymbolResolves fails when a comment or a printed sentence
// names an LND symbol that does not exist at the pinned version.
//
// Three sets. What this module *writes* — every identifier in its own syntax
// trees, which covers both its own declarations and every symbol it calls,
// stdlib and grpc included. What the cited modules *declare*, read out of the
// module cache. And what a comment or a string literal *says*. A word in the
// third set that is in neither of the first two is either a wrong citation or an
// entry for citationExceptions, and there is no third option.
//
// String literals are scanned as well as comments, because two of the renamed
// handler's eight sites were sentences printed to an operator. A citation the
// operator reads is worth more than one only a maintainer reads.
//
// It needs no harness and no network beyond the module cache.
func TestEveryCitedLNDSymbolResolves(t *testing.T) {
	root := repoRoot(t)

	ours, cited := scanThisModule(t, root)
	if len(cited) == 0 {
		t.Fatal("found no identifier-shaped words in any comment or string — " +
			"the scanner is broken, which would make this test pass for the " +
			"wrong reason")
	}
	resolved := declaredInCitedModules(t, root)
	if !resolved["funderProcessFundingSigned"] {
		t.Fatal("the module-cache index does not contain " +
			"funderProcessFundingSigned, which I-1 cites and which does exist — " +
			"the index is broken, and a broken index resolves nothing and " +
			"fails everything")
	}

	var names []string
	for name := range cited {
		if ours[name] || resolved[name] {
			continue
		}
		if _, ok := citationExceptions[name]; ok {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		where := cited[name]
		sort.Slice(where, func(i, j int) bool { return where[i].Pos < where[j].Pos })
		lines := make([]string, 0, len(where))
		printed := 0
		for _, c := range where {
			lines = append(lines, c.Pos)
			if c.Printed {
				printed++
			}
		}
		note := ""
		if printed > 0 {
			note = "\n  " + printedNote(printed, len(where))
		}
		t.Errorf("%s is cited but declares nothing in the module cache at the "+
			"pinned version.\n"+
			"  cited at: %s%s\n"+
			"  Either the symbol was renamed — find what it is called now and "+
			"re-read the function while renaming it, because a rename is where "+
			"an ordering claim goes stale — or it never existed or no longer "+
			"does, in which case add it to citationExceptions in this file with "+
			"the reason.",
			name, strings.Join(lines, ", "), note)
	}
}

// scanThisModule reads every .go file under citationDirs once, and returns what
// this module names in code and what it names in prose.
//
// The two come off the same parse deliberately. "Cited but not ours" is the
// question, and answering it from two different walks is how the two would come
// to disagree about which files are in scope.
func scanThisModule(t *testing.T, root string) (ours map[string]bool, cited map[string][]citation) {
	t.Helper()

	ours = map[string]bool{}
	cited = map[string][]citation{}

	add := func(fset *token.FileSet, pos token.Pos, text string, printed bool) {
		for _, w := range identRe.FindAllString(text, -1) {
			if !identifierShaped(w) {
				continue
			}
			p := fset.Position(pos)
			rel, err := filepath.Rel(root, p.Filename)
			if err != nil {
				rel = p.Filename
			}
			cited[w] = append(cited[w], citation{
				Pos:     rel + ":" + strconv.Itoa(p.Line),
				Printed: printed,
			})
		}
	}

	for _, dir := range citationDirs {
		base := filepath.Join(root, dir)
		if _, err := os.Stat(base); err != nil {
			continue
		}
		err := filepath.Walk(base, func(path string, fi os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if fi.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
			if err != nil {
				t.Errorf("parsing %s: %v", path, err)
				return nil
			}
			// This file declares the two tests other packages point at, so its
			// identifiers count; what it says does not.
			scan := filepath.Base(path) != thisFile

			ast.Inspect(f, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.ImportSpec:
					// An import path is a string literal, and every one of them
					// carries this module's own path. Skipped whole rather than
					// filtered afterwards, so the exclusion is where the reason
					// for it is.
					return false
				case *ast.Ident:
					// Everything this module writes as code: its own
					// declarations, and every symbol it selects from any
					// package. A comment naming something the code also names
					// is a citation the compiler is already checking.
					ours[n.Name] = true
				case *ast.BasicLit:
					if n.Kind != token.STRING {
						return true
					}
					s, err := strconv.Unquote(n.Value)
					if err != nil {
						// A malformed literal cannot compile, so this is
						// unreachable; scan the raw form rather than dropping
						// it silently.
						s = n.Value
					}
					if !scan {
						return true
					}
					if !strings.Contains(s, " ") {
						// A string with no space in it is a name, a path or a
						// key, not a sentence — the module path constant, the
						// gRPC method paths, a SQL column. Those are the
						// program's own vocabulary rather than a claim about
						// somebody else's code, and scanning them says only
						// that this repository is called winthistle.
						return true
					}
					add(fset, n.Pos(), s, true)
				}
				return true
			})
			if scan {
				for _, group := range f.Comments {
					for _, c := range group.List {
						add(fset, c.Pos(), c.Text, false)
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", base, err)
		}
	}
	return ours, cited
}

var (
	// declRe matches the declaration forms the prompt for this check names:
	// func (including methods), type, const, var, and the `Name =` /
	// `Name struct` / `Name interface` shapes.
	declRe = regexp.MustCompile(
		`^\s*(?:func\s+(?:\([^)]*\)\s*)?([A-Za-z_]\w*)` +
			`|type\s+([A-Za-z_]\w*)` +
			`|(?:const|var)\s+([A-Za-z_]\w*)` +
			`|([A-Za-z_]\w*)\s*(?::=)` +
			`|([A-Za-z_]\w*)\s+(?:=|struct\b|interface\b))`)

	// fieldRe matches a struct field or a const-block member: one indent, a
	// name, and then either something or nothing — an iota block's second and
	// later members are a name on a line by themselves, which is how
	// chain.ErrMempoolConflict is declared. Loose on purpose: a false positive
	// here only means a word resolves that need not have, while a miss is a
	// failing build on a citation that was correct.
	fieldRe = regexp.MustCompile(`^\t([A-Za-z_]\w*)(?:[\s=]|$)`)

	protoFieldRe = regexp.MustCompile(
		`(?m)^\s*(?:repeated\s+|optional\s+)?[\w.<>, ]+\s+(\w+)\s*=\s*\d+\s*;`)
	protoTypeRe = regexp.MustCompile(`(?m)^\s*(?:message|service|enum|rpc)\s+(\w+)`)
)

// declaredInCitedModules indexes every name the cited modules declare, at the
// versions go.mod pins.
//
// The module cache is read rather than the modules imported, for two reasons.
// Unexported symbols are most of what this repository cites —
// enforceNewReservedValue, rebroadcastFundingTx, pruneZombieReservations — and
// export data does not carry them. And .proto field names are not Go symbols at
// all, while chan_pending and funding_expiry_blocks are cited as often as
// anything here.
func declaredInCitedModules(t *testing.T, root string) map[string]bool {
	t.Helper()

	out := map[string]bool{}
	roots := citedModuleRoots(t, root)
	if len(roots) == 0 {
		t.Fatal("no cited module found in the module cache; run `go mod download`")
	}

	for _, dir := range roots {
		err := filepath.Walk(dir, func(path string, fi os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if fi.IsDir() {
				if fi.Name() == ".git" {
					return filepath.SkipDir
				}
				return nil
			}
			switch {
			case strings.HasSuffix(path, ".go"):
				body, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				for _, line := range strings.Split(string(body), "\n") {
					if strings.HasPrefix(strings.TrimSpace(line), "//") {
						continue
					}
					if m := declRe.FindStringSubmatch(line); m != nil {
						for _, g := range m[1:] {
							if g != "" {
								out[g] = true
							}
						}
					}
					if m := fieldRe.FindStringSubmatch(line); m != nil {
						out[m[1]] = true
					}
				}
			case strings.HasSuffix(path, ".proto"):
				body, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				for _, m := range protoFieldRe.FindAllStringSubmatch(string(body), -1) {
					out[m[1]] = true
				}
				for _, m := range protoTypeRe.FindAllStringSubmatch(string(body), -1) {
					out[m[1]] = true
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", dir, err)
		}
	}
	return out
}

// citedModuleRoots resolves go.mod's pinned versions to directories in the
// module cache.
//
// go.mod is parsed rather than `go list` asked, because the pin is the point:
// this check must read the version the build is pinned to and fail if that
// version is not downloaded, not quietly resolve something else.
func citedModuleRoots(t *testing.T, root string) []string {
	t.Helper()

	body, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("reading go.mod: %v", err)
	}
	cache := goEnv(t, "GOMODCACHE")

	var out []string
	line := regexp.MustCompile(`^\s+(\S+) (v\S+)`)
	for _, l := range strings.Split(string(body), "\n") {
		m := line.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		path, version := m[1], m[2]
		wanted := false
		for _, prefix := range citedModulePrefixes {
			if strings.HasPrefix(path, prefix) {
				wanted = true
			}
		}
		if !wanted {
			continue
		}
		dir := filepath.Join(cache, filepath.FromSlash(path)+"@"+version)
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			t.Errorf("%s %s is required but not in the module cache at %s.\n"+
				"  Run `go mod download`. Until it is there this check cannot "+
				"tell a renamed symbol from an unread one.", path, version, dir)
			continue
		}
		out = append(out, dir)
	}
	return out
}

// transcribed is one number this build copied out of LND.
type transcribed struct {
	Ours  string // "policy.MinTimeLockDelta", for the failure message
	Value int64  // what this build says
	Pkg   string // the LND package that owns it
	Sym   string // the constant there, exported or not
	Why   string // what goes wrong when they disagree
}

// transcribedConstants is the table.
//
// Every exported numeric constant in internal/ whose comment names an LND symbol
// is here. The three that are not — settle.HorizonWarn, settle.HorizonUrgent and
// prose's widths — are this program's own choices and have nothing to match.
var transcribedConstants = []transcribed{{
	Ours: "policy.MinTimeLockDelta", Value: policy.MinTimeLockDelta,
	Pkg: "github.com/lightningnetwork/lnd/routing", Sym: "MinCLTVDelta",
	Why: "Validate() would arm and publish a batch whose CLTV delta " +
		"UpdateChannelPolicy then refuses for every channel at once, leaving " +
		"all of them at LND's 1000 msat / 1 ppm defaults",
}, {
	Ours: "policy.MaxTimeLockDelta", Value: policy.MaxTimeLockDelta,
	Pkg: "github.com/lightningnetwork/lnd/routing", Sym: "MaxCLTVDelta",
	Why: "same as the minimum, at the other end",
}, {
	Ours: "policy.DefaultBaseFeeMsat", Value: policy.DefaultBaseFeeMsat,
	Pkg: "github.com/lightningnetwork/lnd/chainreg", Sym: "DefaultBitcoinBaseFeeMSat",
	Why: "IsLNDDefault would stop recognising the fee-drain default it exists " +
		"to warn about",
}, {
	Ours: "policy.DefaultFeeRatePPM", Value: policy.DefaultFeeRatePPM,
	Pkg: "github.com/lightningnetwork/lnd/chainreg", Sym: "DefaultBitcoinFeeRate",
	Why: "as above",
}, {
	Ours: "policy.DefaultTimeLockDelta", Value: policy.DefaultTimeLockDelta,
	Pkg: "github.com/lightningnetwork/lnd/chainreg", Sym: "DefaultBitcoinTimeLockDelta",
	Why: "as above",
}, {
	Ours: "settle.ForgetHorizonBlocks", Value: settle.ForgetHorizonBlocks,
	Pkg: "github.com/lightningnetwork/lnd/lncfg", Sym: "DefaultMaxWaitNumBlocksFundingConf",
	Why: "clock B is counted in blocks from this number, and the recovery " +
		"screens name the block a stock peer gives up at",
}, {
	Ours: "settle.MinDepth", Value: settle.MinDepth,
	Pkg: "github.com/lightningnetwork/lnd/lnwallet", Sym: "minRequiredConfs",
	Why: "ExpectedDepth clamps against it, and the plan document shows the " +
		"result as what a stock peer will wait for",
}, {
	Ours: "settle.MaxDepth", Value: settle.MaxDepth,
	Pkg: "github.com/lightningnetwork/lnd/lnwallet", Sym: "maxRequiredConfs",
	Why: "as above",
}, {
	Ours: "settle.MaxFundingAmount", Value: settle.MaxFundingAmount,
	Pkg: "github.com/lightningnetwork/lnd/lnwallet", Sym: "maxChannelSize",
	Why: "ExpectedDepth scales against it",
}, {
	Ours: "settle.MaxFundingAmount", Value: settle.MaxFundingAmount,
	Pkg: "github.com/lightningnetwork/lnd/funding", Sym: "MaxBtcFundingAmount",
	Why: "the wumbo threshold, which is the same number by LND's own comment " +
		"and is checked twice here because the two could drift apart",
}, {
	Ours: "reserve.PerAnchorChannel", Value: reserve.PerAnchorChannel,
	Pkg: "github.com/lightningnetwork/lnd/lnwallet", Sym: "AnchorChanReservedValue",
	Why: "the figure the reserve report explains to the operator",
}, {
	Ours: "reserve.MaxReserve", Value: reserve.MaxReserve,
	Pkg: "github.com/lightningnetwork/lnd/lnwallet", Sym: "MaxAnchorChanReservedValue",
	Why: "as above",
}, {
	Ours: "peers.ReservationTimeout", Value: int64(peers.ReservationTimeout),
	Pkg: "github.com/lightningnetwork/lnd/lnwallet/chanfunding", Sym: "DefaultReservationTimeout",
	Why: "clock A's length, and half of HoldUpperBound — which is how long a " +
		"probe is told it costs a peer",
}, {
	Ours: "peers.SweeperInterval", Value: int64(peers.SweeperInterval),
	Pkg: "github.com/lightningnetwork/lnd/lncfg", Sym: "DefaultZombieSweeperInterval",
	Why: "the other half of HoldUpperBound",
}}

// TestTranscribedConstantsMatchLND fails when a number this build copied out of
// LND no longer matches the LND it is pinned to.
//
// The values are read out of the pinned source with go/types rather than
// imported, because half the table is unexported — lnwallet's confirmation
// bounds are package-private constants inside ScaleNumConfs — and export data
// does not carry those. Loading the six packages from source costs about a
// second.
//
// The direction of the check matters. A mismatch is not automatically a bug in
// this build: peers.ReservationTimeout deliberately describes the *peer's*
// build, and a peer that is not LND is bound by none of it. But every one of
// these was transcribed from the LND in go.mod, so a disagreement means either
// the transcription was wrong or LND changed it, and both want a human.
func TestTranscribedConstantsMatchLND(t *testing.T) {
	wanted := map[string]bool{}
	for _, c := range transcribedConstants {
		wanted[c.Pkg] = true
	}
	var paths []string
	for p := range wanted {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo,
		Dir: repoRoot(t),
	}
	pkgs, err := packages.Load(cfg, paths...)
	if err != nil {
		t.Fatalf("loading LND's packages: %v", err)
	}
	byPath := map[string]*packages.Package{}
	for _, p := range pkgs {
		if p.Types == nil {
			t.Fatalf("%s type-checked to nothing: %v", p.PkgPath, p.Errors)
		}
		byPath[p.PkgPath] = p
	}

	for _, c := range transcribedConstants {
		pkg := byPath[c.Pkg]
		if pkg == nil {
			t.Errorf("%s cites %s.%s, and that package did not load",
				c.Ours, c.Pkg, c.Sym)
			continue
		}
		obj := pkg.Types.Scope().Lookup(c.Sym)
		if obj == nil {
			t.Errorf("%s cites %s.%s, which does not exist at the pinned "+
				"version.\n"+
				"  Find what it is called now, or what replaced it, and "+
				"re-read the reasoning around %s while you are there.",
				c.Ours, c.Pkg, c.Sym, c.Ours)
			continue
		}
		konst, ok := obj.(*types.Const)
		if !ok {
			t.Errorf("%s cites %s.%s, which is no longer a constant (%T)",
				c.Ours, c.Pkg, c.Sym, obj)
			continue
		}
		got, exact := constant.Int64Val(constant.ToInt(konst.Val()))
		if !exact {
			t.Errorf("%s.%s is not an integer this check can compare (%s)",
				c.Pkg, c.Sym, konst.Val())
			continue
		}
		if got != c.Value {
			t.Errorf("%s is %d; %s.%s is %d at the pinned version.\n"+
				"  What it costs: %s.\n"+
				"  Decide which number is right, change it, and change the "+
				"copy that quotes it in the same commit.",
				c.Ours, c.Value, c.Pkg, c.Sym, got, c.Why)
		}
	}
}

// goEnv asks the toolchain rather than reading the environment, because
// GOMODCACHE is usually unset and derived from GOPATH.
func goEnv(t *testing.T, key string) string {
	t.Helper()

	out, err := exec.Command("go", "env", key).Output()
	if err != nil {
		t.Fatalf("go env %s: %v", key, err)
	}
	v := strings.TrimSpace(string(out))
	if v == "" {
		t.Fatalf("go env %s is empty", key)
	}
	return v
}

// printedNote says how many of a name's sites the operator reads, because a
// wrong citation on a terminal costs more than a wrong one in a comment.
func printedNote(printed, total int) string {
	switch {
	case printed == total && total == 1:
		return "That one is a string literal, so the operator reads it."
	case printed == total:
		return "All of those are string literals, so the operator reads them."
	case printed == 1:
		return "One of those is a string literal, so the operator reads it."
	default:
		return strconv.Itoa(printed) + " of those are string literals, so the " +
			"operator reads them."
	}
}
