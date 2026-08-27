package run

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
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
// a wallet saying no: there is no channel through which it could. A moved txid is
// an I-3 breach, and the one failure where sending the operator to their device
// instead of to the file costs the most. The last is issue #22's refusal, which
// was rewritten specifically to avoid naming why a witness is missing — a signer
// that was never asked and a signer that declined produce the same file — and
// this frame used to write "declined" over the top of it, one call away.
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
		{
			"some inputs came back short of a witness",
			fmt.Errorf("batch-signed.txn: %w", combine.ErrIncompleteWitnesses),
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

// returningWallet is step 7 handing back exactly what step 4 produced, which is
// the operator saving the wrong file — or the right file before signing it.
type returningWallet struct{}

func (w returningWallet) Built(context.Context, []Recipient) ([]byte, error) {
	return nil, errors.New("step 4 is not what this fixture is for")
}

func (w returningWallet) Signed(_ context.Context, unsigned []byte) ([]byte, error) {
	return unsigned, nil
}

// TestASuccessfulSigningStepIsNotRecordedAsSigned is issue #37, and it is #25
// with the polarity reversed: that one wrote a false failure over a true row,
// this one wrote a false success.
//
// No node, for the same reason and by the same route as the test above. What is
// asserted is what sign() leaves in the journal when SigningWallet.Signed returns
// bytes, because bytes are the whole of what it establishes. On the .psbt branch
// those bytes have been through combine.Parse and nothing else — five magic
// bytes, a sniffer rather than a parser — so a wallet that hands back the
// unsigned transaction is indistinguishable from one that signed it until
// combine.Accept runs, and Accept runs in armWindow after this frame has
// returned. RecordSigner upserts on (run_id, label), so the write this removes
// did not add a row: it replaced the truthful awaiting one, and prose.signerNote
// rendered "1 signed" on the recovery screen for a run where nothing was.
//
// The stub returns the packet it was handed, which is the operator's mistake in
// its most ordinary form. The row it leaves must be the one step 7 wrote on the
// way in.
func TestASuccessfulSigningStepIsNotRecordedAsSigned(t *testing.T) {
	ctx := context.Background()

	j, err := journal.Open(ctx, filepath.Join(t.TempDir(), "runs.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer j.Close()

	id, err := lnd.NewPendingChanID()
	if err != nil {
		t.Fatal(err)
	}
	const runID = "run-37"
	if err := j.Begin(ctx, runID, []journal.NewChannel{{
		PendingChanID: id,
		PeerPubkey:    "02" + strings.Repeat("22", 32),
		AmountSat:     1_000_000,
	}}); err != nil {
		t.Fatalf("Begin: %v", err)
	}

	out := new(strings.Builder)
	d := Deps{Journal: j, Signing: returningWallet{}, Out: out}

	unsigned := []byte("the packet built at step 4")
	got, err := sign(ctx, d, Options{RunID: runID}, unsigned)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if string(got) != string(unsigned) {
		t.Errorf("sign returned %q, want the bytes the wallet handed back", got)
	}

	r, err := j.Load(ctx, runID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(r.Signers) != 1 {
		t.Fatalf("the journal holds %d signer rows, want 1", len(r.Signers))
	}
	if st := r.Signers[0].State; st == journal.SignerSigned {
		t.Errorf("the journal says the wallet signed, which nothing here checked "+
			"for: %s", st)
	} else if st != journal.SignerAwaiting {
		t.Errorf("the signer reads back as %s, want %s", st, journal.SignerAwaiting)
	}

	// The terminal carried the same claim, one output earlier. Keyed on the shape
	// of the old line rather than on the word, which the honest one still uses.
	flat := strings.Join(strings.Fields(out.String()), " ")
	if regexp.MustCompile(`\bsigned \(`).MatchString(flat) {
		t.Errorf("step 7 printed that the batch was signed:\n%s", flat)
	}
	if !strings.Contains(flat, "nothing has looked at it for signatures yet") {
		t.Errorf("step 7 does not say what it has not checked:\n%s", flat)
	}
}
