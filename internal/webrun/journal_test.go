package webrun

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AusDavo/winthistle/internal/bitcoind"
	"github.com/AusDavo/winthistle/internal/config"
	"github.com/AusDavo/winthistle/internal/journal"
	"github.com/AusDavo/winthistle/internal/lnd"
	"github.com/AusDavo/winthistle/internal/prose"
	"github.com/AusDavo/winthistle/internal/server"
)

// The two read-only screens are the only thing in this package that touches the
// journal without a run behind it, and they are the screens an operator reads on
// a node that is down. So every test here builds a real journal in a temporary
// directory and dials nothing at all: no LND, no Core, no regtest. If one of
// these ever needs a node, that is the property breaking rather than the test
// getting harder to write.

const journalTxID = "3f2a9c5e1b7d4086a1c3e5f70981b2d4c6e8f0a2b4c6d8e0f2a4b6c8d0e2f406"

// journalAt is a launcher over a fresh journal, and the journal itself for the
// test to write into.
func journalAt(t *testing.T) (*Launcher, *journal.Journal, context.Context) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "runs.db")

	j, err := journal.Open(ctx, path)
	if err != nil {
		t.Fatalf("opening the journal: %v", err)
	}
	t.Cleanup(func() { j.Close() })

	cfg := &config.Config{Server: config.Server{
		Bind:    config.DefaultBind,
		Journal: path,
	}}
	return New(cfg, nil), j, ctx
}

func chanID(t *testing.T, b byte) lnd.PendingChanID {
	t.Helper()
	var id lnd.PendingChanID
	id[0] = b
	return id
}

// beginRun puts one arming run in the journal, which is where Unfinished finds
// it: arming is neither published nor aborted.
func beginRun(t *testing.T, j *journal.Journal, ctx context.Context, id string) {
	t.Helper()
	err := j.Begin(ctx, id, []journal.NewChannel{{
		PendingChanID: chanID(t, 1),
		PeerPubkey:    "02cca6c5c966fcf61d121e3a70e03a1cd9eeeea024b26ea666ce974d43b242e636",
		AmountSat:     250_000,
	}})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
}

// publishedRun drives a run to StatePublished, which is the only state a CPFP
// child may be started against.
func publishedRun(t *testing.T, j *journal.Journal, ctx context.Context, id string) {
	t.Helper()
	ids := []lnd.PendingChanID{chanID(t, 7)}
	err := j.Begin(ctx, id, []journal.NewChannel{{
		PendingChanID: ids[0],
		PeerPubkey:    "02cca6c5c966fcf61d121e3a70e03a1cd9eeeea024b26ea666ce974d43b242e636",
		AmountSat:     250_000,
	}})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := j.MarkVerified(ctx, id, ids[0]); err != nil {
		t.Fatalf("MarkVerified: %v", err)
	}
	if err := j.RecordFinalizedTx(ctx, id, journalTxID, "00"); err != nil {
		t.Fatalf("RecordFinalizedTx: %v", err)
	}
	cp := lnd.ChannelPoint{TxID: journalTxID, Index: 0}
	if err := j.MarkPending(ctx, id, ids[0], cp); err != nil {
		t.Fatalf("MarkPending: %v", err)
	}
	if err := j.MarkPublishing(ctx, id); err != nil {
		t.Fatalf("MarkPublishing: %v", err)
	}
	if err := j.MarkPublished(ctx, id); err != nil {
		t.Fatalf("MarkPublished: %v", err)
	}
}

// TestTheRecoveryListPairsRunsWithTheirChildren is the pairing, enforced.
//
// `winthistle recover` with no argument lists the runs and then the CPFP
// children, and the web screen has to do the same. The reason is not symmetry:
// Core leaves locked outputs out of listunspent, so an unfinished child looks
// exactly like change that was already spent — and a screen that listed only the
// runs would tell somebody their node is clean while a coin of theirs is locked.
//
// run.Unfinished is what makes it structural rather than remembered: it writes
// both halves and there is no exported way to get one without the other. This
// test is what notices if that changes.
func TestTheRecoveryListPairsRunsWithTheirChildren(t *testing.T) {
	l, j, ctx := journalAt(t)

	// A journal whose only unfinished thing is a child. The run itself is
	// published, so the run half of the screen has nothing to say — which is
	// exactly the case where dropping the children would read as "clean".
	publishedRun(t, j, ctx, "20260824-1900")
	if _, err := j.BeginBump(ctx, "20260824-1900", journal.BumpPlan{
		ParentTxID:     journalTxID,
		ParentVsizeVB:  7_007,
		ParentFeeSat:   7_007,
		Change:         bitcoind.Outpoint{TxID: journalTxID, Vout: 3},
		ChangeSat:      400_000,
		TargetSatPerVB: 20,
	}); err != nil {
		t.Fatalf("BeginBump: %v", err)
	}

	text, ids, err := l.Unfinished(ctx)
	if err != nil {
		t.Fatalf("Unfinished: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("a published run is not unfinished, but it is listed: %v", ids)
	}
	for _, want := range []string{
		"No unfinished runs",
		"unfinished CPFP child",
		"coin lock",
		"winthistle bump",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the recovery screen is missing %q — a screen that lists the "+
				"runs and not the children says a node is clean while a coin is "+
				"locked:\n%s", want, text)
		}
	}
}

// TestTheRecoveryListReturnsTheIdsItRendered. The ids are the one thing that
// crosses this boundary as data rather than as copy, because a link needs them
// and prose does not render links.
func TestTheRecoveryListReturnsTheIdsItRendered(t *testing.T) {
	l, j, ctx := journalAt(t)
	beginRun(t, j, ctx, "20260824-1901")
	beginRun(t, j, ctx, "20260824-1902")

	text, ids, err := l.Unfinished(ctx)
	if err != nil {
		t.Fatalf("Unfinished: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("Unfinished returned %d ids, want 2: %v", len(ids), ids)
	}
	for _, id := range ids {
		if !strings.Contains(text, id) {
			t.Errorf("id %s is not in the text it is supposed to link into:\n%s",
				id, text)
		}
	}
}

// TestTheRecoveryScreensFitThePane. Both of these are served verbatim in a <pre>
// at prose.PaneWidth and both are read at the worst moment there is, so they are
// measured here as well as in internal/prose — this is the composition, and it is
// what the browser actually gets.
func TestTheRecoveryScreensFitThePane(t *testing.T) {
	l, j, ctx := journalAt(t)
	beginRun(t, j, ctx, "20260824-1903")
	publishedRun(t, j, ctx, "20260824-1904")
	if _, err := j.BeginBump(ctx, "20260824-1904", journal.BumpPlan{
		ParentTxID:     journalTxID,
		ParentVsizeVB:  7_007,
		ParentFeeSat:   7_007,
		Change:         bitcoind.Outpoint{TxID: journalTxID, Vout: 3},
		ChangeSat:      400_000,
		TargetSatPerVB: 20,
	}); err != nil {
		t.Fatalf("BeginBump: %v", err)
	}

	list, _, err := l.Unfinished(ctx)
	if err != nil {
		t.Fatalf("Unfinished: %v", err)
	}
	one, err := l.Journalled(ctx, "20260824-1903")
	if err != nil {
		t.Fatalf("Journalled: %v", err)
	}

	for name, text := range map[string]string{"list": list, "one run": one} {
		for i, line := range strings.Split(text, "\n") {
			if n := len([]rune(line)); n > prose.PaneWidth {
				t.Errorf("%s line %d is %d columns, over the %d-column pane:\n%s",
					name, i+1, n, prose.PaneWidth, line)
			}
		}
	}
}

// TestAMissingJournalledRunIsTheServersSentinel.
//
// internal/server may not import internal/journal, so it cannot name
// journal.ErrNoRun. The translation happens here, and it has to be exact: the
// difference between "no such run" and "the journal could not be read" is the
// difference between a 404 and a page that must not pretend it looked.
func TestAMissingJournalledRunIsTheServersSentinel(t *testing.T) {
	l, j, ctx := journalAt(t)
	beginRun(t, j, ctx, "20260824-1905")

	if _, err := l.Journalled(ctx, "never-happened"); !errors.Is(err, server.ErrNoJournalledRun) {
		t.Fatalf("Journalled of an unknown run returned %v, want %v",
			err, server.ErrNoJournalledRun)
	}
	if _, err := l.Journalled(ctx, "20260824-1905"); err != nil {
		t.Fatalf("Journalled of a run that is there: %v", err)
	}
}

// TestTheRecoveryScreensDoNotNeedANode is the property the whole screen exists
// for, stated as a test rather than as a promise in a comment.
//
// The launcher here has no LND and no Core in its configuration — the fields are
// empty strings, which would fail to dial anything — and both screens still
// render. `winthistle serve` on a machine where LND is down is the state an
// operator reads this in.
func TestTheRecoveryScreensDoNotNeedANode(t *testing.T) {
	l, j, ctx := journalAt(t)
	beginRun(t, j, ctx, "20260824-1906")

	if l.cfg.LND.Address != "" || l.cfg.Bitcoind.Address != "" {
		t.Fatalf("this test is not measuring what it says: the configuration " +
			"names a node")
	}
	if _, _, err := l.Unfinished(ctx); err != nil {
		t.Errorf("the recovery list needed something it should not: %v", err)
	}
	if _, err := l.Journalled(ctx, "20260824-1906"); err != nil {
		t.Errorf("the recovery screen needed something it should not: %v", err)
	}
}
