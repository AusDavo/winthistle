// Command winthistle is the local, guided UI for batch-opening Lightning
// channels. The sequence and both untimed ends of it exist as packages; the
// server and the UI do not — see HANDOFF.md for build order.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/AusDavo/winthistle/internal/methods"
)

const usage = `winthistle — batch-open Lightning channels from cold storage.

Commands:
  print-macaroon-command   print the lncli bakemacaroon line for this build

This build has no server and no UI yet. What exists is the whole sequence as
packages: the peer pre-flight, the fee source, the dress rehearsal and the
reserve check for Phase 0; the armed window and its single publish for Phase 1;
the confirmation watch, the policy pass and the CPFP child for Phase 2; and the
abort and recovery paths under all of it. See HANDOFF.md.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	switch os.Args[1] {
	case "print-macaroon-command":
		if err := printMacaroonCommand(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "winthistle: %v\n", err)
			os.Exit(1)
		}
	case "-h", "--help", "help":
		fmt.Fprint(os.Stdout, usage)
	default:
		fmt.Fprintf(os.Stderr, "winthistle: unknown command %q\n\n", os.Args[1])
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
}

// printMacaroonCommand writes the bake command to stdout and its reasoning to
// stderr.
//
// The split is so that `winthistle print-macaroon-command | sh` bakes the
// credential while the operator still reads why each permission is there. The
// list is generated from internal/methods, which is what makes it authoritative
// rather than illustrative — CLAUDE.md forbids the alternative.
func printMacaroonCommand(args []string) error {
	fs := flag.NewFlagSet("print-macaroon-command", flag.ContinueOnError)
	saveTo := fs.String("save-to", methods.DefaultMacaroonFile,
		"path lncli should write the baked macaroon to")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	if err := methods.Explain(os.Stderr); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(os.Stdout, methods.BakeCommand(*saveTo)); err != nil {
		return err
	}
	return nil
}
