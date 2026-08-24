package webrun

import (
	"context"
	"errors"
	"testing"

	"github.com/AusDavo/winthistle/internal/config"
	"github.com/AusDavo/winthistle/internal/peers"
	"github.com/AusDavo/winthistle/internal/server"
)

// TestThePeerReportDropsEveryHost is the rule that keeps a screen from being an
// action.
//
// peers.Check dials a peer whose Want carries a host. `winthistle doctor` strips
// the hosts unless it is given --connect, and there is no --connect on a page — so
// this strips them always. Tested on the request rather than on the outcome,
// because the outcome needs a node and the rule does not.
//
// The copy is the second half. Wants() builds a fresh slice today, and a future
// one that returned the batch's own would have this function editing the batch
// every server in this process shares.
func TestThePeerReportDropsEveryHost(t *testing.T) {
	in := []peers.Want{
		{Pubkey: "03aa", Host: "alice.onion:9735", AmountSat: 1},
		{Pubkey: "03bb", AmountSat: 2},
	}
	out := noHosts(in)

	for i, w := range out {
		if w.Host != "" {
			t.Errorf("want %d kept the host %q, so a report would dial a peer",
				i, w.Host)
		}
		if w.Pubkey != in[i].Pubkey || w.AmountSat != in[i].AmountSat {
			t.Errorf("want %d lost more than its host: %+v", i, w)
		}
	}
	if in[0].Host == "" {
		t.Error("noHosts edited its argument, which is the batch this process shares")
	}
}

// TestThePeerReportRefusesWithoutABatch, and it is the sentinel rather than a
// bare error: "there is no batch" is not a failure to look, and the screen that
// hears it says what to do instead of reporting that something went wrong.
//
// The other two are not tested here for the reason they are the interesting ones:
// both answer without a batch, and answering means reaching a node. The regtest
// test is where that is exercised.
func TestThePeerReportRefusesWithoutABatch(t *testing.T) {
	l := New(&config.Config{}, nil)

	_, err := l.Peers(context.Background())
	if !errors.Is(err, server.ErrNoBatch) {
		t.Fatalf("a launcher with no batch refused the peer report with %v, want "+
			"server.ErrNoBatch — the screen branches on it", err)
	}
}
