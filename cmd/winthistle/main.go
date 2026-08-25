// Command winthistle is the local, guided tool for batch-opening Lightning
// channels from cold storage.
//
// Four commands, and the order they are in is the order they are used:
// print-macaroon-command bakes the credential LND will accept, doctor checks
// that and everything else, run opens the batch, and recover takes apart a run
// that stopped somewhere it should not have.
//
// There is one front door now. The local web UI is gone: it was a second
// renderer of the same reports and a second place for the copy to be wrong, and
// the only thing that ever wanted live redraw is the peers' ten-minute clock,
// which the inversion took the signing round out of.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/AusDavo/winthistle/internal/abort"
	"github.com/AusDavo/winthistle/internal/config"
	"github.com/AusDavo/winthistle/internal/doctor"
	"github.com/AusDavo/winthistle/internal/journal"
	"github.com/AusDavo/winthistle/internal/methods"
	"github.com/AusDavo/winthistle/internal/prose"
	"github.com/AusDavo/winthistle/internal/run"
)

const usage = `winthistle — batch-open Lightning channels from cold storage.

Commands:
  doctor                   check every prerequisite and print what fixes each
  run --batch FILE --psbt FILE
                           open the batch: Phase 0, the armed window, Phase 2.
                           --psbt is where you save the transaction you build
                           in Sparrow, and where the signed one is read back
  recover [RUN-ID]         list runs that stopped, or take one apart
  print-macaroon-command   print the lncli bakemacaroon line for this build
  example-config           print a winthistle.toml to start from
  example-batch            print a batch file to start from

Common flags:
  --config PATH            winthistle.toml (default: ./winthistle.toml)

This program builds nothing, holds no keys, selects no coins and derives no
addresses. It does the two things Sparrow and LND cannot do between them: it
attributes the funding outputs to peers, so you can see which peer each one
funds and at what amount, and it holds the gate — every channel reaches
chan_pending before the transaction is allowed to reach the network.

The sequence is the peer pre-flight, the fee source and the reserve check for
Phase 0; the armed window, the wallet's two visits and the single publish for
Phase 1; and the confirmation watch and the policy pass for Phase 2, with the
abort and recovery paths under all of it. See HANDOFF.md.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	// Ctrl-C during the armed window has to unwind rather than exit: there are
	// peers holding reservations and a fence of coin locks in Core, and the
	// deferred teardown is what releases both. So the signal cancels the
	// context and the run reports what it took apart.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "print-macaroon-command":
		err = printMacaroonCommand(os.Args[2:])
	case "doctor":
		err = doctorCmd(ctx, os.Args[2:])
	case "run":
		err = runCmd(ctx, os.Args[2:])
	case "recover":
		err = recoverCmd(ctx, os.Args[2:])
	case "example-config":
		fmt.Print(config.Example)
	case "example-batch":
		fmt.Print(config.ExampleBatch)
	case "-h", "--help", "help":
		fmt.Fprint(os.Stdout, usage)
	default:
		fmt.Fprintf(os.Stderr, "winthistle: unknown command %q\n\n", os.Args[1])
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nwinthistle: %v\n", err)
		os.Exit(1)
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

func doctorCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	cfgPath := fs.String("config", config.DefaultPath, "winthistle.toml")
	batchPath := fs.String("batch", "", "a batch file, so the peers and the "+
		"anchor reserve are checked against the batch you mean to open")
	allowConnect := fs.Bool("connect", false, "let the peer check connect to peers "+
		"that are not connected already")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	opts := doctor.Options{Connect: *allowConnect}
	if *batchPath != "" {
		opts.Batch, err = config.LoadBatch(*batchPath)
		if err != nil {
			return err
		}
	}

	report := doctor.Run(ctx, cfg, opts)
	fmt.Print(report.Report())
	if !report.OK() {
		return errors.New(report.Summary())
	}
	return nil
}

func runCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	cfgPath := fs.String("config", config.DefaultPath, "winthistle.toml")
	batchPath := fs.String("batch", "", "the batch file: peers, amounts, policy")
	runID := fs.String("id", "", "the journal's key for this run (default: the time)")
	stopBefore := fs.Bool("stop-before-publish", false,
		"the cold probe: run the whole production path and withhold step 9")
	probe := fs.Bool("probe", false, "shim-probe every peer first. Costs each "+
		"accepted peer a pending-channel slot for ~11 minutes, and this run then "+
		"waits that out before arming")
	psbtPath := fs.String("psbt", "", "where the transaction you build in Sparrow "+
		"gets saved, and where this run reads it back from. The signed one goes "+
		"beside it with -signed on the name. Required")
	change := fs.String("change", "", "your wallet's change address, if you want "+
		"the verifier to check the script rather than recognise the key origins. "+
		"Needed only by a wallet that writes no key origins at all")
	settleFor := fs.Duration("settle-for", run.DefaultSettleFor,
		"how long Phase 2 watches for confirmations and applies policies")
	yes := fs.Bool("yes", false, "do not ask before arming. The blunt-abandon "+
		"confirmation is still asked, always")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *batchPath == "" {
		return errors.New("run needs --batch FILE. `winthistle example-batch` " +
			"prints one to start from")
	}
	// Checked before LND is dialled and long before a peer is told anything: it
	// refuses a path that already exists, and a refusal here has cost nothing.
	wallet, err := run.NewFileWallet(*psbtPath, os.Stdout)
	if err != nil {
		return err
	}

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	batch, err := config.LoadBatch(*batchPath)
	if err != nil {
		return err
	}

	d, closeAll, err := connect(ctx, cfg)
	if err != nil {
		return err
	}
	defer closeAll()
	d.Signing = wallet

	if !*yes {
		fmt.Printf("\n%s", run.BatchSummary(batch))
		ok, err := ask("Open this batch?")
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("nothing was opened")
		}
	}

	id := *runID
	if id == "" {
		id, err = run.NewRunID()
		if err != nil {
			return err
		}
	}

	res, err := run.Do(ctx, d, run.Options{
		Config: cfg, Batch: batch, RunID: id,
		StopBeforePublish: *stopBefore, Probe: *probe, SettleFor: *settleFor,
		Change: *change,
	})
	if res != nil {
		fmt.Printf("\nrun %s\n", res.RunID)
	}
	return err
}

func recoverCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("recover", flag.ContinueOnError)
	cfgPath := fs.String("config", config.DefaultPath, "winthistle.toml")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}

	// Listing needs nothing but the journal, and that is the point: the screen
	// that decides what to do next is readable on a node that is down.
	if fs.NArg() == 0 {
		j, err := journal.Open(ctx, cfg.Server.Journal)
		if err != nil {
			return err
		}
		defer j.Close()

		// Runs and then the CPFP children, and the pairing is inside run.Unfinished
		// rather than here: they are not the same thing and they are not aborted
		// the same way, but a screen that listed one and not the other would be
		// telling somebody their node is clean when a coin of theirs is locked.
		runs, err := run.Unfinished(ctx, j, os.Stdout)
		if err != nil {
			return err
		}
		if len(runs) > 0 {
			fmt.Printf("\nTake one apart with: winthistle recover %s\n", runs[0].ID)
		}
		return nil
	}

	d, closeAll, err := connect(ctx, cfg)
	if err != nil {
		return err
	}
	defer closeAll()

	_, err = run.RecoverOne(ctx, d, fs.Arg(0))
	return err
}

// connect is the terminal's front door onto run.Connect.
//
// The dialling itself lives in internal/run so that every command that touches a
// batch opens the same connections the same way. What stays here is the part
// that is genuinely a terminal's — stdout, and the two prompts that read stdin.
func connect(ctx context.Context, cfg *config.Config) (run.Deps, func(), error) {

	d, closeAll, err := run.Connect(ctx, cfg)
	if err != nil {
		return d, nil, err
	}
	d.Out = os.Stdout
	d.Confirm = confirmBlunt
	return d, closeAll, nil
}

// confirmBlunt is the only prompt in the product where a human authorises
// something that could lose funds if the premise were wrong.
//
// The copy is prose.BluntConfirmation's, unchanged: it says what the premise
// is, why LND's refusal of the safe flag is expected rather than alarming, what
// the fallback gives up, and what has already been checked.
//
// An answer piped in is accepted, and that is deliberate rather than lax. The
// thing this prompt guards against is nobody seeing it, which is end-of-input —
// and that is refused. A pipe is somebody who decided in advance. The real
// safety mechanism here is not the prompt anyway: abort.AbandonPending asks
// PendingChannels itself and refuses a channel that is not pending, offering no
// confirmation at all, because there is nothing a human could usefully
// authorise about removing a live channel.
func confirmBlunt(_ context.Context, req abort.BluntRequest) (bool, error) {
	fmt.Print(prose.BluntConfirmation(req))
	return ask("Abandon it with i_know_what_i_am_doing?")
}

// stdin is read through one buffered reader for the life of the process. A
// fresh bufio.Reader per question would read ahead and swallow the next answer.
var stdin = bufio.NewReader(os.Stdin)

func ask(question string) (bool, error) {
	fmt.Printf("\n%s [y/N] ", question)
	line, err := stdin.ReadString('\n')
	if errors.Is(err, io.EOF) && strings.TrimSpace(line) == "" {
		fmt.Println("\nNothing answered — stdin is at end of input. Taking that " +
			"as no: an authorisation nobody gave is not an authorisation.")
		return false, nil
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("reading the answer: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	}
	return false, nil
}

func loadConfig(path string) (*config.Config, error) {
	cfg, err := config.Load(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%s does not exist. `winthistle example-config` "+
			"prints one to start from:\n\n    winthistle example-config > %s",
			path, path)
	}
	return cfg, err
}
