package proxy

// video_veo_test.go — business-behavior tests for the Veo cross-shape leg
// (e88-s6 §3b): submit translation + correlation on the veo_-encoded id,
// LRO poll → canonical lifecycle (feeding the SAME reconcile machinery),
// the D-V5b guarded URI fetch, and the best-effort local delete.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store/asyncjob"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	geminicodec "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/gemini/codec"
	provtarget "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/target"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
)

// veoStubResolver resolves to a Gemini-format target (the leg dispatch key)
// and records hints.
type veoStubResolver struct {
	mu      sync.Mutex
	baseURL string
	hints   []provtarget.ResolveHints
}

func (s *veoStubResolver) Resolve(_ context.Context, providerID, modelID string, h provtarget.ResolveHints) (provcore.CallTarget, error) {
	s.mu.Lock()
	s.hints = append(s.hints, h)
	s.mu.Unlock()
	return provcore.CallTarget{
		ProviderID:      providerID,
		ProviderName:    "gemini",
		Format:          provcore.FormatGemini,
		BaseURL:         s.baseURL,
		APIKey:          "gk-upstream",
		CredentialID:    "cred-veo",
		CredentialName:  "veo-cred",
		ProviderModelID: modelID + "-provider",
	}, nil
}

// veoUpstream records the wire the provider saw and serves a canned body.
type veoUpstream struct {
	srv     *httptest.Server
	mu      sync.Mutex
	hits    int
	method  string
	uri     string
	body    []byte
	apiKey  string
	ct      string
	status  int
	payload string
}

func newVeoUpstream(t *testing.T, status int, payload string) *veoUpstream {
	t.Helper()
	u := &veoUpstream{status: status, payload: payload}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.hits++
		u.method = r.Method
		u.uri = r.RequestURI
		u.body = b
		u.apiKey = r.Header.Get("x-goog-api-key")
		u.ct = r.Header.Get("Content-Type")
		u.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(u.status)
		_, _ = w.Write([]byte(u.payload))
	}))
	t.Cleanup(u.srv.Close)
	return u
}

// veoDeps wires the submit harness onto a Gemini-format target.
func veoDeps(t *testing.T, upstreamURL string) (*Deps, *fakeJobStore, *captureProducer, *veoStubResolver) {
	t.Helper()
	deps, js, prod := videoDeps(t, upstreamURL)
	deps.Router = &stubRouterCacheTest{targets: []routingcore.RoutingTarget{{
		ProviderID:      "p-gemini",
		ProviderName:    "gemini",
		ModelID:         "veo-3.1",
		ModelName:       "Veo 3.1",
		ProviderModelID: "veo-3.1-provider",
		AdapterType:     "gemini",
	}}}
	res := &veoStubResolver{baseURL: upstreamURL}
	deps.Resolver = res
	return deps, js, prod, res
}

// veoOwnedJob is a Veo-leg job row (id = encoded operation name).
func veoOwnedJob() *asyncjob.Job {
	j := ownedVideoJob()
	j.ProviderID = "p-gemini"
	j.ID = geminicodec.VeoJobID("models/veo-3.1/operations/op1")
	j.ModelID = "veo-3.1"
	j.CredentialID = "cred-veo"
	j.CreatedAt = time.Date(2026, 7, 17, 10, 0, 0, 0, time.UTC)
	return j
}

// --- submit -------------------------------------------------------------------

// A Gemini-format target dispatches the Veo leg: the multipart canonical
// submit translates to the :predictLongRunning JSON wire, correlates on the
// veo_-encoded operation name, stamps the estimate, and answers with the
// canonical video object + the lossy-size coercion marker.
func TestServeVideoSubmit_VeoLeg(t *testing.T) {
	up := newVeoUpstream(t, http.StatusOK, `{"name":"models/veo-3.1/operations/op1","done":false}`)
	deps, js, prod, _ := veoDeps(t, up.srv.URL)
	h := NewHandler(deps).ServeVideoSubmit(videoIngress())

	buf := &strings.Builder{}
	mw := multipart.NewWriter(buf)
	_ = mw.WriteField("prompt", "a golden retriever surfing")
	_ = mw.WriteField("model", "veo-3.1")
	_ = mw.WriteField("seconds", "8")
	_ = mw.WriteField("size", "1792x1024")
	_ = mw.Close()

	rr := doVideo(h, mw.FormDataContentType(), []byte(buf.String()))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", rr.Code, rr.Body.String())
	}

	// Wire: the provider saw the translated JSON on the LRO endpoint with its
	// own auth scheme.
	if up.method != http.MethodPost || up.uri != "/v1beta/models/veo-3.1-provider:predictLongRunning" {
		t.Errorf("upstream saw %s %s, want POST /v1beta/models/veo-3.1-provider:predictLongRunning", up.method, up.uri)
	}
	if up.apiKey != "gk-upstream" || up.ct != "application/json" {
		t.Errorf("upstream auth/ct = (%q, %q), want (gk-upstream, application/json)", up.apiKey, up.ct)
	}
	var wire map[string]any
	_ = json.Unmarshal(up.body, &wire)
	params, _ := wire["parameters"].(map[string]any)
	if params == nil || params["durationSeconds"] != float64(8) || params["aspectRatio"] != "16:9" || params["resolution"] != "1080p" {
		t.Errorf("wire parameters = %v", params)
	}

	// Client: canonical object on the veo_ id + the coercion marker; the
	// model is the RESOLVED gateway id (consistent with the poll response),
	// progress/seconds present for OpenAI-SDK parity.
	var obj map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &obj)
	wantID := geminicodec.VeoJobID("models/veo-3.1/operations/op1")
	if obj["id"] != wantID || obj["object"] != "video" || obj["status"] != "in_progress" ||
		obj["model"] != "veo-3.1" || obj["progress"] != float64(0) || obj["seconds"] != float64(8) {
		t.Errorf("client object = %v", obj)
	}
	if got := rr.Header().Get("X-Nexus-Coerced"); !strings.Contains(got, "size:1792x1024") {
		t.Errorf("X-Nexus-Coerced = %q, want the lossy size marker", got)
	}

	// Correlation + cost: the job row carries the encoded id and the submit
	// row stamps the 8 s × $0.10 estimate.
	if len(js.inserted) != 1 {
		t.Fatalf("inserted %d rows, want 1", len(js.inserted))
	}
	job := js.inserted[0]
	if job.ID != wantID || job.ProviderID != "p-gemini" || job.CredentialID != "cred-veo" ||
		job.RequestedUnits != 8 || job.Status != asyncjob.StatusInProgress || job.ExpiresAt == nil {
		t.Errorf("job row = %+v", job)
	}
	msg := lastAudit(t, deps, prod)
	if !approxEqual(msg.EstimatedCostUsd, 0.8) {
		t.Errorf("EstimatedCostUsd = %v, want 0.8", msg.EstimatedCostUsd)
	}
}

// The Veo submit leg captures its job response symmetric with the OpenAI leg
// (e88-s8 review MEDIUM-1): with payload capture on, the job JSON reaches the
// audit message as an inline response body stamped ActionApprove so the Traffic
// drawer can render it; the multipart video request bytes stay uncaptured (R-7).
func TestServeVideoSubmit_VeoLeg_CapturesResponse(t *testing.T) {
	up := newVeoUpstream(t, http.StatusOK, `{"name":"models/veo-3.1/operations/op1","done":false}`)
	deps, _, prod, _ := veoDeps(t, up.srv.URL)
	deps.PayloadCapture = payloadcapture.NewStore(payloadcapture.Config{StoreResponseBody: true})
	h := NewHandler(deps).ServeVideoSubmit(videoIngress())

	buf := &strings.Builder{}
	mw := multipart.NewWriter(buf)
	_ = mw.WriteField("prompt", "a golden retriever surfing")
	_ = mw.WriteField("model", "veo-3.1")
	_ = mw.WriteField("seconds", "8")
	_ = mw.Close()

	rr := doVideo(h, mw.FormDataContentType(), []byte(buf.String()))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	msg := lastAudit(t, deps, prod)
	if msg.ResponseBody.Kind != audit.BodyInline || len(msg.ResponseBody.InlineBytes) == 0 {
		t.Errorf("Veo job response not captured: kind=%q len=%d", msg.ResponseBody.Kind, len(msg.ResponseBody.InlineBytes))
	}
	// R-7: the multipart video request bytes are never captured.
	if msg.RequestBody.Kind == audit.BodyInline && len(msg.RequestBody.InlineBytes) > 0 {
		t.Errorf("Veo request multipart must not be captured (R-7), got %d bytes", len(msg.RequestBody.InlineBytes))
	}
}

// The Veo leg is allow-list-only: an extra multipart field is the client's
// 400 and never reaches the provider (the OpenAI leg forwards extras — this
// leg cannot translate them).
func TestServeVideoSubmit_VeoExtraField400(t *testing.T) {
	up := newVeoUpstream(t, http.StatusOK, `{}`)
	deps, js, _, _ := veoDeps(t, up.srv.URL)
	h := NewHandler(deps).ServeVideoSubmit(videoIngress())

	buf := &strings.Builder{}
	mw := multipart.NewWriter(buf)
	_ = mw.WriteField("prompt", "a cat")
	_ = mw.WriteField("model", "veo-3.1")
	_ = mw.WriteField("style", "anime") // unknown extra
	_ = mw.Close()

	rr := doVideo(h, mw.FormDataContentType(), []byte(buf.String()))
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "style") {
		t.Fatalf("resp = %d %q, want 400 naming the field", rr.Code, rr.Body.String())
	}
	if up.hits != 0 || len(js.inserted) != 0 {
		t.Errorf("provider hit or row inserted on a refused submit")
	}
}

// Omitted seconds: the wire carries the exact billed default (4), never an
// omitted durationSeconds that would let Veo render its own (longer) default
// while the gateway bills 4 — the money-honesty invariant three review lenses
// flagged.
func TestServeVideoSubmit_VeoOmittedSecondsPinsWireToBilling(t *testing.T) {
	up := newVeoUpstream(t, http.StatusOK, `{"name":"models/veo-3.1/operations/op1","done":false}`)
	deps, js, prod, _ := veoDeps(t, up.srv.URL)
	h := NewHandler(deps).ServeVideoSubmit(videoIngress())
	body, ct := buildVideoMultipart(t, "a cat", "veo-3.1", "") // no seconds
	rr := doVideo(h, ct, body)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var wire map[string]any
	_ = json.Unmarshal(up.body, &wire)
	params, _ := wire["parameters"].(map[string]any)
	if params == nil || params["durationSeconds"] != float64(4) {
		t.Errorf("wire durationSeconds = %v, want the billed default 4", params)
	}
	if js.inserted[0].RequestedUnits != 4 {
		t.Errorf("job RequestedUnits = %v, want 4", js.inserted[0].RequestedUnits)
	}
	msg := lastAudit(t, deps, prod)
	if !approxEqual(msg.EstimatedCostUsd, 0.4) { // 4 s × $0.10
		t.Errorf("EstimatedCostUsd = %v, want 0.4 (wire and billing agree)", msg.EstimatedCostUsd)
	}
}

// A file part under a name other than input_reference is refused on the
// allow-list-only leg (never silently reinterpreted as the input image).
func TestServeVideoSubmit_VeoWrongFilePartName400(t *testing.T) {
	up := newVeoUpstream(t, http.StatusOK, `{}`)
	deps, js, _, _ := veoDeps(t, up.srv.URL)
	h := NewHandler(deps).ServeVideoSubmit(videoIngress())

	buf := &strings.Builder{}
	mw := multipart.NewWriter(buf)
	_ = mw.WriteField("prompt", "a cat")
	_ = mw.WriteField("model", "veo-3.1")
	fw, _ := mw.CreateFormFile("style_reference", "s.png")
	_, _ = fw.Write([]byte("\x89PNGyyyy"))
	_ = mw.Close()

	rr := doVideo(h, mw.FormDataContentType(), []byte(buf.String()))
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "style_reference") {
		t.Fatalf("resp = %d %q, want 400 naming the file field", rr.Code, rr.Body.String())
	}
	if up.hits != 0 || len(js.inserted) != 0 {
		t.Errorf("provider hit or row inserted on a refused submit")
	}
}

// A hostile provider operation name that decodes unsafe fails at MINT (submit)
// → 502, never a 2xx-confirmed job whose follow-ups all 502.
func TestServeVideoSubmit_VeoUnsafeOperationName502(t *testing.T) {
	// An operation name with a traversal segment survives JSON but fails the
	// VeoOperationName round-trip validation.
	up := newVeoUpstream(t, http.StatusOK, `{"name":"models/../../admin"}`)
	deps, js, _, _ := veoDeps(t, up.srv.URL)
	h := NewHandler(deps).ServeVideoSubmit(videoIngress())
	body, ct := buildVideoMultipart(t, "a cat", "veo-3.1", "")
	rr := doVideo(h, ct, body)
	if rr.Code != http.StatusBadGateway || !strings.Contains(rr.Body.String(), "VIDEO_CORRELATION_FAILED") {
		t.Fatalf("resp = %d %q, want 502 VIDEO_CORRELATION_FAILED", rr.Code, rr.Body.String())
	}
	if len(js.inserted) != 0 {
		t.Errorf("no row may exist for an unsafe operation name")
	}
}

// An OpenAI-leg provider that echoes a reserved veo_-prefixed job id is
// refused correlation — the prefix is the gateway's Veo leg namespace and
// would hijack every follow-up's dispatch.
func TestServeVideoSubmit_OpenAILegRejectsReservedVeoID(t *testing.T) {
	up := videoJobUpstream(t, `{"id":"veo_bW9kZWxz","status":"queued"}`)
	deps, js, _ := videoDeps(t, up.URL) // OpenAI-format target
	h := NewHandler(deps).ServeVideoSubmit(videoIngress())
	body, ct := buildVideoMultipart(t, "a cat", "sora-2", "")
	rr := doVideo(h, ct, body)
	if rr.Code != http.StatusBadGateway || !strings.Contains(rr.Body.String(), "VIDEO_CORRELATION_FAILED") {
		t.Fatalf("resp = %d %q, want 502 VIDEO_CORRELATION_FAILED", rr.Code, rr.Body.String())
	}
	if len(js.inserted) != 0 {
		t.Errorf("no row may exist for a reserved-namespace id")
	}
}

// An unmapped size refuses (the closed lossy map, never a guess).
func TestServeVideoSubmit_VeoUnmappedSize400(t *testing.T) {
	up := newVeoUpstream(t, http.StatusOK, `{}`)
	deps, _, _, _ := veoDeps(t, up.srv.URL)
	h := NewHandler(deps).ServeVideoSubmit(videoIngress())
	body, ct := buildVideoMultipart(t, "a cat", "veo-3.1", "")
	_ = body
	buf := &strings.Builder{}
	mw := multipart.NewWriter(buf)
	_ = mw.WriteField("prompt", "a cat")
	_ = mw.WriteField("model", "veo-3.1")
	_ = mw.WriteField("size", "640x480")
	_ = mw.Close()
	_ = ct
	rr := doVideo(h, mw.FormDataContentType(), []byte(buf.String()))
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "640x480") {
		t.Fatalf("resp = %d %q, want 400 naming the size", rr.Code, rr.Body.String())
	}
	if up.hits != 0 {
		t.Errorf("provider hit on a refused submit")
	}
}

// The provider owns its enums — its rejection is NORMALIZED into the OpenAI
// error envelope (the client speaks OpenAI /v1/videos, not the Gemini wire),
// the status code is preserved, and no job row is created.
func TestServeVideoSubmit_VeoProviderErrorNormalized(t *testing.T) {
	provErr := `{"error":{"code":400,"status":"INVALID_ARGUMENT","message":"durationSeconds must be one of 4, 6, 8"}}`
	up := newVeoUpstream(t, http.StatusBadRequest, provErr)
	deps, js, _, _ := veoDeps(t, up.srv.URL)
	h := NewHandler(deps).ServeVideoSubmit(videoIngress())
	body, ct := buildVideoMultipart(t, "a cat", "veo-3.1", "5")
	rr := doVideo(h, ct, body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want the provider 400 preserved", rr.Code)
	}
	var env map[string]map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	e := env["error"]
	// OpenAI envelope shape: type string + message, NOT the Gemini numeric code.
	if e["type"] != "invalid_request_error" || e["message"] != "durationSeconds must be one of 4, 6, 8" {
		t.Errorf("normalized envelope = %v, want OpenAI shape with the provider message", e)
	}
	if _, ok := e["code"].(float64); ok {
		t.Errorf("numeric Gemini code leaked into the OpenAI envelope: %v", e["code"])
	}
	if len(js.inserted) != 0 {
		t.Errorf("no row may exist for a rejected submit")
	}
}

// A provider 2xx without an operation name cannot be correlated — 502 with
// the orphan flagged, exactly the OpenAI leg's no-id posture.
func TestServeVideoSubmit_VeoNoOperationName502(t *testing.T) {
	up := newVeoUpstream(t, http.StatusOK, `{"done":false}`)
	deps, js, _, _ := veoDeps(t, up.srv.URL)
	h := NewHandler(deps).ServeVideoSubmit(videoIngress())
	body, ct := buildVideoMultipart(t, "a cat", "veo-3.1", "")
	rr := doVideo(h, ct, body)
	if rr.Code != http.StatusBadGateway || !strings.Contains(rr.Body.String(), "VIDEO_CORRELATION_FAILED") {
		t.Fatalf("resp = %d %q, want 502 VIDEO_CORRELATION_FAILED", rr.Code, rr.Body.String())
	}
	if len(js.inserted) != 0 {
		t.Errorf("no row may exist without an operation name")
	}
}

// --- poll ---------------------------------------------------------------------

// A veo_ job polls the LRO and relays the canonical translation; the
// last-observed cache records the mapped status.
func TestServeVideoPoll_VeoLegInProgress(t *testing.T) {
	up := newVeoUpstream(t, http.StatusOK, `{"name":"models/veo-3.1/operations/op1","done":false}`)
	deps, js, prod, res := videoFollowDeps(t, up.srv.URL)
	veoRes := &veoStubResolver{baseURL: up.srv.URL}
	deps.Resolver = veoRes
	_ = res
	js.owned = veoOwnedJob()
	h := NewHandler(deps).ServeVideoPoll(videoIngress())

	rr := doVideoFollow(h, http.MethodGet, js.owned.ID)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	if up.method != http.MethodGet || up.uri != "/v1beta/models/veo-3.1/operations/op1" {
		t.Errorf("upstream saw %s %s, want the decoded operation path", up.method, up.uri)
	}
	if len(veoRes.hints) != 1 || veoRes.hints[0].CredentialID != "cred-veo" {
		t.Errorf("resolve hints = %+v, want the credential pin", veoRes.hints)
	}
	var obj map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &obj)
	if obj["status"] != "in_progress" || obj["id"] != js.owned.ID {
		t.Errorf("client object = %v", obj)
	}
	if len(js.observed) != 1 || js.observed[0].Status != asyncjob.StatusInProgress || js.claims != 0 {
		t.Errorf("observed = %+v claims=%d", js.observed, js.claims)
	}
	msg := lastAudit(t, deps, prod)
	if msg.EstimatedCostUsd != 0 {
		t.Errorf("poll row cost = %v, want $0", msg.EstimatedCostUsd)
	}
}

// The first completed observation reconciles on the seconds×price floor —
// identical machinery to the OpenAI leg, driven by the synthesized canonical
// body.
func TestServeVideoPoll_VeoCompletedReconcilesFloor(t *testing.T) {
	lro := `{"done":true,"response":{"generateVideoResponse":{"generatedSamples":[{"video":{"uri":"https://generativelanguage.googleapis.com/f/x"}}]}}}`
	up := newVeoUpstream(t, http.StatusOK, lro)
	deps, js, _, _ := videoFollowDeps(t, up.srv.URL)
	deps.Resolver = &veoStubResolver{baseURL: up.srv.URL}
	js.owned = veoOwnedJob()
	js.claimResult = true
	engine, uc := videoQuotaHarness(t, 1_000_000)
	deps.QuotaEngine = engine
	h := NewHandler(deps).ServeVideoPoll(videoIngress())

	rr := doVideoFollow(h, http.MethodGet, js.owned.ID)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if js.claims != 1 || len(js.observed) != 1 || js.observed[0].Status != asyncjob.StatusCompleted {
		t.Fatalf("claims=%d observed=%+v", js.claims, js.observed)
	}
	if got := vkUsageCents(t, uc); got != 80 {
		t.Errorf("live debit = %d cents, want 80 (8 s × $0.10)", got)
	}
}

// A malformed veo_ id (hostile mint) refuses before any resolve or forward.
// A stored id that no longer decodes is OUR fault, not the provider's:
// submit validates the same round-trip before storing and refuses the job
// otherwise, so this state can only be reached if the stored copy diverged
// after that. The 502 it used to answer sent the operator to the provider,
// and its hint blamed a malformed provider name that submit had already
// ruled out.
//
// The safety property is unchanged and still asserted: a hostile id never
// reaches the upstream.
func TestServeVideoPoll_VeoMalformedStoredID_IsOursNotTheProviders(t *testing.T) {
	up := newVeoUpstream(t, http.StatusOK, `{}`)
	deps, js, _, _ := videoFollowDeps(t, up.srv.URL)
	js.owned = veoOwnedJob()
	js.owned.ID = "veo_!!!not-base64"
	h := NewHandler(deps).ServeVideoPoll(videoIngress())
	rr := doVideoFollow(h, http.MethodGet, js.owned.ID)
	if rr.Code == http.StatusBadGateway {
		t.Fatalf("502 blames the provider for a reference submit already validated: %s", rr.Body.String())
	}
	if rr.Code != http.StatusInternalServerError || !strings.Contains(rr.Body.String(), "VIDEO_JOB_ID_UNSAFE") {
		t.Fatalf("resp = %d %q, want 500 VIDEO_JOB_ID_UNSAFE", rr.Code, rr.Body.String())
	}
	if up.hits != 0 {
		t.Errorf("upstream hit with a hostile id")
	}
}

// --- content ------------------------------------------------------------------

func TestServeVideoContent_VeoNotReadyAndVariant(t *testing.T) {
	up := newVeoUpstream(t, http.StatusOK, `{"done":false}`)
	deps, js, _, _ := videoFollowDeps(t, up.srv.URL)
	deps.Resolver = &veoStubResolver{baseURL: up.srv.URL}
	js.owned = veoOwnedJob()
	h := NewHandler(deps).ServeVideoContent(videoIngress())

	// Unfinished render → 400 with the poll hint.
	rr := doVideoFollow(h, http.MethodGet, js.owned.ID)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "VIDEO_NOT_READY") {
		t.Fatalf("resp = %d %q, want 400 VIDEO_NOT_READY", rr.Code, rr.Body.String())
	}

	// Veo has no thumbnail/spritesheet variants (§3b capability difference).
	req := httptest.NewRequest(http.MethodGet, "/v1/videos/"+js.owned.ID+"/content?variant=thumbnail", nil)
	req.SetPathValue("id", js.owned.ID)
	rr2 := httptest.NewRecorder()
	h(rr2, req)
	if rr2.Code != http.StatusBadRequest || !strings.Contains(rr2.Body.String(), "VIDEO_VARIANT_UNSUPPORTED") {
		t.Fatalf("variant resp = %d %q, want 400 VIDEO_VARIANT_UNSUPPORTED", rr2.Code, rr2.Body.String())
	}
}

// A completed LRO whose URI fails the D-V5b guard (scheme/host) is a 502 that
// never leaks the attempted URL.
func TestServeVideoContent_VeoFetchRefused(t *testing.T) {
	lro := `{"done":true,"response":{"generateVideoResponse":{"generatedSamples":[{"video":{"uri":"http://127.0.0.1:9/x"}}]}}}`
	up := newVeoUpstream(t, http.StatusOK, lro)
	deps, js, _, _ := videoFollowDeps(t, up.srv.URL)
	deps.Resolver = &veoStubResolver{baseURL: up.srv.URL}
	js.owned = veoOwnedJob()
	h := NewHandler(deps).ServeVideoContent(videoIngress())

	rr := doVideoFollow(h, http.MethodGet, js.owned.ID)
	if rr.Code != http.StatusBadGateway || !strings.Contains(rr.Body.String(), "VIDEO_ARTIFACT_FETCH_FAILED") {
		t.Fatalf("resp = %d %q, want 502 VIDEO_ARTIFACT_FETCH_FAILED", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "127.0.0.1") {
		t.Errorf("the refused URL leaked to the client")
	}
}

// A completed artifact streams through the SAME hygiene relay as the OpenAI
// leg (allowlist CT, fingerprint, nosniff) — the fetch seam is stubbed
// because the host allow-list makes a loopback upstream unreachable by
// design.
func TestServeVideoContent_VeoStreams(t *testing.T) {
	lro := `{"done":true,"response":{"generateVideoResponse":{"generatedSamples":[{"video":{"uri":"https://generativelanguage.googleapis.com/f/x:download"}}]}}}`
	up := newVeoUpstream(t, http.StatusOK, lro)
	deps, js, prod, _ := videoFollowDeps(t, up.srv.URL)
	deps.Resolver = &veoStubResolver{baseURL: up.srv.URL}
	js.owned = veoOwnedJob()

	oldFetch := doFetchVeoArtifact
	var fetchedURI, fetchedKey string
	doFetchVeoArtifact = func(_ context.Context, uri, key string) (*http.Response, error) {
		fetchedURI, fetchedKey = uri, key
		h := http.Header{}
		h.Set("Content-Type", "video/mp4")
		return &http.Response{
			StatusCode: http.StatusOK, Header: h, ContentLength: -1,
			Body: &stubBody{data: []byte("VEO-MP4-BYTES"), err: io.EOF},
		}, nil
	}
	t.Cleanup(func() { doFetchVeoArtifact = oldFetch })

	h := NewHandler(deps).ServeVideoContent(videoIngress())
	rr := doVideoFollow(h, http.MethodGet, js.owned.ID)
	if rr.Code != http.StatusOK || rr.Body.String() != "VEO-MP4-BYTES" {
		t.Fatalf("relay = %d %q", rr.Code, rr.Body.String())
	}
	if !strings.HasPrefix(fetchedURI, "https://generativelanguage") || fetchedKey != "gk-upstream" {
		t.Errorf("fetch = (%q, %q), want the LRO URI with the pinned key", fetchedURI, fetchedKey)
	}
	if got := rr.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("nosniff missing")
	}
	msg := lastAudit(t, deps, prod)
	if msg.ArtifactRefs == "" || !strings.Contains(msg.ArtifactRefs, "video/mp4") {
		t.Errorf("artifact_refs = %s, want the fingerprint", msg.ArtifactRefs)
	}
}

// --- delete -------------------------------------------------------------------

// Veo delete is best-effort local: the row marks canceled, the client gets
// the canonical deletion object, and NO provider call happens (LROs are not
// client-deletable — the render completes provider-side).
func TestServeVideoDelete_VeoBestEffortLocal(t *testing.T) {
	up := newVeoUpstream(t, http.StatusOK, `{}`)
	deps, js, _, _ := videoFollowDeps(t, up.srv.URL)
	deps.Resolver = &veoStubResolver{baseURL: up.srv.URL}
	js.owned = veoOwnedJob()
	h := NewHandler(deps).ServeVideoDelete(videoIngress())

	rr := doVideoFollow(h, http.MethodDelete, js.owned.ID)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var obj map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &obj)
	if obj["deleted"] != true || obj["id"] != js.owned.ID {
		t.Errorf("deletion object = %v", obj)
	}
	if up.hits != 0 {
		t.Errorf("provider hit on a local-only delete")
	}
	if len(js.canceled) != 1 || js.canceled[0][2] != "vk-1" {
		t.Errorf("canceled = %v, want one VK-scoped mark", js.canceled)
	}

	// The mark IS the whole effect — a store failure fails loud.
	deps2, js2, _, _ := videoFollowDeps(t, up.srv.URL)
	js2.owned = veoOwnedJob()
	js2.cancelErr = errors.New("pg down")
	h2 := NewHandler(deps2).ServeVideoDelete(videoIngress())
	if rr := doVideoFollow(h2, http.MethodDelete, js2.owned.ID); rr.Code != http.StatusServiceUnavailable {
		t.Errorf("mark failure → %d, want 503", rr.Code)
	}
}

// A provider 2xx the store cannot correlate is a 502 with the orphan flagged
// (Veo parity with the OpenAI leg's insert-failure arm), and an unreachable
// upstream is a plain 502 before any correlation.
func TestServeVideoSubmit_VeoInsertFailureAndUnreachable(t *testing.T) {
	up := newVeoUpstream(t, http.StatusOK, `{"name":"models/veo-3.1/operations/op1","done":false}`)
	deps, js, _, _ := veoDeps(t, up.srv.URL)
	js.insertErr = errors.New("pg down")
	h := NewHandler(deps).ServeVideoSubmit(videoIngress())
	body, ct := buildVideoMultipart(t, "a cat", "veo-3.1", "")
	rr := doVideo(h, ct, body)
	if rr.Code != http.StatusBadGateway || !strings.Contains(rr.Body.String(), "VIDEO_CORRELATION_FAILED") {
		t.Fatalf("insert failure → %d %q, want 502 VIDEO_CORRELATION_FAILED", rr.Code, rr.Body.String())
	}

	deps2, _, _, _ := veoDeps(t, "http://127.0.0.1:1")
	h2 := NewHandler(deps2).ServeVideoSubmit(videoIngress())
	if rr := doVideo(h2, ct, body); rr.Code != http.StatusBadGateway {
		t.Errorf("unreachable upstream → %d, want 502", rr.Code)
	}
}

// LRO poll edges: a provider error is normalized into the OpenAI envelope
// (status preserved), a 2xx that is not a decodable operation is a 502, and
// an unreachable upstream is a 502 — none of them touch the lifecycle cache.
func TestServeVideoPoll_VeoForwardEdges(t *testing.T) {
	provErr := `{"error":{"code":404,"status":"NOT_FOUND","message":"operation not found"}}`
	up := newVeoUpstream(t, http.StatusNotFound, provErr)
	deps, js, _, _ := videoFollowDeps(t, up.srv.URL)
	deps.Resolver = &veoStubResolver{baseURL: up.srv.URL}
	js.owned = veoOwnedJob()
	h := NewHandler(deps).ServeVideoPoll(videoIngress())
	rr := doVideoFollow(h, http.MethodGet, js.owned.ID)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("provider error status = %d, want 404 preserved", rr.Code)
	}
	var env map[string]map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &env)
	if env["error"]["type"] != "invalid_request_error" || env["error"]["message"] != "operation not found" {
		t.Fatalf("provider error = %q, want the OpenAI-normalized envelope", rr.Body.String())
	}

	up2 := newVeoUpstream(t, http.StatusOK, `this is not an operation`)
	deps2, js2, _, _ := videoFollowDeps(t, up2.srv.URL)
	deps2.Resolver = &veoStubResolver{baseURL: up2.srv.URL}
	js2.owned = veoOwnedJob()
	h2 := NewHandler(deps2).ServeVideoPoll(videoIngress())
	rr2 := doVideoFollow(h2, http.MethodGet, js2.owned.ID)
	if rr2.Code != http.StatusBadGateway || !strings.Contains(rr2.Body.String(), "VIDEO_VEO_DECODE_FAILED") {
		t.Fatalf("garbage LRO = %d %q, want 502 VIDEO_VEO_DECODE_FAILED", rr2.Code, rr2.Body.String())
	}

	deps3, js3, _, _ := videoFollowDeps(t, "http://127.0.0.1:1")
	deps3.Resolver = &veoStubResolver{baseURL: "http://127.0.0.1:1"}
	js3.owned = veoOwnedJob()
	h3 := NewHandler(deps3).ServeVideoPoll(videoIngress())
	if rr := doVideoFollow(h3, http.MethodGet, js3.owned.ID); rr.Code != http.StatusBadGateway {
		t.Errorf("unreachable upstream → %d, want 502", rr.Code)
	}
	if len(js.observed)+len(js2.observed)+len(js3.observed) != 0 {
		t.Errorf("lifecycle cache mutated on a non-2xx/undecodable poll")
	}
}

// Content edges on the Veo leg: a failed render → 400 VIDEO_FAILED, a
// completed operation without a URI → 502, and a non-2xx artifact fetch
// relays verbatim.
func TestServeVideoContent_VeoEdges(t *testing.T) {
	upFailed := newVeoUpstream(t, http.StatusOK, `{"done":true,"error":{"code":3,"message":"blocked"}}`)
	deps, js, _, _ := videoFollowDeps(t, upFailed.srv.URL)
	deps.Resolver = &veoStubResolver{baseURL: upFailed.srv.URL}
	js.owned = veoOwnedJob()
	h := NewHandler(deps).ServeVideoContent(videoIngress())
	if rr := doVideoFollow(h, http.MethodGet, js.owned.ID); rr.Code != http.StatusBadRequest ||
		!strings.Contains(rr.Body.String(), "VIDEO_FAILED") {
		t.Errorf("failed render → %d %q, want 400 VIDEO_FAILED", rr.Code, rr.Body.String())
	}

	// done + zero samples = safety-filtered → the codec maps it to FAILED, so
	// the download is a 400 VIDEO_FAILED (not a completed-but-URI-less
	// dead-end), and nothing is billed for the undelivered render.
	upNoURI := newVeoUpstream(t, http.StatusOK, `{"done":true,"response":{"generateVideoResponse":{"generatedSamples":[]}}}`)
	deps2, js2, _, _ := videoFollowDeps(t, upNoURI.srv.URL)
	deps2.Resolver = &veoStubResolver{baseURL: upNoURI.srv.URL}
	js2.owned = veoOwnedJob()
	h2 := NewHandler(deps2).ServeVideoContent(videoIngress())
	if rr := doVideoFollow(h2, http.MethodGet, js2.owned.ID); rr.Code != http.StatusBadRequest ||
		!strings.Contains(rr.Body.String(), "VIDEO_FAILED") {
		t.Errorf("safety-filtered → %d %q, want 400 VIDEO_FAILED", rr.Code, rr.Body.String())
	}

	// The artifact host answers non-2xx (expired signed URL, …) — relayed
	// verbatim via the stubbed fetch seam.
	lro := `{"done":true,"response":{"generateVideoResponse":{"generatedSamples":[{"video":{"uri":"https://storage.googleapis.com/x"}}]}}}`
	up3 := newVeoUpstream(t, http.StatusOK, lro)
	deps3, js3, _, _ := videoFollowDeps(t, up3.srv.URL)
	deps3.Resolver = &veoStubResolver{baseURL: up3.srv.URL}
	js3.owned = veoOwnedJob()
	oldFetch := doFetchVeoArtifact
	doFetchVeoArtifact = func(context.Context, string, string) (*http.Response, error) {
		hh := http.Header{}
		hh.Set("Content-Type", "application/xml")
		return &http.Response{StatusCode: http.StatusForbidden, Header: hh,
			Body: &stubBody{data: []byte("<Error>expired</Error>"), err: io.EOF}}, nil
	}
	t.Cleanup(func() { doFetchVeoArtifact = oldFetch })
	h3 := NewHandler(deps3).ServeVideoContent(videoIngress())
	rr := doVideoFollow(h3, http.MethodGet, js3.owned.ID)
	if rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "expired") {
		t.Errorf("artifact 403 → %d %q, want verbatim relay", rr.Code, rr.Body.String())
	}
}

// The redirect policy: an off-allow-list hop refuses, an allowed hop
// proceeds with the API key STRIPPED, and redirect chains are bounded.
func TestVeoFetchRedirectPolicy(t *testing.T) {
	mk := func(raw string) *http.Request {
		req, err := http.NewRequest(http.MethodGet, raw, nil)
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		req.Header.Set("X-Goog-Api-Key", "gk-secret")
		return req
	}
	check := veoFetchClient.CheckRedirect

	if err := check(mk("https://evil.example.com/loot"), nil); err == nil {
		t.Error("off-allow-list redirect must refuse")
	}
	okReq := mk("https://storage.googleapis.com/bucket/obj")
	if err := check(okReq, make([]*http.Request, 1)); err != nil {
		t.Errorf("allowed redirect refused: %v", err)
	}
	if okReq.Header.Get("X-Goog-Api-Key") != "" {
		t.Error("the API key must be stripped on every redirect hop")
	}
	if err := check(mk("https://storage.googleapis.com/x"), make([]*http.Request, 5)); err == nil {
		t.Error("redirect chains must be bounded")
	}
}

// fetchVeoArtifact front-door validation refuses before any dial.
func TestFetchVeoArtifact_FrontDoorRefusals(t *testing.T) {
	for _, bad := range []string{
		"://not-a-url",
		"http://generativelanguage.googleapis.com/x",
		"https://evil.example.com/x",
	} {
		if _, err := fetchVeoArtifact(context.Background(), bad, "k"); err == nil {
			t.Errorf("fetchVeoArtifact(%q) accepted a refused URI", bad)
		}
	}
}

// Least privilege: the API-key header attaches ONLY on the Gemini API host,
// never on a URI that points straight at the storage host (which authenticates
// via a signed-URL query token). Exercises the real production builder.
func TestFetchVeoArtifact_KeyOnlyOnAPIHost(t *testing.T) {
	apiReq, err := buildVeoArtifactRequest(context.Background(), "https://generativelanguage.googleapis.com/f/x", "gk-secret")
	if err != nil || apiReq.Header.Get("X-Goog-Api-Key") != "gk-secret" {
		t.Errorf("API host: req=%v err=%v, want the key attached", apiReq, err)
	}
	storeReq, err := buildVeoArtifactRequest(context.Background(), "https://storage.googleapis.com/bucket/obj", "gk-secret")
	if err != nil {
		t.Fatalf("storage host build: %v", err)
	}
	if got := storeReq.Header.Get("X-Goog-Api-Key"); got != "" {
		t.Errorf("storage host carried the key (got %q) — signed-URL host must not receive it", got)
	}
	// The builder runs the front-door guard too.
	if _, err := buildVeoArtifactRequest(context.Background(), "https://evil.example.com/x", "k"); err == nil {
		t.Error("builder must refuse an off-allow-list host")
	}
}

// --- fetch guard ---------------------------------------------------------------

func TestVeoURLAllowed(t *testing.T) {
	for _, ok := range []string{
		"https://generativelanguage.googleapis.com/v1beta/files/x:download",
		"https://storage.googleapis.com/bucket/obj",
		"https://sub.storage.googleapis.com/x",
		"https://generativelanguage.googleapis.com:443/x",
	} {
		u := mustParse(t, ok)
		if err := veoURLAllowed(u); err != nil {
			t.Errorf("veoURLAllowed(%q) = %v, want allowed", ok, err)
		}
	}
	for _, bad := range []string{
		"http://generativelanguage.googleapis.com/x",           // plaintext
		"https://user:pw@storage.googleapis.com/x",             // userinfo
		"https://storage.googleapis.com:8443/x",                // exotic port
		"https://evil.example.com/x",                           // foreign host
		"https://xstorage.googleapis.com/x",                    // prefix spoof
		"https://storage.googleapis.com.evil.tld/x",            // suffix spoof
		"https://generativelanguage.googleapis.com.attacker/x", // suffix spoof
	} {
		u := mustParse(t, bad)
		if err := veoURLAllowed(u); err == nil {
			t.Errorf("veoURLAllowed(%q) accepted a hostile URL", bad)
		}
	}
}

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}
