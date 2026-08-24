package bump

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/AusDavo/winthistle/internal/journal"
	"github.com/AusDavo/winthistle/internal/prose"
)

// List shows the CPFP children the journal has left half-done.
//
// It belongs beside the run listing rather than inside it, because the two
// answers are different. An unfinished run may have peers holding reservations
// and channels in a half-open state, and the screen has to price an abort. An
// unfinished child costs one coin lock and nothing else: no peer was spoken to,
// no channel exists, and nothing about it can make a batch worse. Saying so
// plainly is the point — but not saying it at all would leave an operator told
// their node is clean while a coin of theirs is locked.
func List(ctx context.Context, j *journal.Journal, w io.Writer) error {
	bumps, err := j.UnfinishedBumps(ctx)
	if err != nil {
		return fmt.Errorf("reading the journal's CPFP children: %w", err)
	}
	if len(bumps) == 0 {
		return nil
	}

	fmt.Fprintf(w, "\n%d unfinished CPFP child%s.\n\n", len(bumps),
		childrenSuffix(len(bumps)))
	width := idColumn(bumps)
	for _, b := range bumps {
		fmt.Fprintf(w, "  %-*s  bump %-3d %-11s target %.2f sat/vB\n",
			width, b.RunID, b.Seq, b.State, b.Plan.TargetSatPerVB)
		// Everything under a row sits at one small indent rather than in a
		// column of its own. The column was fourteen wide and a run id is
		// twenty-two, so it lined up with the middle of the id above it — the
		// same defect the run list had, found the same way, in a browser.
		fmt.Fprintf(w, "%sparent\n", rowIndent)
		fmt.Fprintf(w, "%s  %s\n", rowIndent, b.Plan.ParentTxID)
		if b.ChildTxID != "" {
			fmt.Fprintf(w, "%schild\n", rowIndent)
			fmt.Fprintf(w, "%s  %s\n", rowIndent, b.ChildTxID)
		}
		if held := heldLocks(b); held > 0 {
			fmt.Fprintf(w, "%s%d coin lock%s still held on the batch's change\n",
				rowIndent, held, prose.Plural(held))
		}
	}

	fmt.Fprint(w, "\n")
	fmt.Fprint(w, prose.Para("Each of these is a coin lock and nothing more. No peer "+
		"was told anything, no channel exists, and the batch each one was meant to "+
		"accelerate is exactly where it was — so unlike an unfinished run, there is "+
		"nothing here to price. What a held lock does cost is a cold wallet that "+
		"declines to spend its own change and does not say why."))
	fmt.Fprint(w, "\n")
	fmt.Fprint(w, prose.Para("A child that is out there and needs to go faster is a "+
		"different thing from these, and it is not listed here: run "+
		"`winthistle bump` again and it replaces it. The child is built BIP-125 "+
		"replaceable so a second lift is a replacement rather than a chain of "+
		"transactions each paying for the last."))

	if anyMayBePublic(bumps) {
		fmt.Fprint(w, "\n")
		fmt.Fprint(w, prose.Para("One or more of these reached the publish call, so "+
			"its bytes may be in a mempool. Releasing the lock is still the right "+
			"thing — it takes nothing back and cannot strand anything — but do not "+
			"read it as the child having been called off. Look for the child's txid "+
			"in your own mempool if you need to know."))
	}
	fmt.Fprint(w, "\nGive one up with: winthistle bump <run-id> --abandon\n")
	return nil
}

// Give releases the coin locks of every unfinished child of one run.
//
// The whole of a bump's teardown, exposed as a command because the thing it
// fixes is invisible from anywhere else: Core leaves locked outputs out of
// listunspent, so a cold wallet holding a lock for a child that will never be
// signed looks exactly like a cold wallet whose change was already spent.
//
// It does not refuse a child that may be public, and it says why rather than
// silently doing it. Releasing a lock is not un-broadcasting anything.
func Give(ctx context.Context, d Deps, runID string) error {
	bumps, err := d.Journal.Bumps(ctx, runID)
	if err != nil {
		return err
	}

	var open []*journal.Bump
	for _, b := range bumps {
		if !b.Finished() || heldLocks(b) > 0 {
			open = append(open, b)
		}
	}
	if len(open) == 0 {
		if len(bumps) == 0 {
			return fmt.Errorf("run %s has no CPFP child in the journal", runID)
		}
		fmt.Fprint(d.Out, prose.Para(fmt.Sprintf(
			"Run %s has %d CPFP child(ren) and none of them is holding anything. "+
				"There is nothing to release.", runID, len(bumps))))
		return nil
	}

	var failures []error
	for _, b := range open {
		section(d.Out, fmt.Sprintf("Giving up on bump %d of run %s", b.Seq, runID))
		if b.MayBePublic() {
			fmt.Fprint(d.Out, prose.Para(fmt.Sprintf(
				"This child is %s, so its bytes may already be in a mempool. The "+
					"lock is being released anyway, which is safe and is not the same "+
					"as calling the child off: a transaction that is out is out, and a "+
					"local note saying \"do not spend this coin\" was never what was "+
					"holding it back. If you need to know, look for %s in your own "+
					"mempool.", b.State, orNone(b.ChildTxID))))
		}
		rep, err := d.Journal.AbandonBump(ctx, d.Wallet, runID, b.Seq)
		fmt.Fprint(d.Out, gaveUp(rep, err))
		if err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// rowIndent is where a child's details sit under its own line, and idColumn is
// how wide the run-id column is. Both are the run list's, for the same reasons:
// see prose.RecoveryList. The two screens are printed one after the other and an
// operator reads them as one page, so they line up the same way.
const rowIndent = "    "

func idColumn(bumps []*journal.Bump) int {
	const clamp = 24
	width := 0
	for _, b := range bumps {
		if n := len([]rune(b.RunID)); n > width {
			width = n
		}
	}
	if width > clamp {
		return clamp
	}
	return width
}

func heldLocks(b *journal.Bump) int {
	n := 0
	for _, l := range b.Locks {
		if !l.Released {
			n++
		}
	}
	return n
}

func anyMayBePublic(bumps []*journal.Bump) bool {
	for _, b := range bumps {
		if b.MayBePublic() {
			return true
		}
	}
	return false
}

func childrenSuffix(n int) string {
	if n == 1 {
		return ""
	}
	return "ren"
}

func orNone(s string) string {
	if s == "" {
		return "it"
	}
	return s
}
