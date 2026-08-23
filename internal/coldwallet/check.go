package coldwallet

import (
	"context"
	"fmt"

	"github.com/AusDavo/winthistle/internal/bitcoind"
)

// Derived is one address the operator is asked to compare.
type Derived struct {
	Index   int
	Address string

	// Mine and Solvable are Core's own answers about the address. They do not
	// establish that the descriptor is the right one — a wallet holding the
	// wrong descriptor recognises its own wrong addresses perfectly well — but
	// they do establish that the address on screen came out of the wallet rather
	// than out of this process's arithmetic.
	Mine     bool
	Solvable bool

	// FromImported is whether Core attributes the address to the descriptor that
	// landed in the wallet, via getaddressinfo's parent_desc. Because each branch
	// is checked against its own descriptor, this also establishes that the
	// address came from the branch it was derived on.
	//
	// It is information and NOT a gate, and the reason is measured. parent_desc
	// is singular, and a wallet can hold two descriptors that derive the same
	// address — which is precisely what a corrected setup leaves behind, since
	// Core cannot remove a descriptor and sortedmulti and multi agree at about
	// half of all indices. Core then attributes the address to one of them, and
	// on Core 29 that is the one whose key manager was created first, active or
	// not. Observed on the harness: a wallet that held a rejected multi() pair
	// and then imported the correct sortedmulti() pair reported three of five
	// receive addresses as belonging to the rejected descriptor — on a pair that
	// was entirely correct, and whose addresses the real cold wallet owns.
	//
	// So a false FromImported means "another descriptor in this wallet derives
	// this address too", which is worth saying and is not a reason to withhold
	// the comparison.
	//
	// Note what is deliberately not used here: getaddressinfo's ischange. It
	// looks like the read-back of the import's internal flag and is not. Core's
	// IsChange means "an output of ours with no address-book entry", so it is
	// true for any address that has never received — including every unused
	// address on the *external* branch, and false for an internal one that has.
	// Observed on the harness: receive addresses 0-3 report ischange=false
	// because the cold wallet's coins landed on them, and receive address 4
	// reports ischange=true because nothing ever did. The authoritative read-back
	// of the internal flag is listdescriptors, which Confirm checks.
	FromImported bool

	// Parent is the descriptor Core named, whichever it was. It is what turns
	// "this belongs to something else" into a line an operator can act on.
	Parent string
}

// AddressCheck is the round trip: the first few addresses of each branch,
// derived from the descriptors the wallet actually holds.
type AddressCheck struct {
	WalletName string
	Receive    []Derived
	Change     []Derived

	// ReceiveDesc and ChangeDesc are the descriptors these came from, as the
	// wallet reports them. Shown alongside the addresses because an operator
	// comparing descriptors is doing a different, weaker check than one
	// comparing addresses, and it is worth being able to do both.
	ReceiveDesc string
	ChangeDesc  string
}

// Recognised reports whether the wallet claims every derived address as its own
// and knows how to spend it.
//
// This is the property that has to hold before the comparison is worth making,
// and it is the only one. An address the wallet disowns is not part of the wallet
// a batch would be built from — the ordinary cause is an imported range that does
// not reach the index being shown, which Config.Validate refuses — and an
// operator who answered "they match" to a screen of addresses the wallet does not
// hold would have confirmed nothing.
//
// It deliberately says nothing about *which* descriptor the addresses came from.
// See Derived.FromImported.
func (a AddressCheck) Recognised() bool {
	for _, d := range a.all() {
		if !d.Mine || !d.Solvable {
			return false
		}
	}
	return len(a.Receive) > 0 && len(a.Change) > 0
}

// Attributed reports whether Core credits every address to the descriptor it was
// derived from.
//
// False is not a fault. It means some other descriptor in this wallet derives the
// same address, which is the state a corrected setup leaves behind — see
// Derived.FromImported for the measurement. Worth reporting, never worth refusing
// on.
func (a AddressCheck) Attributed() bool {
	for _, d := range a.all() {
		if !d.FromImported {
			return false
		}
	}
	return len(a.Receive) > 0 && len(a.Change) > 0
}

// Consistent is Recognised and Attributed together: the wallet holds every
// address and credits each to the descriptor it came from.
//
// Still an internal-consistency check and nothing more. It cannot tell a
// sortedmulti wallet from a multi one, or the right derivation path from a
// wrong one — both are wholly self-consistent. Only the operator's comparison
// against their own wallet software can.
func (a AddressCheck) Consistent() bool { return a.Recognised() && a.Attributed() }

// Elsewhere are the other descriptors in this wallet that derive an address the
// operator is being shown, deduplicated and in the order they were met.
func (a AddressCheck) Elsewhere() []string {
	var out []string
	seen := map[string]bool{}
	for _, d := range a.all() {
		if d.FromImported || d.Parent == "" || seen[d.Parent] {
			continue
		}
		seen[d.Parent] = true
		out = append(out, d.Parent)
	}
	return out
}

func (a AddressCheck) all() []Derived {
	return append(append([]Derived{}, a.Receive...), a.Change...)
}

// DeriveCheck builds the round-trip check.
//
// It derives from the descriptors listdescriptors reports, not from the ones
// that were submitted. That is the whole point of calling it a round trip: if
// Core normalised, truncated or reordered anything on the way in, the addresses
// shown to the operator are the ones the wallet will actually use.
//
// deriveaddresses is used rather than getnewaddress because it has no side
// effect. Asking twice cannot advance a keypool, and the check can therefore be
// re-run as often as the operator likes while they find their hardware.
func DeriveCheck(ctx context.Context, node, wallet *bitcoind.Client,
	l Landed, sample int) (AddressCheck, error) {

	if sample <= 0 {
		sample = DefaultSampleSize
	}
	a := AddressCheck{
		WalletName:  l.Info.Name,
		ReceiveDesc: l.Receive.Desc,
		ChangeDesc:  l.Change.Desc,
	}

	for _, branch := range []struct {
		desc string
		into *[]Derived
		what string
	}{
		{l.Receive.Desc, &a.Receive, "receive"},
		{l.Change.Desc, &a.Change, "change"},
	} {
		addrs, err := node.DeriveAddresses(ctx, branch.desc, 0, sample-1)
		if err != nil {
			return a, fmt.Errorf("deriving %s addresses from the wallet's own "+
				"descriptor: %w", branch.what, err)
		}
		for i, addr := range addrs {
			d := Derived{Index: i, Address: addr}
			info, err := wallet.GetAddressInfo(ctx, addr)
			if err != nil {
				return a, fmt.Errorf("asking the wallet about %s address %d (%s): %w",
					branch.what, i, addr, err)
			}
			d.Mine, d.Solvable = info.IsMine, info.Solvable
			for _, parent := range info.Parents() {
				if parent == branch.desc {
					d.FromImported = true
				} else if d.Parent == "" {
					d.Parent = parent
				}
			}
			*branch.into = append(*branch.into, d)
		}
	}
	return a, nil
}
