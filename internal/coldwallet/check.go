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

// Consistent reports whether Core recognises every derived address as belonging
// to the descriptor that was imported.
//
// This is an internal-consistency check and nothing more. It cannot tell a
// sortedmulti wallet from a multi one, or the right derivation path from a
// wrong one — both are wholly self-consistent. Only the operator's comparison
// against their own wallet software can.
func (a AddressCheck) Consistent() bool {
	for _, d := range append(append([]Derived{}, a.Receive...), a.Change...) {
		if !d.Mine || !d.Solvable || !d.FromImported {
			return false
		}
	}
	return len(a.Receive) > 0 && len(a.Change) > 0
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
					break
				}
			}
			*branch.into = append(*branch.into, d)
		}
	}
	return a, nil
}
