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
	// StateArming: funding streams are open, nothing is finalized. Everything
	// here is cancellable for free.
	StateArming State = "arming"

	// StateSigning: every stream has verified the unsigned transaction, so LND
	// has committed to the funding outpoints (I-3) and the PSBT is out with the
	// signers. Still cancellable for free.
	StateSigning State = "signing"

	// StateArmed: every channel reached chan_pending. This is the I-1 gate, and
	// the journal sets it itself rather than taking a caller's word for it —
	// see MarkPending.
	StateArmed State = "armed"

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
