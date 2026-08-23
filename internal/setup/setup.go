// Package setup is `winthistle setup`: the watch-only wallet, built and then
// questioned.
//
// # It cannot end in success, and that is the whole shape of it
//
// internal/coldwallet's only successful verdict is AwaitingAddressCheck, on
// purpose: nothing this program can check distinguishes a correct cold-storage
// descriptor from a plausible wrong one. Core parses both. The import succeeds
// for both. The read-back is self-consistent for both. And the balance does not
// separate them either — measured on the harness's own 2-of-2, a
// multi()-where-you-wanted-sortedmulti() wallet finds 6 of the cold wallet's
// 8 BTC, because sortedmulti sorts the *derived* keys and the two descriptors
// therefore agree at every index where the keys already happen to be in
// ascending order. A plausible partial balance is a far better disguise than
// zero.
//
// So this command does not report that the setup worked. It builds the wallet,
// derives the first coldwallet.DefaultSampleSize addresses of each branch from
// the descriptors that actually landed, asks a human to compare them against
// their own wallet software, and records the answer. Everything else here exists
// to make that question askable twice: an operator who has to go and find a
// hardware wallet must be able to walk away and come back, and the answer they
// come back with has to be worth something to the next command that runs.
//
// # Re-runnable, in both directions
//
// With --descriptors it installs, which is idempotent: createwallet accepts a
// wallet that exists, and Import reads listdescriptors first and widens to
// whatever range Core has grown to rather than re-asserting the one it was given
// (Core grows a descriptor's range on its own and then refuses to shrink it).
// Without --descriptors it reads the wallet back and asks about what is in it,
// which is the resume path — deriveaddresses has no side effect, so the question
// can be asked as many times as it takes.
//
// # What it refuses to ask
//
// If the wallet does not recognise its own derived addresses, the comparison is
// not worth making and this command will not ask for it. That is an import
// problem rather than a descriptor problem, and an operator who answered "they
// match" to a screen with three addresses missing from the wallet would have
// recorded a confirmation of nothing.
package setup

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/AusDavo/winthistle/internal/bitcoind"
	"github.com/AusDavo/winthistle/internal/coldwallet"
	"github.com/AusDavo/winthistle/internal/config"
	"github.com/AusDavo/winthistle/internal/journal"
)

// Answer is what the operator said to the address check.
//
// Three values rather than a bool, and the third is the one that matters.
// "They do not match" is a wallet that must not fund a batch, and doctor fails
// on it. "I have not compared them yet" is an operator who has gone to find
// their hardware, which is the ordinary case and the reason this command is
// re-runnable. A prompt that collapsed the two would record a rejection every
// time somebody pressed return.
type Answer int

const (
	// NotAnswered: nobody compared anything. Nothing is recorded.
	NotAnswered Answer = iota

	// Matched: the addresses were compared against the operator's own wallet
	// software and they were the same.
	Matched

	// Differed: they were compared and they were not.
	Differed
)

func (a Answer) String() string {
	switch a {
	case Matched:
		return "matched"
	case Differed:
		return "differed"
	default:
		return "not answered"
	}
}

// Deps are the two Core clients and the journal, already open.
//
// LND is deliberately absent. Setting up the cold wallet has nothing to do with
// the node — no channel, no peer, no macaroon — so this command works on a
// machine where LND is down or not yet installed, and the order of an evening's
// work is not forced by which connection this package happens to want.
type Deps struct {
	// Node is a client with no wallet scope: getdescriptorinfo, deriveaddresses
	// and the pre-flight. Wallet is bound to the wallet being built, and does
	// not have to exist yet.
	Node   *bitcoind.Client
	Wallet *bitcoind.Client

	Journal *journal.Journal
	Out     io.Writer

	// Ask puts the derived addresses in front of a human and returns what they
	// said. Nil means do not ask, which records nothing — the right behaviour for
	// anything non-interactive, because the one thing this command must never do
	// is record a comparison nobody made.
	Ask func(ctx context.Context, c coldwallet.AddressCheck) (Answer, error)
}

// Options are the setup.
type Options struct {
	// WalletName is winthistle.toml's [bitcoind] wallet, not a flag of its own.
	// That is the interface rather than an economy: the wallet this command
	// builds has to be the wallet the run will spend from, and a second place to
	// name it is a second thing to get wrong.
	WalletName string

	// Descriptors is the operator's file. Nil means read the wallet back and ask
	// about the descriptors already in it — the resume path.
	Descriptors *config.Descriptors

	// GapLimit and SampleSize are the tool's two knobs, both zero-means-default.
	// They are flags rather than file keys because they are about this program
	// rather than about the cold wallet.
	GapLimit   int
	SampleSize int
}

// Result is what setup did and what it was told.
type Result struct {
	WalletName string

	// Installed is the install path's own result, nil on the resume path.
	Installed *coldwallet.Result

	Landed coldwallet.Landed
	Check  coldwallet.AddressCheck

	// Previous is what the journal already said about this wallet, nil when
	// nobody has ever answered.
	Previous *journal.Setup

	Answer Answer

	// Recorded is the row that was written, nil when nothing was.
	Recorded *journal.Setup
}

var (
	// ErrRefused means nothing was created, and the screen above says why in
	// full. It is short on purpose: the reasons are several paragraphs and they
	// have already been wrapped to the pane.
	ErrRefused = errors.New("nothing was created — the reasons are above")

	// ErrNothingToCompare means the wallet holds no descriptor pair and none was
	// given, so there is nothing to derive addresses from.
	ErrNothingToCompare = errors.New("no descriptors were given and the wallet holds none")

	// ErrInconsistent means Core does not recognise the addresses it derived
	// from its own wallet's descriptors. The comparison is not worth making, so
	// it is not offered.
	ErrInconsistent = errors.New("the wallet does not recognise its own derived addresses")
)

// Do builds or resumes the setup and ends by recording an answer.
func Do(ctx context.Context, d Deps, opts Options) (*Result, error) {
	r := &Result{WalletName: opts.WalletName}
	out := d.Out
	if out == nil {
		out = io.Discard
	}

	if opts.Descriptors != nil {
		cfg := coldwallet.Config{
			WalletName: opts.WalletName,
			Receive:    opts.Descriptors.Receive,
			Change:     opts.Descriptors.Change,
			Birthday:   opts.Descriptors.Birthday,
			GapLimit:   opts.GapLimit,
			SampleSize: opts.SampleSize,
		}
		res, err := coldwallet.Install(ctx, d.Node, d.Wallet, cfg)
		r.Installed = res
		if err != nil {
			// Two kinds of failure, and only one of them has a screen. A refused
			// configuration or an unusable node is fully explained by the report,
			// down to what to do about it, so printing the error underneath as
			// well would repeat several paragraphs as one unwrapped line.
			// Everything else — Core unreachable, an import Core declined — is
			// only in the error, and the report for it would be a description of a
			// wallet that was never built.
			if res != nil && res.Verdict() != coldwallet.AwaitingAddressCheck {
				fmt.Fprint(out, res.Report())
				return r, ErrRefused
			}
			return r, err
		}
		r.Landed, r.Check = res.Landed, res.Check
		fmt.Fprint(out, res.Report())
	} else {
		landed, err := coldwallet.Read(ctx, d.Wallet)
		if err != nil {
			return r, fmt.Errorf("%w: %v.\n\n%s", ErrNothingToCompare, err,
				noDescriptors(opts.WalletName))
		}
		r.Landed = landed
		if r.Check, err = coldwallet.DeriveCheck(ctx, d.Node, d.Wallet, landed,
			sampleSize(opts)); err != nil {

			return r, err
		}
		fmt.Fprint(out, resumeReport(r.Landed, r.Check))
	}

	// What the journal already says. Read after the wallet rather than before,
	// so the comparison is against the descriptors that are in it now.
	if d.Journal != nil {
		prev, err := d.Journal.LatestSetup(ctx, opts.WalletName)
		switch {
		case err == nil:
			r.Previous = prev
		case errors.Is(err, journal.ErrNoSetup):
		default:
			return r, err
		}
		fmt.Fprint(out, previousReport(r.Previous, r.Check))
	}

	// The one thing that is refused rather than asked, and it is narrow on
	// purpose. What has to hold is that the wallet claims these addresses —
	// otherwise the operator would be comparing addresses no batch would ever be
	// built from. What must NOT be a gate is which descriptor Core credits them
	// to: a wallet that holds a superseded pair credits the shared addresses to
	// it, and refusing on that would refuse to ask about a corrected wallet,
	// which is the exact case this command exists to get right. See
	// Derived.FromImported for the measurement.
	if !r.Check.Recognised() {
		return r, fmt.Errorf("%w — so a comparison against your own wallet "+
			"software would settle nothing. The lines above say which addresses "+
			"and how. This is the import rather than the descriptors: the usual "+
			"cause is a gap limit that does not reach the addresses being shown, "+
			"and Config.Validate refuses that before anything is created",
			ErrInconsistent)
	}

	if d.Ask == nil {
		fmt.Fprint(out, notAsked(opts.WalletName))
		return r, nil
	}
	answer, err := d.Ask(ctx, r.Check)
	if err != nil {
		return r, err
	}
	r.Answer = answer

	if answer == NotAnswered {
		fmt.Fprint(out, notAnswered(opts.WalletName))
		return r, nil
	}
	if d.Journal == nil {
		return r, errors.New("there is no journal open to record the answer in")
	}

	rec := journal.Setup{
		Wallet:        opts.WalletName,
		Outcome:       journal.SetupConfirmed,
		Receive:       r.Check.ReceiveDesc,
		Change:        r.Check.ChangeDesc,
		SampleSize:    len(r.Check.Receive),
		FirstReceive:  r.Check.Receive[0].Address,
		FirstChange:   r.Check.Change[0].Address,
		RescannedFrom: r.Landed.Receive.Timestamp,
	}
	if answer == Differed {
		rec.Outcome = journal.SetupRejected
	}
	seq, err := d.Journal.RecordSetup(ctx, rec)
	if err != nil {
		return r, err
	}
	rec.Seq = seq
	r.Recorded = &rec

	fmt.Fprint(out, recordedReport(&rec, r.Landed))
	if answer == Differed {
		return r, ErrRejected
	}
	return r, nil
}

// ErrRejected is returned when the operator said the addresses do not match.
//
// An error rather than a quiet result, because the command exited having done
// something that must not be mistaken for a finished setup — and because a shell
// that ran `winthistle setup && winthistle run` has to stop here.
var ErrRejected = errors.New("the addresses did not match, so this wallet is not set up")

func sampleSize(opts Options) int {
	if opts.SampleSize <= 0 {
		return coldwallet.DefaultSampleSize
	}
	return opts.SampleSize
}
