package doctor

import (
	"errors"
	"strings"
	"testing"

	"github.com/AusDavo/winthistle/internal/prose"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestTheTwoRefusalsThatLookAlike.
//
// Both carry bakery's plain "permission denied" text and they mean opposite
// things: InvalidArgument is CheckMacaroonPermissions answering about the
// macaroon in the request, and an untyped error with the same text is LND's
// interceptor refusing *this* call. Matching on the code alone would confuse
// them; matching on the text alone would too.
func TestTheTwoRefusalsThatLookAlike(t *testing.T) {
	cases := map[string]struct {
		err  error
		want bool
	}{
		"the interceptor refusing us": {
			// bakery.ErrPermissionDenied is errgo.New("permission denied") with
			// no gRPC status attached, so it arrives as codes.Unknown.
			err:  status.Error(codes.Unknown, "permission denied"),
			want: true,
		},
		"the answer about somebody else's macaroon": {
			err:  status.Error(codes.InvalidArgument, "permission denied"),
			want: false,
		},
		"a typed refusal, if LND ever starts sending one": {
			err:  status.Error(codes.PermissionDenied, "nope"),
			want: true,
		},
		"the node being down": {
			err:  status.Error(codes.Unavailable, "connection refused"),
			want: false,
		},
		"a plain error": {
			err:  errors.New("permission denied"),
			want: true,
		},
		"nothing at all": {err: nil, want: false},
	}
	for name, tc := range cases {
		if got := tooNarrow(tc.err); got != tc.want {
			t.Errorf("%s: tooNarrow = %v, want %v (%v)", name, got, tc.want, tc.err)
		}
	}
}

// TestTheReportStaysInThePane. These reports are read in a terminal beside
// something else, often while deciding whether to bring a cold wallet out.
func TestTheReportStaysInThePane(t *testing.T) {
	r := &Report{}

	ok := r.add(Check{Name: "LND"})
	ok.say("bitcoin regtest, lnd 0.21.2-beta, " +
		"02c6047f9441ed7d6d3045406e95c07cd85c778e4b8cef3ca7abac09b95c709ee5")

	bad := r.add(Check{Name: "the macaroon"})
	bad.fail("this credential can also do 3 things the tool promises it cannot, " +
		"which is what admin.macaroon looks like from here and is the whole " +
		"reason the never-list is checked against the file rather than against " +
		"this build's intentions")
	bad.say("    send coins on-chain — /lnrpc.Lightning/SendCoins")
	bad.fix("winthistle print-macaroon-command --save-to /home/someone/.lnd/" +
		"winthistle.macaroon | sh")

	for i, line := range strings.Split(r.Report(), "\n") {
		// A command is exempt, and deliberately: it has to be pasteable, and a
		// shell command cannot be wrapped without changing it. Everything the
		// operator *reads* is held to the pane; the one line they *paste* is not.
		if strings.HasPrefix(line, "  $ ") {
			continue
		}
		// Runes, not bytes. This copy is full of em dashes, and a byte count
		// reports a line as three columns wider than it renders — which is worse
		// than no check, because it is the kind of wrongness that gets fixed by
		// widening the pane. Every other pane test in the repository counts
		// runes; this one did not.
		if n := len([]rune(line)); n > prose.PaneWidth {
			t.Errorf("line %d is %d columns, past the %d-column pane:\n%s",
				i+1, n, prose.PaneWidth, line)
		}
	}
	if r.OK() {
		t.Error("a report with a failure in it says it is OK")
	}
	if !strings.Contains(r.Report(), "Not ready") {
		t.Error("a failing report does not open by saying so")
	}
}

// TestAWarningIsNotAFailure. A warning never stops a run; if something should
// stop a run it is a Fail, and the difference is the whole vocabulary here.
func TestAWarningIsNotAFailure(t *testing.T) {
	r := &Report{}
	c := r.add(Check{Name: "the coins"})
	c.warn("2 coins excluded")
	if !r.OK() {
		t.Error("a warning made the report say the setup is unusable")
	}
	// And a warning after a failure must not downgrade it.
	c.fail("nothing to fund a batch with")
	c.warn("also this")
	if r.Checks[0].Status != Fail {
		t.Errorf("a warning downgraded a failure to %s", r.Checks[0].Status)
	}
}
