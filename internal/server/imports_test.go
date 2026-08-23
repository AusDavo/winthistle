package server_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/AusDavo/winthistle/internal/methods"
)

// publish is the method whose call-site count is the safety property.
const publish = "/walletrpc.WalletKit/PublishTransaction"

// TestTheServerCannotReachAPublishCallOrWriteTheJournal is decision 1's guard,
// and it is the second of two locks rather than the first.
//
// The first lock is arithmetic and it already covers this package:
// methods.Method.CallSites pins WalletKit.PublishTransaction at two production
// call sites, and internal/methods' TestEveryLNDCallSiteIsRegistered type-checks
// the whole module — this package included — and fails on a third. A web handler
// is exactly where a third appears, which is why that check is the one that has
// to be mechanical.
//
// This is the second lock, and it is about what a handler can hold rather than
// what it can call. internal/server may not import internal/arm or
// internal/bump, so no function in this package can be handed an *arm.Armed or a
// *bump.Signed — the two types whose unexported raw-transaction fields are each
// filled by exactly one constructor, after that constructor's own checks. A
// handler cannot name the arguments, so it cannot make the call even by
// accident, and run.Do stays the only route through the armed window.
//
// # internal/journal joined the list when the seams did
//
// The two arm/bump bans are about reaching the network. The third is about the
// record, and it was added with the four callback seams because one of them is
// setup.Ask — whose whole shape exists so that a comparison nobody made is never
// written down as a verdict. NotAnswered must not reach the setups table, and the
// enforcement is layered: internal/webrun's adapter maps every non-answer to
// NotAnswered, setup.Do returns before RecordSetup on NotAnswered, and this ban
// means no handler can write that table at all — it cannot name journal.Setup or
// call RecordSetup, because it cannot import the package they are in.
//
// The same ban is what makes the abort control's refusal honest. Whether a run
// reached the publish call is journal.Run.AbortTarget's answer, and a server that
// could read the journal would sooner or later hold a second copy of that rule.
// It cannot, so it asks — Launcher.AbortRefusal — and the answer comes from the
// same function run.RecoverOne refuses on.
//
// # What the list does not do, because the reasoning matters more than the list
//
// None of the three bans makes the banned code unreachable at run time, and none
// of them is meant to. internal/server imports internal/doctor, which imports
// internal/journal transitively; banning walletrpc would not help either, since
// an import of internal/lnd yields an *lnd.Client whose WalletKit field can be
// selected without naming walletrpc at all. What a ban removes is the ability to
// *name* a type or call a function, which is what stops a handler from being
// handed the argument. The reachability half is the count's job, and adding
// walletrpc here would make this test look like a boundary it is not.
func TestTheServerCannotReachAPublishCallOrWriteTheJournal(t *testing.T) {
	// The count, restated where the risk is, so a change to it is read by
	// somebody working on the UI rather than only by somebody working on the
	// registry.
	m, ok := methods.Lookup(publish)
	if !ok {
		t.Fatalf("%s is not in the registry", publish)
	}
	if m.CallSites != 2 {
		t.Fatalf("%s is pinned at %d production call sites, not 2.\n"+
			"  This package's guard is written on the assumption that the two are "+
			"arm.Publish and bump.Publish. If a third has been added deliberately, "+
			"say where it is and what gate it sits behind — in CLAUDE.md's "+
			"rejected list, in docs/design.html, and here.", publish, m.CallSites)
	}

	banned := map[string]string{
		"github.com/AusDavo/winthistle/internal/arm": "arm.Publish is the funding " +
			"transaction's only broadcast, behind the I-1 gate. A handler that can " +
			"name an *arm.Armed is a second route to it",
		"github.com/AusDavo/winthistle/internal/bump": "bump.Publish is the CPFP " +
			"child's broadcast. A handler that can name a *bump.Signed is a second " +
			"route to it",
		"github.com/AusDavo/winthistle/internal/journal": "the journal is the " +
			"record, and setup.Ask's third answer exists so that a comparison nobody " +
			"made is never recorded as a verdict. A handler that can name " +
			"journal.Setup can write NotAnswered into the setups table. It also " +
			"cannot be allowed a second copy of AbortTarget's rule about a run that " +
			"reached the publish call — it asks Launcher.AbortRefusal instead",
	}

	fset := token.NewFileSet()
	for _, path := range goFiles(t, ".") {
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		for _, imp := range file.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				continue
			}
			if why, bad := banned[p]; bad {
				t.Errorf("%s imports %s.\n  %s.\n"+
					"  What the server may do is start run.Do and answer its "+
					"questions. Publishing stays inside the sequence that earned it.",
					path, p, why)
			}
		}
	}
}

// goFiles is every non-test .go file in the package directory.
//
// Test files are excluded because this file is one: the ban is on what the
// server ships, and a regtest test that drives a whole run legitimately needs
// the packages a handler must not have.
func goFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") ||
			strings.HasSuffix(name, "_test.go") {

			continue
		}
		out = append(out, filepath.Join(dir, name))
	}
	if len(out) == 0 {
		t.Fatal("no non-test .go files found; this check would pass for the " +
			"wrong reason")
	}
	return out
}
