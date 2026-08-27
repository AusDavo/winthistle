package run

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/AusDavo/winthistle/internal/abort"
	"github.com/AusDavo/winthistle/internal/journal"
	"github.com/AusDavo/winthistle/internal/prose"
)

// Unfinished is the runs the journal shows as neither published nor aborted.
//
// Which is all the journal establishes — a run is in the list from the moment
// its streams open, so one being armed right now is in it too. The screen this
// prints hedges accordingly; see prose.RecoveryList.
//
// It listed the CPFP children too, until this build stopped building them. The
// pairing was the whole reason it existed rather than a bare j.Unfinished: a
// screen that listed the runs and not the children would have told somebody
// their node is clean while a coin of theirs was locked. There are no children
// now and nothing takes a coin lock, so what is left is one list — but the name
// stays, because it is the question the screen answers and a caller that wants
// "everything unfinished" should keep finding it here rather than reaching past
// it to the journal.
//
// Everything on it comes off the journal's own rows, so it is the same screen on
// a node that is down — which is the state an operator most often reads it in.
// Nothing here dials anything.
func Unfinished(ctx context.Context, j *journal.Journal, w io.Writer) (
	[]*journal.Run, error) {

	runs, err := j.Unfinished(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading the journal: %w", err)
	}
	fmt.Fprint(w, prose.RecoveryList(runs, time.Now()))
	return runs, nil
}

// Show is one run's recovery screen: what it is, and what an abort would do.
func Show(ctx context.Context, j *journal.Journal, w io.Writer, runID string) (
	*journal.Run, error) {

	run, err := j.Load(ctx, runID)
	if err != nil {
		return nil, err
	}
	fmt.Fprint(w, prose.Recovery(run, time.Now()))
	return run, nil
}

// RecoverOne aborts one journalled run and reports what happened.
//
// The same call the armed window makes when it fails, deliberately: a crash and
// a mid-run failure leave the same artifacts, and a recovery path that only the
// crash case exercised would be a path nothing tests.
//
// It refuses a run that reached the publish call, with journal.ErrMayBePublished
// — that transaction may be in a mempool or already mined, and abandoning a
// pending channel whose funding transaction then confirms strands its funds
// with no force-close path.
func RecoverOne(ctx context.Context, d Deps, runID string) (*abort.Report, error) {
	run, err := Show(ctx, d.Journal, d.Out, runID)
	if err != nil {
		return nil, err
	}

	rep, err := d.Journal.Recover(ctx, d.LND.Lightning, runID, d.Confirm)
	fmt.Fprint(d.Out, prose.RecoveryOutcome(run, rep, err))
	return rep, err
}
