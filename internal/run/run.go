// Package run is the composition: Phase 0, the armed window, and Phase 2, in
// one place, with the gates wired.
//
// Every step in here belongs to another package. What was missing was the thing
// that calls them in order and holds the gates open or shut — until now the
// only place the whole sequence existed was drive() in internal/arm's regtest
// test, which is a fixture and cannot be shipped.
//
// # Where the gates are
//
// Five of them, and they are not decorations:
//
//   - setup.Check, first, before LND is asked anything. The cold wallet's
//     descriptors are the one thing no check can validate — Core parses a wrong
//     pair, the import succeeds, the read-back is self-consistent and the
//     balance is a plausible partial one — so the only verdict that exists is a
//     comparison a human made, and this is where it is read back. It refuses on
//     an exact descriptor-pair match against a recorded rejection and on nothing
//     else; every other state is a line on the screen. First, because a refusal
//     here has cost nothing at all.
//   - rehearsal.Gate, before arm.Open. A signing round slower than
//     limits.abort_after_signing_seconds means the batch is not armed at all.
//     The clock the gate protects has not started yet when it is checked, which
//     is the whole point.
//   - peers.ReadyToArm, before arm.Open. An accepted shim probe costs one of
//     that peer's pending-channel slots for about eleven minutes, and
//     shim_cancel does not give it back, so a run that probed and then armed
//     immediately would collide with itself.
//   - reserve.Finding.StillApplies, once the streams are open. The pre-flight
//     was made against the planned channel list; the streams are what actually
//     opened, and a finding is about a particular count of announced channels.
//   - the journal, at the publish call. arm.Publish will not accept anything
//     but an *arm.Armed, and journal.MarkPublishing refuses a run the journal
//     does not call armed. Neither is this package's to grant.
//
// # --stop-before-publish is not a second code path
//
// The mainnet cold probe runs the production flow and stops before step 9. I-1
// says the gate must be unreachable, so the probe cannot be a separate,
// gentler route through the same steps: it is this route, with the last call
// withheld. Concretely, everything up to and including arm.Finalize is
// unconditional, and the flag is read once, after the batch is armed and the
// backups are in hand, at the only if-statement in the sequence:
//
//	armed, err := arm.Finalize(...)     // steps 7 and 8, always
//	if o.StopBeforePublish { ... }      // the call not made
//	err = arm.Publish(...)              // step 9
//
// What differs afterwards is not a code path but a fact: nothing was published,
// so the run terminates through the abort path, which is what the probe is for.
//
// # Failure means abort, here
//
// Any failure between arm.Open and the publish leaves peers holding
// reservations and possibly channels holding commitment signatures. Nothing has
// been broadcast, so the answer is always the same one — cancel the shims,
// abandon what reached pending, release Core's locks — and this package takes
// it rather than printing a suggestion. It goes through journal.Recover, which
// is the same code winthistle recover runs, and which refuses outright to
// abort a run that reached the publish call.
package run

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/AusDavo/winthistle/internal/abort"
	"github.com/AusDavo/winthistle/internal/arm"
	"github.com/AusDavo/winthistle/internal/bitcoind"
	"github.com/AusDavo/winthistle/internal/coldwallet"
	"github.com/AusDavo/winthistle/internal/combine"
	"github.com/AusDavo/winthistle/internal/config"
	"github.com/AusDavo/winthistle/internal/fees"
	"github.com/AusDavo/winthistle/internal/journal"
	"github.com/AusDavo/winthistle/internal/lnd"
	"github.com/AusDavo/winthistle/internal/peers"
	"github.com/AusDavo/winthistle/internal/plan"
	"github.com/AusDavo/winthistle/internal/prose"
	"github.com/AusDavo/winthistle/internal/rehearsal"
	"github.com/AusDavo/winthistle/internal/reserve"
	"github.com/AusDavo/winthistle/internal/settle"
	"github.com/AusDavo/winthistle/internal/setup"
	"github.com/lightningnetwork/lnd/lnrpc"
)

// Deps are the connections and the journal, already open.
//
// They are passed in rather than dialled here because the thing that dials is
// the thing that reports a dial failure — winthistle doctor — and because a
// test that drives this against the harness should not have to write a
// configuration file to say where the harness is.
type Deps struct {
	LND *lnd.Client

	// Node is Core with no wallet scope, for the fee estimate and
	// testmempoolaccept. Wallet is the watch-only cold wallet.
	Node   *bitcoind.Client
	Wallet *bitcoind.Client

	Journal *journal.Journal

	// Signers hands out the devices for one round. An interface rather than
	// *signers.Set because the transport is not this package's business — it is
	// the same seam rehearsal.Signer draws, and it is what lets the regtest
	// tests drive the whole composition with the simulated cold wallet's two
	// halves rather than with a subprocess.
	Signers Signers

	// Out is where the operator-facing reports go.
	Out io.Writer

	// Confirm authorises LND's i_know_what_i_am_doing flag during an abort.
	// Nil means never escalate, which is the right default for anything
	// non-interactive: abort.AbandonPending re-checks that the channel is
	// pending itself and refuses outright when it is not, so a nil confirmation
	// costs an abort that has to be finished by hand rather than one that
	// removes something it should not have.
	Confirm abort.Confirmation
}

// Signers is where the batch's partial signatures come from.
//
// Round takes a name because the dress rehearsal and the real batch are two
// rounds minutes apart, and a transport that writes files has to keep them
// apart: a signature over the decoy, picked up as the batch's, is refused by
// internal/combine as a moved txid — at the worst moment, blaming the device.
type Signers interface {
	Round(name string) []rehearsal.Device
	Labels() []string
}

// Options are the run.
type Options struct {
	Config *config.Config
	Batch  *config.Batch

	// RunID is the journal's key for this run.
	RunID string

	// StopBeforePublish makes this the cold probe: the whole production path,
	// with step 9 withheld. See the package comment.
	StopBeforePublish bool

	// Probe runs a shim probe against every peer before arming. Off by default,
	// and it should stay off for an ordinary run: a successful probe is step 2
	// with the answer thrown away, and it holds one of the peer's
	// pending-channel slots for about eleven minutes afterwards.
	Probe bool

	// SettleFor bounds Phase 2. Zero means DefaultSettleFor.
	SettleFor time.Duration
}

// DefaultSettleFor is how long Phase 2 watches before handing back.
//
// Long enough for a few blocks and short enough that a terminal is not held
// open all night. Nothing is lost by stopping early: the channels are already
// recoverable, the transaction is already public, and the policy pass can be
// resumed by running the settlement again.
const DefaultSettleFor = 30 * time.Minute

// TeardownBudget is how long the abort path gets after the run has stopped.
//
// It is one signing gate's worth, and that is the unit deliberately: the slow
// part of a teardown is not the RPCs — n shim cancels, n abandons and one batch
// of coin-lock releases, all against a node on the same machine — it is the
// blunt-abandon confirmation, which asks a human once per channel. Five minutes
// is what this product already calls "as long as an operator at the machine gets
// to do a thing".
//
// Running out of it costs nothing that cannot be picked up: `winthistle recover`
// is safe to run as many times as it takes and everything under it is
// idempotent. The seams clamp their own deadlines inside this one, so a
// confirmation nobody answers declines rather than erroring — see
// webrun.deadlineFor.
const TeardownBudget = 5 * time.Minute

// Result is what the run did, however far it got.
type Result struct {
	RunID string

	// Armed is the receipt that every channel is recoverable. Non-nil from the
	// moment arm.Finalize returns, including on a run that then stopped.
	Armed *arm.Armed

	Published  bool
	Settlement *settle.Result

	// Aborted is what the teardown removed, when there was one.
	Aborted *abort.Report
}

// Do runs the batch.
func Do(ctx context.Context, d Deps, o Options) (*Result, error) {
	if o.RunID == "" {
		return nil, errors.New("a run needs an id: it is the journal's key, and " +
			"the journal is what makes a crash recoverable rather than mysterious")
	}
	if d.Signers == nil {
		return nil, errors.New("no signers: there is nothing to sign the batch " +
			"with. Add a [[signer]] block per cold-storage device, and exactly as " +
			"many as the descriptor requires — btcd's finalizer wants exactly m " +
			"signatures, so a 2-of-3 carrying three partials does not finalize")
	}
	res := &Result{RunID: o.RunID}

	p, err := prepare(ctx, d, o)
	if p != nil && len(p.fenced) > 0 {
		// The fence is this package's to release: it is not in the journal,
		// because it is not a lock taken for the batch — it is the coins the
		// batch may not touch, held so Core's own coin selection cannot reach
		// them.
		defer releaseFence(ctx, d, p.fenced)
	}
	if err != nil {
		return res, err
	}

	armed, err := armWindow(ctx, d, o, p, res)
	if err != nil {
		// Nothing has been broadcast — that is what the armed window is defined
		// by — so the answer is always the same, and it is taken rather than
		// suggested.
		fmt.Fprintf(d.Out, "\nThe armed window failed: %v\n\n", err)
		// The teardown's own failure is reported on the screen above rather
		// than returned: the error worth exiting with is the one that stopped
		// the run, and the recovery screen already says what is left and that
		// running it again is safe.
		_ = recoverRun(ctx, d, o, res)
		return res, err
	}
	res.Armed = armed
	reportArmed(d.Out, armed, p)

	if o.StopBeforePublish {
		fmt.Fprint(d.Out, withheld(armed))
		if err := recoverRun(ctx, d, o, res); err != nil {
			// The probe proved the sequence and then failed to clean up after
			// itself, which is not a success: two peers are holding channels
			// that will sit pending for 2016 blocks. Say so in the exit status,
			// because a probe is usually run from a terminal somebody walks away
			// from.
			return res, fmt.Errorf("the batch was armed and step 9 withheld, as "+
				"asked — but taking it apart afterwards did not finish: %w", err)
		}
		return res, nil
	}

	if err := arm.Publish(ctx, d.LND.WalletKit, d.Journal, armed); err != nil {
		// Deliberately no abort here, and no attempt to decide whether the
		// transaction went out. journal.MarkPublishing lands before the RPC, so
		// this run is now in a state Run.AbortTarget refuses — abandoning a
		// pending channel whose funding transaction then confirms strands its
		// funds with no force-close path.
		fmt.Fprint(d.Out, mayBePublic(armed, err))
		return res, err
	}
	res.Published = true
	fmt.Fprintf(d.Out, "\nPublished %s\n\n", armed.TxID)

	res.Settlement = settlePhase(ctx, d, o, armed, p)
	return res, nil
}

// prepared is everything Phase 0 decided. Nothing in it started a clock.
type prepared struct {
	chain   string
	chans   []arm.Channel
	rate    fees.Rate
	coins   coldwallet.Coins
	fenced  []bitcoind.Outpoint
	change  string
	finding reserve.Finding
	topUp   *plan.TopUp
	probes  []peers.Probe
}

func prepare(ctx context.Context, d Deps, o Options) (*prepared, error) {
	p := &prepared{chans: o.Batch.ArmChannels()}

	// 0. The cold wallet, before anything else. This is the only pre-flight that
	// needs neither LND nor the network, and the only one whose evidence is a
	// person: setup.Check reads the descriptor pair the wallet actually holds and
	// refuses if a human has already compared those exact descriptors against
	// their own wallet software and said they are not the cold wallet's. It fires
	// on an exact pair match and nothing else, so it cannot stop a wallet that
	// was fixed, re-imported, or never seen by this build — those are a line on
	// the screen. It runs first because a refusal here has cost nothing: no
	// stream, no reservation, no coin lock, no peer told anything.
	section(d.Out, "Phase 0 — the cold wallet's setup")
	standing, err := setup.Check(ctx, d.Wallet, d.Journal, o.Config.Bitcoind.Wallet)
	fmt.Fprint(d.Out, standing.Note())
	if err != nil {
		return p, err
	}

	info, err := d.LND.Lightning.GetInfo(ctx, &lnrpc.GetInfoRequest{})
	if err != nil {
		return p, fmt.Errorf("asking LND what it is: %w", err)
	}
	if !info.GetSyncedToChain() {
		return p, errors.New("the node is not synced to chain, and LND refuses to " +
			"open a channel while its wallet is behind — \"channels cannot be " +
			"created before the wallet is fully synced\"")
	}
	for _, ch := range info.GetChains() {
		p.chain = ch.GetNetwork()
	}
	if _, err := plan.Params(p.chain); err != nil {
		return p, fmt.Errorf("LND says it is on %q: %w", p.chain, err)
	}

	// 1. The peers, free tier. No stream, no clock.
	section(d.Out, "Phase 0 — the peers")
	facts, err := peers.Check(ctx, d.LND.Lightning, o.Batch.Wants())
	if err != nil {
		return p, fmt.Errorf("the peer pre-flight: %w", err)
	}
	for _, f := range facts {
		fmt.Fprint(d.Out, f.Report())
		if !f.Usable() {
			return p, fmt.Errorf("peer %s: %s", f.Want.Pubkey, unusable(f))
		}
	}

	// 2. The probe, only when asked, and then the gate it creates.
	if o.Probe {
		section(d.Out, "Phase 0 — the shim probe")
		fmt.Fprint(d.Out, prose.Para("A probe that the peer accepts is step 2 with "+
			"the answer thrown away: same RPC, same reservation, same ten minutes. "+
			"shim_cancel is local — it deletes our own map entry and sends the peer "+
			"nothing — so the peer holds that reservation until its own sweeper "+
			"releases it, and arming inside that window can be refused for a reason "+
			"we created ourselves."))
		p.probes, err = peers.DoAll(ctx, d.LND.Lightning, p.chain, o.Batch.Wants())
		if err != nil {
			return p, fmt.Errorf("probing: %w", err)
		}
		for _, pr := range p.probes {
			fmt.Fprint(d.Out, pr.Report(time.Now()))
		}
		if err := waitForPeers(ctx, d, p.probes); err != nil {
			return p, err
		}
	}

	// 3. The fee rate, from Core and nowhere else.
	section(d.Out, "Phase 0 — the fee rate")
	p.rate, err = fees.Estimate(ctx, d.Node, fees.Request{
		TargetBlocks:  o.Config.Fees.TargetBlocks,
		Mode:          o.Config.Fees.Mode,
		FloorSatPerVB: o.Config.Fees.FloorSatPerVB,
	})
	if err != nil {
		return p, fmt.Errorf("the fee rate: %w", err)
	}
	fmt.Fprint(d.Out, p.rate.Report())

	// 4. The coins, and the fence around the ones the batch may not spend.
	section(d.Out, "Phase 0 — the cold wallet's coins")
	minConf := o.Config.Limits.MinConfirmations()
	p.coins, err = coldwallet.SelectCoins(ctx, d.Wallet, minConf)
	if err != nil {
		return p, fmt.Errorf("selecting the cold wallet's coins: %w", err)
	}
	fmt.Fprint(d.Out, p.coins.Report())
	p.fenced, err = coldwallet.FenceOff(ctx, d.Wallet, p.coins)
	if err != nil {
		return p, fmt.Errorf("fencing off the coins the batch may not spend: %w", err)
	}

	p.change, err = coldwallet.ChangeAddress(ctx, d.Wallet)
	if err != nil {
		return p, fmt.Errorf("asking the cold wallet for a change address: %w", err)
	}

	// 5. The anchor reserve, which is about this node's own hot wallet and is
	//    the one thing that can refuse step 5 for a reason unrelated to the
	//    batch. A shortfall is not fatal here: the plan pays it as an output.
	section(d.Out, "Phase 0 — the anchor reserve")
	p.finding, err = reserve.Check(ctx, d.LND.WalletKit, arm.BatchOf(p.chans))
	if err != nil {
		return p, fmt.Errorf("the anchor-reserve pre-flight: %w", err)
	}
	fmt.Fprint(d.Out, p.finding.Report())

	p.topUp, err = plan.ReserveTopUp(ctx, d.LND.Lightning, p.finding)
	if err != nil {
		return p, fmt.Errorf("building the reserve top-up: %w", err)
	}

	// 6. The dress rehearsal, and the gate.
	section(d.Out, "Phase 0 — the dress rehearsal")
	mirror := o.Batch.AmountsSat()
	if p.topUp != nil {
		mirror = append(mirror, p.topUp.AmountSat)
	}
	m, err := rehearsal.Run(ctx, rehearsal.Request{
		Wallet:           d.Wallet,
		Node:             d.Node,
		MirrorSat:        mirror,
		FeeRateSatPerVB:  p.rate.SatPerVB,
		MinConfirmations: minConf,
		Devices:          d.Signers.Round("rehearsal"),
		Limit:            o.Config.Limits.AbortAfterSigning,
	})
	if m != nil {
		fmt.Fprint(d.Out, m.Report())
	}
	if err != nil {
		return p, fmt.Errorf("the dress rehearsal: %w", err)
	}
	if err := rehearsal.Gate(m); err != nil {
		return p, fmt.Errorf("the batch will not be armed: %w", err)
	}
	return p, nil
}

// waitForPeers holds until every probed peer's reservation can be assumed gone.
func waitForPeers(ctx context.Context, d Deps, probes []peers.Probe) error {
	for {
		err := peers.ReadyToArm(probes, time.Now())
		if err == nil {
			return nil
		}
		if !errors.Is(err, peers.ErrPeersStillHolding) {
			return err
		}
		var longest time.Duration
		for _, pr := range probes {
			if r := pr.Remaining(time.Now()); r > longest {
				longest = r
			}
		}
		if longest <= 0 {
			return err
		}
		fmt.Fprintf(d.Out, "waiting %s for the probed peers to release their "+
			"reservations\n", longest.Round(time.Second))
		wait := longest
		if wait > 30*time.Second {
			wait = 30 * time.Second
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

// armWindow is steps 2 to 8. From arm.Open onwards there are n peers holding
// reservations and n clocks running.
func armWindow(ctx context.Context, d Deps, o Options, p *prepared, res *Result) (
	*arm.Armed, error) {

	section(d.Out, "Phase 1 — the armed window")

	streams, err := arm.Open(ctx, d.LND.Lightning, p.chain, p.chans)
	var opened *arm.OpenError
	if errors.As(err, &opened) {
		streams = opened.Streams
	}
	if streams != nil {
		defer streams.Close()
		// The pending channel ids are the only handles that can cancel these
		// streams, so they go on disk before anything else happens — including
		// before this function decides whether Open failed.
		if jerr := d.Journal.Begin(ctx, o.RunID, streams.NewChannels()); jerr != nil {
			return nil, fmt.Errorf("journalling the run: %w", jerr)
		}
	}
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(d.Out, "%d funding stream%s open, and the peers' ten minutes "+
		"start now.\n", len(streams.All), prose.Plural(len(streams.All)))

	// The Phase 0 finding was about the batch the operator approved. These are
	// the streams that actually opened.
	if err := p.finding.StillApplies(streams.Batch()); err != nil {
		return nil, err
	}

	batchPlan, err := streams.Plan(arm.Blueprint{
		Chain:  p.chain,
		Fee:    p.rate.Fee(),
		TopUp:  p.topUp,
		Change: plan.Change{Address: p.change},
		Inputs: plan.Inputs{
			MinConfirmations: o.Config.Limits.MinConfirmations(),
			Allowed:          allowed(p.coins),
			Excluded:         excluded(p.coins),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("assembling the plan: %w", err)
	}
	section(d.Out, "The batch plan")
	fmt.Fprint(d.Out, batchPlan.Document())

	outputs := streams.FundingOutputs()
	if p.topUp != nil {
		outputs = append(outputs, coldwallet.Output{
			Address: p.topUp.Address, AmountSat: p.topUp.AmountSat,
		})
	}
	built, err := coldwallet.Build(ctx, d.Wallet, coldwallet.BuildRequest{
		Outputs:          outputs,
		ChangeAddress:    p.change,
		FeeRateSatPerVB:  p.rate.SatPerVB,
		MinConfirmations: o.Config.Limits.MinConfirmations(),
	})
	if err != nil {
		return nil, fmt.Errorf("building the batch transaction: %w", err)
	}
	if err := d.Journal.RecordLocks(ctx, o.RunID, built.Inputs); err != nil {
		return nil, fmt.Errorf("journalling Core's coin locks: %w", err)
	}

	// Ours first. LND's psbt_verify finds its own funding output and stops, so
	// an output nobody named passes all n of its checks.
	v, err := batchPlan.Verify(built.Raw)
	if err != nil {
		return nil, fmt.Errorf("verifying the transaction Core built: %w", err)
	}
	fmt.Fprint(d.Out, v.Report())
	if !v.OK() {
		return nil, errors.New("the transaction does not match the plan")
	}

	// Then LND's, for every stream, before anything is signed or finalized.
	if err := arm.Verify(ctx, d.LND.Lightning, d.Journal, o.RunID, streams, built.Raw); err != nil {
		return nil, err
	}
	fmt.Fprintf(d.Out, "\npsbt_verify: all %d channels. LND has committed to the "+
		"funding outpoints, and from here only signatures may be added.\n",
		len(streams.All))

	parts, err := sign(ctx, d, o, built)
	if err != nil {
		return nil, err
	}

	final, recheck, err := combine.Complete(batchPlan, built.Raw, parts)
	if err != nil {
		return nil, fmt.Errorf("combining and finalizing in-app: %w", err)
	}
	if !recheck.OK() {
		fmt.Fprint(d.Out, recheck.Report())
		return nil, errors.New("the finalized transaction does not match the plan")
	}
	if final.TxID != built.TxID {
		return nil, fmt.Errorf("I-3: the txid moved from %s to %s while it was "+
			"being signed", built.TxID, final.TxID)
	}

	// The only pre-flight there is, and specifically not a broadcast.
	ok, why, _, err := d.Node.TestMempoolAccept(ctx, hex.EncodeToString(final.RawTx))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("testmempoolaccept would refuse this transaction: %s", why)
	}
	fmt.Fprintf(d.Out, "testmempoolaccept: allowed. %s in fees, %d vB, %.2f sat/vB.\n",
		prose.Sats(final.FeeSat), final.Vsize,
		float64(final.FeeSat)/float64(final.Vsize))

	return arm.Finalize(ctx, d.LND.Lightning, d.Journal, o.RunID, streams, final)
}

// sign is the signing round: the one part of the armed window whose duration
// belongs to the operator and the devices, and the one the rehearsal measured.
func sign(ctx context.Context, d Deps, o Options, built coldwallet.Built) ([]combine.Part, error) {
	section(d.Out, "The signing round")
	devices := d.Signers.Round("batch")

	for _, dev := range devices {
		if err := d.Journal.RecordSigner(ctx, o.RunID, dev.Label,
			journal.SignerAwaiting); err != nil {
			return nil, fmt.Errorf("journalling signer %s: %w", dev.Label, err)
		}
	}

	started := time.Now()
	parts := make([]combine.Part, 0, len(devices))
	for _, dev := range devices {
		part, err := dev.Sign(ctx, built.PSBT)
		if err != nil {
			if jerr := d.Journal.RecordSigner(ctx, o.RunID, dev.Label,
				journal.SignerDeclined); jerr != nil {
				return nil, fmt.Errorf("%s did not sign (%v), and journalling that "+
					"failed too: %w", dev.Label, err, jerr)
			}
			return nil, fmt.Errorf("%s did not sign: %w", dev.Label, err)
		}
		if err := d.Journal.RecordSigner(ctx, o.RunID, dev.Label,
			journal.SignerPartial); err != nil {
			return nil, fmt.Errorf("journalling signer %s: %w", dev.Label, err)
		}
		parts = append(parts, part)
		fmt.Fprintf(d.Out, "%s signed (%s elapsed)\n", dev.Label,
			time.Since(started).Round(time.Second))
	}

	// Said rather than enforced. The gate that stops a slow round is
	// rehearsal.Gate, before anything is armed; by here the peers' clocks are
	// running and stopping would cost the same as continuing.
	elapsed := time.Since(started)
	if elapsed > o.Config.Limits.AbortAfterSigning {
		fmt.Fprint(d.Out, prose.Para(fmt.Sprintf("That round took %s, past the %s "+
			"gate the rehearsal was measured against. Nothing is published and "+
			"nothing is at risk, but the peers' windows may lapse before the batch "+
			"is armed — if they do, the cost is one more signing round.",
			elapsed.Round(time.Second), o.Config.Limits.AbortAfterSigning)))
	}
	return parts, nil
}

// settlePhase is Phase 2: from the single publish to active-and-policied.
func settlePhase(ctx context.Context, d Deps, o Options, armed *arm.Armed,
	p *prepared) *settle.Result {

	section(d.Out, "Phase 2 — settlement")

	window := o.SettleFor
	if window <= 0 {
		window = DefaultSettleFor
	}
	ctx, cancel := context.WithTimeout(ctx, window)
	defer cancel()

	result, err := settle.Settle(ctx, d.LND.Lightning, members(armed, p, o), settle.Options{
		Chain:       d.Node,
		FundingTxID: armed.TxID,
	})
	if result != nil {
		fmt.Fprint(d.Out, result.Report())
	}
	if err != nil {
		fmt.Fprintf(d.Out, "\nThe settlement stopped: %v\n", err)
	}
	return result
}

// members pairs each published channel with the policy chosen for it.
//
// By index, and that is sound rather than convenient: arm.Finalize appends one
// channel point per stream in Streams.All order, and Streams.All is in the
// order arm.Open was given the channels, which is the batch file's order.
func members(armed *arm.Armed, p *prepared, o Options) []settle.Member {
	out := make([]settle.Member, 0, len(armed.Channels))
	for i, cp := range armed.Channels {
		if i >= len(p.chans) {
			break
		}
		ch := p.chans[i]
		m := settle.Member{
			Channel: cp, Peer: ch.Peer, AmountSat: ch.AmountSat, Private: ch.Private,
		}
		if ch.Policy != nil {
			m.Policy = *ch.Policy
		}
		out = append(out, m)
	}
	return out
}

// recoverRun tears down whatever the run left behind, through the journal.
//
// # The teardown outlives the cancellation that caused it
//
// The commonest reason to be here is that the run's context was cancelled —
// Ctrl-C in the terminal, or the web UI's abort control, which is the same
// cancellation from the other front door. On a cancelled context every call
// below fails at once: the journal read is a database/sql query, the shim
// cancels and the abandons are gRPC, and Core's lock release is JSON-RPC. So an
// abort triggered by Ctrl-C would have reported "context canceled" and taken
// nothing apart, which is the exact opposite of what the deferred teardown is
// for.
//
// A fresh context, then, bounded rather than unbounded: releaseFence already
// does this for the same reason, and the bound is here because a teardown that
// hangs holds a terminal the operator has already tried to get out of. What is
// deliberately *not* inherited is cancellation; the deadline is ours.
func recoverRun(ctx context.Context, d Deps, o Options, res *Result) error {
	ctx, done := context.WithTimeout(context.WithoutCancel(ctx), TeardownBudget)
	defer done()

	run, err := d.Journal.Load(ctx, o.RunID)
	if errors.Is(err, journal.ErrNoRun) {
		// Nothing was journalled, which means arm.Open never returned a stream.
		// There is nothing in LND to take down.
		return nil
	}
	if err != nil {
		fmt.Fprintf(d.Out, "could not read run %s back out of the journal: %v\n",
			o.RunID, err)
		return err
	}

	section(d.Out, "Taking the batch apart")
	fmt.Fprint(d.Out, prose.Recovery(run, time.Now()))

	rep, err := d.Journal.Recover(ctx, d.LND.Lightning, d.Wallet, o.RunID, d.Confirm)
	res.Aborted = rep
	fmt.Fprint(d.Out, prose.RecoveryOutcome(run, rep, err))
	return err
}

func releaseFence(ctx context.Context, d Deps, fenced []bitcoind.Outpoint) {
	// A fresh context: the fence has to come off even when the run was
	// cancelled, and a cancelled context cannot make the RPC.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()

	freed, err := d.Wallet.ReleaseLocks(ctx, fenced)
	if err != nil {
		fmt.Fprintf(d.Out, "\ncould not release the %d coin lock%s fencing off the "+
			"coins this batch was not allowed to spend: %v\nThey are memory-only, "+
			"so restarting Core clears them; `winthistle doctor` lists them.\n",
			len(fenced), prose.Plural(len(fenced)), err)
		return
	}
	if len(freed) > 0 {
		fmt.Fprintf(d.Out, "released %d fenced coin lock%s\n", len(freed),
			prose.Plural(len(freed)))
	}
}

func allowed(c coldwallet.Coins) []plan.Outpoint {
	out := make([]plan.Outpoint, 0, len(c.Eligible))
	for _, u := range c.Eligible {
		out = append(out, plan.Outpoint{TxID: u.TxID, Vout: u.Vout})
	}
	return out
}

func excluded(c coldwallet.Coins) []string {
	out := make([]string, 0, len(c.Excluded))
	for _, e := range c.Excluded {
		out = append(out, fmt.Sprintf("%s — %s: %s", e.Coin.Outpoint(), e.Why, e.Detail))
	}
	return out
}

func unusable(f peers.Facts) string {
	if !f.KeyOK {
		return f.KeyProblem
	}
	return fmt.Sprintf("%s (%s). A peer LND is not connected to cannot be asked "+
		"for a channel", f.Connection, f.ConnectDetail)
}

func section(w io.Writer, title string) {
	fmt.Fprintf(w, "\n%s\n%s\n\n", title, underline(len(title)))
}

func underline(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = '-'
	}
	return string(b)
}
