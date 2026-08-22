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
	if len(r.Landed.Others) > 0 {
		b.WriteString("\n")
		b.WriteString(prose.Para(fmt.Sprintf(
			"This wallet already held %d other descriptor(s). Nothing was removed. "+
				"Coin selection will see their coins too, so a wallet that was not "+
				"created for this purpose is worth looking at before it funds a batch:",
			len(r.Landed.Others))))
		for _, d := range r.Landed.Others {
			b.WriteString(prose.Bullet(d.Desc))
		}
	}

	b.WriteString("\n")
	b.WriteString(r.Check.Report())
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

	if !a.Consistent() {
		b.WriteString("\n")
		b.WriteString(prose.Para("Core does not recognise every address above as its " +
			"own, which it should before you compare anything. Something is wrong " +
			"with the import rather than with the descriptor — the detail is on each " +
			"line."))
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
			flags = append(flags, "it belongs to a different descriptor in this wallet")
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
func concerns(cs []Concern, fatal bool) string {
	var b strings.Builder
	var n int
	for _, c := range cs {
		if c.Fatal != fatal {
			continue
		}
		n++
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
