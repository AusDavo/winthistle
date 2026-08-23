package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"strings"
)

// cookieName carries the startup token once it has been exchanged.
//
// Not a __Host- prefixed name: that prefix requires the Secure attribute, which
// requires https, which this does not have and should not pretend to. See the
// package comment on what the token defends against and what it does not.
const cookieName = "winthistle_token"

// opaqueOrigin is the serialisation a browser sends when a request's origin is
// opaque — and, measured against Chrome 152 on this UI, what it sends on *every*
// same-origin top-level form POST, whatever the referrer policy is.
//
// That was found by rendering a page in a browser for the first time, and it had
// made every form in this UI unusable: the Origin check below read "null" as "a
// different origin" and refused, so nothing could be started, answered or
// stopped. Every test had passed because every test chose an Origin header, and
// the one no test chose was the one Chrome sends.
//
// It is treated as an absent Origin, which is what it is: an opaque origin says
// nothing about who sent the request, in either direction. The decision then
// falls to Sec-Fetch-Site, which is the better signal anyway — the browser
// computes it, page JavaScript cannot set it, and "same-origin" means the
// initiator was us. See the unsafe-method check.
const opaqueOrigin = "null"

// tokenParam is the one place the token may appear in a URL. The guard trades it
// for the cookie and redirects, so it appears there exactly once per browser.
const tokenParam = "token"

// newToken is 32 bytes of crypto/rand, base64url, no padding: 43 characters, no
// shell-quoting hazards, and copy-pasteable out of a terminal in one go.
func newToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating the startup token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// allowedOrigins is the exact set of Origin values this server answers to.
//
// Exact strings rather than a pattern: an Origin check written as a prefix or a
// suffix match is the check that lets http://127.0.0.1.evil.example through.
func allowedOrigins(host, port string) map[string]bool {
	out := map[string]bool{}
	for _, authority := range authorities(host, port) {
		out["http://"+authority] = true
	}
	return out
}

// allowedHosts is the same set for the Host header, which carries no scheme.
func allowedHosts(host, port string) map[string]bool {
	out := map[string]bool{}
	for _, authority := range authorities(host, port) {
		out[authority] = true
	}
	return out
}

// authorities is host:port in every spelling that reaches the same socket, and
// no others.
//
// A loopback bind gets the three names of loopback, because an operator who
// types "localhost" and an SSH tunnel that forwards to ::1 are both ordinary. A
// bind that names some other interface gets exactly what it named: the operator
// said which interface, and the guard has no business guessing a second name for
// it.
func authorities(host, port string) []string {
	var hosts []string
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		hosts = []string{"127.0.0.1", "localhost", "[::1]"}
	} else if host == "localhost" {
		hosts = []string{"localhost", "127.0.0.1", "[::1]"}
	} else if strings.Contains(host, ":") {
		hosts = []string{"[" + host + "]"}
	} else {
		hosts = []string{host}
	}

	// Lowercased, because the Host header is matched case-insensitively and a
	// config that named "MyBox.local" would otherwise build a set nothing
	// matches.
	out := make([]string, 0, len(hosts)*2)
	for _, h := range hosts {
		h = strings.ToLower(h)
		out = append(out, h+":"+port)
		// Port 80 is elided by browsers in both Host and Origin.
		if port == "80" {
			out = append(out, h)
		}
	}
	return out
}

// guard is every check, in the order that discloses least.
//
// Host and Origin first, then the token. A DNS-rebinding attempt is refused for
// naming the wrong authority before it is ever told anything about a token, so
// the refusal it sees carries no information about whether the token it guessed
// was close. The token comparison is constant-time regardless, but the ordering
// is free and the disclosure is not.
//
// There is no CORS anywhere in this file, and that is the point rather than an
// omission: no Access-Control-Allow-Origin, no preflight handler, no Vary on
// Origin. A cross-origin request gets a refusal, and the browser is left to
// enforce the same-origin policy it would have enforced anyway.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		// The token spends one request in a query string. This is what stops it
		// leaving in a Referer from that page.
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		// Nothing is fetched from anywhere. The screens are one <style> block
		// and text, so 'none' plus inline styles is the whole allowance, and a
		// script tag that appears later fails loudly rather than running.
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; "+
			"img-src 'none'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")

		if !s.hosts[strings.ToLower(r.Host)] {
			// Deliberately terse, and deliberately not naming the addresses that
			// would have worked: this refusal is what an attacker's page reads.
			refuse(w, http.StatusMisdirectedRequest,
				"This request named a host this server does not answer to.\n\n"+
					"winthistle answers on the address it printed at startup and on "+
					"nothing else. A request arriving under some other name is either "+
					"a proxy rewriting the Host header or a web page attempting DNS "+
					"rebinding against your loopback interface.")
			return
		}

		// Sec-Fetch-Site is the browser telling the truth about who started the
		// request, and it cannot be set by page JavaScript. "same-site" is
		// refused along with "cross-site": another server on 127.0.0.1 at a
		// different port is same-site and is not us.
		switch r.Header.Get("Sec-Fetch-Site") {
		case "", "same-origin", "none":
		default:
			refuse(w, http.StatusForbidden, crossOriginRefusal)
			return
		}

		// Origin, when the browser sent a real one. Absent is not suspicious: a
		// plain navigation carries no Origin, and that is most of this UI. Nor is
		// the literal "null" — see opaqueOrigin, which is what Chrome sends on
		// every form post here.
		if origin := r.Header.Get("Origin"); origin != "" && origin != opaqueOrigin &&
			!s.origins[origin] {

			refuse(w, http.StatusForbidden, crossOriginRefusal)
			return
		}

		// A request that means to change something must prove where it came
		// from, rather than merely fail to disprove it.
		//
		// This is the load-bearing check for every form in this UI, not a belt
		// alongside the Origin braces: a browser's form post arrives with an
		// opaque Origin, so what admits it is Sec-Fetch-Site: same-origin and
		// nothing else. That header is worth leaning on — the browser computes
		// it, it is a forbidden header name so page JavaScript cannot set it, and
		// "same-origin" means the initiator's origin is ours. We cannot be framed
		// into producing one either: frame-ancestors 'none' and X-Frame-Options:
		// DENY, and a sandboxed frame of our own page has an opaque origin and
		// would arrive as cross-site, which the switch above already refuses.
		//
		// A client that is not a browser sends neither, and must send a real
		// Origin instead. The refusal says so.
		if !safeMethod(r.Method) && !s.origins[r.Header.Get("Origin")] &&
			r.Header.Get("Sec-Fetch-Site") != "same-origin" {

			refuse(w, http.StatusForbidden,
				"A request that changes something has to say where it came from.\n\n"+
					"This one carried no Origin this server answers to and no "+
					"Sec-Fetch-Site: same-origin, so there is nothing to distinguish "+
					"it from a form on somebody else's page pointed at your loopback "+
					"interface.")
			return
		}

		if !s.authenticate(w, r) {
			return
		}
		next.ServeHTTP(w, r)
	})
}

const crossOriginRefusal = "This request came from another origin.\n\n" +
	"winthistle sends no CORS headers and accepts no cross-origin request. " +
	"There is no configuration that changes this: a page that can reach a " +
	"channel-opening UI in your browser is the threat the loopback bind does " +
	"not defend against on its own."

// safeMethod reports whether the method only reads. GET, HEAD and OPTIONS, as
// RFC 9110 has them — and OPTIONS is here only so it is classified, since no
// route serves it and there is no preflight to answer.
func safeMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}

// authenticate finds the startup token, or refuses.
//
// Three places, in the order of how long the token stays there: the query, once,
// which is exchanged for a cookie and redirected away; the cookie, which is what
// a browser then sends; and an Authorization: Bearer header, which is what
// anything that is not a browser sends.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) bool {
	if q := r.URL.Query().Get(tokenParam); q != "" {
		if !s.tokenMatches(q) {
			refuse(w, http.StatusForbidden, badToken)
			return false
		}
		http.SetCookie(w, &http.Cookie{
			Name:  cookieName,
			Value: s.token,
			Path:  "/",
			// No Expires: the token dies with the process that printed it, so
			// a cookie that outlived the browser session would only ever be
			// stale.
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
		})
		// 303 to the same request with the token deleted and nothing else
		// touched: the token leaves the address bar and the history on the
		// first navigation, and a screen that grows a query parameter later
		// does not lose it here. Only the path and query survive — no scheme
		// and no host — so this cannot be aimed anywhere but at this server.
		to := r.URL.EscapedPath()
		rest := r.URL.Query()
		rest.Del(tokenParam)
		if len(rest) > 0 {
			to += "?" + rest.Encode()
		}
		http.Redirect(w, r, to, http.StatusSeeOther)
		return false
	}

	if c, err := r.Cookie(cookieName); err == nil && s.tokenMatches(c.Value) {
		return true
	}
	if bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok &&
		s.tokenMatches(bearer) {

		return true
	}

	refuse(w, http.StatusForbidden, badToken)
	return false
}

const badToken = "The startup token is missing or wrong.\n\n" +
	"winthistle printed one URL when it started, containing a token that is " +
	"generated fresh on every start and stored nowhere. Open that URL. If the " +
	"terminal has scrolled away, stop winthistle and start it again: a new " +
	"token is cheaper than a recovered one."

func (s *Server) tokenMatches(got string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) == 1
}

// refuse answers in plain text.
//
// Not a rendered screen: a refusal is read by whoever sent the request, which in
// the cases that matter is not the operator, and a page is a larger thing to
// hand to a stranger than a sentence.
func refuse(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	fmt.Fprintln(w, body)
}

// notLoopback is the paragraph printed when the operator named an interface that
// is not loopback. It is not refused — they named it — but the token and the
// Origin checks were designed on the assumption that they were the second line
// of defence rather than the first.
func notLoopback(bind string) string {
	return fmt.Sprintf("  Note: %s is not a loopback address, so this socket is "+
		"reachable\n  from your network. The startup token and the Origin checks "+
		"are the only\n  things in front of it, and they were designed to sit "+
		"behind a loopback\n  bind rather than instead of one. docs/design.html "+
		"recommends binding\n  127.0.0.1 and reaching the UI through an SSH "+
		"tunnel.\n\n", bind)
}
