package server

import (
	"context"
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// magic is BIP174's five bytes. A .psbt file starts with them, which is the
// whole reason the download decodes rather than serving the base64: a wallet
// with a file picker is reading the binary form.
var magic = []byte("psbt\xff")

// asking puts one question in front of a run and returns it, with a cleanup that
// releases the goroutine. The answer is discarded: these tests are about the
// packet leaving, not about it coming back.
func asking(t *testing.T, s *Server, runID string, q Question) (*Run, *Question) {
	t.Helper()
	run := s.Runs.add(runID)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if q.Deadline.IsZero() {
		q.Deadline = time.Now().Add(time.Minute)
	}
	go run.Ask(ctx, q)
	return run, awaitPending(t, run)
}

// TestThePacketDownloadsAsTheBinaryFile is the outbound leg of the file
// transport, and the binary half of it is the point.
//
// BIP174 defines the .psbt file as the raw serialisation; base64 is the encoding
// for a text transport, which is what the textarea beside this link already is.
// So the response has to be bytes starting with the magic, not the string the
// page shows — a wallet handed a base64 file may or may not read it, and the
// operator finds out at the worst moment.
func TestThePacketDownloadsAsTheBinaryFile(t *testing.T) {
	s := testServer(t)
	_, q := asking(t, s, "run-1", Question{
		Prompt:          "cold1: sign this and hand it back.",
		Payload:         "cHNidP8BAAA=",
		PayloadFilename: "rehearsal-cold1.psbt",
		Choices:         []Choice{{Value: "signed", Label: "signed"}},
	})

	w := serveIt(s, get(t, s, "/runs/run-1/payload/"+q.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("the download is %d, not 200:\n%s", w.Code, w.Body.String())
	}

	got := w.Body.Bytes()
	want, err := base64.StdEncoding.DecodeString("cHNidP8BAAA=")
	if err != nil {
		t.Fatalf("the fixture is not base64: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("the file is %x, not the decoded packet %x", got, want)
	}
	if !strings.HasPrefix(string(got), string(magic)) {
		t.Errorf("the file does not start with BIP174's magic: %x", got)
	}
	if strings.Contains(string(got), "cHNidP8") {
		t.Error("the base64 reached the file, so a wallet reading .psbt files " +
			"gets a text file under a binary name")
	}

	h := w.Header()
	if h.Get("Content-Type") != "application/octet-stream" {
		t.Errorf("Content-Type is %q", h.Get("Content-Type"))
	}
	if h.Get("Content-Length") != strconv.Itoa(len(want)) {
		t.Errorf("Content-Length is %q for %d bytes", h.Get("Content-Length"), len(want))
	}
	if cd := h.Get("Content-Disposition"); cd != `attachment; filename="rehearsal-cold1.psbt"` {
		t.Errorf("Content-Disposition is %q", cd)
	}
}

// TestTheDownloadNameCarriesTheRoundAndTheDevice is the reason the filename is on
// the Question at all, tested here because this package is where it reaches the
// header. internal/webrun's test is the other half: that it composes one.
//
// The rehearsal and the batch are two rounds minutes apart, asking the same m
// devices to sign two different transactions. Both files land in one Downloads
// folder. If they share a name the browser appends "(1)", and which file is
// which is then a question about download order — so the round and the device
// are in the name, and a signature over the decoy cannot be returned as the
// batch's by picking the wrong file.
func TestTheDownloadNameCarriesTheRoundAndTheDevice(t *testing.T) {
	s := testServer(t)

	for _, name := range []string{"rehearsal-cold1.psbt", "batch-cold1.psbt"} {
		_, q := asking(t, s, "run-"+name, Question{
			Prompt:          "sign this",
			Payload:         "cHNidP8BAAA=",
			PayloadFilename: name,
			Choices:         []Choice{{Value: "signed", Label: "signed"}},
		})

		w := serveIt(s, get(t, s, "/runs/run-"+name+"/payload/"+q.ID))
		if got := w.Header().Get("Content-Disposition"); !strings.Contains(got, name) {
			t.Errorf("Content-Disposition is %q, which does not name %q", got, name)
		}
	}
}

// TestADownloadNameCannotLeaveItsHeaderOrNameAPath.
//
// The name is built from config.Signer.Label, which is a string out of the
// operator's own configuration file. It ends up inside a quoted
// Content-Disposition parameter and then in a file name on the operator's own
// machine, so two things have to hold: a quote must not close the parameter
// early, and a slash or a leading dot must not name a path. The allowlist in
// fileName is what holds both, and this is the table.
func TestADownloadNameCannotLeaveItsHeaderOrNameAPath(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"a plain name is untouched", "batch-cold1.psbt", "batch-cold1.psbt"},
		{"the extension is added", "batch-cold1", "batch-cold1.psbt"},
		{"a quote cannot close the parameter", `batch-"; x="y.psbt`, "batch----x--y.psbt"},
		{"a newline cannot split the header", "batch\r\nX-Evil: 1.psbt", "batch--X-Evil--1.psbt"},
		{"a slash cannot name a directory", "../../etc/cron.d/x.psbt", "etc-cron.d-x.psbt"},
		{"a leading dot cannot hide the file", ".ssh-authorized_keys", "ssh-authorized_keys.psbt"},
		{"a label in another script keeps what it can", "冷1", "1.psbt"},
		{"an empty name falls back to the question", "", "winthistle-run-1-q3.psbt"},
		{"all punctuation falls back to the question", "../..", "winthistle-run-1-q3.psbt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := fileName("run-1", &Question{ID: "3", PayloadFilename: tc.in})
			if got != tc.want {
				t.Errorf("fileName(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if strings.ContainsAny(got, "\"\r\n/\\") {
				t.Errorf("fileName(%q) = %q, which is not safe in the header or "+
					"as a file name", tc.in, got)
			}
			if !strings.HasSuffix(got, ".psbt") {
				t.Errorf("fileName(%q) = %q, which a wallet's file picker will "+
					"filter out", tc.in, got)
			}
		})
	}
}

// TestTheHeaderSurvivesAHostileLabel is the same property one layer out: through
// the handler, on the wire, rather than against fileName directly.
func TestTheHeaderSurvivesAHostileLabel(t *testing.T) {
	s := testServer(t)
	_, q := asking(t, s, "run-1", Question{
		Prompt:          "sign this",
		Payload:         "cHNidP8BAAA=",
		PayloadFilename: "batch-\"; filename=\"evil.exe",
		Choices:         []Choice{{Value: "signed", Label: "signed"}},
	})

	w := serveIt(s, get(t, s, "/runs/run-1/payload/"+q.ID))
	// The property is not that the label's text disappears — it does not, and it
	// does not need to. It is that the label cannot become *syntax*: exactly one
	// quoted value, no second parameter, and the extension the browser saves it
	// under is still .psbt. So "evil.exe" survives as noise inside the file name
	// and names nothing.
	cd := w.Header().Get("Content-Disposition")
	if strings.Count(cd, `"`) != 2 {
		t.Errorf("Content-Disposition is %q, which has more than one quoted "+
			"parameter value", cd)
	}
	if strings.Count(cd, ";") != 1 {
		t.Errorf("Content-Disposition is %q, so the label added a parameter", cd)
	}
	if !strings.HasSuffix(cd, `.psbt"`) {
		t.Errorf("Content-Disposition is %q, so the file is not saved as a PSBT", cd)
	}
}

// TestADownloadForAQuestionTheRunHasMovedPastIsRefused is ErrStaleQuestion's
// property on the outbound leg.
//
// Reply already refuses an answer to a superseded question, for a reason that
// applies at least as strongly here: the two rounds of a batch ask the same
// devices about two different transactions, so a stale link that served the
// packet anyway would hand a device the wrong one to sign. The link carries the
// question id for exactly this check.
func TestADownloadForAQuestionTheRunHasMovedPastIsRefused(t *testing.T) {
	s := testServer(t)
	_, q := asking(t, s, "run-1", Question{
		Prompt:          "sign this",
		Payload:         "cHNidP8BAAA=",
		PayloadFilename: "rehearsal-cold1.psbt",
		Choices:         []Choice{{Value: "signed", Label: "signed"}},
	})

	// The id one past the live one is the next question this run will ask.
	next, err := strconv.Atoi(q.ID)
	if err != nil {
		t.Fatalf("the question id %q is not a counter", q.ID)
	}
	for _, id := range []string{strconv.Itoa(next + 1), strconv.Itoa(next - 1), "", "x"} {
		w := serveIt(s, get(t, s, "/runs/run-1/payload/"+id))
		if w.Code == http.StatusOK {
			t.Errorf("question id %q served the packet", id)
		}
		if strings.Contains(w.Body.String(), "psbt") &&
			strings.Contains(w.Body.String(), "cHNidP8") {

			t.Errorf("question id %q leaked the packet into the refusal", id)
		}
	}
}

// TestADownloadFromARunWithNothingPendingIsRefused. A tab left open on a question
// that has since been answered is the ordinary case, not an attack, and it must
// get a refusal that says what to do rather than a zero-byte file.
func TestADownloadFromARunWithNothingPendingIsRefused(t *testing.T) {
	s := testServer(t)
	s.Runs.add("run-1")

	w := serveIt(s, get(t, s, "/runs/run-1/payload/1"))
	if w.Code != http.StatusConflict {
		t.Errorf("a download from an idle run is %d, not 409", w.Code)
	}
	if body := pre(t, w.Body.String()); !strings.Contains(body, "no longer asking") {
		t.Errorf("the refusal does not say the question is gone:\n%s", body)
	}
	// The way back. The nav on this page has overview, doctor and recover on it
	// and no route to the run, so a refusal that only *names* the run screen is
	// a dead end — which is how it rendered before this link existed.
	if !strings.Contains(w.Body.String(), `href="/runs/run-1"`) {
		t.Errorf("the refusal has no link back to the run:\n%s", w.Body.String())
	}
}

// TestADownloadFromAQuestionWithNoPacketIsRefused. Three of the four seams ask
// for a decision rather than a signature and carry no payload, so the refusal
// has to say so rather than serve an empty file named .psbt.
func TestADownloadFromAQuestionWithNoPacketIsRefused(t *testing.T) {
	s := testServer(t)
	_, q := asking(t, s, "run-1", Question{
		Prompt:  "Abandon this channel with i_know_what_i_am_doing?",
		Choices: []Choice{{Value: "no", Label: "No"}},
	})

	w := serveIt(s, get(t, s, "/runs/run-1/payload/"+q.ID))
	if w.Code != http.StatusNotFound {
		t.Errorf("a download from a decision is %d, not 404", w.Code)
	}
	if w.Body.Len() == 0 {
		t.Error("the refusal is empty, so a browser saves it as the file")
	}
}

// TestADownloadFromAnUnknownRunIsRefused. Same screen as every other /runs path
// for an id this process is not driving: a 404 that says the journal is the other
// place to look, rather than a fallthrough.
func TestADownloadFromAnUnknownRunIsRefused(t *testing.T) {
	s := testServer(t)

	w := serveIt(s, get(t, s, "/runs/no-such-run/payload/1"))
	if w.Code != http.StatusNotFound {
		t.Errorf("a download from an unknown run is %d, not 404", w.Code)
	}
}

// TestTheDownloadIsBehindTheToken is the check worth writing once for this route
// specifically rather than trusting the middleware, because it is the first route
// in this package whose body is a PSBT rather than a screen.
//
// The guard wraps the whole mux so this cannot be forgotten by construction. What
// it is worth measuring is that the refusal comes back with none of the packet in
// it, and that the packet is not what the guard's own error body is built from.
func TestTheDownloadIsBehindTheToken(t *testing.T) {
	s := testServer(t)
	_, q := asking(t, s, "run-1", Question{
		Prompt:          "sign this",
		Payload:         "cHNidP8BAAA=",
		PayloadFilename: "rehearsal-cold1.psbt",
		Choices:         []Choice{{Value: "signed", Label: "signed"}},
	})

	r := get(t, s, "/runs/run-1/payload/"+q.ID)
	r.Header.Del("Cookie")
	w := serveIt(s, r)
	if w.Code == http.StatusOK {
		t.Fatal("the download was served without the token")
	}
	if strings.Contains(w.Body.String(), "cHNidP8") ||
		strings.Contains(w.Body.String(), "psbt\xff") {

		t.Errorf("the refusal carries the packet:\n%s", w.Body.String())
	}
}

// TestTheQuestionScreenOffersTheDownload. The link is chrome, like the form it
// sits in, so it is outside the <pre> — but it has to be there, and it has to
// carry the live question's id or the check above refuses it.
func TestTheQuestionScreenOffersTheDownload(t *testing.T) {
	s := testServer(t)
	_, q := asking(t, s, "run-1", Question{
		Prompt:          "sign this",
		Payload:         "cHNidP8BAAA=",
		PayloadFilename: "rehearsal-cold1.psbt",
		Choices:         []Choice{{Value: "signed", Label: "signed"}},
	})

	body := serveIt(s, get(t, s, "/runs/run-1")).Body.String()
	want := `href="/runs/run-1/payload/` + q.ID + `"`
	if !strings.Contains(body, want) {
		t.Errorf("the run screen has no download link (%s):\n%s", want, body)
	}
	if !strings.Contains(body, "<a download ") {
		t.Error("the link has no download attribute, so a browser may render " +
			"the PSBT instead of saving it")
	}
	// The textarea stays. The file is an addition to the copy-paste, not a
	// replacement: an operator on a wallet that takes pasted base64 should not
	// have to route through a file to get it.
	if !strings.Contains(body, `id="payload"`) {
		t.Error("the read-only field is gone, so the download replaced the paste " +
			"rather than joining it")
	}
}

// TestADecisionScreenOffersNoDownload. The link is generated from the payload, so
// a question with none must not carry a link to the refusal above.
func TestADecisionScreenOffersNoDownload(t *testing.T) {
	s := testServer(t)
	asking(t, s, "run-1", Question{
		Prompt:  "Abandon this channel with i_know_what_i_am_doing?",
		Choices: []Choice{{Value: "no", Label: "No"}},
	})

	body := serveIt(s, get(t, s, "/runs/run-1")).Body.String()
	if strings.Contains(body, "/payload/") {
		t.Errorf("a decision screen offers a download:\n%s", body)
	}
}

// TestTwoUnwritableLabelsStillDownloadUnderTwoNames is the defect the table
// above found, kept as its own test because it is the property rather than the
// mechanism.
//
// A label the allowlist has no letters for used to sanitise away to nothing, and
// every such device in every round then downloaded as one fixed name. The round
// and the device are in the name so that the rehearsal's packet and the batch's
// cannot be confused; a silent collapse to one name removes exactly that, with
// nothing on the screen to say so.
func TestTwoUnwritableLabelsStillDownloadUnderTwoNames(t *testing.T) {
	seen := map[string]string{}
	for i, label := range []string{"冷", "寒", "热"} {
		q := &Question{ID: strconv.Itoa(i + 1), PayloadFilename: label}
		name := fileName("run-1", q)
		if was, dup := seen[name]; dup {
			t.Errorf("%q and %q both download as %q", was, label, name)
		}
		seen[name] = label
	}
}
