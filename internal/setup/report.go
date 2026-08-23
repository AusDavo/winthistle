package setup

import (
	"fmt"
	"strings"
	"time"

	"github.com/AusDavo/winthistle/internal/coldwallet"
	"github.com/AusDavo/winthistle/internal/journal"
	"github.com/AusDavo/winthistle/internal/prose"
)

// Question is the prompt itself, kept here rather than at the call site so the
// wording is tested with the rest of the copy.
//
// It asks the narrow question and not the broad one. "Is the setup correct?"
// invites a judgement; "did every address on screen appear in your own wallet"
// is a thing a person can actually have looked at, and it is the only thing
// their answer is being recorded as.
const Question = "Did every address above appear in your own wallet software?"

// resumeReport is the header for a run that read the wallet back rather than
// importing anything.
func resumeReport(l coldwallet.Landed, c coldwallet.AddressCheck) string {
	var b strings.Builder
	b.WriteString(prose.Para(fmt.Sprintf("The watch-only wallet %q already holds "+
		"a descriptor pair. Nothing was imported and nothing was changed — these "+
		"are the addresses it derives, read back from the wallet itself.",
		l.Info.Name)))
	b.WriteString("\n")
	b.WriteString(field("wallet", l.Info.Name))
	b.WriteString(field("private keys", "disabled"))
	b.WriteString(field("rescanned from", rescanned(l)))
	if len(l.Receive.Range) == 2 {
		b.WriteString(field("range", fmt.Sprintf("[%d, %d]",
			l.Receive.Range[0], l.Receive.Range[1])))
	}
	if l.Info.Scanning.Running {
		b.WriteString("\n")
		b.WriteString(prose.Para(fmt.Sprintf("A rescan is running, %.1f%% done. The "+
			"addresses below are derived from the descriptors and are correct "+
			"whatever the rescan finds — it is the balance and the coin list that "+
			"are incomplete until it finishes.", l.Info.Scanning.Progress*100)))
	}
	b.WriteString(coldwallet.StaleWarning(l))
	b.WriteString("\n")
	b.WriteString(c.Report())
	return b.String()
}

// previousReport says what the journal already holds about this wallet.
//
// The three cases are genuinely different and the copy does not blur them. An
// answer about the descriptors in the wallet now is the answer; an answer about
// different descriptors is not, and saying so is the whole reason the record
// stores what it was about; and never having been asked is the ordinary state of
// a first run.
func previousReport(prev *journal.Setup, c coldwallet.AddressCheck) string {
	if prev == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n")

	when := prev.AnsweredAt.UTC().Format("2 January 2006")
	switch {
	case !prev.Describes(c.ReceiveDesc, c.ChangeDesc):
		b.WriteString(prose.Para(fmt.Sprintf("This wallet was answered about before, "+
			"on %s, and the answer was %q — but it was about a different descriptor "+
			"pair from the one above, so it says nothing about these addresses. "+
			"Compare them again.", when, prev.Outcome)))
		b.WriteString(prose.Bullet("then: " + prev.FirstReceive))
		b.WriteString(prose.Bullet("now:  " + c.Receive[0].Address))
	case prev.Outcome == journal.SetupConfirmed:
		b.WriteString(prose.Para(fmt.Sprintf("These exact descriptors were confirmed "+
			"on %s, against %d addresses per branch. Comparing them again costs "+
			"nothing and this run will record whatever you say now, so if you are "+
			"here because you doubt the first answer, that is the right reason to "+
			"be.", when, prev.SampleSize)))
	default:
		b.WriteString(prose.Para(fmt.Sprintf("These exact descriptors were compared "+
			"on %s and the answer was that they did not match. If they have not "+
			"changed since, nothing about this wallet has been fixed — see the "+
			"descriptor above and where it came from.", when)))
	}
	return b.String()
}

// recordedReport is what was written down.
func recordedReport(rec *journal.Setup, l coldwallet.Landed) string {
	var b strings.Builder
	b.WriteString("\n")

	if rec.Outcome == journal.SetupConfirmed {
		b.WriteString(prose.Para(fmt.Sprintf("Recorded: the addresses matched, for "+
			"the descriptors the wallet %q holds now. `winthistle doctor` reads "+
			"this, and it stops applying the moment those descriptors change — the "+
			"record says which pair it was about, so it cannot outlive them.",
			rec.Wallet)))
		b.WriteString("\n")
		b.WriteString(prose.Para("Directed mode is configured. What is left before a " +
			"batch is the node, the credential and the fee source:"))
		b.WriteString(prose.Bullet("winthistle doctor"))
		return b.String()
	}

	b.WriteString(prose.Para(fmt.Sprintf("Recorded: the addresses did not match. The "+
		"wallet %q must not fund a batch, and `winthistle doctor` will now refuse "+
		"it rather than warn about it.", rec.Wallet)))
	b.WriteString("\n")
	b.WriteString(prose.Para("The descriptors are not the ones your cold wallet " +
		"uses. In order of how often it is each of them: a wrong derivation path, " +
		"multi() where the wallet uses sortedmulti(), two keys the wrong way " +
		"round, or an xpub truncated by whatever carried it here. Export the " +
		"descriptor again from the wallet software that holds the keys, rather " +
		"than editing the one you have."))
	b.WriteString("\n")
	b.WriteString(prose.Para("Then, and this is the part that is not obvious: use a " +
		"new wallet name. Core keeps one active receive branch and one active " +
		"change branch, so importing the corrected pair into this wallet " +
		"deactivates the wrong pair rather than removing it, and a deactivated " +
		"descriptor's coins are still in the wallet's coin list. There is no RPC " +
		"that removes a descriptor. Change [bitcoind] wallet in winthistle.toml " +
		"and run setup again."))
	if len(l.Stale()) > 0 {
		b.WriteString("\n")
		b.WriteString(prose.Para(fmt.Sprintf("That applies twice over here: %q "+
			"already holds %d inactive descriptor%s.", l.Info.Name, len(l.Stale()),
			prose.Plural(len(l.Stale())))))
	}
	return b.String()
}

// notAnswered is for the operator who has gone to find their hardware.
//
// This is the ordinary outcome of a first run and the copy treats it as one.
// Nothing has been lost: the wallet is built, the descriptors are imported, and
// the addresses are derived from the descriptors rather than from wallet state,
// so the same command shows the same addresses tomorrow.
func notAnswered(wallet string) string {
	var b strings.Builder
	b.WriteString("\n")
	b.WriteString(prose.Para(fmt.Sprintf("Nothing was recorded, which is the right "+
		"outcome for a comparison nobody has made yet. The wallet %q is built and "+
		"the descriptors are in it; what is missing is one look at the same wallet "+
		"in the software that holds the keys.", wallet)))
	b.WriteString("\n")
	b.WriteString(prose.Para("Come back to exactly this screen with:"))
	b.WriteString(prose.Bullet("winthistle setup"))
	b.WriteString("\n")
	b.WriteString(prose.Para("It re-derives the addresses from the wallet rather " +
		"than from a file, so they are the addresses a batch would actually be " +
		"built against. Asking again changes nothing — deriveaddresses has no " +
		"side effect and cannot advance a keypool."))
	return b.String()
}

// notAsked is for a run with nowhere to put a question: a pipe, a script, a
// terminal at end of input.
func notAsked(wallet string) string {
	var b strings.Builder
	b.WriteString("\n")
	b.WriteString(prose.Para(fmt.Sprintf("Nothing was recorded: there was nobody to "+
		"ask. The wallet %q is built and the addresses above are what it derives, "+
		"and the comparison against your own wallet software is the only step that "+
		"can tell a correct descriptor from a plausible wrong one — so it is not "+
		"something this command will answer on your behalf.", wallet)))
	b.WriteString("\n")
	b.WriteString(prose.Para("Run it again at a terminal:"))
	b.WriteString(prose.Bullet("winthistle setup"))
	return b.String()
}

// noDescriptors is the error screen for a wallet with nothing in it and no file
// to import.
func noDescriptors(wallet string) string {
	var b strings.Builder
	b.WriteString(prose.Para(fmt.Sprintf("There is nothing to compare. The wallet "+
		"%q holds no descriptor pair, and no descriptor file was given, so there "+
		"is nothing to derive an address from.", wallet)))
	b.WriteString("\n")
	b.WriteString(prose.Para("The descriptors and the birthday are the one part of " +
		"setup no program can supply — they come out of the wallet software that " +
		"holds your keys, and nothing here can guess either. Write them into a " +
		"file and pass it:"))
	b.WriteString(prose.Bullet("winthistle example-descriptors > cold.toml"))
	b.WriteString(prose.Bullet("winthistle setup --descriptors cold.toml"))
	return b.String()
}

// rescanned says what Core recorded as the wallet's birthday, which is a
// read-back rather than what anyone asked for.
func rescanned(l coldwallet.Landed) string {
	ts := l.Receive.Timestamp
	if ts <= 1 {
		return "the genesis block — the whole chain"
	}
	return time.Unix(ts, 0).UTC().Format("2 January 2006")
}

func field(label, value string) string {
	return fmt.Sprintf("  %-20s %s\n", label, value)
}
