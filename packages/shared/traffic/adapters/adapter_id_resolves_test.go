package adapters

import (
	"context"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/codecs"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// TestEveryBuiltinAdapterIDResolvesThroughRegistry is the
// agent / compliance-proxy side of the cross-service compliance
// consistency assertion (the ai-gateway side lives in
// packages/ai-gateway/internal/providers/core/format_normalize_consistency_test.go).
//
// The PreHookCallback path (responseprehook.Build) feeds the
// Registry with Meta.AdapterType = strings.ToLower(audCtx.adapter.ID())
// for SSE responses that agent + compliance-proxy capture. If a
// builtin traffic adapter is added without coverage on EITHER side —
// a dedicated Tier 1 codecs registration OR a Tier 2 PatternNormalizer
// rule OR a Tier 3 GenericHTTP catch — Registry.Normalize returns
// ErrUnsupported and the SSE pre-hook silently drops; audit row's
// normalized_response lands NULL on that adapter's traffic.
//
// The test asserts: for every builtin adapter ID, the Registry chain
// (Tier 1 codecs + Tier 2 PatternNormalizer + Tier 3 GenericHTTP)
// produces a non-nil payload for both:
//   - a generic application/json request body (covers JSON-shape audit)
//   - a generic text/event-stream response body (covers SSE pre-hook)
//
// On failure the message names the missing adapter ID — fix is to
// either add a codecs.RegisterDefaultAIBuiltins entry, hook the
// adapter into Tier 2 PatternNormalizer, or confirm Tier 3 GenericHTTP
// is loaded.
func TestEveryBuiltinAdapterIDResolvesThroughRegistry(t *testing.T) {
	reg := normcore.NewRegistry()
	codecs.RegisterDefaultAIBuiltins(reg)
	RegisterTier1AdapterNormalizers(reg)
	// codecs.RegisterDefaultAIBuiltins already registers the Tier 3
	// GenericHTTPNormalizer under the wildcard "*:*:*" key (see
	// register.go:206), so the chain is complete — no separate Tier 3
	// wire-up needed here.

	cases := []struct {
		name        string
		body        []byte
		contentType string
		direction   normcore.Direction
		path        string
		stream      bool
	}{
		{
			name:        "request_json",
			body:        []byte(`{"model":"x","messages":[{"role":"user","content":"hi"}]}`),
			contentType: "application/json",
			direction:   normcore.DirectionRequest,
		},
		{
			name:        "response_sse",
			body:        []byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"),
			contentType: "text/event-stream",
			direction:   normcore.DirectionResponse,
			stream:      true,
		},
	}

	for _, id := range BuiltinTrafficAdapterIDs() {

		t.Run(id, func(t *testing.T) {
			for _, c := range cases {

				t.Run(c.name, func(t *testing.T) {
					payload, err := reg.Normalize(context.Background(), c.body, normcore.Meta{
						AdapterType:  strings.ToLower(id),
						ContentType:  c.contentType,
						Direction:    c.direction,
						EndpointPath: c.path,
						Stream:       c.stream,
					})
					if err != nil {
						t.Fatalf("adapter %q (%s): Registry.Normalize returned %v — no Tier covers this adapter", id, c.name, err)
					}
					if payload.Kind == "" && payload.Protocol == "" {
						t.Fatalf("adapter %q (%s): Registry returned empty payload (no Kind, no Protocol) — Tier 3 fallback missing?", id, c.name)
					}
				})
			}
		})
	}
}
