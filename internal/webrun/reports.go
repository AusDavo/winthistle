package webrun

import (
	"context"
	"fmt"
	"strings"

	"github.com/AusDavo/winthistle/internal/arm"
	"github.com/AusDavo/winthistle/internal/bitcoind"
	"github.com/AusDavo/winthistle/internal/fees"
	"github.com/AusDavo/winthistle/internal/lnd"
	"github.com/AusDavo/winthistle/internal/peers"
	"github.com/AusDavo/winthistle/internal/prose"
	"github.com/AusDavo/winthistle/internal/reserve"
	"github.com/AusDavo/winthistle/internal/server"
)

// The three pre-flight reports a browser can ask for without starting a run.
//
// They are Phase 0's steps 1, 3 and 5, called on their own and rendered with the
// same Report() `winthistle run` prints. The text is internal/peers',
// internal/fees' and internal/reserve's, which is decision 3's oracle holding
// over strings this package did not write.
//
// This package writes exactly one thing an operator reads: the rule between two
// peers' reports. See Peers for why it is here and not a change to a report.
//
// # Why they dial per call rather than holding connections open
//
// The same reason Unfinished opens the journal per call. `winthistle serve` has
// to start on a machine where LND is down — the doctor screen is exactly what an
// operator reads in that state — and a connection held from New would either
// refuse the start or sit there stale across an evening. Each of these dials what
// its own check needs, reads, and hangs up.
//
// The per-report split is the useful part of that. Fees needs Core and not LND;
// peers and the reserve need LND and not Core. So the fee report answers on a
// node whose LND is down and the peer report answers on one whose Core is down,
// which is the same property /recover has for the journal and is worth more than
// one screen that needs everything.
//
// # None of them opens the run journal, and that is load-bearing
//
// run.Connect would have been the obvious way to reach a *lnd.Client here, and it
// is the wrong one: it opens the journal too. A journal handle held behind a
// screen an operator reloads is a write lock held against `winthistle recover` in
// another terminal, which is the tool they reach for when the UI is the thing
// that looks wrong — Unfinished's comment says so and this is the same hazard.
// It is also what internal/server's doctorMu serialises, so three screens that
// opened no journal are three screens that need no share of it.
//
// # The peer report never connects to a peer
//
// peers.Check dials a peer whose Want carries a host, which is a change to the
// node. A diagnostic must not make one — `winthistle doctor` strips the hosts
// unless it is given --connect, and there is no --connect here to give. So Peers
// strips them unconditionally, and a peer LND is not already connected to reports
// NotTried rather than being reached for.

// Peers is peers.Facts.Report() for every channel in the batch, one after
// another with a rule between them.
//
// The rule is this function's only words and it is a render fix rather than a
// preference. Every peer's report closes with the same four-line caveat — nothing
// here is authoritative, minimum channel size is enforced in accept_channel — and
// on a page that shows every peer at once those four lines arrive three times in
// forty. In a terminal they are separated by the scroll and by everything else
// Phase 0 prints; on one page they read as a stutter. A rule turns the repetition
// into a paragraph each peer's own section ends with, which is what it is.
//
// It changes no report. The rule goes between them, so each Report() is still
// served whole and byte for byte, which is what decision 3's oracle is over.
//
// A peer whose facts are unusable is reported rather than refused. run.Do stops
// on one — it is about to arm — and this screen is not about to do anything, so
// the whole batch is shown and every peer's own paragraph says what is wrong with
// it. Refusing here would hide the other peers behind the first bad one.
func (l *Launcher) Peers(ctx context.Context) (string, error) {
	if l.batch == nil {
		return "", server.ErrNoBatch
	}

	cli, err := lnd.Dial(ctx, l.cfg.LND)
	if err != nil {
		return "", fmt.Errorf("connecting to LND at %s: %w", l.cfg.LND.Address, err)
	}
	defer cli.Close()

	facts, err := peers.Check(ctx, cli.Lightning, noHosts(l.batch.Wants()))
	if err != nil {
		return "", err
	}

	var b strings.Builder
	for i, f := range facts {
		if i > 0 {
			b.WriteString("\n" + peerRule + "\n\n")
		}
		b.WriteString(f.Report())
	}
	return b.String(), nil
}

// peerRule is the line between two peers' reports.
//
// prose.PaneWidth, which is the column every report in this repository is written
// and tested to, so it is exactly as wide as the widest line it separates rather
// than a number chosen to look right.
var peerRule = strings.Repeat("-", prose.PaneWidth)

// noHosts is the --connect that is not on offer.
//
// peers.Check dials a peer whose Want carries a host, and that is a change to the
// node — small, and still a change, made by a screen an operator reloads. So the
// host is removed from the request rather than guarded against further in: a peer
// LND is not already connected to reports NotTried, which is a fact, and the
// alternative would be a diagnostic quietly opening connections.
//
// Its own function so the rule can be tested without a node. It is one line, and
// the one line is the whole difference between a report and an action.
func noHosts(wants []peers.Want) []peers.Want {
	out := make([]peers.Want, len(wants))
	copy(out, wants)
	for i := range out {
		out[i].Host = ""
	}
	return out
}

// Fees is fees.Rate.Report(): the rate, and where the rate came from.
//
// Core with no wallet scope, which is what estimatesmartfee wants and what
// run.Connect gives the same call. Nothing about a fee estimate is per-wallet,
// and a client bound to the cold wallet would make this screen fail on a node
// whose cold wallet is not loaded — a state `make -C regtest bootstrap` exists
// for and which has nothing to do with the fee market.
func (l *Launcher) Fees(ctx context.Context) (string, error) {
	nodeCfg := l.cfg.Bitcoind
	nodeCfg.Wallet = ""
	node, err := bitcoind.New(nodeCfg)
	if err != nil {
		return "", err
	}

	rate, err := fees.Estimate(ctx, node, fees.Request{
		TargetBlocks:  l.cfg.Fees.TargetBlocks,
		Mode:          l.cfg.Fees.Mode,
		FloorSatPerVB: l.cfg.Fees.FloorSatPerVB,
	})
	if err != nil {
		return "", err
	}
	return rate.Report(), nil
}

// Reserve is reserve.Finding.Report(), about this node's own on-chain wallet.
//
// arm.BatchOf rather than a count written here, because the rule it encodes —
// an unannounced channel is invisible to LND's reserve check — is a fact about
// LND that already has one home. A second loop over the batch's channels in this
// package would be a second place that rule could be got wrong.
//
// With no batch it asks about one announced channel, which is what doctor does.
// The reserve is a fact about this node rather than about the batch, so the
// answer is worth having either way; internal/server's screen is what says which
// of the two questions was asked.
func (l *Launcher) Reserve(ctx context.Context) (string, error) {
	cli, err := lnd.Dial(ctx, l.cfg.LND)
	if err != nil {
		return "", fmt.Errorf("connecting to LND at %s: %w", l.cfg.LND.Address, err)
	}
	defer cli.Close()

	b := reserve.Batch{Public: 1}
	if l.batch != nil {
		b = arm.BatchOf(l.batch.ArmChannels())
	}

	f, err := reserve.Check(ctx, cli.WalletKit, b)
	if err != nil {
		return "", err
	}
	return f.Report(), nil
}
