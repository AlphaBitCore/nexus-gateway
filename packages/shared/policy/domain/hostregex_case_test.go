package domain

import "testing"

func regexDomain(id, pattern string) InterceptionDomain {
	return InterceptionDomain{
		ID:            id,
		Name:          id,
		HostPattern:   pattern,
		HostMatchType: HostMatchRegex,
		Enabled:       true,
	}
}

// TestMatchHost_UppercaseRegexPatternIsNotADeadRule is the live defect.
//
// This engine is the per-connection path: tlsbump's forward exchange and the
// agent's bridge both call MatchHost, and an unmatched host is relayed
// uninspected. MatchHost lowercases the host, and matchHost lowercases the
// PATTERN for exact / glob / prefix — but the regex was compiled verbatim, so a
// pattern with any capital in it could never match a lowercased host. Not the
// mixed-case host, not the lowercase one: the rule simply never fired, with
// nothing to tell the admin who wrote it.
func TestMatchHost_UppercaseRegexPatternIsNotADeadRule(t *testing.T) {
	e := NewEngine()
	if err := e.Swap([]InterceptionDomain{regexDomain("d1", `^API\.openai\.com$`)}); err != nil {
		t.Fatalf("Swap: %v", err)
	}

	for _, host := range []string{
		"api.openai.com",
		"API.openai.com",
		"Api.OpenAI.Com",
		"API.OPENAI.COM",
		"api.openai.com:443", // the port form MatchHost strips
	} {
		if got := e.MatchHost(host); got == nil {
			t.Errorf("host %q did not match the rule — it would be relayed uninspected", host)
		}
	}
}

// TestMatchHost_LowercaseRegexPatternStillMatches pins that the fold did not
// break the spelling that already worked.
func TestMatchHost_LowercaseRegexPatternStillMatches(t *testing.T) {
	e := NewEngine()
	if err := e.Swap([]InterceptionDomain{regexDomain("d1", `^api\.openai\.com$`)}); err != nil {
		t.Fatalf("Swap: %v", err)
	}
	for _, host := range []string{"api.openai.com", "API.OPENAI.COM"} {
		if got := e.MatchHost(host); got == nil {
			t.Errorf("host %q did not match", host)
		}
	}
}

// TestMatchHost_RegexStillDiscriminates — the fold must not turn a rule into a
// match-everything. A host the pattern does not describe must still miss.
func TestMatchHost_RegexStillDiscriminates(t *testing.T) {
	e := NewEngine()
	if err := e.Swap([]InterceptionDomain{regexDomain("d1", `^api\.openai\.com$`)}); err != nil {
		t.Fatalf("Swap: %v", err)
	}
	for _, host := range []string{"api.anthropic.com", "evil.com", "notapi.openai.com"} {
		if got := e.MatchHost(host); got != nil {
			t.Errorf("host %q matched a rule that does not describe it (matched %q)", host, got.ID)
		}
	}
}

// TestMatchHost_InvalidRegexIsRefusedAtSwap pins that prefixing the fold did not
// swallow a compile error: a bad pattern must still fail the config swap rather
// than install a rule that never matches.
func TestMatchHost_InvalidRegexIsRefusedAtSwap(t *testing.T) {
	e := NewEngine()
	if err := e.Swap([]InterceptionDomain{regexDomain("bad", `^api\.(openai$`)}); err == nil {
		t.Fatal("an uncompilable host regex was accepted by Swap")
	}
}
