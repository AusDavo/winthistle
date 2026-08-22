package methods

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// DefaultMacaroonFile is where the printed command saves the credential. It
// matches the path winthistle.toml's example uses.
const DefaultMacaroonFile = "winthistle.macaroon"

// BakeCommand renders the lncli invocation that bakes a credential granting
// exactly the methods this build calls, and nothing else.
//
// The permissions are "uri" entities, not LND's coarse ones, and that is the
// whole point: FundingStateStep's own permission is onchain:write + offchain:write,
// and a macaroon carrying those two entities could also SendCoins and SendPayment.
// The uri entity restricts the credential to literal method paths — see
// BakeMacaroon in rpcserver.go, which validates each one against the set of
// registered methods.
func BakeCommand(saveTo string) string {
	if saveTo == "" {
		saveTo = DefaultMacaroonFile
	}
	app := App()

	var b strings.Builder
	fmt.Fprintf(&b, "lncli bakemacaroon --save_to %s", saveTo)
	for _, m := range app {
		fmt.Fprintf(&b, " \\\n  %s", m.URI())
	}
	return b.String()
}

// Explain writes the reasoning that belongs beside the command: what each method
// is for, what this build can therefore do, and what the credential still cannot
// do no matter how the batch goes.
//
// Separate from BakeCommand so the command can go to stdout and this to stderr —
// `winthistle print-macaroon-command | sh` then does the right thing.
func Explain(w io.Writer) error {
	app, all := App(), All()

	var harness []Method
	for _, m := range all {
		if m.Use == InHarness {
			harness = append(harness, m)
		}
	}

	bw := &errWriter{w: w}

	fmt.Fprintf(bw, "%d method%s, generated from the registry in internal/methods.\n",
		len(app), plural(len(app)))
	fmt.Fprint(bw, "Do not hand this tool admin.macaroon.\n\n")
	fmt.Fprint(bw, para("The second column is the coarse permission LND itself "+
		"records for the call. It is shown to make the case against baking it: "+
		"the entity that would cover this method also covers methods this tool "+
		"must never be able to reach.", ""))
	fmt.Fprintln(bw)

	for _, svc := range services(app) {
		fmt.Fprintf(bw, "%s\n", svc)
		for _, m := range app {
			if m.Service() != svc {
				continue
			}
			fmt.Fprintf(bw, "  %-20s  %s\n", m.Short(), ops(m.Ops))
			fmt.Fprint(bw, para(m.Why, "      "))
		}
		fmt.Fprintln(bw)
	}

	fmt.Fprint(bw, para(fmt.Sprintf("Whatever else happens, this credential "+
		"cannot %s. Those are refused at the registry, not merely left out of "+
		"it. And if a method is missing the tool fails at setup, which is the "+
		"right direction to fail in.", capabilities()), ""))

	if len(harness) > 0 {
		fmt.Fprintf(bw, "\nDeliberately absent — the regtest fixtures call %s, the app does not:\n",
			pluralThese(len(harness)))
		for _, m := range harness {
			fmt.Fprintf(bw, "  %s\n", m.Name)
		}
	}
	return bw.err
}

// para wraps explanatory text to a fixed column, indented.
//
// Same reasoning as internal/reserve's copy: this is read in a terminal, often a
// narrow one, and text that overruns is text nobody reads.
func para(text, indent string) string {
	const width = 76

	var (
		b      strings.Builder
		line   = indent
		filled bool
	)
	for _, word := range strings.Fields(text) {
		if filled && len(line)+1+len(word) > width {
			b.WriteString(line + "\n")
			line, filled = indent, false
		}
		if filled {
			line += " "
		}
		line += word
		filled = true
	}
	if filled {
		b.WriteString(line + "\n")
	}
	return b.String()
}

// capabilities lists, once each, what the never-list rules out.
func capabilities() string {
	seen := map[string]bool{}
	var out []string
	for _, c := range Forbidden() {
		if !seen[c.Does] {
			seen[c.Does] = true
			out = append(out, c.Does)
		}
	}
	sort.Strings(out)
	switch len(out) {
	case 0:
		return "do anything at all"
	case 1:
		return out[0]
	default:
		return strings.Join(out[:len(out)-1], ", ") + " or " + out[len(out)-1]
	}
}

// services returns the distinct proto services in ms, in a stable order.
func services(ms []Method) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range ms {
		if !seen[m.Service()] {
			seen[m.Service()] = true
			out = append(out, m.Service())
		}
	}
	sort.Strings(out)
	return out
}

func ops(in []Op) string {
	parts := make([]string, 0, len(in))
	for _, o := range in {
		parts = append(parts, o.String())
	}
	return strings.Join(parts, " + ")
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func pluralThese(n int) string {
	if n == 1 {
		return "this one"
	}
	return "these"
}

// errWriter collapses a run of Fprintf calls into one error check.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) Write(p []byte) (int, error) {
	if e.err != nil {
		return 0, e.err
	}
	n, err := e.w.Write(p)
	e.err = err
	return n, err
}
