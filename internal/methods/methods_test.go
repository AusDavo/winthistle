package methods_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/AusDavo/winthistle/internal/methods"
	"google.golang.org/grpc"
)

func TestRegistryIsWellFormed(t *testing.T) {
	all := methods.All()
	if len(all) == 0 {
		t.Fatal("the registry is empty")
	}
	for i := 1; i < len(all); i++ {
		if all[i-1].Name >= all[i].Name {
			t.Errorf("All() is not sorted: %s before %s", all[i-1].Name, all[i].Name)
		}
	}
	for _, m := range all {
		if got, want := m.URI().Entity, "uri"; got != want {
			t.Errorf("%s bakes entity %q, want %q", m.Name, got, want)
		}
		if m.URI().Action != m.Name {
			t.Errorf("%s bakes action %q, want the method path", m.Name, m.URI().Action)
		}
		if _, ok := methods.Lookup(m.Name); !ok {
			t.Errorf("%s is in All() but Lookup cannot find it", m.Name)
		}
	}
}

// TestServiceAndShort pins the parsing the bake command and the guard both rely
// on. A method path that split wrongly would produce a credential LND rejects.
func TestServiceAndShort(t *testing.T) {
	for _, m := range methods.All() {
		if !strings.HasPrefix(m.Name, "/"+m.Service()+"/") {
			t.Errorf("%s: service %q does not prefix it", m.Name, m.Service())
		}
		if !strings.HasSuffix(m.Name, "/"+m.Short()) {
			t.Errorf("%s: short name %q does not end it", m.Name, m.Short())
		}
	}
}

// TestBakeCommandCarriesEveryAppMethodAndNoHarnessOne is the property that makes
// the printed command trustworthy: it is the app's methods, all of them, and
// nothing else.
func TestBakeCommandCarriesEveryAppMethodAndNoHarnessOne(t *testing.T) {
	cmd := methods.BakeCommand("")

	if !strings.HasPrefix(cmd, "lncli bakemacaroon --save_to "+methods.DefaultMacaroonFile) {
		t.Errorf("command does not start with the bake invocation:\n%s", cmd)
	}
	for _, m := range methods.All() {
		perm := "uri:" + m.Name
		present := strings.Contains(cmd, perm)
		switch m.Use {
		case methods.InApp:
			if !present {
				t.Errorf("%s is InApp but the command omits %s", m.Name, perm)
			}
		case methods.InHarness:
			if present {
				t.Errorf("%s is InHarness but the command grants %s — the "+
					"operator's credential would carry a fixture's permission",
					m.Name, perm)
			}
		}
	}

	// Every continuation is a real one: a line ending without a backslash in the
	// middle would silently truncate the permission list when pasted.
	lines := strings.Split(cmd, "\n")
	for i, line := range lines[:len(lines)-1] {
		if !strings.HasSuffix(line, `\`) {
			t.Errorf("line %d does not continue: %q", i+1, line)
		}
	}
	if strings.HasSuffix(lines[len(lines)-1], `\`) {
		t.Errorf("the command ends in a continuation: %q", lines[len(lines)-1])
	}
}

// TestBakeCommandGrantsNothingDangerous checks the never-list from the outside:
// the registry cannot carry one of these (validate refuses it), so the printed
// command cannot either, and this is the test that says so where an operator
// would look for it.
func TestBakeCommandGrantsNothingDangerous(t *testing.T) {
	cmd := methods.BakeCommand("")
	for _, c := range methods.Forbidden() {
		if strings.Contains(cmd, c.Method) {
			t.Errorf("the baked credential would let it %s, via %s", c.Does, c.Method)
		}
	}
	// Coarse entities are what the uri form exists to avoid. None should appear.
	for _, coarse := range []string{"onchain:write", "offchain:write", "onchain:read", "offchain:read"} {
		if strings.Contains(cmd, coarse) {
			t.Errorf("the command grants the coarse permission %s", coarse)
		}
	}
}

func TestExplainMentionsEveryMethod(t *testing.T) {
	var buf bytes.Buffer
	if err := methods.Explain(&buf); err != nil {
		t.Fatalf("Explain: %v", err)
	}
	out := buf.String()
	for _, m := range methods.All() {
		if !strings.Contains(out, m.Short()) {
			t.Errorf("the explanation never mentions %s", m.Name)
		}
	}
	if !strings.Contains(out, "admin.macaroon") {
		t.Error("the explanation does not warn against admin.macaroon")
	}
}

// TestGuardRefusesAnUnregisteredMethod covers the runtime half. The interceptor
// must refuse before the invoker runs — a call that reaches the wire has already
// spent the credential.
func TestGuardRefusesAnUnregisteredMethod(t *testing.T) {
	guard := methods.UnaryGuard()

	invoked := false
	invoker := func(context.Context, string, any, any, *grpc.ClientConn, ...grpc.CallOption) error {
		invoked = true
		return nil
	}

	err := guard(context.Background(), "/lnrpc.Lightning/SendCoins", nil, nil, nil, invoker)
	if !errors.Is(err, methods.ErrUnregistered) {
		t.Fatalf("guard allowed SendCoins: %v", err)
	}
	if invoked {
		t.Error("the invoker ran anyway")
	}
	if !strings.Contains(err.Error(), "/lnrpc.Lightning/SendCoins") {
		t.Errorf("the error does not name the method: %v", err)
	}
	// SendCoins is on the never-list, so the refusal should say so rather than
	// inviting whoever hit it to add a registry entry.
	if !strings.Contains(err.Error(), "never-list") {
		t.Errorf("the error does not mention the never-list: %v", err)
	}

	// A registered method must pass through untouched.
	invoked = false
	if err := guard(context.Background(), "/lnrpc.Lightning/GetInfo", nil, nil, nil, invoker); err != nil {
		t.Fatalf("guard refused GetInfo: %v", err)
	}
	if !invoked {
		t.Error("the invoker did not run for a registered method")
	}
}

func TestStreamGuardRefusesAnUnregisteredMethod(t *testing.T) {
	guard := methods.StreamGuard()

	streamed := false
	streamer := func(context.Context, *grpc.StreamDesc, *grpc.ClientConn, string,
		...grpc.CallOption) (grpc.ClientStream, error) {

		streamed = true
		return nil, nil
	}

	_, err := guard(context.Background(), &grpc.StreamDesc{}, nil,
		"/lnrpc.Lightning/SubscribeInvoices", streamer)
	if !errors.Is(err, methods.ErrUnregistered) {
		t.Fatalf("stream guard allowed SubscribeInvoices: %v", err)
	}
	if streamed {
		t.Error("the streamer ran anyway")
	}

	// OpenChannel is a stream and is registered; it must pass.
	streamed = false
	if _, err := guard(context.Background(), &grpc.StreamDesc{}, nil,
		"/lnrpc.Lightning/OpenChannel", streamer); err != nil {
		t.Fatalf("stream guard refused OpenChannel: %v", err)
	}
	if !streamed {
		t.Error("the streamer did not run for a registered method")
	}
}
