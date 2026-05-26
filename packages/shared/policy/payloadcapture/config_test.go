package payloadcapture

import "testing"

func TestDefaultConfig(t *testing.T) {
	c := DefaultConfig()
	if c.StoreRequestBody {
		t.Errorf("StoreRequestBody: want false, got true")
	}
	if c.StoreResponseBody {
		t.Errorf("StoreResponseBody: want false, got true")
	}
	if c.MaxInlineBodyBytes != DefaultMaxInlineBodyBytes {
		t.Errorf("MaxInlineBodyBytes: want %d, got %d", DefaultMaxInlineBodyBytes, c.MaxInlineBodyBytes)
	}
	if c.MaxRequestBytes != DefaultMaxRequestBytes {
		t.Errorf("MaxRequestBytes: want %d, got %d", DefaultMaxRequestBytes, c.MaxRequestBytes)
	}
	if c.MaxResponseBytes != DefaultMaxResponseBytes {
		t.Errorf("MaxResponseBytes: want %d, got %d", DefaultMaxResponseBytes, c.MaxResponseBytes)
	}
	if DefaultMaxInlineBodyBytes != 256*1024 {
		t.Errorf("DefaultMaxInlineBodyBytes: want 262144, got %d", DefaultMaxInlineBodyBytes)
	}
	if DefaultMaxRequestBytes != 10*1024*1024 {
		t.Errorf("DefaultMaxRequestBytes: want 10485760, got %d", DefaultMaxRequestBytes)
	}
	if DefaultMaxResponseBytes != 10*1024*1024 {
		t.Errorf("DefaultMaxResponseBytes: want 10485760, got %d", DefaultMaxResponseBytes)
	}
}

// TestDefaults_NetworkCapsExceedInlineCap pins the invariant that a
// fresh install must NOT silently truncate forwarded bytes. If a
// future change drops MaxRequestBytes / MaxResponseBytes below
// MaxInlineBodyBytes, every non-trivial client (Claude Code, large
// tool schemas, vision payloads) would 413 even though the captured
// inline copy fits — regression of the bug fixed in this iteration.
func TestDefaults_NetworkCapsExceedInlineCap(t *testing.T) {
	c := DefaultConfig()
	if c.MaxRequestBytes <= c.MaxInlineBodyBytes {
		t.Errorf("MaxRequestBytes (%d) must exceed MaxInlineBodyBytes (%d) so the proxy never truncates a forwardable body just because the inline cutoff is smaller",
			c.MaxRequestBytes, c.MaxInlineBodyBytes)
	}
	if c.MaxResponseBytes <= c.MaxInlineBodyBytes {
		t.Errorf("MaxResponseBytes (%d) must exceed MaxInlineBodyBytes (%d)",
			c.MaxResponseBytes, c.MaxInlineBodyBytes)
	}
}
