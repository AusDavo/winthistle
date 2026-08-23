package coldwallet

import (
	"fmt"
	"strings"
	"time"

	"github.com/AusDavo/winthistle/internal/prose"
)

// Report is the operator-facing text for a pre-flight that failed.
//
// The pruned case is the one that carries weight. It is not a misconfiguration
// and there is nothing to fix on this node short of re-downloading the chain, so
// the copy has to say that plainly and then point at the mode that does work
// rather than leaving the operator to discover it.
func (p Preflight) Report() string {
	var b strings.Builder
	switch p.Reason {
	case PrunedPastBirthday:
		b.WriteString("Directed mode is not available on this node.\n\n")
		b.WriteString(prose.Para(fmt.Sprintf(
			"Core is pruned to block %d, and that block is dated %s. The cold "+
				"wallet's birthday is %s, so the blocks a descriptor import would "+
				"have to rescan have been deleted. Core would import the descriptors "+
				"without complaint and then report a zero balance, which is the "+
				"failure this check exists to get ahead of.",
			p.PruneHeight, p.PruneHorizon.Format("2 January 2006"),
			p.Birthday.UTC().Format("2 January 2006"))))
		b.WriteString("\nWhat to do:\n")
		b.WriteString(prose.Bullet("Use assisted mode. Sparrow builds the transaction " +
			"and this app verifies it output by output. All four invariants hold " +
			"either way; what is lost is coin control, exact fee rates and " +
			"testmempoolaccept."))
		b.WriteString(prose.Bullet("Or turn pruning off and reindex, which means " +
			"re-downloading and re-verifying the chain. That is hours to days, and " +
			"it is a decision about this node rather than about this batch."))
	case NoWalletSupport:
		b.WriteString("Directed mode is not available on this node.\n\n")
		b.WriteString(prose.Para("This Core was built with --disable-wallet, so it has " +
			"no wallet for the cold storage descriptors to live in. Assisted mode " +
			"needs nothing beyond LND and is the way forward here."))
	case TooOld:
		b.WriteString("Directed mode is not available on this node.\n\n")
		b.WriteString(prose.Para(fmt.Sprintf(
			"Core %s is below the %s this build needs for descriptor wallets. "+
				"Upgrading Core is safe and does not touch the wallet; assisted mode "+
				"works in the meantime.",
			version(p.Version), version(MinCoreVersion))))
	case StillSyncing:
		b.WriteString("Not yet — Core is still catching up.\n\n")
		b.WriteString(prose.Para(fmt.Sprintf(
			"Core is in initial block download, %.1f%% verified at block %d. A "+
				"rescan now would search the part of the chain that has arrived and "+
				"report what it found as if that were everything. Wait for the sync "+
				"and run this again.",
			p.VerificationProgress*100, p.Height)))
	default:
		b.WriteString(prose.Para(p.Summary()))
	}
	return b.String()
}

// Report is the operator-facing text for a completed setup.
//
// It ends in a question, not in a success message, and that is the whole design
// of this screen: nothing the app can check distinguishes a correct descriptor
// from a plausible wrong one. See the package comment.
func (r *Result) Report() string {
	var b strings.Builder

	switch r.Verdict() {
	case Refused:
		b.WriteString("The watch-only wallet was not created.\n\n")
		b.WriteString(concerns(r.Concerns, true))
		return b.String()
	case Unavailable:
		return r.Preflight.Report()
	}

	b.WriteString(prose.Para(fmt.Sprintf("The watch-only wallet %q is built and "+
		"holds both descriptors.", r.Landed.Info.Name)))
	b.WriteString("\n")

	b.WriteString(field("wallet", r.Landed.Info.Name))
	b.WriteString(field("private keys", "disabled"))
	b.WriteString(field("birthday", birthday(r.Config)))
	b.WriteString(field("rescanned from", rescan(r.Config, r.Landed)))
	b.WriteString(field("range", fmt.Sprintf("[0, %d]", r.Config.gapLimit())))
	if n := len(r.Landed.Others); n > 0 {
		b.WriteString(field("other descriptors", fmt.Sprintf(
			"%d already in this wallet — see below", n)))
	}

	if warn := concerns(r.Concerns, false); warn != "" {
		b.WriteString("\n")
		b.WriteString(warn)
	}
	if len(r.Warnings) > 0 {
		b.WriteString("\nCore also said:\n")
		for _, w := range r.Warnings {
			b.WriteString(prose.Bullet(w))
		}
	}
	b.WriteString(StaleWarning(r.Landed))

	b.WriteString("\n")
	b.WriteString(r.Check.Report())
	return b.String()
}

// StaleWarning is what to say about descriptors in the wallet that are not the
// pair being compared. Empty when there are none.
//
// This is the highest-stakes copy in the setup path, because it is read in
// exactly the situation setup exists to produce: the operator compared the
// addresses, found them wrong, fixed the descriptor and ran it again. Core does
// not let them undo the first attempt — there is no RPC that removes a
// descriptor from a wallet — and the wrong descriptor's coins stay selectable.
// So the copy has to say that plainly and name the way out, which is a different
// wallet rather than a repair of this one.
func StaleWarning(l Landed) string {
	stale, other := l.Stale(), l.OtherActive()
	if len(stale) == 0 && len(other) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n")

	if len(stale) > 0 {
		b.WriteString(prose.Para(fmt.Sprintf(
			"This wallet holds %d descriptor%s that %s no longer active. Core keeps "+
				"one active receive branch and one active change branch, so importing "+
				"a pair does not replace an earlier pair — it deactivates it and "+
				"leaves it here. There is no RPC that removes a descriptor from a "+
				"Core wallet.",
			len(stale), prose.Plural(len(stale)), prose.IsAre(len(stale)))))
		for _, d := range stale {
			b.WriteString(prose.Bullet(d.Desc))
		}
		b.WriteString("\n")
		b.WriteString(prose.Para("Their coins are still in this wallet's own coin " +
			"list, so coin selection can still spend them — deactivating a " +
			"descriptor does not hide what it found. If the descriptor above is one " +
			"you rejected, this wallet's balance is now partly the wallet you meant " +
			"and partly the one you did not, and no import can separate them again."))
		b.WriteString("\n")
		b.WriteString(prose.Para(fmt.Sprintf("What to do: set up under a new wallet "+
			"name and point winthistle.toml at it. It is one line — [bitcoind] "+
			"wallet — and it costs another rescan and nothing else. Keeping %q is "+
			"only safe if you know what the inactive descriptor is and are content "+
			"for a batch to spend its coins.", l.Info.Name)))
	}

	if len(other) > 0 {
		b.WriteString("\n")
		b.WriteString(prose.Para(fmt.Sprintf(
			"It also holds %d active descriptor%s this setup did not import. Coin "+
				"selection will see their coins as well, so a wallet that was not "+
				"created for this purpose is worth reading before it funds a batch:",
			len(other), prose.Plural(len(other)))))
		for _, d := range other {
			b.WriteString(prose.Bullet(d.Desc))
		}
	}
	return b.String()
}

// Report renders the round-trip check.
func (a AddressCheck) Report() string {
	var b strings.Builder

	b.WriteString("Now check the addresses.\n\n")
	b.WriteString(prose.Para("This is the only step that can tell a correct " +
		"descriptor from a plausible wrong one. A wallet holding the wrong " +
		"descriptor imports cleanly, recognises its own wrong addresses, reports a " +
		"zero balance and explains none of it. Open your own wallet software, look " +
		"at the same wallet's first addresses, and compare them to these."))
	b.WriteString("\n")

	b.WriteString("  receive\n")
	b.WriteString(addresses(a.Receive))
	b.WriteString("\n  change\n")
	b.WriteString(addresses(a.Change))

	if !a.Recognised() {
		b.WriteString("\n")
		b.WriteString(prose.Para("Core does not recognise every address above as its " +
			"own, which it should before you compare anything. That is the import " +
			"rather than the descriptor: the usual cause is an imported range that " +
			"does not reach the index being shown. The detail is on each line."))
	}
	if elsewhere := a.Elsewhere(); len(elsewhere) > 0 {
		b.WriteString("\n")
		b.WriteString(prose.Para("Some of the addresses above are derived by another " +
			"descriptor in this wallet as well, and Core credits each address to only " +
			"one. This is what a wallet looks like after a descriptor has been " +
			"corrected — Core cannot remove the old one, and two descriptors over the " +
			"same keys agree wherever the keys were already in order. It does not " +
			"make the addresses wrong and it does not change what to compare them " +
			"against. The other descriptor is:"))
		for _, d := range elsewhere {
			b.WriteString(prose.Bullet(d))
		}
	}

	b.WriteString("\n")
	b.WriteString(prose.Para("If they match, directed mode is configured. If any of " +
		"them does not, stop: the descriptors are not the ones your cold wallet " +
		"uses, and the usual causes are a wrong derivation path, multi() where the " +
		"wallet uses sortedmulti(), two keys the wrong way round, or a truncated " +
		"xpub. Fixing it costs nothing now and cannot be fixed at all once a batch " +
		"is funded to the wrong addresses."))
	return b.String()
}

func addresses(ds []Derived) string {
	var b strings.Builder
	for _, d := range ds {
		line := fmt.Sprintf("    %d  %s", d.Index, d.Address)
		var flags []string
		if !d.Mine {
			flags = append(flags, "the wallet does not recognise this address")
		}
		if !d.Solvable {
			flags = append(flags, "the wallet cannot work out how to spend it")
		}
		if d.Mine && !d.FromImported {
			flags = append(flags, "another descriptor in this wallet derives this "+
				"address too, and Core credits it to that one")
		}
		b.WriteString(line + "\n")
		for _, f := range flags {
			b.WriteString(prose.Wrap("- "+f, "       ", "         "))
		}
	}
	return b.String()
}

func field(label, value string) string {
	return fmt.Sprintf("  %-20s %s\n", label, value)
}

func birthday(c Config) string {
	if c.timestamp() == 0 {
		return "the genesis block — the whole chain was rescanned"
	}
	return c.Birthday.UTC().Format("2 January 2006")
}

// rescan says what Core recorded, which is not always what was asked for: Core
// stores the earliest block at or before the timestamp, and clamps anything
// before the genesis block to 1.
func rescan(c Config, l Landed) string {
	got := l.Receive.Timestamp
	if got <= 1 {
		return "the genesis block"
	}
	at := time.Unix(got, 0).UTC().Format("2 January 2006")
	if got == c.timestamp() {
		return at
	}
	return fmt.Sprintf("%s — Core moved it back from %s to the block boundary",
		at, c.Birthday.UTC().Format("2 January 2006"))
}

// concerns renders the warnings, or the refusals when fatal is set.
//
// Each item leads with its subject on a line of its own. That is not decoration:
// the same concern applies to the receive and the change descriptor often enough
// that a list of two identically-worded refusals is the ordinary case — a
// multipath descriptor pasted into both fields produces exactly that — and a
// refusal an operator cannot attribute to a line of their file is a refusal they
// have to guess at.
func concerns(cs []Concern, fatal bool) string {
	var b strings.Builder
	var n int
	for _, c := range cs {
		if c.Fatal != fatal {
			continue
		}
		n++
		if c.Subject != "" {
			b.WriteString("  - " + c.Subject + "\n")
			b.WriteString(prose.Wrap(c.Headline, "      ", "      "))
			if c.Detail != "" {
				b.WriteString(prose.Wrap(c.Detail, "      ", "      "))
			}
			continue
		}
		b.WriteString(prose.Wrap(c.Headline, "  - ", "    "))
		if c.Detail != "" {
			b.WriteString(prose.Wrap(c.Detail, "    ", "    "))
		}
	}
	if n == 0 {
		return ""
	}
	head := "Worth knowing before you go on:\n"
	if fatal {
		head = "Why:\n"
	}
	return head + b.String()
}

// Report renders the coin split.
func (c Coins) Report() string {
	var b strings.Builder

	b.WriteString(prose.Table([]prose.Row{
		prose.Note("usable in this batch", c.EligibleSat,
			fmt.Sprintf("%d coin%s", len(c.Eligible), prose.Plural(len(c.Eligible)))),
		prose.Note("left out", c.ExcludedSat,
			fmt.Sprintf("%d coin%s", len(c.Excluded), prose.Plural(len(c.Excluded)))),
	}))

	if len(c.Excluded) == 0 {
		return b.String()
	}

	b.WriteString("\n")
	b.WriteString(prose.Para("These coins are not in the plan, and your wallet " +
		"software will still count them, so the two balances will disagree:"))
	for _, e := range c.Excluded {
		b.WriteString(fmt.Sprintf("  - %s  %s\n",
			prose.Sats(satFromBTC(e.Coin.Amount)), e.Coin.Outpoint()))
		b.WriteString(prose.Wrap(e.Why.String()+" — "+e.Detail, "      ", "      "))
	}

	if hasLegacy(c) {
		b.WriteString("\n")
		b.WriteString(prose.Para("The legacy ones are not a preference. LND refuses a " +
			"funding transaction with any non-SegWit input outright — " +
			"verifyAllInputsSegWit, \"risk of malleability\" — because a malleable " +
			"input is a TXID that can move after psbt_verify has committed to it, " +
			"and a moved TXID destroys every channel in the batch (I-3)."))
		b.WriteString("\n")
		b.WriteString(prose.Para("To use them, move them to a SegWit address of the " +
			"same cold wallet first, in a separate transaction, and let it confirm."))
	}
	return b.String()
}

func hasLegacy(c Coins) bool {
	for _, e := range c.Excluded {
		if e.Why == Legacy {
			return true
		}
	}
	return false
}
