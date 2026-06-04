package typology

import "testing"

// TestClassifyPath_AIGatewayIngressPaths is the primary behavioral test
// for ClassifyPath. Every path AI Gateway registers a handler on (see
// packages/ai-gateway/cmd/ai-gateway/wiring/routes.go) must classify to
// the correct (EndpointKind, WireShape). A miss here means AIGW
// dispatch would fall back to the unclassified path during Phase 2 — a
// hard production regression.
//
// Paths intentionally NOT covered by AIGW handlers (audio/*, images/*,
// batches) but covered by the CP/Agent passthrough classifier are
// tested in TestClassifyPath_PassthroughOnlyPaths below.
func TestClassifyPath_AIGatewayIngressPaths(t *testing.T) {
	cases := []struct {
		method   string
		path     string
		wantKind EndpointKind
		wantWire WireShape
	}{
		// Chat family
		{"POST", "/v1/chat/completions", EndpointKindChat, WireShapeOpenAIChat},
		{"POST", "/v1/responses", EndpointKindChat, WireShapeOpenAIResponses},
		{"POST", "/v1/messages", EndpointKindChat, WireShapeAnthropicMessages},
		// Azure-shaped chat
		{"POST", "/openai/deployments/my-deployment/chat/completions", EndpointKindChat, WireShapeOpenAIChat},
		// GLM-shaped chat
		{"POST", "/api/paas/v4/chat/completions", EndpointKindChat, WireShapeOpenAIChat},
		// Gemini AI Studio — AIGW registers POST /v1beta/models/{model};
		// /v1/models/... is GET-only catalog and isn't an AIGW handler path
		// (intercepted-only — see PassthroughOnlyPaths below).
		{"POST", "/v1beta/models/gemini-1.5-pro:generateContent", EndpointKindChat, WireShapeGeminiGenerateContent},
		{"POST", "/v1beta/models/gemini-1.5-pro:streamGenerateContent", EndpointKindChat, WireShapeGeminiGenerateContent},

		// Embedding family
		{"POST", "/v1/embeddings", EndpointKindEmbeddings, WireShapeOpenAIEmbeddings},
		{"POST", "/openai/deployments/my-deploy/embeddings", EndpointKindEmbeddings, WireShapeOpenAIEmbeddings},
		{"POST", "/api/paas/v4/embeddings", EndpointKindEmbeddings, WireShapeOpenAIEmbeddings},
		// Gemini AI Studio embedding (POST /v1beta/models/{model}:embedContent
		// hits the same AIGW catch-all handler as generateContent).
		{"POST", "/v1beta/models/embedding-001:embedContent", EndpointKindEmbeddings, WireShapeGeminiEmbedContent},
		{"POST", "/v1beta/models/embedding-001:batchEmbedContents", EndpointKindEmbeddings, WireShapeGeminiEmbedContent},

		// Models catalog (GET, no body) — AIGW registers GET /v1/models
		// and GET /v1/models/{model}.
		{"GET", "/v1/models", EndpointKindModels, WireShapeNone},
		{"GET", "/v1/models/gpt-4o", EndpointKindModels, WireShapeNone},
	}
	for _, c := range cases {
		gotKind, gotWire, ok := ClassifyPath(c.method, c.path)
		if !ok {
			t.Errorf("ClassifyPath(%q, %q) returned ok=false; expected (%v, %v)", c.method, c.path, c.wantKind, c.wantWire)
			continue
		}
		if gotKind != c.wantKind {
			t.Errorf("ClassifyPath(%q, %q) kind = %v, want %v", c.method, c.path, gotKind, c.wantKind)
		}
		if gotWire != c.wantWire {
			t.Errorf("ClassifyPath(%q, %q) wire = %v, want %v", c.method, c.path, gotWire, c.wantWire)
		}
	}
}

// TestClassifyPath_PassthroughOnlyPaths covers paths the
// Compliance Proxy + Agent intercept (transparent MITM) but AI Gateway
// does not register a handler for: OpenAI audio (STT + TTS), OpenAI
// images, OpenAI batches, OpenAI legacy /v1/completions, Cohere embed,
// and Vertex AI generateContent / embedContent (project-scoped path).
func TestClassifyPath_PassthroughOnlyPaths(t *testing.T) {
	cases := []struct {
		method   string
		path     string
		wantKind EndpointKind
		wantWire WireShape
	}{
		// OpenAI legacy text completions
		{"POST", "/v1/completions", EndpointKindChat, WireShapeOpenAICompletionsLegacy},
		// Cohere embed (v1 + v2)
		{"POST", "/v1/embed", EndpointKindEmbeddings, WireShapeCohereEmbed},
		{"POST", "/v2/embed", EndpointKindEmbeddings, WireShapeCohereEmbed},
		// STT
		{"POST", "/v1/audio/transcriptions", EndpointKindSTT, WireShapeOpenAIAudioTranscriptions},
		{"POST", "/v1/audio/translations", EndpointKindSTT, WireShapeOpenAIAudioTranscriptions},
		// TTS
		{"POST", "/v1/audio/speech", EndpointKindTTS, WireShapeOpenAIAudioSpeech},
		// Image generation
		{"POST", "/v1/images/generations", EndpointKindImageGeneration, WireShapeOpenAIImages},
		{"POST", "/v1/images/edits", EndpointKindImageGeneration, WireShapeOpenAIImages},
		{"POST", "/v1/images/variations", EndpointKindImageGeneration, WireShapeOpenAIImages},
		// Batches
		{"POST", "/v1/batches", EndpointKindBatch, WireShapeOpenAIBatches},
		// Vertex generateContent (project-scoped path)
		{"POST", "/v1/projects/my-proj/locations/us-central1/publishers/google/models/gemini-pro:generateContent", EndpointKindChat, WireShapeVertexGenerateContent},
		{"POST", "/v1beta/projects/my-proj/locations/us-central1/publishers/google/models/gemini-1.5:streamGenerateContent", EndpointKindChat, WireShapeVertexGenerateContent},
		// Vertex embedContent
		{"POST", "/v1/projects/my-proj/locations/us-central1/publishers/google/models/text-embedding-004:embedContent", EndpointKindEmbeddings, WireShapeVertexEmbedContent},
		{"POST", "/v1/projects/my-proj/locations/us-central1/publishers/google/models/text-embedding-004:batchEmbedContents", EndpointKindEmbeddings, WireShapeVertexEmbedContent},
	}
	for _, c := range cases {
		gotKind, gotWire, ok := ClassifyPath(c.method, c.path)
		if !ok {
			t.Errorf("ClassifyPath(%q, %q) returned ok=false; expected (%v, %v)", c.method, c.path, c.wantKind, c.wantWire)
			continue
		}
		if gotKind != c.wantKind {
			t.Errorf("ClassifyPath(%q, %q) kind = %v, want %v", c.method, c.path, gotKind, c.wantKind)
		}
		if gotWire != c.wantWire {
			t.Errorf("ClassifyPath(%q, %q) wire = %v, want %v", c.method, c.path, gotWire, c.wantWire)
		}
	}
}

// TestClassifyPath_MethodCaseInsensitive verifies HTTP method matching
// is case-insensitive (per the equalFold helper).
func TestClassifyPath_MethodCaseInsensitive(t *testing.T) {
	for _, method := range []string{"POST", "post", "Post", "pOsT"} {
		gotKind, gotWire, ok := ClassifyPath(method, "/v1/chat/completions")
		if !ok || gotKind != EndpointKindChat || gotWire != WireShapeOpenAIChat {
			t.Errorf("ClassifyPath(%q, /v1/chat/completions) = (%v, %v, %v), want (chat, openai-chat, true)",
				method, gotKind, gotWire, ok)
		}
	}
}

// TestClassifyPath_MethodMismatch verifies a wrong method blocks the
// match. A POST to GET-only /v1/models or a GET to a POST-only path
// returns (_, _, false).
func TestClassifyPath_MethodMismatch(t *testing.T) {
	if _, _, ok := ClassifyPath("POST", "/v1/models"); ok {
		t.Errorf("ClassifyPath(POST, /v1/models) ok=true; /v1/models is GET-only")
	}
	if _, _, ok := ClassifyPath("GET", "/v1/chat/completions"); ok {
		t.Errorf("ClassifyPath(GET, /v1/chat/completions) ok=true; chat completions is POST-only")
	}
}

// TestClassifyPath_Unclassified verifies unknown paths return
// (empty, WireShapeNone, false) — the "unclassified" semantics callers
// rely on for the graceful-degradation default.
func TestClassifyPath_Unclassified(t *testing.T) {
	cases := []struct {
		method string
		path   string
	}{
		{"POST", "/unknown/path"},
		{"POST", "/v1/embed/extra/suffix"},
		{"POST", "/v1/chat"},             // missing /completions
		{"POST", "/v2/chat/completions"}, // wrong version prefix
		{"GET", "/some/other/route"},
		{"POST", ""},
		{"", "/v1/chat/completions"}, // method required for POST patterns
	}
	for _, c := range cases {
		gotKind, gotWire, ok := ClassifyPath(c.method, c.path)
		if ok {
			t.Errorf("ClassifyPath(%q, %q) = (%v, %v, true); want unclassified",
				c.method, c.path, gotKind, gotWire)
		}
		if gotKind != "" {
			t.Errorf("ClassifyPath(%q, %q) kind = %q; want empty on miss", c.method, c.path, gotKind)
		}
		if gotWire != WireShapeNone {
			t.Errorf("ClassifyPath(%q, %q) wire = %q; want WireShapeNone on miss", c.method, c.path, gotWire)
		}
	}
}

// TestClassifyPath_AzureDeploymentPathTolerance verifies the Azure
// wildcard segment accepts any non-slash deployment name, including
// names with dots, underscores, and hyphens (Azure permits these).
func TestClassifyPath_AzureDeploymentPathTolerance(t *testing.T) {
	cases := []string{
		"/openai/deployments/deployment-name/chat/completions",
		"/openai/deployments/dep_with_underscore/chat/completions",
		"/openai/deployments/dep.with.dot/chat/completions",
		"/openai/deployments/a/chat/completions",
		"/openai/deployments/very-long-deployment-name-2024-08-06/chat/completions",
	}
	for _, p := range cases {
		gotKind, gotWire, ok := ClassifyPath("POST", p)
		if !ok || gotKind != EndpointKindChat || gotWire != WireShapeOpenAIChat {
			t.Errorf("ClassifyPath(POST, %q) = (%v, %v, %v); want (chat, openai-chat, true)",
				p, gotKind, gotWire, ok)
		}
	}
	// NOTE: a deployment-name that contains a literal slash (resulting
	// in /openai/deployments/dep/extra/chat/completions) DOES classify
	// today because the glob matcher's "*" can match across slashes when
	// the next pattern literal is itself a "/" — see TestGlobMatch_EdgeCases
	// "/a/*/b/*/c" case. Azure deployment names never contain "/" in
	// production so this is a theoretical false-positive only; matched
	// here to lock the existing matcher semantics during Phase 1 (no
	// semantic change). A stricter single-segment matcher is a candidate
	// Phase-3 follow-up if the false-positive ever surfaces in practice.
	if _, _, ok := ClassifyPath("POST", "/openai/deployments/dep/extra/chat/completions"); !ok {
		t.Errorf("ClassifyPath(POST, ...two-segment-deployment...) ok=false; expected true (matcher allows star-cross-slash when next literal is '/')")
	}
}

// TestGlobMatch_EdgeCases exercises the glob matcher edge cases that
// the path-pattern rules depend on. Tests are pinned at the matcher
// level (not via ClassifyPath) so a regression in glob behavior is
// localized.
func TestGlobMatch_EdgeCases(t *testing.T) {
	cases := []struct {
		pattern string
		s       string
		want    bool
	}{
		// Exact matches
		{"/v1/embeddings", "/v1/embeddings", true},
		{"/v1/embeddings", "/v1/embedding", false},
		{"/v1/embeddings", "/v1/embeddings/extra", false},

		// Trailing star
		{"/prefix/*", "/prefix/a", true},
		// Trailing star matches zero-length remainder (matcher semantic).
		{"/prefix/*", "/prefix/", true},
		// Trailing star cannot cross a slash on its own.
		{"/prefix/*", "/prefix/a/b", false},

		// Mid-segment star
		{"/v1*/models/x", "/v1/models/x", true},
		{"/v1*/models/x", "/v1beta/models/x", true},
		{"/v1*/models/x", "/v2/models/x", false},

		// Wildcard with trailing literal — zero-length star match is allowed.
		{"/v1/models/*:embedContent", "/v1/models/abc:embedContent", true},
		{"/v1/models/*:embedContent", "/v1/models/:embedContent", true},

		// Multiple wildcards
		{"/a/*/b/*/c", "/a/x/b/y/c", true},
		// When the next pattern literal after the star is "/", the star
		// can advance past slashes until the next literal alignment
		// succeeds. Locks the existing matcher semantic.
		{"/a/*/b/*/c", "/a/x/y/b/y/c", true},

		// Pattern longer than string
		{"/v1/chat/completions/extra", "/v1/chat/completions", false},

		// Empty pattern + string
		{"", "", true},
		{"", "x", false},
		{"x", "", false},
	}
	for _, c := range cases {
		if got := globMatch(c.pattern, c.s); got != c.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", c.pattern, c.s, got, c.want)
		}
	}
}

// TestEqualFold_ASCII pins the case-insensitive ASCII compare used by
// the method matcher.
func TestEqualFold_ASCII(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"POST", "post", true},
		{"GET", "get", true},
		{"Patch", "PATCH", true},
		{"POST", "POSTING", false},
		{"GET", "POST", false},
		{"", "", true},
		{"", "x", false},
	}
	for _, c := range cases {
		if got := equalFold(c.a, c.b); got != c.want {
			t.Errorf("equalFold(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
