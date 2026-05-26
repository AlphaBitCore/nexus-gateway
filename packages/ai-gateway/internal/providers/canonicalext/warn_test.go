package canonicalext

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestWarnOnce_DedupesByProviderField(t *testing.T) {
	ResetWarnSeenForTest()
	defer ResetWarnSeenForTest()
	prev := slog.Default()
	defer slog.SetDefault(prev)

	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))

	WarnOnce("anthropic", "logit_bias")
	WarnOnce("anthropic", "logit_bias")
	WarnOnce("anthropic", "logit_bias")

	count := strings.Count(buf.String(), "nexus_field_unsupported")
	if count != 1 {
		t.Fatalf("expected 1 WARN, got %d:\n%s", count, buf.String())
	}
	if !strings.Contains(buf.String(), `provider=anthropic`) {
		t.Errorf("missing provider attr: %s", buf.String())
	}
	if !strings.Contains(buf.String(), `field=logit_bias`) {
		t.Errorf("missing field attr: %s", buf.String())
	}

	// A different field on the same provider must still warn once.
	WarnOnce("anthropic", "seed")
	if c := strings.Count(buf.String(), "nexus_field_unsupported"); c != 2 {
		t.Fatalf("expected 2 WARN after second field, got %d", c)
	}

	// Same field on a different provider is independent.
	WarnOnce("gemini", "logit_bias")
	if c := strings.Count(buf.String(), "nexus_field_unsupported"); c != 3 {
		t.Fatalf("expected 3 WARN after second provider, got %d", c)
	}
}

func TestScanUnsupported_OnlyWarnsUnsupported(t *testing.T) {
	ResetWarnSeenForTest()
	defer ResetWarnSeenForTest()
	prev := slog.Default()
	defer slog.SetDefault(prev)

	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))

	body := []byte(`{
		"model":"x",
		"messages":[{"role":"user","content":"hi"}],
		"temperature":0.5,
		"logit_bias":{"50256":-100},
		"seed":42,
		"nexus":{"ext":{"vendor":{"k":"v"}}}
	}`)
	supported := map[string]struct{}{
		"model":       {},
		"messages":    {},
		"temperature": {},
	}
	ScanUnsupported("anthropic", body, supported)

	out := buf.String()
	// nexus passthrough must never warn.
	if strings.Contains(out, "field=nexus") {
		t.Errorf("nexus passthrough must be exempt: %s", out)
	}
	// Private fields (starting with _ or $) must be exempt.
	for _, body := range [][]byte{
		[]byte(`{"_source":"vendor-doc","model":"x","messages":[]}`),
		[]byte(`{"$schema":"vendor-doc","model":"x","messages":[]}`),
	} {
		ResetWarnSeenForTest()
		buf.Reset()
		ScanUnsupported("anthropic", body, supported)
		if strings.Contains(buf.String(), "field=_source") || strings.Contains(buf.String(), "field=$schema") {
			t.Errorf("private metadata field warned: %s", buf.String())
		}
	}
	// Supported fields must not warn.
	for _, f := range []string{"model", "messages", "temperature"} {
		if strings.Contains(out, "field="+f) {
			t.Errorf("supported field %q warned: %s", f, out)
		}
	}
	// Unsupported fields must warn exactly once each.
	for _, f := range []string{"logit_bias", "seed"} {
		c := strings.Count(out, "field="+f)
		if c != 1 {
			t.Errorf("expected 1 WARN for %q, got %d:\n%s", f, c, out)
		}
	}
}
