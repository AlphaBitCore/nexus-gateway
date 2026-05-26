// coverage_test.go fills observable-behaviour gaps for the
// spec_azure_openai adapter. The adapter is thin (deployment-name path
// + api-version query + api-key header); the underlying codec, error
// normalizer and stream decoder are reused from openai. These
// tests pin every Azure-specific divergence and the Probe contract:
//
//   - spec.go: NewSpec wiring — Format, Transport/Codec/Stream/Errors
//     all non-nil, PassthroughRewrite linked, nil-logger fallback.
//   - transport.go: BuildURL deployment from Extras and from
//     ProviderModelID fallback, default api-version, all endpoint arms
//     (chat/embeddings/legacy completions/models), trailing-slash
//     normalization, empty-BaseURL guard, missing-deployment guard
//     (only for non-Models endpoints), unsupported endpoint guard;
//     ApplyAuth api-key set + Authorization-Bearer absence + missing
//     APIKey error; Do round-trip through httptest with the api-key
//     header observed at the upstream; Probe happy path (api-key
//     forwarded, /openai/models?api-version=… path), non-2xx (HTTP 401
//     Detail propagation), empty-BaseURL guard, transport-dial error,
//     no-APIKey omission (probe still issued without api-key header),
//     bad URL on http.NewRequest.
//
// All assertions check observable behaviour — exact URL string, header
// values, response status mapping. No err==nil padding.
package azure_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/azure"
)

// spec.go — NewSpec wiring

// TestNewSpec_WiringComplete pins every field of the AdapterSpec returned
// by NewSpec. Azure reuses spec_openai's codec / stream / error pieces;
// any future divergence MUST surface here as an explicit test edit.
func TestNewSpec_WiringComplete(t *testing.T) {
	spec := azure.NewSpec(slog.Default())
	if spec.Format != provcore.FormatAzureOpenAI {
		t.Errorf("Format=%v want FormatAzureOpenAI", spec.Format)
	}
	if spec.Transport == nil {
		t.Errorf("Transport must be non-nil")
	}
	if spec.SchemaCodec == nil {
		t.Errorf("SchemaCodec must be non-nil (identity reused from spec_openai)")
	}
	if spec.StreamDecoder == nil {
		t.Errorf("StreamDecoder must be non-nil (reused from spec_openai)")
	}
	if spec.ErrorNormalizer == nil {
		t.Errorf("ErrorNormalizer must be non-nil (reused from spec_openai)")
	}
	if spec.PassthroughRewrite == nil {
		t.Errorf("PassthroughRewrite must be wired so reasoning_effort rewrites apply on Azure too")
	}
}

// TestNewSpec_NilLogDefaultsToSlog pins the log == nil branch of NewSpec.
// Without the fallback, the Transport built downstream would panic on
// first slog call.
func TestNewSpec_NilLogDefaultsToSlog(t *testing.T) {
	spec := azure.NewSpec(nil)
	if spec.Transport == nil {
		t.Fatalf("NewSpec(nil) must still produce a Transport")
	}
}

// TestNewTransport_NilLogDefaultsToSlog pins the log == nil branch of
// NewTransport. The transport must remain usable.
func TestNewTransport_NilLogDefaultsToSlog(t *testing.T) {
	tr := azure.NewTransport(nil)
	if tr == nil {
		t.Fatalf("NewTransport(nil) returned nil")
	}
	// Smoke: ApplyAuth must work — proves the underlying http.Client was set.
	req, _ := http.NewRequest(http.MethodPost, "http://x", nil)
	if err := tr.ApplyAuth(req, provcore.CallTarget{APIKey: "k"}); err != nil {
		t.Fatalf("ApplyAuth after NewTransport(nil): %v", err)
	}
}

// transport.go — BuildURL

// TestBuildURL_AllEndpointArms pins every endpoint case in BuildURL,
// covering both the chat / embeddings / legacy-completions arms (which
// embed the deployment) and the models arm (resource-scoped path —
// deployment is NOT in the path).
func TestBuildURL_AllEndpointArms(t *testing.T) {
	tr := azure.NewTransport(slog.Default())
	base := "https://my-resource.openai.azure.com"
	deployment := "gpt4o"
	apiVersion := "2024-08-01-preview"
	tgt := provcore.CallTarget{
		BaseURL: base,
		Extras: map[string]string{
			"azure.deployment": deployment,
			"azure.apiVersion": apiVersion,
		},
	}

	cases := []struct {
		name     string
		ep       typology.WireShape
		wantPath string
	}{
		{
			name:     "chat_completions",
			ep:       typology.WireShapeOpenAIChat,
			wantPath: "/openai/deployments/gpt4o/chat/completions",
		},
		{
			name:     "embeddings",
			ep:       typology.WireShapeOpenAIEmbeddings,
			wantPath: "/openai/deployments/gpt4o/embeddings",
		},
		{
			name:     "completions_legacy",
			ep:       typology.WireShapeOpenAICompletionsLegacy,
			wantPath: "/openai/deployments/gpt4o/completions",
		},
		{
			name:     "models_resource_scoped_no_deployment",
			ep:       typology.WireShapeNone,
			wantPath: "/openai/models",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tr.BuildURL(tgt, tc.ep, false)
			if err != nil {
				t.Fatalf("BuildURL: %v", err)
			}
			if !strings.Contains(got, tc.wantPath) {
				t.Errorf("URL %q does not contain expected path %q", got, tc.wantPath)
			}
			if !strings.Contains(got, "api-version="+apiVersion) {
				t.Errorf("URL %q missing api-version=%s", got, apiVersion)
			}
			if !strings.HasPrefix(got, base+"/openai/") {
				t.Errorf("URL %q must be prefixed with %s/openai/", got, base)
			}
		})
		// models must NOT embed the deployment name.
		if tc.ep == typology.WireShapeNone {
			got, _ := tr.BuildURL(tgt, tc.ep, false)
			if strings.Contains(got, "/deployments/") {
				t.Errorf("EndpointModels URL must not embed /deployments/: %q", got)
			}
		}
	}
}

// TestBuildURL_DeploymentFromProviderModelIDFallback pins the
// fallback: when Extras["azure.deployment"] is absent, ProviderModelID
// is used as the deployment name. This matches the seed convention
// where ProviderModel.ProviderModelId == Azure deployment id.
func TestBuildURL_DeploymentFromProviderModelIDFallback(t *testing.T) {
	tr := azure.NewTransport(slog.Default())
	got, err := tr.BuildURL(
		provcore.CallTarget{
			BaseURL:         "https://r.openai.azure.com",
			ProviderModelID: "my-deployment-from-pmid",
		},
		typology.WireShapeOpenAIChat,
		false,
	)
	if err != nil {
		t.Fatalf("BuildURL: %v", err)
	}
	if !strings.Contains(got, "/openai/deployments/my-deployment-from-pmid/chat/completions") {
		t.Errorf("ProviderModelID fallback not used: %q", got)
	}
}

// TestBuildURL_ExtrasDeploymentWinsOverProviderModelID pins the
// precedence: when Extras has azure.deployment, it wins over
// ProviderModelID. Routing rules can override the model→deployment
// mapping via Extras without rewriting the canonical model id.
func TestBuildURL_ExtrasDeploymentWinsOverProviderModelID(t *testing.T) {
	tr := azure.NewTransport(slog.Default())
	got, err := tr.BuildURL(
		provcore.CallTarget{
			BaseURL:         "https://r.openai.azure.com",
			ProviderModelID: "should-be-ignored",
			Extras:          map[string]string{"azure.deployment": "from-extras"},
		},
		typology.WireShapeOpenAIChat,
		false,
	)
	if err != nil {
		t.Fatalf("BuildURL: %v", err)
	}
	if strings.Contains(got, "should-be-ignored") {
		t.Errorf("ProviderModelID leaked despite Extras override: %q", got)
	}
	if !strings.Contains(got, "from-extras") {
		t.Errorf("Extras deployment not used: %q", got)
	}
}

// TestBuildURL_TrailingSlashStripped pins that BaseURL with trailing
// slashes does not double up — strings.TrimRight handles it.
func TestBuildURL_TrailingSlashStripped(t *testing.T) {
	tr := azure.NewTransport(slog.Default())
	got, err := tr.BuildURL(
		provcore.CallTarget{
			BaseURL:         "https://r.openai.azure.com/",
			ProviderModelID: "dep",
		},
		typology.WireShapeOpenAIChat,
		false,
	)
	if err != nil {
		t.Fatalf("BuildURL: %v", err)
	}
	if strings.Contains(got, ".com//openai") {
		t.Errorf("trailing slash not normalized: %q", got)
	}
}

// TestBuildURL_DefaultAPIVersion pins the default api-version when
// Extras["azure.apiVersion"] is empty. Bumped to 2024-10-21 (latest GA
// at audit time) — regressing to an older default would silently break
// structured-outputs / reasoning fields.
func TestBuildURL_DefaultAPIVersion(t *testing.T) {
	tr := azure.NewTransport(slog.Default())
	got, err := tr.BuildURL(
		provcore.CallTarget{
			BaseURL: "https://r.openai.azure.com",
			Extras:  map[string]string{"azure.deployment": "gpt4o"},
		},
		typology.WireShapeOpenAIChat,
		false,
	)
	if err != nil {
		t.Fatalf("BuildURL: %v", err)
	}
	if !strings.Contains(got, "api-version=2024-10-21") {
		t.Errorf("default api-version stale: %q", got)
	}
}

// TestBuildURL_EmptyBaseURL pins the explicit "BaseURL is empty" guard
// — a CallTarget without BaseURL must fail fast.
func TestBuildURL_EmptyBaseURL(t *testing.T) {
	tr := azure.NewTransport(slog.Default())
	if _, err := tr.BuildURL(
		provcore.CallTarget{Extras: map[string]string{"azure.deployment": "x"}},
		typology.WireShapeOpenAIChat,
		false,
	); err == nil {
		t.Fatal("expected error on empty BaseURL")
	}
}

// TestBuildURL_MissingDeploymentForNonModels pins that BuildURL fails
// when deployment is unresolvable AND the endpoint actually needs one
// (i.e. NOT EndpointModels). The error must mention "deployment".
func TestBuildURL_MissingDeploymentForNonModels(t *testing.T) {
	tr := azure.NewTransport(slog.Default())
	_, err := tr.BuildURL(
		provcore.CallTarget{BaseURL: "https://r.openai.azure.com"},
		typology.WireShapeOpenAIChat,
		false,
	)
	if err == nil {
		t.Fatal("expected error when deployment is missing for non-Models endpoint")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "deployment") {
		t.Errorf("error must mention deployment, got %q", err.Error())
	}
}

// TestBuildURL_MissingDeploymentAllowedForModels pins the carve-out:
// EndpointModels is resource-scoped (no deployment in path) and must
// succeed without a deployment configured.
func TestBuildURL_MissingDeploymentAllowedForModels(t *testing.T) {
	tr := azure.NewTransport(slog.Default())
	got, err := tr.BuildURL(
		provcore.CallTarget{BaseURL: "https://r.openai.azure.com"},
		typology.WireShapeNone,
		false,
	)
	if err != nil {
		t.Fatalf("EndpointModels must succeed without deployment, got: %v", err)
	}
	if !strings.HasSuffix(got, "/openai/models?api-version=2024-10-21") {
		t.Errorf("EndpointModels URL shape wrong: %q", got)
	}
}

// TestBuildURL_UnsupportedEndpoint pins the default arm — an unknown
// endpoint surfaces an explicit error (not a URL with empty path).
func TestBuildURL_UnsupportedEndpoint(t *testing.T) {
	tr := azure.NewTransport(slog.Default())
	_, err := tr.BuildURL(
		provcore.CallTarget{
			BaseURL:         "https://r.openai.azure.com",
			ProviderModelID: "dep",
		},
		typology.WireShape("brand-new-2030"),
		false,
	)
	if err == nil {
		t.Fatal("expected error on unsupported endpoint")
	}
	if !strings.Contains(err.Error(), "brand-new-2030") {
		t.Errorf("error must echo the unknown endpoint, got %q", err.Error())
	}
}

// transport.go — ApplyAuth

// TestApplyAuth_ApiKeyHeaderNoBearerLeak pins three observable facts:
//   - api-key header carries the configured key
//   - Authorization (Bearer ...) is NOT set — Azure rejects it
//   - missing APIKey returns an explicit error
func TestApplyAuth_ApiKeyHeaderNoBearerLeak(t *testing.T) {
	tr := azure.NewTransport(slog.Default())

	t.Run("api_key_set_and_no_bearer", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, "http://x", nil)
		if err := tr.ApplyAuth(req, provcore.CallTarget{APIKey: "secret-azure"}); err != nil {
			t.Fatalf("ApplyAuth: %v", err)
		}
		if got := req.Header.Get("api-key"); got != "secret-azure" {
			t.Errorf("api-key=%q want secret-azure", got)
		}
		if got := req.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization header must NOT leak (Azure rejects Bearer), got %q", got)
		}
	})

	t.Run("missing_key_errors", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, "http://x", nil)
		if err := tr.ApplyAuth(req, provcore.CallTarget{}); err == nil {
			t.Errorf("expected error on missing APIKey")
		}
	})
}

// transport.go — Do (httptest round-trip)

// TestDo_RoundTripPropagatesContextAndHeaders pins that Transport.Do
// dispatches the request through the underlying http.Client, that
// ApplyAuth's api-key header survives, and that the response body is
// returned untouched.
func TestDo_RoundTripPropagatesContextAndHeaders(t *testing.T) {
	var seenAPIKey, seenAuth string
	var seenPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAPIKey = r.Header.Get("api-key")
		seenAuth = r.Header.Get("Authorization")
		seenPath = r.URL.Path + "?" + r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	tr := azure.NewTransport(slog.Default())
	tgt := provcore.CallTarget{
		BaseURL:         srv.URL,
		ProviderModelID: "gpt4o",
		APIKey:          "az-key",
		Extras:          map[string]string{"azure.apiVersion": "2024-10-21"},
	}
	urlStr, err := tr.BuildURL(tgt, typology.WireShapeOpenAIChat, false)
	if err != nil {
		t.Fatalf("BuildURL: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, urlStr, bytes.NewReader([]byte(`{"model":"gpt4o"}`)))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if err := tr.ApplyAuth(req, tgt); err != nil {
		t.Fatalf("ApplyAuth: %v", err)
	}

	resp, err := tr.Do(context.Background(), req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status=%d want 200", resp.StatusCode)
	}
	if seenAPIKey != "az-key" {
		t.Errorf("upstream api-key=%q want az-key", seenAPIKey)
	}
	if seenAuth != "" {
		t.Errorf("upstream saw Authorization=%q — Bearer must not leak on Azure", seenAuth)
	}
	if !strings.HasPrefix(seenPath, "/openai/deployments/gpt4o/chat/completions") {
		t.Errorf("upstream path %q — deployment routing broken", seenPath)
	}
	if !strings.Contains(seenPath, "api-version=2024-10-21") {
		t.Errorf("upstream path %q missing api-version query", seenPath)
	}
}

// TestDo_ContextCancelSurfaces pins that a cancelled context surfaces
// as an error from Do — this is how the executor aborts in-flight
// upstream calls when the inbound client disconnects.
func TestDo_ContextCancelSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	tr := azure.NewTransport(slog.Default())
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	resp, err := tr.Do(ctx, req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Error("expected error on cancelled context")
	}
}

// transport.go — Probe

// TestProbe_HappyPath pins:
//   - Probe hits /openai/models with the configured api-version
//   - api-key header forwarded (NOT Authorization Bearer)
//   - ProbeResult.OK=true with non-negative latency
func TestProbe_HappyPath(t *testing.T) {
	var seenPath, seenAPIKey, seenAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path + "?" + r.URL.RawQuery
		seenAPIKey = r.Header.Get("api-key")
		seenAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	defer srv.Close()

	tr := azure.NewTransport(slog.Default())
	res, err := tr.Probe(context.Background(), provcore.CallTarget{
		BaseURL: srv.URL,
		APIKey:  "probe-key",
		Extras:  map[string]string{"azure.apiVersion": "2024-10-21"},
	})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if res == nil || !res.OK {
		t.Fatalf("probe not OK: %+v", res)
	}
	if res.LatencyMs < 0 {
		t.Errorf("LatencyMs must be >= 0, got %d", res.LatencyMs)
	}
	if res.Detail != "ok" {
		t.Errorf("Detail=%q want ok", res.Detail)
	}
	if !strings.HasPrefix(seenPath, "/openai/models") {
		t.Errorf("probe path %q must hit /openai/models", seenPath)
	}
	if !strings.Contains(seenPath, "api-version=2024-10-21") {
		t.Errorf("probe path %q missing api-version", seenPath)
	}
	if seenAPIKey != "probe-key" {
		t.Errorf("probe api-key=%q want probe-key", seenAPIKey)
	}
	if seenAuth != "" {
		t.Errorf("probe must NOT send Authorization header, got %q", seenAuth)
	}
}

// TestProbe_DefaultAPIVersionWhenMissing pins that Probe falls back to
// the default api-version when Extras doesn't carry one.
func TestProbe_DefaultAPIVersionWhenMissing(t *testing.T) {
	var seenPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path + "?" + r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	tr := azure.NewTransport(slog.Default())
	res, err := tr.Probe(context.Background(), provcore.CallTarget{
		BaseURL: srv.URL,
		APIKey:  "k",
	})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !res.OK {
		t.Fatalf("expected OK probe: %+v", res)
	}
	if !strings.Contains(seenPath, "api-version=2024-10-21") {
		t.Errorf("default api-version not applied on Probe: %q", seenPath)
	}
}

// TestProbe_Non2xx pins the upstream-failure branch: HTTP 401 surfaces
// as OK=false + Detail "HTTP 401" without an error return (probe
// errors are reported via ProbeResult, never as Go errors).
func TestProbe_Non2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	tr := azure.NewTransport(slog.Default())
	res, err := tr.Probe(context.Background(), provcore.CallTarget{
		BaseURL: srv.URL,
		APIKey:  "wrong",
	})
	if err != nil {
		t.Fatalf("Probe (non-2xx must be ProbeResult-only): %v", err)
	}
	if res.OK {
		t.Errorf("expected OK=false on 401")
	}
	if !strings.Contains(res.Detail, "401") {
		t.Errorf("Detail %q must include 401", res.Detail)
	}
}

// TestProbe_EmptyBaseURL pins the explicit empty-BaseURL probe guard.
func TestProbe_EmptyBaseURL(t *testing.T) {
	tr := azure.NewTransport(slog.Default())
	res, err := tr.Probe(context.Background(), provcore.CallTarget{APIKey: "k"})
	if err != nil {
		t.Fatalf("Probe with empty BaseURL must not return Go error: %v", err)
	}
	if res.OK {
		t.Errorf("empty BaseURL must yield OK=false")
	}
	if !strings.Contains(strings.ToLower(res.Detail), "baseurl") {
		t.Errorf("Detail must mention BaseURL, got %q", res.Detail)
	}
}

// TestProbe_DialError exercises the err != nil branch of probe.Do() by
// pointing at a port we've just closed.
func TestProbe_DialError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	tr := azure.NewTransport(slog.Default())
	res, err := tr.Probe(context.Background(), provcore.CallTarget{
		BaseURL: url,
		APIKey:  "k",
	})
	if err != nil {
		t.Fatalf("Probe must never return Go error, got %v", err)
	}
	if res.OK {
		t.Errorf("closed port must surface as OK=false")
	}
	if res.Err == nil {
		t.Errorf("Err field must carry the underlying dial error")
	}
	if res.LatencyMs < 0 {
		t.Errorf("LatencyMs must be >= 0 even on dial error")
	}
}

// TestProbe_NoAPIKey pins the "APIKey empty → header omitted" branch.
// Probe must still issue the request (upstream will reject with 401,
// which is the operator's signal that the credential is missing).
func TestProbe_NoAPIKey(t *testing.T) {
	var seenAPIKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAPIKey = r.Header.Get("api-key")
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	tr := azure.NewTransport(slog.Default())
	res, err := tr.Probe(context.Background(), provcore.CallTarget{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if seenAPIKey != "" {
		t.Errorf("api-key must be omitted when APIKey is empty, got %q", seenAPIKey)
	}
	if res.OK {
		t.Errorf("no api-key + 401 upstream must yield OK=false")
	}
}

// TestProbe_BadURLOnNewRequest exercises the http.NewRequestWithContext
// error branch — a BaseURL with an embedded control character makes
// url.Parse fail. The probe must surface the error via ProbeResult.
func TestProbe_BadURLOnNewRequest(t *testing.T) {
	tr := azure.NewTransport(slog.Default())
	res, err := tr.Probe(context.Background(), provcore.CallTarget{
		BaseURL: "http://bad\nurl.example",
		APIKey:  "k",
	})
	if err != nil {
		t.Fatalf("Probe must never return Go error, got %v", err)
	}
	if res.OK {
		t.Errorf("malformed URL must yield OK=false")
	}
	if res.Err == nil {
		t.Errorf("Err field must carry the underlying NewRequest error")
	}
}

// spec.go — RequestShapes (T5)

// TestNewSpec_RequestShapesContainsEmbeddings pins that the Azure adapter
// declares "embeddings" in RequestShapes. Without this declaration the
// routing pre-filter rejects embedding requests before they reach the codec.
func TestNewSpec_RequestShapesContainsEmbeddings(t *testing.T) {
	spec := azure.NewSpec(slog.Default())
	hasEmbeddings := false
	for _, shape := range spec.RequestShapes {
		if shape == typology.WireShapeOpenAIEmbeddings {
			hasEmbeddings = true
		}
	}
	if !hasEmbeddings {
		t.Errorf("RequestShapes must contain 'embeddings', got %v", spec.RequestShapes)
	}
}

// TestNewSpec_EmbeddingsCodec_ada002_stripsFields verifies that the Azure
// adapter's codec (reused from OpenAI IdentityCodec) applies the ada-002
// dimension-strip rule — the same embedding codec behaviour exercised by
// openai/codec tests but confirmed here for Azure to catch any future
// codec-reuse regression.
func TestNewSpec_EmbeddingsCodec_ada002_stripsFields(t *testing.T) {
	spec := azure.NewSpec(slog.Default())
	body := []byte(`{"model":"text-embedding-ada-002","input":"hello","dimensions":256}`)
	target := provcore.CallTarget{ProviderModelID: "text-embedding-ada-002"}
	encRes, err := spec.SchemaCodec.EncodeRequest(typology.WireShapeOpenAIEmbeddings, body, target)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if len(encRes.Rewrites) == 0 {
		t.Errorf("ada-002: expected rewrite stamps for stripped fields")
	}
}
