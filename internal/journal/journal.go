// Package journal is the run journal: the record of what a batch did, in the
// order it did it.
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

	// StateAborted: the abort completed with nothing left behind.
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

	// ChanCancelled: its shim was cancelled before it ever reached pending.
	ChanCancelled ChannelState = "cancelled"
)

// SignerState is how far one cold-storage signer has got with the batch PSBT.
//
// SignerPartial is the only success state, and it is named for I-2: signers
// return partial signatures. A signer that could return a complete transaction
// would be a signer that could publish, which is the thing the whole design
// exists to prevent.
type SignerState string

const (
	SignerAwaiting SignerState = "awaiting"
	SignerPartial  SignerState = "partial"
	SignerDeclined SignerState = "declined"
)

// BumpState is how far one CPFP child has got.
//
// Shorter than State, because a child has less to go wrong. There is no gate to
// count to: I-1 is about the funding transaction, and by the time a child exists
// its parent is already public and every channel in the batch is already
// recoverable. What the states carry instead is the same write-before-the-RPC
// discipline, for the one reason that still applies — a child found in
// BumpPublishing may be in a mempool, and an operator has to be able to tell
// that from one that never went out.
type BumpState string

const (
	// BumpBuilding: the row exists and the change outpoint is claimed. Written
	// before walletcreatefundedpsbt, so a crash inside that call leaves a lock
	// this journal admits to rather than an orphan only Core knows about.
	BumpBuilding BumpState = "building"

	// BumpSigning: the child is built and its PSBT is out with the signers.
	BumpSigning BumpState = "signing"

	// BumpSigned: the partials are merged, finalized in-app and verified, and
	// the raw transaction is on disk.
	BumpSigned BumpState = "signed"

	// BumpPublishing is written before PublishTransaction is called. A child in
	// this state may be public.
	BumpPublishing BumpState = "publishing"

	// BumpPublished: the publish call returned without error.
	BumpPublished BumpState = "published"

	// BumpAbandoned: given up on, and its coin lock released. Nothing was
	// broadcast — or if it was, the state would be one of the two above.
	BumpAbandoned BumpState = "abandoned"

	// BumpSuperseded: this child was published and a later child of the same
	// change outpoint replaced it. The CPFP child is built BIP-125 replaceable,
	// so a second lift is an ordinary replacement rather than a grandchild, and
	// this is what the replaced one becomes once the replacement is out.
	//
	// It is a state rather than a deletion because the row is a record: those
	// bytes really were broadcast, and a journal that removed them would be
	// claiming they never existed. Adding it needed no schema change — the column
	// is TEXT and the value is new — which is the only kind of growth this
	// journal supports.
	BumpSuperseded BumpState = "superseded"
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
// # Why a bump gets its own tables
//
// A CPFP child is a second transaction against the same run, with its own
// signing round and its own coin lock, and there were two other places to put
// it. Neither works.
//
// A second row in `runs` does not, because `runs` is the I-1 gate's state
// machine and a bump is not on it. Begin refuses a run with no channels — "a
// batch with no channels in it is not a batch" — so a bump row would need
// fictional ones; Unfinished would list it; Run.AbortTarget would build a batch
// abort for it; and Recover would run abort.Run over it. That is three special
// cases in the recovery path, which is the one path whose value comes from being
// uniform.
//
// Extra columns on `signers` and `locks` do not either, and that one is a fact
// rather than a preference: Open runs a single CREATE TABLE IF NOT EXISTS block
// and there is no version table and no migration machinery, so an ALTER would
// silently not reach a journal written by an earlier build. New tables are the
// only shape that works on an operator's existing file.
//
// So each of the three mirrors the shape of its batch counterpart, and Bump
// carries its own small state machine: no shims, no channels, no peers, and an
// abort that is one action rather than three.
//
// # And why the setup answer is in here at all
//
// `setups` is not a run either, and it is not a transaction. It is one row
// saying that a human looked at a list of addresses and said whether they
// matched — which is the only check that can tell a correct cold-storage
// descriptor from a plausible wrong one, and therefore the only fact about a
// setup worth keeping.
//
// It is here rather than in a file of its own because this file is already the
// tool's whole durable state: it is opened by every command, it is the one thing
// the design tells an operator to back up, its columns are STRICT so a typo
// cannot land as an integer, and it already holds the other record that must
// never be rewritten (see BumpSuperseded). A second store would be a second
// format, a second set of permissions and a second way to be half-written, in
// exchange for nothing.
//
// It has no foreign key to runs, and that is the point rather than an omission:
// a setup is not on the I-1 state machine, so Unfinished never lists one,
// Run.AbortTarget never builds one, and Recover never runs abort.Run over one.
// The only thing that reads it is winthistle doctor.
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

CREATE TABLE IF NOT EXISTS locks (
    run_id   TEXT    NOT NULL REFERENCES runs(id),
    txid     TEXT    NOT NULL,
    vout     INTEGER NOT NULL,
    released INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (run_id, txid, vout)
) STRICT;

CREATE TABLE IF NOT EXISTS bumps (
    run_id            TEXT    NOT NULL REFERENCES runs(id),
    seq               INTEGER NOT NULL,
    state             TEXT    NOT NULL,
    parent_txid       TEXT    NOT NULL,
    parent_vsize_vb   INTEGER NOT NULL,
    parent_fee_sat    INTEGER NOT NULL,
    change_txid       TEXT    NOT NULL,
    change_vout       INTEGER NOT NULL,
    change_sat        INTEGER NOT NULL,
    target_sat_per_vb REAL    NOT NULL,
    child_txid        TEXT,
    child_fee_sat     INTEGER,
    raw_tx            TEXT,
    created_at        TEXT    NOT NULL,
    updated_at        TEXT    NOT NULL,
    PRIMARY KEY (run_id, seq)
) STRICT;

CREATE TABLE IF NOT EXISTS bump_signers (
    run_id     TEXT    NOT NULL,
    seq        INTEGER NOT NULL,
    label      TEXT    NOT NULL,
    state      TEXT    NOT NULL,
    updated_at TEXT    NOT NULL,
    PRIMARY KEY (run_id, seq, label),
    FOREIGN KEY (run_id, seq) REFERENCES bumps(run_id, seq)
) STRICT;

CREATE TABLE IF NOT EXISTS bump_locks (
    run_id   TEXT    NOT NULL,
    seq      INTEGER NOT NULL,
    txid     TEXT    NOT NULL,
    vout     INTEGER NOT NULL,
    released INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (run_id, seq, txid, vout),
    FOREIGN KEY (run_id, seq) REFERENCES bumps(run_id, seq)
) STRICT;

CREATE TABLE IF NOT EXISTS setups (
    wallet         TEXT    NOT NULL,
    seq            INTEGER NOT NULL,
    outcome        TEXT    NOT NULL,
    receive_desc   TEXT    NOT NULL,
    change_desc    TEXT    NOT NULL,
    sample_size    INTEGER NOT NULL,
    first_receive  TEXT    NOT NULL,
    first_change   TEXT    NOT NULL,
    rescanned_from INTEGER NOT NULL,
    answered_at    TEXT    NOT NULL,
    PRIMARY KEY (wallet, seq)
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

	// ErrNoBump means the journal has no such CPFP child.
	ErrNoBump = errors.New("no such bump in the journal")

	// ErrNotPublic means a bump was asked for against a run whose funding
	// transaction never reached the publish call. There is nothing in any
	// mempool to accelerate, and a child of a transaction nobody has is a
	// transaction that can never confirm.
	ErrNotPublic = errors.New("the run's funding transaction was never published")

	// ErrBumpNotSigned means a child was about to be broadcast before it was
	// combined, finalized and verified in-app. The same shape as ErrNotArmed and
	// for the same reason: the write that says a transaction is ready to go out
	// is the one that has to have landed before it does.
	ErrBumpNotSigned = errors.New("the CPFP child is not signed and verified")
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
