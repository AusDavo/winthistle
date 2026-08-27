package regtestenv

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AusDavo/winthistle/internal/lnd"
)

// ForceCloseOutOfBand force-closes one of a harness node's channels through
// regtest/bin/lncli and returns the txid of the commitment transaction LND
// broadcast.
//
// Out of band of the Go client entirely, and that is the whole point rather than
// a convenience. internal/methods' never-list carries
// /lnrpc.Lightning/CloseChannel — one of the ten refusals `winthistle doctor`
// makes LND confirm about the baked credential — and
// TestEveryLNDCallSiteIsRegistered loads "./..." with Tests: true, so a Go call
// to a never-listed method anywhere in this module, harness and _test.go
// included, trips the guard. The only way to quiet it would be to register a
// method whose *inability to be called* is a stated property of the product.
//
// So the test that proves a channel is force-closeable cannot be written in Go
// against this module's client, and should not be: the app is deliberately
// incapable of closing a channel. The harness plays the operator instead, with
// the operator's own tool, the same way it plays Sparrow with a Core wallet. This
// takes a *testing.T, which is what keeps internal/ from calling it.
//
// There is no docker control here and there must not be. This shells out to the
// same wrapper script an operator would run; starting and stopping containers is
// a different capability and no test in this repository needs it.
//
// A unilateral close broadcasts the node's *local* commitment transaction, which
// spends the 2-of-2 funding output. It needs nothing from the peer at the moment
// it is made — which is exactly why the signature the peer handed us at funding
// time is the whole of what makes it work, and why this is the observation that
// turns I-1's chan_pending receipt from a reading of LND's source into a
// measurement.
func (e *Env) ForceCloseOutOfBand(t *testing.T, node string, cp lnd.ChannelPoint) string {
	t.Helper()

	stdout, stderr, err := e.lncli(t, node, "closechannel", "--force",
		"--chan_point", cp.String())
	if err != nil {
		t.Fatalf("force-closing %s from %s: %v\n%s", cp, node, err, stderr)
	}
	if s := strings.TrimSpace(stderr); s != "" {
		t.Logf("lncli %s closechannel --force said: %s", node, s)
	}
	txid, err := closingTxIDIn(stdout)
	if err != nil {
		t.Fatalf("reading the commitment txid out of lncli's output: %v\n%s", err, stdout)
	}
	return txid
}

// lncli runs regtest/bin/lncli against one harness node.
//
// stdout and stderr come back separately because lncli uses both: it writes its
// progress to stderr and its one line of JSON to stdout — see
// executeChannelClose in lnd's cmd/commands, which says so in a comment — so a
// caller that merged them could not parse either.
func (e *Env) lncli(t *testing.T, node string, args ...string) (
	stdout, stderr string, err error) {

	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	script := filepath.Join(e.Root, "regtest", "bin", "lncli")
	cmd := exec.CommandContext(ctx, script, append([]string{node}, args...)...)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err = cmd.Run()
	return out.String(), errOut.String(), err
}

// closingTxIDIn reads lncli closechannel's one line of JSON.
//
// {"closing_txid": "..."} — printed by closeChannel as soon as the
// CloseStatusUpdate_ClosePending update arrives, which is after the commitment
// has been broadcast.
func closingTxIDIn(stdout string) (string, error) {
	var resp struct {
		ClosingTxID string `json:"closing_txid"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &resp); err != nil {
		return "", err
	}
	if resp.ClosingTxID == "" {
		return "", fmt.Errorf("no closing_txid in it")
	}
	return resp.ClosingTxID, nil
}
