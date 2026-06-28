package normalize

import (
	"context"
	"github.com/goccy/go-json"
	"strings"
	"testing"

	core "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// TestPhaseE_RequestNormalizedReuse_ByteIdentity is the golden gate for Phase E:
// the bytes the request path stamps onto the audit Record (json.Marshal of the
// canonical computed with the aligned Meta) MUST be byte-identical to what the
// audit bridge produces by re-Normalizing the same raw body. If these ever
// diverge, the reuse path would silently change traffic_event_normalized.
//
// The corpus covers the cases the architecture review flagged: a plain
// content-type, a charset-parameterized content-type, and a mixed-case adapter
// type — the two fields the request-path Meta aligns (StripContentTypeParams +
// lowercase AdapterType).
func TestPhaseE_RequestNormalizedReuse_ByteIdentity(t *testing.T) {
	reg := BuildRegistry()
	auditFn := core.BuildAuditFn(reg, nil)
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hello world"}],"temperature":0.7,"max_tokens":128}`)
	const path = "/v1/chat/completions"

	// Duplicate top-level key is the classic re-marshal-divergence case: both
	// paths run the identical reg.Normalize, so the marshaled bytes must match.
	dupKeyBody := []byte(`{"model":"gpt-4o","model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`)

	cases := []struct {
		name, adapter, contentType string
		body                       []byte
	}{
		{"plain json", "openai", "application/json", body},
		{"charset param", "openai", "application/json; charset=utf-8", body},
		{"mixed-case adapter", "OpenAI", "application/json", body},
		{"duplicate top-level key", "openai", "application/json", dupKeyBody},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Request path: aligned Meta (lowercase adapter + stripped content-type),
			// then marshal the pointer exactly as finalizeAudit does.
			reqMeta := core.Meta{
				AdapterType:  strings.ToLower(tc.adapter),
				Model:        "gpt-4o",
				ContentType:  core.StripContentTypeParams(tc.contentType),
				Direction:    core.DirectionRequest,
				EndpointPath: path,
			}
			payload, err := reg.Normalize(context.Background(), tc.body, reqMeta)
			if err != nil {
				t.Fatalf("request normalize: %v", err)
			}
			stamp, err := json.Marshal(&payload)
			if err != nil {
				t.Fatalf("marshal stamp: %v", err)
			}

			// Audit path: BuildAuditFn lowercases the adapter + strips the
			// content-type internally, then marshals.
			raw, status, errReason := auditFn("request", tc.contentType, tc.adapter, "gpt-4o", path, false, tc.body)
			if status != "ok" {
				t.Fatalf("audit normalize status = %q (%s), want ok", status, errReason)
			}
			if string(stamp) != string(raw) {
				t.Fatalf("reuse stamp != audit re-normalize:\n stamp = %s\n audit = %s", stamp, raw)
			}
		})
	}

	// Parse-failure: a body the registry cannot normalize must make the request
	// path leave the canonical nil (err != nil → no stamp), so recordToMessage
	// falls back to the audit re-Normalize — preserving the partial/failed status
	// exactly as today. This proves the stamp is correctly SUPPRESSED on failure.
	t.Run("parse failure suppresses the stamp", func(t *testing.T) {
		malformed := []byte(`{this is not valid json`)
		reqMeta := core.Meta{
			AdapterType:  "openai",
			Model:        "gpt-4o",
			ContentType:  "application/json",
			Direction:    core.DirectionRequest,
			EndpointPath: path,
		}
		if _, err := reg.Normalize(context.Background(), malformed, reqMeta); err == nil {
			t.Fatal("expected malformed body to fail normalize so the request path leaves canonical nil (no stamp)")
		}
		// The audit fallback still runs and must not report "ok".
		_, status, _ := auditFn("request", "application/json", "openai", "gpt-4o", path, false, malformed)
		if status == "ok" {
			t.Fatalf("audit status on malformed body = %q, want partial/failed", status)
		}
	})
}
