package methods

import (
	"strings"
	"testing"
)

// TestValidateRefusesAForbiddenMethod proves the never-list is a rail rather
// than a comment. Without this, "the credential cannot send coins" is a sentence
// someone can invalidate by adding one registry entry.
func TestValidateRefusesAForbiddenMethod(t *testing.T) {
	original := registry
	t.Cleanup(func() { registry = original })

	for _, c := range Forbidden() {
		registry = append([]Method{{
			Name: c.Method,
			Use:  InApp,
			Ops:  []Op{{"onchain", "write"}},
			Why:  "a test, pretending someone added this",
		}}, original...)

		err := validate()
		if err == nil {
			t.Errorf("registering %s was accepted; the credential could then %s",
				c.Method, c.Does)
			continue
		}
		if !strings.Contains(err.Error(), c.Method) {
			t.Errorf("validate rejected %s without naming it: %v", c.Method, err)
		}
	}
}

// TestTheRegistryAsItStandsIsValid duplicates what init() panics over, so a
// failure reads as a test failure rather than a mysterious panic in an unrelated
// package.
func TestTheRegistryAsItStandsIsValid(t *testing.T) {
	if err := validate(); err != nil {
		t.Fatalf("the registry is invalid: %v", err)
	}
}

// TestCapabilitiesReadsAsASentence: the printed claim is generated, so it has to
// survive an edit to the never-list without turning into a list of fragments.
func TestCapabilitiesReadsAsASentence(t *testing.T) {
	got := capabilities()
	if !strings.Contains(got, " or ") {
		t.Errorf("capabilities() = %q, which does not join its last clause", got)
	}
	if strings.Contains(got, ",,") || strings.HasSuffix(got, ",") {
		t.Errorf("capabilities() = %q", got)
	}
	for _, c := range Forbidden() {
		if !strings.Contains(got, c.Does) {
			t.Errorf("capabilities() omits %q: %q", c.Does, got)
		}
	}
}
