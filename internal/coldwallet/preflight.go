package coldwallet

import (
	"context"
	"fmt"
	"time"

	"github.com/AusDavo/winthistle/internal/bitcoind"
)

// MinCoreVersion is the floor from the design's prerequisites: Core 25.
//
// getnetworkinfo reports the version as MMmmrr00 — 250000 is 25.0, 290000 is
// 29.0. Descriptor wallets are older than this, but 25 is where the descriptor
// wallet path is the default and the surrounding RPCs behave the way this code
// expects them to.
const MinCoreVersion = 250000

// Reason is why directed mode is or is not available.
type Reason int

const (
	// Ready: Core can serve directed mode.
	Ready Reason = iota

	// NoWalletSupport: Core was built with --disable-wallet, so there is no
	// wallet to import descriptors into.
	NoWalletSupport

	// TooOld: below MinCoreVersion.
	TooOld

	// StillSyncing: Core is in initial block download. A rescan against a chain
	// that is still arriving finds whatever has arrived, silently.
	StillSyncing

	// PrunedPastBirthday: the blocks the rescan needs are gone. This is the one
	// the design singles out — Umbrel and friends prune by default, and this is
	// where an otherwise well-run node cannot do directed mode.
	PrunedPastBirthday
)

func (r Reason) String() string {
	switch r {
	case Ready:
		return "ready"
	case NoWalletSupport:
		return "no wallet support"
	case TooOld:
		return "core too old"
	case StillSyncing:
		return "still syncing"
	case PrunedPastBirthday:
		return "pruned past the birthday"
	default:
		return "unknown"
	}
}

// Preflight is what was learned about the node before anything was created.
type Preflight struct {
	Version       int
	SubVersion    string
	Chain         string
	Height        int64
	WalletSupport bool

	InitialBlockDownload bool
	VerificationProgress float64

	Pruned      bool
	PruneHeight int64

	// PruneHorizon is the header time of the oldest complete block Core still
	// holds. Core reports the horizon as a height and the operator knows the
	// birthday as a date; this is the only honest way to put them side by side.
	PruneHorizon time.Time

	Birthday time.Time

	Reason Reason
}

// Available reports whether directed mode can be set up on this node.
func (p Preflight) Available() bool { return p.Reason == Ready }

// RunPreflight asks Core whether it can serve directed mode for this birthday.
//
// It runs before the wallet is created, because every failure it can find is one
// that would otherwise surface partway through an import — or, worse, not at
// all: a pruned node imports a descriptor happily and simply finds nothing.
func RunPreflight(ctx context.Context, node *bitcoind.Client, cfg Config) (Preflight, error) {
	p := Preflight{Birthday: cfg.Birthday}

	net, err := node.GetNetworkInfo(ctx)
	if err != nil {
		return p, fmt.Errorf("asking Core its version: %w", err)
	}
	p.Version, p.SubVersion = net.Version, net.SubVersion

	chain, err := node.GetBlockchainInfo(ctx)
	if err != nil {
		return p, fmt.Errorf("asking Core about the chain: %w", err)
	}
	p.Chain, p.Height = chain.Chain, chain.Blocks
	p.InitialBlockDownload = chain.InitialBlockDownload
	p.VerificationProgress = chain.VerificationProgress
	p.Pruned, p.PruneHeight = chain.Pruned, chain.PruneHeight

	// listwallets is the wallet-support probe: a Core built with
	// --disable-wallet does not register the method, so this comes back as
	// -32601 rather than as an empty list.
	if _, err := node.ListWallets(ctx); err != nil {
		if bitcoind.IsRPCError(err, bitcoind.ErrMethodNotFound) {
			p.Reason = NoWalletSupport
			return p, nil
		}
		return p, fmt.Errorf("asking Core which wallets it has loaded: %w", err)
	}
	p.WalletSupport = true

	if p.Version < MinCoreVersion {
		p.Reason = TooOld
		return p, nil
	}
	if p.InitialBlockDownload {
		p.Reason = StillSyncing
		return p, nil
	}

	if p.Pruned {
		// getblockheader works for a pruned block — Core keeps every header —
		// so the horizon can be dated even though the block itself is gone.
		horizon, err := node.BlockTime(ctx, p.PruneHeight)
		if err != nil {
			return p, fmt.Errorf("dating Core's prune horizon at height %d: %w",
				p.PruneHeight, err)
		}
		p.PruneHorizon = horizon
		if horizon.After(cfg.Birthday) {
			p.Reason = PrunedPastBirthday
			return p, nil
		}
	}

	p.Reason = Ready
	return p, nil
}

// Summary is the one line a log or an error wants.
func (p Preflight) Summary() string {
	switch p.Reason {
	case NoWalletSupport:
		return "this Core node was built with --disable-wallet, so it has no wallet " +
			"to import descriptors into"
	case TooOld:
		return fmt.Sprintf("Core %s is below the %s this build needs",
			version(p.Version), version(MinCoreVersion))
	case StillSyncing:
		return fmt.Sprintf("Core is still in initial block download (%.1f%% verified)",
			p.VerificationProgress*100)
	case PrunedPastBirthday:
		return fmt.Sprintf("Core is pruned to block %d, dated %s — later than the cold "+
			"wallet's birthday of %s, so the blocks a descriptor import would have to "+
			"rescan are gone", p.PruneHeight, p.PruneHorizon.Format("2006-01-02"),
			p.Birthday.UTC().Format("2006-01-02"))
	default:
		return fmt.Sprintf("Core %s on %s, %d blocks, unpruned enough for a birthday of %s",
			version(p.Version), p.Chain, p.Height, p.Birthday.UTC().Format("2006-01-02"))
	}
}

// version renders Core's MMmmrr00 integer as a version string.
func version(v int) string {
	major, minor, rev := v/10000, (v/100)%100, v%100
	if rev == 0 {
		return fmt.Sprintf("%d.%d", major, minor)
	}
	return fmt.Sprintf("%d.%d.%d", major, minor, rev)
}
