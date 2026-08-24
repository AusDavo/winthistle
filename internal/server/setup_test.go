package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/AusDavo/winthistle/internal/prose"
)

// setupServer is a server that can run a setup, and nothing else configured.
func setupServer(t *testing.T) (*Server, *stubLauncher) {
	t.Helper()
	l := &stubLauncher{wallet: "winthistle-cold", batch: "1 channel, 5 000 000 sat"}
	return launching(t, l), l
}

// fitsThePane measures every line of a rendered screen against the column the
// copy was written to.
//
// The screens are served verbatim in a <pre> at prose.PaneWidth, so a paragraph
// that was never wrapped does not overflow a terminal — it overflows the page,
// which is worse, because the page is where an operator reads it. Three screens
// in a row shipped over the pane because nobody measured them.
//
// Lines prose cannot break are the exception and they are named rather than
// tolerated in general: a wallet name, a descriptor and a 66-character pubkey
// have no space in them to wrap at.
func fitsThePane(t *testing.T, name, text string) {
	t.Helper()
	for i, line := range strings.Split(text, "\n") {
		if n := len([]rune(line)); n > prose.PaneWidth {
			t.Errorf("%s, line %d is %d columns and the pane is %d:\n%s",
				name, i+1, n, prose.PaneWidth, line)
		}
	}
}

// TestTheSetupScreenFitsThePane, measured on the page rather than on the copy
// functions, because the page is what an operator reads and it is the composition
// — intro, then busy note, then form — that has been the thing to overflow.
func TestTheSetupScreenFitsThePane(t *testing.T) {
	s, _ := setupServer(t)

	w := serveIt(s, get(t, s, "/setup"))
	if w.Code != http.StatusOK {
		t.Fatalf("the setup screen got %d:\n%s", w.Code, w.Body.String())
	}
	fitsThePane(t, "/setup", pre(t, w.Body.String()))

	// And with the control withheld, which is a different composition.
	s.Runs.addKind("a-setup", KindSetup, "winthistle-cold")
	w = serveIt(s, get(t, s, "/setup"))
	fitsThePane(t, "/setup while something is going", pre(t, w.Body.String()))
}

// TestTheSetupScreenCannotNameAFile is the guard on the one decision this screen
// makes.
//
// `winthistle setup --descriptors FILE` installs, and this screen never does. A
// descriptor file needs a path, and a path posted from a browser is a browser
// choosing which file this process opens, imports and rescans against. So the
// form has no field at all — not a text field, not a file picker — and this is
// what says so if one is added.
func TestTheSetupScreenCannotNameAFile(t *testing.T) {
	s, _ := setupServer(t)

	body := serveIt(s, get(t, s, "/setup")).Body.String()
	form := body[strings.Index(body, `action="/setup"`):]
	for _, forbidden := range []string{"<input", "<textarea", "descriptor"} {
		if strings.Contains(form, forbidden) {
			t.Errorf("the setup form contains %q.\n"+
				"  This screen is the resume path and there is no field on it that "+
				"could make it the other one: a path posted from a browser is a "+
				"browser choosing which file this process reads and imports.",
				forbidden)
		}
	}
	if !strings.Contains(body, `method="post" action="/setup"`) {
		t.Error("the setup screen has no control on it at all")
	}
}

// TestStartingASetupGivesTheAdapterItsCaller.
//
// webrun.Ask has been built, unit tested and unreachable since the four callback
// seams landed, which is the one place written-but-uncalled code is deliberate in
// this repository. This is the assertion that it ended: a POST reaches
// StartSetup, and the run it reaches it with says what it is and what it is
// about, because every screen downstream reads those two fields to avoid saying
// batch things about a setup.
func TestStartingASetupGivesTheAdapterItsCaller(t *testing.T) {
	s, l := setupServer(t)
	l.block = make(chan struct{})
	defer close(l.block)

	w := serveIt(s, post(t, s, "/setup", url.Values{}))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("starting a setup got %d:\n%s", w.Code, w.Body.String())
	}
	l.runCtx(t)

	l.mu.Lock()
	n := l.setups
	l.mu.Unlock()
	if n != 1 {
		t.Fatalf("StartSetup was called %d times, want 1", n)
	}

	live := s.Runs.Live()
	if live == nil {
		t.Fatal("the setup is not in the registry, so no tab can come back to it")
	}
	if live.Kind != KindSetup {
		t.Errorf("the setup went into the registry as kind %d, not KindSetup", live.Kind)
	}
	if live.About != "winthistle-cold" {
		t.Errorf("the run does not name the wallet it is questioning: %q", live.About)
	}
	if want := "/runs/" + live.ID; w.Header().Get("Location") != want {
		t.Errorf("the POST redirected to %q, want %q",
			w.Header().Get("Location"), want)
	}
}

// TestTheSetupsContextIsNotTheRequests is decision 2 on the one question in this
// product that is *designed* to be walked away from.
//
// The operator is expected to close the laptop and go to a safe. If the response
// ending ended the question, the ordinary case would be the failing one.
func TestTheSetupsContextIsNotTheRequests(t *testing.T) {
	s, l := setupServer(t)
	l.block = make(chan struct{})
	defer close(l.block)

	reqCtx, closeTab := context.WithCancel(context.Background())
	if w := serveIt(s, post(t, s, "/setup", url.Values{}).WithContext(reqCtx)); w.Code != http.StatusSeeOther {
		t.Fatalf("starting a setup got %d", w.Code)
	}

	setupCtx := l.runCtx(t)
	closeTab()
	time.Sleep(50 * time.Millisecond)
	select {
	case <-setupCtx.Done():
		t.Fatal("closing the tab ended the setup's context.\n" +
			"  The address check is the one question built to be walked away from: " +
			"the operator is at a safe, and the same addresses have to be here " +
			"when they come back.")
	default:
	}
}

// TestASetupIsNotOfferedTheBatchsAbortControl.
//
// That control cancels an armed window with n shims, n pending channels and
// Core's coin locks behind it, and the screen behind it says so in three
// paragraphs. A setup has read two RPCs and is waiting on a person. Offering it
// there would be offering a teardown of nothing, described as a teardown of a
// batch — and the URL is reachable by hand and by a bookmark, so it is refused
// rather than merely unlinked.
func TestASetupIsNotOfferedTheBatchsAbortControl(t *testing.T) {
	s, _ := setupServer(t)
	sess := s.Runs.addKind("a-setup", KindSetup, "winthistle-cold")

	body := serveIt(s, get(t, s, "/runs/"+sess.ID)).Body.String()
	if strings.Contains(body, "/abort") {
		t.Error("the setup's run screen links to the batch's abort control")
	}
	mustSay(t, pre(t, body),
		"There is no control on this screen that stops a setup",
		"Every way of not answering it ends the same: nothing recorded")

	for _, r := range []*http.Request{
		get(t, s, "/runs/"+sess.ID+"/abort"),
		post(t, s, "/runs/"+sess.ID+"/abort", url.Values{}),
	} {
		w := serveIt(s, r)
		if w.Code != http.StatusNotFound {
			t.Fatalf("%s %s got %d, want 404", r.Method, r.URL.Path, w.Code)
		}
		got := pre(t, w.Body.String())
		if says(got, "abandon") && !says(got, "a setup has none of those") {
			t.Errorf("the refusal describes a batch teardown as though it "+
				"applied:\n%s", got)
		}
		mustSay(t, got, "run a-setup is not one",
			"Nothing about a setup needs stopping")
	}
}

// TestASetupAndABatchShareTheOneSlot, and the refusal must not explain a
// collision that could not have happened.
//
// Two batches collide over coin selection: the second one's dress rehearsal
// builds a decoy over the coins the first is about to spend. A setup colliding
// with a batch does not — it reads descriptors and derives addresses. The old
// copy said the dress-rehearsal sentence unconditionally, which would have been
// a false statement in operator copy the moment this screen existed.
func TestASetupAndABatchShareTheOneSlot(t *testing.T) {
	s, l := setupServer(t)
	l.block = make(chan struct{})
	defer close(l.block)

	if w := serveIt(s, post(t, s, "/setup", url.Values{})); w.Code != http.StatusSeeOther {
		t.Fatalf("starting the setup got %d", w.Code)
	}
	l.runCtx(t)

	w := serveIt(s, post(t, s, "/runs", url.Values{}))
	if w.Code != http.StatusConflict {
		t.Fatalf("starting a batch behind a setup got %d, want 409", w.Code)
	}
	got := pre(t, w.Body.String())
	if says(got, "dress rehearsal") {
		t.Errorf("the refusal explains a two-batch collision that did not "+
			"happen:\n%s", got)
	}
	mustSay(t, got, "A setup of the cold wallet winthistle-cold is going")

	// And the other direction: the setup screen withholds its control too, and
	// says which of the two is the reason.
	body := serveIt(s, get(t, s, "/setup")).Body.String()
	if strings.Contains(body, `action="/setup"`) {
		t.Error("the setup screen offers its control while a setup is going")
	}
}

// TestTheOverviewDoesNotDescribeASetupAsATwoBatchCollision.
//
// The overview withholds the batch control while anything is going, and the
// paragraph under it explains the collision. That paragraph is about coin
// selection — the second batch's dress rehearsal building a decoy over the coins
// the first is about to spend — and it is false about a setup, which reads
// descriptors and derives addresses. Found by looking at the page.
//
// The run list is the other half. Two rows that differ only by id are two rows an
// operator cannot tell apart, and a setup and a batch are not the same thing to
// attach to.
func TestTheOverviewDoesNotDescribeASetupAsATwoBatchCollision(t *testing.T) {
	s, _ := setupServer(t)
	s.Runs.addKind("a-setup", KindSetup, "winthistle-cold")

	body := serveIt(s, get(t, s, "/")).Body.String()
	if says(body, "dress rehearsal") {
		t.Errorf("the overview explains a two-batch collision over a setup:\n%s",
			body)
	}
	if !says(body, "A setup of the cold wallet winthistle-cold is going") {
		t.Errorf("the overview does not say what is going:\n%s", body)
	}
	if !says(body, "a setup of winthistle-cold, started") {
		t.Errorf("the run list does not say which kind the row is:\n%s", body)
	}

	// And a batch keeps the sentence that is true about it.
	s2, _ := setupServer(t)
	s2.Runs.add("20260824-1930")
	if body := serveIt(s2, get(t, s2, "/")).Body.String(); !says(body, "dress rehearsal") {
		t.Errorf("a live batch stopped explaining the collision it does have:\n%s",
			body)
	}
}

// TestTheSetupScreenIsAbsentRatherThanOfferedWithoutAWallet.
//
// The wallet's name is winthistle.toml's [bitcoind] wallet rather than a field on
// the page, because the wallet a setup questions has to be the wallet a batch
// will spend from. With no name there is nothing to question, and both the GET
// and the POST have to say the same thing: a screen that offered the control and
// a handler that then refused it teaches an operator to press it twice.
func TestTheSetupScreenIsAbsentRatherThanOfferedWithoutAWallet(t *testing.T) {
	s := launching(t, &stubLauncher{})

	for _, r := range []*http.Request{
		get(t, s, "/setup"),
		post(t, s, "/setup", url.Values{}),
	} {
		w := serveIt(s, r)
		if w.Code != http.StatusNotImplemented {
			t.Fatalf("%s /setup with no wallet got %d, want 501", r.Method, w.Code)
		}
		mustSay(t, pre(t, w.Body.String()), "[bitcoind] wallet")
	}

	// No launcher at all is the shape a test drives rather than one `winthistle
	// serve` produces, and it must not be a panic.
	bare := testServer(t)
	if w := serveIt(bare, get(t, bare, "/setup")); w.Code != http.StatusNotImplemented {
		t.Fatalf("/setup with no launcher got %d, want 501", w.Code)
	}
}

// TestASetupsScreenAsksTheJournalForNothing.
//
// progressBlock renders how many channels have their receipt, which is the number
// I-1 turns on and is a fact about a batch. A setup opens no channel, and its id
// is this process's rather than the journal's — so asking would be asking the
// journal about a run it has never had, and rendering the answer would be putting
// a batch's arithmetic on a screen with no batch in it.
func TestASetupsScreenAsksTheJournalForNothing(t *testing.T) {
	s, l := setupServer(t)
	sess := s.Runs.addKind("a-setup", KindSetup, "winthistle-cold")
	l.progress = map[string]string{sess.ID: "3 of 3 channels have their receipt"}

	got := pre(t, serveIt(s, get(t, s, "/runs/"+sess.ID)).Body.String())
	if says(got, "receipt") {
		t.Errorf("a setup's screen rendered a batch's progress:\n%s", got)
	}
}

// TestTheJournalScreenDoesNotPromiseARowASetupWillNeverWrite.
//
// journalNote's dangerous case is a live batch that has not written its row yet,
// where an empty list would otherwise read as "this node is clean". A setup is
// not that case and must not be described as it: it records its answer against
// the wallet, so it has no run row that is coming.
func TestTheJournalScreenDoesNotPromiseARowASetupWillNeverWrite(t *testing.T) {
	s, l := setupServer(t)
	l.unfinished = "No unfinished runs. Nothing to recover.\n"
	s.Runs.addKind("a-setup", KindSetup, "winthistle-cold")

	got := pre(t, serveIt(s, get(t, s, "/recover")).Body.String())
	if says(got, "has not written a row to this journal yet") {
		t.Errorf("the journal screen says a setup's run row is still coming:\n%s", got)
	}
	mustSay(t, got, "it will never appear in the list below")
}

// TestTheSetupsClockIsNotCalledAGate.
//
// Every other deadline in this UI is carved out of the signing gate, and the copy
// over a pending question says so. The address check's is not a gate on anything:
// nothing is armed, no peer knows it is happening, and letting it lapse records
// nothing. Calling it a signing gate would name a mechanism that is not running.
func TestTheSetupsClockIsNotCalledAGate(t *testing.T) {
	sess := &Run{ID: "a-setup", Kind: KindSetup, About: "winthistle-cold"}
	asked := time.Now()
	q := &Question{Asked: asked, Deadline: asked.Add(15 * time.Minute)}

	got := waitingOn(sess, q)
	if says(got, "signing gate") {
		t.Errorf("the setup's clock is called a signing gate:\n%s", got)
	}
	mustSay(t, got, "Letting it pass records nothing at all")

	batch := &Run{ID: "20260824-1930"}
	signing := &Question{Asked: asked, Deadline: asked.Add(5 * time.Minute),
		Reply: "paste what cold1 gave back, base64"}
	if got := waitingOn(batch, signing); !says(got, "signing gate") {
		t.Errorf("a batch's signing round stopped being the signing gate:\n%s", got)
	}

	// And a batch's *other* question is not a signing round either. The
	// blunt-abandon confirmation is asked during a teardown: nothing is being
	// signed, and letting it pass declines the escalation rather than costing one
	// more round. It said the signing-gate sentence for several slices, over the
	// one prompt in this product where a human authorises something that could
	// lose funds if the premise were wrong.
	decision := &Question{Asked: asked, Deadline: asked.Add(5 * time.Minute)}
	got = waitingOn(batch, decision)
	mustSay(t, got, "not a signing gate")
	if says(got, "one more signing round") {
		t.Errorf("the blunt-abandon confirmation says letting it pass costs a "+
			"signing round, when what it costs is an abort finished by hand:\n%s", got)
	}
	mustSay(t, got, "finished by hand")
}
