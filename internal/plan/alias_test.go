package plan

import (
	"strings"
	"testing"
)

// flat collapses the wrapped prose so an assertion can name a phrase without
// caring where prose.Para chose to break the line.
func flat(s string) string { return strings.Join(strings.Fields(s), " ") }

// The alias makes the key legible. It must never replace it: an alias is
// self-declared gossip, so a sheet naming only "bitrefill" would ask the
// operator to check an amount against a name any node can adopt.
func TestTheSheetNamesThePeerByAliasAndKeyTogether(t *testing.T) {
	p := &Plan{
		Chain: "regtest",
		Fee:   Fee{TargetSatPerVB: 5},
		Channels: []Channel{{
			Peer:      "02cca6c5c966fcf61d121e3a70e03a1cd9eeeea024b26ea666ce974d43b242e636",
			Alias:     "bitrefill",
			Address:   "bcrt1qqypqxpq9qcrsszg2pvxq6rs0zqg3yyc5z5tpwxqergd3c8g7rusq7snjn6",
			AmountSat: 1_000_000,
		}},
		Change: Change{Address: "bcrt1q5zs69gay5kn2029f4246etdw47ctrv4nvzy38a"},
	}

	doc := p.Document()
	if !strings.Contains(doc, "bitrefill") {
		t.Error("the alias is absent, so the sheet is unreadable against a signer")
	}
	if !strings.Contains(doc, "02cca6c5c966fcf6") {
		t.Fatal("the pubkey prefix is gone. An alias is not an identifier and " +
			"must not be the only thing naming a channel's counterparty")
	}
	if !strings.Contains(flat(doc), "channel 1 to bitrefill (02cca6c5c966fcf6…)") {
		t.Errorf("the label does not pair the two:\n%s", doc)
	}

	// And the caveat, because an operator who trusts the name has been misled
	// by this document rather than by the network.
	if !strings.Contains(flat(doc), "whatever each node says about itself") {
		t.Error("the sheet shows aliases without saying they are self-declared")
	}
}

// A peer the graph has never heard of, or one that never set an alias, is
// ordinary. It must render as the key alone and say nothing about aliases.
func TestNoAliasRendersTheKeyAloneAndDropsTheCaveat(t *testing.T) {
	p := &Plan{
		Chain: "regtest",
		Fee:   Fee{TargetSatPerVB: 5},
		Channels: []Channel{{
			Peer:      "02cca6c5c966fcf61d121e3a70e03a1cd9eeeea024b26ea666ce974d43b242e636",
			Address:   "bcrt1qqypqxpq9qcrsszg2pvxq6rs0zqg3yyc5z5tpwxqergd3c8g7rusq7snjn6",
			AmountSat: 1_000_000,
		}},
		Change: Change{Address: "bcrt1q5zs69gay5kn2029f4246etdw47ctrv4nvzy38a"},
	}

	doc := p.Document()
	if !strings.Contains(flat(doc), "channel 1 to 02cca6c5c966fcf6…") {
		t.Errorf("an aliasless channel should read as the key alone:\n%s", doc)
	}
	if strings.Contains(flat(doc), "whatever each node says about itself") {
		t.Error("the alias caveat is shown for a batch with no aliases in it, " +
			"which is a paragraph about nothing")
	}
}

// The alias is presentation and nothing else: the verifier is about a
// transaction, and a transaction carries no aliases. A wrong one must not be
// able to fail a batch.
func TestTheVerifierIgnoresTheAlias(t *testing.T) {
	base := func(alias string) *Plan {
		return &Plan{
			Chain: "regtest",
			Fee:   Fee{TargetSatPerVB: 5},
			Channels: []Channel{{
				Peer:      "02cca6c5c966fcf61d121e3a70e03a1cd9eeeea024b26ea666ce974d43b242e636",
				Alias:     alias,
				Address:   "bcrt1qqypqxpq9qcrsszg2pvxq6rs0zqg3yyc5z5tpwxqergd3c8g7rusq7snjn6",
				AmountSat: 1_000_000,
			}},
			Change: Change{Address: "bcrt1q5zs69gay5kn2029f4246etdw47ctrv4nvzy38a"},
		}
	}

	right, err := base("bitrefill").Outputs()
	if err != nil {
		t.Fatalf("Outputs: %v", err)
	}
	wrong, err := base("not-who-you-think").Outputs()
	if err != nil {
		t.Fatalf("Outputs: %v", err)
	}
	none, err := base("").Outputs()
	if err != nil {
		t.Fatalf("Outputs: %v", err)
	}

	// Same scripts, same amounts, whatever the label says.
	for i := range right {
		if string(right[i].Script) != string(wrong[i].Script) ||
			right[i].AmountSat != wrong[i].AmountSat {
			t.Fatal("the alias changed what the verifier will check, so a name " +
				"read out of gossip is load-bearing. It must not be")
		}
		if string(right[i].Script) != string(none[i].Script) {
			t.Fatal("an absent alias changed the scripts")
		}
	}
}
