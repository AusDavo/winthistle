// Command winthistle is the local, guided tool for batch-opening Lightning
// channels from cold storage.
//
// Four commands, and the order they are in is the order they are used:
// print-macaroon-command bakes the credential, doctor checks the setup, run
// opens the batch, recover takes apart a run that stopped somewhere it should
// not have. The web UI docs/design.html describes does not exist yet; these
// commands drive the same packages it will.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/AusDavo/winthistle/internal/abort"
	"github.com/AusDavo/winthistle/internal/bitcoind"
	"github.com/AusDavo/winthistle/internal/config"
	"github.com/AusDavo/winthistle/internal/doctor"
	"github.com/AusDavo/winthistle/internal/journal"
	"github.com/AusDavo/winthistle/internal/lnd"
	"github.com/AusDavo/winthistle/internal/methods"
	"github.com/AusDavo/winthistle/internal/prose"
	"github.com/AusDavo/winthistle/internal/run"
	"github.com/AusDavo/winthistle/internal/signers"
)

const usage = `winthistle — batch-open Lightning channels from cold storage.

Commands:
  doctor                   check every prerequisite and print what fixes each
  run --batch FILE         open the batch: Phase 0, the armed window, Phase 2
  recover [RUN-ID]         list runs that stopped, or take one apart
  print-macaroon-command   print the lncli bakemacaroon line for this build
  example-config           print a winthistle.toml to start from
  example-batch            print a batch file to start from

Common flags:
  --config PATH            winthistle.toml (default: ./winthistle.toml)

The web UI is not built yet. What exists is the whole sequence: the peer
pre-flight, the fee source, the dress rehearsal and the reserve check for
Phase 0; the armed window and its single publish for Phase 1; the confirmation
watch, the policy pass and the CPFP child for Phase 2; and the abort and
recovery paths under all of it. See HANDOFF.md.
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
	psbtDir := fs.String("psbt-dir", "", "where to write PSBTs for signers that "+
		"have no command (default: alongside the journal)")
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

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	batch, err := config.LoadBatch(*batchPath)
	if err != nil {
		return err
	}

	d, closeAll, err := connect(ctx, cfg, *psbtDir)
	if err != nil {
		return err
	}
	defer closeAll()

	if !*yes {
		fmt.Printf("%s\n", batchSummary(batch))
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
		id, err = newRunID()
		if err != nil {
			return err
		}
	}

	res, err := run.Do(ctx, d, run.Options{
		Config: cfg, Batch: batch, RunID: id,
		StopBeforePublish: *stopBefore, Probe: *probe, SettleFor: *settleFor,
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

		runs, err := run.List(ctx, j, os.Stdout)
		if err != nil {
			return err
		}
		if len(runs) > 0 {
			fmt.Printf("\nTake one apart with: winthistle recover %s\n", runs[0].ID)
		}
		return nil
	}

	d, closeAll, err := connect(ctx, cfg, "")
	if err != nil {
		return err
	}
	defer closeAll()

	_, err = run.RecoverOne(ctx, d, fs.Arg(0))
	return err
}

// connect opens everything a run or a recovery needs.
func connect(ctx context.Context, cfg *config.Config, psbtDir string) (
	run.Deps, func(), error) {

	var d run.Deps
	d.Out = os.Stdout
	d.Confirm = confirmBlunt

	cli, err := lnd.Dial(ctx, cfg.LND)
	if err != nil {
		return d, nil, fmt.Errorf("connecting to LND at %s: %w\nTry: winthistle doctor",
			cfg.LND.Address, err)
	}
	d.LND = cli

	nodeCfg := cfg.Bitcoind
	nodeCfg.Wallet = ""
	if d.Node, err = bitcoind.New(nodeCfg); err != nil {
		cli.Close()
		return d, nil, err
	}
	if d.Wallet, err = bitcoind.New(cfg.Bitcoind); err != nil {
		cli.Close()
		return d, nil, err
	}

	j, err := journal.Open(ctx, cfg.Server.Journal)
	if err != nil {
		cli.Close()
		return d, nil, fmt.Errorf("opening the run journal at %s: %w",
			cfg.Server.Journal, err)
	}
	d.Journal = j

	if psbtDir == "" {
		psbtDir = strings.TrimSuffix(cfg.Server.Journal, ".db") + "-psbt"
	}
	if len(cfg.Signers) > 0 {
		if d.Signers, err = signers.New(cfg.Signers, signers.Options{
			Dir: psbtDir, Out: os.Stdout,
		}); err != nil {
			cli.Close()
			j.Close()
			return d, nil, err
		}
	}

	return d, func() { cli.Close(); j.Close() }, nil
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

func batchSummary(b *config.Batch) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "\n%d channel%s, %s in total\n", len(b.Channels),
		prose.Plural(len(b.Channels)), prose.Sats(b.TotalSat()))
	for _, ch := range b.Channels {
		kind := ""
		if ch.Private {
			kind = "  (unannounced)"
		}
		fmt.Fprintf(&sb, "  %14s  %s%s\n", prose.Sats(ch.AmountSat), ch.Peer, kind)
		fmt.Fprintf(&sb, "                  %s\n", ch.Policy.Summary())
	}
	return sb.String()
}

// newRunID is the journal's key: sortable, and unique even if two runs start in
// the same second.
func newRunID() (string, error) {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return time.Now().UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b[:]), nil
}
