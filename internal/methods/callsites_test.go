package methods_test

import (
	"fmt"
	"go/ast"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/AusDavo/winthistle/internal/methods"
	"golang.org/x/tools/go/packages"
)

// lndRPCPackage is the import path prefix every generated LND client interface
// lives under: lnrpc itself, plus one package per subserver.
const lndRPCPackage = "github.com/lightningnetwork/lnd/lnrpc"

// thisModule is this repo's module path.
const thisModule = "github.com/AusDavo/winthistle/"

// harnessSupportPackage is the one package in this module that talks to LND
// without being the app.
//
// It is excluded from the "must be InApp" rule, and the exclusion is not taken
// on trust: harnessOnlyImports below fails if any non-test file imports it, so
// the exemption cannot quietly become a hole for production code to hide in.
const harnessSupportPackage = "github.com/AusDavo/winthistle/internal/regtestenv"

// callSite is one place an LND RPC is invoked.
type callSite struct {
	Method     string // "/lnrpc.Lightning/AbandonChannel"
	Position   string // file:line, for the failure message
	Production bool   // not a _test.go file, and not the harness support package
}

// TestEveryLNDCallSiteIsRegistered is the check CLAUDE.md's "make drift
// impossible" asks for.
//
// It type-checks the whole module, finds every call on a generated
// lnrpc/walletrpc client interface, and requires the registry to list exactly
// those methods:
//
//   - a call with no entry fails, because the baked macaroon would be too narrow
//     and the app would break at whatever step that call sits on;
//   - an entry with no call fails, because the baked macaroon would be wider than
//     the code needs, and a permission granted for no reason is the thing the
//     uri-entity approach exists to avoid;
//   - a Use that disagrees with where the calls actually are fails, since Use is
//     what decides whether the operator's credential carries the method at all.
//
// It needs no harness and no network beyond the module cache, so it runs in
// `make check` and in `make test-unit`.
func TestEveryLNDCallSiteIsRegistered(t *testing.T) {
	root := repoRoot(t)

	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo | packages.NeedImports |
			packages.NeedDeps,
		// Test files are loaded because they are call sites too — they just are
		// not production ones. Seeing them is what lets the Use tag be checked
		// rather than believed.
		Tests: true,
		Dir:   root,
	}
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		t.Fatalf("loading the module: %v", err)
	}
	if n := packages.PrintErrors(pkgs); n > 0 {
		t.Fatalf("%d package(s) failed to load; the check cannot be trusted "+
			"until they do", n)
	}
	if len(pkgs) == 0 {
		t.Fatal("no packages loaded")
	}

	sites := findCallSites(t, pkgs, root)
	if len(sites) == 0 {
		t.Fatal("found no LND call sites at all — the detector is broken, " +
			"which would make this test pass for the wrong reason")
	}

	harnessOnlyImports(t, pkgs, root)

	// Group by method: what was called, and whether any caller was production.
	type usage struct {
		positions  []string
		production []string
	}
	found := map[string]*usage{}
	for _, s := range sites {
		u := found[s.Method]
		if u == nil {
			u = &usage{}
			found[s.Method] = u
		}
		u.positions = append(u.positions, s.Position)
		if s.Production {
			u.production = append(u.production, s.Position)
		}
	}

	registered := map[string]methods.Method{}
	for _, m := range methods.All() {
		registered[m.Name] = m
	}

	for _, name := range sortedKeys(found) {
		u := found[name]
		m, ok := registered[name]
		if !ok {
			t.Errorf("%s is called but not registered.\n"+
				"  called at: %s\n"+
				"  Add it to internal/methods/methods.go with its call site and "+
				"LND's own permission for it.", name, strings.Join(u.positions, ", "))
			continue
		}
		switch m.Use {
		case methods.InApp:
			if len(u.production) == 0 {
				t.Errorf("%s is registered InApp but every call to it is in a test "+
					"or in the harness support package.\n"+
					"  called at: %s\n"+
					"  Either the app stopped calling it — in which case the "+
					"operator's macaroon should stop carrying it — or the entry "+
					"belongs to InHarness.",
					name, strings.Join(u.positions, ", "))
			}
		case methods.InHarness:
			if len(u.production) > 0 {
				t.Errorf("%s is registered InHarness but the app calls it.\n"+
					"  production call site(s): %s\n"+
					"  The printed macaroon leaves InHarness methods out, so this "+
					"build would fail against its own credential.",
					name, strings.Join(u.production, ", "))
			}
		}
	}

	for _, m := range methods.All() {
		if _, ok := found[m.Name]; !ok {
			t.Errorf("%s is registered but nothing calls it.\n"+
				"  The printed macaroon would grant a permission this build has "+
				"no use for. Remove the entry, or add the call site it describes: %s",
				m.Name, m.Why)
		}
	}
}

// findCallSites walks every loaded file and reports each call made on a
// generated LND client interface.
//
// The detection is on types, not text, and it follows the repo's own idiom.
// internal/abort takes an lnrpc.LightningClient directly; internal/reserve takes
// a three-method interface of its own, so that a pre-flight cannot move a coin.
// Both are call sites, and only a type-aware check sees the second one: a
// narrow interface is recognised by asking which LND client satisfies it.
func findCallSites(t *testing.T, pkgs []*packages.Package, root string) []callSite {
	t.Helper()

	clients := lndClientInterfaces(pkgs)
	if len(clients) == 0 {
		t.Fatal("found no LND client interfaces in the dependency graph")
	}

	// Tests: true makes each package appear more than once (the package, its
	// test variant, the external test package), so the same file is walked
	// repeatedly. Dedupe on the exact position.
	seen := map[string]bool{}
	var out []callSite

	for _, pkg := range pkgs {
		if pkg.TypesInfo == nil {
			continue
		}
		harness := strings.HasPrefix(trimTestSuffix(pkg.PkgPath), harnessSupportPackage)

		for _, file := range pkg.Syntax {
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				selection := pkg.TypesInfo.Selections[sel]
				if selection == nil || selection.Kind() != types.MethodVal {
					return true
				}
				pos := pkg.Fset.Position(call.Pos())
				method, err := rpcMethodName(selection, clients)
				if err != nil {
					t.Errorf("%s: %v", shorten(root, pos.String()), err)
					return true
				}
				if method == "" {
					return true
				}

				key := method + "@" + pos.String()
				if seen[key] {
					return true
				}
				seen[key] = true

				isTest := strings.HasSuffix(pos.Filename, "_test.go")
				out = append(out, callSite{
					Method:     method,
					Position:   shorten(root, pos.String()),
					Production: !isTest && !harness,
				})
				return true
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Method != out[j].Method {
			return out[i].Method < out[j].Method
		}
		return out[i].Position < out[j].Position
	})
	return out
}

// lndClient is one generated service client interface, with the gRPC service its
// methods belong to.
type lndClient struct {
	service string // "lnrpc.Lightning"
	named   *types.Named
}

// lndClientInterfaces finds every generated LND service client in the dependency
// graph, deps included.
//
// Discovered rather than listed, so a call into a subserver this build has never
// touched — routerrpc, signrpc — is caught the first time it appears rather than
// the first time someone remembers to extend this test.
func lndClientInterfaces(roots []*packages.Package) []lndClient {
	var out []lndClient

	packages.Visit(roots, nil, func(pkg *packages.Package) {
		if pkg.Types == nil {
			return
		}
		path := pkg.Types.Path()
		if path != lndRPCPackage && !strings.HasPrefix(path, lndRPCPackage+"/") {
			return
		}
		scope := pkg.Types.Scope()
		for _, name := range scope.Names() {
			obj, ok := scope.Lookup(name).(*types.TypeName)
			if !ok || !obj.Exported() {
				continue
			}
			// <Service>Client is the client interface. The generated per-stream
			// interfaces are named <Service>_<Method>Client and their methods
			// (Recv, CloseSend) are not RPCs; the underscore separates them.
			if !strings.HasSuffix(name, "Client") || strings.Contains(name, "_") {
				continue
			}
			named, ok := obj.Type().(*types.Named)
			if !ok {
				continue
			}
			if iface, ok := named.Underlying().(*types.Interface); !ok ||
				iface.NumMethods() == 0 {

				continue
			}
			out = append(out, lndClient{
				service: pathBase(path) + "." + strings.TrimSuffix(name, "Client"),
				named:   named,
			})
		}
	})
	sort.Slice(out, func(i, j int) bool { return out[i].service < out[j].service })
	return out
}

// rpcMethodName turns a method call into a gRPC method path, or "" if the
// receiver has nothing to do with LND.
//
// Two ways a receiver can be an LND client. Directly — the type is one of the
// generated <Service>Client interfaces. Or indirectly — the type is a narrower
// interface declared here, of the kind this repo uses to stop a component doing
// more than its job, in which case the client that satisfies it says which
// service the method belongs to. Exactly one client must satisfy it; if more than
// one does, the method path is genuinely ambiguous and guessing would put the
// wrong permission in the operator's credential.
func rpcMethodName(sel *types.Selection, clients []lndClient) (string, error) {
	recv := deref(sel.Recv())
	name := sel.Obj().Name()

	// Direct: the receiver is a generated client interface.
	for _, c := range clients {
		if types.Identical(recv, c.named) {
			return "/" + c.service + "/" + name, nil
		}
	}

	// Indirect: the receiver is a local interface that an LND client satisfies.
	iface, ok := recv.Underlying().(*types.Interface)
	if !ok || iface.NumMethods() == 0 {
		return "", nil
	}
	var matched []lndClient
	for _, c := range clients {
		if types.Implements(c.named, iface) {
			matched = append(matched, c)
		}
	}
	switch len(matched) {
	case 0:
		return "", nil
	case 1:
		return "/" + matched[0].service + "/" + name, nil
	default:
		services := make([]string, 0, len(matched))
		for _, c := range matched {
			services = append(services, c.service)
		}
		return "", fmt.Errorf("%s is called on an interface satisfied by more than "+
			"one LND client (%s), so its method path cannot be determined. "+
			"Narrow the interface, or take the concrete client",
			name, strings.Join(services, ", "))
	}
}

// harnessOnlyImports is the guard on the harness exemption: internal/regtestenv
// is excused from the "must be InApp" rule only for as long as nothing but a
// test imports it.
func harnessOnlyImports(t *testing.T, pkgs []*packages.Package, root string) {
	t.Helper()

	for _, pkg := range pkgs {
		if !strings.HasPrefix(trimTestSuffix(pkg.PkgPath), thisModule) {
			continue
		}
		for _, file := range pkg.Syntax {
			if strings.HasSuffix(pkg.Fset.Position(file.Pos()).Filename, "_test.go") {
				continue
			}
			for _, imp := range file.Imports {
				p, err := strconv.Unquote(imp.Path.Value)
				if err != nil {
					continue
				}
				if p == harnessSupportPackage {
					t.Errorf("%s imports %s from a non-test file.\n"+
						"  The call-site check exempts that package from the "+
						"InApp rule because only tests use it. Production code "+
						"importing it turns the exemption into a blind spot.",
						shorten(root, pkg.Fset.Position(imp.Pos()).String()), p)
				}
			}
		}
	}
}

// pathBase is path.Base, spelled out so the loop above can keep using `path` for
// an import path without shadowing the package.
func pathBase(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

func deref(t types.Type) types.Type {
	if p, ok := t.(*types.Pointer); ok {
		return p.Elem()
	}
	return t
}

// trimTestSuffix strips the markers go/packages adds to test variants, so
// "…/internal/regtestenv [test]" and "…/internal/regtestenv_test" compare equal
// to the package itself.
func trimTestSuffix(pkgPath string) string {
	pkgPath = strings.TrimSuffix(pkgPath, ".test")
	if i := strings.Index(pkgPath, " ["); i >= 0 {
		pkgPath = pkgPath[:i]
	}
	return strings.TrimSuffix(pkgPath, "_test")
}

// shorten makes a position readable: repo-relative rather than absolute, so a
// failure message can be pasted straight into an editor.
func shorten(root, pos string) string {
	file, rest, ok := strings.Cut(pos, ":")
	if !ok {
		return pos
	}
	rel, err := filepath.Rel(root, file)
	if err != nil {
		return pos
	}
	return rel + ":" + rest
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s", dir)
		}
		dir = parent
	}
}
