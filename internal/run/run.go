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
// Four of them, and they are not decorations:
//
//   - plan.Verify, before LND is shown anything. This is the one the tool exists
//     for. psbt_verify finds its own funding output and stops — it never asserts
//     that its output is the only one, which is what lets n channels share a
//     transaction — so an output nobody named passes all n of LND's checks. Ours
//     runs first, which is why a mis-paste at step 4 costs a redo inside clock A
//     rather than a channel.
//   - peers.ReadyToArm, before arm.Open. An accepted shim probe costs one of
//     that peer's pending-channel slots for about eleven minutes, and
//     shim_cancel does not give it back, so a run that probed and then armed
//     immediately would collide with itself.
//   - reserve.Finding.StillApplies, once the streams are open. The pre-flight
//     was made against the planned channel list; the streams are what actually
//     opened, and a finding is about a particular count of announced channels.
//   - the journal, at the publish call. arm.Publish will not accept anything
//     but an *arm.Armed, and journal.MarkPublishing refuses a run whose channels
//     are not all at chan_pending — it counts them itself. Neither is this
//     package's to grant.
//
// # Two gates this used to have, and why neither survived the inversion
//
// This comment listed five, and two of them were about machinery the app has
// given up rather than about the batch:
//
//   - a check on the cold wallet's descriptor pair, which read back a human's
//     verdict that its addresses matched the wallet software holding the keys.
//     The app no longer selects coins, derives addresses or asks Core for change,
//     so it never touches those descriptors — and a refusal about a wallet
//     nothing in the run reads is a refusal with no subject.
//   - an abort gate, which refused to arm a batch whose signing round a dress
//     rehearsal had measured as slower than limits.abort_after_signing_seconds.
//     It measured a window that no longer contains signing: the gate opens at
//     step 6, before anything is signed, so a slow round costs time against
//     clock B and cannot cost the batch. It also needed Core to build a mirror
//     transaction and m devices to sign it, and this package has neither now.
//
// Both are deleted, along with the packages that held them and the config key
// the second one read.
//
// # --stop-before-publish is not a second code path
//
// The mainnet cold probe runs the production flow and stops before step 8. I-1
// says the gate must be unreachable, so the probe cannot be a separate,
// gentler route through the same steps: it is this route, with the last call
// withheld. Concretely, everything up to and including the signing round is
// unconditional, and the flag is read once, after the batch is armed and signed,
// at the only if-statement in the sequence:
//
//	armed, final, err := armWindow(...)  // steps 2 to 7, always
//	if o.StopBeforePublish { ... }       // the call not made
//	err = arm.Publish(...)               // step 8
//
// What differs afterwards is not a code path but a fact: nothing was published,
// so the run terminates through the abort path, which is what the probe is for.
//
// # Failure means abort, here
//
// Any failure between arm.Open and the publish leaves peers holding
// reservations and possibly channels holding commitment signatures. Nothing has
// been broadcast, so the answer is always the same one — abandon what reached
// pending, cancel the shims — and this package takes it rather than printing a
// suggestion. It goes through journal.Recover, which
// is the same code winthistle recover runs, and which refuses outright to
// abort a run that reached the publish call.
package run

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/AusDavo/winthistle/internal/abort"
	"github.com/AusDavo/winthistle/internal/arm"
	"github.com/AusDavo/winthistle/internal/combine"
	"github.com/AusDavo/winthistle/internal/config"
	"github.com/AusDavo/winthistle/internal/journal"
	"github.com/AusDavo/winthistle/internal/lnd"
	"github.com/AusDavo/winthistle/internal/peers"
	"github.com/AusDavo/winthistle/internal/plan"
	"github.com/AusDavo/winthistle/internal/prose"
	"github.com/AusDavo/winthistle/internal/reserve"
	"github.com/AusDavo/winthistle/internal/settle"
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

	Journal *journal.Journal

	// Signing is the wallet at steps 4 and 7: the thing that builds the
	// transaction and then signs it. Required.
	Signing SigningWallet

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

	// FeeRateSatPerVB overrides [fees] target_sat_per_vb for this run. Zero means
	// use the configured one. Neither is an estimate: see feeFor.
	FeeRateSatPerVB float64

	// Change names the wallet's change address, when the operator knows it and
	// wants the stronger check. Empty is the ordinary case: the app does not build
	// the transaction, so it does not know where the change goes, and
	// plan.RecogniseChangeIn reads it out of the packet's own key origins instead.
	// Naming it here is strictly stronger — a script rather than a claim — and it
	// is the answer for a wallet that writes no key origins at all.
	Change string
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
// confirmation nobody answers declines rather than erroring.
const TeardownBudget = 5 * time.Minute

// Result is what the run did, however far it got.
type Result struct {
	RunID string

	// Armed is the receipt that every channel is recoverable. Non-nil from the
	// moment arm.Receipts returns, including on a run that then stopped — which
	// is now before anything was signed rather than after.
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
	if d.Signing == nil {
		return nil, errors.New("no signing wallet: there is nothing to build the " +
			"batch transaction or sign it. On the command line that is --psbt FILE, " +
			"which is where the transaction you build in Sparrow gets saved and where " +
			"this run reads it back from")
	}
	res := &Result{RunID: o.RunID}

	p, err := prepare(ctx, d, o)
	if err != nil {
		return res, err
	}

	armed, final, err := armWindow(ctx, d, o, p, res)
	if err != nil {
		// Nothing has been broadcast — that is what the armed window is defined
		// by — so the answer is always the same, and it is taken rather than
		// suggested.
		// Wrapped like the rest of the copy. It was a bare Fprintf until a
		// browser rendered it: LND's verbatim errors run to 250 characters, so
		// the one paragraph an operator reads at the worst moment was the one
		// paragraph that did not match the pane everything around it is written
		// to.
		fmt.Fprintf(d.Out, "\n%s\n", prose.Para(fmt.Sprintf(
			"The armed window failed: %v", err)))
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

	if err := arm.Publish(ctx, d.LND.WalletKit, d.Journal, armed, final.RawTx); err != nil {
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
	fee     plan.Fee
	finding reserve.Finding
	topUp   *plan.TopUp
	probes  []peers.Probe

	// facts is Phase 0's peer pre-flight, kept because the batch plan renders
	// each peer's alias beside its key and the graph was already asked once.
	facts []peers.Facts
}

func prepare(ctx context.Context, d Deps, o Options) (*prepared, error) {
	p := &prepared{chans: o.Batch.ArmChannels()}

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
	p.facts = facts
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

	// 3. The fee rate, declared rather than fetched. The app does not choose the
	//    fee — Sparrow does, at step 4 — and it no longer asks anything what the
	//    fee should be either. What the verifier needs is something to compare the
	//    built transaction against, and that is what the operator said they were
	//    aiming at.
	section(d.Out, "Phase 0 — the fee rate")
	p.fee, err = feeFor(o)
	if err != nil {
		return p, err
	}
	fmt.Fprint(d.Out, feeReport(p.fee, o))

	// 4. The anchor reserve, which is about this node's own hot wallet and is
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

// armWindow is steps 2 to 7. From arm.Open onwards there are n peers holding
// reservations and n clocks running.
//
// It returns the armed batch *and* the signed transaction, because after the
// inversion those are two separate things produced at two separate times: the
// batch is armed in step 6 over an unsigned transaction, and the bytes to
// broadcast arrive in step 7 from something that is not this program.
//
// # Where clock A actually goes now
//
// Steps 2 to 6 are the only part under the peers' ten minutes, and the only step
// inside them that takes any time at all is step 4 — the operator in Sparrow,
// pasting n addresses in and choosing coins. Everything else is local: the
// verifier is arithmetic, psbt_verify is one RPC per stream, and the receipts
// arrived in 0.55 s at n = 2 on the harness. So a batch that blows clock A blows
// it in a wallet's Send tab, before LND has been shown anything, and the cost is
// n shim cancels.
func armWindow(ctx context.Context, d Deps, o Options, p *prepared, res *Result) (
	*arm.Armed, *combine.Finalized, error) {

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
			return nil, nil, fmt.Errorf("journalling the run: %w", jerr)
		}
	}
	if err != nil {
		return nil, nil, err
	}
	fmt.Fprintf(d.Out, "%d funding stream%s open, and the peers' ten minutes "+
		"start now.\n", len(streams.All), prose.Plural(len(streams.All)))

	// The Phase 0 finding was about the batch the operator approved. These are
	// the streams that actually opened.
	if err := p.finding.StillApplies(streams.Batch()); err != nil {
		return nil, nil, err
	}

	// Step 3 and step 4: the recipients, and then the operator in their wallet.
	// The addresses are printed in full because they are copy-pasted from the
	// terminal, never typed, and a mis-paste is what plan.Verify is for.
	section(d.Out, "Step 4 — build the transaction in your wallet")
	pay := recipientsOf(streams, p)
	fmt.Fprint(d.Out, recipientTable(pay))
	fmt.Fprint(d.Out, prose.Para("Enter these as recipients, choose your coins and "+
		"the fee, and save the PSBT. Do not sign it yet: nothing is recoverable "+
		"until step 6, so a transaction signed and broadcast before then would "+
		"confirm one 2-of-2 output per channel with no channel behind any of them."))

	unsigned, err := d.Signing.Built(ctx, pay)
	if err != nil {
		return nil, nil, err
	}

	// The plan is assembled now rather than at step 3, because until the packet
	// exists there is nothing to say about the change output. The app does not
	// build the transaction, so it does not choose where the change goes; what it
	// can do is recognise the wallet's own key origins on it, or be told the
	// address. See plan.RecogniseChangeIn for how weak that claim is and why it is
	// the right shape anyway.
	change, err := changeIn(o, unsigned)
	if err != nil {
		return nil, nil, err
	}
	batchPlan, err := streams.Plan(arm.Blueprint{
		Chain:   p.chain,
		Fee:     p.fee,
		TopUp:   p.topUp,
		Aliases: aliases(p.facts),
		Change:  change,
		// No Allowed and no Excluded: Sparrow picks the coins, so there is no set
		// this app could hold the transaction to. MinConfirmations is stated rather
		// than enforced and the verifier reports it as unchecked, which is the
		// honest version of a constraint nobody can check from a PSBT.
		Inputs: plan.Inputs{MinConfirmations: o.Config.Limits.MinConfirmations()},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("assembling the plan: %w", err)
	}
	section(d.Out, "The batch plan")
	fmt.Fprint(d.Out, batchPlan.Document())

	// Ours first. LND's psbt_verify finds its own funding output and stops, so
	// an output nobody named passes all n of its checks — and it pins the funding
	// outpoint while it is at it, so a mis-paste caught here costs a redo and one
	// caught there costs a channel.
	section(d.Out, "Step 5 — does it match the plan?")
	v, err := batchPlan.Verify(unsigned)
	if err != nil {
		return nil, nil, fmt.Errorf("verifying the transaction your wallet built: %w", err)
	}
	fmt.Fprint(d.Out, v.Report())
	if !v.OK() {
		return nil, nil, errors.New("the transaction does not match the plan. " +
			"Nothing has been pinned and nothing has been signed: build it again")
	}

	// Then LND's, for every stream, with skip_finalize. Nothing is signed.
	verified, err := arm.Verify(ctx, d.LND.Lightning, d.Journal, o.RunID, streams, unsigned)
	if err != nil {
		return nil, nil, err
	}
	fmt.Fprintf(d.Out, "\n%s", prose.Para(fmt.Sprintf("psbt_verify: all %d "+
		"channels, with skip_finalize. LND has committed to the funding outpoints "+
		"of %s, and from here only signatures may be added.",
		len(streams.All), verified.TxID)))

	// Step 6, and it is the gate. Every channel becomes recoverable by
	// force-close here, over a transaction nobody has signed.
	armed, err := arm.Receipts(ctx, d.LND.Lightning, d.Journal, streams, verified)
	if err != nil {
		return nil, nil, err
	}
	fmt.Fprintf(d.Out, "\n%s", prose.Para(fmt.Sprintf("%d of %d channels reached "+
		"chan_pending, with nothing signed. Every one of them is recoverable by "+
		"force-close, the peers' ten minutes are no longer running, and nothing "+
		"has been broadcast.", len(armed.Channels), len(streams.All))))

	// Written before the round rather than after it, so a crash mid-signing is
	// legible as one. The journal refuses this unless the batch is armed.
	if err := d.Journal.MarkSigning(ctx, o.RunID); err != nil {
		return nil, nil, fmt.Errorf("journalling the start of the signing round: %w", err)
	}

	signed, err := sign(ctx, d, o, unsigned)
	if err != nil {
		return nil, nil, err
	}

	final, recheck, err := combine.Accept(batchPlan, unsigned, signed)
	if err != nil {
		return nil, nil, fmt.Errorf("checking what the signing wallet returned: %w", err)
	}
	if !recheck.OK() {
		fmt.Fprint(d.Out, recheck.Report())
		return nil, nil, errors.New("the finalized transaction does not match the plan")
	}
	if final.TxID != verified.TxID {
		return nil, nil, fmt.Errorf("I-3: the txid moved from %s to %s while it was "+
			"being signed", verified.TxID, final.TxID)
	}

	// The recheck prints nothing else on success — a clean re-run is not news —
	// but its reports are printed, deliberately. Two reasons. The size is exact
	// here rather than an upper bound, because every witness is present, so a fee
	// rate that sat inside tolerance at step 5 can land outside it now and this is
	// the first place anybody could know. And since item 6 these findings no
	// longer stop anything, so a caller that stayed quiet on success would be the
	// difference between a small change output being said out loud and it being a
	// surprise on the day it matters.
	fmt.Fprint(d.Out, recheck.Reported())

	// There is no pre-flight here any more, and its absence is stated rather than
	// left to be discovered. testmempoolaccept validated the batch without
	// relaying it, and it was Core's; Core is gone. What survives is narrower and
	// is not nothing: combine.Accept, above, executed every input's witness
	// against its own script, which answers "will each input validate" more
	// directly than a mempool test does and needs no chain data at all. What is
	// genuinely lost is node policy — min relay fee, standardness, ancestor
	// limits — and plan.Verify lists exactly that in Verification.Unchecked,
	// which the operator has already read by this point.
	fmt.Fprintf(d.Out, "signed and checked: %s in fees, %d vB, %.2f sat/vB. "+
		"Every witness executed against its own script.\n",
		prose.Sats(final.FeeSat), final.Vsize,
		float64(final.FeeSat)/float64(final.Vsize))

	return armed, final, nil
}

// sign is step 7: the operator signs, with the gate already open.
//
// The one part of the sequence whose duration belongs to the operator and the
// devices, and after the inversion it is outside every window that used to make
// that dangerous. Every channel reached chan_pending before this function is
// called, so the peers' ten minutes have stopped and there is nothing left inside
// them. What a slow step 7 spends is clock B — 2016 blocks from broadcast, and
// the transaction has not been broadcast yet, so in practice nothing at all.
//
// There is no gate here and there deliberately is not one.
// limits.abort_after_signing_seconds used to be checked before arming, against a
// dress rehearsal's measurement of this round; the round it measured no longer
// exists, and neither the gate nor the key does either. Stopping in the middle
// of this step costs an abort of n pending channels, which is the same thing it
// costs before it.
func sign(ctx context.Context, d Deps, o Options, unsigned []byte) ([]byte, error) {
	section(d.Out, "Step 7 — sign it")
	fmt.Fprint(d.Out, prose.Para("Take as long as this needs. Every channel in the "+
		"batch is already recoverable by force-close and nothing is in any mempool, "+
		"so there is no clock on this step that costs a restart — what it spends is "+
		"the 2016 blocks the peers give the funding transaction to confirm, counted "+
		"from a broadcast that has not happened yet."))

	label := combine.SigningWalletLabel
	if err := d.Journal.RecordSigner(ctx, o.RunID, label,
		journal.SignerAwaiting); err != nil {
		return nil, fmt.Errorf("journalling the signing step: %w", err)
	}

	started := time.Now()
	signed, err := d.Signing.Signed(ctx, unsigned)
	if err != nil {
		if jerr := d.Journal.RecordSigner(ctx, o.RunID, label,
			journal.SignerDeclined); jerr != nil {
			return nil, fmt.Errorf("the batch was not signed (%v), and journalling "+
				"that failed too: %w", err, jerr)
		}
		return nil, fmt.Errorf("the batch was not signed: %w", err)
	}
	if err := d.Journal.RecordSigner(ctx, o.RunID, label,
		journal.SignerSigned); err != nil {
		return nil, fmt.Errorf("journalling the signing step: %w", err)
	}
	fmt.Fprintf(d.Out, "signed (%s elapsed)\n", time.Since(started).Round(time.Second))
	return signed, nil
}

// changeIn decides how the plan will identify the change output.
//
// Named beats recognised and both beat nothing. The address, when the operator
// gave one, is a script the verifier can compare; the recognition is the wallet's
// own claim about its own output, read out of the packet's key origins. A wallet
// that writes neither leaves an output nobody can account for, and the refusal
// says which of the two to supply rather than reporting the plan as broken.
func changeIn(o Options, unsigned []byte) (plan.Change, error) {
	if o.Change != "" {
		return plan.Change{Address: o.Change}, nil
	}
	rec, err := plan.RecogniseChangeIn(unsigned)
	if err != nil {
		return plan.Change{}, fmt.Errorf("reading the wallet's key origins out of "+
			"the transaction: %w", err)
	}
	if rec == nil {
		return plan.Change{}, errors.New("this transaction's inputs carry no key " +
			"origin information, so there is no way to tell which output is your " +
			"change and which is an address nobody named — and the difference is the " +
			"whole check this tool exists for. Pass --change ADDRESS with the change " +
			"address your wallet used, which is a stronger check than the one it " +
			"replaces. Nothing has been pinned: this costs a redo")
	}
	return plan.Change{Recognise: rec}, nil
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

	// No Chain: that was Bitcoin Core, and this build dials no Bitcoin node. LND
	// moving a channel out of pending_open_channels is the authoritative signal
	// and needs nobody's help; what is lost is the "2 of an expected 3" depth
	// line, and settle.Options says so.
	result, err := settle.Settle(ctx, d.LND.Lightning, members(armed, p, o),
		settle.Options{FundingTxID: armed.TxID})
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
// By index, and that is sound rather than convenient: arm.Receipts appends one
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
// Ctrl-C in the terminal. On a cancelled context every call
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

	rep, err := d.Journal.Recover(ctx, d.LND.Lightning, o.RunID, d.Confirm)
	res.Aborted = rep
	fmt.Fprint(d.Out, prose.RecoveryOutcome(run, rep, err))
	return err
}

// aliases indexes Phase 0's peer facts by key, for the plan's labels.
//
// Phase 0 asked the graph once; this is that answer carried forward rather than
// a second lookup inside the armed window, where every RPC is on the peers'
// clock.
func aliases(facts []peers.Facts) map[string]string {
	out := make(map[string]string, len(facts))
	for _, f := range facts {
		if f.Alias != "" {
			out[strings.ToLower(f.Want.Pubkey)] = f.Alias
		}
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
