package journal

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SetupOutcome is what the operator said when they were shown the addresses.
//
// There are two, and neither of them is "installed". Creating the wallet and
// importing the descriptors establishes nothing about whether the descriptors
// are the right ones — a wrong pair imports cleanly, recognises its own wrong
// addresses and reports a plausible partial balance — so the only outcome worth
// recording is the answer to the comparison a human made.
type SetupOutcome string

const (
	// SetupConfirmed: the operator compared the derived addresses against their
	// own wallet software and they matched.
	SetupConfirmed SetupOutcome = "confirmed"

	// SetupRejected: the operator compared them and they did not.
	//
	// Worth a row of its own rather than an absent one. "Never asked" and "asked,
	// and the answer was no" are the same silence to anything reading the
	// journal, and only one of them is a wallet that must not fund a batch.
	SetupRejected SetupOutcome = "rejected"
)

// Setup is one answer to the round-trip address check.
//
// Every string in it is a read-back rather than an input: the descriptors are
// the ones listdescriptors reported after the import, and the addresses are the
// ones deriveaddresses produced from those. That is what makes the row
// self-invalidating — see LatestSetup. A record of "the operator said yes" that
// did not say *what to*, would age into a claim about a wallet it no longer
// describes.
type Setup struct {
	// Wallet is the Core wallet name, which is also winthistle.toml's
	// [bitcoind] wallet. It is the key because that is the thing a later run
	// will point at.
	Wallet string

	// Seq numbers the answers about one wallet, from 1. Append-only: an operator
	// who mis-compared and re-ran leaves two rows, and the later one wins,
	// because a record that erased the first would be claiming the question was
	// only ever asked once.
	Seq int64

	Outcome SetupOutcome

	// Receive and Change are the descriptors the addresses were derived from,
	// exactly as the wallet reports them.
	Receive string
	Change  string

	// SampleSize is how many addresses per branch were on screen. It is here
	// because it is the strength of the check: sortedmulti and multi agree at
	// about half of all indices for a 2-of-2, so one address is a coin toss and
	// five is not.
	SampleSize int

	// FirstReceive and FirstChange are index 0 of each branch — the two lines a
	// human can recognise the record by, and the tripwire doctor re-derives.
	FirstReceive string
	FirstChange  string

	// RescannedFrom is the timestamp Core recorded for the receive descriptor,
	// which is what it actually rescanned from rather than what it was asked
	// for. 0 or 1 means the genesis block.
	RescannedFrom int64

	AnsweredAt time.Time
}

// ErrNoSetup means nobody has answered the address check for this wallet.
var ErrNoSetup = errors.New("no answer to the round-trip address check for this wallet")

// RecordSetup writes one answer and returns its sequence number.
//
// There is no write before this one and nothing to gate: unlike every other
// write in this journal, nothing irreversible happens either side of it. The
// addresses were derived with deriveaddresses, which has no side effect and can
// be asked again as often as the operator likes, so a crash before the answer
// costs nothing but the question.
func (j *Journal) RecordSetup(ctx context.Context, s Setup) (int64, error) {
	switch {
	case s.Wallet == "":
		return 0, errors.New("a setup record needs the wallet it is about")
	case s.Outcome != SetupConfirmed && s.Outcome != SetupRejected:
		return 0, fmt.Errorf("%q is not an answer to the address check", s.Outcome)
	case s.Receive == "" || s.Change == "":
		return 0, errors.New("a setup record needs both descriptors the addresses " +
			"were derived from: an answer that does not say what it was about is " +
			"not a record")
	case s.FirstReceive == "" || s.FirstChange == "":
		return 0, errors.New("a setup record needs the first address of each branch")
	case s.SampleSize <= 0:
		return 0, fmt.Errorf("a sample of %d addresses is not a comparison", s.SampleSize)
	}

	var seq int64
	err := j.tx(ctx, func(tx *sql.Tx) error {
		var next sql.NullInt64
		if err := tx.QueryRowContext(ctx,
			`SELECT MAX(seq) FROM setups WHERE wallet = ?`, s.Wallet).Scan(&next); err != nil {
			return fmt.Errorf("numbering the setup answers for %q: %w", s.Wallet, err)
		}
		seq = next.Int64 + 1

		_, err := tx.ExecContext(ctx,
			`INSERT INTO setups (wallet, seq, outcome, receive_desc, change_desc,
			                     sample_size, first_receive, first_change,
			                     rescanned_from, answered_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			s.Wallet, seq, string(s.Outcome), s.Receive, s.Change,
			s.SampleSize, s.FirstReceive, s.FirstChange, s.RescannedFrom, now())
		if err != nil {
			return fmt.Errorf("recording the setup answer for %q: %w", s.Wallet, err)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return seq, nil
}

// LatestSetup returns the most recent answer about a wallet, whatever it was.
//
// Whatever it was, and not "the most recent confirmation". A wallet whose last
// answer was no is a wallet that must not fund a batch, and a reader that
// searched for the newest yes would find one from before the descriptors were
// corrected and report a confirmed setup.
func (j *Journal) LatestSetup(ctx context.Context, wallet string) (*Setup, error) {
	s := &Setup{Wallet: wallet}
	var outcome, answeredAt string
	err := j.db.QueryRowContext(ctx,
		`SELECT seq, outcome, receive_desc, change_desc, sample_size,
		        first_receive, first_change, rescanned_from, answered_at
		   FROM setups WHERE wallet = ? ORDER BY seq DESC LIMIT 1`, wallet).
		Scan(&s.Seq, &outcome, &s.Receive, &s.Change, &s.SampleSize,
			&s.FirstReceive, &s.FirstChange, &s.RescannedFrom, &answeredAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("wallet %q: %w", wallet, ErrNoSetup)
	}
	if err != nil {
		return nil, fmt.Errorf("reading the setup answers for %q: %w", wallet, err)
	}
	s.Outcome = SetupOutcome(outcome)
	s.AnsweredAt = parseTime(answeredAt)
	return s, nil
}

// Describes reports whether this answer is about the descriptor pair a wallet
// holds now.
//
// This is what stops a confirmation ageing into a lie. Core cannot remove a
// descriptor, so a wallet whose pair was corrected still holds the old one, and
// a run pointed at that wallet has to be able to tell "the addresses I compared"
// from "the addresses this wallet derives today". Exact string equality is the
// right test: the descriptor a wallet reports is Core's own rendering of it, so
// two spellings of the same keys are two different records deliberately — see
// the note in internal/coldwallet about sortedmulti key order.
func (s *Setup) Describes(receive, change string) bool {
	return s != nil && s.Receive == receive && s.Change == change
}
