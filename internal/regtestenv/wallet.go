package regtestenv

import (
	"context"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
)

// leaseID is the lock identifier the fixtures lease under.
//
// Thirty-two bytes, non-zero, and not chanfunding.LndInternalLockID — LeaseOutput
// rejects both of those. Fixed rather than random so the cleanup can release what
// it leased even if the test that took the lease has already failed.
var leaseID = []byte("winthistle-regtest-fixture-lease")

// LeaseAllUnspent leases every coin in alice's own wallet, and returns how much
// it took out of circulation.
//
// This is how the reserved-value refusal is reproduced on purpose: LND's balance
// for that check excludes leased coins, so a node with 5 BTC and every UTXO
// leased is, as far as psbt_verify is concerned, a node with nothing. The
// harness's own fragility was this same mechanism — see HANDOFF.md's coin-lock
// and lease bullets, under "The harness".
//
// The returned func gives the coins back, for a test that needs to see the node
// recover. It is idempotent and it also runs at cleanup whether the caller
// called it or not, because a leaked lease does not fail this test — it fails the
// next one, with an error about the reserve that says nothing about a lease.
func (e *Env) LeaseAllUnspent(t *testing.T) (leasedSat int64, release func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	resp, err := e.Alice.WalletKit.ListUnspent(ctx, &walletrpc.ListUnspentRequest{
		MinConfs: 0,
		MaxConfs: math.MaxInt32,
		Account:  "default",
	})
	if err != nil {
		t.Fatalf("listing alice's unspent outputs: %v", err)
	}

	var (
		leased int64
		held   []*lnrpc.OutPoint
	)
	for _, u := range resp.GetUtxos() {
		op := u.GetOutpoint()
		if _, err := e.Alice.WalletKit.LeaseOutput(ctx, &walletrpc.LeaseOutputRequest{
			Id:       leaseID,
			Outpoint: op,
			// Longer than any single test, so the lease cannot lapse halfway
			// through and turn a refusal into a pass.
			ExpirationSeconds: 3600,
		}); err != nil {
			t.Fatalf("leasing %s:%d: %v", op.GetTxidStr(), op.GetOutputIndex(), err)
		}
		leased += u.GetAmountSat()
		held = append(held, op)
	}

	var once sync.Once
	release = func() {
		once.Do(func() { e.releaseLeases(t, held) })
	}
	t.Cleanup(release)
	return leased, release
}

// releaseLeases gives the leased outpoints back.
func (e *Env) releaseLeases(t *testing.T, held []*lnrpc.OutPoint) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	for _, op := range held {
		_, err := e.Alice.WalletKit.ReleaseOutput(ctx, &walletrpc.ReleaseOutputRequest{
			Id:       leaseID,
			Outpoint: op,
		})
		if err != nil {
			// Loud, but not fatal: the cleanup of an already-failed test should
			// not hide the failure that mattered.
			t.Errorf("releasing the lease on %s:%d — the next test will see a "+
				"reserve error that has nothing to do with it: %v",
				op.GetTxidStr(), op.GetOutputIndex(), err)
		}
	}
}
