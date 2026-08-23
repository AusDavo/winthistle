package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AusDavo/winthistle/internal/config"
)

// testServer is a server on the default bind with no node behind it.
//
// Every test in this file is about the guard, which runs before any handler, so
// nothing here needs LND or Core: the route it asks for is one the guard must
// refuse before it can be reached.
func testServer(t *testing.T) *Server {
	t.Helper()
	s, err := New(&config.Config{
		Path:   "winthistle.toml",
		Server: config.Server{Bind: config.DefaultBind, Journal: t.TempDir() + "/runs.db"},
	}, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// get is a request that has already got past everything except the one thing the
// caller is testing: the right Host, the right token, no Origin.
func get(t *testing.T, s *Server, path string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Host = "127.0.0.1:7420"
	r.AddCookie(&http.Cookie{Name: cookieName, Value: s.token})
	return r
}

func serveIt(s *Server, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

// TestABadHostIsRefusedBeforeTheTokenIsConsidered is the DNS-rebinding check,
// and the ordering half of it matters as much as the refusal.
//
// A page on evil.example whose DNS resolves to 127.0.0.1 reaches this socket
// with a correct token nowhere in sight and Host: evil.example. It must be
// refused for the host, and it must not be told anything about the token —
// otherwise the refusal is a distinguisher an attacker can iterate against.
func TestABadHostIsRefusedBeforeTheTokenIsConsidered(t *testing.T) {
	s := testServer(t)

	for _, host := range []string{
		"evil.example",
		"127.0.0.1.evil.example:7420",
		"127.0.0.1:7421", // right host, wrong port: a different server
		"192.168.1.10:7420",
		"[::1]:7421",
	} {
		r := get(t, s, "/doctor")
		r.Host = host
		w := serveIt(s, r)

		if w.Code != http.StatusMisdirectedRequest {
			t.Errorf("Host %q got %d, want %d", host, w.Code, http.StatusMisdirectedRequest)
		}
		if body := w.Body.String(); strings.Contains(strings.ToLower(body), "token") {
			t.Errorf("Host %q was refused with a body that mentions the token: %q",
				host, body)
		}
	}
}

// TestTheThreeSpellingsOfLoopbackAreAccepted: an operator who types localhost
// and an SSH tunnel that forwards to ::1 are both ordinary, and refusing them
// would be a security theatre that costs the operator the evening.
func TestTheThreeSpellingsOfLoopbackAreAccepted(t *testing.T) {
	s := testServer(t)

	for _, host := range []string{"127.0.0.1:7420", "localhost:7420", "[::1]:7420"} {
		r := get(t, s, "/runs/nothing-here")
		r.Host = host
		if w := serveIt(s, r); w.Code != http.StatusNotFound {
			t.Errorf("Host %q got %d, want the handler to be reached (404)",
				host, w.Code)
		}
	}
}

// TestABindThatNamesAnInterfaceGetsThatNameOnly. A loopback bind gets loopback's
// three spellings because they all reach the same socket. A bind that names some
// other interface gets exactly what it named: the operator said which, and
// guessing a second name for it would widen the check the operator set.
func TestABindThatNamesAnInterfaceGetsThatNameOnly(t *testing.T) {
	s, err := New(&config.Config{
		Server: config.Server{Bind: "192.168.1.10:7420"},
	}, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !s.hosts["192.168.1.10:7420"] {
		t.Error("the bind address itself is not an accepted host")
	}
	for _, host := range []string{"127.0.0.1:7420", "localhost:7420"} {
		if s.hosts[host] {
			t.Errorf("%q was accepted for a bind that named 192.168.1.10", host)
		}
	}
}

// TestAWildcardBindIsRefusedHereToo. config.checkBind refuses it, but this
// package must not depend on having been handed a configuration that went
// through that check: the token and the Origin checks are layered on the
// assumption that the socket is not on every interface the machine grows.
func TestAWildcardBindIsRefusedHereToo(t *testing.T) {
	for _, bind := range []string{"0.0.0.0:7420", ":7420", "[::]:7420", "*:7420"} {
		if _, err := New(&config.Config{
			Server: config.Server{Bind: bind},
		}, Options{}); err == nil {
			t.Errorf("New accepted the wildcard bind %q", bind)
		}
	}
}

// TestACrossOriginRequestIsRefused, in both the ways a browser tells us.
//
// Sec-Fetch-Site: same-site is refused along with cross-site, and that is the
// part worth a test rather than a comment — another server on 127.0.0.1 at a
// different port is same-site, and it is not us.
func TestACrossOriginRequestIsRefused(t *testing.T) {
	s := testServer(t)

	for _, tc := range []struct{ what, origin, site string }{
		{"a cross-site fetch", "https://evil.example", "cross-site"},
		{"another port on loopback", "http://127.0.0.1:9999", "same-site"},
		{"a bare same-site claim", "", "same-site"},
		{"an origin we do not serve", "http://localhost:7421", ""},
		{"https on our own authority", "https://127.0.0.1:7420", ""},
	} {
		r := get(t, s, "/doctor")
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		if tc.site != "" {
			r.Header.Set("Sec-Fetch-Site", tc.site)
		}
		if w := serveIt(s, r); w.Code != http.StatusForbidden {
			t.Errorf("%s got %d, want %d", tc.what, w.Code, http.StatusForbidden)
		}
	}
}

// TestAPlainNavigationCarriesNoOriginAndIsFine. Absent is not suspicious: a link
// click sends no Origin and Sec-Fetch-Site: none, and that is most of this UI.
func TestAPlainNavigationCarriesNoOriginAndIsFine(t *testing.T) {
	s := testServer(t)

	r := get(t, s, "/runs/nothing-here")
	r.Header.Set("Sec-Fetch-Site", "none")
	r.Header.Set("Sec-Fetch-Mode", "navigate")
	if w := serveIt(s, r); w.Code != http.StatusNotFound {
		t.Fatalf("a plain navigation got %d", w.Code)
	}

	r = get(t, s, "/runs/nothing-here")
	r.Header.Set("Origin", "http://127.0.0.1:7420")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	if w := serveIt(s, r); w.Code != http.StatusNotFound {
		t.Fatalf("a same-origin request got %d", w.Code)
	}
}

// TestSomethingThatChangesSomethingHasToSayWhereItCameFrom.
//
// No route serves an unsafe method yet, and this is the check that has to be in
// place before one does: a POST with no Origin is indistinguishable from a form
// on somebody else's page aimed at the loopback interface, so it is refused for
// that rather than 405'd for the method.
func TestSomethingThatChangesSomethingHasToSayWhereItCameFrom(t *testing.T) {
	s := testServer(t)

	r := httptest.NewRequest(http.MethodPost, "/doctor", nil)
	r.Host = "127.0.0.1:7420"
	r.AddCookie(&http.Cookie{Name: cookieName, Value: s.token})
	if w := serveIt(s, r); w.Code != http.StatusForbidden {
		t.Errorf("an originless POST got %d, want %d", w.Code, http.StatusForbidden)
	}

	r = httptest.NewRequest(http.MethodPost, "/doctor", nil)
	r.Host = "127.0.0.1:7420"
	r.Header.Set("Origin", "http://127.0.0.1:7420")
	r.AddCookie(&http.Cookie{Name: cookieName, Value: s.token})
	// Past the guard, so the router answers: no route takes a POST.
	if w := serveIt(s, r); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("a POST with a good Origin got %d, want the router's %d",
			w.Code, http.StatusMethodNotAllowed)
	}
}

// TestNoTokenNoScreen, and the three places a token is accepted from.
func TestNoTokenNoScreen(t *testing.T) {
	s := testServer(t)

	bare := httptest.NewRequest(http.MethodGet, "/doctor", nil)
	bare.Host = "127.0.0.1:7420"
	if w := serveIt(s, bare); w.Code != http.StatusForbidden {
		t.Errorf("no token got %d, want %d", w.Code, http.StatusForbidden)
	}

	wrong := httptest.NewRequest(http.MethodGet, "/doctor", nil)
	wrong.Host = "127.0.0.1:7420"
	wrong.AddCookie(&http.Cookie{Name: cookieName, Value: "not-the-token"})
	if w := serveIt(s, wrong); w.Code != http.StatusForbidden {
		t.Errorf("a wrong cookie got %d, want %d", w.Code, http.StatusForbidden)
	}

	bearer := httptest.NewRequest(http.MethodGet, "/runs/nothing-here", nil)
	bearer.Host = "127.0.0.1:7420"
	bearer.Header.Set("Authorization", "Bearer "+s.token)
	if w := serveIt(s, bearer); w.Code != http.StatusNotFound {
		t.Errorf("a bearer token got %d, want the handler to be reached", w.Code)
	}
}

// TestTheTokenLeavesTheURLOnTheFirstRequest.
//
// The startup URL is the only place the token appears in a query string, and it
// appears there once: the guard trades it for a cookie and redirects to the bare
// path, so it is out of the address bar and out of the history before the first
// screen is drawn. Referrer-Policy is what keeps it out of a Referer in the
// meantime.
func TestTheTokenLeavesTheURLOnTheFirstRequest(t *testing.T) {
	s := testServer(t)

	r := httptest.NewRequest(http.MethodGet, "/?"+tokenParam+"="+s.token, nil)
	r.Host = "127.0.0.1:7420"
	w := serveIt(s, r)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("the startup URL got %d, want %d", w.Code, http.StatusSeeOther)
	}
	if loc := w.Header().Get("Location"); loc != "/" {
		t.Errorf("Location = %q, want %q — the token must not survive the redirect", loc, "/")
	}
	if got := w.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("Referrer-Policy = %q; the token is in this page's URL", got)
	}

	var set *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == cookieName {
			set = c
		}
	}
	if set == nil {
		t.Fatal("no cookie was set, so the redirect goes to a page that refuses")
	}
	if set.Value != s.token {
		t.Error("the cookie does not carry the token")
	}
	if !set.HttpOnly {
		t.Error("the cookie is readable by script")
	}
	if set.SameSite != http.SameSiteStrictMode {
		t.Errorf("SameSite = %v, want Strict", set.SameSite)
	}

	// A wrong token in the query is refused rather than redirected, so a bad
	// link cannot be laundered into a page that then asks for the cookie.
	bad := httptest.NewRequest(http.MethodGet, "/?"+tokenParam+"=wrong", nil)
	bad.Host = "127.0.0.1:7420"
	if w := serveIt(s, bad); w.Code != http.StatusForbidden {
		t.Errorf("a wrong token in the query got %d, want %d", w.Code, http.StatusForbidden)
	}
}

// TestNoCORSHeaderEverAppears.
//
// design.html says "no CORS at all", and this is the test that keeps it true
// after somebody adds a fetch() to a screen and reaches for the header that
// makes their console error go away. Every response, refusals included.
func TestNoCORSHeaderEverAppears(t *testing.T) {
	s := testServer(t)

	requests := []*http.Request{
		get(t, s, "/runs/nothing-here"),
		httptest.NewRequest(http.MethodGet, "/doctor", nil),     // no token
		httptest.NewRequest(http.MethodOptions, "/doctor", nil), // a preflight
	}
	requests[1].Host = "127.0.0.1:7420"
	requests[2].Host = "127.0.0.1:7420"
	requests[2].Header.Set("Origin", "https://evil.example")
	requests[2].Header.Set("Access-Control-Request-Method", "POST")

	for _, r := range requests {
		w := serveIt(s, r)
		for name := range w.Header() {
			if strings.HasPrefix(strings.ToLower(name), "access-control-") {
				t.Errorf("%s %s answered with %s: %q",
					r.Method, r.URL.Path, name, w.Header().Get(name))
			}
		}
	}
}

// TestEveryResponseCarriesTheHardeningHeaders, including the refusals, because a
// refusal is a page a browser renders too.
func TestEveryResponseCarriesTheHardeningHeaders(t *testing.T) {
	s := testServer(t)

	want := map[string]string{
		"Cache-Control":          "no-store",
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "no-referrer",
		"X-Frame-Options":        "DENY",
	}

	for _, r := range []*http.Request{
		get(t, s, "/runs/nothing-here"),
		httptest.NewRequest(http.MethodGet, "/doctor", nil), // refused: no token
	} {
		r.Host = "127.0.0.1:7420"
		w := serveIt(s, r)
		for name, value := range want {
			if got := w.Header().Get(name); got != value {
				t.Errorf("%s: %s = %q, want %q", r.URL.Path, name, got, value)
			}
		}
		csp := w.Header().Get("Content-Security-Policy")
		if !strings.Contains(csp, "default-src 'none'") {
			t.Errorf("%s: CSP = %q", r.URL.Path, csp)
		}
	}
}

// TestTheTokenIsFreshAndNotWritable.
//
// Not a config key, and there is no way to set it: winthistle.toml holds
// connection details and never a secret, only the paths to them, and a token in
// a file is a long-lived secret in a file. So it is generated per start, which
// means two servers never share one.
func TestTheTokenIsFreshAndNotWritable(t *testing.T) {
	a, b := testServer(t), testServer(t)
	if a.Token() == b.Token() {
		t.Fatal("two servers were given the same token")
	}
	if len(a.Token()) < 40 {
		t.Errorf("the token is %d characters; 32 bytes of base64url is 43",
			len(a.Token()))
	}
	if !strings.Contains(a.StartupURL(), a.Token()) {
		t.Error("the startup URL does not carry the token")
	}
	if !strings.HasPrefix(a.StartupURL(), "http://127.0.0.1:7420/") {
		t.Errorf("StartupURL = %q; it must name the address, not a name that "+
			"could resolve elsewhere", a.StartupURL())
	}
}

// TestTheRedirectKeepsEverythingButTheToken. The token is deleted from the
// query; a screen that grows a parameter later must not lose it to the
// exchange, and the redirect must stay a path rather than becoming a URL that
// could point anywhere.
func TestTheRedirectKeepsEverythingButTheToken(t *testing.T) {
	s := testServer(t)

	r := httptest.NewRequest(http.MethodGet,
		"/doctor?batch=evening&"+tokenParam+"="+s.token+"&connect=1", nil)
	r.Host = "127.0.0.1:7420"
	w := serveIt(s, r)

	loc := w.Header().Get("Location")
	if strings.Contains(loc, s.token) || strings.Contains(loc, tokenParam) {
		t.Fatalf("Location = %q still carries the token", loc)
	}
	if !strings.HasPrefix(loc, "/doctor?") {
		t.Fatalf("Location = %q, want a path on this server", loc)
	}
	for _, want := range []string{"batch=evening", "connect=1"} {
		if !strings.Contains(loc, want) {
			t.Errorf("Location = %q dropped %q", loc, want)
		}
	}
}

// TestAHostIsMatchedCaseInsensitively, in both directions: the header's case and
// the config's.
func TestAHostIsMatchedCaseInsensitively(t *testing.T) {
	s := testServer(t)
	r := get(t, s, "/runs/nothing-here")
	r.Host = "LocalHost:7420"
	if w := serveIt(s, r); w.Code != http.StatusNotFound {
		t.Errorf("Host %q got %d, want the handler to be reached", r.Host, w.Code)
	}

	named, err := New(&config.Config{
		Server: config.Server{Bind: "MyBox.local:7420"},
	}, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !named.hosts["mybox.local:7420"] {
		t.Errorf("a bind of MyBox.local built the host set %v", named.hosts)
	}
}

// TestABrowsersFormPostIsAdmitted is the regression test for the bug the first
// browser render found, and it is the one test in this file written from a
// measurement rather than from the spec.
//
// Chrome 152 sends `Origin: null` on a same-origin top-level form POST to this
// UI — measured, twice, with the referrer policy set to no-referrer and to
// same-origin, and with a real click rather than a scripted submit. The guard's
// Origin check read that literal as "a different origin" and refused, so every
// form in the UI was unusable: nothing could be started, answered or stopped.
//
// The whole suite passed, because every test chose `Origin:
// http://127.0.0.1:7420`. That is the lesson worth keeping more than the fix: a
// header a browser controls is not a header a test may invent.
func TestABrowsersFormPostIsAdmitted(t *testing.T) {
	s := testServer(t)

	for _, tc := range []struct {
		name   string
		origin string
		site   string
		admit  bool
	}{
		// What Chrome actually sends. Must be admitted.
		{"a browser form post", opaqueOrigin, "same-origin", true},
		// What a non-browser client sends: a real Origin, no fetch metadata.
		{"a client with a real origin", "http://127.0.0.1:7420", "", true},
		{"localhost's spelling", "http://localhost:7420", "", true},

		// An opaque origin says nothing, so on its own it is not enough. This is
		// the half that keeps the fix from being a hole: without Sec-Fetch-Site
		// there is nothing to distinguish this from a form on someone else's page.
		{"an opaque origin and no fetch metadata", opaqueOrigin, "", false},
		// A sandboxed or cross-site initiator arrives as cross-site, whatever it
		// claims about its origin.
		{"an opaque origin from another site", opaqueOrigin, "cross-site", false},
		{"an opaque origin from the same site", opaqueOrigin, "same-site", false},
		// And a real foreign origin is still refused, both ways.
		{"a foreign origin", "http://evil.example", "", false},
		{"a foreign origin claiming same-origin", "http://evil.example", "same-origin", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/runs", strings.NewReader(""))
			r.Host = "127.0.0.1:7420"
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			if tc.site != "" {
				r.Header.Set("Sec-Fetch-Site", tc.site)
			}
			r.AddCookie(&http.Cookie{Name: cookieName, Value: s.token})

			w := serveIt(s, r)
			// Admitted means "reached a handler", not "succeeded": this server has
			// no launcher, so the handler's own refusal is a 501. What must never
			// happen is the guard's 403.
			refused := w.Code == http.StatusForbidden
			if tc.admit && refused {
				t.Fatalf("the guard refused it:\n%s", w.Body.String())
			}
			if !tc.admit && !refused {
				t.Fatalf("the guard admitted it, got %d:\n%s", w.Code, w.Body.String())
			}
		})
	}
}
