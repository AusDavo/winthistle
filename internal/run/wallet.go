package run

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/AusDavo/winthistle/internal/combine"
	"github.com/AusDavo/winthistle/internal/prose"
)

// SigningWallet is Sparrow, or anything that builds a transaction to addresses
// you paste in and exports a PSBT.
//
// Two calls, because after the inversion the wallet is asked for two different
// things at two moments that are not adjacent. Step 4 wants the funded, unsigned
// transaction, and it is inside clock A with n peers holding reservations. Step 7
// wants signatures on that same transaction, with the gate already open and no
// clock that costs a restart. Between them the app verifies the packet against
// the plan, hands it to all n streams and collects n chan_pending receipts.
//
// The names say what comes back rather than what the wallet does, because this
// program does neither: it does not build the transaction and it does not hold a
// key. What it does is refuse the ones that are wrong.
//
// # Why this is not the Signers seam
//
// Signers hands out m devices, each returning a partial signature, and the
// rehearsal measures the round they make. That shape had a reason and the reason
// is gone: I-2 is dissolved, so the packet no longer has to come back in pieces,
// and the round is no longer inside the window a rehearsal could predict. It also
// never had a step 4 — there is no call on it that means "give me a transaction
// you funded yourself" — so half of this seam could not have been built out of it
// however the other half went. Signers survives for the CPFP child, which still
// is a multi-device round.
type SigningWallet interface {
	// Built is step 4: the funded, unsigned PSBT paying every recipient in pay.
	// It must be unsigned — see FileWallet.Built for why that is I-1 rather than
	// tidiness.
	//
	// pay is data, and the table an operator reads is copy: internal/run prints
	// the addresses in full to Out before calling this, because they are
	// copy-pasted from a terminal. A transport with a human at the other end
	// ignores the argument, and one with a program at the other end is the reason
	// it is there — a fixture or a future transport should not have to scrape the
	// transcript to find out what the batch pays.
	Built(ctx context.Context, pay []Recipient) ([]byte, error)

	// Signed is step 7: the same transaction with signatures on it. unsigned is
	// the packet this app verified and LND pinned, passed so that an
	// implementation which has to hand something to the operator has the exact
	// bytes rather than a second copy of them.
	Signed(ctx context.Context, unsigned []byte) ([]byte, error)
}

// DefaultPoll is how often a file transport looks for the file it is waiting on.
const DefaultPoll = 2 * time.Second

// FileWallet is the CLI's transport: a path in, a path out, and no assumption
// about what is at the other end.
//
// What carries the PSBT is a file, both directions, because Sparrow is on the
// same desktop as the devices. No browser, no tunnel, no file sync, no shared
// mount, no extra daemon.
type FileWallet struct {
	// Unsigned is where the operator saves the transaction they built at step 4.
	Unsigned string

	// Out is where the instructions go. Nil means os.Stdout.
	Out io.Writer

	// Poll is how often to look. Zero means DefaultPoll.
	Poll time.Duration
}

// NewFileWallet checks the paths before a peer has been told anything.
//
// It refuses a path that already exists, and that is a rule rather than caution:
// the funding addresses this transaction pays to did not exist before this run —
// LND issued them at step 2 — so a file that predates the run cannot be a
// transaction for it. Reading one would arm a batch against somebody else's
// outputs and find out at step 5. Refusing here costs nothing, because nothing
// has been opened yet.
func NewFileWallet(path string, out io.Writer) (*FileWallet, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("no PSBT path: --psbt FILE is where the transaction " +
			"you build in Sparrow gets saved, and where this run reads it from")
	}
	path = filepath.Clean(path)
	if _, err := os.Stat(path); err == nil {
		return nil, fmt.Errorf("%s already exists. The funding addresses this batch "+
			"pays to do not exist yet — LND issues them when the streams open — so a "+
			"file that is already there cannot be this batch's transaction. Name a "+
			"path that does not exist, or move that one aside", path)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("looking at %s: %w", path, err)
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("making %s: %w", dir, err)
		}
	}
	if out == nil {
		out = os.Stdout
	}
	return &FileWallet{Unsigned: path, Out: out}, nil
}

// SignedPath is where the signed transaction goes: the unsigned path with
// "-signed" before the extension.
//
// Derived rather than asked for, so the two paths cannot be given the same value.
// A wallet that wrote the signed transaction over the unsigned one would leave
// nothing to compare it against, and "is this the file I already read" is a
// question this transport should not have to answer.
func (w *FileWallet) SignedPath() string {
	ext := filepath.Ext(w.Unsigned)
	return strings.TrimSuffix(w.Unsigned, ext) + "-signed" + ext
}

// Built waits for the unsigned transaction and refuses a signed one.
//
// # The refusal is I-1, not tidiness
//
// Step 4 is before the gate. If the wallet signs at this point, the operator is
// holding a broadcastable funding transaction while nothing has reached
// chan_pending — and Sparrow has a broadcast button two clicks from the signing
// one. A transaction that reaches the network there defeats I-1 from outside: the
// peers have accepted the channels but stored no commitment signatures, so a
// confirmation would leave n 2-of-2 outputs with no channel behind them. That is
// the one thing the whole sequence is arranged to prevent, and the ten seconds
// between "Sign" and "Broadcast" is the only place it can still happen.
//
// So the copy says do not sign yet, and this refuses the file if the copy was not
// read. It costs a redo inside clock A, which is what every step-4 mistake costs.
func (w *FileWallet) Built(ctx context.Context, _ []Recipient) ([]byte, error) {
	// The recipients are not repeated here. internal/run has already printed them
	// to the same writer, in full and aligned, and a second rendering of the
	// addresses would be a second thing to keep in step with the plan.
	fmt.Fprint(w.Out, prose.Bullet("Save it here, unsigned. Binary or base64, "+
		"either is read:"))
	fmt.Fprintf(w.Out, "      %s\n", w.Unsigned)

	raw, err := w.wait(ctx, w.Unsigned, "the transaction you built")
	if err != nil {
		return nil, err
	}
	if err := combine.Unsigned(raw); err != nil {
		return nil, fmt.Errorf("%s carries signatures already: %w.\n\nNothing is "+
			"lost and nothing is at risk yet — no channel has been told about this "+
			"transaction. Delete that file, build the transaction again without "+
			"signing it, and save it there. Then sign at step 7, when every channel "+
			"is already recoverable and broadcasting early cannot cost anything",
			w.Unsigned, err)
	}
	return raw, nil
}

// Signed waits for the signed transaction.
func (w *FileWallet) Signed(ctx context.Context, unsigned []byte) ([]byte, error) {
	got := w.SignedPath()

	// Remove the answer before asking the question, so a file left by an earlier
	// attempt cannot be read as this one's. The unsigned path is guaranteed fresh
	// by NewFileWallet; this one is ours to name and therefore ours to clear.
	if err := os.Remove(got); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("clearing %s: %w", got, err)
	}

	fmt.Fprint(w.Out, prose.Bullet("Sign the transaction you built — the same one, "+
		"unchanged — and save it here:"))
	fmt.Fprintf(w.Out, "      %s\n", got)
	fmt.Fprint(w.Out, prose.Bullet("Let the wallet finalize it. A complete "+
		"transaction is what this step wants now: every channel in the batch is "+
		"already recoverable, so a wallet holding signed bytes front-runs nothing."))

	return w.wait(ctx, got, "the signed transaction")
}

// wait polls for a file and reads it through the one sniffer.
//
// Nothing bounds it but the context. The two waits have different clocks above
// them and neither is this transport's to enforce: step 4 is inside the peers'
// ten minutes, where a deadline of ours would abort a batch the peers were still
// holding, and step 7 has no deadline at all now that the gate is open. What ends
// either is the operator, or the run's own context.
func (w *FileWallet) wait(ctx context.Context, path, what string) ([]byte, error) {
	poll := w.Poll
	if poll <= 0 {
		poll = DefaultPoll
	}
	tick := time.NewTicker(poll)
	defer tick.Stop()

	for {
		body, err := os.ReadFile(path)
		switch {
		case err == nil:
			raw, perr := combine.Parse(body)
			if perr != nil {
				return nil, fmt.Errorf("%s is not a PSBT this build can read: %w",
					path, perr)
			}
			return raw, nil
		case !os.IsNotExist(err):
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for %s at %s: %w", what, path, ctx.Err())
		case <-tick.C:
		}
	}
}
