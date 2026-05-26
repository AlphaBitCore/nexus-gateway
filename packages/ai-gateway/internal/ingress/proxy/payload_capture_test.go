package proxy

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
)

// TestHandler_ReadBody_HonorsMaxRequestBytes verifies that the AI
// Gateway's request body reader bounds by MaxRequestBytes (the network
// read cap) and NOT by MaxInlineBodyBytes (the inline-vs-spill cutoff).
// MaxInlineBodyBytes is intentionally smaller than MaxRequestBytes here
// to pin the regression: bodies that fit within MaxRequestBytes must
// be returned in full so the proxy can forward them to the upstream
// unchanged, even when MaxInlineBodyBytes is tiny.
func TestHandler_ReadBody_HonorsMaxRequestBytes(t *testing.T) {
	payload := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"` + string(bytes.Repeat([]byte("x"), 10*1024)) + `"}]}`)

	tests := []struct {
		name    string
		store   *payloadcapture.Store
		wantLen int
		wantErr error
	}{
		{
			// MaxInlineBodyBytes is set tiny on purpose — the network
			// read cap (MaxRequestBytes) is far larger so readBody must
			// NOT truncate. This is the exact scenario that broke
			// claude-gw before the body-cap fix: the inbound body was
			// sliced to the inline cap and the truncated payload was
			// forwarded upstream.
			name: "tiny inline cap does not affect network read",
			store: payloadcapture.NewStore(payloadcapture.Config{
				MaxInlineBodyBytes: 4096,
				MaxRequestBytes:    1 << 20, // 1 MiB
			}),
			wantLen: len(payload),
		},
		{
			name: "request body within MaxRequestBytes returns full body",
			store: payloadcapture.NewStore(payloadcapture.Config{
				MaxInlineBodyBytes: DefaultInlineCutoffForTest(),
				MaxRequestBytes:    1 << 20,
			}),
			wantLen: len(payload),
		},
		{
			name:    "nil store falls back to 10 MiB default",
			store:   nil,
			wantLen: len(payload),
		},
		{
			name: "zero MaxRequestBytes coerces to default and admits the body",
			store: payloadcapture.NewStore(payloadcapture.Config{
				MaxInlineBodyBytes: 1024,
				MaxRequestBytes:    0,
			}),
			wantLen: len(payload),
		},
		{
			// Request body exceeding MaxRequestBytes triggers the 413
			// path. readBody must NOT silently truncate — that was the
			// pre-fix behavior that produced corrupt JSON for the
			// upstream and surfaced as a 400 from Anthropic.
			name: "body over MaxRequestBytes returns errRequestTooLarge",
			store: payloadcapture.NewStore(payloadcapture.Config{
				MaxInlineBodyBytes: 4096,
				MaxRequestBytes:    1024,
			}),
			wantErr: errRequestTooLarge,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handler{deps: &Deps{PayloadCapture: tc.store}}
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(payload))
			in := Ingress{
				WireShape:     typology.WireShapeOpenAIChat,
				BodyFormat:   provcore.FormatOpenAI,
			}
			body, _, _, err := h.readBody(req, in)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("readBody: want err %v, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("readBody: %v", err)
			}
			if len(body) != tc.wantLen {
				t.Errorf("len(body): want %d, got %d", tc.wantLen, len(body))
			}
		})
	}
}

// DefaultInlineCutoffForTest exposes the package default inline-vs-spill
// cutoff for the table above without leaking the import into every line.
func DefaultInlineCutoffForTest() int64 {
	return payloadcapture.DefaultMaxInlineBodyBytes
}

// TestHandler_PayloadCaptureConfig_NilSafe asserts that reading the
// snapshot helper on a Handler without a store does not panic and
// returns the conservative default (capture off, 256 KiB inline cutoff,
// 10 MiB network caps).
func TestHandler_PayloadCaptureConfig_NilSafe(t *testing.T) {
	h := &Handler{deps: &Deps{}}
	got := h.payloadCaptureConfig()
	want := payloadcapture.DefaultConfig()
	if got != want {
		t.Errorf("payloadCaptureConfig: want %+v, got %+v", want, got)
	}

	// Fully nil deps also degrades cleanly (defensive, not expected in prod).
	h2 := &Handler{}
	got2 := h2.payloadCaptureConfig()
	if got2 != want {
		t.Errorf("payloadCaptureConfig (nil deps): want %+v, got %+v", want, got2)
	}
}

// TestHandler_PayloadCaptureConfig_ReflectsStore confirms the helper
// round-trips Set/Get so the hot path observes admin toggles atomically.
func TestHandler_PayloadCaptureConfig_ReflectsStore(t *testing.T) {
	store := payloadcapture.NewStore(payloadcapture.DefaultConfig())
	h := &Handler{deps: &Deps{PayloadCapture: store}}

	if got := h.payloadCaptureConfig(); got.StoreRequestBody {
		t.Errorf("initial StoreRequestBody: want false, got true")
	}

	store.Set(payloadcapture.Config{
		StoreRequestBody:   true,
		StoreResponseBody:  true,
		MaxInlineBodyBytes: 2048,
		MaxRequestBytes:    8 * 1024 * 1024,
		MaxResponseBytes:   4 * 1024 * 1024,
	})

	got := h.payloadCaptureConfig()
	if !got.StoreRequestBody {
		t.Error("StoreRequestBody: want true after Set")
	}
	if !got.StoreResponseBody {
		t.Error("StoreResponseBody: want true after Set")
	}
	if got.MaxInlineBodyBytes != 2048 {
		t.Errorf("MaxInlineBodyBytes: want 2048, got %d", got.MaxInlineBodyBytes)
	}
	if got.MaxRequestBytes != 8*1024*1024 {
		t.Errorf("MaxRequestBytes: want 8 MiB, got %d", got.MaxRequestBytes)
	}
	if got.MaxResponseBytes != 4*1024*1024 {
		t.Errorf("MaxResponseBytes: want 4 MiB, got %d", got.MaxResponseBytes)
	}
}
