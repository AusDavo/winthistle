package run

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/AusDavo/winthistle/internal/bitcoind"
	"github.com/AusDavo/winthistle/internal/config"
	"github.com/AusDavo/winthistle/internal/journal"
	"github.com/AusDavo/winthistle/internal/lnd"
	"github.com/AusDavo/winthistle/internal/signers"
)

// Connect opens everything a run, a bump or a recovery needs, and returns the
// function that closes it.
//
// It lives here rather than in cmd/winthistle because it is the same set of
// connections for every command that touches a batch, and because a test that
// drives this against the harness should not have to write a configuration file
// to say where the harness is. There was a second reason once — the local web UI
// was a second front door and two sets of dialling decisions would have drifted
// — and it went with the UI.
//
// What it deliberately does not fill in is Out, Confirm and Signers. Those are
// the terminal's own: stdout, and the prompts that read stdin.
func Connect(ctx context.Context, cfg *config.Config) (Deps, func(), error) {
	var d Deps

	cli, err := lnd.Dial(ctx, cfg.LND)
	if err != nil {
		return d, nil, fmt.Errorf("connecting to LND at %s: %w\nTry: winthistle doctor",
			cfg.LND.Address, err)
	}
	d.LND = cli

	// Node has no wallet scope — estimatesmartfee and testmempoolaccept — and
	// Wallet is bound to the watch-only cold wallet.
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

	return d, func() { cli.Close(); j.Close() }, nil
}

// ConfiguredSigners builds the transports from the [[signer]] blocks.
//
// For the CPFP child, which is the only multi-device round left. The batch's
// wallet is a SigningWallet and comes in through --psbt; these are the devices
// `winthistle bump` asks for partial signatures, and a configuration with no
// [[signer]] blocks is now an ordinary configuration rather than an unusable one.
//
// Separate from Connect because it reads the configuration rather than opening a
// connection, and because it is the one thing here a run does not want at all.
// Nil for an empty configuration is right — bump refuses a child with no
// signers, and the refusal it gives says what to add.
func ConfiguredSigners(cfg *config.Config, psbtDir string, out io.Writer) (Signers, error) {
	if len(cfg.Signers) == 0 {
		return nil, nil
	}
	if psbtDir == "" {
		psbtDir = DefaultPSBTDir(cfg)
	}
	set, err := signers.New(cfg.Signers, signers.Options{Dir: psbtDir, Out: out})
	if err != nil {
		// Spelled out rather than returned straight through: `return signers.New(…)`
		// would hand back a Signers interface holding a nil *signers.Set, which is
		// not nil, and Do's "no signers" refusal would be skipped in favour of a
		// nil dereference.
		return nil, err
	}
	return set, nil
}

// DefaultPSBTDir is where the file handshake writes when nothing named a
// directory: beside the journal, because that is the one path the configuration
// always has.
func DefaultPSBTDir(cfg *config.Config) string {
	return strings.TrimSuffix(cfg.Server.Journal, ".db") + "-psbt"
}
