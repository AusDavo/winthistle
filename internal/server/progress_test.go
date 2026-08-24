package server

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

// The attach screen's whole job during the armed window is the receipt count,
// and it comes from the journal through the launcher rather than from the Run —
// which holds only the transcript the run wrote. This is that wiring.
func TestTheAttachScreenShowsTheJournalsAccountOfTheRun(t *testing.T) {
	l := &stubLauncher{progress: map[string]string{
		"2026-08-24T09-00-00": "phase: arming\n\n" +
			"  receipts      2 of 3\n" +
			"  funding tx    HELD — nothing has been broadcast\n" +
			"  peer window   about 6m41s left for the channels still waiting\n",
	}}
	s := launching(t, l)
	run := s.Runs.add("2026-08-24T09-00-00")
	if _, err := run.Write([]byte("Phase 1 — the armed window\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	w := serveIt(s, get(t, s, "/runs/"+run.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("attaching got %d", w.Code)
	}
	got := pre(t, w.Body.String())

	for _, want := range []string{
		"receipts      2 of 3",
		"funding tx    HELD",
		"peer window   about 6m41s",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the screen does not carry %q:\n%s", want, got)
		}
	}

	// State above transcript. The transcript grows without bound, so a receipt
	// count printed after it is one nobody scrolls to.
	if strings.Index(got, "receipts      2 of 3") >
		strings.Index(got, "Phase 1 — the armed window") {
		t.Error("the state block is below the transcript, which is where it gets " +
			"pushed off the screen by the run's own output")
	}
}

// A run that has journalled nothing has opened no stream. That is ordinary — it
// is every run for its first moments — so the screen shows the transcript alone
// rather than an error about a missing run.
func TestARunWithNothingJournalledYetIsNotAnError(t *testing.T) {
	s := launching(t, &stubLauncher{progress: map[string]string{}})
	run := s.Runs.add("fresh")
	if _, err := run.Write([]byte("Phase 0 — the cold wallet's setup\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	w := serveIt(s, get(t, s, "/runs/fresh"))
	if w.Code != http.StatusOK {
		t.Fatalf("attaching got %d", w.Code)
	}
	got := pre(t, w.Body.String())
	if !strings.Contains(got, "Phase 0") {
		t.Error("the transcript is missing")
	}
	if strings.Contains(got, "could not be read") {
		t.Error("a run with nothing journalled was reported as a journal failure")
	}
}

// A journal that cannot be read is said out loud. The receipt count is the live
// reading of I-1, and silently dropping it would make a run whose state is
// unknown look like one with no channels.
func TestAnUnreadableJournalIsSaidRatherThanOmitted(t *testing.T) {
	s := launching(t, &stubLauncher{
		journalErr: errors.New("disk I/O error"),
		progress:   map[string]string{},
	})
	run := s.Runs.add("unreadable")
	if _, err := run.Write([]byte("Phase 1\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	w := serveIt(s, get(t, s, "/runs/unreadable"))
	if w.Code != http.StatusOK {
		t.Fatalf("attaching got %d, want the screen to still render", w.Code)
	}
	got := pre(t, w.Body.String())
	if !strings.Contains(got, "could not be read") {
		t.Errorf("an unreadable journal is not mentioned:\n%s", got)
	}
	if !strings.Contains(got, "disk I/O error") {
		t.Error("the underlying error is not shown, so there is nothing to act on")
	}
	if !strings.Contains(got, "Phase 1") {
		t.Error("the transcript was dropped along with the state block")
	}
}
