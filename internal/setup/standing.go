package setup

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/AusDavo/winthistle/internal/bitcoind"
	"github.com/AusDavo/winthistle/internal/coldwallet"
	"github.com/AusDavo/winthistle/internal/journal"
	"github.com/AusDavo/winthistle/internal/prose"
)

// Standing is what is known about a cold wallet before anything is opened
// against it: the descriptor pair it actually holds, and whatever a human has
// said about that pair.
//
// It exists because the only check that can tell a correct cold-storage
// descriptor from a plausible wrong one is a comparison a person made, and a
// record of one is worth nothing unless the thing about to spend from the wallet
// reads it.
type Standing struct {
	Wallet string
	Landed coldwallet.Landed

	// Record is the latest answer about this wallet, or nil when nobody has ever
	// been asked.
	Record *journal.Setup

	// Applies is whether Record is about the descriptor pair the wallet holds
	// now. False with a non-nil Record is not a fault and not evidence either
	// way: Core cannot remove a descriptor, so an answer given before a
	// re-import describes a pair this wallet no longer derives from.
	Applies bool
}

// Rejected reports whether a human looked at these exact descriptors and said
// they are not the cold wallet's.
func (s *Standing) Rejected() bool {
	return s != nil && s.Applies && s.Record.Outcome == journal.SetupRejected
}

// Confirmed reports whether a human looked at these exact descriptors and said
// they are.
func (s *Standing) Confirmed() bool {
	return s != nil && s.Applies && s.Record.Outcome == journal.SetupConfirmed
}

// ErrRejectedWallet means the wallet's exact descriptors were compared against
// the operator's own wallet software and did not match.
//
// It is a refusal rather than a warning, and it is the only one in this build
// whose evidence is a person rather than a node. What it prevents is narrow: a
// batch funded from a wallet whose addresses somebody already established are
// not the cold wallet's — coins found by a descriptor nobody controls the keys
// to, change paid to a script the operator's own software will not show them,
// and a signing round that fails at the worst possible moment if the keys are
// not the devices' either.
var ErrRejectedWallet = errors.New("this wallet's descriptors were compared and did not match")

// Check reads the wallet and the journal, and refuses a wallet whose exact
// descriptors were rejected.
//
// It is deliberately narrow in what it refuses and deliberately wide in what it
// reads.
//
// Narrow, because the refusal fires only on an exact descriptor-pair match
// against the latest recorded answer. It therefore cannot false-positive on a
// wallet that was fixed by hand, on a wallet somebody re-imported, or on one
// this build has never seen — all of those come back as "no evidence", which is
// a line on the screen and not a stop.
//
// Wide, because every way coldwallet.Read can fail is a wallet that cannot fund
// a batch, and each of them surfaces later and worse if it is let through: a
// wallet with private keys breaks I-2, a legacy wallet cannot hold the
// descriptor at all, a wallet with no active pair knows about no coins, and one
// with no internal branch cannot derive change — which means no change output,
// which means no CPFP, which is the only acceleration I-4 leaves available.
func Check(ctx context.Context, wallet *bitcoind.Client, j *journal.Journal,
	name string) (*Standing, error) {

	s := &Standing{Wallet: name}
	if wallet == nil {
		return s, errors.New("no Core wallet client to read the cold wallet with")
	}
	landed, err := coldwallet.Read(ctx, wallet)
	if err != nil {
		return s, err
	}
	s.Landed = landed

	if j == nil {
		return s, errors.New("no journal open, so the answer to the address check " +
			"cannot be read — and that answer is the only thing that separates a " +
			"correct descriptor from a plausible wrong one")
	}
	rec, err := j.LatestSetup(ctx, name)
	switch {
	case err == nil:
		s.Record = rec
		s.Applies = rec.Describes(landed.Receive.Desc, landed.Change.Desc)
	case errors.Is(err, journal.ErrNoSetup):
	default:
		return s, err
	}

	if s.Rejected() {
		return s, fmt.Errorf("%w on %s: %w", ErrRejectedWallet,
			s.Record.AnsweredAt.UTC().Format("2 January 2006"), errRejectedDetail)
	}
	return s, nil
}

// errRejectedDetail is the second half of the refusal, kept separate so the
// sentinel stays short enough to read in an exit line.
var errRejectedDetail = errors.New("nothing in this batch can put that right — " +
	"see the screen above")

// Note is the line or two this belongs on a pre-flight screen.
//
// The three no-evidence cases are one sentence each rather than one shared
// sentence, because they are not the same thing: nobody was asked, somebody was
// asked about a different wallet, and somebody said yes are three different
// amounts of knowledge about the transaction that is about to be built.
func (s *Standing) Note() string {
	if s == nil {
		return ""
	}
	var b strings.Builder
	if s.Landed.Receive.Desc != "" {
		b.WriteString(fmt.Sprintf("  %-20s %s\n", "wallet", s.Landed.Info.Name))
	}

	switch {
	case s.Rejected():
		b.WriteString("\n")
		b.WriteString(prose.Para(fmt.Sprintf("Stopping. On %s these exact "+
			"descriptors were compared against your own wallet software and they "+
			"did not match, and nothing has changed since — this is the same pair.",
			s.Record.AnsweredAt.UTC().Format("2 January 2006"))))
		b.WriteString("\n")
		b.WriteString(prose.Para("Nothing was opened and nothing was asked of any " +
			"peer. What that answer means is that the addresses this wallet derives " +
			"are not your cold wallet's, so the coins it can see were found by a " +
			"descriptor you may not hold the keys to, and the change output would be " +
			"paid to a script your own software will not show you."))
		b.WriteString("\nWhat to do:\n")
		b.WriteString(prose.Bullet("If the answer was a mistake, run `winthistle " +
			"setup` and compare them again. It re-derives from the wallet and records " +
			"whatever you say now."))
		b.WriteString(prose.Bullet("If it was not, export the descriptors again from " +
			"the software that holds the keys, and set up under a new wallet name. " +
			"Core keeps one active receive branch and one active change branch, so " +
			"importing a corrected pair into this wallet deactivates the rejected one " +
			"rather than removing it — and a deactivated descriptor's coins are still " +
			"in this wallet's coin list. There is no RPC that removes a descriptor."))
	case s.Confirmed():
		b.WriteString(fmt.Sprintf("  %-20s confirmed %s, %d addresses per branch\n",
			"addresses",
			s.Record.AnsweredAt.UTC().Format("2 January 2006"), s.Record.SampleSize))
	case s.Record != nil:
		b.WriteString(fmt.Sprintf("  %-20s %s\n", "addresses",
			"answered about a different descriptor pair"))
		b.WriteString("\n")
		b.WriteString(prose.Para(fmt.Sprintf("The last answer about this wallet was "+
			"%q, on %s, but it was about descriptors this wallet no longer derives "+
			"from — so it says nothing either way about the addresses this batch will "+
			"be built against. Not a reason to stop; a reason to know.",
			s.Record.Outcome,
			s.Record.AnsweredAt.UTC().Format("2 January 2006"))))
	default:
		b.WriteString(fmt.Sprintf("  %-20s %s\n", "addresses", "never compared"))
		b.WriteString("\n")
		b.WriteString(prose.Para("Nobody has compared this wallet's addresses " +
			"against the software that holds the keys, and that comparison is the " +
			"only thing that can tell a correct descriptor from a plausible wrong " +
			"one — a wrong one imports cleanly and shows a plausible partial " +
			"balance. Not a reason to stop, because a wallet imported by hand is a " +
			"working wallet; `winthistle setup` is what settles it."))
	}
	if stale := len(s.Landed.Stale()); stale > 0 && !s.Rejected() {
		b.WriteString("\n")
		b.WriteString(prose.Para(fmt.Sprintf("This wallet also holds %d inactive "+
			"descriptor%s whose coins are still in its coin list, so this batch can "+
			"spend them. Core has no RPC that removes a descriptor.",
			stale, prose.Plural(stale))))
	}
	return b.String()
}
