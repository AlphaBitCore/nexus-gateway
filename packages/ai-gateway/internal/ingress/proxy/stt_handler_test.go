package proxy

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/forwardheader"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/generativecaps"
	provbuiltins "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/builtins"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	provtarget "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/target"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// --- STT-local test stubs -------------------------------------------------

// sttStubResolver returns a single fixed CallTarget pointing at the test
// upstream. A non-empty APIKey is required — the OpenAI transport's ApplyAuth
// rejects an empty key.
type sttStubResolver struct {
	baseURL string
	apiKey  string
	err     error
}

func (s sttStubResolver) Resolve(_ context.Context, providerID, modelID string, _ provtarget.ResolveHints) (provcore.CallTarget, error) {
	if s.err != nil {
		return provcore.CallTarget{}, s.err
	}
	return provcore.CallTarget{
		ProviderID:      providerID,
		ProviderName:    "openai",
		Format:          provcore.FormatOpenAI,
		BaseURL:         s.baseURL,
		APIKey:          s.apiKey,
		CredentialID:    "cred-1",
		CredentialName:  "test-cred",
		ProviderModelID: modelID + "-provider",
	}, nil
}

// sttStubModels serves one price row for the metering path.
type sttStubModels struct {
	inputPricePM  float64
	outputPricePM float64
}

func (m sttStubModels) GetModel(_ context.Context, id string) (*store.Model, error) {
	in := m.inputPricePM
	out := m.outputPricePM
	return &store.Model{ID: id, InputPricePM: &in, OutputPricePM: &out}, nil
}
func (m sttStubModels) GetModelByCode(_ context.Context, _ string) (*store.Model, error) {
	return nil, errors.New("unused")
}
func (m sttStubModels) GetModelByCodeOrAlias(_ context.Context, _ string) (*store.Model, error) {
	return nil, errors.New("unused")
}
func (m sttStubModels) ListEnabledModels(_ context.Context) ([]store.Model, error) {
	return nil, nil
}
func (m sttStubModels) FetchModelPricing(_ context.Context, _ []string) ([]store.ModelPricing, error) {
	return nil, nil
}

// rpmVKAuth authenticates with a VK carrying a fixed RPM so the rate-limit
// arm can fire.
type rpmVKAuth struct{ rpm *int }

func (a rpmVKAuth) Authenticate(_ context.Context, _ *http.Request) (*vkauth.VKMeta, error) {
	return &vkauth.VKMeta{ID: "vk-1", Name: "tvk", OrganizationID: "org-1", RateLimitRpm: a.rpm}, nil
}

// --- helpers --------------------------------------------------------------

// buildSTTMultipart returns a multipart body + its Content-Type carrying the
// given model, response_format (omitted when empty), and audio bytes.
func buildSTTMultipart(t *testing.T, model, responseFormat string, audio []byte) ([]byte, string) {
	t.Helper()
	buf := &bytes.Buffer{}
	w := multipart.NewWriter(buf)
	if model != "" {
		if err := w.WriteField("model", model); err != nil {
			t.Fatalf("write model: %v", err)
		}
	}
	if responseFormat != "" {
		if err := w.WriteField("response_format", responseFormat); err != nil {
			t.Fatalf("write response_format: %v", err)
		}
	}
	fw, err := w.CreateFormFile("file", "audio.mp3")
	if err != nil {
		t.Fatalf("create file part: %v", err)
	}
	if _, err := fw.Write(audio); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	return buf.Bytes(), w.FormDataContentType()
}

// sttDeps assembles handler deps wired to the given upstream URL. Returns the
// deps and the capturing audit producer.
func sttDeps(t *testing.T, upstreamURL string) (*Deps, *captureProducer) {
	t.Helper()
	provReg := provcore.NewRegistry()
	provbuiltins.Register(provReg, nil, slog.Default())
	provReg.Freeze()
	prod := &captureProducer{}
	aw := audit.NewWriter(prod, "nexus.event.ai-traffic", nil, slog.Default()).Start()
	deps := &Deps{
		VKAuth: &stubVKAuthCacheTest{meta: &vkauth.VKMeta{ID: "vk-1", Name: "tvk", OrganizationID: "org-1"}},
		Router: &stubRouterCacheTest{targets: []routingcore.RoutingTarget{{
			ProviderID:      "p-openai",
			ProviderName:    "openai",
			ModelID:         "whisper-1",
			ModelName:       "Whisper",
			ProviderModelID: "whisper-1-provider",
			AdapterType:     "openai",
		}}},
		Resolver:       sttStubResolver{baseURL: upstreamURL, apiKey: "sk-upstream"},
		ProviderReg:    provReg,
		Models:         sttStubModels{inputPricePM: 6.0},
		UpstreamClient: &http.Client{},
		AuditWriter:    aw,
		Allowlist:      forwardheader.Default(),
		Logger:         slog.Default(),
	}
	return deps, prod
}

func doSTT(h http.HandlerFunc, path, contentType string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	rr := httptest.NewRecorder()
	h(rr, req)
	return rr
}

// --- pure-helper tests ----------------------------------------------------

func TestSTTFormatSupported(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"", true}, {"json", true}, {"verbose_json", true}, {"text", true},
		{"srt", false}, {"vtt", false}, {"streaming", false}, {"garbage", false},
	} {
		if got := sttFormatSupported(tc.in); got != tc.want {
			t.Errorf("sttFormatSupported(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestSTTAudioSeconds(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantSecs float64
		wantOK   bool
	}{
		{"verbose_json duration", `{"text":"hi","duration":12.5}`, 12.5, true},
		{"json no duration", `{"text":"hi"}`, 0, false},
		{"zero duration", `{"text":"hi","duration":0}`, 0, false},
		{"non-number duration", `{"text":"hi","duration":"x"}`, 0, false},
		{"empty body", ``, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			secs, ok := sttAudioSeconds([]byte(tc.body))
			if ok != tc.wantOK || secs != tc.wantSecs {
				t.Errorf("sttAudioSeconds(%s) = (%v, %v), want (%v, %v)", tc.body, secs, ok, tc.wantSecs, tc.wantOK)
			}
		})
	}
}

func TestSTTUsageTokens(t *testing.T) {
	if got := sttUsageTokens([]byte(`{"usage":{"prompt_tokens":11}}`), "prompt_tokens", "input_tokens"); got != 11 {
		t.Errorf("prompt_tokens = %d, want 11", got)
	}
	if got := sttUsageTokens([]byte(`{"usage":{"input_tokens":7}}`), "prompt_tokens", "input_tokens"); got != 7 {
		t.Errorf("input_tokens fallback = %d, want 7", got)
	}
	if got := sttUsageTokens([]byte(`{"text":"hi"}`), "prompt_tokens", "input_tokens"); got != 0 {
		t.Errorf("absent usage = %d, want 0", got)
	}
}

func TestSTTParseErrorResponse(t *testing.T) {
	status, code, _, _ := sttParseErrorResponse(&http.MaxBytesError{Limit: 26 << 20})
	if status != http.StatusRequestEntityTooLarge || code != "STT_UPLOAD_TOO_LARGE" {
		t.Errorf("MaxBytesError → (%d,%q), want (413, STT_UPLOAD_TOO_LARGE)", status, code)
	}
	status, code, msg, _ := sttParseErrorResponse(errors.New("stt: multipart is missing the required 'file' part"))
	if status != http.StatusBadRequest || code != "STT_BAD_MULTIPART" {
		t.Errorf("generic → (%d,%q), want (400, STT_BAD_MULTIPART)", status, code)
	}
	if !strings.Contains(msg, "missing the required 'file'") {
		t.Errorf("400 message should carry the parser message, got %q", msg)
	}
}

func TestHostOf(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://api.openai.com/v1", "api.openai.com"},
		{"http://127.0.0.1:3050", "127.0.0.1:3050"},
		{"api.host.local", "api.host.local"},
		{"", ""},
	} {
		if got := hostOf(tc.in); got != tc.want {
			t.Errorf("hostOf(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestMeterSTT(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", nil)

	t.Run("duration prices per-second model", func(t *testing.T) {
		h := &Handler{deps: &Deps{Models: sttStubModels{inputPricePM: 6.0}, Logger: slog.Default()}}
		rec := &audit.Record{StatusCode: 200}
		h.meterSTT(rec, []byte(`{"text":"hi","duration":10}`), "whisper-1", req)
		// 10 seconds / 1e6 * 6.0 = 6e-5
		if rec.EstimatedCostUsd <= 0 {
			t.Fatalf("expected non-zero cost, got %v", rec.EstimatedCostUsd)
		}
		want := 10.0 / 1e6 * 6.0
		if !approxEqual(rec.EstimatedCostUsd, want) {
			t.Errorf("cost = %v, want %v", rec.EstimatedCostUsd, want)
		}
	})

	t.Run("usage tokens win over duration", func(t *testing.T) {
		h := &Handler{deps: &Deps{Models: sttStubModels{inputPricePM: 6.0, outputPricePM: 12.0}, Logger: slog.Default()}}
		rec := &audit.Record{StatusCode: 200}
		h.meterSTT(rec, []byte(`{"text":"hi","duration":10,"usage":{"prompt_tokens":1000,"completion_tokens":500}}`), "gpt-4o-transcribe", req)
		if rec.PromptTokens != 1000 || rec.CompletionTokens != 500 {
			t.Fatalf("tokens = (%d,%d), want (1000,500)", rec.PromptTokens, rec.CompletionTokens)
		}
		// Token path: 1000/1e6*6 + 500/1e6*12 = 0.006 + 0.006 = 0.012
		want := 1000.0/1e6*6.0 + 500.0/1e6*12.0
		if !approxEqual(rec.EstimatedCostUsd, want) {
			t.Errorf("cost = %v, want %v (token path must win)", rec.EstimatedCostUsd, want)
		}
	})

	t.Run("no duration no tokens prices zero", func(t *testing.T) {
		h := &Handler{deps: &Deps{Models: sttStubModels{inputPricePM: 6.0}, Logger: slog.Default()}}
		rec := &audit.Record{StatusCode: 200}
		h.meterSTT(rec, []byte(`{"text":"hi"}`), "whisper-1", req)
		if rec.EstimatedCostUsd != 0 {
			t.Errorf("underivable cost = %v, want 0 (honest $0 + WARN, never priced on bytes)", rec.EstimatedCostUsd)
		}
	})
}

// --- handler tests --------------------------------------------------------

func TestServeSTT_AuthReject(t *testing.T) {
	deps, _ := sttDeps(t, "http://127.0.0.1:0")
	deps.VKAuth = stubAuthErrSTT{}
	h := NewHandler(deps).ServeSTT(sttIngress())
	body, ct := buildSTTMultipart(t, "whisper-1", "", []byte("AUDIO"))
	rr := doSTT(h, "/v1/audio/transcriptions", ct, body)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", rr.Code, rr.Body.String())
	}
}

type stubAuthErrSTT struct{}

func (stubAuthErrSTT) Authenticate(_ context.Context, _ *http.Request) (*vkauth.VKMeta, error) {
	return nil, vkauth.ErrMissing
}

func TestServeSTT_RateLimited(t *testing.T) {
	deps, _ := sttDeps(t, "http://127.0.0.1:0")
	rpm := 5
	deps.VKAuth = rpmVKAuth{rpm: &rpm}
	deps.RateLimiter = denyAllRateLimiter{}
	h := NewHandler(deps).ServeSTT(sttIngress())
	body, ct := buildSTTMultipart(t, "whisper-1", "", []byte("AUDIO"))
	rr := doSTT(h, "/v1/audio/transcriptions", ct, body)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rr.Code)
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Error("Retry-After must be set on rate-limit rejection")
	}
}

func TestServeSTT_GenCapReject(t *testing.T) {
	deps, _ := sttDeps(t, "http://127.0.0.1:0")
	handler := NewHandler(deps)
	// Saturate the per-VK STT concurrency cap (default 4) for vk-1.
	caps, _ := generativeCapsForTest()
	for i := range caps {
		if !handler.genConcurrency.Acquire(typology.EndpointKindSTT, "vk-1", caps) {
			t.Fatalf("pre-acquire %d failed", i)
		}
	}
	h := handler.ServeSTT(sttIngress())
	body, ct := buildSTTMultipart(t, "whisper-1", "", []byte("AUDIO"))
	rr := doSTT(h, "/v1/audio/transcriptions", ct, body)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (gen-cap saturated)", rr.Code)
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Error("Retry-After must be set on gen-cap rejection")
	}
}

func TestServeSTT_BadContentType(t *testing.T) {
	deps, _ := sttDeps(t, "http://127.0.0.1:0")
	h := NewHandler(deps).ServeSTT(sttIngress())
	rr := doSTT(h, "/v1/audio/transcriptions", "application/json", []byte(`{"model":"x"}`))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (non-multipart)", rr.Code)
	}
}

func TestServeSTT_BadMultipart(t *testing.T) {
	deps, _ := sttDeps(t, "http://127.0.0.1:0")
	h := NewHandler(deps).ServeSTT(sttIngress())
	// No file part → 400.
	buf := &bytes.Buffer{}
	w := multipart.NewWriter(buf)
	_ = w.WriteField("model", "whisper-1")
	_ = w.Close()
	rr := doSTT(h, "/v1/audio/transcriptions", w.FormDataContentType(), buf.Bytes())
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("missing-file status = %d, want 400", rr.Code)
	}
}

func TestServeSTT_FormatUnsupported(t *testing.T) {
	deps, _ := sttDeps(t, "http://127.0.0.1:0")
	h := NewHandler(deps).ServeSTT(sttIngress())
	body, ct := buildSTTMultipart(t, "whisper-1", "srt", []byte("AUDIO"))
	rr := doSTT(h, "/v1/audio/transcriptions", ct, body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (srt deferred)", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "STT_FORMAT_UNSUPPORTED") {
		t.Errorf("expected STT_FORMAT_UNSUPPORTED, got %s", rr.Body.String())
	}
}

func TestSTTStreamRequested(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"true", true}, {"1", true}, {" TRUE ", true},
		{"", false}, {"false", false}, {"0", false}, {"no", false},
	} {
		if got := sttStreamRequested(tc.in); got != tc.want {
			t.Errorf("sttStreamRequested(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestServeSTT_StreamRejected(t *testing.T) {
	deps, _ := sttDeps(t, "http://127.0.0.1:0")
	h := NewHandler(deps).ServeSTT(sttIngress())
	// Build a multipart carrying stream=true (the transcribe SSE trigger).
	buf := &bytes.Buffer{}
	w := multipart.NewWriter(buf)
	_ = w.WriteField("model", "whisper-1")
	_ = w.WriteField("stream", "true")
	fw, _ := w.CreateFormFile("file", "audio.mp3")
	_, _ = fw.Write([]byte("AUDIO"))
	_ = w.Close()
	rr := doSTT(h, "/v1/audio/transcriptions", w.FormDataContentType(), buf.Bytes())
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (stream=true deferred)", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "STT_FORMAT_UNSUPPORTED") {
		t.Errorf("want STT_FORMAT_UNSUPPORTED, got %s", rr.Body.String())
	}
}

func TestServeSTT_Success(t *testing.T) {
	var gotPath, gotAuth, gotCT, gotModel string
	var gotClientAuth string
	var gotCTCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		gotCTCount = len(r.Header["Content-Type"])
		gotClientAuth = r.Header.Get("X-Client-Secret")
		_ = r.ParseMultipartForm(1 << 20)
		gotModel = r.FormValue("model")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"hello world","duration":10}`))
	}))
	defer srv.Close()

	deps, prod := sttDeps(t, srv.URL)
	aw := deps.AuditWriter
	h := NewHandler(deps).ServeSTT(sttIngress())

	audio := []byte("RIFFAUDIOBYTES")
	body, ct := buildSTTMultipart(t, "whisper-1", "verbose_json", audio)
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", bytes.NewReader(body))
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Authorization", "Bearer client-key") // must NOT leak upstream
	req.Header.Set("X-Client-Secret", "shhh")            // must NOT leak upstream
	rr := httptest.NewRecorder()
	h(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "hello world") {
		t.Errorf("transcript not relayed: %s", rr.Body.String())
	}
	if gotPath != "/v1/audio/transcriptions" {
		t.Errorf("upstream path = %q, want /v1/audio/transcriptions", gotPath)
	}
	if gotAuth != "Bearer sk-upstream" {
		t.Errorf("upstream Authorization = %q, want provider key (client key must not leak)", gotAuth)
	}
	if gotClientAuth != "" {
		t.Errorf("client X-Client-Secret leaked upstream: %q", gotClientAuth)
	}
	if gotModel != "whisper-1-provider" {
		t.Errorf("upstream model = %q, want rewritten whisper-1-provider", gotModel)
	}
	if !strings.HasPrefix(gotCT, "multipart/form-data") {
		t.Errorf("upstream Content-Type = %q, want multipart/form-data", gotCT)
	}
	// Exactly ONE Content-Type must reach the upstream — the fresh re-emit
	// boundary. A duplicate (the client's stale-boundary value re-added by the
	// allowlist) would frame the body against the wrong boundary.
	if gotCTCount != 1 {
		t.Errorf("upstream Content-Type header count = %d, want 1 (no duplicate from the client)", gotCTCount)
	}
	if gotCT == ct {
		t.Errorf("upstream Content-Type = client's original %q; must be the re-emitted boundary", gotCT)
	}

	// Audit row: endpoint_type=stt, artifact_refs (input audio fingerprint),
	// coverage=none, non-zero cost.
	aw.Close()
	prod.mu.Lock()
	msgs := append([][]byte(nil), prod.messages...)
	prod.mu.Unlock()
	if len(msgs) != 1 {
		t.Fatalf("captured %d audit messages, want 1", len(msgs))
	}
	var evt mq.TrafficEventMessage
	if err := json.Unmarshal(msgs[0], &evt); err != nil {
		t.Fatalf("unmarshal audit envelope: %v", err)
	}
	if evt.EndpointType != string(typology.EndpointKindSTT) {
		t.Errorf("EndpointType = %q, want stt", evt.EndpointType)
	}
	if evt.ComplianceCoverage != "none" {
		t.Errorf("ComplianceCoverage = %q, want none (v1a no output scan)", evt.ComplianceCoverage)
	}
	if !strings.Contains(evt.ArtifactRefs, "sha256") {
		t.Errorf("ArtifactRefs missing audio fingerprint: %q", evt.ArtifactRefs)
	}
	want := 10.0 / 1e6 * 6.0
	if !approxEqual(evt.EstimatedCostUsd, want) {
		t.Errorf("EstimatedCostUsd = %v, want %v", evt.EstimatedCostUsd, want)
	}
}

func TestServeSTT_TranslationsPathPreserved(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"hola"}`))
	}))
	defer srv.Close()

	deps, _ := sttDeps(t, srv.URL)
	h := NewHandler(deps).ServeSTT(sttIngress())
	body, ct := buildSTTMultipart(t, "whisper-1", "", []byte("AUDIO"))
	rr := doSTT(h, "/v1/audio/translations", ct, body)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if gotPath != "/v1/audio/translations" {
		t.Errorf("upstream path = %q, want /v1/audio/translations (gap #6 — ingress path preserved)", gotPath)
	}
}

// TestServeSTT_GenCapReleasedOnError proves the panic-safe tail releases the
// per-VK slot on the error path: with the cap at 4, five SEQUENTIAL requests
// against an unreachable upstream must each return 502 (not 429) — every
// request's slot is released before the next runs.
func TestServeSTT_GenCapReleasedOnError(t *testing.T) {
	// Point the resolver at a closed port so the forward errors (502).
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	badURL := closed.URL
	closed.Close() // now nothing listens → Do() errors

	deps, _ := sttDeps(t, badURL)
	h := NewHandler(deps).ServeSTT(sttIngress())
	body, ct := buildSTTMultipart(t, "whisper-1", "", []byte("AUDIO"))
	for i := range 5 {
		rr := doSTT(h, "/v1/audio/transcriptions", ct, body)
		if rr.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d got 429 — slot leaked (not released on error)", i)
		}
		if rr.Code != http.StatusBadGateway {
			t.Fatalf("request %d status = %d, want 502", i, rr.Code)
		}
	}
}

// sttEmptyRouter resolves to zero targets (no provider serves the model).
type sttEmptyRouter struct{}

func (sttEmptyRouter) ResolveTargets(_ context.Context, _ *routingcore.RoutingContext) (*routingcore.RouteResult, error) {
	return &routingcore.RouteResult{Dispatch: nil}, nil
}

func TestServeSTT_NoRouteTargets(t *testing.T) {
	deps, _ := sttDeps(t, "http://127.0.0.1:0")
	deps.Router = sttEmptyRouter{}
	h := NewHandler(deps).ServeSTT(sttIngress())
	body, ct := buildSTTMultipart(t, "whisper-1", "", []byte("AUDIO"))
	rr := doSTT(h, "/v1/audio/transcriptions", ct, body)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (no route targets — client-correctable, not 5xx)", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "NO_COMPATIBLE_PROVIDER") {
		t.Errorf("want NO_COMPATIBLE_PROVIDER, got %s", rr.Body.String())
	}
}

// Neither of these has reached an upstream, so neither may answer 502. A
// gateway that reports its own unwired dependency as a provider fault sends
// the operator to the provider's status page and poisons every dashboard
// that counts 502s as provider unavailability.
func TestServeSTT_ResolverNil_IsOursNotTheProviders(t *testing.T) {
	deps, _ := sttDeps(t, "http://127.0.0.1:0")
	deps.Resolver = nil
	h := NewHandler(deps).ServeSTT(sttIngress())
	body, ct := buildSTTMultipart(t, "whisper-1", "", []byte("AUDIO"))
	rr := doSTT(h, "/v1/audio/transcriptions", ct, body)
	if rr.Code == http.StatusBadGateway {
		t.Fatalf("502 blames a provider that was never contacted: %s", rr.Body.String())
	}
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
	// The old code claimed nothing was compatible, when nothing was asked.
	if strings.Contains(rr.Body.String(), "NO_COMPATIBLE_PROVIDER") {
		t.Errorf("an unwired resolver is not a compatibility problem: %s", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "STT_RESOLVER_UNAVAILABLE") {
		t.Errorf("want STT_RESOLVER_UNAVAILABLE, got %s", rr.Body.String())
	}
}

func TestServeSTT_ResolveError_IsOursNotTheProviders(t *testing.T) {
	deps, _ := sttDeps(t, "http://127.0.0.1:0")
	deps.Resolver = sttStubResolver{err: errors.New("credential decrypt failed")}
	h := NewHandler(deps).ServeSTT(sttIngress())
	body, ct := buildSTTMultipart(t, "whisper-1", "", []byte("AUDIO"))
	rr := doSTT(h, "/v1/audio/transcriptions", ct, body)
	if rr.Code == http.StatusBadGateway {
		t.Fatalf("502 blames a provider for our own credential lookup: %s", rr.Body.String())
	}
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "PROVIDER_RESOLVE_FAILED") {
		t.Errorf("want PROVIDER_RESOLVE_FAILED, got %s", rr.Body.String())
	}
}

func TestServeSTT_ProviderAuthFails(t *testing.T) {
	// The OpenAI transport's ApplyAuth errors on an empty API key → 502.
	deps, _ := sttDeps(t, "http://127.0.0.1:0")
	deps.Resolver = sttStubResolver{baseURL: "http://127.0.0.1:0", apiKey: ""}
	h := NewHandler(deps).ServeSTT(sttIngress())
	body, ct := buildSTTMultipart(t, "whisper-1", "", []byte("AUDIO"))
	rr := doSTT(h, "/v1/audio/transcriptions", ct, body)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (empty provider key)", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "PROVIDER_AUTH_FAILED") {
		t.Errorf("want PROVIDER_AUTH_FAILED, got %s", rr.Body.String())
	}
}

func TestServeSTT_UnknownProviderFormat(t *testing.T) {
	// A resolved Format with no registered adapter → 502.
	deps, _ := sttDeps(t, "http://127.0.0.1:0")
	deps.Resolver = sttUnknownFormatResolver{}
	h := NewHandler(deps).ServeSTT(sttIngress())
	body, ct := buildSTTMultipart(t, "whisper-1", "", []byte("AUDIO"))
	rr := doSTT(h, "/v1/audio/transcriptions", ct, body)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (unknown provider format)", rr.Code)
	}
}

type sttUnknownFormatResolver struct{}

func (sttUnknownFormatResolver) Resolve(_ context.Context, providerID, modelID string, _ provtarget.ResolveHints) (provcore.CallTarget, error) {
	return provcore.CallTarget{
		ProviderID:      providerID,
		Format:          provcore.Format("no-such-adapter"),
		BaseURL:         "http://127.0.0.1:0",
		APIKey:          "sk-x",
		ProviderModelID: modelID,
	}, nil
}

func sttIngress() Ingress {
	return Ingress{WireShape: typology.WireShapeOpenAIAudioTranscriptions, BodyFormat: provcore.FormatOpenAI}
}

// generativeCapsForTest returns the STT concurrency cap for saturation tests.
func generativeCapsForTest() (int, bool) {
	c, ok := generativecaps.Lookup(typology.EndpointKindSTT)
	return c.MaxConcurrentPerVK, ok
}
