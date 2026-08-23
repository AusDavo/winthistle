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
// construction and there is no code path that reads a preference about it. (The
// CPFP child is replaceable, deliberately and unconditionally — see
// plan.MaxBIP125Sequence — which is also not a preference.) Honouring the key would be
// a lie and ignoring it silently would be worse, because an operator who wrote
// allow_rbf = true and saw the run proceed would reasonably conclude it had
// been honoured.
//
// # The fee floor has no default, on purpose
//
// fees.Request.FloorSatPerVB is the operator's own floor, and this package will
// not invent one. Core's estimatesmartfee is entitled to answer "insufficient
// data" — it does so on every regtest node, on a freshly synced one, and on any
// node that has been offline — and a batch built at a guessed rate cannot be
// corrected afterwards, because I-4 forbids replacing it. So an unset floor
// stays unset and internal/fees refuses to produce a rate when it is the only
// thing left; see winthistle doctor, which says so before the evening starts
// rather than during it.
package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/AusDavo/winthistle/internal/bitcoind"
	"github.com/AusDavo/winthistle/internal/lnd"
	"github.com/AusDavo/winthistle/internal/policy"
	"github.com/AusDavo/winthistle/internal/rehearsal"
)

// DefaultPath is where the tool looks when it is not told.
const DefaultPath = "winthistle.toml"

// Defaults for the keys that have one. The abort gate's default is
// rehearsal.DefaultAbortAfterSigning rather than a number written twice.
const (
	DefaultBind         = "127.0.0.1:7420"
	DefaultJournal      = "~/.winthistle/runs.db"
	DefaultAbortAfter   = rehearsal.DefaultAbortAfterSigning
	DefaultTargetBlocks = 6
)

// Config is winthistle.toml.
type Config struct {
	// Path is where it was read from, so a report can say which file it means.
	Path string

	LND      lnd.Config
	Bitcoind bitcoind.Config
	Server   Server
	Limits   Limits
	Fees     Fees
	Signers  []Signer
}

// Server is the [server] block.
type Server struct {
	// Bind is the address the UI will listen on. A localhost bind is not an
	// authentication boundary — any process on the machine can reach it — so the
	// design pairs it with a startup token and strict Origin and Host checks.
	// What is enforced here is the weaker, earlier thing: a wildcard bind is
	// refused, because "0.0.0.0" is not an interface anybody chose.
	Bind string

	// Journal is the run journal's SQLite file. It holds no key material and it
	// is the one piece of state worth backing up.
	Journal string
}

// Limits is the [limits] block.
type Limits struct {
	// AbortAfterSigning is docs/design.html's 5:00 gate, measured by the dress
	// rehearsal and enforced by rehearsal.Gate before anything is armed.
	AbortAfterSigning time.Duration

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

// Fees is the [fees] block: what to ask Core, and what to do when Core has
// nothing to say.
type Fees struct {
	// FloorSatPerVB has no default. Zero means the operator set none, which is
	// legal and is exactly the state that makes a node with no estimate an error
	// rather than a guess.
	FloorSatPerVB float64

	TargetBlocks int
	Mode         string
}

// Signer is one cold-storage device and how to reach it.
type Signer struct {
	// Label is what the operator calls it. It is what the journal records, what
	// a refusal names, and it never has to be unique to a key.
	Label string

	// Command is a shell command that reads a base64 PSBT on stdin and writes
	// the signed one on stdout. Empty means the file handshake instead — see
	// internal/signers, which is also where the reason this is not a "sign for
	// me" API is written down.
	Command string
}

var knownSections = map[string]bool{
	"lnd": true, "bitcoind": true, "server": true,
	"limits": true, "fees": true, "signer": true,
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

	b := doc.section("bitcoind")
	baddr, err := b.str(path, "address", "")
	fail(err)
	cookie, err := b.str(path, "cookie", "")
	fail(err)
	user, err := b.str(path, "user", "")
	fail(err)
	pass, err := b.str(path, "pass", "")
	fail(err)
	wallet, err := b.str(path, "wallet", "")
	fail(err)
	c.Bitcoind = bitcoind.Config{
		Address: baddr, Cookie: p(cookie), User: user, Pass: pass, Wallet: wallet,
	}

	s := doc.section("server")
	bind, err := s.str(path, "bind", DefaultBind)
	fail(err)
	journal, err := s.str(path, "journal", DefaultJournal)
	fail(err)
	c.Server = Server{Bind: bind, Journal: p(journal)}

	lim := doc.section("limits")
	if lim.has("allow_rbf") {
		errs = append(errs, fmt.Sprintf("%s: allow_rbf is not a setting. Replacing "+
			"the funding transaction changes every outpoint in it, and every peer "+
			"holds a commitment signature against the old ones, so the funding "+
			"transaction's replaceability is off at construction and nothing reads "+
			"a preference about it (I-4). The CPFP child `winthistle bump` builds "+
			"is replaceable, so that a second lift replaces it — also not a "+
			"preference. "+
			"Delete the line.", where(path, lim.lineOf("allow_rbf"))))
	}
	secs, err := lim.integer(path, "abort_after_signing_seconds",
		int64(DefaultAbortAfter/time.Second))
	fail(err)
	confirmed, err := lim.boolean(path, "require_confirmed_inputs", true)
	fail(err)
	c.Limits = Limits{
		AbortAfterSigning:      time.Duration(secs) * time.Second,
		RequireConfirmedInputs: confirmed,
	}

	f := doc.section("fees")
	floor, err := f.number(path, "floor_sat_per_vb", 0)
	fail(err)
	target, err := f.integer(path, "target_blocks", DefaultTargetBlocks)
	fail(err)
	mode, err := f.str(path, "mode", "")
	fail(err)
	c.Fees = Fees{FloorSatPerVB: floor, TargetBlocks: int(target), Mode: mode}

	for _, t := range doc.array("signer") {
		label, err := t.str(path, "label", "")
		fail(err)
		command, err := t.str(path, "command", "")
		fail(err)
		c.Signers = append(c.Signers, Signer{Label: label, Command: command})
	}

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

	need(c.Bitcoind.Address != "", "[bitcoind] address is required: the host:port "+
		"of Core's JSON-RPC listener. Directed mode builds the batch with Core, and "+
		"there is no other builder in this build")
	need(c.Bitcoind.Wallet != "", "[bitcoind] wallet is required: the name of the "+
		"watch-only descriptor wallet holding the cold storage descriptors")
	need(c.Bitcoind.Cookie != "" || (c.Bitcoind.User != "" && c.Bitcoind.Pass != ""),
		"[bitcoind] needs either cookie, or user and pass together")

	need(c.Server.Journal != "", "[server] journal is required: where to keep the "+
		"run journal. It holds no key material and it is what makes a crashed run "+
		"recoverable rather than mysterious")
	if msg := checkBind(c.Server.Bind); msg != "" {
		out = append(out, msg)
	}

	need(c.Limits.AbortAfterSigning > 0, fmt.Sprintf(
		"[limits] abort_after_signing_seconds is %d. The gate has to be a positive "+
			"number of seconds — it is what a measured signing round is compared "+
			"against before the batch may be armed",
		int64(c.Limits.AbortAfterSigning/time.Second)))
	if c.Limits.AbortAfterSigning >= rehearsal.PeerWindow {
		out = append(out, fmt.Sprintf("[limits] abort_after_signing_seconds is %s, "+
			"which is the whole of the peers' %s window or more. The gate exists to "+
			"leave room for the build, n psbt_verify calls, the merge, n "+
			"psbt_finalize calls and the backup export after the signing round ends",
			c.Limits.AbortAfterSigning, rehearsal.PeerWindow))
	}

	need(c.Fees.FloorSatPerVB >= 0, "[fees] floor_sat_per_vb cannot be negative")
	need(c.Fees.TargetBlocks > 0, "[fees] target_blocks must be at least 1")
	switch strings.ToUpper(c.Fees.Mode) {
	case "", "CONSERVATIVE", "ECONOMICAL":
	default:
		out = append(out, fmt.Sprintf("[fees] mode is %q; Core has two, "+
			"CONSERVATIVE and ECONOMICAL", c.Fees.Mode))
	}

	labels := map[string]bool{}
	for i, s := range c.Signers {
		line := doc.array("signer")[i].line
		if s.Label == "" {
			out = append(out, fmt.Sprintf("%s: this [[signer]] has no label. The "+
				"label is what a refusal names, and with m devices in a room \"a "+
				"device returned a different transaction\" is a hunt",
				where(path, line)))
			continue
		}
		if labels[s.Label] {
			out = append(out, fmt.Sprintf("%s: two signers are called %q",
				where(path, line), s.Label))
		}
		labels[s.Label] = true
	}
	return out
}

// checkBind refuses a bind address that names no interface.
//
// docs/design.html: the tool refuses a non-loopback bind unless a config key
// explicitly names the interface. A wildcard names none of them — it is every
// interface the machine has now and every one it grows later — so it is refused
// outright, while an explicit address is the operator saying which.
func checkBind(bind string) string {
	if bind == "" {
		return "[server] bind is empty"
	}
	host, port, err := net.SplitHostPort(bind)
	if err != nil {
		return fmt.Sprintf("[server] bind %q is not host:port", bind)
	}
	if port == "" {
		return fmt.Sprintf("[server] bind %q names no port", bind)
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]", "*":
		return fmt.Sprintf("[server] bind %q is a wildcard, which is every "+
			"interface this machine has and every one it grows later. Name the "+
			"interface: \"127.0.0.1:%s\" for the ordinary case, or the address of "+
			"the one you mean", bind, port)
	}
	return ""
}

// Loopback reports whether the server will bind an address only this machine
// can reach. False is not refused — the operator named an interface — but it is
// worth a doctor line, because a localhost bind is the assumption the token and
// the Origin checks are layered on top of.
func (c *Config) Loopback() bool {
	host, _, err := net.SplitHostPort(c.Server.Bind)
	if err != nil {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return host == "localhost"
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
// refuses — see the package comment — and plus the two keys the design's block
// did not have: the fee floor, which has no default and is what stands in when
// Core cannot estimate, and the signers.
const Example = `[lnd]
address  = "127.0.0.1:10009"
tls_cert = "~/.lnd/tls.cert"
macaroon = "~/.lnd/winthistle.macaroon"   # baked, not admin

[bitcoind]
address = "127.0.0.1:8332"
cookie  = "~/.bitcoin/.cookie"
wallet  = "winthistle-cold"

[server]
bind    = "127.0.0.1:7420"
journal = "~/.winthistle/runs.db"

[limits]
abort_after_signing_seconds = 300     # the 5:00 gate
require_confirmed_inputs    = true

[fees]
floor_sat_per_vb = 2.0                # no default: see winthistle doctor
target_blocks    = 6
mode             = "CONSERVATIVE"

[[signer]]
label   = "cold1"
command = "my-signer cold1"           # reads a base64 PSBT, writes one back

[[signer]]
label   = "cold2"
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
