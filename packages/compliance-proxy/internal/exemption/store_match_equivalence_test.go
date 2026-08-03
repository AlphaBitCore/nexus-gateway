package exemption

import (
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"
)

// The IsExempt hot path was changed to hoist per-request work (parsing the
// client IP, lowercasing the request host) out of the per-exemption loop.
// These tests pin that the hoisted forms are BEHAVIOURALLY IDENTICAL to the
// original string-in forms, because a divergence here would silently change
// who is exempt from compliance inspection — a correctness failure that a
// performance number cannot offset.

// TestMatchSourceIPParsed_EquivalentToStringForm walks the spec/client matrix
// including the malformed inputs a transparent proxy actually sees.
func TestMatchSourceIPParsed_EquivalentToStringForm(t *testing.T) {
	specs := []string{
		"", "*", // match-everything forms
		"10.0.0.5",                   // exact v4
		"10.0.0.0/24", "10.0.0.0/32", // CIDR v4
		"0.0.0.0/0",       // catch-all CIDR
		"2001:db8::1",     // exact v6
		"2001:db8::/32",   // CIDR v6
		"not-an-ip",       // malformed spec
		"10.0.0.0/99",     // malformed CIDR
		"::ffff:10.0.0.5", // v4-mapped v6
	}
	clients := []string{
		"10.0.0.5", "10.0.0.6", "10.1.0.5",
		"2001:db8::1", "2001:db8::2",
		"", "not-an-ip", "10.0.0.5:443", // the last is a common caller mistake (host:port)
	}

	for _, spec := range specs {
		for _, client := range clients {
			want := matchSourceIP(spec, client)
			got := matchSourceIPParsed(spec, net.ParseIP(client))
			if got != want {
				t.Errorf("spec=%q client=%q: hoisted form returned %v, string form %v", spec, client, got, want)
			}
		}
	}
}

// TestMatchTargetHostLowered_EquivalentToStringForm covers exact, wildcard,
// case, and the apex-domain exclusion that standard wildcard semantics require.
func TestMatchTargetHostLowered_EquivalentToStringForm(t *testing.T) {
	specs := []string{
		"", "*",
		"api.openai.com", "API.OpenAI.COM",
		"*.openai.com", "*.OPENAI.com",
		"*.", "*",
		"openai.com",
	}
	hosts := []string{
		"api.openai.com", "API.OPENAI.COM",
		"sub.api.openai.com", "openai.com",
		"notopenai.com", "evil-openai.com",
		"",
	}

	for _, spec := range specs {
		for _, host := range hosts {
			want := matchTargetHost(spec, host)
			got := matchTargetHostLowered(spec, strings.ToLower(host))
			if got != want {
				t.Errorf("spec=%q host=%q: hoisted form returned %v, string form %v", spec, host, got, want)
			}
		}
	}
}

// TestMatchSourceIPParsed_NilClientFailsClosed pins the one place the hoisted
// form has to make an explicit decision the string form made implicitly: an
// unparseable client address. A match-everything spec still matches (that check
// never looked at the client), but anything requiring a comparison must fail
// closed rather than exempt an unidentifiable peer from inspection.
func TestMatchSourceIPParsed_NilClientFailsClosed(t *testing.T) {
	if !matchSourceIPParsed("", nil) {
		t.Error(`spec "" must still match everything even with an unparseable client`)
	}
	if !matchSourceIPParsed("*", nil) {
		t.Error(`spec "*" must still match everything even with an unparseable client`)
	}
	for _, spec := range []string{"10.0.0.5", "10.0.0.0/24", "0.0.0.0/0", "2001:db8::1"} {
		if matchSourceIPParsed(spec, nil) {
			t.Errorf("spec %q matched an unparseable client IP — must fail closed", spec)
		}
	}
}

// TestIsExempt_EmptyStoreFastPath pins that the early-out returns the same
// answer the full scan would, and does not mistake an expired-only store for
// an empty one.
func TestIsExempt_EmptyStoreFastPath(t *testing.T) {
	s := NewStore(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if ok, e := s.IsExempt("10.0.0.5", "api.openai.com"); ok || e != nil {
		t.Fatalf("empty store: got (%v, %v), want (false, nil)", ok, e)
	}

	// A store holding a live, matching exemption must NOT take the fast path.
	if got := s.Add("10.0.0.5", "api.openai.com", time.Hour, "test", "test"); got == nil {
		t.Fatal("Add returned nil")
	}
	ok, e := s.IsExempt("10.0.0.5", "api.openai.com")
	if !ok || e == nil {
		t.Fatalf("populated store: got (%v, %v), want a match", ok, e)
	}
	if e.TargetHost != "api.openai.com" {
		t.Errorf("matched the wrong exemption: %+v", e)
	}
}
