package wiring

import (
	"testing"
	"time"
)

func TestParseDurationOrDefault_EmptyReturnsDefault(t *testing.T) {
	def := 30 * time.Second
	got := ParseDurationOrDefault("", def)
	if got != def {
		t.Errorf("got %v; want %v", got, def)
	}
}

func TestParseDurationOrDefault_ValidString(t *testing.T) {
	got := ParseDurationOrDefault("5m", 10*time.Second)
	if got != 5*time.Minute {
		t.Errorf("got %v; want 5m", got)
	}
}

func TestParseDurationOrDefault_InvalidStringReturnsDefault(t *testing.T) {
	def := 10 * time.Second
	got := ParseDurationOrDefault("notaduration", def)
	if got != def {
		t.Errorf("got %v; want default %v", got, def)
	}
}

func TestParseDurationOrDefault_ZeroStringReturnsDefault(t *testing.T) {
	// "0" is technically a valid duration (0 ns) — ParseDuration succeeds.
	got := ParseDurationOrDefault("0", 5*time.Second)
	if got != 0 {
		t.Errorf("got %v; want 0", got)
	}
}

func TestExtractDomains_Empty(t *testing.T) {
	if got := ExtractDomains(nil); len(got) != 0 {
		t.Errorf("got %v; want empty", got)
	}
}

func TestExtractDomains_PlainHostname(t *testing.T) {
	got := ExtractDomains([]string{"example.com"})
	if len(got) != 1 || got[0] != "example.com" {
		t.Errorf("got %v", got)
	}
}

func TestExtractDomains_HostColonPort(t *testing.T) {
	got := ExtractDomains([]string{"api.example.com:443"})
	if len(got) != 1 || got[0] != "api.example.com" {
		t.Errorf("got %v", got)
	}
}

func TestExtractDomains_WildcardStripsStar(t *testing.T) {
	got := ExtractDomains([]string{"*.example.com"})
	if len(got) != 1 || got[0] != "example.com" {
		t.Errorf("got %v", got)
	}
}

func TestExtractDomains_WildcardWithPort(t *testing.T) {
	got := ExtractDomains([]string{"*.example.com:443"})
	if len(got) != 1 || got[0] != "example.com" {
		t.Errorf("got %v", got)
	}
}

func TestExtractDomains_DeduplicatesEntries(t *testing.T) {
	got := ExtractDomains([]string{"api.example.com:443", "api.example.com:80", "api.example.com"})
	if len(got) != 1 || got[0] != "api.example.com" {
		t.Errorf("expected single deduplicated entry, got %v", got)
	}
}

func TestExtractDomains_MultipleDistinctEntries(t *testing.T) {
	got := ExtractDomains([]string{"a.com:443", "b.com", "*.c.com"})
	want := map[string]bool{"a.com": true, "b.com": true, "c.com": true}
	if len(got) != 3 {
		t.Fatalf("got %v (len %d); want 3 entries", got, len(got))
	}
	for _, h := range got {
		if !want[h] {
			t.Errorf("unexpected host %q", h)
		}
	}
}

func TestExtractDomains_EmptyEntrySkipped(t *testing.T) {
	got := ExtractDomains([]string{""})
	if len(got) != 0 {
		t.Errorf("got %v; want empty for blank entry", got)
	}
}

func TestComposeMetricsURL_EmptyAdvertiseHostFallsBack(t *testing.T) {
	got := ComposeMetricsURL("", ":9090")
	want := "http://127.0.0.1:9090/metrics"
	if got != want {
		t.Errorf("got %q; want %q", got, want)
	}
}

func TestComposeMetricsURL_ZeroZeroHostFallsBack(t *testing.T) {
	got := ComposeMetricsURL("0.0.0.0", ":9090")
	if got != "http://127.0.0.1:9090/metrics" {
		t.Errorf("got %q", got)
	}
}

func TestComposeMetricsURL_DoubleColonHostFallsBack(t *testing.T) {
	got := ComposeMetricsURL("::", ":9090")
	if got != "http://127.0.0.1:9090/metrics" {
		t.Errorf("got %q", got)
	}
}

func TestComposeMetricsURL_ExplicitAdvertiseHost(t *testing.T) {
	got := ComposeMetricsURL("metrics.example.com", ":9090")
	want := "http://metrics.example.com:9090/metrics"
	if got != want {
		t.Errorf("got %q; want %q", got, want)
	}
}

func TestComposeMetricsURL_BindAddrNoPort(t *testing.T) {
	// net.SplitHostPort fails for "/metrics-path" style, falls back to raw concat.
	got := ComposeMetricsURL("", "/metrics-path")
	if got != "http://127.0.0.1/metrics-path/metrics" {
		t.Errorf("got %q", got)
	}
}

func TestComposeMetricsURL_FullHostPort(t *testing.T) {
	got := ComposeMetricsURL("proxy.internal", "0.0.0.0:3040")
	want := "http://proxy.internal:3040/metrics"
	if got != want {
		t.Errorf("got %q; want %q", got, want)
	}
}

func TestComposeManagementBaseURL_EmptyAdvertiseHostFallsBack(t *testing.T) {
	got := ComposeManagementBaseURL("", ":9090")
	want := "http://127.0.0.1:9090"
	if got != want {
		t.Errorf("got %q; want %q", got, want)
	}
}

func TestComposeManagementBaseURL_ZeroZeroHostFallsBack(t *testing.T) {
	got := ComposeManagementBaseURL("0.0.0.0", ":9090")
	if got != "http://127.0.0.1:9090" {
		t.Errorf("got %q", got)
	}
}

func TestComposeManagementBaseURL_DoubleColonFallsBack(t *testing.T) {
	got := ComposeManagementBaseURL("::", ":9090")
	if got != "http://127.0.0.1:9090" {
		t.Errorf("got %q", got)
	}
}

func TestComposeManagementBaseURL_ExplicitHost(t *testing.T) {
	got := ComposeManagementBaseURL("mgmt.example.com", ":9091")
	want := "http://mgmt.example.com:9091"
	if got != want {
		t.Errorf("got %q; want %q", got, want)
	}
}

func TestComposeManagementBaseURL_BindAddrNoPort(t *testing.T) {
	got := ComposeManagementBaseURL("", "/socket")
	if got != "http://127.0.0.1/socket" {
		t.Errorf("got %q", got)
	}
}

func TestComposeManagementBaseURL_FullHostPort(t *testing.T) {
	got := ComposeManagementBaseURL("proxy.internal", "0.0.0.0:3040")
	want := "http://proxy.internal:3040"
	if got != want {
		t.Errorf("got %q; want %q", got, want)
	}
}
