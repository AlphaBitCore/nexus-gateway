package forwardheader

import (
	"net/http"
	"testing"
)

func TestApply(t *testing.T) {
	// The embedded default request allowlist admits a base set (accept,
	// content-type, user-agent, ...) but NEVER credential / framing headers
	// (authorization, cookie, x-api-key are on the hard denylist).
	resolved := Default()

	t.Run("empty src returns no drops and copies nothing", func(t *testing.T) {
		dst := http.Header{}
		if dropped := Apply(dst, http.Header{}, resolved, "openai"); dropped != nil {
			t.Errorf("dropped = %v, want nil", dropped)
		}
		if len(dst) != 0 {
			t.Errorf("dst mutated for empty src: %v", dst)
		}
	})

	t.Run("credential headers are dropped, never copied", func(t *testing.T) {
		src := http.Header{
			"Authorization": {"Bearer client-secret"},
			"Cookie":        {"session=abc"},
			"X-Api-Key":     {"leak"},
		}
		dst := http.Header{}
		dropped := Apply(dst, src, resolved, "openai")
		if dst.Get("Authorization") != "" || dst.Get("Cookie") != "" || dst.Get("X-Api-Key") != "" {
			t.Fatalf("credential header leaked into dst: %v", dst)
		}
		if len(dropped) != 3 {
			t.Errorf("dropped count = %d, want 3", len(dropped))
		}
	})

	t.Run("allowlisted header is copied, multi-value preserved", func(t *testing.T) {
		// content-type is in the base request allowlist.
		src := http.Header{"Content-Type": {"multipart/form-data"}, "Accept": {"a", "b"}}
		dst := http.Header{}
		Apply(dst, src, resolved, "openai")
		if dst.Get("Content-Type") != "multipart/form-data" {
			t.Errorf("Content-Type not forwarded: %v", dst)
		}
		if vals := dst.Values("Accept"); len(vals) != 2 {
			t.Errorf("Accept multi-value not preserved: %v", vals)
		}
	})

	t.Run("nil Resolved falls back to defaults without panicking", func(t *testing.T) {
		src := http.Header{"Authorization": {"Bearer x"}}
		dst := http.Header{}
		Apply(dst, src, nil, "openai")
		if dst.Get("Authorization") != "" {
			t.Errorf("credential leaked under nil Resolved: %v", dst)
		}
	})
}
