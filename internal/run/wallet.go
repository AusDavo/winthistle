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
// you hand it and exports a PSBT.
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
	// the addresses in full to Out before calling this, because a terminal is
	// where they can be read and selected. A transport with a human at the other
	// end could ignore the argument; FileWallet does not, because it renders pay a
	// second time as the CSV Sparrow loads. A fixture or a future transport should
	// not have to scrape the transcript to find out what the batch pays.
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
//
// RecipientsPath is refused on the same rule, for the same reason turned around:
// a batch-recipients.csv already on disk was written for some other set of
// funding addresses, and the operator loading it into Sparrow builds a
// transaction paying them. That transaction is refused at step 5 — every one of
// its outputs is unnamed — but it is refused inside clock A, and a refusal here
// is free.
//
// It is refused rather than overwritten, which is where it parts company with
// SignedPaths. Signed clears the names it is about to watch one line before it
// starts watching them, and those are names this program has spent the whole run
// telling the wallet to write. The CSV is a name this program invents out of the
// operator's own --psbt stem, in the operator's own directory, and quietly
// truncating a file we did not create and were not asked about is not something
// to do at all, let alone to do silently.
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
	w := &FileWallet{Unsigned: path, Out: out}
	if csv := w.RecipientsPath(); csv != "" {
		if _, err := os.Stat(csv); err == nil {
			return nil, fmt.Errorf("%s already exists, and this run writes that file: "+
				"it is the recipients list Sparrow loads, named from --psbt. The one "+
				"that is there was written for funding addresses that are not this "+
				"batch's, and loading it would build a transaction paying them. Name a "+
				"different --psbt path, or move that one aside", csv)
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("looking at %s: %w", csv, err)
		}
	}
	return w, nil
}

// RecipientsSuffix is what the generated recipients file's name ends in.
//
// "-recipients" rather than the issue's batch.csv, because this repository
// already has a batch file — the peers and amounts --batch names — and two files
// called the batch would be one too many. The extension is fixed rather than
// inherited from --psbt: it says what the file is to every program that opens it,
// which is the whole point of generating it.
const RecipientsSuffix = "-recipients.csv"

// RecipientsPath is where the Send to Many CSV goes: the --psbt stem with
// RecipientsSuffix on it.
//
// Derived, for SignedPath's reason and one more. The operator is never asked for
// a second path, so there is no second path to get wrong; and because the suffix
// carries its own extension, no --psbt value can make this collide with Unsigned
// or with either of SignedPaths. A --psbt of batch-recipients.csv derives
// batch-recipients-recipients.csv, which is ugly and is not the same file.
func (w *FileWallet) RecipientsPath() string {
	ext := filepath.Ext(w.Unsigned)
	return strings.TrimSuffix(w.Unsigned, ext) + RecipientsSuffix
}

// SignedPath is where the signed transaction goes: the unsigned path with
// "-signed" before the extension.
//
// Derived rather than asked for, so the paths cannot be given the same value.
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
//
// # The recipients file
//
// This is the one file this application writes outside its journal, and it is
// written here rather than earlier because here is where pay arrives. It is a
// convenience and it is scoped like one: it removes the typing at step 4, which
// is the only part of clock A that takes any time, and it removes nothing from
// step 5, which is still the whole check on what comes back.
//
// The addresses *are* rendered twice now — once as the table internal/run has
// already printed to this same writer, once as the CSV. That used to be an
// argument against doing it, and what makes it safe is that both renderings come
// off the same []Recipient in the same call, so they cannot say different things.
// The table is not replaced by the file: the table is the attribution, which is
// what this program is for, and a "see the CSV" line in its place would move the
// one screen where an operator can see which peer gets which output into a file
// they may never open.
func (w *FileWallet) Built(ctx context.Context, pay []Recipient) ([]byte, error) {
	w.writeRecipients(pay)

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

// writeRecipients writes the Send to Many CSV and says where it is.
//
// A failure to write it is reported and not returned, which is the one place this
// transport swallows an error on purpose. The file saves typing; the table above
// it says the same thing and is already on screen. Aborting the run over it would
// end a batch inside clock A — n peers holding reservations — because a
// convenience could not be produced, and that trade is the wrong way round. What
// must not happen is the copy naming a file that is not there, so the failure
// says so in the same place the path would have been.
func (w *FileWallet) writeRecipients(pay []Recipient) {
	path := w.RecipientsPath()
	if err := os.WriteFile(path, recipientsCSV(pay), 0o600); err != nil {
		fmt.Fprint(w.Out, prose.Bullet(fmt.Sprintf("The recipients file could not be "+
			"written (%v), so enter the recipients from the table above instead. "+
			"Nothing else about this run changes: step 5 checks the transaction you "+
			"build either way.", err)))
		return
	}
	fmt.Fprint(w.Out, prose.Bullet("You do not have to type any of that. Sparrow's "+
		"Send to Many → Load CSV reads this file, which has just been written with "+
		"exactly the recipients above — this saves you typing; step 5 is still what "+
		"checks it:"))
	fmt.Fprintf(w.Out, "      %s\n", path)
	fmt.Fprint(w.Out, prose.Bullet("The amounts in it are BTC, not sats, and the "+
		"file cannot say so for itself — set Sparrow's unit to BTC before you load "+
		"it. In sats mode it finds no recipients at all and tells you why, which is "+
		"the failure worth having: sats read in BTC mode would load a hundred "+
		"million times too large without a word."))
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
//
// # A file that is there is not yet a file that is finished
//
// The wallet writing these files is not atomic from a poller's point of view. A
// poll landing between the create and the last write sees a file that exists,
// reads it without error, and comes away holding a prefix of a transaction —
// zero bytes, if it landed early enough. That prefix used to be returned, and
// the operator got a decode failure for a file that was correct a millisecond
// later: at step 4 that is inside clock A, which on a five-channel batch is
// every peer's reservation. Issue #8.
//
// So a candidate is returned only when readWhole says what it read is the whole
// file. What is deliberately not done is retry on a decode failure. It would
// close the same race, and it would cost the thing this transport is for: a
// genuinely wrong file — the operator saved the wrong transaction — would become
// indistinguishable from a slow one, and step 4's refusal of a signed packet is
// I-1's last gate. A decode failure stays loud, on the first complete file.
func (w *FileWallet) waitAny(ctx context.Context, paths []string, what string) ([]byte, string, error) {
	poll := w.Poll
	if poll <= 0 {
		poll = DefaultPoll
	}
	tick := time.NewTicker(poll)
	defer tick.Stop()

	unsettled := make(map[string]int, len(paths))
	said := make(map[string]bool, len(paths))

	for {
		for _, path := range paths {
			body, state, err := readWhole(path)
			if err != nil {
				return nil, "", fmt.Errorf("reading %s: %w", path, err)
			}
			if body != nil {
				return body, path, nil
			}
			if state == fileAbsent {
				unsettled[path] = 0
				continue
			}
			// There and not whole. Waiting is right, and waiting in silence is not:
			// the operator's wallet has told them it saved the file, and issue #5 is
			// the standing lesson that a wait nobody can see is the worse failure.
			// Not on the first look, because an ordinary save caught mid-write is
			// complete by the next one and saying so would be noise on every run.
			unsettled[path]++
			if unsettled[path] >= unsettledPollsBeforeSaying && !said[path] {
				said[path] = true
				fmt.Fprint(w.Out, prose.Bullet(fmt.Sprintf("%s is there and %s, so "+
					"it has not been read: half a transaction is not read on "+
					"purpose. If your wallet says it has finished saving, save it "+
					"again over that file.", path, notWholeYet(state))))
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

// unsettledPollsBeforeSaying is how many consecutive looks find a file present
// and unfinished before the wait says so on screen.
//
// Two rather than one, and a count of looks rather than a duration, because what
// it measures is "this did not settle across a whole poll interval" — which is
// the same statement at Poll = 2s and at the 5ms the tests use. Nothing decides
// anything on this number; it only chooses the moment a line is printed.
const unsettledPollsBeforeSaying = 2

// notWholeYet says what was seen, rather than what it implies.
//
// The old wording was "is still being written", which asserts a writer this
// program never observed. It only ever observed one of two things, and by the
// time this prints the more likely of them is the one that names no writer at
// all: issue #10 measured Sparrow's gap between two encoder chunks at 0.05-0.3
// ms, with no syscall and no I/O in it, and this line does not print until a
// path has been unfinished across two consecutive polls — two seconds at
// DefaultPoll. A file that has been at zero bytes that long is more likely empty
// and staying empty than caught mid-write. The remedy is the same either way,
// which is why the wording is the whole of the fix.
func notWholeYet(state fileState) string {
	if state == fileGrew {
		return "grew while it was being read, so what came back was a prefix"
	}
	return "has nothing in it yet"
}

// fileState is what one look at a candidate path found.
//
// Three of the four are "nothing to read yet", and they are kept apart because
// the screen names what was observed. fileEmpty and fileGrew are both a wallet
// that may be part-way through saving, but only fileGrew has actually seen the
// file move; fileEmpty has seen zero bytes, which is also what an empty file
// that stays empty looks like forever.
type fileState int

const (
	fileAbsent fileState = iota
	fileEmpty
	fileGrew
	fileWhole
)

// readWhole reads path if what is there is a whole file, and holds its peace if
// it is not.
//
// The state says what this look found, so the caller can tell "the wallet has
// not saved yet" from the two shapes of "not finished". A nil body with a nil
// error is one of the latter: come back next tick.
//
// The check is two stats around the read. A wallet part-way through writing is
// growing the file, so a size that moved across the read means what came back is
// a prefix; a size of zero is that same case caught at its beginning, and is
// skipped without reading because no valid transaction is empty in either
// encoding. The stats cost a syscall each and are made on every look, which is
// the point: a file that was already complete when the first poll found it is
// read on that poll, with no extra tick of latency added to the common case.
//
// # What this does not catch, and why that is the trade
//
// A writer that writes in several chunks leaves a prefix sitting still between
// two of them. If a look lands in one of those gaps the file is not moving while
// it is read, and a prefix of a transaction is indistinguishable from a short
// transaction: only decoding it could tell, and decoding it to decide whether to
// wait is the fix this must not be. So that gap is closed only to the width of
// the read.
//
// Closing it entirely means requiring the size to be unchanged across two
// consecutive *polls*, which is a whole poll interval of latency on every run —
// two seconds, at DefaultPoll, added to a step inside clock A, for a file that
// was finished before anybody looked at it. Nothing can have both; a poller
// cannot ask the filesystem whether the writer has more to say.
//
// What makes this the right side of that trade is the shape of the exposure, and
// issue #10 measured it rather than leaving it asserted. Sparrow 2.5.3 is the
// wallet this transport is built around; its three save paths were read in source
// and each shape was run under strace.
//
// The binary PSBT save is one write(2) at any size — an unbuffered
// FileOutputStream.write(byte[]), which the JDK issues as a single write for the
// whole array, measured at 1.1 KB, 6 KB, 12 KB and 30 KB. The base64 PSBT save
// and the .txn final-transaction save go through an OutputStreamWriter, whose
// encoder buffer is 8192 bytes, and those do split: one write below 8 KB and
// 8192-byte chunks above it. So step 4 has one encoding that never splits and one
// that splits on a large batch, and step 7's .txn is hex and therefore twice the
// transaction's size, which puts a 2-of-2 batch of about fifteen inputs over the
// line. A writer that splits is not the exotic case this comment used to call it.
//
// What the earlier reading had right is which window is the wide one, and the
// source says it more plainly than the guess did: all three paths create and
// truncate the file *before* they compute what to put in it, so the file sits at
// zero bytes for the whole of the serialization. That is the window the flake
// landed in, and it is closed here outright.
//
// The prefix window is the gap between two encoder chunks, and it measures
// 0.05–0.3 ms under strace, which inflates it. There is no syscall in that gap,
// no I/O and no operator — it is the encoding of the next 8 KB. A poll would have
// to land inside one of those *and* finish its read inside it. That is what the
// two-poll rule would buy, for two seconds on every run.
func readWhole(path string) (body []byte, state fileState, err error) {
	before, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fileAbsent, nil
		}
		return nil, fileAbsent, err
	}
	if before.Size() == 0 {
		return nil, fileEmpty, nil
	}

	body, err = os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Gone between the stat and the open. There is nothing here to wait on
			// yet, which is what an absent file means.
			return nil, fileAbsent, nil
		}
		return nil, fileGrew, err
	}

	after, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fileAbsent, nil
		}
		return nil, fileGrew, err
	}
	if after.Size() != before.Size() || int64(len(body)) != after.Size() {
		return nil, fileGrew, nil
	}
	return body, fileWhole, nil
}
