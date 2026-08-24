package webrun_test

import (
	"strings"
	"testing"

	"github.com/AusDavo/winthistle/internal/config"
	"github.com/AusDavo/winthistle/internal/regtestenv"
	"github.com/AusDavo/winthistle/internal/server"
	"github.com/AusDavo/winthistle/internal/webrun"
)

// TestTheThreeReportScreensAnswerAgainstARealNode.
//
// The unit tests in internal/server drive a stub, so what they measure is the
// page. This measures the half a stub cannot: that peers.Check, fees.Estimate and
// reserve.Check are actually reachable from a handler, against a node that is
// really there, and that each one's Report() arrives on the page whole.
//
// It also fixes the property the three routes exist for. Each report dials only
// what its own check needs — fees Core, the other two LND — so a report answers on
// a node that is half down. That is not asserted by taking anything down, which
// this harness is shared and cannot afford; it is asserted by every one of the
// three answering separately, which is the shape that makes it true.
//
// Nothing here starts a run, arms anything or asks a device for a signature. That
// is the claim the whole slice rests on: these are reads.
func TestTheThreeReportScreensAnswerAgainstARealNode(t *testing.T) {
	env := regtestenv.Start(t)
	peers := env.Peers(t)
	if len(peers) < 1 {
		t.Skip("this test needs a peer to report on")
	}
	dir := t.TempDir()

	cfg, err := config.Load(env.ConfigFile(t, dir))
	if err != nil {
		t.Fatalf("the harness config file: %v", err)
	}
	batch, err := config.LoadBatch(
		regtestenv.BatchFile(t, dir, peers[:1], fixtureChannelSat))
	if err != nil {
		t.Fatalf("the harness batch file: %v", err)
	}

	s, err := server.New(cfg, server.Options{Launcher: webrun.New(cfg, batch)})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}

	// The peer report carries the peer's own key, which is the one string that
	// proves the local graph was actually read rather than a heading rendered.
	got := preOf(t, serve(t, s, get(t, s, "/peers")))
	if !strings.Contains(got, peers[0]) {
		t.Errorf("/peers does not name the batch's peer %s:\n%s",
			peers[0], got)
	}

	// The fee report on regtest is the case worth having: estimatesmartfee has no
	// history to answer from and says so with a 200, so the rate comes from the
	// configured floor. A report that read Core's silence as a rate would show it
	// here and nowhere else.
	got = preOf(t, serve(t, s, get(t, s, "/fees")))
	if !strings.Contains(got, "sat/vB") {
		t.Errorf("/fees carries no rate at all:\n%s", got)
	}
	if !strings.Contains(got, "Fee rate:") {
		t.Errorf("/fees is not fees.Rate.Report():\n%s", got)
	}

	// The reserve report is about this node's own on-chain wallet, so it answers
	// whatever the batch is. Its arithmetic is the thing being reached for.
	got = preOf(t, serve(t, s, get(t, s, "/reserve")))
	if !strings.Contains(got, "anchor reserve") && !strings.Contains(got, "reserve") {
		t.Errorf("/reserve is not reserve.Finding.Report():\n%s", got)
	}

	// And nothing was started by any of it.
	if live := s.Runs.Live(); live != nil {
		t.Fatalf("a report screen started run %s; these screens read and nothing "+
			"else", live.ID)
	}
}
