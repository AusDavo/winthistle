package run

import (
	"bytes"
	"regexp"
	"strings"
	"testing"

	"github.com/AusDavo/winthistle/internal/arm"
	"github.com/AusDavo/winthistle/internal/lnd"
	"github.com/AusDavo/winthistle/internal/prose"
	"github.com/lightningnetwork/lnd/lnrpc"
)

// armedScreen renders reportArmed the way a run prints it. reportArmed takes an
// io.Writer, an *arm.Armed and a *prepared, and every field it reads is either
// exported or in this package, so the screen is renderable with no node — which
// is the point: it was printed into two live transcripts and asserted on by
// neither, so its sentences were rendered by nothing.
func armedScreen(t *testing.T, n int, backups int) string {
	t.Helper()

	armed := &arm.Armed{
		RunID: "20260827-101010-abc123",
		TxID:  "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90",
	}
	p := &prepared{}
	for i := 0; i < n; i++ {
		armed.Channels = append(armed.Channels, lnd.ChannelPoint{Index: uint32(i)})
		p.chans = append(p.chans, arm.Channel{
			Peer: "02" + strings.Repeat("ab", 32),
		})
	}
	if backups > 0 {
		snap := &lnrpc.ChanBackupSnapshot{
			SingleChanBackups: &lnrpc.ChannelBackups{},
			MultiChanBackup: &lnrpc.MultiChanBackup{
				MultiChanBackup: make([]byte, 4738),
			},
		}
		for i := 0; i < backups; i++ {
			snap.SingleChanBackups.ChanBackups = append(
				snap.SingleChanBackups.ChanBackups, &lnrpc.ChannelBackup{})
		}
		armed.Backup = snap
	}

	var b bytes.Buffer
	reportArmed(&b, armed, p)
	return b.String()
}

// flat joins a wrapped screen back into one line, so an assertion on a sentence
// is not an assertion on where prose.Para happened to break it.
func flat(s string) string { return strings.Join(strings.Fields(s), " ") }

// TestTheArmedScreenNamesTheNodeAsTheCustodian pins the two claims issue #39
// found inverted, and it keys on the shape of the old claim rather than on a
// bare word, because the true sentence contains most of the false one's words.
//
// LND's funderProcessFundingSigned parses commitSig from the peer's
// funding_signed (funding/manager.go:2805 at v0.21.2-beta) and hands it to this
// node's CompleteReservation at :2813, before chan_pending is emitted at :2897.
// So this node stored the peer's signature, in this node's channel database —
// which is what the backup paragraph on the same screen has always said, and
// what the screen above it denied.
func TestTheArmedScreenNamesTheNodeAsTheCustodian(t *testing.T) {
	screen := flat(armedScreen(t, 3, 3))

	// The custodian, said the way arm.go:756-757 says it.
	const want = "this node has stored the peer's commitment signature"
	if !strings.Contains(screen, want) {
		t.Errorf("the armed screen does not say %q:\n%s", want, screen)
	}

	// The inversion: the peer storing a signature is what makes the peer's own
	// side real, and it is not what a force-close from here depends on.
	inverted := regexp.MustCompile(`peer has stored its commitment`)
	if inverted.MatchString(screen) {
		t.Errorf("the armed screen still puts the commitment signature on the "+
			"peer:\n%s", screen)
	}

	// The custody claim. "even if this node vanished" says the backup being
	// exported eighteen lines below it is optional, and the backup paragraph
	// says the opposite.
	vanished := regexp.MustCompile(`even if this node\b`)
	if vanished.MatchString(screen) {
		t.Errorf("the armed screen still says the funds come back without this "+
			"node:\n%s", screen)
	}

	// The conclusion is I-1 and does not move.
	if !strings.Contains(screen, "already recoverable") ||
		!strings.Contains(screen, "force-close would get the funds back") {
		t.Errorf("the armed screen no longer says the batch is recoverable by "+
			"force-close:\n%s", screen)
	}
}

// TestTheArmedScreenStandsUpWithNoBackupParagraph is the conditionality half.
// The backup paragraph renders only when the export carried single-channel
// backups, so the sentence above it has to be true on its own — otherwise the
// screen makes an unhedged claim with its correction on a branch, which is the
// mistake issue #21 named.
func TestTheArmedScreenStandsUpWithNoBackupParagraph(t *testing.T) {
	with := flat(armedScreen(t, 3, 3))
	without := flat(armedScreen(t, 3, 0))

	if !strings.Contains(with, "lives in this node's channel database") {
		t.Fatalf("the backup paragraph did not render with backups present:\n%s", with)
	}
	if strings.Contains(without, "channel backups exported") {
		t.Fatalf("the backup paragraph rendered with no backups:\n%s", without)
	}

	const custodian = "this node has stored the peer's commitment signature"
	if !strings.Contains(without, custodian) {
		t.Errorf("with no backup paragraph the screen never names the custodian:\n%s",
			without)
	}
	if regexp.MustCompile(`even if this node\b`).MatchString(without) {
		t.Errorf("with no backup paragraph the screen makes the custody claim with "+
			"nothing to correct it:\n%s", without)
	}
}

// TestTheArmedScreenFitsThePane measures the screen nothing had ever measured.
func TestTheArmedScreenFitsThePane(t *testing.T) {
	for _, n := range []int{1, 3, 12} {
		for _, backups := range []int{0, n} {
			screen := armedScreen(t, n, backups)
			for i, line := range strings.Split(screen, "\n") {
				if w := len([]rune(line)); w > prose.PaneWidth {
					t.Errorf("n=%d backups=%d line %d is %d wide, over %d: %q",
						n, backups, i+1, w, prose.PaneWidth, line)
				}
			}
		}
	}
}
