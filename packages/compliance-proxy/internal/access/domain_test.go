package access

import "testing"

func TestDomainAllowlist_ExactMatch(t *testing.T) {
	al, err := NewDomainAllowlist([]string{"api.openai.com:443", "api.anthropic.com:8080"})
	if err != nil {
		t.Fatalf("NewDomainAllowlist: %v", err)
	}

	tests := []struct {
		name string
		host string
		port string
		want bool
	}{
		{"exact match", "api.openai.com", "443", true},
		{"exact match other port", "api.anthropic.com", "8080", true},
		{"wrong host", "evil.com", "443", false},
		{"case insensitive", "API.OpenAI.COM", "443", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := al.Allow(tt.host, tt.port); got != tt.want {
				t.Errorf("Allow(%s, %s) = %v, want %v", tt.host, tt.port, got, tt.want)
			}
		})
	}
}

func TestDomainAllowlist_WildcardMatch(t *testing.T) {
	al, err := NewDomainAllowlist([]string{"*.openai.com:443"})
	if err != nil {
		t.Fatalf("NewDomainAllowlist: %v", err)
	}

	tests := []struct {
		name string
		host string
		port string
		want bool
	}{
		{"subdomain match", "api.openai.com", "443", true},
		{"deep subdomain", "sub.api.openai.com", "443", true},
		{"case insensitive wildcard", "API.OpenAI.COM", "443", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := al.Allow(tt.host, tt.port); got != tt.want {
				t.Errorf("Allow(%s, %s) = %v, want %v", tt.host, tt.port, got, tt.want)
			}
		})
	}
}

func TestDomainAllowlist_WildcardNoMatchBare(t *testing.T) {
	al, err := NewDomainAllowlist([]string{"*.openai.com:443"})
	if err != nil {
		t.Fatalf("NewDomainAllowlist: %v", err)
	}

	// *.openai.com should NOT match the bare domain openai.com.
	if al.Allow("openai.com", "443") {
		t.Error("wildcard *.openai.com should not match bare openai.com")
	}
}

func TestDomainAllowlist_PortMismatch(t *testing.T) {
	al, err := NewDomainAllowlist([]string{"api.openai.com:443"})
	if err != nil {
		t.Fatalf("NewDomainAllowlist: %v", err)
	}

	if al.Allow("api.openai.com", "8080") {
		t.Error("should not match on wrong port")
	}
}

func TestDomainAllowlist_DefaultPort(t *testing.T) {
	al, err := NewDomainAllowlist([]string{"api.openai.com"})
	if err != nil {
		t.Fatalf("NewDomainAllowlist: %v", err)
	}

	if !al.Allow("api.openai.com", "443") {
		t.Error("entry without port should default to 443")
	}
	if al.Allow("api.openai.com", "80") {
		t.Error("should not match port 80 when default is 443")
	}
}

func TestDomainAllowlist_OpenWildcardRejected(t *testing.T) {
	_, err := NewDomainAllowlist([]string{"*:443"})
	if err == nil {
		t.Error("expected error for open wildcard *:443")
	}

	_, err = NewDomainAllowlist([]string{"*"})
	if err == nil {
		t.Error("expected error for open wildcard *")
	}
}

func TestDomainAllowlist_RejectsEmptyEntry(t *testing.T) {
	if _, err := NewDomainAllowlist([]string{""}); err == nil {
		t.Error("expected error for empty domain entry")
	}
	if _, err := NewDomainAllowlist([]string{"   "}); err == nil {
		t.Error("expected error for whitespace-only domain entry")
	}
}

func TestDomainAllowlist_IPv6BracketedEntries(t *testing.T) {
	// IPv6 literal with explicit port.
	al, err := NewDomainAllowlist([]string{"[2001:db8::1]:8443"})
	if err != nil {
		t.Fatalf("NewDomainAllowlist with IPv6 bracket+port: %v", err)
	}
	if !al.Allow("2001:db8::1", "8443") {
		t.Error("expected IPv6 literal to allow on configured port")
	}
	if al.Allow("2001:db8::1", "443") {
		t.Error("expected IPv6 literal to reject other ports")
	}

	// IPv6 literal without explicit port → defaults to 443.
	al2, err := NewDomainAllowlist([]string{"[2001:db8::2]"})
	if err != nil {
		t.Fatalf("NewDomainAllowlist with IPv6 bracket no-port: %v", err)
	}
	if !al2.Allow("2001:db8::2", "443") {
		t.Error("expected bracketed IPv6 without port to default to 443")
	}
}

func TestDomainAllowlist_RejectsMalformedBracketedEntries(t *testing.T) {
	// Missing closing bracket.
	if _, err := NewDomainAllowlist([]string{"[2001:db8::1"}); err == nil {
		t.Error("expected error for IPv6 entry missing closing bracket")
	}
	// Garbage after bracket (no colon).
	if _, err := NewDomainAllowlist([]string{"[2001:db8::1]junk"}); err == nil {
		t.Error("expected error for IPv6 entry with non-colon suffix after bracket")
	}
}

func TestDomainAllowlist_WildcardWrongPortSkipsEntry(t *testing.T) {
	// Two wildcard entries on different ports — the port-mismatch loop branch
	// (`if port != w.port { continue }`) must skip the wrong-port entry and
	// the second iteration must still match the correct one.
	al, err := NewDomainAllowlist([]string{"*.openai.com:8443", "*.openai.com:443"})
	if err != nil {
		t.Fatalf("NewDomainAllowlist: %v", err)
	}
	if !al.Allow("api.openai.com", "443") {
		t.Error("expected match on second wildcard (port 443) after first port-skip")
	}
	if al.Allow("api.openai.com", "9999") {
		t.Error("must not match when no wildcard's port matches")
	}
}

func TestDomainAllowlist_WildcardSuffixDoesNotMatchEqualLengthHost(t *testing.T) {
	// CIDR-boundary equivalent for wildcards: the suffix is ".openai.com",
	// and any host equal in length to the suffix must not match. This guards
	// the `len(host) > len(w.suffix)` condition that prevents the wildcard
	// from matching the bare apex with a stripped leading-dot.
	al, err := NewDomainAllowlist([]string{"*.openai.com:443"})
	if err != nil {
		t.Fatalf("NewDomainAllowlist: %v", err)
	}
	// ".openai.com" is exactly the suffix length — must NOT match. Note: a
	// real hostname starting with "." is invalid in practice but the guard
	// still applies and is the exact off-by-one we want to lock down.
	if al.Allow(".openai.com", "443") {
		t.Error("wildcard suffix must not match a host equal to the suffix itself")
	}
}
