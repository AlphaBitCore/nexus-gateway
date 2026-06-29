package wiring

import (
	"bytes"
	"testing"

	"github.com/goccy/go-json"

	auditevent "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/audit/event"
)

// TestRecomputeNormalizedForView_FillsFromBody asserts the detail drawer's
// view-time recompute reconstructs the normalized projection from the stored
// request/response bodies when the persisted columns are empty (the current
// write-path behavior — normalized is no longer stamped).
func TestRecomputeNormalizedForView_FillsFromBody(t *testing.T) {
	reg := InitNormalizeRegistry()
	ev := &auditevent.Event{
		ProviderName:    "openai",
		ModelName:       "gpt-4o",
		Path:            "/v1/chat/completions",
		PayloadRequest:  []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`),
		PayloadResponse: []byte(`{"choices":[{"message":{"role":"assistant","content":"hi there"}}]}`),
	}

	recomputeNormalizedForView(ev, reg)

	if len(ev.NormalizedRequest) == 0 {
		t.Fatal("request normalized must be recomputed from the stored body")
	}
	if len(ev.NormalizedResponse) == 0 {
		t.Fatal("response normalized must be recomputed from the stored body")
	}
	// The recomputed projection must carry the actual message text — proves a
	// real normalize ran over the stored body, not an empty placeholder.
	if !json.Valid(ev.NormalizedRequest) {
		t.Fatalf("recomputed request normalized is not valid JSON: %s", ev.NormalizedRequest)
	}
	if !bytes.Contains(ev.NormalizedRequest, []byte("hello")) {
		t.Fatalf("recomputed request normalized must contain the user text, got %s", ev.NormalizedRequest)
	}
	if !bytes.Contains(ev.NormalizedResponse, []byte("hi there")) {
		t.Fatalf("recomputed response normalized must contain the assistant text, got %s", ev.NormalizedResponse)
	}
}

// TestRecomputeNormalizedForView_UsesIngressFormatAdapter asserts the recompute
// keys on the stored ingress_format (the domain-matched adapter) when present —
// the authoritative path that keeps agent-UI and CP-UI recompute in agreement
// (CP reads the same traffic_event.ingress_format). An empty ingress_format
// still falls back to path/sniff (covered by the cross-provider test above).
func TestRecomputeNormalizedForView_UsesIngressFormatAdapter(t *testing.T) {
	reg := InitNormalizeRegistry()
	ev := &auditevent.Event{
		IngressFormat:  "openai",
		ModelName:      "gpt-4o",
		Path:           "/v1/chat/completions",
		PayloadRequest: []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"ping-if"}]}`),
	}
	recomputeNormalizedForView(ev, reg)
	if len(ev.NormalizedRequest) == 0 || !bytes.Contains(ev.NormalizedRequest, []byte("ping-if")) {
		t.Fatalf("recompute keyed on ingress_format must produce normalized with the user text, got %s", ev.NormalizedRequest)
	}
}

// TestRecomputeNormalizedForView_RedactedBodyStaysRedacted asserts the PII-safety
// contract: the recompute is a pure function of the stored (already-redaction-
// governed) body, so a redacted body yields redacted normalized text and never
// resurfaces the original value. This is the structural guarantee that makes
// dropping the stored projection safe.
func TestRecomputeNormalizedForView_RedactedBodyStaysRedacted(t *testing.T) {
	reg := InitNormalizeRegistry()
	ev := &auditevent.Event{
		ProviderName:   "openai",
		ModelName:      "gpt-4o",
		Path:           "/v1/chat/completions",
		PayloadRequest: []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"my card is [REDACTED]"}]}`),
	}

	recomputeNormalizedForView(ev, reg)

	if !bytes.Contains(ev.NormalizedRequest, []byte("[REDACTED]")) {
		t.Fatalf("recompute must preserve the redaction marker, got %s", ev.NormalizedRequest)
	}
	if bytes.Contains(ev.NormalizedRequest, []byte("4111")) {
		t.Fatalf("recompute must not resurrect pre-redaction PII, got %s", ev.NormalizedRequest)
	}
}

// TestRecomputeNormalizedForView_ResolvesByPathAcrossProviders proves the
// empty-adapter design decision: the recompute keys nothing on the (unknown)
// provider — it resolves the normalizer via the request path + content sniff,
// exactly as the control plane does for agent rows. Each provider's canonical
// ingress path must still produce a non-empty normalized projection carrying
// the user's text, so the agent-UI and CP-UI recompute agree on agent rows.
func TestRecomputeNormalizedForView_ResolvesByPathAcrossProviders(t *testing.T) {
	reg := InitNormalizeRegistry()
	cases := []struct {
		name string
		path string
		body string
		text string
	}{
		{"openai chat", "/v1/chat/completions", `{"model":"gpt-4o","messages":[{"role":"user","content":"ping-oa"}]}`, "ping-oa"},
		{"anthropic messages", "/v1/messages", `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"ping-an"}]}`, "ping-an"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := &auditevent.Event{
				Path:           tc.path,
				PayloadRequest: []byte(tc.body),
			}
			recomputeNormalizedForView(ev, reg)
			if len(ev.NormalizedRequest) == 0 {
				t.Fatalf("path %s must resolve a normalizer and produce normalized output", tc.path)
			}
			if !bytes.Contains(ev.NormalizedRequest, []byte(tc.text)) {
				t.Fatalf("recomputed normalized must carry the user text %q, got %s", tc.text, ev.NormalizedRequest)
			}
		})
	}
}

// TestRecomputeNormalizedForView_PreservesStored asserts a row that already
// carries a stored projection (an upload from an older agent build) is left
// untouched — recompute only fills empty directions.
func TestRecomputeNormalizedForView_PreservesStored(t *testing.T) {
	reg := InitNormalizeRegistry()
	stored := json.RawMessage(`{"stored":"keep-me"}`)
	ev := &auditevent.Event{
		ProviderName:      "openai",
		PayloadRequest:    []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"x"}]}`),
		NormalizedRequest: stored,
	}

	recomputeNormalizedForView(ev, reg)

	if string(ev.NormalizedRequest) != string(stored) {
		t.Fatalf("stored normalized must be preserved, got %s", ev.NormalizedRequest)
	}
}

// TestRecomputeNormalizedForView_NilRegistryNoop asserts a nil registry (or nil
// event) is a safe no-op rather than a panic — the drawer degrades to showing
// only what an old row stored.
func TestRecomputeNormalizedForView_NilRegistryNoop(t *testing.T) {
	ev := &auditevent.Event{PayloadRequest: []byte(`{"model":"gpt-4o"}`)}
	recomputeNormalizedForView(ev, nil)
	if len(ev.NormalizedRequest) != 0 {
		t.Fatal("nil registry must leave normalized empty")
	}
	recomputeNormalizedForView(nil, InitNormalizeRegistry()) // must not panic
}

// TestLooksLikeSSE covers the response content-sniff that selects the streaming
// decoder at view time (the agent audit row does not store the content type).
func TestLooksLikeSSE(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"data field", "data: {\"x\":1}\n\n", true},
		{"event field", "event: message\ndata: {}\n", true},
		{"leading whitespace", "\n\n  data: {}", true},
		{"plain json", `{"choices":[]}`, false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := looksLikeSSE([]byte(tc.body)); got != tc.want {
				t.Fatalf("looksLikeSSE(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}
