package run

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AusDavo/winthistle/internal/combine"
	"github.com/AusDavo/winthistle/internal/journal"
	"github.com/AusDavo/winthistle/internal/lnd"
)

// refusingWallet is step 7 going wrong, and step 4 is not this test's business.
type refusingWallet struct{ err error }

func (w refusingWallet) Built(context.Context, []Recipient) ([]byte, error) {
	return nil, errors.New("step 4 is not what this fixture is for")
}

func (w refusingWallet) Signed(context.Context, []byte) ([]byte, error) {
	return nil, w.err
}

// TestAFailedSigningStepIsRecordedAsAwaitedAndNeverAsDeclined is issue #25.
//
// No node: sign uses d.Out, d.Journal and d.Signing and nothing else, so Deps.LND
// stays nil, and journal.Begin reaches a run in a temp database. What is asserted
// is what the journal holds afterwards, because that is the half that persists —
// the recovery screen counts these rows later and renders a decline as "1
// declined", and an operator who reads that looks at their signing device.
//
// Every error class below comes out of FileWallet.Signed, and not one of them is
// a wallet saying no: there is no channel through which it could. The last is a
// moved txid, which is an I-3 breach and the one failure where sending the
// operator to their device instead of to the file costs the most.
func TestAFailedSigningStepIsRecordedAsAwaitedAndNeverAsDeclined(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		err  error
	}{
		{
			"the context was cancelled, which is Ctrl-C at the prompt",
			context.Canceled,
		},
		{
			"a stale file could not be cleared, before the wallet was prompted",
			fmt.Errorf("clearing batch-signed.psbt: %w", os.ErrPermission),
		},
		{
			"the file was neither encoding",
			errors.New("batch-signed.txn is neither a PSBT nor a raw " +
				"transaction this build can read"),
		},
		{
			"the txid moved, which is I-3",
			fmt.Errorf("batch-signed.txn: %w", combine.ErrTXIDMoved),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j, err := journal.Open(ctx, filepath.Join(t.TempDir(), "runs.db"))
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer j.Close()

			id, err := lnd.NewPendingChanID()
			if err != nil {
				t.Fatal(err)
			}
			const runID = "run-25"
			if err := j.Begin(ctx, runID, []journal.NewChannel{{
				PendingChanID: id,
				PeerPubkey:    "02" + strings.Repeat("11", 32),
				AmountSat:     1_000_000,
			}}); err != nil {
				t.Fatalf("Begin: %v", err)
			}

			d := Deps{
				Journal: j,
				Signing: refusingWallet{err: tc.err},
				Out:     io.Discard,
			}
			if _, err := sign(ctx, d, Options{RunID: runID}, nil); err == nil {
				t.Fatal("a step-7 failure returned no error, and the run carried on")
			} else if !errors.Is(err, tc.err) {
				t.Errorf("the failure lost its cause: %v", err)
			}

			r, err := j.Load(ctx, runID)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if len(r.Signers) != 1 {
				t.Fatalf("the journal holds %d signer rows, want 1", len(r.Signers))
			}
			got := r.Signers[0].State
			if got == journal.SignerDeclined {
				t.Errorf("the journal says the wallet declined, which nothing here "+
					"observed: %s", got)
			}
			if got != journal.SignerAwaiting {
				t.Errorf("the signer reads back as %s, want %s", got,
					journal.SignerAwaiting)
			}
		})
	}
}
