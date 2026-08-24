package server

import (
	"fmt"
	"html"
	"net/http"
	"strings"
	"testing"

	"github.com/AusDavo/winthistle/internal/prose"
)

// pre returns the first <pre> block's content, undone.
//
// This is the oracle's reader, and it is deliberately literal: screen() writes
// "<pre>" immediately followed by the escaped text and immediately followed by
// "</pre>", with no whitespace of its own, so this cannot silently trim
// something the copy meant.
func pre(t *testing.T, body string) string {
	t.Helper()
	_, rest, ok := strings.Cut(body, "<pre>")
	if !ok {
		t.Fatalf("no <pre> in the page:\n%s", body)
	}
	inner, _, ok := strings.Cut(rest, "</pre>")
	if !ok {
		t.Fatalf("unterminated <pre> in the page:\n%s", body)
	}
	return html.UnescapeString(inner)
}

// TestTheTextIsTheOracle is decision 3's guard, and it is written for the commit
// that has not happened yet.
//
// The twelve Report() renderers are served verbatim for v1, so this test looks
// tautological today. It is not: it fixes the property that survives the day one
// of those screens is re-rendered as HTML. The recovery screen will be — CLAUDE.md
// says its wording is the highest-stakes copy in the product, and a terminal in a
// browser is not the last word on it — and on that day this test does not get
// deleted. It has to keep passing, which means the HTML has to be produced from
// the same string that prose's overrun tests measure, rather than beside it.
//
// The adversarial input is the second half. Copy in this repository contains <,
// >, & and quotes — "channel < the minimum", "you can't", every "n of m" — and a
// renderer that escapes for HTML and then forgets to unescape for a diff is how
// "don't" becomes "don&#39;t" in the one screen nobody wants to be surprised by.
func TestTheTextIsTheOracle(t *testing.T) {
	texts := []string{
		"Ready — 12 checks\n\n",
		"PublishTransaction & the <arm.Armed> it will not take.",
		`He said "don't abandon anything first" — and it's the one move that loses funds.`,
		"a batch of 3 channels: 2-of-3, m < n, 5 000 000 sat\n\ttabbed\r\nCRLF\n",
		prose.Para("A trailing space at the end of a wrapped line is still copy. " +
			"So is a line of exactly seventy-eight characters, which is where the " +
			"pane ends and where an escape that changed the length would show."),
		"", // an empty screen is a screen
	}

	for _, text := range texts {
		page := screen("test", "/", text)
		if got := pre(t, page); got != text {
			t.Errorf("the text did not survive the page.\n got: %q\nwant: %q", got, text)
		}
		if strings.Count(page, "<pre>") != 1 {
			t.Errorf("screen() produced %d <pre> blocks; the oracle assumes one",
				strings.Count(page, "<pre>"))
		}
	}
}

// TestTheChokepointEscapes: the same function that makes the oracle possible is
// what stops a transcript from becoming markup. A run's transcript carries peer
// pubkeys, file paths and error strings from LND and Core, none of which this
// repository chose.
func TestTheChokepointEscapes(t *testing.T) {
	page := screen("test", "/", `<script>alert(1)</script>`)
	if strings.Contains(page, "<script>") {
		t.Fatalf("a screen rendered a script tag:\n%s", page)
	}
	if !strings.Contains(page, "&lt;script&gt;") {
		t.Fatalf("the script tag was neither rendered nor escaped:\n%s", page)
	}
}

// TestTheRunScreenGoesThroughTheChokepoint proves the handler uses it, which is
// the half a unit test of screen() alone cannot show.
func TestTheRunScreenGoesThroughTheChokepoint(t *testing.T) {
	s := testServer(t)
	run := s.Runs.add("2026-08-23T19-04-00")
	transcript := "Peer 1 of 3 <alice> refused: capacity 5 000 000 sat & the " +
		"minimum is 20 000 000\n"
	if _, err := run.Write([]byte(transcript)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	w := serveIt(s, get(t, s, "/runs/"+run.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("attaching got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "<alice>") {
		t.Error("the transcript reached the page unescaped")
	}
	if got := pre(t, body); !strings.Contains(got, transcript) {
		t.Errorf("the transcript is not in the page verbatim:\n%s", got)
	}
}

// TestNoScreenFetchesAnything. The CSP in guard.go allows nothing but inline
// styles, so a stylesheet link or a script tag would fail in the browser rather
// than load — but it would fail silently, and a page that quietly stopped
// working is worse than one that never asked. Every screen is one <style> block
// and text.
func TestNoScreenFetchesAnything(t *testing.T) {
	s, _ := journalling(t)
	s.Runs.add("a-run")

	for _, path := range []string{
		"/", "/runs/a-run", "/runs/no-such-run",
		"/recover", "/recover/20260824-1930", "/recover/no-such-run",
		"/peers", "/fees", "/reserve",
	} {
		body := serveIt(s, get(t, s, path)).Body.String()
		for _, forbidden := range []string{
			"<script", "<link", "<img", "http://", "https://", "//fonts.",
		} {
			if strings.Contains(body, forbidden) {
				t.Errorf("%s contains %q, which the CSP will refuse to load",
					path, forbidden)
			}
		}
	}
}

// TestALongLineSoftWraps is what the page owes a report it must not reflow.
//
// A doctor report against a real node contains tokens prose.Wrap cannot break —
// an absolute path to winthistle.toml, a 66-character pubkey, a descriptor with
// its checksum — so lines past prose.PaneWidth are ordinary rather than a bug in
// the copy. Serving verbatim means the page cannot fix them by rewrapping, so it
// has to hold them: pre-wrap and a max-width in the same column the text was
// written to, which turns an overrun into a wrapped line instead of a horizontal
// scrollbar over eighty columns of prose.
func TestALongLineSoftWraps(t *testing.T) {
	page := screen("test", "/",
		"read: /home/someone/a/very/long/path/that/prose/cannot/break/winthistle.toml\n")

	for _, want := range []string{
		"white-space: pre-wrap",
		fmt.Sprintf("max-width: %dch", prose.PaneWidth),
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the page does not declare %q, so a line prose could not "+
				"break becomes a horizontal scrollbar", want)
		}
	}
	if strings.Contains(page, "overflow-x: scroll") {
		t.Error("the page scrolls horizontally rather than wrapping")
	}
}
