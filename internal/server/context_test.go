package server_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestOnlyTheDoctorScreenReadsTheRequestContext is decision 2's mechanical half,
// and it is the guard for the mistake no behavioural test in this package can
// catch.
//
// A handler that plumbs r.Context() into a run would pass every test here and
// then abort a batch on a laptop lid. The reason is worth restating: a request
// context is cancelled when the response ends, and a response ends on a reload,
// a closed tab, a lid and a Wi-Fi blip — none of which is evidence about a batch
// with n peers holding reservations against outpoints.
//
// There is exactly one place a request context legitimately is used, and the
// distinction is not the transport: the doctor screen runs the pre-flight, and a
// pre-flight has nothing to unwind. So this test finds every r.Context() in the
// package's non-test files and requires it to be in that one function. A run's
// context comes from Server.base instead, which is the process's.
func TestOnlyTheDoctorScreenReadsTheRequestContext(t *testing.T) {
	const allowed = "doctor"

	fset := token.NewFileSet()
	found := 0

	for _, path := range goFiles(t, ".") {
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			req := requestParam(fn)
			if req == "" {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Context" {
					return true
				}
				ident, ok := sel.X.(*ast.Ident)
				if !ok || ident.Name != req {
					return true
				}
				found++
				if fn.Name.Name != allowed {
					t.Errorf("%s: %s uses %s.Context().\n"+
						"  A request context is cancelled when the response ends, and a "+
						"response ends on a reload, a closed tab, a laptop lid and a "+
						"Wi-Fi blip. Decision 2 is that none of those is evidence about "+
						"a batch with peers holding reservations.\n"+
						"  The one legitimate use is the %s screen, because a pre-flight "+
						"has nothing to unwind. Everything else takes its context from "+
						"Server.base.",
						fset.Position(call.Pos()), fn.Name.Name, req, allowed)
				}
				return true
			})
		}
	}

	// A test that found nothing would pass for the wrong reason: it would also
	// pass on the day somebody renamed the parameter and this stopped matching.
	if found == 0 {
		t.Fatalf("no %s.Context() call was found anywhere in the package, so this "+
			"check is not measuring what it says. The %s screen is supposed to have "+
			"one.", "r", allowed)
	}
}

// requestParam is the name of the *http.Request parameter, or "".
func requestParam(fn *ast.FuncDecl) string {
	if fn.Type.Params == nil {
		return ""
	}
	for _, field := range fn.Type.Params.List {
		star, ok := field.Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		sel, ok := star.X.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Request" {
			continue
		}
		if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "http" {
			continue
		}
		if len(field.Names) > 0 {
			return field.Names[0].Name
		}
	}
	return ""
}
