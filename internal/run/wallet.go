package run

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
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
// # Why this replaced the m-device seam
//
// The seam before this one handed out m devices, each returning a partial
// signature, and a dress rehearsal measured the round they made. That shape had
// a reason and the reason is gone: I-2 is dissolved, so the packet no longer has
// to come back in pieces, and the round is no longer inside a window anything
// could usefully predict. It also never had a step 4 — there was no call on it
// meaning "give me a transaction you funded yourself" — so half of this seam
// could not have been built out of it however the other half went. It is
// deleted, along with the CPFP child that was its last caller.
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

// RawTXExtension is what a wallet asked to save a finalized transaction calls
// it. Sparrow does; so does anything else offering to export the finished bytes.
const RawTXExtension = ".txn"

// SignedPaths is every path Signed will read, in the order it prefers them.
//
// Step 7 takes a signed PSBT *or* a finalized raw transaction, and SignedPath
// inherits its extension from --psbt — so on the documented invocation it names
// batch-signed.psbt and a batch-signed.txn saved beside it would never be looked
// at. Nothing would say so, either. Step 7 has no deadline by design, because
// the gate is open and every channel is already recoverable, so the run would
// simply keep waiting while the operator watched their wallet report that it had
// saved the file. A wrong name here costs silence rather than a refusal, which is
// why this is two paths and not a paragraph in the copy.
//
// SignedPath stays first, so a run that somehow produces both is not ambiguous
// about which it read. A candidate equal to Unsigned, or to one already listed,
// is dropped: SignedPath's own rule is that nothing may overwrite the
// transaction we still have to compare against, and widening the search must not
// quietly widen that.
func (w *FileWallet) SignedPaths() []string {
	ext := filepath.Ext(w.Unsigned)
	stem := strings.TrimSuffix(w.Unsigned, ext)

	paths := make([]string, 0, 2)
	for _, p := range []string{w.SignedPath(), stem + "-signed" + RawTXExtension} {
		if p == w.Unsigned || slices.Contains(paths, p) {
			continue
		}
		paths = append(paths, p)
	}
	return paths
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

	body, err := w.wait(ctx, w.Unsigned, "the transaction you built")
	if err != nil {
		return nil, err
	}
	// combine.Parse, and nothing else. A raw transaction is refused here even
	// though step 7 reads one, and that is the point rather than an oversight: a
	// raw signed transaction at step 4 is the packet the refusal below exists to
	// catch, and combine.Unsigned cannot read one. A raw transaction is also
	// useless here on its own terms — no witness UTXOs and no key origins, so the
	// verifier could neither check the inputs nor account for the change output.
	raw, perr := combine.Parse(body)
	if perr != nil {
		return nil, fmt.Errorf("%s is not a PSBT this build can read: %w",
			w.Unsigned, perr)
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

// Signed waits for the signed transaction, in either encoding a signing wallet
// writes: a PSBT, or the finalized raw transaction.
//
// # Why two encodings here and one at step 4
//
// A finished transaction is the natural thing to have in hand at this moment.
// Sparrow's View Final Transaction produces the raw hex, lncli is fed exactly
// that at the equivalent prompt in the manual workflow this tool replaces, and a
// mainnet cold probe lost a batch — two peers' pending-channel slots, ~2016
// blocks each — to this build refusing the wrapper and nothing else. See issue
// #3.
//
// It is safe here and would not be at step 4. Step 4 is before the gate, where a
// signed packet is the one thing that can still defeat I-1 from outside, and
// combine.Unsigned refuses it by reading a PSBT's partial signatures — which a
// raw transaction does not have. Step 7 is after the gate: every channel is
// already recoverable, so what arrives is checked for being the transaction LND
// pinned rather than for being unsigned, and a raw transaction answers that
// question more directly than a PSBT does. So Built keeps calling combine.Parse
// alone, and the second encoding lives here, at this call site only.
func (w *FileWallet) Signed(ctx context.Context, unsigned []byte) ([]byte, error) {
	paths := w.SignedPaths()

	// Remove the answer before asking the question, so a file left by an earlier
	// attempt cannot be read as this one's. The unsigned path is guaranteed fresh
	// by NewFileWallet; these are ours to name and therefore ours to clear, and
	// every one of them has to go — a stale file under the name we are about to
	// start watching is the same defect as a stale file under the one we always
	// watched.
	for _, p := range paths {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("clearing %s: %w", p, err)
		}
	}

	fmt.Fprint(w.Out, prose.Bullet("Sign the transaction you built — the same one, "+
		"unchanged — and save it under either of these names:"))
	for _, p := range paths {
		fmt.Fprintf(w.Out, "      %s\n", p)
	}
	fmt.Fprint(w.Out, prose.Bullet("Let the wallet finalize it. A complete "+
		"transaction is what this step wants now: every channel in the batch is "+
		"already recoverable, so a wallet holding signed bytes front-runs nothing."))
	fmt.Fprint(w.Out, prose.Bullet("A signed PSBT or a raw transaction, either is "+
		"read — Sparrow's View Final Transaction hex is a raw transaction, and so "+
		"is what a wallet writes when it offers to export the finished bytes. The "+
		"name it lands under does not decide which: both paths above are watched, "+
		"and what is inside the file is what is read."))

	body, got, err := w.waitAny(ctx, paths, "the signed transaction")
	if err != nil {
		return nil, err
	}
	return w.decodeSigned(body, unsigned, got)
}

// decodeSigned turns what the wallet wrote into the packet combine.Accept takes.
//
// PSBT first, raw transaction second, and the order is not a preference: a PSBT
// is settled by BIP174's magic bytes rather than guessed at, so trying it first
// means the fallback is only ever reached by something that is definitely not
// one. combine.SignedFromTX does the rest, against the base — which is where
// every fact the verifier needs already lives, the raw transaction contributing
// only the completed witnesses.
func (w *FileWallet) decodeSigned(body, unsigned []byte, path string) ([]byte, error) {
	raw, perr := combine.Parse(body)
	if perr == nil {
		return raw, nil
	}
	signed, terr := combine.SignedFromTX(unsigned, body)
	if terr == nil {
		return signed, nil
	}
	if errors.Is(terr, combine.ErrNotATransaction) {
		// Neither, so neither diagnosis is the answer on its own and printing one
		// would send the operator to check the wrong half of their export.
		return nil, fmt.Errorf("%s is neither a PSBT nor a raw transaction this "+
			"build can read. As a PSBT: %v. As a raw transaction: %v", path, perr, terr)
	}
	// It is a transaction and there is something wrong with it. That is the
	// answer; the PSBT sniffer's complaint about magic bytes is noise beside it.
	return nil, fmt.Errorf("%s: %w", path, terr)
}

// wait polls for a file and hands back what was in it, undecoded.
//
// Decoding is the caller's, because the two callers do not want the same answer:
// Built takes a PSBT and only a PSBT, which is I-1, and Signed takes either
// encoding. It used to decode here, through combine.Parse, and that shared
// sniffer is exactly what must not learn about raw transactions — teaching it
// would teach step 4 about them too.
//
// Nothing bounds it but the context. The two waits have different clocks above
// them and neither is this transport's to enforce: step 4 is inside the peers'
// ten minutes, where a deadline of ours would abort a batch the peers were still
// holding, and step 7 has no deadline at all now that the gate is open. What ends
// either is the operator, or the run's own context.
func (w *FileWallet) wait(ctx context.Context, path, what string) ([]byte, error) {
	body, _, err := w.waitAny(ctx, []string{path}, what)
	return body, err
}

// waitAny is wait over more than one name, and it reports which one answered.
//
// The paths are tried in order on every tick, so the caller's preference decides
// a tie rather than the filesystem doing it. Built passes one path; Signed passes
// the set that step 7 accepts, because a wallet that saves a raw transaction
// names it .txn and would otherwise be waited on forever.
func (w *FileWallet) waitAny(ctx context.Context, paths []string, what string) ([]byte, string, error) {
	poll := w.Poll
	if poll <= 0 {
		poll = DefaultPoll
	}
	tick := time.NewTicker(poll)
	defer tick.Stop()

	for {
		for _, path := range paths {
			body, err := os.ReadFile(path)
			switch {
			case err == nil:
				return body, path, nil
			case !os.IsNotExist(err):
				return nil, "", fmt.Errorf("reading %s: %w", path, err)
			}
		}
		select {
		case <-ctx.Done():
			return nil, "", fmt.Errorf("waiting for %s at %s: %w",
				what, strings.Join(paths, " or "), ctx.Err())
		case <-tick.C:
		}
	}
}
