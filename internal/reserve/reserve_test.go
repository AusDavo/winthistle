package reserve

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"google.golang.org/grpc"
)

// fakeWalletKit answers the three calls the pre-flight makes and records how it
// was asked, because *how* is the part that has to match LND.
type fakeWalletKit struct {
	utxos  []int64 // amounts, sat
	leases []uint64

	// reserves maps additional_public_channels to what LND would return.
	reserves map[uint32]int64

	unspentReqs []*walletrpc.ListUnspentRequest
	reserveReqs []uint32
	leaseCalls  int
}

func (f *fakeWalletKit) ListUnspent(_ context.Context, req *walletrpc.ListUnspentRequest,
	_ ...grpc.CallOption) (*walletrpc.ListUnspentResponse, error) {

	f.unspentReqs = append(f.unspentReqs, req)
	resp := &walletrpc.ListUnspentResponse{}
	for _, amt := range f.utxos {
		resp.Utxos = append(resp.Utxos, &lnrpc.Utxo{AmountSat: amt})
	}
	return resp, nil
}

func (f *fakeWalletKit) ListLeases(_ context.Context, _ *walletrpc.ListLeasesRequest,
	_ ...grpc.CallOption) (*walletrpc.ListLeasesResponse, error) {

	f.leaseCalls++
	resp := &walletrpc.ListLeasesResponse{}
	for _, v := range f.leases {
		resp.LockedUtxos = append(resp.LockedUtxos, &walletrpc.UtxoLease{Value: v})
	}
	return resp, nil
}

func (f *fakeWalletKit) RequiredReserve(_ context.Context, req *walletrpc.RequiredReserveRequest,
	_ ...grpc.CallOption) (*walletrpc.RequiredReserveResponse, error) {

	f.reserveReqs = append(f.reserveReqs, req.GetAdditionalPublicChannels())
	return &walletrpc.RequiredReserveResponse{
		RequiredReserve: f.reserves[req.GetAdditionalPublicChannels()],
	}, nil
}

// anchorReserves is LND's arithmetic — 10,000 per public anchor channel, capped
// at 100,000 — over a node that already has `existing` of them. Written out here
// so the tests are about our reading of it, not about our reimplementation.
func anchorReserves(existing int, upTo int) map[uint32]int64 {
	out := map[uint32]int64{}
	for add := 0; add <= upTo; add++ {
		v := int64((existing + add) * PerAnchorChannel)
		if v > MaxReserve {
			v = MaxReserve
		}
		out[uint32(add)] = v
	}
	return out
}

// TestCheckAsksLNDTheWayLNDAsksItself pins the request shape. Every one of these
// arguments is load-bearing: a different account, a different confirmation floor,
// or a different additional-channel count would make the pre-flight predict a
// verdict LND is not going to reach.
func TestCheckAsksLNDTheWayLNDAsksItself(t *testing.T) {
	cli := &fakeWalletKit{
		utxos:    []int64{100_000, 100_000},
		reserves: anchorReserves(2, 3),
	}
	f, err := Check(context.Background(), cli, Batch{Public: 3})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}

	if len(cli.unspentReqs) != 1 {
		t.Fatalf("ListUnspent called %d times, want 1", len(cli.unspentReqs))
	}
	req := cli.unspentReqs[0]
	if req.GetAccount() != "default" {
		t.Errorf("account = %q, want %q — CheckReservedValue reads only the "+
			"default account", req.GetAccount(), "default")
	}
	if req.GetMinConfs() != 0 {
		t.Errorf("min_confs = %d, want 0 — ListUnspentWitnessFromDefaultAccount "+
			"is called with 0, so an unconfirmed top-up already counts",
			req.GetMinConfs())
	}
	if req.GetMaxConfs() != math.MaxInt32 {
		t.Errorf("max_confs = %d, want MaxInt32", req.GetMaxConfs())
	}
	if req.GetUnconfirmedOnly() {
		t.Error("unconfirmed_only is set")
	}

	// 0 for the node as it stands, 1 for what verify will demand, 3 for the
	// batch once it is pending. Nothing else.
	want := map[uint32]bool{0: true, 1: true, 3: true}
	for _, got := range cli.reserveReqs {
		if !want[got] {
			t.Errorf("RequiredReserve asked for additional=%d, which is not a "+
				"figure this check has a use for", got)
		}
		delete(want, got)
	}
	if len(want) > 0 {
		t.Errorf("RequiredReserve was never asked for %v", want)
	}

	if f.Available != 200_000 {
		t.Errorf("Available = %d, want 200000", f.Available)
	}
	if f.NowRequired != 20_000 || f.AtVerify != 30_000 || f.AfterBatch != 50_000 {
		t.Errorf("figures = now %d, verify %d, after %d; want 20000/30000/50000",
			f.NowRequired, f.AtVerify, f.AfterBatch)
	}
}

// TestCheckMemoisesTheSingleChannelCase: for a batch of one, additional=1 is both
// the verify figure and the post-batch figure, and asking twice is waste.
func TestCheckMemoisesTheSingleChannelCase(t *testing.T) {
	cli := &fakeWalletKit{utxos: []int64{1_000_000}, reserves: anchorReserves(1, 1)}
	if _, err := Check(context.Background(), cli, Batch{Public: 1}); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(cli.reserveReqs) != 2 {
		t.Errorf("RequiredReserve called %d times for a batch of one, want 2: %v",
			len(cli.reserveReqs), cli.reserveReqs)
	}
}

func TestCheckCountsOnlyUnlockedCoins(t *testing.T) {
	// The leases are reported but never added: LND's own balance comes from
	// ListUnspentWitness, which skips a leased outpoint entirely.
	cli := &fakeWalletKit{
		utxos:    []int64{},
		leases:   []uint64{500_000_000},
		reserves: anchorReserves(0, 1),
	}
	f, err := Check(context.Background(), cli, Batch{Public: 1})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if f.Available != 0 {
		t.Errorf("Available = %d, want 0 — a leased coin must not count", f.Available)
	}
	if f.Leased != 500_000_000 {
		t.Errorf("Leased = %d, want 500000000", f.Leased)
	}
	if f.Verdict() != WouldBeRefused {
		t.Errorf("verdict = %s, want %s: 5 BTC in the wallet, all of it leased, "+
			"is exactly the state that produced this finding on the harness",
			f.Verdict(), WouldBeRefused)
	}
}

func TestCheckRejectsANonBatch(t *testing.T) {
	cli := &fakeWalletKit{reserves: anchorReserves(0, 1)}
	for _, b := range []Batch{{}, {Public: -1}, {Private: -2}} {
		if _, err := Check(context.Background(), cli, b); err == nil {
			t.Errorf("Check(%+v) returned no error", b)
		}
	}
}

func TestVerdicts(t *testing.T) {
	cases := []struct {
		name string
		f    Finding
		want Verdict
	}{{
		name: "clear",
		f:    Finding{Batch: Batch{Public: 3}, Available: 60_000, AtVerify: 30_000, AfterBatch: 50_000},
		want: Clear,
	}, {
		name: "exactly the post-batch figure is enough",
		f:    Finding{Batch: Batch{Public: 3}, Available: 50_000, AtVerify: 30_000, AfterBatch: 50_000},
		want: Clear,
	}, {
		name: "short only afterwards",
		f:    Finding{Batch: Batch{Public: 3}, Available: 49_999, AtVerify: 30_000, AfterBatch: 50_000},
		want: ShortAfterBatch,
	}, {
		name: "exactly the verify figure passes verify",
		f:    Finding{Batch: Batch{Public: 3}, Available: 30_000, AtVerify: 30_000, AfterBatch: 50_000},
		want: ShortAfterBatch,
	}, {
		name: "one satoshi under the verify figure is a refusal",
		f:    Finding{Batch: Batch{Public: 3}, Available: 29_999, AtVerify: 30_000, AfterBatch: 50_000},
		want: WouldBeRefused,
	}, {
		name: "an all-private batch is not checked at all",
		f:    Finding{Batch: Batch{Private: 3}, Available: 0, AtVerify: 30_000, AfterBatch: 0},
		want: NotApplicable,
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.f.Verdict(); got != c.want {
				t.Errorf("verdict = %s, want %s", got, c.want)
			}
			if c.f.Blocking() != (c.want == WouldBeRefused) {
				t.Errorf("Blocking() = %v for verdict %s", c.f.Blocking(), got(c.f))
			}
		})
	}
}

func got(f Finding) Verdict { return f.Verdict() }

func TestShortfalls(t *testing.T) {
	f := Finding{Batch: Batch{Public: 3}, Available: 12_000, AtVerify: 30_000, AfterBatch: 50_000}
	if f.ShortfallAtVerify() != 18_000 {
		t.Errorf("ShortfallAtVerify() = %d, want 18000", f.ShortfallAtVerify())
	}
	if f.ShortfallAfterBatch() != 38_000 {
		t.Errorf("ShortfallAfterBatch() = %d, want 38000", f.ShortfallAfterBatch())
	}

	clear := Finding{Batch: Batch{Public: 1}, Available: 99_000, AtVerify: 10_000, AfterBatch: 10_000}
	if clear.ShortfallAtVerify() != 0 || clear.ShortfallAfterBatch() != 0 {
		t.Errorf("a covered wallet reported a shortfall: %d / %d",
			clear.ShortfallAtVerify(), clear.ShortfallAfterBatch())
	}
}

// TestTheRefusalReportSaysWhatHappenedAndWhatToDo is a test on the copy, because
// the copy is the deliverable. LND's own wording is the thing being replaced, and
// each of these is a specific way the operator would otherwise be misled.
func TestTheRefusalReportSaysWhatHappenedAndWhatToDo(t *testing.T) {
	f := Finding{
		Batch: Batch{Public: 3}, Available: 0, Leased: 500_000_000,
		NowRequired: 20_000, AtVerify: 30_000, AfterBatch: 50_000,
	}
	report := f.Report()

	mustContain := map[string]string{
		"this node's own on-chain wallet": "own on-chain wallet",
		"the figure LND will demand":      "30,000 sat",
		"the shortfall":                   "short by",
		"the post-batch figure":           "50,000 sat",
		"the leased balance":              "500,000,000 sat",
		"where it would fail":             "step 5",
		"LND's own wording, quoted":       "reserved wallet balance invalidated",
		"that the cold wallet is fine":    "cold wallet is not the problem",
		"that a top-up counts at once":    "mempool",
		"the top-up-output remedy":        "top-up output",
	}
	for what, substr := range mustContain {
		if !strings.Contains(report, substr) {
			t.Errorf("the refusal report does not say %s (looked for %q)", what, substr)
		}
	}

	if !strings.Contains(f.Summary(), "30,000 sat") {
		t.Errorf("the one-line summary omits the shortfall: %q", f.Summary())
	}
}

func TestTheShortAfterReportDoesNotClaimTheBatchWillFail(t *testing.T) {
	f := Finding{
		Batch: Batch{Public: 3}, Available: 40_000,
		NowRequired: 20_000, AtVerify: 30_000, AfterBatch: 50_000,
	}
	report := f.Report()
	if !strings.Contains(report, "first verify will pass") {
		t.Errorf("the report does not say the batch's first verify passes:\n%s", report)
	}
	if strings.Contains(report, "Stop here") {
		t.Errorf("the report tells the operator to stop over a warning:\n%s", report)
	}
	if !strings.Contains(report, "10,000 sat") {
		t.Errorf("the report omits the post-batch shortfall:\n%s", report)
	}

	// And it must not promise the whole batch verifies, which is what this used to
	// assert. A later verify in the same batch can be judged against AfterBatch —
	// TestALaterVerifyCountsAnEarlierChannelInTheBatch measures it — so a report
	// that says the batch will verify is asserting something the program cannot
	// know.
	if !strings.Contains(report, "refused mid-batch") {
		t.Errorf("the report does not say a later verify can be refused mid-batch, "+
			"so it reads as a promise that the whole batch verifies:\n%s", report)
	}
}

// TestPrivateMembersAreAccountedForOutLoud: a batch of five reported against a
// figure for three must say why, or it reads as an off-by-two.
func TestPrivateMembersAreAccountedForOutLoud(t *testing.T) {
	f := Finding{
		Batch: Batch{Public: 3, Private: 2}, Available: 1_000_000,
		NowRequired: 20_000, AtVerify: 30_000, AfterBatch: 50_000,
	}
	report := f.Report()
	for _, want := range []string{"2 of the 5 channels", "announced channels only"} {
		if !strings.Contains(report, want) {
			t.Errorf("the report does not mention the private members (%q):\n%s", want, report)
		}
	}

	allPublic := Finding{
		Batch: Batch{Public: 3}, Available: 1_000_000,
		NowRequired: 20_000, AtVerify: 30_000, AfterBatch: 50_000,
	}
	if strings.Contains(allPublic.Report(), "private") {
		t.Errorf("a wholly public batch is told about private channels:\n%s",
			allPublic.Report())
	}
}

// TestReportsAreWrappedToAPane. Terminal copy that overruns is copy the operator
// does not read, and this is the screen it matters most on.
func TestReportsAreWrappedToAPane(t *testing.T) {
	findings := []Finding{
		{Batch: Batch{Public: 3}, Available: 0, Leased: 500_000, NowRequired: 20_000, AtVerify: 30_000, AfterBatch: 50_000},
		{Batch: Batch{Public: 3}, Available: 40_000, NowRequired: 20_000, AtVerify: 30_000, AfterBatch: 50_000},
		{Batch: Batch{Public: 3, Private: 1}, Available: 9_000_000, NowRequired: 20_000, AtVerify: 30_000, AfterBatch: 50_000},
		{Batch: Batch{Private: 2}, Available: 9_000_000, NowRequired: 20_000, AtVerify: 30_000, AfterBatch: 20_000},
	}
	for _, f := range findings {
		for i, line := range strings.Split(f.Report(), "\n") {
			// Runes, not bytes: the reports contain em dashes.
			if n := len([]rune(line)); n > 80 {
				t.Errorf("%s report, line %d is %d columns wide:\n%s",
					f.Verdict(), i+1, n, line)
			}
		}
	}
}
