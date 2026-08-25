package run

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/AusDavo/winthistle/internal/config"
	"github.com/AusDavo/winthistle/internal/prose"
)

// NewRunID is the journal's key for a run: sortable, and unique even if two
// start in the same second.
//
// It lived in internal/server while there were two front doors, so that a run
// started from the browser and one started from the command line sorted together
// in `winthistle recover`. There is one front door now, and the format is kept
// because an operator's journal already holds ids in it.
func NewRunID() (string, error) {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("making a run id: %w", err)
	}
	return time.Now().UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b[:]), nil
}

// BatchSummary is the batch, as it is shown before the operator agrees to open
// it.
//
// The amounts and the peers are what is being agreed to, so this renders them
// once: a second renderer of the same facts is a second thing to keep in step
// with the plan document.
func BatchSummary(b *config.Batch) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d channel%s, %s in total\n", len(b.Channels),
		prose.Plural(len(b.Channels)), prose.Sats(b.TotalSat()))
	// The peer on its own line, indented, rather than beside the amount. Two
	// spaces plus a 14-wide amount plus a 66-character pubkey is 84 characters
	// against a 78-column pane, so the old single line always soft-wrapped — on
	// the first screen an operator sees, which made a correct batch look
	// mangled. The pubkey is not abbreviated: this is the screen where it is
	// checked.
	for _, ch := range b.Channels {
		kind := ""
		if ch.Private {
			kind = "   (unannounced)"
		}
		fmt.Fprintf(&sb, "\n  %s%s\n", prose.Sats(ch.AmountSat), kind)
		fmt.Fprintf(&sb, "    %s\n", ch.Peer)
		fmt.Fprintf(&sb, "    %s\n", ch.Policy.Summary())
	}
	return sb.String()
}
