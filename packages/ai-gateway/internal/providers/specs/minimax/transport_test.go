package minimax_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/minimax"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// Tests in this file close the coverage gap on spec_minimax (binding
// [[unit_test_coverage_95]]). They assert OBSERVABLE behavior on the
// MiniMax adapter surface — the OpenAI-compat URL matrix, GroupId
// forwarding, embeddings/models routing, the auth header contract, the
// Do round-trip, the Probe success/failure matrix — and the NewSpec
// wiring (Format, IdentityCodec passthrough, shared OpenAI stream
// decoder, shared OpenAI error normalizer). MiniMax delegates codec /
// stream / errors to spec_openai (see package doc on spec.go), so the
// MiniMax-specific surface tested here is the Transport + the NewSpec
// wiring + nil-logger defaults.

// NewSpec wiring + nil-logger defaults

// TestNewSpec_Wiring pins the AdapterSpec field-set the MiniMax adapter
// returns: Format = "minimax", and the four delegate seats are non-nil
// (Transport / SchemaCodec / StreamDecoder / ErrorNormalizer). The
// generic specAdapter panics on construction if any of these are nil
// (see spec_adapter.go), so a regression here would also break
// every MiniMax cold-boot.
func TestNewSpec_Wiring(t *testing.T) {
	s := minimax.NewSpec(slog.Default())
	if !s.Valid() {
		t.Fatal("AdapterSpec.Valid() = false; expected fully wired spec")
	}
	if s.Format != provcore.FormatMiniMax {
		t.Errorf("Format = %q, want %q", s.Format, provcore.FormatMiniMax)
	}
	if s.Transport == nil {
		t.Error("Transport must be wired")
	}
	if s.SchemaCodec == nil {
		t.Error("SchemaCodec must be wired (openai.IdentityCodec)")
	}
	if s.StreamDecoder == nil {
		t.Error("StreamDecoder must be wired (openai.NewStreamDecoder)")
	}
	if s.ErrorNormalizer == nil {
		t.Error("ErrorNormalizer must be wired (openai.ErrorNormalizerInstance)")
	}
}

// TestNewSpec_NilLoggerFallback pins the slog.Default() fallback on
// NewSpec so the adapter is safe to construct without explicit logger
// wiring (the production wire path in cmd/ai-gateway always passes a
// real logger, but tests + the registry boot path occasionally pass
// nil).
func TestNewSpec_NilLoggerFallback(t *testing.T) {
	s := minimax.NewSpec(nil)
	if !s.Valid() {
		t.Fatal("nil-logger NewSpec must still produce a valid spec")
	}
	if s.Format != provcore.FormatMiniMax {
		t.Errorf("Format = %q, want %q", s.Format, provcore.FormatMiniMax)
	}
}

// TestNewSpec_SchemaCodec_IdentityPassthrough exercises the wired
// IdentityCodec to confirm MiniMax does NOT mutate canonical OpenAI
// bodies on encode (per the package doc — MiniMax serves the
// OpenAI-compat path so we forward bytes verbatim). A regression here
// would silently rewrite the wire body and either break model lookup
// or trigger MiniMax 400s.
func TestNewSpec_SchemaCodec_IdentityPassthrough(t *testing.T) {
	s := minimax.NewSpec(slog.Default())
	body := []byte(`{"model":"MiniMax-M2","messages":[{"role":"user","content":"hi"}]}`)
	encRes, err := s.SchemaCodec.EncodeRequest(typology.WireShapeOpenAIChat, body, provcore.CallTarget{})
	out := encRes.Body
	rewrites := encRes.Rewrites
	if err != nil {
		t.Fatalf("IdentityCodec.EncodeRequest err = %v", err)
	}
	if string(out) != string(body) {
		t.Errorf("IdentityCodec must passthrough bytes; got %q want %q", out, body)
	}
	// IdentityCodec advertises no field rewrites — that contract is
	// load-bearing for the cache hash + audit pipeline.
	if len(rewrites) != 0 {
		t.Errorf("IdentityCodec must return zero rewrites, got %v", rewrites)
	}
}

// NewTransport nil-logger fallback

// TestNewTransport_NilLoggerFallback pins the slog.Default() fallback
// on the Transport constructor. The HTTP + probe clients are mandatory;
// a regression that dropped specutil.NewHTTPClient or NewProbeClient
// would produce a Transport that nil-panics on the first Do().
func TestNewTransport_NilLoggerFallback(t *testing.T) {
	tr := minimax.NewTransport(nil)
	if tr == nil {
		t.Fatal("NewTransport(nil) returned nil")
	}
}

// BuildURL — full endpoint matrix

// TestBuildURL_AllEndpoints covers every Endpoint arm — chat
// completions (/v1/chat/completions), embeddings (/v1/embeddings),
// models (/v1/models) — plus the trailing-slash trim and the
// unsupported-endpoint default. Per the package doc, MiniMax's
// chatcompletion_pro native shape is NOT routable, so the chat path
// must land on the OpenAI-compat /v1/chat/completions URL.
func TestBuildURL_AllEndpoints(t *testing.T) {
	tr := minimax.NewTransport(slog.Default())
	cases := []struct {
		name     string
		endpoint typology.WireShape
		baseURL  string
		want     string
	}{
		{"chat openai-compat", typology.WireShapeOpenAIChat, "https://api.minimax.io", "https://api.minimax.io/v1/chat/completions"},
		{"chat trailing-slash trim", typology.WireShapeOpenAIChat, "https://api.minimax.io/", "https://api.minimax.io/v1/chat/completions"},
		{"chat mainland host", typology.WireShapeOpenAIChat, "https://api.minimaxi.com", "https://api.minimaxi.com/v1/chat/completions"},
		{"embeddings", typology.WireShapeOpenAIEmbeddings, "https://api.minimax.io", "https://api.minimax.io/v1/embeddings"},
		{"embeddings trailing-slash trim", typology.WireShapeOpenAIEmbeddings, "https://api.minimax.io/", "https://api.minimax.io/v1/embeddings"},
		{"models", typology.WireShapeNone, "https://api.minimax.io", "https://api.minimax.io/v1/models"},
		{"models trailing-slash trim", typology.WireShapeNone, "https://api.minimax.io/", "https://api.minimax.io/v1/models"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tr.BuildURL(provcore.CallTarget{BaseURL: tc.baseURL}, tc.endpoint, false)
			if err != nil {
				t.Fatalf("BuildURL err = %v", err)
			}
			if got != tc.want {
				t.Errorf("BuildURL = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestBuildURL_GroupIdAppendedOnEmbeddings pins that the
// minimax.groupId extra rides the embeddings path as well — MiniMax's
// billing attribution treats embeddings + chat the same and the seeded
// provider template carries one GroupId per tenant.
func TestBuildURL_GroupIdAppendedOnEmbeddings(t *testing.T) {
	tr := minimax.NewTransport(slog.Default())
	got, err := tr.BuildURL(
		provcore.CallTarget{
			BaseURL: "https://api.minimax.io",
			Extras:  map[string]string{"minimax.groupId": "tenant-9"},
		},
		typology.WireShapeOpenAIEmbeddings,
		false,
	)
	if err != nil {
		t.Fatalf("BuildURL err = %v", err)
	}
	if !strings.HasSuffix(got, "?GroupId=tenant-9") {
		t.Errorf("GroupId must be appended on embeddings, got %q", got)
	}
}

// TestBuildURL_NoGroupIdNoQueryString pins the negative path — absent
// minimax.groupId, the URL stays clean (a stray "?GroupId=" query
// string would 400 on MiniMax and would also break URL-equality
// downstream cache lookups).
func TestBuildURL_NoGroupIdNoQueryString(t *testing.T) {
	tr := minimax.NewTransport(slog.Default())
	got, err := tr.BuildURL(provcore.CallTarget{BaseURL: "https://api.minimax.io"}, typology.WireShapeOpenAIChat, false)
	if err != nil {
		t.Fatalf("BuildURL err = %v", err)
	}
	if strings.Contains(got, "?") {
		t.Errorf("URL must have no query string when groupId absent, got %q", got)
	}
}

// TestBuildURL_EmptyBaseErrors_EveryEndpoint pins the empty-BaseURL
// guard across all three real endpoints (chat / embeddings / models).
// Per the package doc, MiniMax has no implicit default host — the
// seeded provider template ships an explicit baseUrl per region (api.
// minimax.io for international, api.minimaxi.com for mainland) and a
// misconfigured row must fail loudly rather than silently route to one
// region.
func TestBuildURL_EmptyBaseErrors_EveryEndpoint(t *testing.T) {
	tr := minimax.NewTransport(slog.Default())
	for _, ep := range []typology.WireShape{typology.WireShapeOpenAIChat, typology.WireShapeOpenAIEmbeddings, typology.WireShapeNone} {
		_, err := tr.BuildURL(provcore.CallTarget{}, ep, false)
		if err == nil {
			t.Errorf("endpoint=%s: empty BaseURL must error", ep)
			continue
		}
		if !strings.Contains(err.Error(), "BaseURL is empty") {
			t.Errorf("endpoint=%s: err must mention BaseURL, got %v", ep, err)
		}
	}
}

// TestBuildURL_UnsupportedEndpoint pins the default switch arm — any
// endpoint outside the chat/embeddings/models trio (responses-api,
// transcriptions, etc.) MUST surface a typed error so the dispatcher
// never silently routes the wrong shape to MiniMax.
func TestBuildURL_UnsupportedEndpoint(t *testing.T) {
	tr := minimax.NewTransport(slog.Default())
	_, err := tr.BuildURL(provcore.CallTarget{BaseURL: "https://api.minimax.io"}, typology.WireShapeOpenAIResponses, false)
	if err == nil {
		t.Fatal("EndpointResponsesAPI must error on MiniMax (unsupported)")
	}
	if !strings.Contains(err.Error(), "unsupported endpoint") {
		t.Errorf("err must mention 'unsupported endpoint', got %v", err)
	}
}

// TestApplyAuth_MissingAPIKey pins the missing-key guard — MiniMax
// rejects unauthenticated calls and we want the error surfaced at the
// adapter layer (before any network dial) so retries don't pile up on
// a fundamentally misconfigured provider row.
func TestApplyAuth_MissingAPIKey(t *testing.T) {
	tr := minimax.NewTransport(slog.Default())
	req, _ := http.NewRequest(http.MethodPost, "https://api.minimax.io/v1/chat/completions", nil)
	err := tr.ApplyAuth(req, provcore.CallTarget{})
	if err == nil {
		t.Fatal("missing APIKey must error")
	}
	if !strings.Contains(err.Error(), "missing API key") {
		t.Errorf("err must mention 'missing API key', got %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("Authorization must NOT be set on error path, got %q", got)
	}
}

// Do — round-trip through httptest

// TestDo_DelegatesToClient drives a real HTTP round-trip through
// httptest to cover Transport.Do. We assert (a) the method + path
// reach the upstream, (b) the body round-trips, and (c) the
// Authorization header set by ApplyAuth rides through Do unchanged.
// MiniMax sees a normal Bearer token on the wire — the GroupId lives
// in the query string, never in a header.
func TestDo_DelegatesToClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q, want /v1/chat/completions", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer mm_test" {
			t.Errorf("Authorization = %q, want Bearer mm_test", got)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"MiniMax-M2"`) {
			t.Errorf("body did not survive Do, got %q", body)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1"}`))
	}))
	defer srv.Close()

	tr := minimax.NewTransport(slog.Default())
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"MiniMax-M2","messages":[]}`))
	if err := tr.ApplyAuth(req, provcore.CallTarget{APIKey: "mm_test"}); err != nil {
		t.Fatalf("ApplyAuth err = %v", err)
	}
	resp, err := tr.Do(context.Background(), req, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("Do err = %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"chatcmpl-1"`) {
		t.Errorf("response body lost: %q", body)
	}
}

// TestDo_CtxCancelSurfaces pins that a cancelled context surfaces an
// error out of Do (rather than blocking on the HTTP client). This is
// load-bearing on the executor's per-request deadline path — if Do
// swallowed ctx cancellation, the executor would never return.
func TestDo_CtxCancelSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	tr := minimax.NewTransport(slog.Default())
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	resp, err := tr.Do(ctx, req, provcore.CallTarget{})
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("cancelled ctx must surface an error from Do")
	}
}

// Probe — full happy/sad matrix

// TestProbe_Success covers the 2xx happy path. The probe hits
// /v1/models with the bearer token and reports OK=true + Detail="ok".
// LatencyMs is non-negative.
func TestProbe_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %q, want /v1/models", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer mm_probe" {
			t.Errorf("Authorization = %q, want Bearer mm_probe", got)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	tr := minimax.NewTransport(slog.Default())
	r, err := tr.Probe(context.Background(), provcore.CallTarget{BaseURL: srv.URL, APIKey: "mm_probe"})
	if err != nil {
		t.Fatalf("Probe err = %v", err)
	}
	if !r.OK {
		t.Errorf("expected OK probe, got %+v", r)
	}
	if r.Detail != "ok" {
		t.Errorf("Detail = %q, want ok", r.Detail)
	}
	if r.LatencyMs < 0 {
		t.Errorf("LatencyMs = %d, must be >= 0", r.LatencyMs)
	}
}

// TestProbe_SuccessWithoutAPIKey covers the "APIKey == ”" branch on
// Probe — the call still goes out, no Authorization header is set, and
// the result reflects whatever MiniMax returns. We assert no Bearer
// header ever rides the wire on the missing-key path (so a partially
// seeded provider row probes without leaking a "Bearer " literal).
func TestProbe_SuccessWithoutAPIKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization must be absent without APIKey, got %q", got)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	tr := minimax.NewTransport(slog.Default())
	r, err := tr.Probe(context.Background(), provcore.CallTarget{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("Probe err = %v", err)
	}
	if !r.OK {
		t.Errorf("expected OK probe, got %+v", r)
	}
}

// TestProbe_EmptyBaseURL pins the empty-BaseURL guard so a
// half-configured target produces a not-OK probe without dialing the
// network.
func TestProbe_EmptyBaseURL(t *testing.T) {
	tr := minimax.NewTransport(slog.Default())
	r, err := tr.Probe(context.Background(), provcore.CallTarget{})
	if err != nil {
		t.Fatalf("Probe err = %v", err)
	}
	if r == nil || r.OK {
		t.Errorf("empty BaseURL must produce not-OK probe, got %+v", r)
	}
	if !strings.Contains(r.Detail, "BaseURL is empty") {
		t.Errorf("Detail must mention BaseURL, got %q", r.Detail)
	}
}

// TestProbe_HTTPFailure covers the non-2xx branch. A 5xx from
// /v1/models marks the probe not-OK but does NOT surface a Go error
// (a returned err would crash the orchestrator's polling loop). The
// Detail string carries the status code.
func TestProbe_HTTPFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	tr := minimax.NewTransport(slog.Default())
	r, err := tr.Probe(context.Background(), provcore.CallTarget{BaseURL: srv.URL, APIKey: "k"})
	if err != nil {
		t.Fatalf("Probe err = %v", err)
	}
	if r.OK {
		t.Errorf("5xx must mark OK=false, got %+v", r)
	}
	if !strings.Contains(r.Detail, "500") {
		t.Errorf("Detail must mention status, got %q", r.Detail)
	}
}

// TestProbe_TransportError covers the dial-failure branch — pointing at
// a closed server records r.Err and OK=false.
func TestProbe_TransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := srv.URL
	srv.Close()

	tr := minimax.NewTransport(slog.Default())
	r, err := tr.Probe(context.Background(), provcore.CallTarget{BaseURL: addr, APIKey: "k"})
	if err != nil {
		t.Fatalf("Probe err = %v", err)
	}
	if r.OK {
		t.Error("closed server must produce not-OK probe")
	}
	if r.Err == nil {
		t.Error("Err must be populated on transport failure")
	}
}

// TestProbe_BadURL covers the http.NewRequestWithContext error branch —
// a base URL containing an illegal control char fails at request
// construction, before any network call.
func TestProbe_BadURL(t *testing.T) {
	tr := minimax.NewTransport(slog.Default())
	r, err := tr.Probe(context.Background(), provcore.CallTarget{BaseURL: "http://\x7f", APIKey: "k"})
	if err != nil {
		t.Fatalf("Probe err = %v", err)
	}
	if r.OK {
		t.Error("malformed URL must produce not-OK probe")
	}
	if r.Err == nil {
		t.Error("Err must be populated on request-construction failure")
	}
}
