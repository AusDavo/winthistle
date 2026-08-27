// Package journal is the run journal: the record of what a run wrote, in the
// order it wrote it.
//
// Not "what a batch did", which is what this line used to say. The distinction
// is the contract issue #24 wrote over three files and #30 filed this line
// under: every state change is written *before* the call it describes, so the
// journal records what a run wrote and not whether it is still writing. A run
// in arming, signing or aborting may be running right now — see
// Unfinished's doc comment, and prose.stateMeans, which renders these states
// to an operator.
//
// It exists for one moment in particular. Between finalizing the transaction and
// its confirmation there is a window in which the app holds the only
// broadcastable copy of a transaction that n channels already depend on, and a
// crash in that window must leave artifacts rather than mystery. So every state
// change is written *before* the RPC it describes, and the write before
// PublishTransaction is the one the whole scheme turns on: if it is not on disk,
// the publish did not happen.
//
// What it maps, per I-1's needs: pending_chan_id ↔ funding outpoint ↔ signer
// state ↔ finalized raw transaction. abort.Target is the seam — a journalled run
// converts straight into one, which is what makes recovery after a crash the
// same code path as an abort during a run.
//
// # What is deliberately not in here
//
// No key material of any kind: no seeds, no xprvs, no xpubs, no descriptors, and
// no PSBTs. A signer is a label and a state, nothing else. The finalized raw
// transaction *is* stored — it has to be, because we own rebroadcast (I-1) and
// losing it after the peers hold their commitment signatures is the worst
// outcome available — so the file is still sensitive, and *.db is gitignored.
package journal

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"time"

	// Pure Go, no cgo. The design promises a single static binary, and the cgo
	// SQLite drivers would break that promise for the sake of a build tag.
	_ "modernc.org/sqlite"
)

// State is where a run has got to. The order below is the order a healthy run
// passes through, and it is what decides whether a crashed run may be aborted.
type State string

const (
	// StateArming: funding streams are open and no channel has reached
	// chan_pending yet. Everything here is cancellable for free.
	//
	// It covers psbt_verify too. Verifying and collecting the receipts are one
	// contiguous stretch of the sequence with no operator step between them, so a
	// run found here says "the gate is not open" and the channel rows say how
	// far each member got.
	StateArming State = "arming"

	// StateArmed: every channel reached chan_pending. This is the I-1 gate, and
	// the journal sets it itself rather than taking a caller's word for it —
	// see MarkPending.
	//
	// After the inversion it is reached with *nothing signed*: psbt_verify with
	// skip_finalize takes every channel to chan_pending over the unsigned
	// transaction. So an armed run with no raw_tx on disk is the ordinary
	// mid-run state rather than a contradiction, and MarkPublishing is where the
	// bytes are insisted on.
	StateArmed State = "armed"

	// StateSigning: the gate is open and the unsigned transaction is out with
	// the signing wallet. Still cancellable for free — abort is cheap right
	// through this state, because nothing has been broadcast.
	//
	// It comes *after* StateArmed, and it used to come before. The old sequence
	// signed inside the peers' ten minutes and reached chan_pending afterwards;
	// this one reaches chan_pending first and signs with no clock A running. A
	// run written by a build from before the inversion can hold this state with
	// its channels still in ChanVerified rather than ChanPending, which is what
	// tells the two apart on an operator's existing file — the channel rows, not
	// the run state.
	StateSigning State = "signing"

	// StatePublishing is written *before* PublishTransaction is called, and
	// therefore before the transaction can possibly be in anyone's mempool. A
	// run found in this state may already be public and MUST NOT be aborted.
	StatePublishing State = "publishing"

	// StatePublished: the publish call returned without error.
	StatePublished State = "published"

	// StateAborting: an abort is in progress, or one was interrupted partway.
	StateAborting State = "aborting"

	// StateAborted: the abort completed with nothing left behind — and since #32
	// both halves of that are established rather than one standing in for the
	// other. Nothing failed, which is abort.Report.Clean(), *and* a re-read of
	// this run's own rows through AbortTarget finds no channel to abandon and no
	// shim to cancel. Clean() alone is a fact about the calls, not about what
	// they left: a shim that came back already gone is not a failure and can
	// still leave a channel standing. See Recover.
	StateAborted State = "aborted"
)

// ChannelState is how far one member of the batch got.
type ChannelState string

const (
	// ChanShimRegistered: the funding stream is open and LND has given us an
	// address, but no transaction has been shown to it.
	ChanShimRegistered ChannelState = "shim_registered"

	// ChanVerified: psbt_verify succeeded, so LND has committed to this
	// channel's funding outpoint and only signatures may be added (I-3).
	ChanVerified ChannelState = "verified"

	// ChanPending: chan_pending arrived. The receipt that matters — the peer's
	// commitment signature is stored and the channel is force-closeable.
	ChanPending ChannelState = "pending"

	// ChanAbandoned: removed from LND by the abort path.
	ChanAbandoned ChannelState = "abandoned"

	// ChanCancelled: this run cancelled its shim, and LND accepted the cancel.
	//
	// It is a cause with an actor in it, and it is written only where this build
	// was the actor — abort.CancelShim returned nil. A shim that was already
	// gone when the abort asked is not this state: see ChanShimGone, and
	// recordAbort, which is where the two are told apart. #32 item 1.
	ChanCancelled ChannelState = "cancelled"

	// ChanShimGone: LND held no funding intent under this pending channel id
	// when the abort asked, and this journal had never verified the channel.
	//
	// Both halves are needed and only the pair is terminal. The absence on its
	// own says nothing about the channel — abort.ErrNoShim is matched off LND's
	// own text and means "no funding intent under that id", which a shim that
	// was never registered, one a previous cancel took, one an LND restart
	// dropped and one that CompleteReservation consumed all produce alike. What
	// settles it here is the row: psbt_verify is what starts LND's funding flow
	// after the inversion, so a channel this run never verified is one LND never
	// created, and there is nothing of it to abandon.
	//
	// It is deliberately never written over a verified row. There the same
	// absence is exactly what a channel that reached chan_pending looks like
	// from here, and that row is left as the journal last established it.
	ChanShimGone ChannelState = "shim_gone"
)

// SignerState is how far one signer has got with a PSBT.
//
// There are two success states because there are two rounds with different
// shapes, and neither name is a euphemism for the other:
//
//   - SignerSigned is the batch. One wallet builds the transaction at step 4 and
//     signs it at step 7, and what it hands back is one file with complete
//     witnesses on it. This used to be recorded as SignerPartial, which was named
//     for I-2 — signers returned partial signatures because a signer that could
//     return a complete transaction would be a signer that could publish. I-2 is
//     dissolved: the gate closed at step 6, before anything was signed, so a
//     wallet holding a signed batch front-runs nothing.
//   - SignerPartial has no writer in this build. It was the CPFP child, which
//     went out to m devices and came back in m pieces, and that round is deleted.
//     The constant stays because journals earlier builds wrote carry the value,
//     both from a child and from a batch round of before the inversion, and a
//     recovery screen that could not name it would render somebody's real run as
//     a state this build does not recognise.
//
// SignerDeclined has no writer either, since issue #25, and a row carrying it is
// not evidence that a wallet said no. The step-7 frame used to write it on every
// error out of run.SigningWallet.Signed — a file that never appeared, one that
// could not be read, a moved txid, Ctrl-C — over the top of the SignerAwaiting
// row it had written a moment before, so a journal from before that fix carries
// it wherever step 7 failed, whatever the reason. Nothing in this build can
// observe a refusal: the file transport has no channel through which a wallet
// declines, and whether a run is still going is the run's own state. The constant
// stays renderable for those rows. Do not write it, and do not repurpose it.
//
// Anything reading these back must handle every one of them, writer or not, and
// prose.signerNote is the only thing that does. There is no CHECK constraint and
// no migration table, so an unrecognised value round-trips silently — which is
// why that function has a default branch and says so.
type SignerState string

const (
	SignerAwaiting SignerState = "awaiting"
	SignerSigned   SignerState = "signed"
	SignerPartial  SignerState = "partial"
	SignerDeclined SignerState = "declined"
)

// Journal is an open run journal.
type Journal struct {
	db *sql.DB
}

// Open opens or creates the journal at path.
//
// synchronous=full is not the default under WAL, and it is not optional here:
// the guarantee that a crash mid-publish leaves artifacts rather than mystery is
// exactly the guarantee that a committed write has reached the disk before the
// publish RPC goes out.
func Open(ctx context.Context, path string) (*Journal, error) {
	// Absolute, then assembled as a URL, so a path with a space or a '?' in it
	// cannot be read as part of the pragma list.
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("opening journal %s: %w", path, err)
	}
	dsn := url.URL{
		Scheme: "file",
		Path:   abs,
		RawQuery: url.Values{"_pragma": {
			"journal_mode(wal)",
			"synchronous(full)",
			"foreign_keys(1)",
			"busy_timeout(5000)",
		}}.Encode(),
	}

	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return nil, fmt.Errorf("opening journal %s: %w", path, err)
	}

	// One connection. The journal has a single writer by construction, and
	// serialising here is cheaper than teaching every call site to expect
	// SQLITE_BUSY.
	db.SetMaxOpenConns(1)

	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("opening journal %s: %w", path, err)
	}
	if _, err := db.ExecContext(ctx, schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("creating the journal schema in %s: %w", path, err)
	}
	return &Journal{db: db}, nil
}

// Close closes the journal.
func (j *Journal) Close() error { return j.db.Close() }

// STRICT so a typo in a state string cannot land as an integer, and so the
// column types mean what they say. Outpoints are stored the way the rest of the
// codebase carries them — txid as hex in display byte order — because the
// journal is read by humans during a recovery and a reversed txid there is the
// worst place to discover the convention.
//
// # Five tables this build no longer writes
//
// `bumps`, `bump_signers` and `bump_locks` went with `winthistle bump`,
// `setups` went with `winthistle setup`, and `locks` went with the last thing
// that took a coin lock in Bitcoin Core. The reasoning that put them here is
// worth keeping, because it is the rule for the next table: Open runs a single
// CREATE TABLE IF NOT EXISTS block and there is no version table and no
// migration machinery, so an ALTER would silently not reach a journal written by
// an earlier build. Grow this schema with new tables, never with new columns.
//
// That same absence is why deleting them from this block does not drop them from
// a journal already on disk. They stay there, unread, and that is the intended
// outcome rather than an oversight: nothing in this build can be confused by
// them, and an operator's record of a child they really did broadcast, or of a
// wallet they really did compare addresses against, is not something to destroy
// on their behalf.
//
// `locks` is the one that could have been read rather than dropped, and was not.
// Nothing has written a lock row since the app stopped selecting coins, so on a
// journal this build made the list is always empty; and Core's locks are
// memory-only, so a lock a run of an earlier build took is released by the next
// restart of the node holding it. Carrying a reader for it would have meant
// carrying a Bitcoin Core client through the whole abort path for a list that is
// empty and a lock that expires on its own.
const schema = `
CREATE TABLE IF NOT EXISTS runs (
    id         TEXT PRIMARY KEY,
    state      TEXT NOT NULL,
    txid       TEXT,
    raw_tx     TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;

CREATE TABLE IF NOT EXISTS channels (
    run_id          TEXT    NOT NULL REFERENCES runs(id),
    pending_chan_id TEXT    NOT NULL,
    peer_pubkey     TEXT    NOT NULL,
    amount_sat      INTEGER NOT NULL,
    state           TEXT    NOT NULL,
    funding_txid    TEXT,
    funding_index   INTEGER,
    PRIMARY KEY (run_id, pending_chan_id)
) STRICT;

CREATE TABLE IF NOT EXISTS signers (
    run_id     TEXT NOT NULL REFERENCES runs(id),
    label      TEXT NOT NULL,
    state      TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (run_id, label)
) STRICT;

`

var (
	// ErrNoRun means the journal has no such run.
	ErrNoRun = errors.New("no such run in the journal")

	// ErrNotArmed means a publish was about to happen before every channel in
	// the batch was recoverable. That is I-1, and it is refused here as well as
	// at the call site, because the journal is the only component that knows
	// whether every chan_pending actually arrived.
	ErrNotArmed = errors.New("the batch is not fully armed — publishing now would breach I-1")

	// ErrMayBePublished means the run reached the publish call, so the funding
	// transaction may be in a mempool or already mined. Abandoning a pending
	// channel whose funding transaction then confirms strands its funds with no
	// force-close path, so an abort is refused outright.
	ErrMayBePublished = errors.New("the run reached the publish call — it must not be aborted")

	// ErrOutpointMoved means a second chan_pending receipt for a channel already
	// journalled as pending named a different funding outpoint.
	//
	// It is refused rather than reconciled. The journalled outpoint is the one an
	// abort abandons, and LND commits to the funding outpoint at psbt_verify, so
	// there is no route by which the second answer is the true one — only routes
	// by which accepting it makes the teardown abandon the wrong channel.
	ErrOutpointMoved = errors.New("a receipt named a different funding outpoint " +
		"for a channel already journalled as pending")

	// ErrTxIDMoved means a run's pinned txid was about to be rewritten.
	//
	// The pin is recorded before the first psbt_verify and is what I-3 is checked
	// against. LND commits to the funding outpoints at that call, so a later txid
	// is not a correction — it is a different transaction, one that funds none of
	// this run's channels. Recording it would give the run a rebroadcast duty for
	// bytes nothing depends on and would leave the real commitment unnamed.
	ErrTxIDMoved = errors.New("the run's pinned txid does not match")
)

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// parseTime reads a timestamp back. A journal written by an older build is worth
// more than a parse error, so an unreadable stamp reads as the zero time.
func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
