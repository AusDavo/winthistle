// Package server is the local web UI: one loopback socket, a token printed at
// startup, and no CORS at all.
//
// docs/design.html's hosting section is the spec, and it is short because the
// deployment is: a single binary on the LND box, bound to loopback, reached
// through an SSH tunnel. What it asks for is "a token printed to the terminal at
// startup and required on every request, plus strict Origin and Host validation,
// and no CORS at all", because a localhost bind is not an authentication
// boundary — any process on the machine can reach it, and any web page the
// operator visits can attempt DNS rebinding against it.
//
// This is the first net/http surface in this repository. The only other one is
// bitcoind's JSON-RPC client, which is a client, so the security shape below had
// no precedent to follow and is the part that had to be right first.
//
// It now serves six screens and the four unsafe methods behind them: start a
// batch, start a setup of the cold wallet, answer the questions either of them
// asks, and stop a batch. What it cannot do is publish — see the guard on
// decision 1 — and what it does not do yet is the bump screen, mixing transports
// per device, and the five reports.
//
// The newest is the setup screen, which is what finally gave webrun.Ask a caller.
// It is the resume path only: no descriptor file crosses this boundary, because a
// path posted from a browser is a browser choosing which file this process opens
// and imports. setup.go is where that decision and its guard are written down.
//
// The two newest screens are the journal's, under /recover, and they are the
// only ones that work on a node that is down. They are also read-only, on
// purpose: recover.go is where that decision and its guards are written down,
// along with what keeps a journal row from being read as a run that is
// happening.
//
// # The three decisions, and the guard each one needs
//
// HANDOFF.md asked for three decisions before any handler was written, on the
// grounds that each one decides the shape of everything after it. They are made,
// they are recorded in that file, and each one names a guard rather than a
// promise. The guards are here because this is the package they are about.
//
// ## 1. run.Do stays a straight-line blocking function
//
// It is driven by a goroutine, and the HTTP handlers feed its four callback
// seams — rehearsal.Signer, abort.Confirmation, bump.Approve, setup.Ask — over
// channels. One code path for the CLI and the UI, and --stop-before-publish
// stays one `if` between arm.Finalize and arm.Publish, read nowhere else. The
// channel is ask.go; the adapters that turn a Question into one of the four seam
// types are in internal/webrun, because each of them owns a verdict this package
// must not invent.
//
// The guard: **a handler must not be able to reach a publish call.** The
// registry pins WalletKit.PublishTransaction at two production call sites and
// internal/methods' call-site test fails on a third — that test type-checks the
// whole module, so it already covers this package, and a web handler is exactly
// where a third call site appears. That is the mechanical half. The other half
// is that this package cannot import internal/arm, internal/bump or
// internal/journal at all: TestTheServerCannotReachAPublishCallOrWriteTheJournal
// enforces it, so a handler cannot be handed an *arm.Armed or a *bump.Signed
// even by accident, and cannot name the table a setup answer is recorded in.
// What the server may do is start run.Do and answer its questions. Publishing
// stays inside the sequence that earned it, and recording stays with the package
// that owns the record.
//
// The journal ban carries two consequences worth stating where they are relied
// on. setup.Ask's third answer exists so a comparison nobody made is never
// written down as a verdict, and no handler here can write that row. And whether
// a run reached the publish call is journal.Run.AbortTarget's answer, so the
// abort control asks — Launcher.AbortRefusal — rather than holding a second copy
// of the rule run.RecoverOne refuses on.
//
// ## 2. A closing browser tab does not abort
//
// It is indistinguishable from a reload, a laptop lid or a Wi-Fi blip, and
// aborting a partially-armed batch on any of those is worse than what abort
// protects against. The clock drives the abort — the 5:00 gate and the peers'
// ten minutes — not the transport.
//
// The guard is the shape of Registry: **a run's state lives in the registry, not
// in the connection.** Nothing in this package cancels a run's context when a
// response ends, and there is no per-connection state a run's lifetime depends
// on. The token is per server start rather than per tab, so any tab that has it
// can re-attach to a live run by its id; the transcript is accumulated on the
// Run and rendered from the beginning on every attach, so a reconnect is a
// re-attach rather than a resume. This asymmetry with Ctrl-C — which does cancel
// the context and does unwind through the abort path — is the most dangerous
// part of the change, and it is written down in HANDOFF.md, not only here.
//
// It has a second, mechanical half now that a handler can start a run:
// TestOnlyTheDoctorScreenReadsTheRequestContext parses this package and requires
// every r.Context() to be in the doctor screen. That is the one place a request
// context legitimately is used, and the distinction is not the transport — a
// pre-flight has nothing to unwind and a run has peers holding reservations. A
// run's context comes from Server.base, which is the process's.
//
// And because a closing tab does not abort, something else has to: the abort
// control on the run screen is required by this decision rather than optional.
// See control.go.
//
// ## 3. The twelve Report() renderers are served verbatim
//
// In a <pre>, at prose.PaneWidth, for v1. The design calls this a "guided web
// UI" and this is a terminal in a browser, which is the honest thing to ship
// before the copy has a second rendering — CLAUDE.md says the recovery screen's
// wording is the highest-stakes copy in the product, and two renderings is how
// it drifts while the overrun tests cover only one.
//
// The guard, for when a screen is later re-rendered as HTML: **the text version
// is the test oracle.** Every screen goes through one function, screen(), and
// TestTheTextIsTheOracle takes the served HTML, unescapes the <pre>, and
// requires it back byte for byte. An HTML re-rendering does not get to delete
// that test; it has to keep passing it, which means the HTML has to be built
// from the same string rather than beside it.
//
// # What the token is, and what it is not
//
// It is 32 bytes from crypto/rand, base64url, generated at every start and
// printed once. It is not a config key, and it must not become one:
// winthistle.toml holds connection details and never a secret, only the paths to
// them — internal/config's package comment says so — and a token in a file is a
// long-lived secret in a file that is read by a tool the operator pastes
// commands into.
//
// It is carried by a cookie, because the screens are plain navigation with no
// JavaScript and a cookie is the only thing a browser sends on a link click. The
// startup URL carries the token in the query, once; the first request exchanges
// it for the cookie and redirects to the bare path, so it leaves the address bar
// and the history. Referrer-Policy: no-referrer is set on every response so it
// cannot leave in a Referer either.
//
// One honest limit: **cookies are not port-scoped.** Any other server on
// 127.0.0.1 that the operator's browser visits will be sent this cookie, and
// any local process can already reach this socket regardless. The token defends
// against a *web page* — DNS rebinding, a cross-origin form post, a stray
// fetch — which is what design.html says it is for. It is not a defence against
// another program on the same machine, and nothing here should be read as
// claiming otherwise.
package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/AusDavo/winthistle/internal/config"
	"github.com/AusDavo/winthistle/internal/prose"
)

// Options is what the server needs that is not the configuration file.
type Options struct {
	// Doctor is the pre-flight's options, so the peer and reserve checks on the
	// doctor screen can be about the batch the operator means to open.
	Doctor DoctorOptions

	// Launcher is what a POST to /runs hands the work to. Nil means this server
	// cannot start a run — every screen still serves, and the control is not
	// offered rather than offered and then refused.
	//
	// An interface because internal/server may not name the run's types: see
	// Launcher, and decision 1's import ban.
	Launcher Launcher
}

// Server is the UI. One per process.
type Server struct {
	cfg  *config.Config
	opts Options

	// token is the startup token. Generated in New and never re-read from
	// anywhere: there is no way to set it, so there is no way to set it badly.
	token string

	// origins and hosts are the exact authorities this server answers to,
	// computed once from the bind address. See guard.go.
	origins map[string]bool
	hosts   map[string]bool

	// Runs is every run this process is driving. It outlives every connection —
	// decision 2.
	Runs *Registry

	// doctorMu serialises the doctor screen. Two concurrent pre-flights would
	// open the run journal twice and report the same failure in two places, and
	// a browser that reloads a slow page is how that happens.
	doctorMu sync.Mutex

	// baseCtx is the context a run is started from, set by Serve. It is emphati-
	// cally not a request's: decision 2 says a response ending means nothing, so
	// a run's lifetime hangs off the process rather than off a connection. Ctrl-C
	// on `winthistle serve` cancels it, which shuts the socket down and — see the
	// asymmetry table in HANDOFF.md — is a different thing from Ctrl-C on
	// `winthistle run`.
	baseMu  sync.Mutex
	baseCtx context.Context

	mux *http.ServeMux
}

// base is the context every run and every journal read behind a control is
// started from. Background when Serve has not been called, which is the case in
// a test that drives Handler directly.
func (s *Server) base() context.Context {
	s.baseMu.Lock()
	defer s.baseMu.Unlock()
	if s.baseCtx == nil {
		return context.Background()
	}
	return s.baseCtx
}

// New builds the server and its token.
//
// It refuses the same bind config refuses, rather than trusting that the
// configuration was loaded through internal/config: a wildcard bind is every
// interface this machine has and every one it grows later, and the token and the
// Origin checks are layered on the assumption that it is not.
func New(cfg *config.Config, opts Options) (*Server, error) {
	if cfg == nil {
		return nil, errors.New("the server needs a configuration: the bind " +
			"address, the journal and the credentials all come from it")
	}
	host, port, err := net.SplitHostPort(cfg.Server.Bind)
	if err != nil {
		return nil, fmt.Errorf("[server] bind %q is not host:port", cfg.Server.Bind)
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]", "*":
		return nil, fmt.Errorf("[server] bind %q is a wildcard. Name the "+
			"interface: \"127.0.0.1:%s\" for the ordinary case", cfg.Server.Bind, port)
	}

	token, err := newToken()
	if err != nil {
		return nil, err
	}

	s := &Server{
		cfg:     cfg,
		opts:    opts,
		token:   token,
		origins: allowedOrigins(host, port),
		hosts:   allowedHosts(host, port),
		Runs:    NewRegistry(),
	}
	s.mux = http.NewServeMux()
	s.routes()
	return s, nil
}

// routes is every path this server has. Method patterns, so a POST to a screen
// is a 405 from the router rather than a screen rendered for a request that
// meant to change something.
func (s *Server) routes() {
	s.mux.HandleFunc("GET /{$}", s.index)
	s.mux.HandleFunc("GET /doctor", s.doctor)

	// The cold wallet's setup: the screen, and the POST that gives setup.Ask its
	// first caller. It is the resume path only — no descriptor file crosses this
	// boundary, because a path posted from a browser is a browser choosing which
	// file this process reads. See setup.go.
	s.mux.HandleFunc("GET /setup", s.setupScreen)
	s.mux.HandleFunc("POST /setup", s.startSetup)
	s.mux.HandleFunc("GET /runs/{id}", s.attach)

	// The file transport's outbound leg. A GET, because it reads: the packet is
	// the pending question's, checked by id, and serving it changes nothing.
	// See transport.go.
	s.mux.HandleFunc("GET /runs/{id}/payload/{question}", s.downloadPayload)

	// The journal, read-only. There is no POST under /recover and there is not
	// meant to be: an abort of a journalled run asks abort.Confirmation once per
	// channel and a browser answers those through a Run, which a journalled run
	// does not have. See recover.go. The method patterns are what make a POST
	// here a 405 rather than a handler somebody has to remember not to write.
	s.mux.HandleFunc("GET /recover", s.recoverList)
	s.mux.HandleFunc("GET /recover/{id}", s.recoverOne)

	// The unsafe methods. See control.go: the guard in front of them already
	// refuses one that cannot say where it came from, and neither of the two
	// locks on the publish call is a convention.
	s.mux.HandleFunc("POST /runs", s.startRun)
	s.mux.HandleFunc("POST /runs/{id}/answer", s.answer)
	s.mux.HandleFunc("GET /runs/{id}/abort", s.abortScreen)
	s.mux.HandleFunc("POST /runs/{id}/abort", s.abortRun)
}

// Handler is the whole server behind the guard, for a test or an embedding.
//
// The guard is not optional and not a route: it wraps the mux, so a handler
// added later is behind it whether or not whoever added it remembered.
func (s *Server) Handler() http.Handler { return s.guard(s.mux) }

// Token is the startup token, for the line printed to the terminal.
func (s *Server) Token() string { return s.token }

// StartupURL is the one line the operator copies.
//
// The token is in the query because this is the only request that can carry it
// there — the guard exchanges it for a cookie and redirects to "/" — and the
// host is spelled 127.0.0.1 rather than localhost so it cannot resolve to
// something else.
func (s *Server) StartupURL() string {
	return "http://" + s.cfg.Server.Bind + "/?" + tokenParam + "=" +
		url.QueryEscape(s.token)
}

// Serve listens and serves until the context is cancelled.
//
// The listener is opened before anything is printed, so a port already in use is
// an error rather than a URL that does not answer.
//
// # Ctrl-C here now ends a run, and that is a change
//
// It used to be true that Ctrl-C on `winthistle serve` touched no run, because
// nothing this server served could start one. Now that it can, the run's context
// is this one — so Ctrl-C shuts the socket down *and* unwinds the run through
// the abort path, exactly as Ctrl-C on `winthistle run` does. That is decision
// 2's own sentence rather than an exception to it: what ends a run is the clock,
// or the operator's Ctrl-C on the process. A tab closing is still nothing.
//
// The alternative — leave the run alone — reads safer and is not. This process
// exits when Serve returns, and a run left running would be killed between two
// RPCs with n shims open and Core holding coin locks, which is the one state the
// abort path exists to avoid. So the socket closes first, and then this waits
// for the run to finish coming apart.
func (s *Server) Serve(ctx context.Context, out io.Writer) error {
	ln, err := net.Listen("tcp", s.cfg.Server.Bind)
	if err != nil {
		return fmt.Errorf("binding %s: %w", s.cfg.Server.Bind, err)
	}

	// Before the first request can arrive, so no handler ever sees the
	// Background fallback in production. A run started from here outlives the
	// response that started it — decision 2 — and ends with this process.
	s.baseMu.Lock()
	s.baseCtx = ctx
	s.baseMu.Unlock()

	fmt.Fprintf(out, "winthistle is serving on %s\n\n  %s\n\n",
		s.cfg.Server.Bind, s.StartupURL())
	if !s.cfg.Loopback() {
		fmt.Fprint(out, notLoopback(s.cfg.Server.Bind))
	}

	srv := &http.Server{
		Handler: s.Handler(),
		// A header that never finishes arriving holds a connection open. There
		// is no write timeout: the doctor screen runs every pre-flight, which
		// takes as long as LND and Core take to answer.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	errs := make(chan error, 1)
	go func() { errs <- srv.Serve(ln) }()

	select {
	case err := <-errs:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
		s.waitForRuns(out)
		return nil
	}
}

// UnwindGrace is how long Serve waits for a cancelled run to finish coming
// apart before it gives up and says what is left.
//
// A backstop rather than the bound. The real bound is internal/run's own
// teardown budget — it makes the abort calls on a context that survives the
// cancellation that triggered them, with a deadline of its own — and this is
// comfortably longer, so Ctrl-C does not abandon a teardown that is still inside
// its own budget. It is not the same constant because this package may not
// import internal/run: decision 1's ban is what keeps a handler from being able
// to name an *arm.Armed, and a shared constant is not worth a hole in it.
const UnwindGrace = 10 * time.Minute

// waitForRuns holds the process open while a cancelled run unwinds.
//
// Polled rather than signalled: the thing being waited for is a goroutine
// calling Run.Finish, and a channel to wait on would be a second way to know a
// run is over — see Registry, where "finished" has one definition.
func (s *Server) waitForRuns(out io.Writer) {
	live := s.Runs.Live()
	if live == nil {
		return
	}
	fmt.Fprintf(out, "\n%s", unwinding(live.ID))

	deadline := time.Now().Add(UnwindGrace)
	for time.Now().Before(deadline) {
		if s.Runs.Live() == nil {
			fmt.Fprintf(out, "run %s is unwound.\n", live.ID)
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	fmt.Fprint(out, stillUnwinding(live.ID))
}

// The two lines Ctrl-C prints, and they are the only operator copy in this
// package that goes to a terminal rather than through screen().
//
// Wrapped, like everything else. They were not, and they came out at 185 and 377
// columns — printed at the one moment an operator is reading hardest, while a
// batch with n shims open is coming apart under them. The same defect the
// transcript had, in the one place no page test was ever going to look:
// TestTheServersOwnCopyFitsThePane measures them now.
func unwinding(id string) string {
	return prose.Para(fmt.Sprintf("Run %s is going, so this is taking it apart "+
		"before it exits: cancelling the shims, abandoning what reached pending, "+
		"releasing Core's coin locks. Waiting up to %s.", id, UnwindGrace)) + "\n"
}

func stillUnwinding(id string) string {
	return prose.Para(fmt.Sprintf("Run %s has not finished coming apart after %s, "+
		"and this is exiting anyway rather than holding the terminal "+
		"indefinitely. Nothing was published — that is what the armed window is "+
		"defined by — but shims or coin locks may be left. `winthistle recover %s` "+
		"is safe to run as many times as it takes, and `winthistle doctor` lists "+
		"the locks.", id, UnwindGrace, id))
}
