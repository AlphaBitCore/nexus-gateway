package intercept

import (
	"bytes"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
)

func TestCaptureRequestBody_DisabledReturnsNil(t *testing.T) {
	store := payloadcapture.NewStore(payloadcapture.Config{
		StoreRequestBody:   false,
		MaxInlineBodyBytes: 1024,
	})
	got := CaptureRequestBody(store, []byte("hello"))
	if got != nil {
		t.Errorf("StoreRequestBody=false must yield nil, got %q", got)
	}
}

func TestCaptureRequestBody_EnabledCopiesBytes(t *testing.T) {
	store := payloadcapture.NewStore(payloadcapture.Config{
		StoreRequestBody:   true,
		MaxInlineBodyBytes: 1024,
	})
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	got := CaptureRequestBody(store, body)
	if !bytes.Equal(got, body) {
		t.Errorf("captured bytes mismatch:\n got %s\nwant %s", got, body)
	}
	// Mutating the source must not leak into the captured copy.
	body[0] = '!'
	if got[0] == '!' {
		t.Error("captured body aliases source slice; defensive copy required")
	}
}

// TestCaptureRequestBody_DoesNotTruncate pins the semantic: the
// agent does NOT truncate at MaxInlineBodyBytes locally. The Hub demuxes
// inline-vs-spill on receipt; oversized bodies are inlined-truncated by
// the wider audit pipeline (Stage 1) or spilled via pre-signed URL
// (Stage 2). The capture helper just hands over the full slice.
func TestCaptureRequestBody_DoesNotTruncate(t *testing.T) {
	store := payloadcapture.NewStore(payloadcapture.Config{
		StoreRequestBody:   true,
		MaxInlineBodyBytes: 8,
	})
	body := []byte("0123456789abcdef")
	got := CaptureRequestBody(store, body)
	if !bytes.Equal(got, body) {
		t.Errorf("agent capture must not truncate at MaxInlineBodyBytes; got %q want %q", got, body)
	}
}

func TestCaptureRequestBody_EmptyBodyReturnsNil(t *testing.T) {
	store := payloadcapture.NewStore(payloadcapture.Config{
		StoreRequestBody:   true,
		MaxInlineBodyBytes: 1024,
	})
	if got := CaptureRequestBody(store, nil); got != nil {
		t.Errorf("nil body: want nil, got %v", got)
	}
	if got := CaptureRequestBody(store, []byte{}); got != nil {
		t.Errorf("empty body: want nil, got %v", got)
	}
}

func TestCaptureRequestBody_NilStoreReturnsNil(t *testing.T) {
	if got := CaptureRequestBody(nil, []byte("x")); got != nil {
		t.Errorf("nil store: want nil, got %v", got)
	}
}

func TestCaptureResponseBody_DisabledReturnsNil(t *testing.T) {
	store := payloadcapture.NewStore(payloadcapture.Config{
		StoreResponseBody:  false,
		MaxInlineBodyBytes: 1024,
	})
	if got := CaptureResponseBody(store, []byte("x")); got != nil {
		t.Errorf("StoreResponseBody=false must yield nil, got %q", got)
	}
}

func TestCaptureResponseBody_EnabledReturnsFullCopy(t *testing.T) {
	store := payloadcapture.NewStore(payloadcapture.Config{
		StoreResponseBody:  true,
		MaxInlineBodyBytes: 4,
	})
	body := []byte("abcdefghij")
	got := CaptureResponseBody(store, body)
	if !bytes.Equal(got, body) {
		t.Errorf("response capture must return full slice; got %q want %q", got, body)
	}
}

// TestCapture_OnlyOneStageActive asserts the two flags are independent:
// turning on request capture must not cause response capture, and vice
// versa. Mirrors the UI's two-toggle contract in the Settings tab.
func TestCapture_OnlyOneStageActive(t *testing.T) {
	store := payloadcapture.NewStore(payloadcapture.Config{
		StoreRequestBody:   true,
		StoreResponseBody:  false,
		MaxInlineBodyBytes: 1024,
	})
	if got := CaptureRequestBody(store, []byte("req")); string(got) != "req" {
		t.Errorf("request should be captured: %q", got)
	}
	if got := CaptureResponseBody(store, []byte("resp")); got != nil {
		t.Errorf("response should NOT be captured when flag off, got %q", got)
	}

	store.Set(payloadcapture.Config{
		StoreRequestBody:   false,
		StoreResponseBody:  true,
		MaxInlineBodyBytes: 1024,
	})
	if got := CaptureRequestBody(store, []byte("req")); got != nil {
		t.Errorf("request should NOT be captured when flag off, got %q", got)
	}
	if got := CaptureResponseBody(store, []byte("resp")); string(got) != "resp" {
		t.Errorf("response should be captured: %q", got)
	}
}
