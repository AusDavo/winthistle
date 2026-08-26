// Package config reads winthistle.toml and the batch file.
//
// Two files, deliberately separate, as docs/design.html has it: winthistle.toml
// holds connection details and never a secret, only the paths to them; the
// batch file is the money — which peers, how much, and what each channel will
// charge to route.
//
// # What this package refuses
//
// It refuses more than it accepts, and that is the design rather than an
// accident of implementation. A configuration file is read once, at the start
// of a run that may end with a cold wallet on the table and three peers holding
// reservations, so the failure worth engineering against is not the malformed
// file — that one announces itself — but the well-formed file with a key
// slightly misspelled, running to completion on a default nobody chose. So
// every unknown section and every unknown key is an error naming its line.
//
// One key is refused even when it is spelled correctly: allow_rbf. It appears
// in docs/design.html's example block, and it is not a setting. I-4 is that
// replacing the funding transaction moves every outpoint and destroys every
// channel in the batch, so the funding transaction's replaceability is off at
// construction and there is no code path that reads a preference about it — nor
// any code path that builds a replaceable transaction of any kind, now the CPFP
// child is gone. Honouring the key would be
// a lie and ignoring it silently would be worse, because an operator who wrote
// allow_rbf = true and saw the run proceed would reasonably conclude it had
// been honoured.
//
// A rate that quietly became zero would be the worst default this tool could
// ship: it is the one figure in a batch with no right answer, and I-4 means a
// batch built at the wrong one cannot be corrected by replacing it. So an unset
// rate is a refusal, at load time, before the evening starts rather than during
// it.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/AusDavo/winthistle/internal/lnd"
	"github.com/AusDavo/winthistle/internal/policy"
)

// DefaultPath is where the tool looks when it is not told.
const DefaultPath = "winthistle.toml"

// Defaults for the keys that have one.
const DefaultJournal = "~/.winthistle/runs.db"

// Config is winthistle.toml.
type Config struct {
	// Path is where it was read from, so a report can say which file it means.
	Path string

	LND     lnd.Config
	Journal Journal
	Limits  Limits
}

// Journal is the [journal] block.
//
// It was [server], with bind beside it, until the local web UI went. A section
// named for a component that no longer exists is the same defect as a key that
// changes nothing, so it is renamed and the old spelling is retired by name.
// docs/design.html has always shown it as [journal] path.
type Journal struct {
	// Path is the run journal's SQLite file. It holds no key material and it is
	// the one piece of state worth backing up.
	Path string
}

// Limits is the [limits] block.
//
// One key left in it. abort_after_signing_seconds was docs/design.html's 5:00
// gate: the dress rehearsal measured a signing round and refused to arm a batch
// whose round would not fit inside the peers' ten minutes. The inversion moved
// signing to step 7, after the gate opens, so the window it bounded no longer
// contains a signing round and nothing was left to enforce it. It is retired by
// name rather than kept as a number nothing reads.
type Limits struct {
	// RequireConfirmedInputs becomes the batch's confirmation floor: one
	// confirmation, or none. I-4 is why it defaults to true — an unconfirmed
	// parent can be replaced, which moves our input, which moves our txid, which
	// destroys every channel in the batch.
	RequireConfirmedInputs bool
}

// MinConfirmations is the input floor the plan and Core's coin selection share.
func (l Limits) MinConfirmations() int {
	if l.RequireConfirmedInputs {
		return 1
	}
	return 0
}

var knownSections = map[string]bool{
	"lnd": true, "journal": true, "limits": true,
}

// Load reads winthistle.toml.
func Load(path string) (*Config, error) {
	doc, err := parseFile(path)
	if err != nil {
		return nil, err
	}
	c := &Config{Path: path}
	base := filepath.Dir(path)

	// A relative path in the file is relative to the file, not to whatever
	// directory the operator happened to run the tool from. The alternative
	// makes "winthistle doctor" and "cd /; winthistle doctor" read different
	// credentials, which is the kind of difference that is only ever discovered
	// at the worst moment.
	p := func(s string) string {
		if s == "" {
			return ""
		}
		return resolve(base, s)
	}

	var errs []string
	fail := func(err error) {
		if err != nil {
			errs = append(errs, err.Error())
		}
	}

	l := doc.section("lnd")
	addr, err := l.str(path, "address", "")
	fail(err)
	cert, err := l.str(path, "tls_cert", "")
	fail(err)
	mac, err := l.str(path, "macaroon", "")
	fail(err)
	c.LND = lnd.Config{Address: addr, TLSCert: p(cert), Macaroon: p(mac)}

	jr := doc.section("journal")
	journalPath, err := jr.str(path, "path", DefaultJournal)
	fail(err)
	c.Journal = Journal{Path: p(journalPath)}

	lim := doc.section("limits")
	if lim.has("allow_rbf") {
		errs = append(errs, fmt.Sprintf("%s: allow_rbf is not a setting. Replacing "+
			"the funding transaction changes every outpoint in it, and every peer "+
			"holds a commitment signature against the old ones, so there is no "+
			"preference to express (I-4). This program does not build the "+
			"transaction and no longer reads its sequence numbers either: Core "+
			"relays a higher-fee conflict whatever they signal, so replaceable: "+
			"false is a statement of intent and not a defence. What holds I-4 is "+
			"that only you can sign your inputs, and no code path here replaces a "+
			"funding transaction. Delete the line.",
			where(path, lim.lineOf("allow_rbf"))))
	}
	confirmed, err := lim.boolean(path, "require_confirmed_inputs", true)
	fail(err)
	c.Limits = Limits{RequireConfirmedInputs: confirmed}

	errs = append(errs, doc.unknown(knownSections)...)
	errs = append(errs, c.validate(path, doc)...)
	if len(errs) > 0 {
		return nil, &Invalid{Path: path, Problems: errs}
	}
	return c, nil
}

// validate says what is wrong with a file that parsed.
//
// Everything at once rather than the first thing: a config file is edited in a
// text editor and re-run, and a validator that reports one problem per attempt
// turns one edit into five.
func (c *Config) validate(path string, doc *document) []string {
	var out []string
	need := func(cond bool, msg string) {
		if !cond {
			out = append(out, msg)
		}
	}

	need(c.LND.Address != "", "[lnd] address is required: the host:port of LND's "+
		"gRPC listener, e.g. \"127.0.0.1:10009\"")
	need(c.LND.TLSCert != "", "[lnd] tls_cert is required: the path to LND's tls.cert")
	need(c.LND.Macaroon != "", "[lnd] macaroon is required: the path to the "+
		"credential this tool bakes. Make one with `winthistle print-macaroon-command`")
	if strings.HasSuffix(c.LND.Macaroon, "admin.macaroon") {
		out = append(out, "[lnd] macaroon points at admin.macaroon. Bake the narrow "+
			"credential instead — `winthistle print-macaroon-command` prints the "+
			"command, and the whole point of it is that this tool cannot send coins, "+
			"close a channel or widen its own permissions")
	}

	need(c.Journal.Path != "", "[journal] path is required: where to keep the "+
		"run journal. It holds no key material and it is what makes a crashed run "+
		"recoverable rather than mysterious")

	return out
}

// Invalid is a configuration file that could not be used, and every reason.
type Invalid struct {
	Path     string
	Problems []string
}

func (e *Invalid) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s cannot be used:", e.Path)
	for _, p := range e.Problems {
		b.WriteString("\n  - " + p)
	}
	return b.String()
}

// resolve expands ~ and makes a path absolute against base.
func resolve(base, path string) string {
	switch {
	case path == "~" || strings.HasPrefix(path, "~/"):
		home, err := os.UserHomeDir()
		if err != nil {
			return path
		}
		return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(path, "~"), "/"))
	case filepath.IsAbs(path):
		return filepath.Clean(path)
	default:
		return filepath.Join(base, path)
	}
}

// Example is the file `winthistle doctor` prints when there is none.
//
// It is the same block as docs/design.html's, minus allow_rbf, which this tool
// refuses — see the package comment — and with [fees] as this build reads it:
// one key, the rate you intend to pay, which has no default.
const Example = `[lnd]
address  = "127.0.0.1:10009"
tls_cert = "~/.lnd/tls.cert"
macaroon = "~/.lnd/winthistle.macaroon"   # baked, not admin

[journal]
path = "~/.winthistle/runs.db"

[limits]
require_confirmed_inputs = true

`

// PolicyDefaults is what a batch file's [policy] block starts from when it says
// nothing: LND's own numbers, so that "no policy" and "LND's policy" are the
// same thing and the plan document can say which one the operator chose.
func PolicyDefaults() policy.Policy {
	return policy.Policy{
		BaseFeeMsat:   policy.DefaultBaseFeeMsat,
		FeeRatePPM:    policy.DefaultFeeRatePPM,
		TimeLockDelta: policy.DefaultTimeLockDelta,
	}
}
