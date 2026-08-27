package run

import (
	"context"
	"fmt"

	"github.com/AusDavo/winthistle/internal/config"
	"github.com/AusDavo/winthistle/internal/journal"
	"github.com/AusDavo/winthistle/internal/lnd"
)

// Connect opens everything a run or a recovery needs, and returns the function
// that closes it.
//
// One socket and one file: LND, and the run journal. Bitcoin Core was dialled
// here too, for the fee estimate and for testmempoolaccept, and item 5 of
// docs/replan-2026-08.md removed both.
//
// It lives here rather than in cmd/winthistle because it is the same set of
// connections for every command that touches a batch, and because a test that
// drives this against the harness should not have to write a configuration file
// to say where the harness is. There was a second reason once — the local web UI
// was a second front door and two sets of dialling decisions would have drifted
// — and it went with the UI.
//
// What it deliberately does not fill in is Out, Confirm and Signing. Those are
// the terminal's own: stdout, the prompts that read stdin, and the three files
// --psbt names — the two it reads back, and the recipients CSV it writes.
func Connect(ctx context.Context, cfg *config.Config) (Deps, func(), error) {
	var d Deps

	cli, err := lnd.Dial(ctx, cfg.LND)
	if err != nil {
		return d, nil, fmt.Errorf("connecting to LND at %s: %w\nTry: winthistle doctor",
			cfg.LND.Address, err)
	}
	d.LND = cli

	j, err := journal.Open(ctx, cfg.Journal.Path)
	if err != nil {
		cli.Close()
		return d, nil, fmt.Errorf("opening the run journal at %s: %w",
			cfg.Journal.Path, err)
	}
	d.Journal = j

	return d, func() { cli.Close(); j.Close() }, nil
}
