package arm

import (
	"errors"
	"testing"

	"github.com/AusDavo/winthistle/internal/reserve"
)

// The reserve is worked out from *announced* channels, so the count Phase 0
// hands it is not n. Both directions of that are here: what BatchOf counts from
// the plan, and what Streams counts once LND has the streams.
func TestBatchOfCountsAnnouncedMembers(t *testing.T) {
	cases := []struct {
		name  string
		chans []Channel
		want  reserve.Batch
	}{
		{"all public", []Channel{{}, {}, {}}, reserve.Batch{Public: 3}},
		{"all private", []Channel{{Private: true}, {Private: true}},
			reserve.Batch{Private: 2}},
		{"mixed", []Channel{{}, {Private: true}, {}},
			reserve.Batch{Public: 2, Private: 1}},
		{"none", nil, reserve.Batch{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := BatchOf(tc.chans); got != tc.want {
				t.Errorf("BatchOf = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestStreamsCountTheSameWayThePlanDid(t *testing.T) {
	chans := []Channel{{}, {Private: true}, {}}
	s := &Streams{All: []*Stream{{}, {Private: true}, {}}}

	if s.PublicCount() != 2 {
		t.Errorf("PublicCount = %d, want 2", s.PublicCount())
	}
	if got, want := s.Batch(), BatchOf(chans); got != want {
		t.Fatalf("the streams count %+v and the plan counted %+v", got, want)
	}
}

// A Phase 0 finding is about a particular count of announced channels. If the
// batch changed shape between the check and the arm, both of the figures the
// finding turns on are the wrong ones — and the armed window is the worst place
// to be reading a stale number.
func TestAFindingDoesNotApplyToADifferentBatch(t *testing.T) {
	f := reserve.Finding{Batch: reserve.Batch{Public: 2, Private: 1}}

	if err := f.StillApplies(reserve.Batch{Public: 2, Private: 1}); err != nil {
		t.Fatalf("the finding refused its own batch: %v", err)
	}
	for _, changed := range []reserve.Batch{
		{Public: 3},             // the private member was announced after all
		{Public: 1, Private: 1}, // a peer was dropped
		{Public: 2},             // the private member was dropped
		{Public: 2, Private: 2}, // one was added
	} {
		err := f.StillApplies(changed)
		if err == nil {
			t.Errorf("a finding for %+v was accepted for %+v", f.Batch, changed)
			continue
		}
		if !errors.Is(err, reserve.ErrBatchChanged) {
			t.Errorf("wrong sentinel for %+v: %v", changed, err)
		}
	}
}
