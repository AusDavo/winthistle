package config

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// This is a reader for the subset of TOML this tool's two files use, and it is
// deliberately not a TOML parser.
//
// Two reasons, in order. The first is that strictness is the feature: every key
// this reader does not recognise is an error naming the line, so a misspelled
// abort_after_signing_seconds is a refusal rather than a silently-defaulted
// gate. A general parser hands back a map, and a map cannot tell you what the
// operator meant to write. The second is that the subset is genuinely small —
// tables, arrays of tables, and four scalar types — and a dependency to read
// ten keys is a dependency in every future audit of a tool whose whole argument
// is that you can read it.
//
// What it accepts, and nothing else:
//
//	# a comment, on its own line or after a value
//	[table]
//	[[array-of-tables]]
//	key = "a string"          escapes: \\ \" \n \t
//	key = 300                 integer, optional sign, optional _ separators
//	key = 2.5                 float
//	key = true                bool
//
// Not accepted, on purpose: bare keys with dots, multi-line strings, literal
// strings, arrays, inline tables, dates, and a key appearing twice.

// scalar is one value, with the line it was written on so a refusal can name it.
type scalar struct {
	raw  string
	line int
}

// table is one [section] or one [[array]] element.
type table struct {
	name  string
	line  int
	order []string
	vals  map[string]scalar
	used  map[string]bool
}

// document is a parsed file, in the order it was written.
type document struct {
	path   string
	tables []*table
}

func parseFile(path string) (*document, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parse(path, f)
}

func parse(path string, r io.Reader) (*document, error) {
	doc := &document{path: path}
	counts := map[string]int{}

	// The keys written before any [table] header belong to the root, which this
	// format has no use for — every setting lives in a section. Refusing them
	// here means "wallet = ..." at the top of the file is an error rather than a
	// value nothing ever reads.
	var cur *table

	sc := bufio.NewScanner(r)
	for line := 1; sc.Scan(); line++ {
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		switch {
		case strings.HasPrefix(text, "[["):
			name, err := header(text, "[[", "]]", path, line)
			if err != nil {
				return nil, err
			}
			cur = &table{name: name, line: line,
				vals: map[string]scalar{}, used: map[string]bool{}}
			counts[name]++
			doc.tables = append(doc.tables, cur)

		case strings.HasPrefix(text, "["):
			name, err := header(text, "[", "]", path, line)
			if err != nil {
				return nil, err
			}
			if counts[name] > 0 {
				return nil, fmt.Errorf("%s: [%s] appears twice", where(path, line), name)
			}
			cur = &table{name: name, line: line,
				vals: map[string]scalar{}, used: map[string]bool{}}
			counts[name]++
			doc.tables = append(doc.tables, cur)

		default:
			key, raw, err := keyValue(text, path, line)
			if err != nil {
				return nil, err
			}
			if cur == nil {
				return nil, fmt.Errorf("%s: %s is not inside any section. Every "+
					"setting in this file belongs to one — see the example in "+
					"docs/design.html", where(path, line), key)
			}
			if prev, dup := cur.vals[key]; dup {
				return nil, fmt.Errorf("%s: %s is set twice, here and on line %d",
					where(path, line), key, prev.line)
			}
			cur.vals[key] = scalar{raw: raw, line: line}
			cur.order = append(cur.order, key)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return doc, nil
}

func header(text, opener, closer string, path string, line int) (string, error) {
	body := strings.TrimSpace(text)
	if i := commentStart(body); i >= 0 {
		body = strings.TrimSpace(body[:i])
	}
	if !strings.HasSuffix(body, closer) {
		return "", fmt.Errorf("%s: %q is not a section header", where(path, line), text)
	}
	name := strings.TrimSpace(body[len(opener) : len(body)-len(closer)])
	if name == "" || strings.ContainsAny(name, "[]. \t\"'") {
		return "", fmt.Errorf("%s: %q is not a section name", where(path, line), name)
	}
	return name, nil
}

func keyValue(text, path string, line int) (key, raw string, err error) {
	eq := strings.Index(text, "=")
	if eq < 0 {
		return "", "", fmt.Errorf("%s: %q is neither a section header nor a "+
			"key = value line", where(path, line), text)
	}
	key = strings.TrimSpace(text[:eq])
	raw = strings.TrimSpace(text[eq+1:])
	if key == "" || strings.ContainsAny(key, " \t\"'[]") {
		return "", "", fmt.Errorf("%s: %q is not a key", where(path, line), key)
	}
	if i := commentStart(raw); i >= 0 {
		raw = strings.TrimSpace(raw[:i])
	}
	if raw == "" {
		return "", "", fmt.Errorf("%s: %s has no value", where(path, line), key)
	}
	return key, raw, nil
}

// commentStart finds a # that begins a comment rather than sitting inside a
// quoted string. Returns -1 when there is none.
func commentStart(s string) int {
	quoted := false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			if quoted {
				i++
			}
		case '"':
			quoted = !quoted
		case '#':
			if !quoted {
				return i
			}
		}
	}
	return -1
}

func where(path string, line int) string {
	return fmt.Sprintf("%s:%d", filepath.Base(path), line)
}

// section returns the single table with this name, or nil.
func (d *document) section(name string) *table {
	for _, t := range d.tables {
		if t.name == name {
			return t
		}
	}
	return nil
}

// array returns every element of an array of tables, in file order.
func (d *document) array(name string) []*table {
	var out []*table
	for _, t := range d.tables {
		if t.name == name {
			out = append(out, t)
		}
	}
	return out
}

// unknown reports every section and key nothing asked for.
//
// This is the whole reason the reader tracks use. A configuration file is read
// once, at the start of a run that may end with a cold wallet on a table, and
// the failure mode worth engineering against is not a malformed file — that one
// announces itself — but a well-formed file with one key spelled slightly
// wrong, quietly running with a default the operator did not choose.
func (d *document) unknown(known map[string]bool) []string {
	var out []string
	for _, t := range d.tables {
		if why, gone := retiredSections[t.name]; gone {
			out = append(out, fmt.Sprintf("%s: [%s] %s",
				where(d.path, t.line), t.name, why))
			continue
		}
		if !known[t.name] {
			out = append(out, fmt.Sprintf("%s: [%s] is not a section this tool reads",
				where(d.path, t.line), t.name))
			continue
		}
		for _, k := range t.order {
			if t.used[k] {
				continue
			}
			if why, gone := retiredKeys[t.name+"."+k]; gone {
				out = append(out, fmt.Sprintf("%s: [%s] %s %s",
					where(d.path, t.vals[k].line), t.name, k, why))
				continue
			}
			out = append(out, fmt.Sprintf("%s: [%s] has no key %q",
				where(d.path, t.vals[k].line), t.name, k))
		}
	}
	sort.Strings(out)
	return out
}

// retiredSections and retiredKeys are what a removed setting says instead of
// "has no key".
//
// Removing a key is a loud breaking change, and that is the right direction to
// fail in — the file is read once, at the start of a run that may end with a
// cold wallet on the table, so a setting that quietly stopped doing anything is
// worse than one that refuses. But the refusal has to be worth reading. "[fees]
// has no key \"mode\"" reads like a typo; "[fees] mode is gone: the fee rate is
// no longer asked of Bitcoin Core" reads like the change it is.
//
// A key belongs here for one release cycle of this file's own making — long
// enough that anybody with a configuration from before the change gets the
// sentence rather than the shrug. Nothing reads these but the refusal.
var retiredSections = map[string]string{
	"server": "is gone, and its one surviving key moved: the run journal's path " +
		"is [journal] path now. A section named for a component that no longer " +
		"exists is the same defect as a key that changes nothing.",
	"bitcoind": "is gone: this tool dials no Bitcoin node. It built the batch " +
		"with Core once — coin selection, change derivation, the fee estimate and " +
		"testmempoolaccept — and it does none of those now. Your wallet builds the " +
		"transaction and this tool checks it. Delete the block.",
	"signer": "is gone: it named the m cold-storage devices a signing round " +
		"went out to, and there is no such round left. The batch is signed once, " +
		"in your own wallet, and reaches this tool through the file --psbt names. " +
		"Delete the block.",
}

var retiredKeys = map[string]string{
	"fees.floor_sat_per_vb": "is gone: it was the floor under Core's " +
		"estimatesmartfee, for the nodes that have no estimate. Core is not asked " +
		"for a fee any more and nothing else is asked either. Set " +
		"target_sat_per_vb to the rate you mean to pay.",
	"fees.target_blocks": "is gone: it was estimatesmartfee's confirmation " +
		"target, and nothing estimates now. Set target_sat_per_vb to the rate you " +
		"mean to pay.",
	"fees.mode": "is gone: it was estimatesmartfee's CONSERVATIVE or ECONOMICAL, " +
		"and nothing estimates now. Set target_sat_per_vb to the rate you mean to " +
		"pay.",
	"limits.abort_after_signing_seconds": "is gone: it was the 5:00 gate, and " +
		"the window it bounded no longer contains a signing round. The batch is " +
		"signed at step 7, after every channel has reached chan_pending, so a slow " +
		"round costs time and cannot cost the batch. Delete the line.",
}

// The accessors below all mark the key used, so unknown can tell the difference
// between a key this tool ignores and a key it does not have.

func (t *table) has(key string) bool {
	if t == nil {
		return false
	}
	_, ok := t.vals[key]
	return ok
}

// lineOf is the line a key was written on, for a refusal that has to point at
// one. Zero when the key is absent.
func (t *table) lineOf(key string) int {
	if t == nil {
		return 0
	}
	return t.vals[key].line
}

func (t *table) str(path, key, def string) (string, error) {
	if t == nil {
		return def, nil
	}
	v, ok := t.vals[key]
	if !ok {
		return def, nil
	}
	t.used[key] = true
	s, err := unquote(v.raw)
	if err != nil {
		return "", fmt.Errorf("%s: %s must be a quoted string: %w",
			where(path, v.line), key, err)
	}
	return s, nil
}

func (t *table) integer(path, key string, def int64) (int64, error) {
	if t == nil {
		return def, nil
	}
	v, ok := t.vals[key]
	if !ok {
		return def, nil
	}
	t.used[key] = true
	n, err := strconv.ParseInt(strings.ReplaceAll(v.raw, "_", ""), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %s must be a whole number, not %s",
			where(path, v.line), key, v.raw)
	}
	return n, nil
}

func (t *table) number(path, key string, def float64) (float64, error) {
	if t == nil {
		return def, nil
	}
	v, ok := t.vals[key]
	if !ok {
		return def, nil
	}
	t.used[key] = true
	f, err := strconv.ParseFloat(strings.ReplaceAll(v.raw, "_", ""), 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %s must be a number, not %s",
			where(path, v.line), key, v.raw)
	}
	return f, nil
}

func (t *table) boolean(path, key string, def bool) (bool, error) {
	if t == nil {
		return def, nil
	}
	v, ok := t.vals[key]
	if !ok {
		return def, nil
	}
	t.used[key] = true
	switch v.raw {
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	return false, fmt.Errorf("%s: %s must be true or false, not %s",
		where(path, v.line), key, v.raw)
}

// unquote reads a double-quoted string with TOML's basic escapes.
func unquote(raw string) (string, error) {
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return "", fmt.Errorf("%s is not in double quotes", raw)
	}
	body := raw[1 : len(raw)-1]
	var b strings.Builder
	for i := 0; i < len(body); i++ {
		if body[i] != '\\' {
			b.WriteByte(body[i])
			continue
		}
		i++
		if i >= len(body) {
			return "", fmt.Errorf("the string ends in a backslash")
		}
		switch body[i] {
		case '\\':
			b.WriteByte('\\')
		case '"':
			b.WriteByte('"')
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		default:
			return "", fmt.Errorf("\\%c is not an escape this reader knows", body[i])
		}
	}
	return b.String(), nil
}
