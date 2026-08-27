package run

import (
	"errors"
	"strings"
	"testing"

	"github.com/AusDavo/winthistle/internal/arm"
	"github.com/AusDavo/winthistle/internal/lnd"
)

// TestAJournalFailureNeverSwallowsTheRefusalItArrivedWith is #42's other half.
//
// When Begin genuinely must run — some streams did open — and fails, two
// different things have gone wrong and the operator needs both. The refusal from
// LND or the peer is the only sentence naming what to change; the journalling
// failure is why `winthistle recover` will not see the shims that are open.
// Returning either one alone loses a remedy.
//
// The pending channel ids are in there too, because armWindow is the last place
// that knows them: a shim outlives the stream it came on, so hanging up releases
// nothing.
func TestAJournalFailureNeverSwallowsTheRefusalItArrivedWith(t *testing.T) {
	ids := make([]lnd.PendingChanID, 0, 2)
	for range 2 {
		id, err := lnd.NewPendingChanID()
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	streams := &arm.Streams{All: []*arm.Stream{
		{PendingChanID: ids[0]}, {PendingChanID: ids[1]},
	}}

	refusal := errors.New("channel is too small, the minimum channel size is: 20000 SAT")
	jerr := errors.New("database is locked")

	got := unjournalledStreams(streams, refusal, jerr)
	text := got.Error()

	for what, want := range map[string]string{
		"the refusal it arrived with": "the minimum channel size is",
		"the journalling failure":     "database is locked",
		"that recover cannot see it":  "cannot see them",
		"the first pending chan id":   ids[0].String(),
		"the second pending chan id":  ids[1].String(),
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the error does not carry %s (looked for %q):\n%v", what, want, got)
		}
	}

	// Both causes have to survive errors.Is, not just the printed text: the run's
	// caller unwraps.
	if !errors.Is(got, refusal) {
		t.Error("the refusal does not survive unwrapping, so a caller keying on it " +
			"sees only the journalling failure")
	}
	if !errors.Is(got, jerr) {
		t.Error("the journalling failure does not survive unwrapping")
	}

	// And with no refusal to carry, it still says what is open and un-journalled.
	alone := unjournalledStreams(streams, nil, jerr).Error()
	if !strings.Contains(alone, ids[0].String()) {
		t.Errorf("a journalling failure on its own does not name the open shims:\n%s",
			alone)
	}
}
