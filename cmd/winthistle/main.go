// Command winthistle is the local, guided tool for batch-opening Lightning
// channels from cold storage.
//
// Seven commands, and the order they are in is the order they are used: setup
// builds the watch-only wallet from the cold wallet's descriptors,
// print-macaroon-command bakes the credential, doctor checks both, serve puts
// the local web UI on a loopback socket, run opens the batch, bump accelerates
// one that went out too cheap, and recover takes apart a run that stopped
// somewhere it should not have.
//
// serve is the newest and the least finished. It carries the security shape
// docs/design.html asks for — loopback bind, a token printed at startup, strict
// Origin and Host checks, no CORS — and it can now start a run, answer the four
// questions a run asks, and stop one. What it cannot do is publish: the two
// locks on that are the pinned call-site count and internal/server's import ban,
// neither of them a convention. The transports, the countdown and the remaining
// screens are still to come.
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
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/AusDavo/winthistle/internal/abort"
	"github.com/AusDavo/winthistle/internal/bitcoind"
	"github.com/AusDavo/winthistle/internal/bump"
	"github.com/AusDavo/winthistle/internal/coldwallet"
	"github.com/AusDavo/winthistle/internal/config"
	"github.com/AusDavo/winthistle/internal/doctor"
	"github.com/AusDavo/winthistle/internal/fees"
	"github.com/AusDavo/winthistle/internal/journal"
	"github.com/AusDavo/winthistle/internal/methods"
	"github.com/AusDavo/winthistle/internal/prose"
	"github.com/AusDavo/winthistle/internal/run"
	"github.com/AusDavo/winthistle/internal/server"
	"github.com/AusDavo/winthistle/internal/setup"
	"github.com/AusDavo/winthistle/internal/webrun"
)

const usage = `winthistle — batch-open Lightning channels from cold storage.

Commands:
  setup                    build the watch-only wallet from the cold wallet's
                           descriptors, and end by comparing addresses
  doctor                   check every prerequisite and print what fixes each
  serve                    serve the local web UI on [server] bind, and print
                           the URL with the startup token in it
  run --batch FILE         open the batch: Phase 0, the armed window, Phase 2
  bump RUN-ID              build, sign and broadcast a CPFP child of a stalled
                           batch. Never a replacement — see I-4
  recover [RUN-ID]         list runs that stopped, or take one apart
  print-macaroon-command   print the lncli bakemacaroon line for this build
  example-config           print a winthistle.toml to start from
  example-batch            print a batch file to start from
  example-descriptors      print the cold wallet descriptor file to start from

Common flags:
  --config PATH            winthistle.toml (default: ./winthistle.toml)

The web UI can open a batch: it starts a run, answers the four questions a run
asks, and stops one. What it cannot do is publish — that stays inside the
sequence that earned it. Missing from it still: the file and QR transports, the
countdown, and the screens that list what an earlier run left behind, which
recover is still the only way to read.

Either front door drives the same sequence: the peer pre-flight, the fee source,
the dress rehearsal and the reserve check for Phase 0; the armed window and its
single publish for Phase 1; the confirmation watch, the policy pass and the CPFP
child for Phase 2; and the abort and recovery paths under all of it. See
HANDOFF.md.
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
	case "setup":
		err = setupCmd(ctx, os.Args[2:])
	case "print-macaroon-command":
		err = printMacaroonCommand(os.Args[2:])
	case "doctor":
		err = doctorCmd(ctx, os.Args[2:])
	case "serve":
		err = serveCmd(ctx, os.Args[2:])
	case "run":
		err = runCmd(ctx, os.Args[2:])
	case "bump":
		err = bumpCmd(ctx, os.Args[2:])
	case "recover":
		err = recoverCmd(ctx, os.Args[2:])
	case "example-config":
		fmt.Print(config.Example)
	case "example-batch":
		fmt.Print(config.ExampleBatch)
	case "example-descriptors":
		fmt.Print(config.ExampleDescriptors)
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

// setupCmd is `winthistle setup`: the watch-only wallet, and the one question
// the program cannot answer for itself.
//
// There is no --yes here and there must not be. Every other prompt in this tool
// guards a decision the operator has already made by running the command; this
// one is the operator *supplying evidence* — that they looked at a hardware
// wallet and saw the same addresses — and a flag that answered it would be a
// flag that fabricates the evidence. The command is re-runnable instead: nothing
// is lost by walking away, because deriveaddresses has no side effect and the
// same addresses are there tomorrow.
func setupCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	cfgPath := fs.String("config", config.DefaultPath, "winthistle.toml")
	descPath := fs.String("descriptors", "", "the cold wallet's descriptor file: "+
		"both branches and the birthday. Leave it out to re-ask about the "+
		"descriptors already in the wallet")
	gapLimit := fs.Int("gap-limit", coldwallet.DefaultGapLimit,
		"the top of the imported descriptor range. Core grows it on its own and "+
			"then refuses to shrink it, so this is a floor rather than a setting")
	sample := fs.Int("sample", coldwallet.DefaultSampleSize,
		"how many addresses per branch to compare. Five rather than one because "+
			"sortedmulti and multi agree at about half of all indices for a 2-of-2")
	rescan := fs.Duration("rescan-timeout", 6*time.Hour,
		"how long to allow importdescriptors, which blocks for the whole rescan")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}

	var descs *config.Descriptors
	if *descPath != "" {
		if descs, err = config.LoadDescriptors(*descPath); err != nil {
			return err
		}
	}

	// The import blocks for the whole rescan — minutes to hours on mainnet — so
	// both clients get a timeout that covers it rather than bitcoind's two
	// minutes. The design's other half of this is the rule that an import
	// happens during setup and never during a batch.
	nodeCfg := cfg.Bitcoind
	nodeCfg.Wallet, nodeCfg.Timeout = "", *rescan
	node, err := bitcoind.New(nodeCfg)
	if err != nil {
		return err
	}
	walletCfg := cfg.Bitcoind
	walletCfg.Timeout = *rescan
	wallet, err := bitcoind.New(walletCfg)
	if err != nil {
		return err
	}

	if dir := filepath.Dir(cfg.Server.Journal); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("making %s: %w", dir, err)
		}
	}
	j, err := journal.Open(ctx, cfg.Server.Journal)
	if err != nil {
		return fmt.Errorf("opening the run journal at %s: %w", cfg.Server.Journal, err)
	}
	defer j.Close()

	_, err = setup.Do(ctx, setup.Deps{
		Node: node, Wallet: wallet, Journal: j, Out: os.Stdout,
		Ask: askComparison,
	}, setup.Options{
		WalletName:  cfg.Bitcoind.Wallet,
		Descriptors: descs,
		GapLimit:    *gapLimit,
		SampleSize:  *sample,
	})
	return err
}

// askComparison is the three-way prompt behind the address check.
//
// Three answers rather than two, because "no" and "not yet" are different facts
// and only one of them is a wallet that must not fund a batch. Anything that is
// not a clear yes or no — a bare return, a typo, end of input — is "not yet",
// which records nothing. That is the safe default in both directions: a stray
// keypress cannot confirm a wallet nobody looked at, and it cannot condemn a
// working one either.
func askComparison(_ context.Context, c coldwallet.AddressCheck) (setup.Answer, error) {
	fmt.Printf("\n%s\n", setup.Question)
	fmt.Print("  yes / no / anything else if you have not compared them yet: ")

	line, err := stdin.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return setup.NotAnswered, fmt.Errorf("reading the answer: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return setup.Matched, nil
	case "n", "no":
		return setup.Differed, nil
	}
	return setup.NotAnswered, nil
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

// serveCmd starts the local web UI.
//
// Ctrl-C here shuts the socket down and, if a run is going, cancels it and waits
// for it to come apart. That used to say "no run is touched", which was true
// only because nothing this UI served could start one; now that it can, the
// honest reading of decision 2 is its own sentence — what ends a run is the
// clock, or the operator's Ctrl-C on the process. A tab closing is still
// nothing, and that is the asymmetry worth knowing about. internal/server's
// package comment and HANDOFF.md both say why at length.
func serveCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	cfgPath := fs.String("config", config.DefaultPath, "winthistle.toml")
	batchPath := fs.String("batch", "", "a batch file, so the doctor screen's "+
		"peer and anchor-reserve checks are about the batch you mean to open")
	allowConnect := fs.Bool("connect", false, "let the doctor screen's peer "+
		"check connect to peers that are not connected already")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	opts := server.Options{Doctor: doctor.Options{Connect: *allowConnect}}
	if *batchPath != "" {
		opts.Doctor.Batch, err = config.LoadBatch(*batchPath)
		if err != nil {
			return err
		}
	}

	// The launcher is what a POST to /runs hands the work to. It is built even
	// without a batch — a launcher with none reports so, and the control is
	// absent rather than offered and then refused — and it dials nothing until a
	// run actually starts, because `winthistle serve` has to work on a machine
	// where LND is down. That is the state the doctor screen is read in.
	opts.Launcher = webrun.New(cfg, opts.Doctor.Batch)

	s, err := server.New(cfg, opts)
	if err != nil {
		return err
	}
	return s.Serve(ctx, os.Stdout)
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
		fmt.Printf("\n%s", webrun.Summary(batch))
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
		id, err = server.NewRunID()
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

// bumpCmd is `winthistle bump`: the CPFP child of a batch that went out too
// cheap.
//
// It takes a run id rather than a txid, and that is the interface rather than a
// convenience. The journal is what says a transaction was actually published,
// what its change output was, and whether an earlier child of it is already
// holding a coin lock — and a bump built without any of that would be a
// transaction with no record of why it exists.
func bumpCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("bump", flag.ContinueOnError)
	cfgPath := fs.String("config", config.DefaultPath, "winthistle.toml")
	target := fs.Float64("target", 0, "the sat/vB to lift the batch and its child "+
		"to together — not the child's own rate. Zero asks Core, through the same "+
		"estimator the batch used")
	buildOnly := fs.Bool("build-only", false, "build and verify the child, and ask "+
		"no device for anything. The arithmetic is the part that can be wrong")
	abandon := fs.Bool("abandon", false, "give up on this run's unfinished child "+
		"and release the coin lock it is holding on the batch's change output")
	psbtDir := fs.String("psbt-dir", "", "where to write PSBTs for signers that "+
		"have no command (default: alongside the journal)")
	yes := fs.Bool("yes", false, "do not ask before the signing round")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("bump needs the id of the run whose batch it is " +
			"accelerating: `winthistle recover` lists them")
	}
	runID := fs.Arg(0)

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	d, closeAll, err := connect(ctx, cfg, *psbtDir)
	if err != nil {
		return err
	}
	defer closeAll()

	deps := bump.Deps{
		LND:       d.LND.Lightning,
		Publisher: d.LND.WalletKit,
		Node:      d.Node,
		Wallet:    d.Wallet,
		Journal:   d.Journal,
		Signers:   d.Signers,
		Out:       os.Stdout,
	}
	if !*yes {
		deps.Approve = approve
	}

	if *abandon {
		return bump.Give(ctx, deps, runID)
	}

	res, err := bump.Do(ctx, deps, bump.Options{
		RunID:          runID,
		TargetSatPerVB: *target,
		BuildOnly:      *buildOnly,
		Fees: fees.Request{
			TargetBlocks:  cfg.Fees.TargetBlocks,
			Mode:          cfg.Fees.Mode,
			FloorSatPerVB: cfg.Fees.FloorSatPerVB,
		},
	})
	if res != nil && res.Seq > 0 {
		fmt.Printf("\nrun %s, bump %d\n", res.RunID, res.Seq)
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
		// The children too. They are not runs and they are not aborted the same
		// way, but they are the other thing in this journal that can be left
		// half-done, and a screen that listed one and not the other would be
		// telling somebody their node is clean when a coin of theirs is locked.
		return bump.List(ctx, j, os.Stdout)
	}

	d, closeAll, err := connect(ctx, cfg, "")
	if err != nil {
		return err
	}
	defer closeAll()

	_, err = run.RecoverOne(ctx, d, fs.Arg(0))
	return err
}

// connect is the terminal's front door onto run.Connect.
//
// The dialling itself moved to internal/run when the web UI grew a second front
// door: decision 1 is that the CLI and the browser are one code path through
// run.Do, and two sets of dialling decisions underneath that would drift. What
// stays here is the part that is genuinely a terminal's — stdout, and the two
// prompts that read stdin.
func connect(ctx context.Context, cfg *config.Config, psbtDir string) (
	run.Deps, func(), error) {

	d, closeAll, err := run.Connect(ctx, cfg)
	if err != nil {
		return d, nil, err
	}
	d.Out = os.Stdout
	d.Confirm = confirmBlunt

	if d.Signers, err = run.ConfiguredSigners(cfg, psbtDir, os.Stdout); err != nil {
		closeAll()
		return d, nil, err
	}
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

// approve is the ordinary yes/no in front of a signing round.
//
// Not the same kind of prompt as confirmBlunt, and it does not pretend to be:
// nothing after this point can lose the batch. What it is protecting is the
// operator's evening — a bump is a second cold-wallet session, and the screen
// above it is the arithmetic they are agreeing to pay.
func approve(_ context.Context, question string) (bool, error) {
	return ask(question)
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
