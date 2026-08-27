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
//     runs first, which is why a wrong output at step 4 costs a redo inside clock
//     A rather than a channel.
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
	// with step 8 withheld. See the package comment.
	StopBeforePublish bool

	// Probe runs a shim probe against every peer before arming. Off by default,
	// and it should stay off for an ordinary run: a successful probe is step 2
	// with the answer thrown away, and it holds one of the peer's
	// pending-channel slots for about eleven minutes afterwards.
	Probe bool

	// SettleFor bounds Phase 2. Zero means DefaultSettleFor.
	SettleFor time.Duration

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

// TeardownBudget is gone, and abort.CallBudget replaced it.
//
// It was five minutes around the whole teardown, sized on the reasoning that the
// slow part of an abort is the blunt confirmation rather than the RPCs. That was
// right about where the time goes and wrong about what to do with it: the
// confirmation sits between two LND calls, so an operator who took longer than
// the budget deciding got the abandon refused with a deadline *after* saying yes.
// Its own comment claimed "the seams clamp their own deadlines inside this one,
// so a confirmation nobody answers declines rather than erroring" — which was
// never true of confirmBlunt, which discards the context and reads stdin.
//
// The clock is per LND call now, in internal/abort, and there is none on the
// operator. See abort.CallBudget, which says the rest.

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
			return res, fmt.Errorf("the batch was armed and step 8 withheld, as "+
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
// getting n recipients in and choosing coins. Everything else is local: the
// verifier is arithmetic, psbt_verify is one RPC per stream, and the receipts
// arrived in 0.55 s at n = 2 on the harness. So a batch that blows clock A blows
// it in a wallet's Send tab, before LND has been shown anything, and the cost is
// n shim cancels.
//
// The first half of that step is smaller than it was. FileWallet writes the
// recipients as a CSV Sparrow's Send to Many loads in one action, so what is left
// inside the clock is choosing coins and a fee. It does not make the budget
// generous — it makes the part of it that scales with n stop scaling.
func armWindow(ctx context.Context, d Deps, o Options, p *prepared, res *Result) (
	*arm.Armed, *combine.Finalized, error) {

	section(d.Out, "Phase 1 — the armed window")

	streams, err := arm.Open(ctx, d.LND.Lightning, p.chain, p.chans)
	var opened *arm.OpenError
	if errors.As(err, &opened) {
		streams = opened.Streams
	}
	var newChans []journal.NewChannel
	if streams != nil {
		defer streams.Close()
		newChans = streams.NewChannels()
	}
	// The pending channel ids are the only handles that can cancel these streams,
	// so they go on disk before anything else happens — including before this
	// function decides whether Open failed.
	//
	// Only if there are any, though. arm.Open hands back its Streams on the FIRST
	// channel's failure too, so that its shims can be released; on that path
	// NewChannels() is empty and Begin refuses an empty batch outright. Calling it
	// anyway manufactured a journalling complaint and returned that in place of
	// the peer's own refusal — the operator was shown "a batch with no channels in
	// it is not a batch" where LND had said "Number of pending channels exceed
	// maximum", which is the sentence naming the remedy. #42.
	if len(newChans) > 0 {
		if jerr := d.Journal.Begin(ctx, o.RunID, newChans); jerr != nil {
			return nil, nil, unjournalledStreams(streams, err, jerr)
		}
	}
	if err != nil {
		return nil, nil, err
	}
	fmt.Fprintf(d.Out, "%d funding stream%s open, and the peers' ten minutes "+
		"start now.\n", len(streams.All), prose.Plural(len(streams.All)))
	// #33, once for the transcript's clock A. The ten minutes are
	// chanfunding.DefaultReservationTimeout, swept on
	// lncfg.DefaultZombieSweeperInterval, and both are LND's defaults rather
	// than anything a peer agreed to.
	fmt.Fprint(d.Out, "\n", prose.StockLNDNote(false, true))

	// The Phase 0 finding was about the batch the operator approved. These are
	// the streams that actually opened.
	if err := p.finding.StillApplies(streams.Batch()); err != nil {
		return nil, nil, err
	}

	// Step 3 and step 4: the recipients, and then the operator in their wallet.
	// The addresses are printed in full and never abbreviated, because this table
	// is the attribution — which peer gets which output, at what amount — and it
	// is the last screen on which that is legible to a human. How they reach the
	// wallet is the transport's business: FileWallet writes them as a CSV as well,
	// and prints where. Whichever route they take, an output nobody named is what
	// plan.Verify is for.
	section(d.Out, "Step 4 — build the transaction in your wallet")
	pay := recipientsOf(streams, p)
	fmt.Fprint(d.Out, recipientTable(pay))
	fmt.Fprint(d.Out, prose.Para("Pay exactly these recipients, choose your coins "+
		"and the fee, and save the PSBT. Do not sign it yet: nothing is recoverable "+
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
	// outpoint while it is at it, so a wrong output caught here costs a redo and
	// one caught there costs a channel.
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
	fmt.Fprint(d.Out, "\n", prose.Para("That recovery starts when the funding "+
		"transaction confirms, so do not close anything to undo this. LND does "+
		"not refuse a force-close on a pending channel: it would broadcast a "+
		"commitment whose parent is nowhere, destroying the channel and "+
		"recovering nothing. To stop here, Ctrl-C — the teardown abandons, and "+
		"this program cannot close a channel at all."))

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

	// The row that says the wallet signed is written here, one statement after
	// the only thing that establishes it, and not in sign() where the bytes
	// arrived. Issue #37. Accept has just executed every input's witness against
	// its own script; what sign() had was a file and a nil error out of
	// combine.Parse, which is a sniffer that reads five magic bytes. So on the
	// .psbt branch nothing had looked for a signature at all, and an operator who
	// saved the step-4 transaction at step 7 got journal.SignerSigned written over
	// the truthful SignerAwaiting row — RecordSigner upserts on (run_id, label) —
	// with the recovery screen reading it back afterwards as "1 signed" for a run
	// where nothing was. The .txn branch was guarded, by SignedFromTX's witness
	// count, so the same mistake journalled differently depending on which
	// encoding the wallet happened to save.
	//
	// This is #32's ordering rule with the mechanics reversed and the reason
	// unchanged: a row may only claim what has been established at the moment it
	// is written. MarkVerified moved *ahead* of its call because that row is read
	// to mean "the call may have landed, go and look", so its safe direction is
	// early. This one is read to mean "it did happen", so its safe direction is
	// late — and a crash in the gap leaves SignerAwaiting, which is the wallet
	// having been asked with nothing usable back, that state in its own words.
	if err := d.Journal.RecordSigner(ctx, o.RunID, combine.SigningWalletLabel,
		journal.SignerSigned); err != nil {
		return nil, nil, fmt.Errorf("journalling the signing step: %w", err)
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
	// #33, once for the transcript's clock B.
	fmt.Fprint(d.Out, "\n", prose.StockLNDNote(true, false))

	label := combine.SigningWalletLabel
	if err := d.Journal.RecordSigner(ctx, o.RunID, label,
		journal.SignerAwaiting); err != nil {
		return nil, fmt.Errorf("journalling the signing step: %w", err)
	}

	started := time.Now()
	signed, err := d.Signing.Signed(ctx, unsigned)
	if err != nil {
		// Nothing is journalled here, and the row written above is left standing.
		// Issue #25: this frame used to record journal.SignerDeclined, the one
		// SignerState that names an intent, and RecordSigner upserts on
		// (run_id, label) — so the true row was replaced by a false one, and the
		// recovery screen read it back later as "1 declined". Every error out of
		// Signed lands here: a failed os.Remove, before the wallet has been
		// prompted at all; the context being cancelled, which is Ctrl-C; a file
		// that is neither encoding; and combine's refusal of a moved txid, which
		// is an I-3 breach and the one failure an operator must not be sent to
		// their signing device over. Nothing in this build can observe a refusal
		// — a file transport has no channel through which a wallet says no.
		//
		// What the frame established is that the wallet was asked and nothing
		// usable came back, which is journal.SignerAwaiting in its own words. The
		// distinction a second state would draw — asked and still waiting against
		// asked and the answer was not usable — is the difference between a live
		// run and a stopped one, and that is the run's state rather than a
		// signer's. The cause is in the error below, on the terminal this step is
		// already printing to.
		return nil, fmt.Errorf("the batch was not signed: %w", err)
	}
	// Nothing is journalled on the way out either, and issue #37 is why: what
	// this frame has established is that a file came back and decoded, which on
	// the .psbt branch means combine.Parse recognised five magic bytes. No
	// signature is looked for anywhere on that path. The row that says the wallet
	// signed is written by armWindow, immediately after combine.Accept executes
	// every witness, and the SignerAwaiting row above stands until then.
	//
	// The printed line says the same and no more. It used to read "signed", which
	// was this claim one output earlier — and the honest version of it is already
	// printed by armWindow once the check has run, as "signed and checked".
	fmt.Fprintf(d.Out, "a file came back after %s; nothing has checked it for "+
		"signatures yet.\n", time.Since(started).Round(time.Second))
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

	// No Chain, here or anywhere: that was Bitcoin Core, and this build dials no
	// Bitcoin node. LND moving a channel out of pending_open_channels is the
	// authoritative signal and needs nobody's help; what is lost is the "2 of an
	// expected 3" depth line, and the report now says that as a design fact
	// rather than as a Core that failed to connect (issue #21). settle.Chain's
	// own comment says who does fill it — the harness, and only the harness.
	result, err := settle.Settle(ctx, d.LND.Lightning, members(armed, p, o),
		settle.Options{FundingTxID: armed.TxID})
	if result != nil {
		fmt.Fprint(d.Out, result.Report())
	}
	switch {
	case err == nil:
	case errors.Is(err, settle.ErrStuck):
		// Not a stop, and it must not read as one. Every other member was
		// watched to the end; these are the ones the loop could not police, and
		// the report above says what to do about each of them.
		fmt.Fprintf(d.Out, "\nThe settlement finished, and %v\n", err)
	default:
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

// unjournalledStreams is what to say when Begin failed with streams already open.
//
// This is the one shape arm.Open's doc exists for: n peers holding reservations
// with nothing on disk to cancel. `winthistle recover` works off the journal and
// will not see them, and a shim outlives the stream it came on —
// TestAShimSurvivesItsStreamBeingHungUp on a live node — so hanging up does not
// release them either. The pending channel ids are therefore printed here, in the
// error, because this is the last moment this program knows them.
//
// openErr, when there is one, comes first and is not discarded. It is the peer's
// or LND's own sentence about why the batch stopped, and it is the only thing
// here that names a remedy.
func unjournalledStreams(streams *arm.Streams, openErr, jerr error) error {
	ids := streams.PendingChanIDs()
	labels := make([]string, 0, len(ids))
	for _, id := range ids {
		labels = append(labels, id.String())
	}
	left := fmt.Errorf("journalling the run failed, so the %d funding stream%s that "+
		"did open %s not on disk and `winthistle recover` cannot see %s. Cancel "+
		"%s out of band with lncli fundingstatestep --shim_cancel, or wait out the "+
		"peers' window — pending channel id%s %s: %w",
		len(ids), prose.Plural(len(ids)), prose.IsAre(len(ids)), them(len(ids)),
		them(len(ids)), prose.Plural(len(ids)), strings.Join(labels, ", "), jerr)
	if openErr == nil {
		return left
	}
	return fmt.Errorf("%w. And %w", openErr, left)
}

// them is the pronoun for a count, so a one-stream failure does not read as a
// plural.
func them(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}

// recoverRun tears down whatever the run left behind, through the journal.
//
// # The teardown outlives the cancellation that caused it
//
// The commonest reason to be here is that the run's context was cancelled —
// Ctrl-C in the terminal. On a cancelled context every call below fails at once:
// the journal read is a database/sql query, and the shim cancels and the
// abandons are gRPC. So an abort triggered by Ctrl-C would have reported
// "context canceled" and taken nothing apart, which is the exact opposite of
// what the deferred teardown is for.
//
// So cancellation is deliberately not inherited. **The deadline is not ours
// either, any more.** There used to be a five-minute TeardownBudget here, and it
// bounded the blunt confirmation along with the calls — see the note where that
// constant used to be. What bounds a teardown now is abort.CallBudget, per LND
// call, which is what keeps an unresponsive node from holding a terminal the
// operator has already tried to get out of. The operator's own deliberation is
// not a hang and is not on a clock.
//
// This comment used to cite releaseFence and Core's JSON-RPC lock release as the
// precedent and as part of the work. Item 5 deleted both: the app takes no coin
// locks and dials no Bitcoin node.
func recoverRun(ctx context.Context, d Deps, o Options, res *Result) error {
	ctx = context.WithoutCancel(ctx)

	run, err := d.Journal.Load(ctx, o.RunID)
	if errors.Is(err, journal.ErrNoRun) {
		// No run row, so there is nothing for this path to work from: Recover
		// reads the journal and the journal is empty. That is all this knows, and
		// it used to say more — "which means arm.Open never returned a stream" —
		// which is a cause asserted from a row's absence. Begin writes the run row
		// and the channel rows in one transaction, so ErrNoRun is also what a
		// successful arm.Open followed by a failed Begin looks like, and that is
		// the shape arm.Open's doc exists for. #42.
		//
		// It is not silently lost: armWindow names those streams and prints their
		// pending channel ids in the error it returns on that path, because that
		// is the last moment anything knows them. What is left here is genuinely
		// nothing to do.
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
