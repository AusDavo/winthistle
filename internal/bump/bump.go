// Package bump is `winthistle bump`: the CPFP child that accelerates a batch
// which went out too cheap.
//
// I-4 forbids replacing the funding transaction — replacing it moves every
// outpoint in it, and every peer holds a commitment signature against the old
// ones — so a batch that is underpaying can only be accelerated by spending its
// own change. internal/plan sized the change output at planning time so that
// such a child would be affordable; internal/settle builds one; this package is
// what turns a built child into a broadcast one, which turns out to be most of
// the work.
//
// # It is a second cold-wallet session, and that is the shape of the whole thing
//
// The change output belongs to cold storage, so the child is returned unsigned
// and there is no shortcut that does not amount to a hot key able to spend the
// batch's change (I-2). So this command is not a wiring job on top of
// settle.BuildChild: it is a second signing round, with its own transport, its
// own verification and its own journal rows. Everything here exists because of
// that one fact.
//
// # The three things it does that BuildChild does not
//
//  1. It finds the parent, from Core rather than from anything remembered. A
//     transaction being bumped is by definition unconfirmed, so getmempoolentry
//     holds its exact size and fee — and its ancestors, which is the figure the
//     arithmetic actually needs. See Locate.
//  2. It verifies, twice. A one-in one-out child cannot go through plan.Plan,
//     which refuses a batch with no channels, and an unverified PSBT reaching a
//     cold-storage device is exactly what internal/plan exists to prevent. See
//     verify.go.
//  3. It broadcasts, which is the second and last call to
//     WalletKit.PublishTransaction in this build. See publish.go for why that is
//     allowed to exist and what makes it unable to carry a funding transaction.
//
// # What it will not do
//
// Replace anything. There is no code path in this repository that bumps the
// parent, and this package is what exists instead.
package bump

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/AusDavo/winthistle/internal/abort"
	"github.com/AusDavo/winthistle/internal/bitcoind"
	"github.com/AusDavo/winthistle/internal/combine"
	"github.com/AusDavo/winthistle/internal/fees"
	"github.com/AusDavo/winthistle/internal/journal"
	"github.com/AusDavo/winthistle/internal/plan"
	"github.com/AusDavo/winthistle/internal/prose"
	"github.com/AusDavo/winthistle/internal/rehearsal"
	"github.com/AusDavo/winthistle/internal/settle"
	"github.com/lightningnetwork/lnd/lnrpc"
	"google.golang.org/grpc"
)

// Client is the slice of LND this package reads.
//
// One method, and it changes nothing. A bump does not open a stream, does not
// touch a channel and does not ask the node for a coin — the only thing it wants
// from LND besides the broadcast is the countdown, and PendingChannels is where
// LND publishes it.
type Client interface {
	PendingChannels(ctx context.Context, in *lnrpc.PendingChannelsRequest,
		opts ...grpc.CallOption) (*lnrpc.PendingChannelsResponse, error)
}

// Signers is where the child's partial signatures come from.
//
// The same interface internal/run takes and the same rehearsal.Device it hands
// back, so the child goes to the devices through the transport the batch used.
// The round's name carries the bump's sequence number, which matters for the
// file transport: a second bump minutes after the first must not pick up the
// first one's signed file.
type Signers interface {
	Round(name string) []rehearsal.Device
	Labels() []string
}

// Deps are the connections and the journal, already open.
type Deps struct {
	LND       Client
	Publisher Publisher

	// Node is Core with no wallet scope: the mempool entry, the fee estimate and
	// testmempoolaccept. Wallet is the watch-only cold wallet that owns the
	// change output.
	Node   *bitcoind.Client
	Wallet *bitcoind.Client

	Journal *journal.Journal
	Signers Signers

	Out io.Writer

	// Approve is asked once, after the child is built and verified and before it
	// goes to the devices. Nil means do not ask, which is right for anything
	// non-interactive: nothing after this point can lose the batch, and the
	// operator has already decided by running the command.
	Approve func(ctx context.Context, question string) (bool, error)
}

// Options are the bump.
type Options struct {
	RunID string

	// TargetSatPerVB is the rate to lift the parent and child to together. Zero
	// means ask Core, through internal/fees — never a fee API, which would be
	// handed the size of what is being built and the moment it is being built.
	TargetSatPerVB float64

	// Fees configures that estimate when TargetSatPerVB is zero.
	Fees fees.Request

	// BuildOnly stops after the child is built and verified, without asking any
	// device for a signature. It is the bump's dry run: the arithmetic is the
	// part that can be wrong, and checking it costs nothing and reveals whether
	// a cold wallet is worth bringing out.
	BuildOnly bool
}

// Result is what the bump did, however far it got.
type Result struct {
	RunID string
	Seq   int64

	Located *Located
	Child   *settle.Child

	// Verified is the check on what was built; Rechecked the check on what came
	// back from the devices.
	Verified  *Verification
	Rechecked *Verification

	Published bool

	// Released is the coin lock handed back when the bump did not finish.
	Released *abort.Report
}

// Located is the parent, as Core and the cold wallet see it right now.
//
// Nothing in it is remembered from the run that produced the batch. The journal
// says which transaction to look for; everything else is read, because the whole
// premise of a bump is that the fee market moved and the figures the batch was
// built against are stale.
type Located struct {
	RunID      string
	ParentTxID string

	// Parent carries the figures the arithmetic uses, which are the ancestor
	// figures when the parent is not the bottom of its own package. See Locate.
	Parent settle.Parent

	// Entry is what Core said, unmodified, so the report can show its working.
	Entry bitcoind.MempoolEntry

	// Change is the batch's change output, however it had to be found.
	Change Change

	// Replaces is the standing child this lift will replace, or nil for a first
	// lift. Non-nil means the change output is already spent by a child of ours
	// that is sitting in a mempool, and this bump is an RBF of it rather than a
	// new spend — which is what building the child replaceable bought.
	Replaces *journal.Bump

	// StandingFeeSat is what that child pays, from Core's mempool entry rather
	// than from the journal: BIP-125 rule 3 is about the fee the network can see,
	// and the row records what was intended.
	StandingFeeSat int64

	// ExpiryBlocks is the smallest funding_expiry_blocks across the batch's
	// members that LND still lists as pending, and HasExpiry whether there was
	// one to report. This is the clock the signing round is racing.
	ExpiryBlocks int32
	HasExpiry    bool
}

// Change is the batch's change output: the one thing a CPFP child spends.
//
// It carries its own scripts because there are two ways to find it and only one
// of them is listunspent. Once an unconfirmed transaction spends an output Core
// drops it from listunspent, so a *second* lift — which by definition spends
// what the first one spent — has to reconstruct the coin from the parent's own
// outputs plus getaddressinfo. Both routes fill the same four fields, so
// everything downstream is indifferent to which one ran.
type Change struct {
	Outpoint  bitcoind.Outpoint
	AmountSat int64

	// ScriptPubKey and WitnessScript are what sizing a spend of this output
	// needs. WitnessScript is empty for a single-sig cold wallet, which is fine —
	// plan.ChildVsize only wants it for P2WSH.
	ScriptPubKey  []byte
	WitnessScript []byte
}

var (
	// ErrParentConfirmed means the batch confirmed while the operator was
	// deciding, which is the outcome a bump was trying to buy.
	ErrParentConfirmed = errors.New("the batch has confirmed, so there is nothing to accelerate")

	// ErrParentMissing means Core has the transaction neither in its mempool nor
	// in a block. That is a re-broadcast, not a bump.
	ErrParentMissing = errors.New("the batch is in neither the mempool nor the chain")

	// ErrChangeSpent means something already spends the batch's change output and
	// it is not a child this journal knows about — so it is not ours to replace.
	ErrChangeSpent = errors.New("the batch's change output is spent by something this journal did not build")

	// ErrCheaperThanStanding means the lift asked for would produce a replacement
	// paying less than the child already in the mempool. BIP-125 rule 3 refuses
	// that, and so does this, before a device is asked for anything.
	ErrCheaperThanStanding = errors.New("the replacement would pay less than the child it replaces")

	// ErrChangeAmbiguous means the cold wallet holds more than one output of the
	// parent, so which one is the change cannot be decided.
	ErrChangeAmbiguous = errors.New("more than one output of the batch belongs to the cold wallet")

	// ErrDeclined means the operator was asked and said no.
	ErrDeclined = errors.New("the child was not signed")
)

// Locate finds the parent and its change output.
//
// # The parent's figures are Core's, not ours
//
// A transaction being bumped is unconfirmed by definition, so Core is holding
// its exact virtual size and its exact fee and there is no reason to re-derive
// either. Summing prevouts would produce a second opinion that could disagree
// with the one the fee market uses, and the disagreement would be invisible.
//
// # And they are the ancestor figures, which is not the obvious choice
//
// walletcreatefundedpsbt charges the fee that lifts the whole *unconfirmed
// ancestor package* to the rate it is given, and "the ancestor package" means
// every unconfirmed transaction the parent depends on as well as the parent. For
// a batch funded from confirmed coins those are the same thing — ancestorcount
// is 1 and the ancestor figures equal the transaction's own — which is the
// ordinary case and exactly why the difference is easy to miss. A batch built
// from an unconfirmed coin is not that case, and passing the parent's own size
// there would ask plan.ChildFeeSat a different question from the one Core
// answers, so the two would disagree and the disagreement would be reported as
// Core misbehaving.
//
// Core's own wording settles it: ancestorsize and fees.ancestor are documented
// as "including this one".
//
// # The change output identifies itself
//
// listunspent on the watch-only cold wallet, filtered to the parent's txid,
// finds it unambiguously, and that is a property of the plan rather than a
// heuristic. Every other output of a batch belongs to somebody else: the funding
// outputs are the peers' 2-of-2 P2WSH, and the reserve top-up pays the node's own
// wallet through lnrpc NewAddress rather than the Core wallet. So the change is
// the only output of the batch cold-watch can see. More than one is refused
// rather than guessed at.
func Locate(ctx context.Context, d Deps, runID string) (*Located, error) {
	run, err := d.Journal.Load(ctx, runID)
	if err != nil {
		return nil, err
	}
	switch run.State {
	case journal.StatePublishing, journal.StatePublished:
	default:
		return nil, fmt.Errorf("run %s is %s, so no transaction of it is in any "+
			"mempool: %w", runID, run.State, journal.ErrNotPublic)
	}
	if run.TxID == "" {
		return nil, fmt.Errorf("run %s reached the publish call with no txid "+
			"journalled, which should not be possible", runID)
	}

	l := &Located{RunID: runID, ParentTxID: run.TxID}

	entry, inMempool, err := d.Node.MempoolEntry(ctx, run.TxID)
	if err != nil {
		return nil, err
	}
	if !inMempool {
		confs, present, err := d.Node.Confirmations(ctx, run.TxID)
		if err != nil {
			return nil, err
		}
		if present && confs > 0 {
			return l, fmt.Errorf("%w: %s has %d confirmation%s",
				ErrParentConfirmed, run.TxID, confs, prose.Plural(int(confs)))
		}
		return l, fmt.Errorf("%w: Core has no record of %s. A child of a "+
			"transaction nobody has would spend an output that does not exist, so "+
			"it could never confirm. The finalized batch is in this run's journal "+
			"row: re-broadcast that first, and bump it afterwards if it needs it",
			ErrParentMissing, run.TxID)
	}
	l.Entry = entry

	// The figures the arithmetic uses. See the doc comment.
	vsize, fee := entry.VsizeVB, entry.FeeSat
	if entry.HasUnconfirmedAncestors() {
		vsize, fee = entry.AncestorVsizeVB, entry.AncestorFeeSat
	}

	change, err := locateChange(ctx, d, runID, run.TxID, entry)
	if err != nil {
		return l, err
	}
	l.Change = change
	l.Parent = settle.Parent{
		TxID:      run.TxID,
		VsizeVB:   vsize,
		FeeSat:    fee,
		Change:    plan.Outpoint{TxID: change.Outpoint.TxID, Vout: change.Outpoint.Vout},
		ChangeSat: change.AmountSat,
	}

	// Is a child of ours already spending it? If so this lift is a replacement,
	// and what it has to beat is that child's absolute fee.
	standing, err := d.Journal.StandingChild(ctx, runID, change.Outpoint)
	if err != nil {
		return l, err
	}
	if standing != nil {
		l.Replaces = standing
		l.StandingFeeSat = standing.ChildFeeSat
		// Core's figure rather than the journal's where both exist: the row says
		// what was intended and BIP-125 rule 3 is about what the network can see.
		if standing.ChildTxID != "" {
			if e, in, err := d.Node.MempoolEntry(ctx, standing.ChildTxID); err == nil && in {
				l.StandingFeeSat = e.FeeSat
			}
		}
	}

	l.ExpiryBlocks, l.HasExpiry, err = nearestExpiry(ctx, d.LND, run.TxID)
	if err != nil {
		// Not fatal. The countdown is what the report warns about, not something
		// the arithmetic needs, and a bump on a node that will not answer this
		// question is still a bump worth making.
		fmt.Fprintf(d.Out, "could not read the funding horizon from LND: %v\n", err)
	}
	return l, nil
}

// locateChange picks the batch's change output out, by whichever of the two
// routes applies.
//
// listunspent is the ordinary one, and it identifies the change without a
// heuristic: every other output of a batch belongs to somebody else, since the
// funding outputs are the peers' 2-of-2 scripts and the reserve top-up pays the
// node's own wallet through lnrpc NewAddress rather than the Core wallet.
//
// The second route exists because of the second lift. Core drops an output from
// listunspent the moment an unconfirmed transaction spends it, so a replacement
// of a standing child cannot see the coin it is about to re-spend. When the
// journal says a child of ours is out there, the coin is reconstructed from the
// parent's own outputs — which are still in the mempool, so getrawtransaction
// answers — plus getaddressinfo for the scripts. Same four fields either way.
func locateChange(ctx context.Context, d Deps, runID, parentTxID string,
	entry bitcoind.MempoolEntry) (Change, error) {

	utxos, err := d.Wallet.ListUnspent(ctx, 0, math.MaxInt32)
	if err != nil {
		return Change{}, fmt.Errorf("listing the cold wallet's outputs: %w", err)
	}
	var found []bitcoind.UTXO
	for _, u := range utxos {
		if u.TxID == parentTxID {
			found = append(found, u)
		}
	}
	switch len(found) {
	case 1:
		return changeFromUTXO(found[0])
	case 0:
		// Spent, or locked, or not ours — and those need different answers.
		return spentChange(ctx, d, runID, parentTxID, entry)
	default:
		ops := make([]string, 0, len(found))
		for _, u := range found {
			ops = append(ops, fmt.Sprintf("%s:%d (%s)", u.TxID, u.Vout,
				prose.Sats(satsOf(u.Amount))))
		}
		return Change{}, fmt.Errorf("%w: %v. A batch has exactly one output "+
			"this wallet owns, and which of these is the CPFP lever cannot be "+
			"guessed", ErrChangeAmbiguous, ops)
	}
}

func changeFromUTXO(u bitcoind.UTXO) (Change, error) {
	script, err := hex.DecodeString(u.ScriptPubKey)
	if err != nil {
		return Change{}, fmt.Errorf("the change output's script is not hex: %w", err)
	}
	// Empty for a single-sig wallet, which is not an error: plan.ChildVsize only
	// wants a witness script for P2WSH.
	witness, err := hex.DecodeString(u.WitnessScript)
	if err != nil {
		return Change{}, fmt.Errorf("the change output's witness script is not hex: %w", err)
	}
	return Change{
		Outpoint:      u.Outpoint(),
		AmountSat:     satsOf(u.Amount),
		ScriptPubKey:  script,
		WitnessScript: witness,
	}, nil
}

// spentChange handles every reason listunspent showed nothing, and they are not
// interchangeable.
//
// A child of ours already spending it is the second-lift case and the whole
// point of building the child replaceable: reconstruct the coin and carry on.
// A lock this run is holding looks identical from Core's side and needs the
// opposite advice. Something else spending it is not ours to replace.
func spentChange(ctx context.Context, d Deps, runID, parentTxID string,
	entry bitcoind.MempoolEntry) (Change, error) {

	// The journal first, because it is the only thing that can tell a coin we
	// are holding from a coin somebody spent.
	bumps, err := d.Journal.Bumps(ctx, runID)
	if err != nil {
		return Change{}, err
	}
	var (
		standing *journal.Bump
		holding  *journal.Bump
	)
	for _, b := range bumps {
		switch {
		case b.Standing():
			standing = b
		case !b.Finished() && heldLocks(b) > 0:
			holding = b
		}
	}

	if standing != nil {
		change, err := changeFromParent(ctx, d, parentTxID, standing.Plan.Change)
		if err != nil {
			return Change{}, fmt.Errorf("a child of this run (bump %d, %s) is "+
				"standing in the mempool, so this lift would replace it — but the "+
				"change output it spends could not be read back: %w",
				standing.Seq, standing.ChildTxID, err)
		}
		return change, nil
	}

	if holding != nil {
		return Change{}, fmt.Errorf("the cold wallet reports no unspent output of "+
			"%s, and bump %d of this run is %s and still holds a lock on %s. Core "+
			"leaves locked outputs out of listunspent, so a coin this run is "+
			"already holding is indistinguishable from a spent one from here — the "+
			"journal is what tells them apart.\n"+
			"Give up on that bump first, which releases the lock: "+
			"`winthistle bump %s --abandon`",
			parentTxID, holding.Seq, holding.State, holding.Plan.Change, runID)
	}

	if entry.DescendantCount > 1 {
		return Change{}, fmt.Errorf("%w: Core lists %d transaction(s) already "+
			"spending outputs of %s (%v), and this journal has no child of this "+
			"run in a mempool.\nA child this build made would be journalled, so "+
			"either this is not the journal that made it or something else is "+
			"spending the batch's change. Replacing a transaction whose history is "+
			"not on disk is not something to do from a command line",
			ErrChangeSpent, entry.DescendantCount-1, parentTxID, entry.SpentBy)
	}

	return Change{}, fmt.Errorf("the cold wallet holds no unspent output of %s. "+
		"Every other output of a batch belongs to somebody else — the funding "+
		"outputs are the peers' scripts and the reserve top-up pays LND's own "+
		"wallet — so the change is the only one this wallet should see. Check that "+
		"this is the wallet that funded the batch", parentTxID)
}

// changeFromParent reconstructs a change output Core will no longer list.
//
// Every figure comes from Core: the value and script from the parent's own
// outputs, and the witness script from getaddressinfo, whose "hex" field is the
// script behind a P2WSH address. The journal supplies only *which* output to
// look at, which is the one thing Core cannot say once the coin is spent.
//
// Ownership is re-checked rather than assumed. The journal names an outpoint;
// this asks the wallet whether that outpoint is still one of its own, so a
// journal pointed at the wrong wallet produces a refusal rather than a
// transaction paying a stranger.
func changeFromParent(ctx context.Context, d Deps, parentTxID string,
	want bitcoind.Outpoint) (Change, error) {

	outs, err := d.Node.TxOutputs(ctx, parentTxID)
	if err != nil {
		return Change{}, err
	}
	for _, o := range outs {
		if o.Vout != want.Vout {
			continue
		}
		if o.Address == "" {
			return Change{}, fmt.Errorf("output %d of %s pays a non-standard script, "+
				"so the cold wallet cannot be asked whether it owns it", o.Vout, parentTxID)
		}
		info, err := d.Wallet.GetAddressInfo(ctx, o.Address)
		if err != nil {
			return Change{}, fmt.Errorf("asking the cold wallet about %s: %w",
				o.Address, err)
		}
		if !info.Ours() {
			return Change{}, fmt.Errorf("the journal says the batch's change is "+
				"output %d of %s, and the cold wallet does not own that address (%s). "+
				"This is not the wallet that funded the batch", o.Vout, parentTxID,
				o.Address)
		}
		script, err := hex.DecodeString(o.ScriptHex)
		if err != nil {
			return Change{}, fmt.Errorf("output %d of %s has a script that is not "+
				"hex: %w", o.Vout, parentTxID, err)
		}
		// Empty unless the address is P2SH or P2WSH, which is right: a single-sig
		// change output has no witness script and plan.ChildVsize wants none.
		witness, err := hex.DecodeString(info.Hex)
		if err != nil {
			return Change{}, fmt.Errorf("the witness script behind %s is not hex: %w",
				o.Address, err)
		}
		return Change{
			Outpoint:      want,
			AmountSat:     o.AmountSat,
			ScriptPubKey:  script,
			WitnessScript: witness,
		}, nil
	}
	return Change{}, fmt.Errorf("%s has no output %d", parentTxID, want.Vout)
}

// heldByAnotherBump names an unfinished bump of this run that is still holding a
// coin lock, or "" if there is none.
func heldByAnotherBump(ctx context.Context, d Deps, runID string) (string, error) {
	bumps, err := d.Journal.Bumps(ctx, runID)
	if err != nil {
		return "", err
	}
	for _, b := range bumps {
		if b.Finished() {
			continue
		}
		for _, l := range b.Locks {
			if !l.Released {
				return fmt.Sprintf("bump %d of this run is %s and still holds a lock "+
					"on %s", b.Seq, b.State, l.Outpoint), nil
			}
		}
	}
	return "", nil
}

// nearestExpiry is the smallest funding_expiry_blocks across the batch's members
// still pending: how many blocks remain before the first peer gives up.
//
// LND computes it as waitBlocksForFundingConf + broadcastHeight - currentHeight
// and its own proto says a negative value means the responder has very likely
// cancelled. It is the only clock in this whole command, and it belongs to the
// peers rather than to us: our node is the initiator and never times out.
func nearestExpiry(ctx context.Context, cli Client, parentTxID string) (int32, bool, error) {
	if cli == nil {
		return 0, false, nil
	}
	resp, err := cli.PendingChannels(ctx, &lnrpc.PendingChannelsRequest{})
	if err != nil {
		return 0, false, fmt.Errorf("listing this node's pending channels: %w", err)
	}
	var (
		nearest int32
		found   bool
	)
	for _, p := range resp.GetPendingOpenChannels() {
		cp := p.GetChannel().GetChannelPoint()
		if len(cp) < 64 || cp[:64] != parentTxID {
			continue
		}
		if n := p.GetFundingExpiryBlocks(); !found || n < nearest {
			nearest, found = n, true
		}
	}
	return nearest, found, nil
}

// Do is the whole command: find the parent, build the child, verify it, sign it,
// verify what came back, and broadcast.
//
// # Where it gives up, and where it does not
//
// Any failure before the publish releases the coin lock the build took and
// leaves the batch exactly as it was. That is the whole of a bump's teardown —
// there is no shim to cancel and no channel to abandon, because a child talks to
// no peer and creates nothing.
//
// The one place it deliberately does *not* refuse is the funding horizon. If the
// peers' countdown runs out while the devices are signing, the child is still
// worth broadcasting: what expired is the peers' patience, not the child's
// validity, and under I-4 confirming the parent remains the only way the change
// ever becomes spendable again. Withholding the publish there would cost the
// coins to save channels that are already lost. See reportRace.
func Do(ctx context.Context, d Deps, o Options) (*Result, error) {
	if o.RunID == "" {
		return nil, errors.New("a bump needs the id of the run whose batch it is " +
			"accelerating: the journal is what says which transaction that is")
	}
	if d.Out == nil {
		return nil, errors.New("a bump has to be able to show its arithmetic")
	}
	res := &Result{RunID: o.RunID}

	section(d.Out, "The batch being accelerated")
	located, err := Locate(ctx, d, o.RunID)
	res.Located = located
	if err != nil {
		return res, err
	}
	fmt.Fprint(d.Out, located.Report())

	target := o.TargetSatPerVB
	if target <= 0 {
		section(d.Out, "The target rate")
		rate, err := fees.Estimate(ctx, d.Node, o.Fees)
		if err != nil {
			return res, fmt.Errorf("choosing a target rate: %w\n"+
				"Name one with --target instead. A bump is the one place where "+
				"guessing is worse than asking, because I-4 means the batch gets no "+
				"second attempt", err)
		}
		fmt.Fprint(d.Out, rate.Report())
		target = rate.SatPerVB
	}
	if target <= located.Parent.Rate() {
		return res, fmt.Errorf("%w: it pays %.2f sat/vB and the target is %.2f",
			settle.ErrNoChildNeeded, located.Parent.Rate(), target)
	}

	// The ceiling, and — on a second lift — whether the replacement can beat what
	// it is replacing. Both are checked against the arithmetic before anything is
	// built, because this is the cheapest possible moment to find out and the
	// alternative is an operator who has already fetched two hardware devices.
	childVsize := estimateChildVsize(located)
	if err := checkCeiling(located, target, childVsize); err != nil {
		return res, err
	}
	if err := checkReplacement(located, target, childVsize); err != nil {
		return res, err
	}

	seq, err := d.Journal.BeginBump(ctx, o.RunID, journal.BumpPlan{
		ParentTxID:     located.Parent.TxID,
		ParentVsizeVB:  located.Parent.VsizeVB,
		ParentFeeSat:   located.Parent.FeeSat,
		Change:         located.Change.Outpoint,
		ChangeSat:      located.Parent.ChangeSat,
		TargetSatPerVB: target,
	})
	if err != nil {
		return res, fmt.Errorf("journalling the bump: %w", err)
	}
	res.Seq = seq

	child, exp, err := buildAndVerify(ctx, d, o, res, located, target)
	if err != nil {
		res.Released = giveUp(ctx, d, o.RunID, seq)
		return res, err
	}

	if o.BuildOnly {
		fmt.Fprint(d.Out, buildOnly(child))
		res.Released = giveUp(ctx, d, o.RunID, seq)
		return res, nil
	}

	if d.Approve != nil {
		ok, err := d.Approve(ctx, fmt.Sprintf(
			"Sign this child and broadcast it, lifting the batch to %.2f sat/vB?",
			target))
		if err != nil || !ok {
			res.Released = giveUp(ctx, d, o.RunID, seq)
			if err != nil {
				return res, err
			}
			return res, ErrDeclined
		}
	}

	signed, err := Sign(ctx, d, o.RunID, seq, child, exp, res)
	if err != nil {
		res.Released = giveUp(ctx, d, o.RunID, seq)
		return res, err
	}

	// The horizon, re-read after the round and reported rather than enforced.
	fmt.Fprint(d.Out, reportRace(ctx, d, located))

	if err := Publish(ctx, d.Publisher, d.Journal, signed); err != nil {
		// No teardown. MarkBumpPublishing landed before the RPC, so these bytes
		// may be in a mempool, and releasing the coin lock now would say the
		// change is free to spend when it may already be spent.
		return res, err
	}
	res.Published = true

	// Only now. Until those bytes were out, the older child was still the one in
	// the mempool, and a journal that had already called it superseded would have
	// been making a claim about the network it could not make.
	if located.Replaces != nil {
		if err := d.Journal.Supersede(ctx, o.RunID, located.Replaces.Seq); err != nil {
			fmt.Fprintf(d.Out, "\nthe replacement was published and the journal could "+
				"not record that bump %d is superseded: %v\nBoth rows say published, "+
				"which is untidy rather than dangerous — the network has replaced one "+
				"with the other regardless.\n", located.Replaces.Seq, err)
		}
	}
	fmt.Fprint(d.Out, published(signed, located))
	return res, nil
}

// buildAndVerify asks Core for the child and checks what it produced.
func buildAndVerify(ctx context.Context, d Deps, o Options, res *Result,
	l *Located, target float64) (*settle.Child, Expectation, error) {

	section(d.Out, "The child")
	child, err := settle.BuildChild(ctx, settle.ChildRequest{
		Wallet:         d.Wallet,
		Parent:         l.Parent,
		TargetSatPerVB: target,
	})
	if err != nil {
		return nil, Expectation{}, err
	}
	res.Child = child

	if err := d.Journal.RecordBumpChild(ctx, o.RunID, res.Seq, child.TxID,
		child.FeeSat, child.Inputs); err != nil {
		return nil, Expectation{}, fmt.Errorf("journalling the child: %w", err)
	}

	chain, err := chainName(ctx, d)
	if err != nil {
		return nil, Expectation{}, err
	}
	exp := Expectation{
		Chain:          chain,
		Parent:         l.Parent,
		TargetSatPerVB: target,
		PaysTo:         child.PaysTo,
		FeeSat:         child.FeeSat,
	}

	v, err := Verify(child.Raw, exp)
	res.Verified = v
	if err != nil {
		return nil, exp, fmt.Errorf("verifying the child Core built: %w", err)
	}
	fmt.Fprint(d.Out, child.Report(l.Parent, target))
	fmt.Fprint(d.Out, v.Report("what was built"))
	if !v.OK() {
		return nil, exp, errors.New("the child does not match the arithmetic it was " +
			"built from, so it is not going to any device")
	}
	return child, exp, nil
}

// Sign is the second cold-wallet session: the round, the merge, the finalize,
// and the verification of what came back.
//
// It produces the only *Signed there is, which is what makes Publish reachable —
// see publish.go. The merge is internal/combine's, unchanged: I-2 applies to the
// child no less than to the batch, so the devices return partials and the last
// signature is applied here.
func Sign(ctx context.Context, d Deps, runID string, seq int64, child *settle.Child,
	exp Expectation, res *Result) (*Signed, error) {

	if d.Signers == nil {
		return nil, errors.New("no signers: the change output belongs to cold " +
			"storage, so there is nothing that can spend it. Add a [[signer]] block " +
			"per device")
	}

	section(d.Out, "The signing round")
	fmt.Fprint(d.Out, prose.Para("This is a second cold-wallet session, and it is "+
		"the whole cost of a bump. The batch's change output is a cold-wallet "+
		"output like any other, so accelerating the batch needs every signer in "+
		"turn, returning a partial. There is no shortcut that does not amount to a "+
		"hot key able to spend the batch's change (I-2)."))

	devices := d.Signers.Round(fmt.Sprintf("bump-%d", seq))
	for _, dev := range devices {
		if err := d.Journal.RecordBumpSigner(ctx, runID, seq, dev.Label,
			journal.SignerAwaiting); err != nil {
			return nil, fmt.Errorf("journalling signer %s: %w", dev.Label, err)
		}
	}

	started := time.Now()
	parts := make([]combine.Part, 0, len(devices))
	for _, dev := range devices {
		part, err := dev.Sign(ctx, child.PSBT)
		if err != nil {
			if jerr := d.Journal.RecordBumpSigner(ctx, runID, seq, dev.Label,
				journal.SignerDeclined); jerr != nil {
				return nil, fmt.Errorf("%s did not sign (%v), and journalling that "+
					"failed too: %w", dev.Label, err, jerr)
			}
			return nil, fmt.Errorf("%s did not sign the child: %w", dev.Label, err)
		}
		if err := d.Journal.RecordBumpSigner(ctx, runID, seq, dev.Label,
			journal.SignerPartial); err != nil {
			return nil, fmt.Errorf("journalling signer %s: %w", dev.Label, err)
		}
		parts = append(parts, part)
		fmt.Fprintf(d.Out, "%s signed (%s elapsed)\n", dev.Label,
			time.Since(started).Round(time.Second))
	}

	merged, err := combine.Merge(child.Raw, parts)
	if err != nil {
		return nil, fmt.Errorf("merging the partials: %w", err)
	}
	final, err := combine.Finalize(merged)
	if err != nil {
		return nil, fmt.Errorf("finalizing the child in-app: %w", err)
	}

	// The second verification, on the bytes rather than on the packet. The child
	// that came back is not the artifact that will be broadcast until it has been
	// extracted, and only the extracted form can be checked with every witness
	// present.
	view, err := final.View()
	if err != nil {
		return nil, err
	}
	recheck, err := Recheck(view, exp, child.VsizeVB)
	res.Rechecked = recheck
	if err != nil {
		return nil, fmt.Errorf("verifying the signed child: %w", err)
	}
	fmt.Fprint(d.Out, recheck.Report("what came back"))
	if !recheck.OK() {
		return nil, errors.New("the signed child does not match what was verified " +
			"before it went out")
	}
	if final.TxID != child.TxID {
		return nil, fmt.Errorf("the child's txid moved from %s to %s while it was "+
			"being signed. Only signatures were added, so this should not be "+
			"possible — do not broadcast it", child.TxID, final.TxID)
	}

	// The only pre-flight there is, and specifically not a broadcast.
	ok, why, _, err := d.Node.TestMempoolAccept(ctx, hex.EncodeToString(final.RawTx))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("testmempoolaccept would refuse this child: %s\n"+
			"It validates without relaying, so nothing went out. The batch is "+
			"untouched", why)
	}
	fmt.Fprintf(d.Out, "\ntestmempoolaccept: allowed. %s in fees, %d vB, %.0f sat/vB "+
		"for the child; %.2f sat/vB for the pair.\n", prose.Sats(final.FeeSat),
		final.Vsize, float64(final.FeeSat)/float64(final.Vsize), recheck.PackageRate)

	if err := d.Journal.RecordBumpRawTx(ctx, runID, seq, final.TxID,
		hex.EncodeToString(final.RawTx)); err != nil {
		return nil, fmt.Errorf("journalling the finalized child: %w", err)
	}

	return &Signed{
		RunID:       runID,
		Seq:         seq,
		TxID:        final.TxID,
		VsizeVB:     final.Vsize,
		FeeSat:      final.FeeSat,
		PackageRate: recheck.PackageRate,
		Signers:     final.Signers,
		rawTx:       final.RawTx,
	}, nil
}

// estimateChildVsize sizes the child before Core has built one, from the change
// output's own scripts.
//
// The same estimate internal/plan used when it decided this change output was
// big enough, and the same one internal/settle falls back to when Core refuses
// outright. Two figures arrived at the same way are comparable, which is the
// only reason it is worth computing here rather than waiting for the build.
// Zero means it could not be worked out, and the checks below then decline to
// guess — Verify still runs on the built child, before any device is asked.
func estimateChildVsize(l *Located) int64 {
	n, err := plan.ChildVsize(l.Change.ScriptPubKey, l.Change.WitnessScript)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// checkCeiling refuses a lift the node could not broadcast, before a device is
// asked for anything.
//
// See MaxChildFeeRateSatPerVB for why a CPFP child meets this ceiling and
// nothing else in the build does.
func checkCeiling(l *Located, target float64, childVsize int64) error {
	if childVsize <= 0 {
		return nil
	}
	fee := plan.ChildFeeSat(l.Parent.VsizeVB, l.Parent.FeeSat, target, childVsize)
	rate := float64(fee) / float64(childVsize)
	if rate <= MaxChildFeeRateSatPerVB {
		return nil
	}
	return fmt.Errorf("this lift needs a child paying about %.0f sat/vB of its "+
		"own, above the %d sat/vB ceiling this node will broadcast.\n"+
		"A %d vB child has to carry the whole package's lift: %s to move %d vB of "+
		"parent from %.2f to %.2f sat/vB.\n%s",
		rate, MaxChildFeeRateSatPerVB, childVsize, prose.Sats(fee),
		l.Parent.VsizeVB, l.Parent.Rate(), target, ceilingDetail(rate))
}

// checkReplacement is BIP-125 rule 3, applied before a cold wallet comes out.
//
// A replacement has to pay more *absolute* fee than the transaction it replaces,
// not merely a higher rate — and on a second lift the two are easy to confuse,
// because the operator is thinking in package sat/vB while the network is
// comparing two absolute figures. Core refuses a cheaper replacement with
// "insufficient fee", which names neither number.
//
// Rule 4 is in here too: the replacement must also pay for its own bandwidth at
// the incremental relay rate, so the fee has to rise by at least
// childVsize * incrementalRelayFee on top of matching. One sat/vB is Core's
// default -incrementalrelayfee and this uses it as a floor rather than asking,
// because asking would make the refusal depend on a node setting the operator
// cannot see in the message.
//
// This is a refusal rather than a warning for the same reason everything else
// here is: the only purpose of the transaction is to reach a rate, and a
// signing round that ends in "insufficient fee" has spent a cold-wallet session
// on nothing.
func checkReplacement(l *Located, target float64, childVsize int64) error {
	if l.Replaces == nil {
		return nil
	}
	if childVsize <= 0 || l.StandingFeeSat <= 0 {
		return nil
	}
	fee := plan.ChildFeeSat(l.Parent.VsizeVB, l.Parent.FeeSat, target, childVsize)
	need := l.StandingFeeSat + childVsize*IncrementalRelaySatPerVB
	if fee >= need {
		return nil
	}

	// What target *would* clear the bar, so the refusal is actionable. Invert
	// ChildFeeSat: fee = ceil(target * (parent + child)) - parentFee.
	enough := float64(need+l.Parent.FeeSat) / float64(l.Parent.VsizeVB+childVsize)
	return fmt.Errorf("%w: bump %d of this run is in the mempool paying %s, and a "+
		"%.2f sat/vB lift would pay %s.\n"+
		"A replacement has to beat the standing transaction's *absolute* fee, not "+
		"just its rate — BIP-125 rule 3 — and pay for its own bandwidth on top, "+
		"which for a %d vB child is another %s. So it needs %s, or about "+
		"%.2f sat/vB on the package.\n"+
		"The child already out there is not lost by asking: it stays in the "+
		"mempool, and the batch keeps whatever acceleration it already has",
		ErrCheaperThanStanding, l.Replaces.Seq, prose.Sats(l.StandingFeeSat),
		target, prose.Sats(fee), childVsize,
		prose.Sats(childVsize*IncrementalRelaySatPerVB), prose.Sats(need), enough)
}

// giveUp releases the coin lock the build took, and says what it freed.
//
// The whole of a bump's teardown. Nothing was broadcast, no peer was spoken to,
// and no channel exists — so the only thing that outlives a failed bump is a
// cold wallet quietly declining to spend its own change.
func giveUp(ctx context.Context, d Deps, runID string, seq int64) *abort.Report {
	// A fresh context: the lock has to come off even when the run was cancelled,
	// and a cancelled context cannot make the RPC.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()

	rep, err := d.Journal.AbandonBump(ctx, d.Wallet, runID, seq)
	fmt.Fprint(d.Out, gaveUp(rep, err))
	return rep
}

// chainName asks Core which chain it is on, so the verifier decodes addresses
// against the right parameters.
func chainName(ctx context.Context, d Deps) (string, error) {
	info, err := d.Node.GetBlockchainInfo(ctx)
	if err != nil {
		return "", fmt.Errorf("asking Core which chain it is on: %w", err)
	}
	if _, err := plan.Params(info.Chain); err != nil {
		return "", fmt.Errorf("Core says it is on %q: %w", info.Chain, err)
	}
	return info.Chain, nil
}

func satsOf(btc float64) int64 { return int64(math.Round(btc * 1e8)) }

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
