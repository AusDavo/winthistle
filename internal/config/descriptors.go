package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/AusDavo/winthistle/internal/coldwallet"
)

// Descriptors is the third file: what the cold wallet is.
//
// # Why it is a file, and a third one
//
// Two descriptors and a date is not much to pass, and the obvious alternatives
// are worse for reasons that are specific rather than stylistic.
//
// Not command-line flags. A descriptor is three hundred characters of extended
// public key, and CLAUDE.md's rule about xpubs is the reason: a single one
// deanonymises the whole cold wallet's history, permanently. A flag puts it in
// the shell history of every machine it is ever typed on and in the ps output of
// every other local user for the length of the rescan, which on mainnet is
// hours. A file has an owner and a mode.
//
// Not winthistle.toml. That file holds connection details and the paths to
// secrets, it is read by every command on every run, and it is edited. This one
// is read once, by one command, and then the descriptors live in Core where they
// are needed. Keeping a permanent copy of the most privacy-critical artifact in
// the file the tool opens most often is the opposite of what it costs to pass it
// once.
//
// # Why the birthday is in here rather than beside it
//
// Because it belongs to the same wallet. A birthday supplied separately from the
// descriptors — a flag, an environment variable, a prompt — is how the right
// descriptor gets paired with the wrong date, which produces a wallet that
// imports cleanly and reports a zero balance because it never scanned the blocks
// the coins arrived in. One artifact, one file, exported together.
type Descriptors struct {
	Path string

	// Receive and Change are the external and internal branches, exactly as the
	// operator's own wallet software exported them. A checksum is optional; Core
	// computes one either way.
	Receive string
	Change  string

	// Birthday is the cold wallet's, and Core rescans from it. Never inferred
	// and never defaulted — see LoadDescriptors.
	Birthday time.Time
}

var descriptorSections = map[string]bool{"cold": true}

// GenesisWord is what to write for a birthday when the cold wallet's own is
// unknown and the operator has decided to scan the whole chain.
//
// Spelled out rather than left blank, and this is the one thing in the file with
// no default at all: an absent birthday is an error and "genesis" is a
// multi-hour rescan on mainnet, so the difference between them has to be
// something somebody typed.
const GenesisWord = "genesis"

// LoadDescriptors reads the cold wallet's descriptor file.
//
// Every key is required. There is no default for any of them and there must not
// be: a wrong descriptor and a wrong birthday each produce a wallet that imports
// without complaint and then lies, and the whole reason this file exists is that
// nothing in this program can supply either value.
func LoadDescriptors(path string) (*Descriptors, error) {
	doc, err := parseFile(path)
	if err != nil {
		return nil, err
	}
	d := &Descriptors{Path: path}
	var errs []string
	fail := func(err error) {
		if err != nil {
			errs = append(errs, err.Error())
		}
	}

	c := doc.section("cold")
	recv, err := c.str(path, "receive", "")
	fail(err)
	chg, err := c.str(path, "change", "")
	fail(err)
	born, err := c.str(path, "birthday", "")
	fail(err)

	d.Receive = strings.TrimSpace(recv)
	d.Change = strings.TrimSpace(chg)

	if c == nil {
		errs = append(errs, fmt.Sprintf("%s has no [cold] section. It needs three "+
			"keys — receive, change and birthday — and `winthistle "+
			"example-descriptors` prints the file to start from", path))
	} else {
		if d.Receive == "" {
			errs = append(errs, "[cold] receive is required: the cold wallet's "+
				"external descriptor, the branch its funding addresses come from")
		}
		if d.Change == "" {
			errs = append(errs, "[cold] change is required: the cold wallet's internal "+
				"descriptor. Without it Core cannot derive change, and a batch with no "+
				"change output cannot be fee-bumped by CPFP (I-4)")
		}
		switch {
		case born == "":
			errs = append(errs, "[cold] birthday is required: the date the cold "+
				"wallet was created, which is where Core starts its rescan. Nothing "+
				"here can guess it, and a wrong one produces a wallet that imports "+
				"cleanly and reports a zero balance because it never scanned the "+
				"blocks the coins arrived in. Write \""+GenesisWord+"\" to scan the "+
				"whole chain deliberately")
		case strings.EqualFold(born, GenesisWord):
			d.Birthday = coldwallet.Genesis
		default:
			t, err := parseBirthday(born)
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v",
					where(path, c.lineOf("birthday")), err))
			} else {
				d.Birthday = t
			}
		}
	}

	errs = append(errs, doc.unknown(descriptorSections)...)
	if len(errs) > 0 {
		return nil, &Invalid{Path: path, Problems: errs}
	}
	return d, nil
}

// parseBirthday reads a date.
//
// One format, YYYY-MM-DD, read as UTC midnight. Not a permissive parser: 03/04
// is the third of April in most of the world and the fourth of March in one
// large part of it, and a birthday six months out is a rescan that silently
// misses half a year of the wallet's history. A date this tool cannot read is an
// error the operator fixes in a second; a date it reads wrongly is a zero
// balance nobody can explain.
//
// It is read as midnight rather than as an instant because Core walks back to
// the last block at or before the timestamp anyway, so any time of day inside
// the birthday would be rounded down to roughly the same block — and asking for
// a time of day would imply a precision the value does not have.
func parseBirthday(s string) (time.Time, error) {
	t, err := time.ParseInLocation("2006-01-02", s, time.UTC)
	if err != nil {
		return time.Time{}, fmt.Errorf("birthday %q is not a date. Write it as "+
			"YYYY-MM-DD — one format, because 03/04 is two different days depending "+
			"on where it was written and a rescan from the wrong one of them finds "+
			"part of the wallet — or %q to scan the whole chain", s, GenesisWord)
	}
	// A day early costs nothing; a day late costs the coins that arrived on the
	// first day. Core clamps to the block boundary at or before the timestamp,
	// so midnight UTC of the stated day is already the safe side of it.
	return t, nil
}

// ExampleDescriptors is what `winthistle example-descriptors` prints.
//
// The keys are tpubs, and every fixture and example in this repository is,
// deliberately: a mainnet xpub in a file that might be pasted into an issue, a
// commit or a chat window deanonymises the whole cold wallet's history and git
// history cannot be un-published.
const ExampleDescriptors = `# The cold wallet, as your own wallet software exported it. Read once, by
# ` + "`winthistle setup`" + `, and then the descriptors live in Core.
#
# Both branches, on their own lines. If your wallet gave you one descriptor
# with a <0;1> step in it, split it: /0/* here and /1/* below. Core keeps
# only the first branch of a multipath descriptor, so importing one twice
# would derive change from the addresses it also funds.
#
# The birthday is the date the cold wallet was created. Core rescans from
# there, so a date after the coins arrived reports a zero balance and says
# nothing about why. "genesis" scans the whole chain — correct, and hours on
# mainnet.

[cold]
receive  = "wsh(sortedmulti(2,[aabbccdd/48h/1h/0h/2h]tpubA.../0/*,[11223344/48h/1h/0h/2h]tpubB.../0/*))"
change   = "wsh(sortedmulti(2,[aabbccdd/48h/1h/0h/2h]tpubA.../1/*,[11223344/48h/1h/0h/2h]tpubB.../1/*))"
birthday = "2023-04-11"
`
