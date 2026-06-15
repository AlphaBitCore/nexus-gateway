package codecs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// Compile-time pins: every codec wired through RegisterSniffer must
// satisfy the optional Sniffer capability.
var (
	_ core.Sniffer = (*AnthropicMessagesNormalizer)(nil)
	_ core.Sniffer = (*OpenAIChatNormalizer)(nil)
	_ core.Sniffer = (*OpenAIResponsesNormalizer)(nil)
	_ core.Sniffer = (*GeminiGenerateNormalizer)(nil)
)

func TestAnthropicLooksLike(t *testing.T) {
	n := NewAnthropicMessagesNormalizer()
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"sse event message_start", "event: message_start\ndata: {\"type\":\"message_start\"}\n", true},
		{"sse data message_start", "data: {\"type\":\"message_start\",\"message\":{}}\n", true},
		{"nonstream response head", `{"id":"msg_1","type":"message","role":"assistant","content":[],"stop_reason":"end_turn"}`, true},
		{"bedrock request marker", `{"anthropic_version":"bedrock-2023-05-31","messages":[]}`, true},
		{"request messages plus max_tokens", `{"model":"claude-sonnet-4-6","max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`, true},
		{"request messages without max_tokens", `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`, false},
		{"error envelope", `{"type":"error","error":{"type":"not_found_error","message":"x"}}`, false},
		{"openai sse", "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\"}\n", false},
		{"gemini sse", "data: {\"candidates\": [{\"content\": {}}]}\n", false},
		{"typed message without stop_reason", `{"type":"message","content":[]}`, false},
		{"plain json", `{"hello":"world"}`, false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := n.LooksLike([]byte(tc.raw), core.Meta{}); got != tc.want {
				t.Fatalf("LooksLike(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
	// Direction-respect: request markers are evidence about a REQUEST
	// body; a response body echoing the words "messages"/"max_tokens"
	// (e.g. an error message quoting the request) must not divert the
	// response probes.
	reqBody := []byte(`{"model":"claude-sonnet-4-6","max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`)
	if n.LooksLike(reqBody, core.Meta{Direction: core.DirectionResponse}) {
		t.Fatal("request markers must not match when Direction=response")
	}
	if !n.LooksLike(reqBody, core.Meta{Direction: core.DirectionRequest}) {
		t.Fatal("request markers must match when Direction=request")
	}
}

func TestOpenAIChatLooksLike(t *testing.T) {
	n := NewOpenAIChatNormalizer()
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"sse chatcmpl chunk", "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\"}\n", true},
		{"nonstream object marker", `{"id":"chatcmpl-1","object":"chat.completion","choices":[]}`, true},
		{"chunk object marker without data prefix", `{"object":"chat.completion.chunk","choices":[]}`, true},
		{"request messages plus model", `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`, true},
		// chatgpt-web requests also carry messages+model but wrap the
		// role in an `author` object — the exclusion keeps them on the
		// Tier-2 chatgpt-web spec that decodes them fully.
		{"chatgpt-web request shape excluded", `{"action":"next","messages":[{"id":"m1","author":{"role":"user"},"content":{"content_type":"text","parts":["hi"]}}],"model":"auto"}`, false},
		{"messages without model", `{"messages":[{"role":"user","content":"hi"}]}`, false},
		// A bare `data:` prefix is shared by EVERY SSE protocol; the
		// sniffer must not treat it as OpenAI on its own.
		{"foreign sse data payload", "data: {\"candidates\": []}\n", false},
		{"anthropic sse", "event: message_start\ndata: {\"type\":\"message_start\"}\n", false},
		// Minimal hand-rolled bodies without the object discriminator
		// stay unclaimed (bounded recall — Tier 2 still handles them).
		{"object-less minimal body", `{"model":"gpt-4o","choices":[]}`, false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := n.LooksLike([]byte(tc.raw), core.Meta{}); got != tc.want {
				t.Fatalf("LooksLike(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
	// Direction-respect: the request probe must stay silent on
	// response-direction bodies.
	reqBody := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	if n.LooksLike(reqBody, core.Meta{Direction: core.DirectionResponse}) {
		t.Fatal("request markers must not match when Direction=response")
	}
	if !n.LooksLike(reqBody, core.Meta{Direction: core.DirectionRequest}) {
		t.Fatal("request markers must match when Direction=request")
	}
}

func TestOpenAIResponsesLooksLike(t *testing.T) {
	n := NewOpenAIResponsesNormalizer()
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"nonstream object marker", `{"id":"resp_1","object":"response","status":"completed","output":[]}`, true},
		{"nonstream object marker spaced", `{"id": "resp_1", "object": "response", "output": []}`, true},
		{"sse response.created first frame", "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{}}\n", true},
		{"sse response.output_text.delta first frame", "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"Hi\"}\n", true},
		// Same key, different value — Chat Completions wires must stay
		// with the openai-chat sniffer.
		{"openai chat nonstream", `{"id":"chatcmpl-1","object":"chat.completion","choices":[]}`, false},
		{"openai chat sse", "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\"}\n", false},
		{"anthropic sse", "event: message_start\ndata: {\"type\":\"message_start\"}\n", false},
		{"gemini sse", "data: {\"candidates\": [{\"content\": {}}]}\n", false},
		// A bare `event:` line whose name is not response.* (foreign SSE)
		// must not match.
		{"foreign sse event name", "event: delta_encoding\ndata: \"v1\"\n", false},
		{"plain json", `{"hello":"world"}`, false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := n.LooksLike([]byte(tc.raw), core.Meta{}); got != tc.want {
				t.Fatalf("LooksLike(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestGeminiGenerateLooksLike(t *testing.T) {
	n := NewGeminiGenerateNormalizer()
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"sse candidates", "data: {\"candidates\": [{\"content\": {}}]}\n", true},
		{"nonstream candidates compact", `{"candidates":[{"content":{}}],"usageMetadata":{}}`, true},
		{"nonstream candidates spaced", `{ "candidates": [ { "content": {} } ] }`, true},
		{"finish-reason-only final chunk", `{"candidates":[{"finishReason":"STOP","index":0}],"usageMetadata":{"totalTokenCount":7}}`, true},
		{"request contents plus generationConfig", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"temperature":0.2}}`, true},
		{"request contents plus systemInstruction", `{"systemInstruction":{"parts":[{"text":"be brief"}]},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`, true},
		{"request contents plus safetySettings", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"safetySettings":[]}`, true},
		// "contents" alone is the same generic-word trap as
		// "candidates" — a document API's contents list must not be
		// claimed without a corroborating Gemini request key.
		{"bare contents without corroborator", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`, false},
		{"foreign contents list", `{"contents":["chapter 1","chapter 2"]}`, false},
		// "candidates" alone is any JSON API's option list — without a
		// corroborating Gemini key the sniffer must not claim it.
		{"foreign candidates list", `{"candidates":["alice","bob"],"total":2}`, false},
		{"foreign candidates objects", `{"candidates":[{"name":"alice"},{"name":"bob"}]}`, false},
		{"openai sse", "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\"}\n", false},
		{"anthropic sse", "event: message_start\ndata: {\"type\":\"message_start\"}\n", false},
		{"plain json", `{"choices":[]}`, false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := n.LooksLike([]byte(tc.raw), core.Meta{}); got != tc.want {
				t.Fatalf("LooksLike(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
	// Direction-respect: the request probe must stay silent on
	// response-direction bodies.
	reqBody := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{}}`)
	if n.LooksLike(reqBody, core.Meta{Direction: core.DirectionResponse}) {
		t.Fatal("request markers must not match when Direction=response")
	}
	if !n.LooksLike(reqBody, core.Meta{Direction: core.DirectionRequest}) {
		t.Fatal("request markers must match when Direction=request")
	}
}

func TestGeminiGenerateLooksLike_ProbeIsBounded(t *testing.T) {
	// The discriminator sits beyond the 512-byte probe window: the
	// sniff must stay O(prefix) and decline rather than scan the body.
	raw := `{"padding":"` + strings.Repeat("x", 600) + `","candidates":[]}`
	if NewGeminiGenerateNormalizer().LooksLike([]byte(raw), core.Meta{}) {
		t.Fatal("marker beyond the probe window must not match")
	}
}

// snifferAllowed maps every conformance corpus case to the set of
// sniffers whose probe MAY match its wire bytes (nil/empty = none may
// claim). The FIRST entry is the case's owner: the sniffer that wins
// the registry walk because registration order (anthropic → openai →
// openai-responses → gemini) tries it first. A second entry encodes a
// KNOWN byte-level ambiguity — today only the request direction, where
// an Anthropic request (messages+model+max_tokens) is a superset of
// the OpenAI request marker pair (messages+model); the walk-order
// discrimination is pinned end-to-end by
// TestRequestSniffOrderDiscrimination and the *-req-keymissed corpus
// goldens. snifferMustMatch lists the cases the owner MUST claim —
// the wires whose goldens (or key-missed twins) rely on the Tier-1.5
// sniff pass. A corpus case missing from snifferAllowed fails the
// test, so every future capture gets classified here explicitly.
var snifferAllowed = map[string][]string{
	"anthropic-error-body":            nil,
	"sse-comment-prefix":              nil, // unknown SSE: no AI codec may sniff-claim it
	"anthropic-messages-request":      {"anthropic", "openai"},
	"anthropic-nonstream-tooluse":     {"anthropic"},
	"anthropic-req-keymissed":         {"anthropic", "openai"},
	"anthropic-sse-text":              {"anthropic"},
	"anthropic-sse-thinking":          {"anthropic"},
	"anthropic-sse-tooluse-bash":      {"anthropic"},
	"anthropic-sse-tooluse-keymissed": {"anthropic"},
	"anthropic-sse-tooluse-only":      {"anthropic"},
	"chatgpt-web-req":                 nil, // messages+model present, but the `author` exclusion keeps openai off it
	"chatgpt-web-resp-sse":            nil,
	"chatgpt-web-resp-sse-noresume":   nil,
	"claude-web-req":                  nil,
	"cursor-connectrpc-resp":          nil,
	"gemini-sse-text":                 {"gemini"},
	"json-candidates-nonai":           nil, // adversarial: top-level "candidates" without any Gemini key — no sniffer may claim it
	"json-messages-nonai":             nil, // adversarial: non-AI "messages"+"model" JSON whose message objects lack role/content — the openai request probe's `author` exclusion keeps it off, and no other sniffer carries its markers
	"json-no-content-type":            nil,
	"ndjson-two-lines":                nil,
	"openai-chat-nonstream-basic":     {"openai"}, // hand-rolled minimal body lacks "object"; sniff recall not required
	"openai-req-keymissed":            {"openai"},
	"openai-sse-text":                 {"openai"},
	"openai-sse-text-keymissed":       {"openai"},
	"openai-sse-toolcalls":            {"openai"},
	"sse-unknown-shape":               nil,
}

var snifferMustMatch = map[string]bool{
	"anthropic-messages-request":      true,
	"anthropic-nonstream-tooluse":     true,
	"anthropic-req-keymissed":         true,
	"anthropic-sse-text":              true,
	"anthropic-sse-thinking":          true,
	"anthropic-sse-tooluse-bash":      true,
	"anthropic-sse-tooluse-keymissed": true,
	"anthropic-sse-tooluse-only":      true,
	"gemini-sse-text":                 true,
	"openai-req-keymissed":            true,
	"openai-sse-text":                 true,
	"openai-sse-text-keymissed":       true,
	"openai-sse-toolcalls":            true,
}

// TestSnifferCrossCorpusPrecision is the precision matrix for the
// Tier-1.5 sniff pass: no sniffer outside a case's allowed set may
// probe-match its corpus wire (a false positive steals key-missed
// traffic from the codec that owns it), and each owner must claim the
// wires the keymissed goldens depend on. Probes run with an unset
// Direction — the loosest meta the registry can pass, so both request
// and response markers are active.
func TestSnifferCrossCorpusPrecision(t *testing.T) {
	sniffers := map[string]core.Sniffer{
		"anthropic":        NewAnthropicMessagesNormalizer(),
		"openai":           NewOpenAIChatNormalizer(),
		"openai-responses": NewOpenAIResponsesNormalizer(),
		"gemini":           NewGeminiGenerateNormalizer(),
	}
	entries, err := os.ReadDir("../conformance/corpus")
	if err != nil {
		t.Fatalf("read corpus dir: %v", err)
	}
	seen := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		wirePath := filepath.Join("../conformance/corpus", name, "wire")
		raw, err := os.ReadFile(wirePath)
		if err != nil {
			t.Fatalf("read %s: %v", wirePath, err)
		}
		allowed, classified := snifferAllowed[name]
		if !classified {
			t.Errorf("corpus case %q is not classified in snifferAllowed — add it with an explicit allowed set", name)
			continue
		}
		seen++
		owner := ""
		if len(allowed) > 0 {
			owner = allowed[0]
		}
		isAllowed := func(sname string) bool {
			for _, a := range allowed {
				if a == sname {
					return true
				}
			}
			return false
		}
		for sname, s := range sniffers {
			got := s.LooksLike(raw, core.Meta{})
			if got && !isAllowed(sname) {
				t.Errorf("%s sniffer claims %q (allowed %v) — precision violation, it would steal key-missed traffic", sname, name, allowed)
			}
			if !got && sname == owner && snifferMustMatch[name] {
				t.Errorf("%s sniffer fails to claim its own wire %q — keymissed golden depends on this match", sname, name)
			}
		}
	}
	if seen == 0 {
		t.Fatal("no corpus cases found — wrong relative path?")
	}
}

// TestRequestSniffOrderDiscrimination pins the walk-order resolution of
// the one known request-direction byte ambiguity: an Anthropic request
// body carries messages+model+max_tokens, which satisfies BOTH the
// anthropic probe (messages+max_tokens) and the openai probe
// (messages+model). The registry registers anthropic's sniffer first,
// so a key-missed Anthropic request must land on anthropic-messages —
// never openai-chat. The symmetric openai request (no max_tokens) must
// land on openai-chat.
func TestRequestSniffOrderDiscrimination(t *testing.T) {
	reg := core.NewRegistry()
	RegisterDefaultAIBuiltins(reg)
	reg.Freeze()

	cases := []struct {
		name         string
		body         string
		wantSpec     string
		wantProtocol string
	}{
		{
			name:         "anthropic request claims anthropic-messages not openai-chat",
			body:         `{"model":"claude-sonnet-4-6","max_tokens":1024,"system":"be brief","messages":[{"role":"user","content":[{"type":"text","text":"What is a gateway?"}]}]}`,
			wantSpec:     "anthropic-messages",
			wantProtocol: "anthropic-messages",
		},
		{
			name:         "openai request claims openai-chat",
			body:         `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"What is a gateway?"}],"temperature":0.7}`,
			wantSpec:     "openai-chat",
			wantProtocol: "openai-chat",
		},
		{
			name:         "gemini request claims gemini-generate",
			body:         `{"contents":[{"role":"user","parts":[{"text":"What is a gateway?"}]}],"generationConfig":{"temperature":0.2}}`,
			wantSpec:     "gemini-generate",
			wantProtocol: "gemini-generate",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Key-missed shape: AdapterType is a host name no Tier-1 key
			// resolves, no endpoint path — only the sniff walk can claim.
			meta := core.Meta{
				AdapterType: "unknown.example.com",
				Direction:   core.DirectionRequest,
			}
			p, err := reg.Normalize(context.Background(), []byte(tc.body), meta)
			if err != nil {
				t.Fatalf("Normalize: %v", err)
			}
			if p.DetectedSpec != tc.wantSpec {
				t.Fatalf("DetectedSpec = %q, want %q", p.DetectedSpec, tc.wantSpec)
			}
			if p.Protocol != tc.wantProtocol {
				t.Fatalf("Protocol = %q, want %q", p.Protocol, tc.wantProtocol)
			}
			if p.Kind != core.KindAIChat {
				t.Fatalf("Kind = %q, want %q", p.Kind, core.KindAIChat)
			}
			if len(p.Messages) == 0 {
				t.Fatal("sniff-claimed request must surface decoded messages")
			}
		})
	}
}
