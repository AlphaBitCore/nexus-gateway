package proxy

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"

	"github.com/goccy/go-json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store/asyncjob"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/generativecaps"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/builtins"
	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// --- video-local test stubs -------------------------------------------------

// fakeJobStore records store calls and serves configurable rows. It is the
// handler-facing seam — the SQL semantics live in the asyncjob package's own
// tests. Zero value: empty store (GetOwned → ErrNotFound, count 0).
type fakeJobStore struct {
	mu          sync.Mutex
	nonTerminal int
	countErr    error
	insertErr   error
	inserted    []asyncjob.Job

	// follow-up (poll/delete) seams
	owned       *asyncjob.Job // GetOwned result; nil → ErrNotFound
	ownedErr    error         // overrides owned when set
	ownedArgs   [][3]string   // recorded (id, vk, endpointType)
	observeErr  error
	observed    []asyncjob.Observation
	claimErr    error
	claimResult bool
	claims      int
	cancelErr   error
	canceled    [][3]string // recorded (provider, id, vk)
}

func (f *fakeJobStore) Insert(_ context.Context, j asyncjob.Job) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.insertErr != nil {
		return f.insertErr
	}
	f.inserted = append(f.inserted, j)
	return nil
}
func (f *fakeJobStore) GetByID(context.Context, string, string) (asyncjob.Job, error) {
	return asyncjob.Job{}, asyncjob.ErrNotFound
}
func (f *fakeJobStore) GetOwned(_ context.Context, id, vk, endpointType string) (asyncjob.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ownedArgs = append(f.ownedArgs, [3]string{id, vk, endpointType})
	if f.ownedErr != nil {
		return asyncjob.Job{}, f.ownedErr
	}
	if f.owned == nil {
		return asyncjob.Job{}, asyncjob.ErrNotFound
	}
	return *f.owned, nil
}
func (f *fakeJobStore) MarkObserved(_ context.Context, _, _ string, obs asyncjob.Observation) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.observeErr != nil {
		return f.observeErr
	}
	f.observed = append(f.observed, obs)
	return nil
}
func (f *fakeJobStore) ReconcileOnce(context.Context, string, string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claimErr != nil {
		return false, f.claimErr
	}
	f.claims++
	return f.claimResult, nil
}
func (f *fakeJobStore) MarkCanceled(_ context.Context, provider, id, vk string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cancelErr != nil {
		return f.cancelErr
	}
	f.canceled = append(f.canceled, [3]string{provider, id, vk})
	return nil
}
func (f *fakeJobStore) CountNonTerminal(context.Context, string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.nonTerminal, f.countErr
}
func (f *fakeJobStore) MarkExpired(context.Context, time.Time, time.Time) (int64, error) {
	return 0, nil
}

// buildVideoMultipart assembles a submit body. Empty values omit the field.
func buildVideoMultipart(t *testing.T, prompt, model, seconds string) ([]byte, string) {
	t.Helper()
	buf := &bytes.Buffer{}
	w := multipart.NewWriter(buf)
	for _, f := range []struct{ name, value string }{
		{"prompt", prompt}, {"model", model}, {"seconds", seconds},
	} {
		if f.value == "" {
			continue
		}
		if err := w.WriteField(f.name, f.value); err != nil {
			t.Fatalf("write %s: %v", f.name, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	return buf.Bytes(), w.FormDataContentType()
}

// videoDeps assembles handler deps wired to the given upstream URL with a
// fresh fake job store. Price row: $0.10/s of generated video → 100000 per 1M
// seconds.
func videoDeps(t *testing.T, upstreamURL string) (*Deps, *fakeJobStore, *captureProducer) {
	t.Helper()
	deps, prod := sttDeps(t, upstreamURL)
	deps.Router = &stubRouterCacheTest{targets: []routingcore.RoutingTarget{{
		ProviderID:      "p-openai",
		ProviderName:    "openai",
		ModelID:         "sora-2",
		ModelName:       "Sora 2",
		ProviderModelID: "sora-2-provider",
		AdapterType:     "openai",
	}}}
	deps.Models = sttStubModels{inputPricePM: 100000}
	js := &fakeJobStore{}
	deps.AsyncJobs = js
	return deps, js, prod
}

func doVideo(h http.HandlerFunc, contentType string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/videos", bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	rr := httptest.NewRecorder()
	h(rr, req)
	return rr
}

// videoJobUpstream serves a canned provider job object.
func videoJobUpstream(t *testing.T, jobJSON string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/videos" {
			t.Errorf("upstream path = %q, want /v1/videos", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(jobJSON))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// --- pure-helper tests -------------------------------------------------------

func TestVideoMaxNonTerminalJobs(t *testing.T) {
	if got := videoMaxNonTerminalJobs(slog.Default()); got != videoMaxNonTerminalDefault {
		t.Errorf("default = %d, want %d", got, videoMaxNonTerminalDefault)
	}
	t.Setenv("AI_GATEWAY_VIDEO_MAX_NONTERMINAL_JOBS", "9")
	if got := videoMaxNonTerminalJobs(slog.Default()); got != 9 {
		t.Errorf("env override = %d, want 9", got)
	}
	// A garbage or non-positive value must keep the built-in default — a typo
	// must never disable the spend bound.
	for _, bad := range []string{"zero", "-3", "0"} {
		t.Setenv("AI_GATEWAY_VIDEO_MAX_NONTERMINAL_JOBS", bad)
		if got := videoMaxNonTerminalJobs(slog.Default()); got != videoMaxNonTerminalDefault {
			t.Errorf("garbage %q = %d, want default %d", bad, got, videoMaxNonTerminalDefault)
		}
	}
}

func TestVideoParseErrorResponse(t *testing.T) {
	status, code, _, _ := videoParseErrorResponse(&http.MaxBytesError{Limit: 16 << 20})
	if status != http.StatusRequestEntityTooLarge || code != "VIDEO_UPLOAD_TOO_LARGE" {
		t.Errorf("MaxBytesError → (%d,%q), want (413, VIDEO_UPLOAD_TOO_LARGE)", status, code)
	}
	status, code, msg, _ := videoParseErrorResponse(errors.New("video: multipart is missing the required non-empty 'prompt' field"))
	if status != http.StatusBadRequest || code != "VIDEO_BAD_MULTIPART" {
		t.Errorf("generic → (%d,%q), want (400, VIDEO_BAD_MULTIPART)", status, code)
	}
	if !strings.Contains(msg, "prompt") {
		t.Errorf("400 message should carry the parser message, got %q", msg)
	}
}

func TestVideoJobStatusFromResponse(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{`{"id":"v1","status":"queued"}`, asyncjob.StatusQueued},
		{`{"id":"v1","status":"in_progress"}`, asyncjob.StatusInProgress},
		{`{"id":"v1","status":"completed"}`, asyncjob.StatusCompleted},
		{`{"id":"v1","status":"failed"}`, asyncjob.StatusFailed},
		// Unknown / absent provider states normalize to queued (the job WAS
		// just submitted; the store's vocabulary guard would reject the raw
		// string and orphan the correlation).
		{`{"id":"v1","status":"processing"}`, asyncjob.StatusQueued},
		{`{"id":"v1"}`, asyncjob.StatusQueued},
	} {
		if got := videoJobStatusFromResponse([]byte(tc.body)); got != tc.want {
			t.Errorf("status(%s) = %q, want %q", tc.body, got, tc.want)
		}
	}
}

func TestVideoJobExpiry(t *testing.T) {
	if got := videoJobExpiry([]byte(`{"id":"v1"}`)); got != nil {
		t.Errorf("absent expires_at = %v, want nil", got)
	}
	if got := videoJobExpiry([]byte(`{"expires_at":"soon"}`)); got != nil {
		t.Errorf("non-numeric expires_at = %v, want nil", got)
	}
	got := videoJobExpiry([]byte(`{"expires_at":1789000000}`))
	if got == nil || got.Unix() != 1789000000 {
		t.Errorf("expires_at = %v, want unix 1789000000", got)
	}
}

func TestVideoEstimateUsd(t *testing.T) {
	h := &Handler{deps: &Deps{Models: sttStubModels{inputPricePM: 100000}}}
	req := httptest.NewRequest(http.MethodPost, "/v1/videos", nil)
	// 8 s × $0.10/s ($100000 per 1M seconds) = $0.80, priced.
	if got, priced := h.videoEstimateUsd(req, "sora-2", 8); !approxEqual(got, 0.8) || !priced {
		t.Errorf("estimate = (%v, %v), want (0.8, priced)", got, priced)
	}
	// No Models dependency → $0 but PRICED (fail open — a wiring gap must not
	// 503 every video request under a quota).
	h2 := &Handler{deps: &Deps{}}
	if got, priced := h2.videoEstimateUsd(req, "sora-2", 8); got != 0 || !priced {
		t.Errorf("no-models estimate = (%v, %v), want (0, priced/fail-open)", got, priced)
	}
	// A model with NO price row → unpriced (the missing-row vs explicit-0
	// distinction the cost-cap guard rests on).
	h3 := &Handler{deps: &Deps{Models: videoUnpricedModels{}}}
	if _, priced := h3.videoEstimateUsd(req, "sora-2", 8); priced {
		t.Error("nil InputPricePM must report unpriced")
	}
}

// --- handler tests ------------------------------------------------------------

func TestServeVideoSubmit_NoStore503(t *testing.T) {
	deps, _, _ := videoDeps(t, "http://unused.invalid")
	deps.AsyncJobs = nil
	h := NewHandler(deps)
	body, ct := buildVideoMultipart(t, "a cat", "sora-2", "8")
	rr := doVideo(h.ServeVideoSubmit(videoIngress()), ct, body)
	if rr.Code != http.StatusServiceUnavailable || !strings.Contains(rr.Body.String(), "VIDEO_STORE_UNAVAILABLE") {
		t.Fatalf("= %d %s, want 503 VIDEO_STORE_UNAVAILABLE", rr.Code, rr.Body.String())
	}
}

func TestServeVideoSubmit_AuthReject(t *testing.T) {
	deps, _, _ := videoDeps(t, "http://unused.invalid")
	deps.VKAuth = stubAuthErrSTT{}
	h := NewHandler(deps)
	body, ct := buildVideoMultipart(t, "a cat", "sora-2", "")
	rr := doVideo(h.ServeVideoSubmit(videoIngress()), ct, body)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("= %d, want 401", rr.Code)
	}
}

func TestServeVideoSubmit_JobsLimit429(t *testing.T) {
	deps, js, _ := videoDeps(t, "http://unused.invalid")
	js.nonTerminal = videoMaxNonTerminalDefault // at the cap
	h := NewHandler(deps)
	body, ct := buildVideoMultipart(t, "a cat", "sora-2", "8")
	rr := doVideo(h.ServeVideoSubmit(videoIngress()), ct, body)
	if rr.Code != http.StatusTooManyRequests || !strings.Contains(rr.Body.String(), "VIDEO_JOBS_LIMIT") {
		t.Fatalf("= %d %s, want 429 VIDEO_JOBS_LIMIT", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Error("429 must carry Retry-After")
	}
	if len(js.inserted) != 0 {
		t.Error("no job may be inserted when the render bound rejects")
	}
}

func TestServeVideoSubmit_CountError503(t *testing.T) {
	deps, js, _ := videoDeps(t, "http://unused.invalid")
	js.countErr = errors.New("db down")
	h := NewHandler(deps)
	body, ct := buildVideoMultipart(t, "a cat", "sora-2", "8")
	rr := doVideo(h.ServeVideoSubmit(videoIngress()), ct, body)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("= %d, want 503 when the render bound cannot be checked", rr.Code)
	}
}

func TestServeVideoSubmit_BadContentType(t *testing.T) {
	deps, _, _ := videoDeps(t, "http://unused.invalid")
	h := NewHandler(deps)
	rr := doVideo(h.ServeVideoSubmit(videoIngress()), "application/json", []byte(`{}`))
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "VIDEO_BAD_MULTIPART") {
		t.Fatalf("= %d %s, want 400 VIDEO_BAD_MULTIPART", rr.Code, rr.Body.String())
	}
}

func TestServeVideoSubmit_ParseError400(t *testing.T) {
	deps, _, _ := videoDeps(t, "http://unused.invalid")
	h := NewHandler(deps)
	body, ct := buildVideoMultipart(t, "", "sora-2", "") // missing prompt
	rr := doVideo(h.ServeVideoSubmit(videoIngress()), ct, body)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "prompt") {
		t.Fatalf("= %d %s, want 400 naming the missing prompt", rr.Code, rr.Body.String())
	}
}

func TestServeVideoSubmit_NoRouteTargets404(t *testing.T) {
	deps, _, _ := videoDeps(t, "http://unused.invalid")
	deps.Router = &stubRouterCacheTest{targets: nil}
	h := NewHandler(deps)
	body, ct := buildVideoMultipart(t, "a cat", "unknown-model", "")
	rr := doVideo(h.ServeVideoSubmit(videoIngress()), ct, body)
	if rr.Code != http.StatusNotFound || !strings.Contains(rr.Body.String(), "NO_COMPATIBLE_PROVIDER") {
		t.Fatalf("= %d %s, want 404 NO_COMPATIBLE_PROVIDER", rr.Code, rr.Body.String())
	}
}

func TestServeVideoSubmit_ResolverNil502(t *testing.T) {
	deps, _, _ := videoDeps(t, "http://unused.invalid")
	deps.Resolver = nil
	h := NewHandler(deps)
	body, ct := buildVideoMultipart(t, "a cat", "sora-2", "")
	rr := doVideo(h.ServeVideoSubmit(videoIngress()), ct, body)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("= %d, want 502", rr.Code)
	}
}

func TestServeVideoSubmit_ResolveError502(t *testing.T) {
	deps, _, _ := videoDeps(t, "http://unused.invalid")
	deps.Resolver = sttStubResolver{err: errors.New("credential gone")}
	h := NewHandler(deps)
	body, ct := buildVideoMultipart(t, "a cat", "sora-2", "")
	rr := doVideo(h.ServeVideoSubmit(videoIngress()), ct, body)
	if rr.Code != http.StatusBadGateway || !strings.Contains(rr.Body.String(), "PROVIDER_RESOLVE_FAILED") {
		t.Fatalf("= %d %s, want 502 PROVIDER_RESOLVE_FAILED", rr.Code, rr.Body.String())
	}
}

func TestServeVideoSubmit_QuotaReject429(t *testing.T) {
	deps, js, _ := videoDeps(t, "http://unused.invalid")
	deps.QuotaEngine = newQuotaEngineWithPolicy(t, "virtual_key", "reject", 100, 200) // over the cap
	h := NewHandler(deps)
	body, ct := buildVideoMultipart(t, "a cat", "sora-2", "8")
	rr := doVideo(h.ServeVideoSubmit(videoIngress()), ct, body)
	if rr.Code != http.StatusTooManyRequests || !strings.Contains(rr.Body.String(), "QUOTA_EXCEEDED") {
		t.Fatalf("= %d %s, want 429 QUOTA_EXCEEDED", rr.Code, rr.Body.String())
	}
	if len(js.inserted) != 0 {
		t.Error("no job may be inserted when quota rejects")
	}
}

// The happy path: forward → provider 2xx → job row inserted with the full
// correlation record → estimate stamped on the submit row → body relayed.
func TestServeVideoSubmit_Success(t *testing.T) {
	srv := videoJobUpstream(t, `{"id":"video_123","object":"video","status":"queued","expires_at":1789000000}`)
	deps, js, prod := videoDeps(t, srv.URL)
	h := NewHandler(deps)

	body, ct := buildVideoMultipart(t, "a calico cat sailing a paper boat", "sora-2", "8")
	rr := doVideo(h.ServeVideoSubmit(videoIngress()), ct, body)
	if rr.Code != http.StatusOK {
		t.Fatalf("= %d %s, want 200", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"video_123"`) {
		t.Errorf("job body not relayed: %s", rr.Body.String())
	}

	// Correlation row: the full authz + pin + render-bound record.
	if len(js.inserted) != 1 {
		t.Fatalf("inserted %d rows, want 1", len(js.inserted))
	}
	job := js.inserted[0]
	if job.ProviderID != "p-openai" || job.ID != "video_123" {
		t.Errorf("identity = (%q,%q), want (p-openai, video_123)", job.ProviderID, job.ID)
	}
	if job.VirtualKeyID != "vk-1" || job.OrganizationID != "org-1" {
		t.Errorf("authz binding = (%q,%q), want (vk-1, org-1)", job.VirtualKeyID, job.OrganizationID)
	}
	if job.CredentialID != "cred-1" {
		t.Errorf("credential pin = %q, want cred-1", job.CredentialID)
	}
	if job.ModelID != "sora-2" || job.RequestedUnits != 8 {
		t.Errorf("model/units = (%q,%v), want (sora-2, 8)", job.ModelID, job.RequestedUnits)
	}
	if job.Status != asyncjob.StatusQueued {
		t.Errorf("status = %q, want queued", job.Status)
	}
	if job.ExpiresAt == nil || job.ExpiresAt.Unix() != 1789000000 {
		t.Errorf("expires_at = %v, want unix 1789000000", job.ExpiresAt)
	}

	// The submit row is the single cost-bearing row: 8 s × $0.10/s = $0.80.
	msg := lastAudit(t, deps, prod)
	if !approxEqual(msg.EstimatedCostUsd, 0.8) {
		t.Errorf("EstimatedCostUsd = %v, want 0.8 (estimate-as-floor)", msg.EstimatedCostUsd)
	}
	if msg.EndpointType != string(typology.EndpointKindVideoGeneration) {
		t.Errorf("endpoint_type = %q, want video_generation", msg.EndpointType)
	}
}

// Default seconds: an omitted `seconds` estimates 4 s (the provider default).
func TestServeVideoSubmit_DefaultSecondsEstimate(t *testing.T) {
	srv := videoJobUpstream(t, `{"id":"video_d","status":"queued"}`)
	deps, js, prod := videoDeps(t, srv.URL)
	h := NewHandler(deps)
	body, ct := buildVideoMultipart(t, "a cat", "sora-2", "")
	rr := doVideo(h.ServeVideoSubmit(videoIngress()), ct, body)
	if rr.Code != http.StatusOK {
		t.Fatalf("= %d, want 200", rr.Code)
	}
	if len(js.inserted) != 1 || js.inserted[0].RequestedUnits != videoDefaultSeconds {
		t.Fatalf("requested units = %+v, want default %d", js.inserted, videoDefaultSeconds)
	}
	msg := lastAudit(t, deps, prod)
	if !approxEqual(msg.EstimatedCostUsd, 0.4) { // 4 s × $0.10
		t.Errorf("EstimatedCostUsd = %v, want 0.4", msg.EstimatedCostUsd)
	}
}

// A provider 2xx with no job id cannot be correlated: 502, nothing inserted,
// and NO estimate stamped (there is no job to account).
func TestServeVideoSubmit_NoJobID502(t *testing.T) {
	srv := videoJobUpstream(t, `{"object":"video","status":"queued"}`)
	deps, js, prod := videoDeps(t, srv.URL)
	h := NewHandler(deps)
	body, ct := buildVideoMultipart(t, "a cat", "sora-2", "8")
	rr := doVideo(h.ServeVideoSubmit(videoIngress()), ct, body)
	if rr.Code != http.StatusBadGateway || !strings.Contains(rr.Body.String(), "VIDEO_CORRELATION_FAILED") {
		t.Fatalf("= %d %s, want 502 VIDEO_CORRELATION_FAILED", rr.Code, rr.Body.String())
	}
	if len(js.inserted) != 0 {
		t.Error("nothing may be inserted without a job id")
	}
	msg := lastAudit(t, deps, prod)
	if msg.EstimatedCostUsd != 0 {
		t.Errorf("uncorrelated submit stamped cost %v, want 0", msg.EstimatedCostUsd)
	}
}

func TestServeVideoSubmit_InsertFailure502(t *testing.T) {
	srv := videoJobUpstream(t, `{"id":"video_x","status":"queued"}`)
	deps, js, _ := videoDeps(t, srv.URL)
	js.insertErr = asyncjob.ErrConflict
	h := NewHandler(deps)
	body, ct := buildVideoMultipart(t, "a cat", "sora-2", "8")
	rr := doVideo(h.ServeVideoSubmit(videoIngress()), ct, body)
	if rr.Code != http.StatusBadGateway || !strings.Contains(rr.Body.String(), "VIDEO_CORRELATION_FAILED") {
		t.Fatalf("= %d %s, want 502 VIDEO_CORRELATION_FAILED", rr.Code, rr.Body.String())
	}
}

// Provider 4xx: relayed verbatim, no job row, no estimate.
func TestServeVideoSubmit_ProviderRejectRelayed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"seconds not enabled for this model"}}`))
	}))
	t.Cleanup(srv.Close)
	deps, js, prod := videoDeps(t, srv.URL)
	h := NewHandler(deps)
	body, ct := buildVideoMultipart(t, "a cat", "sora-2", "8")
	rr := doVideo(h.ServeVideoSubmit(videoIngress()), ct, body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("= %d, want the provider's 400 relayed", rr.Code)
	}
	if len(js.inserted) != 0 {
		t.Error("provider-rejected submit must not insert a job")
	}
	msg := lastAudit(t, deps, prod)
	if msg.EstimatedCostUsd != 0 {
		t.Errorf("provider-rejected submit stamped cost %v, want 0", msg.EstimatedCostUsd)
	}
}

// The gencaps slot must be released on error exits (a leak would wedge the
// per-VK cap at 2 concurrent).
func TestServeVideoSubmit_GenCapReleasedOnError(t *testing.T) {
	deps, _, _ := videoDeps(t, "http://unused.invalid")
	deps.Router = &stubRouterCacheTest{targets: nil} // force the 404 arm
	handler := NewHandler(deps).ServeVideoSubmit(videoIngress())
	caps, _ := generativecaps.Lookup(typology.EndpointKindVideoGeneration)
	for i := range caps.MaxConcurrentPerVK + 2 {
		body, ct := buildVideoMultipart(t, "a cat", "sora-2", "")
		rr := doVideo(handler, ct, body)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("iteration %d: = %d, want 404 (slot leak would 429 past the cap)", i, rr.Code)
		}
	}
}

// --- prompt-scan arms ---------------------------------------------------------

// A PII-laden prompt under a redact hook fails CLOSED: the multipart wire
// cannot reverse-encode a redacted prompt, so forwarding would leak the
// unredacted content upstream.
func TestServeVideoSubmit_RedactFailsClosed(t *testing.T) {
	deps, js, _ := videoDeps(t, "http://unused.invalid")
	deps.HookConfigCache = newPiiRedactHookCache(t)
	h := NewHandler(deps)
	body, ct := buildVideoMultipart(t, "email me at cat@example.com", "sora-2", "")
	rr := doVideo(h.ServeVideoSubmit(videoIngress()), ct, body)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("= %d %s, want 403 (redact fails closed on the multipart wire)", rr.Code, rr.Body.String())
	}
	if len(js.inserted) != 0 {
		t.Error("a blocked submit must not insert a job")
	}
}

// A benign prompt under a content-scanning hook passes with coverage
// prompt-only (the honesty stamp: a content hook actually scanned cleanly).
func TestServeVideoSubmit_BenignPromptScannedCoverage(t *testing.T) {
	srv := videoJobUpstream(t, `{"id":"video_ok","status":"queued"}`)
	deps, _, prod := videoDeps(t, srv.URL)
	deps.HookConfigCache = newPiiRedactHookCache(t)
	h := NewHandler(deps)
	body, ct := buildVideoMultipart(t, "a calico cat on a boat", "sora-2", "")
	rr := doVideo(h.ServeVideoSubmit(videoIngress()), ct, body)
	if rr.Code != http.StatusOK {
		t.Fatalf("= %d %s, want 200", rr.Code, rr.Body.String())
	}
	msg := lastAudit(t, deps, prod)
	if msg.ComplianceCoverage != "prompt-only" {
		t.Errorf("coverage = %q, want prompt-only (a content hook scanned cleanly)", msg.ComplianceCoverage)
	}
}

// No hook cache wired: nothing scanned, coverage stays none, request flows.
func TestServeVideoSubmit_NoHooksCoverageNone(t *testing.T) {
	srv := videoJobUpstream(t, `{"id":"video_n","status":"queued"}`)
	deps, _, prod := videoDeps(t, srv.URL)
	h := NewHandler(deps)
	body, ct := buildVideoMultipart(t, "a cat", "sora-2", "")
	rr := doVideo(h.ServeVideoSubmit(videoIngress()), ct, body)
	if rr.Code != http.StatusOK {
		t.Fatalf("= %d, want 200", rr.Code)
	}
	msg := lastAudit(t, deps, prod)
	if msg.ComplianceCoverage != "none" {
		t.Errorf("coverage = %q, want none (no content hook bound)", msg.ComplianceCoverage)
	}
}

// videoIngress is the submit route's Ingress fixture.
func videoIngress() Ingress {
	return Ingress{WireShape: typology.WireShapeOpenAIVideos, BodyFormat: provcore.FormatOpenAI}
}

// lastAudit closes the writer and unmarshals the single captured traffic
// event (the STT test pattern).
func lastAudit(t *testing.T, deps *Deps, prod *captureProducer) mq.TrafficEventMessage {
	t.Helper()
	deps.AuditWriter.Close()
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
	return evt
}

func TestServeVideoSubmit_RateLimited(t *testing.T) {
	deps, _, _ := videoDeps(t, "http://127.0.0.1:0")
	rpm := 5
	deps.VKAuth = rpmVKAuth{rpm: &rpm}
	deps.RateLimiter = denyAllRateLimiter{}
	h := NewHandler(deps).ServeVideoSubmit(videoIngress())
	body, ct := buildVideoMultipart(t, "a cat", "sora-2", "")
	rr := doVideo(h, ct, body)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rr.Code)
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Error("Retry-After must be set on rate-limit rejection")
	}
}

func TestServeVideoSubmit_GenCapReject(t *testing.T) {
	deps, _, _ := videoDeps(t, "http://127.0.0.1:0")
	handler := NewHandler(deps)
	caps, _ := generativecaps.Lookup(typology.EndpointKindVideoGeneration)
	for i := range caps.MaxConcurrentPerVK {
		if !handler.genConcurrency.Acquire(typology.EndpointKindVideoGeneration, "vk-1", caps.MaxConcurrentPerVK) {
			t.Fatalf("pre-acquire %d failed", i)
		}
	}
	h := handler.ServeVideoSubmit(videoIngress())
	body, ct := buildVideoMultipart(t, "a cat", "sora-2", "")
	rr := doVideo(h, ct, body)
	if rr.Code != http.StatusTooManyRequests || !strings.Contains(rr.Body.String(), "GENERATIVE_CONCURRENCY_LIMIT") {
		t.Fatalf("= %d %s, want 429 GENERATIVE_CONCURRENCY_LIMIT", rr.Code, rr.Body.String())
	}
}

func TestServeVideoSubmit_UpstreamDown502(t *testing.T) {
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	badURL := closed.URL
	closed.Close() // nothing listens → Do() errors
	deps, js, _ := videoDeps(t, badURL)
	h := NewHandler(deps).ServeVideoSubmit(videoIngress())
	body, ct := buildVideoMultipart(t, "a cat", "sora-2", "")
	rr := doVideo(h, ct, body)
	if rr.Code != http.StatusBadGateway || !strings.Contains(rr.Body.String(), "UPSTREAM_ERROR") {
		t.Fatalf("= %d %s, want 502 UPSTREAM_ERROR", rr.Code, rr.Body.String())
	}
	if len(js.inserted) != 0 {
		t.Error("a failed forward must not insert a job")
	}
}

func TestServeVideoSubmit_UnknownProviderFormat502(t *testing.T) {
	deps, _, _ := videoDeps(t, "http://127.0.0.1:0")
	deps.Resolver = sttUnknownFormatResolver{}
	h := NewHandler(deps).ServeVideoSubmit(videoIngress())
	body, ct := buildVideoMultipart(t, "a cat", "sora-2", "")
	rr := doVideo(h, ct, body)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (unknown provider format)", rr.Code)
	}
}

// An upload past the 16 MiB caps ceiling trips the MaxBytesReader mid-parse
// and maps to 413 — never an unbounded read.
func TestServeVideoSubmit_UploadTooLarge413(t *testing.T) {
	deps, _, _ := videoDeps(t, "http://127.0.0.1:0")
	h := NewHandler(deps).ServeVideoSubmit(videoIngress())
	buf := &bytes.Buffer{}
	w := multipart.NewWriter(buf)
	if err := w.WriteField("prompt", "a cat"); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteField("model", "sora-2"); err != nil {
		t.Fatal(err)
	}
	fw, err := w.CreateFormFile("input_reference", "big.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(bytes.Repeat([]byte("A"), 17<<20)); err != nil { // > 16 MiB
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	rr := doVideo(h, w.FormDataContentType(), buf.Bytes())
	if rr.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rr.Body.String(), "VIDEO_UPLOAD_TOO_LARGE") {
		t.Fatalf("= %d %s, want 413 VIDEO_UPLOAD_TOO_LARGE", rr.Code, rr.Body.String())
	}
}

// A hard-block hook verdict rejects the submit before any forward (the block
// arm, distinct from the redact-fails-closed arm).
func TestServeVideoSubmit_HookBlock403(t *testing.T) {
	deps, js, _ := videoDeps(t, "http://127.0.0.1:0")
	deps.HookConfigCache = newPiiBlockHookCacheVideo(t)
	h := NewHandler(deps).ServeVideoSubmit(videoIngress())
	body, ct := buildVideoMultipart(t, "email me at cat@example.com", "sora-2", "")
	rr := doVideo(h, ct, body)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("= %d %s, want 403 (hook block)", rr.Code, rr.Body.String())
	}
	if len(js.inserted) != 0 {
		t.Error("a blocked submit must not insert a job")
	}
}

// newPiiBlockHookCacheVideo seeds a PII detector whose match verdict is a
// hard BLOCK (inflightAction=block), driving the pipeline's Block decision.
func newPiiBlockHookCacheVideo(t *testing.T) *pipeline.HookConfigCache {
	t.Helper()
	loader := func(_ context.Context) ([]hookcore.HookConfig, error) {
		return []hookcore.HookConfig{{
			ID:                "pii-block-1",
			ImplementationID:  "pii-detector",
			Name:              "pii-block",
			Priority:          10,
			Enabled:           true,
			Stage:             "request",
			FailBehavior:      "fail-closed",
			TimeoutMs:         1000,
			ApplicableIngress: []string{"ALL"},
			Config: map[string]any{
				"onMatch": map[string]any{
					"inflightAction": "block-hard",
					"storageAction":  "redact",
				},
				"patternDefinitions": []any{
					map[string]any{
						"id":          "email",
						"regex":       `[a-z0-9._%+-]+@[a-z0-9.-]+\.[a-z]{2,}`,
						"flags":       "i",
						"replacement": "[REDACTED_EMAIL]",
					},
				},
			},
		}}, nil
	}
	cache := pipeline.NewHookConfigCache(loader, builtins.Registry, 0, slog.Default())
	if err := cache.Start(context.Background()); err != nil {
		t.Fatalf("cache.Start: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	return cache
}

// --- review-gate additions -----------------------------------------------------

// videoUnpricedModels serves a model with NO price row (nil InputPricePM) —
// the "brand-new catalog entry, price not yet set" rollout state.
type videoUnpricedModels struct{ sttStubModels }

func (videoUnpricedModels) GetModel(_ context.Context, id string) (*store.Model, error) {
	return &store.Model{ID: id}, nil
}

// An unpriced video model under an ACTIVE cost quota fails closed: a $0
// estimate would bypass every cap AND record $0 on the job's single
// cost-bearing row for real seconds-of-GPU spend (chat-path parity).
func TestServeVideoSubmit_UnpricedModelUnderCostQuota503(t *testing.T) {
	deps, js, _ := videoDeps(t, "http://127.0.0.1:0")
	deps.Models = videoUnpricedModels{}
	deps.QuotaEngine = newQuotaEngineWithPolicy(t, "virtual_key", "reject", 10_000, 0) // active limit, under it
	h := NewHandler(deps).ServeVideoSubmit(videoIngress())
	body, ct := buildVideoMultipart(t, "a cat", "sora-2", "8")
	rr := doVideo(h, ct, body)
	if rr.Code != http.StatusServiceUnavailable || !strings.Contains(rr.Body.String(), "QUOTA_MODEL_UNPRICED") {
		t.Fatalf("= %d %s, want 503 QUOTA_MODEL_UNPRICED", rr.Code, rr.Body.String())
	}
	if len(js.inserted) != 0 {
		t.Error("no job may be created for an unpriced model under an active cost quota")
	}
}

// Without an enforced cost quota an unpriced model still flows (fail-open,
// like chat) but the audit row carries the unpriced marker so cost dashboards
// don't read "$0 = no spend".
func TestServeVideoSubmit_UnpricedModelNoQuotaFlowsWithMarker(t *testing.T) {
	srv := videoJobUpstream(t, `{"id":"video_u","status":"queued"}`)
	deps, _, prod := videoDeps(t, srv.URL)
	deps.Models = videoUnpricedModels{}
	h := NewHandler(deps).ServeVideoSubmit(videoIngress())
	body, ct := buildVideoMultipart(t, "a cat", "sora-2", "8")
	rr := doVideo(h, ct, body)
	if rr.Code != http.StatusOK {
		t.Fatalf("= %d, want 200 (no enforced quota → fail open)", rr.Code)
	}
	msg := lastAudit(t, deps, prod)
	if msg.EstimatedCostUsd != 0 {
		t.Errorf("unpriced estimate = %v, want 0", msg.EstimatedCostUsd)
	}
	// The unpriced marker rides the details wire; assert on the raw envelope.
	prod.mu.Lock()
	raw := string(prod.messages[0])
	prod.mu.Unlock()
	if !strings.Contains(raw, `"unpriced":true`) {
		t.Errorf("audit envelope missing the unpriced marker")
	}
}

// The upstream wire contract (STT-parity): the forward carries the REWRITTEN
// provider model id under a single fresh-boundary Content-Type, provider auth
// replaces the client's, and client credentials never leak.
func TestServeVideoSubmit_UpstreamWireIntegrity(t *testing.T) {
	var gotModel, gotAuth, gotClientSecret, gotCT string
	var gotCTCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(1 << 20)
		gotModel = r.FormValue("model")
		gotAuth = r.Header.Get("Authorization")
		gotClientSecret = r.Header.Get("X-Client-Secret")
		gotCT = r.Header.Get("Content-Type")
		gotCTCount = len(r.Header["Content-Type"])
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"video_w","status":"queued"}`))
	}))
	t.Cleanup(srv.Close)
	deps, _, _ := videoDeps(t, srv.URL)
	h := NewHandler(deps).ServeVideoSubmit(videoIngress())

	body, ct := buildVideoMultipart(t, "a cat", "sora-2", "8")
	req := httptest.NewRequest(http.MethodPost, "/v1/videos", bytes.NewReader(body))
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Authorization", "Bearer client-key") // must NOT leak upstream
	req.Header.Set("X-Client-Secret", "shhh")            // must NOT leak upstream
	rr := httptest.NewRecorder()
	h(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("= %d %s, want 200", rr.Code, rr.Body.String())
	}
	if gotModel != "sora-2-provider" {
		t.Errorf("upstream model = %q, want rewritten sora-2-provider (alias pricing rides this)", gotModel)
	}
	if gotAuth != "Bearer sk-upstream" {
		t.Errorf("upstream Authorization = %q, want the provider key (client key must not leak)", gotAuth)
	}
	if gotClientSecret != "" {
		t.Errorf("client X-Client-Secret leaked upstream: %q", gotClientSecret)
	}
	if gotCTCount != 1 || !strings.HasPrefix(gotCT, "multipart/form-data") || gotCT == ct {
		t.Errorf("upstream Content-Type = %q (count %d, client %q); want ONE fresh re-emit boundary", gotCT, gotCTCount, ct)
	}
}

// newObserveKeywordHookCacheVideo seeds a keyword filter in OBSERVE posture:
// a match stamps KEYWORD_BLOCKED but approves — the exact case the D5
// generative escalation exists for.
func newObserveKeywordHookCacheVideo(t *testing.T) *pipeline.HookConfigCache {
	t.Helper()
	loader := func(_ context.Context) ([]hookcore.HookConfig, error) {
		return []hookcore.HookConfig{{
			ID:                "kw-observe-1",
			ImplementationID:  "keyword-filter",
			Name:              "kw-observe",
			Priority:          10,
			Enabled:           true,
			Stage:             "request",
			FailBehavior:      "fail-open",
			TimeoutMs:         1000,
			ApplicableIngress: []string{"ALL"},
			Config: map[string]any{
				"patterns": []any{map[string]any{"pattern": "forbiddenword", "category": "banlist"}},
				"onMatch":  map[string]any{"action": "approve"},
			},
		}}, nil
	}
	cache := pipeline.NewHookConfigCache(loader, builtins.Registry, 0, slog.Default())
	if err := cache.Start(context.Background()); err != nil {
		t.Fatalf("cache.Start: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	return cache
}

// The D5 escalation fired THROUGH the video path (the reachability the
// generative_block.go comment rests on): an observe-only keyword match on a
// video prompt escalates to a hard 403 GENERATIVE_PROMPT_BLOCKED.
func TestServeVideoSubmit_ObserveMatchEscalates403(t *testing.T) {
	deps, js, _ := videoDeps(t, "http://127.0.0.1:0")
	deps.HookConfigCache = newObserveKeywordHookCacheVideo(t)
	h := NewHandler(deps).ServeVideoSubmit(videoIngress())
	body, ct := buildVideoMultipart(t, "please render forbiddenword scene", "sora-2", "")
	rr := doVideo(h, ct, body)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("= %d %s, want 403 (observe match must escalate on video)", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("X-Nexus-Hook"); !strings.Contains(got, "generative-prompt-blocked") {
		t.Errorf("X-Nexus-Hook = %q, want the generative-prompt-blocked attribution", got)
	}
	if len(js.inserted) != 0 {
		t.Error("an escalated submit must not insert a job")
	}
}

// erroringVideoHook always fails at execute time.
type erroringVideoHook struct {
	hookcore.AnyEndpointAnyModality
}

func (erroringVideoHook) Execute(context.Context, *hookcore.HookInput) (*hookcore.HookResult, error) {
	return nil, errors.New("scanner exploded")
}

// A hook that ERRORS (fail-open) means the prompt was not cleanly scanned —
// coverage must stay none even though the request flows (the D-V7/F-sec-8
// honesty arm).
func TestServeVideoSubmit_ErroredHookCoverageNone(t *testing.T) {
	reg := builtins.Registry.Clone()
	reg.Register("video-erroring-hook", func(_ *hookcore.HookConfig) (hookcore.Hook, error) {
		return erroringVideoHook{}, nil
	})
	reg.Freeze()
	loader := func(_ context.Context) ([]hookcore.HookConfig, error) {
		return []hookcore.HookConfig{
			{
				ID: "pii-clean-1", ImplementationID: "pii-detector", Name: "pii-clean",
				Priority: 10, Enabled: true, Stage: "request", FailBehavior: "fail-closed", TimeoutMs: 1000,
				ApplicableIngress: []string{"ALL"},
				Config: map[string]any{
					"onMatch": map[string]any{"inflightAction": "redact", "storageAction": "redact"},
					"patternDefinitions": []any{
						map[string]any{"id": "email", "regex": `[a-z0-9._%+-]+@[a-z0-9.-]+\.[a-z]{2,}`, "flags": "i", "replacement": "[X]"},
					},
				},
			},
			{
				ID: "err-1", ImplementationID: "video-erroring-hook", Name: "erroring",
				Priority: 5, Enabled: true, Stage: "request", FailBehavior: "fail-open", TimeoutMs: 1000,
				ApplicableIngress: []string{"ALL"}, Config: map[string]any{},
			},
		}, nil
	}
	cache := pipeline.NewHookConfigCache(loader, reg, 0, slog.Default())
	if err := cache.Start(context.Background()); err != nil {
		t.Fatalf("cache.Start: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	srv := videoJobUpstream(t, `{"id":"video_e","status":"queued"}`)
	deps, _, prod := videoDeps(t, srv.URL)
	deps.HookConfigCache = cache
	h := NewHandler(deps).ServeVideoSubmit(videoIngress())
	body, ct := buildVideoMultipart(t, "a benign cat", "sora-2", "")
	rr := doVideo(h, ct, body)
	if rr.Code != http.StatusOK {
		t.Fatalf("= %d %s, want 200 (fail-open hook error flows)", rr.Code, rr.Body.String())
	}
	msg := lastAudit(t, deps, prod)
	if msg.ComplianceCoverage != "none" {
		t.Errorf("coverage = %q, want none (a hook errored — the prompt was NOT cleanly scanned)", msg.ComplianceCoverage)
	}
}

// A hook cache with zero configs builds a nil pipeline: nothing scanned,
// coverage none, request flows (a legal deployment posture, not fail-open).
func TestServeVideoSubmit_EmptyHookCacheFlows(t *testing.T) {
	srv := videoJobUpstream(t, `{"id":"video_0","status":"queued"}`)
	deps, _, prod := videoDeps(t, srv.URL)
	deps.HookConfigCache = hookCacheWith(t) // zero configs → nil pipeline
	h := NewHandler(deps).ServeVideoSubmit(videoIngress())
	body, ct := buildVideoMultipart(t, "a cat", "sora-2", "")
	rr := doVideo(h, ct, body)
	if rr.Code != http.StatusOK {
		t.Fatalf("= %d, want 200", rr.Code)
	}
	msg := lastAudit(t, deps, prod)
	if msg.ComplianceCoverage != "none" {
		t.Errorf("coverage = %q, want none", msg.ComplianceCoverage)
	}
}

// The input_reference fingerprint lands on the audit row (reference only) on
// the happy path.
func TestServeVideoSubmit_InputReferenceFingerprintStamped(t *testing.T) {
	srv := videoJobUpstream(t, `{"id":"video_r","status":"queued"}`)
	deps, _, prod := videoDeps(t, srv.URL)
	h := NewHandler(deps).ServeVideoSubmit(videoIngress())

	buf := &bytes.Buffer{}
	w := multipart.NewWriter(buf)
	_ = w.WriteField("prompt", "a cat")
	_ = w.WriteField("model", "sora-2")
	fw, err := w.CreateFormFile("input_reference", "ref.png")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write([]byte("\x89PNG\r\n\x1a\nref-bytes"))
	_ = w.Close()

	rr := doVideo(h, w.FormDataContentType(), buf.Bytes())
	if rr.Code != http.StatusOK {
		t.Fatalf("= %d %s, want 200", rr.Code, rr.Body.String())
	}
	msg := lastAudit(t, deps, prod)
	if !strings.Contains(msg.ArtifactRefs, "sha256") {
		t.Errorf("ArtifactRefs missing the input_reference fingerprint: %q", msg.ArtifactRefs)
	}
}

// A hook config whose implementation does not exist, declared fail-closed,
// makes BuildPipeline error under strictFailClosed — the submit must 500
// (an unbuildable pipeline must never let an unscanned generative prompt
// through), never forward.
func TestServeVideoSubmit_PipelineBuildError500(t *testing.T) {
	loader := func(_ context.Context) ([]hookcore.HookConfig, error) {
		return []hookcore.HookConfig{{
			ID: "ghost-1", ImplementationID: "does-not-exist", Name: "ghost",
			Priority: 10, Enabled: true, Stage: "request", FailBehavior: "fail-closed", TimeoutMs: 1000,
			ApplicableIngress: []string{"ALL"}, Config: map[string]any{},
		}}, nil
	}
	cache := pipeline.NewHookConfigCache(loader, builtins.Registry, 0, slog.Default())
	if err := cache.Start(context.Background()); err != nil {
		t.Fatalf("cache.Start: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	deps, js, _ := videoDeps(t, "http://127.0.0.1:0")
	deps.HookConfigCache = cache
	h := NewHandler(deps).ServeVideoSubmit(videoIngress())
	body, ct := buildVideoMultipart(t, "a cat", "sora-2", "")
	rr := doVideo(h, ct, body)
	if rr.Code != http.StatusInternalServerError || !strings.Contains(rr.Body.String(), "VIDEO_PIPELINE_ERROR") {
		t.Fatalf("= %d %s, want 500 VIDEO_PIPELINE_ERROR (strict fail-closed)", rr.Code, rr.Body.String())
	}
	if len(js.inserted) != 0 {
		t.Error("an unbuildable pipeline must not let a submit through")
	}
}

// erroringModels fails every lookup — the estimate must fail OPEN (priced)
// so a transient catalog error doesn't 503 every video under a quota.
type erroringModels struct{ sttStubModels }

func (erroringModels) GetModel(context.Context, string) (*store.Model, error) {
	return nil, errors.New("catalog down")
}

func TestVideoEstimateUsd_LookupErrorFailsOpen(t *testing.T) {
	h := &Handler{deps: &Deps{Models: erroringModels{}}}
	req := httptest.NewRequest(http.MethodPost, "/v1/videos", nil)
	usd, priced := h.videoEstimateUsd(req, "sora-2", 8)
	if usd != 0 || !priced {
		t.Fatalf("= (%v, %v), want (0, priced/fail-open) on a transient lookup error", usd, priced)
	}
}
