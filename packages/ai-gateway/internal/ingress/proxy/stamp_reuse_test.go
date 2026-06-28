package proxy

import (
	"github.com/goccy/go-json"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/requestcontext"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// TestStampRequestNormalizedReuse covers the Phase-E producer site: the request
// goroutine marshals the computed canonical onto the audit Record so the async
// writer reuses it. This is the integration line between the normalize layer
// (byte-identity proven separately) and the audit reuse branch.
func TestStampRequestNormalizedReuse(t *testing.T) {
	np := normcore.NormalizedPayload{Kind: "ai-chat", Protocol: "openai", NormalizeVersion: "1", Model: "gpt-4o"}
	rctx := requestcontext.NewBuilder().WithNormalized(&np).WithRawBody([]byte(`{"model":"gpt-4o"}`)).Build()
	want, err := json.Marshal(&np)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("stamps when lazy on + canonical computed + body captured", func(t *testing.T) {
		s := &proxyState{
			h:        &Handler{lazyCanonical: true},
			rec:      &audit.Record{RequestBody: []byte(`{"model":"gpt-4o"}`)},
			rctxFull: rctx,
		}
		s.stampRequestNormalizedReuse()
		if string(s.rec.RequestNormalizedReuse) != string(want) {
			t.Fatalf("RequestNormalizedReuse = %s, want %s", s.rec.RequestNormalizedReuse, want)
		}
		if s.rec.RequestNormalizedProtocol != "openai" {
			t.Fatalf("protocol = %q, want openai", s.rec.RequestNormalizedProtocol)
		}
		if s.rec.RequestNormalizedKind != "ai-chat" {
			t.Fatalf("kind = %q, want ai-chat", s.rec.RequestNormalizedKind)
		}
	})

	t.Run("no stamp when flag off (kill-switch -> legacy re-Normalize)", func(t *testing.T) {
		s := &proxyState{
			h:        &Handler{lazyCanonical: false},
			rec:      &audit.Record{RequestBody: []byte(`{"model":"gpt-4o"}`)},
			rctxFull: rctx,
		}
		s.stampRequestNormalizedReuse()
		if s.rec.RequestNormalizedReuse != nil {
			t.Fatal("kill-switch (flag off) must not stamp reuse bytes")
		}
	})

	t.Run("no stamp when body not captured", func(t *testing.T) {
		s := &proxyState{h: &Handler{lazyCanonical: true}, rec: &audit.Record{}, rctxFull: rctx}
		s.stampRequestNormalizedReuse()
		if s.rec.RequestNormalizedReuse != nil {
			t.Fatal("no captured body → no stamp (writer would not normalize the request anyway)")
		}
	})

	t.Run("no stamp when canonical nil", func(t *testing.T) {
		empty := requestcontext.NewBuilder().WithRawBody([]byte(`{"model":"gpt-4o"}`)).Build()
		s := &proxyState{h: &Handler{lazyCanonical: true}, rec: &audit.Record{RequestBody: []byte(`{"model":"gpt-4o"}`)}, rctxFull: empty}
		s.stampRequestNormalizedReuse()
		if s.rec.RequestNormalizedReuse != nil {
			t.Fatal("nil canonical → no stamp (audit derives async)")
		}
	})
}
