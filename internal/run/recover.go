package run

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/AusDavo/winthistle/internal/abort"
	"github.com/AusDavo/winthistle/internal/bump"
	"github.com/AusDavo/winthistle/internal/journal"
	"github.com/AusDavo/winthistle/internal/prose"
)

// Unfinished is everything one journal can have left half-done: the runs that
// stopped somewhere they should not have, and then the CPFP children.
//
// The pairing is the whole reason this function exists rather than two exported
// ones. A screen that listed the runs and not the children would tell somebody
// their node is clean while a coin of theirs is locked — Core leaves locked
// outputs out of listunspent, so a held lock looks exactly like change that was
// already spent — and a screen that listed only the children would be missing
// the half that has peers holding reservations. Both front doors call this, so
// neither can grow half a screen: `winthistle recover` with no argument, and the
// web UI's /recover.
//
// Everything on it comes off the journal's own rows, so it is the same screen on
// a node that is down — which is the state an operator most often reads it in.
// Nothing here dials LND or Core.
func Unfinished(ctx context.Context, j *journal.Journal, w io.Writer) (
	[]*journal.Run, error) {

	runs, err := listRuns(ctx, j, w)
	if err != nil {
		return nil, err
	}
	if err := bump.List(ctx, j, w); err != nil {
		return runs, err
	}
	return runs, nil
}

// listRuns is the run half, and it is deliberately not exported.
//
// It was List until the web UI needed the same screen. An exported function that
// lists the runs and not the children is a trap: it reads like the whole answer,
// it is the obvious thing for a third front door to call, and what it leaves out
// is invisible from the screen it produces. Unfinished is the only way in.
func listRuns(ctx context.Context, j *journal.Journal, w io.Writer) (
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

	rep, err := d.Journal.Recover(ctx, d.LND.Lightning, d.Wallet, runID, d.Confirm)
	fmt.Fprint(d.Out, prose.RecoveryOutcome(run, rep, err))
	return rep, err
}
