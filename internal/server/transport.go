package server

import (
	"encoding/base64"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"

	"github.com/AusDavo/winthistle/internal/prose"
)

// The file transport: the packet leaves as a download and comes back as an
// upload, around the same Question.Payload the textarea already shows.
//
// # Why a file at all, when the textarea works
//
// Because the textarea is a copy-paste of two to three kilobytes of base64, and
// the thing on the other end of it is a wallet with a file picker. docs/design.html
// makes the topology argument: the UI is viewed on the desktop through an ssh
// tunnel, so a download hands the PSBT to the desktop's wallet and an upload
// hands it back — no file sync, no shared mount, no extra daemon.
//
// # Binary, not base64 text
//
// BIP174 defines the .psbt file as the raw serialisation, magic bytes and all;
// base64 is the encoding for text transports, which is what the textarea is.
// A wallet that reads .psbt files reads the binary form, and only some of them
// also accept base64 in a file, so the download decodes.
//
// The decode is encoding/base64's StdEncoding, which is exactly what produced
// the string: internal/coldwallet's Build takes Core's `psbt` field and decodes
// it with base64.StdEncoding (build.go:184) to fill Built.Raw, and Built.PSBT —
// the string this handler is handed — is that same field untouched. Built.PSBT's
// own comment says "as Core returns it and as a browser download carries it".
//
// The upload leg accepts both forms, because an operator moving files by hand
// should not have to know which one their wallet wrote. See uploadedPayload.
//
// # What this handler is not
//
// It is not a second seam. There is one pending question per run and the
// download serves that question's payload, checked by id the same way an answer
// is: a browser holding an older screen must not be able to download the packet
// for a question the run has moved past, because the two questions in a signing
// round are about two different transactions.
//
// It reads. GET is the honest method for it, and nothing in it touches the run.

// downloadPayload is GET /runs/{id}/payload/{question}.
func (s *Server) downloadPayload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	run := s.Runs.Get(id)
	if run == nil {
		s.noSuchRun(w, id)
		return
	}

	q := run.Pending()
	if q == nil || q.ID != r.PathValue("question") {
		// The same refusal Reply gives, for the same reason. It does not name
		// the live question's id — the id is not something an operator types —
		// and it carries a link to the run screen rather than only telling them
		// to reload it: a browser that followed a stale link is on a page whose
		// nav has overview, doctor and recover on it and no route back to the
		// one screen being named. Found by rendering it.
		s.refuseRunScreen(w, http.StatusConflict, id, stalePayload(q))
		return
	}
	if q.Payload == "" {
		s.refuseScreen(w, http.StatusNotFound, "run "+id, prose.Para(
			"This question has no packet to download. It is asking for a decision "+
				"rather than a signature, so the answer is one of the buttons on the "+
				"run screen."))
		return
	}

	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(q.Payload))
	if err != nil {
		// Not reachable from internal/webrun, which carries Core's own base64
		// through untouched. It is here because the alternative to saying so is
		// serving a file that is not a PSBT under a name that says it is.
		s.refuseScreen(w, http.StatusInternalServerError, "run "+id, prose.Para(
			"The packet on this question is not base64, so there is no file to "+
				"hand over: "+err.Error()+". Nothing is wrong with the run — it is "+
				"still asking, and the packet is still readable in the field on the "+
				"run screen, which is what to sign from until this is fixed."))
		return
	}

	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", `attachment; filename="`+fileName(id, q)+`"`)
	h.Set("Content-Length", fmt.Sprint(len(raw)))
	w.Write(raw)
}

// fileName is the download's name, sanitised.
//
// Two things it has to survive. The first is a header: the name is built from
// config.Signer.Label, which is a string out of the operator's own
// configuration file, and it ends up inside a quoted Content-Disposition
// parameter. net/http turns newlines in a header value into spaces, so this is
// not a header-splitting hole — but a label containing a quote would still end
// the parameter early and hand the browser something else to parse, so the
// character set is an allowlist rather than a blocklist.
//
// The second is a file system, which is the operator's rather than ours: a label
// with a slash or a leading dot in it must not name a path. Only the allowlisted
// characters survive, so it cannot.
//
// The fallback is the third thing, and it was a defect until a test found it.
// A label written in a script this allowlist has no letters for — 冷1, 寒2 —
// sanitises to punctuation and then to nothing, so every device in every round
// downloaded under one fixed name. That is worse than a bad name: the round and
// the device are in the name precisely so two packets cannot be confused, and a
// silent collapse to one name removes that with no sign that anything happened.
// So the fallback is built from the run and the question id, which are a counter
// and therefore always distinct — the rehearsal's packet and the batch's still
// arrive under two names even when the label contributes nothing.
func fileName(runID string, q *Question) string {
	// Anything outside the allowlist becomes a dash rather than vanishing, so
	// two labels that differ only in punctuation still produce two names.
	name := safe(q.PayloadFilename)
	if name == "" {
		name = "winthistle-" + safe(runID) + "-q" + safe(q.ID)
		if name == "winthistle--q" {
			name = "winthistle"
		}
	}
	if !strings.HasSuffix(name, ".psbt") {
		name += ".psbt"
	}
	return name
}

// safe is fileName's allowlist over one component, without the extension or the
// fallback. Split out because the fallback is built from two of them.
func safe(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), ".-")
}

// stalePayload is the copy for a download link that is no longer live.
func stalePayload(live *Question) string {
	var b strings.Builder
	b.WriteString(prose.Para(
		"That download is for a question this run is no longer asking, so it was " +
			"not served. The two rounds of a batch ask the same devices about two " +
			"different transactions — a decoy first, then the batch — and handing " +
			"over the wrong one would produce a signature over the wrong packet."))
	if live == nil {
		b.WriteString("\n")
		b.WriteString(prose.Para(
			"This run is not waiting on anything at the moment. The link below " +
				"goes to the run screen, which says what it is doing."))
		return b.String()
	}
	b.WriteString("\n")
	b.WriteString(prose.Para(
		"It is asking something else now. Take the packet from the run screen " +
			"instead of from this link — it is below."))
	return b.String()
}

// runPath is the run screen, for a refusal that names it.
func runPath(runID string) string {
	return html.EscapeString("/runs/" + url.PathEscape(runID))
}

// payloadPath is the download link for a question. Generated in one place
// because the id in it is what makes the refusal above possible.
func payloadPath(runID string, q *Question) string {
	return html.EscapeString("/runs/" + url.PathEscape(runID) +
		"/payload/" + url.PathEscape(q.ID))
}
