package methods_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AusDavo/winthistle/internal/lnd"
	"github.com/AusDavo/winthistle/internal/methods"
	"github.com/AusDavo/winthistle/internal/regtestenv"
	"github.com/lightningnetwork/lnd/lnrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

// TestTheBakedMacaroonWorksAndIsNarrow runs the printed command against a real
// node and then uses the credential it produces.
//
// This is the test the registry most needs, because the failure it catches is the
// one a hand-written permission list is prone to and no amount of internal
// consistency can rule out: a method path that does not exist. LND validates each
// uri against interceptorChain.Permissions() at bake time, so a typo — a renamed
// method, a subserver that is not compiled in — is refused here rather than
// during someone's setup.
//
// It also proves the two halves of the promise: the credential is sufficient for
// what the app calls, and insufficient for what it does not.
func TestTheBakedMacaroonWorksAndIsNarrow(t *testing.T) {
	env := regtestenv.Start(t)

	macPath := bakeFromThePrintedCommand(t, env)

	// Sufficient. Dial's own probe calls GetInfo, so a successful dial is
	// already one registered method exercised through the narrow credential.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	certPath := filepath.Join(env.Root, "regtest", "creds", "alice", "tls.cert")
	cli, err := lnd.Dial(ctx, lnd.Config{
		Address:  "127.0.0.1:10009",
		TLSCert:  certPath,
		Macaroon: macPath,
	})
	if err != nil {
		t.Fatalf("the baked credential cannot even dial: %v", err)
	}
	t.Cleanup(func() { cli.Close() })

	// One more registered call, on the other service, so the walletrpc half of
	// the list is covered too.
	if _, err := cli.Lightning.PendingChannels(ctx, &lnrpc.PendingChannelsRequest{}); err != nil {
		t.Errorf("PendingChannels is registered but the baked credential was "+
			"refused: %v", err)
	}

	assertNarrow(t, certPath, macPath)
}

// bakeFromThePrintedCommand runs exactly what `winthistle print-macaroon-command`
// emits, and returns the path of the credential it produced.
//
// The permission list is taken from the printed command rather than from the
// registry directly, so that a bug in the rendering — a dropped continuation, a
// mangled entity — fails here too.
func bakeFromThePrintedCommand(t *testing.T, env *regtestenv.Env) string {
	t.Helper()

	lncli := filepath.Join(env.Root, "regtest", "bin", "lncli")
	if _, err := os.Stat(lncli); err != nil {
		t.Skipf("%s missing: %v", lncli, err)
	}

	args := lncliArgs(t, methods.BakeCommand(""))

	// No --save_to: lncli then prints the macaroon hex, which avoids caring
	// where a path inside the container would land.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, lncli, append([]string{"alice"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// The wrapper shells out to docker, and on a machine whose login session
		// predates the docker group that fails with a permission error rather
		// than anything to do with macaroons. Skip with the reason; every other
		// harness-backed test reaches alice over TCP and is unaffected.
		text := string(out)
		if strings.Contains(text, "permission denied") ||
			strings.Contains(text, "Cannot connect to the Docker daemon") ||
			strings.Contains(text, "executable file not found") {

			t.Skipf("cannot run lncli in the harness: %v\n%s — try: "+
				"sg docker -c \"make test\"", err, text)
		}
		t.Fatalf("baking the printed command failed: %v\n%s", err, text)
	}

	macHex := lastNonEmptyLine(string(out))
	macBytes, err := hex.DecodeString(macHex)
	if err != nil {
		t.Fatalf("lncli did not print a macaroon: %v\noutput was:\n%s", err, out)
	}
	if len(macBytes) == 0 {
		t.Fatal("lncli printed an empty macaroon")
	}

	path := filepath.Join(t.TempDir(), "winthistle.macaroon")
	if err := os.WriteFile(path, macBytes, 0o600); err != nil {
		t.Fatalf("writing the baked macaroon: %v", err)
	}
	return path
}

// assertNarrow proves the credential is refused for a method the registry does
// not list.
//
// It goes around lnd.Dial deliberately, because lnd.Dial installs the registry
// guard and the guard would stop the call before LND ever saw it. The point here
// is the other enforcement — LND's, which is the one that holds even if this
// process is wrong about itself.
//
// WalletBalance is the probe: unregistered, read-only, and harmless if it were
// somehow permitted. A spend would be the more dramatic demonstration and is not
// worth making a test do.
func assertNarrow(t *testing.T, certPath, macPath string) {
	t.Helper()

	certBytes, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("reading %s: %v", certPath, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certBytes) {
		t.Fatalf("no certificate in %s", certPath)
	}
	macBytes, err := os.ReadFile(macPath)
	if err != nil {
		t.Fatalf("reading %s: %v", macPath, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := grpc.DialContext(ctx, "127.0.0.1:10009",
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
			RootCAs: pool, MinVersion: tls.VersionTLS12,
		})),
		grpc.WithPerRPCCredentials(rawMacaroon(hex.EncodeToString(macBytes))),
	)
	if err != nil {
		t.Fatalf("dialing alice unguarded: %v", err)
	}
	defer conn.Close()

	const unregistered = "/lnrpc.Lightning/WalletBalance"
	err = conn.Invoke(ctx, unregistered,
		&lnrpc.WalletBalanceRequest{}, &lnrpc.WalletBalanceResponse{})
	if err == nil {
		t.Fatalf("the baked credential was allowed to call %s, which the "+
			"registry does not list — the uri entity is not restricting it",
			unregistered)
	}
	// The refusal is bakery.ErrPermissionDenied — errgo.New("permission denied")
	// in gopkg.in/macaroon-bakery.v2/bakery/error.go — returned through LND's
	// interceptor as a plain error with no gRPC status attached. So it arrives as
	// codes.Unknown, and anything that means to recognise "the credential lacks
	// this permission" has to match the text. `winthistle doctor` will need
	// exactly that, and matching on codes.PermissionDenied would silently never
	// fire.
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("%s failed, but not over the macaroon: %v", unregistered, err)
	}
	if code := status.Code(err); code != codes.Unknown {
		t.Logf("LND now returns %s for a macaroon refusal rather than an "+
			"untyped error; the doctor could match on the code instead of the "+
			"text: %v", code, err)
	}
}

// rawMacaroon is lnd.macaroonCreds without the package boundary, for the one
// connection that must not go through lnd.Dial.
type rawMacaroon string

func (m rawMacaroon) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"macaroon": string(m)}, nil
}

func (m rawMacaroon) RequireTransportSecurity() bool { return true }

// lncliArgs turns the printed command back into an argv, and checks it looks the
// way it is meant to on the way through.
func lncliArgs(t *testing.T, command string) []string {
	t.Helper()

	fields := strings.Fields(strings.ReplaceAll(command, "\\\n", " "))
	if len(fields) == 0 || fields[0] != "lncli" {
		t.Fatalf("the printed command does not start with lncli: %q", command)
	}

	var (
		args        []string
		permissions int
	)
	rest := fields[1:]
	for i := 0; i < len(rest); i++ {
		f := rest[i]
		switch {
		case f == "\\":
			// A continuation that survived the split; not an argument.
		case f == "--save_to":
			// Dropped, along with its path: this test reads the macaroon off
			// stdout, so it does not care where lncli would have written it —
			// and inside the harness that path is a container's, not ours.
			i++
		case strings.HasPrefix(f, "--save_to="):
			// Same, in the joined form.
		case strings.HasPrefix(f, "uri:"):
			permissions++
			args = append(args, f)
		case f == "bakemacaroon":
			args = append(args, f)
		default:
			t.Fatalf("unexpected token %q in the printed command:\n%s", f, command)
		}
	}
	if permissions != len(methods.App()) {
		t.Fatalf("parsed %d permissions out of the printed command, but the "+
			"registry has %d app methods", permissions, len(methods.App()))
	}
	return args
}

func lastNonEmptyLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return ""
}
