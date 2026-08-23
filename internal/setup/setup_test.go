package setup

import (
	"strings"
	"testing"
	"time"

	"github.com/AusDavo/winthistle/internal/bitcoind"
	"github.com/AusDavo/winthistle/internal/coldwallet"
	"github.com/AusDavo/winthistle/internal/journal"
	"github.com/AusDavo/winthistle/internal/prose"
)

// Regtest addresses and regtest descriptors throughout. Nothing here talks to a
// node; these exist so the copy can be rendered and read.
const (
	fixtureWallet  = "winthistle-cold"
	fixtureReceive = "wsh(sortedmulti(2,[1b51e4f1/84h/1h/0h]tpubA/0/*,[4cf33624/84h/1h/0h]tpubB/0/*))#aaaaaaaa"
	fixtureChange  = "wsh(sortedmulti(2,[1b51e4f1/84h/1h/0h]tpubA/1/*,[4cf33624/84h/1h/0h]tpubB/1/*))#bbbbbbbb"
	fixtureStale   = "wsh(multi(2,[1b51e4f1/84h/1h/0h]tpubA/0/*,[4cf33624/84h/1h/0h]tpubB/0/*))#cccccccc"
	addr           = "bcrt1qxqaa0r9vce8ll9q76jqxp7s0u8dz904kdv28aqkd4sl0zj457n7sehqtsu"
	changeAddr     = "bcrt1qlyj3uce4l6zns44nqpz2t0yvx6m2el0lkd74qqrzjk9juzyu34tqv6jpq2"
)

func fixtureCheck() coldwallet.AddressCheck {
	branch := func(a string) []coldwallet.Derived {
		out := make([]coldwallet.Derived, 0, 5)
		for i := range 5 {
			out = append(out, coldwallet.Derived{
				Index: i, Address: a, Mine: true, Solvable: true, FromImported: true,
			})
		}
		return out
	}
	return coldwallet.AddressCheck{
		WalletName:  fixtureWallet,
		Receive:     branch(addr),
		Change:      branch(changeAddr),
		ReceiveDesc: fixtureReceive,
		ChangeDesc:  fixtureChange,
	}
}

func fixtureLanded(stale bool) coldwallet.Landed {
	l := coldwallet.Landed{
		Info: bitcoind.WalletInfo{Name: fixtureWallet, Descriptors: true},
		Receive: bitcoind.WalletDescriptor{
			Desc: fixtureReceive, Active: true, Timestamp: 1699920000, Range: []int{0, 999},
		},
		Change: bitcoind.WalletDescriptor{
			Desc: fixtureChange, Active: true, Internal: true, Range: []int{0, 999},
		},
	}
	if stale {
		l.Others = []bitcoind.WalletDescriptor{{Desc: fixtureStale}}
	}
	return l
}

func confirmed() *journal.Setup {
	return &journal.Setup{
		Wallet: fixtureWallet, Seq: 1, Outcome: journal.SetupConfirmed,
		Receive: fixtureReceive, Change: fixtureChange, SampleSize: 5,
		FirstReceive: addr, FirstChange: changeAddr,
		AnsweredAt: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
	}
}

func flat(s string) string { return strings.Join(strings.Fields(s), " ") }

func mustSay(t *testing.T, got, want string) {
	t.Helper()
	if !strings.Contains(flat(got), want) {
		t.Errorf("the screen does not say %q:\n%s", want, got)
	}
}

// TestTheWalkAwayScreenDoesNotReadAsAFailure. This is the ordinary outcome of a
// first run — the operator has gone to find a hardware wallet — and copy that
// treated it as an error would teach them to answer yes to get rid of it.
func TestTheWalkAwayScreenDoesNotReadAsAFailure(t *testing.T) {
	got := notAnswered(fixtureWallet)
	mustSay(t, got, "Nothing was recorded")
	mustSay(t, got, "winthistle setup")
	mustSay(t, got, "no side effect")
	if strings.Contains(strings.ToLower(got), "fail") {
		t.Errorf("the walk-away screen calls itself a failure:\n%s", got)
	}
}

// TestTheRejectionScreenSaysToUseANewWallet. The non-obvious half of the answer:
// Core keeps one active receive branch and one active change branch, so importing
// the corrected pair deactivates the rejected one rather than removing it, and a
// deactivated descriptor's coins are still in the wallet's coin list. There is no
// RPC that removes a descriptor, so the fix is a different wallet.
func TestTheRejectionScreenSaysToUseANewWallet(t *testing.T) {
	rec := confirmed()
	rec.Outcome = journal.SetupRejected
	got := recordedReport(rec, fixtureLanded(true))

	mustSay(t, got, "did not match")
	mustSay(t, got, "must not fund a batch")
	mustSay(t, got, "use a new wallet name")
	mustSay(t, got, "no RPC that removes a descriptor")
	mustSay(t, got, "[bitcoind] wallet")
}

// TestTheConfirmationSaysWhatItIsAbout. The record is keyed on the descriptors it
// confirms, and an operator who did not know that would read the confirmation as
// a property of the wallet name.
func TestTheConfirmationSaysWhatItIsAbout(t *testing.T) {
	got := recordedReport(confirmed(), fixtureLanded(false))
	mustSay(t, got, "the addresses matched")
	mustSay(t, got, "stops applying the moment those descriptors change")
	mustSay(t, got, "winthistle doctor")
}

// TestAnAnswerAboutOtherDescriptorsIsNotAnAnswer.
func TestAnAnswerAboutOtherDescriptorsIsNotAnAnswer(t *testing.T) {
	prev := confirmed()
	prev.Receive = fixtureStale
	prev.FirstReceive = "bcrt1qsomethingelse"

	got := previousReport(prev, fixtureCheck())
	mustSay(t, got, "a different descriptor pair")
	mustSay(t, got, "Compare them again")
	if !strings.Contains(got, prev.FirstReceive) {
		t.Errorf("the screen does not show the address that was confirmed:\n%s", got)
	}

	// And the same answer about the same descriptors is an answer.
	same := previousReport(confirmed(), fixtureCheck())
	mustSay(t, same, "were confirmed on 1 March 2026")
}

// TestTheSetupScreensStayInThePane. Same rule as the recovery screens: this is
// read at a terminal, and a wrapped address is unreadable.
func TestTheSetupScreensStayInThePane(t *testing.T) {
	rejected := confirmed()
	rejected.Outcome = journal.SetupRejected

	for name, screen := range map[string]string{
		"resume":            resumeReport(fixtureLanded(true), fixtureCheck()),
		"previous, matched": previousReport(confirmed(), fixtureCheck()),
		"previous, other":   previousReport(rejectedAbout(fixtureStale), fixtureCheck()),
		"recorded, yes":     recordedReport(confirmed(), fixtureLanded(false)),
		"recorded, no":      recordedReport(rejected, fixtureLanded(true)),
		"not answered":      notAnswered(fixtureWallet),
		"not asked":         notAsked(fixtureWallet),
		"no descriptors":    noDescriptors(fixtureWallet),
		"question":          Question,
	} {
		for i, line := range strings.Split(screen, "\n") {
			// A descriptor is one token and cannot be wrapped without changing
			// it, so it gets a line of its own and is allowed past the pane —
			// the same exemption doctor's commands have.
			if strings.Contains(line, "wsh(") {
				continue
			}
			if n := len([]rune(line)); n > prose.PaneWidth {
				t.Errorf("%s line %d is %d columns, over %d:\n%s",
					name, i+1, n, prose.PaneWidth, line)
			}
		}
	}
}

func rejectedAbout(desc string) *journal.Setup {
	s := confirmed()
	s.Outcome = journal.SetupRejected
	s.Receive = desc
	return s
}

// TestTheGateIsRecognitionAndNotAttribution.
//
// The measured failure this guards against: a wallet that held a rejected pair
// and then imported the correct one credits the shared addresses to the rejected
// descriptor, because getaddressinfo's parent_desc is singular and Core names the
// key manager that was created first. Three of five receive addresses on the
// harness. Gating the question on attribution would refuse to ask about a wallet
// that is entirely correct, which is the exact case this command exists for.
func TestTheGateIsRecognitionAndNotAttribution(t *testing.T) {
	c := fixtureCheck()
	for i := range c.Receive {
		if i%2 == 0 {
			c.Receive[i].FromImported = false
			c.Receive[i].Parent = fixtureStale
		}
	}
	if !c.Recognised() {
		t.Error("a wallet that owns every address is reported as not recognising them")
	}
	if c.Attributed() {
		t.Error("attribution is reported as intact when it is not")
	}
	if c.Consistent() {
		t.Error("Consistent should be Recognised and Attributed together")
	}
	if got := c.Elsewhere(); len(got) != 1 || got[0] != fixtureStale {
		t.Errorf("Elsewhere = %v, want the one other descriptor", got)
	}

	// An address the wallet disowns is the case that IS a gate: it is not part
	// of the wallet a batch would be built from.
	c.Receive[1].Mine = false
	if c.Recognised() {
		t.Error("an address the wallet disowns passed the gate")
	}
}
