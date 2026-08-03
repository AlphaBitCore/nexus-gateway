package proxy

// video_follow_handler_test.go — business-behavior tests for ServeVideoPoll /
// ServeVideoDelete (e88-s6 T6): 404 non-disclosure (upstream never probed),
// pinned-credential resolve, verbatim relay with the 1 MiB ceiling, hostile
// job-id containment, last-observed cache discipline, and the
// first-terminal-observer live cost reconcile (estimate-as-floor — a gamed
// provider usage payload must not move the debited amount).

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store/asyncjob"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/quota"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	provtarget "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/target"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// videoAuthErr always fails VK authentication.
type videoAuthErr struct{ err error }

func (s videoAuthErr) Authenticate(context.Context, *http.Request) (*vkauth.VKMeta, error) {
	return nil, s.err
}

// ownedVideoJob is the canonical in-flight job row the follow-up tests serve.
func ownedVideoJob() *asyncjob.Job {
	return &asyncjob.Job{
		ProviderID:     "p-openai",
		ID:             "video_abc",
		EndpointType:   "video_generation",
		VirtualKeyID:   "vk-1",
		OrganizationID: "org-1",
		CredentialID:   "cred-pin-1",
		ModelID:        "sora-2",
		RequestedUnits: 8,
		Status:         asyncjob.StatusInProgress,
	}
}

// videoPinResolver records the ResolveHints of every call — the seam that
// proves the follow-up pins the job row's submit-time credential.
type videoPinResolver struct {
	mu      sync.Mutex
	baseURL string
	err     error
	hints   []provtarget.ResolveHints
}

func (s *videoPinResolver) Resolve(_ context.Context, providerID, modelID string, h provtarget.ResolveHints) (provcore.CallTarget, error) {
	s.mu.Lock()
	s.hints = append(s.hints, h)
	s.mu.Unlock()
	if s.err != nil {
		return provcore.CallTarget{}, s.err
	}
	return provcore.CallTarget{
		ProviderID:      providerID,
		ProviderName:    "openai",
		Format:          provcore.FormatOpenAI,
		BaseURL:         s.baseURL,
		APIKey:          "sk-upstream",
		CredentialID:    "cred-pin-1",
		CredentialName:  "pinned-cred",
		ProviderModelID: modelID,
	}, nil
}

// followUpstream records what the provider actually received.
type followUpstream struct {
	srv    *httptest.Server
	mu     sync.Mutex
	hits   int
	method string
	uri    string // raw RequestURI — pins the escaped id form
}

func newFollowUpstream(t *testing.T, status int, body string) *followUpstream {
	t.Helper()
	f := &followUpstream{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.hits++
		f.method = r.Method
		f.uri = r.RequestURI
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// videoFollowDeps wires the follow-up harness: submit harness + an owned job
// row + the hint-recording resolver.
func videoFollowDeps(t *testing.T, upstreamURL string) (*Deps, *fakeJobStore, *captureProducer, *videoPinResolver) {
	t.Helper()
	deps, js, prod := videoDeps(t, upstreamURL)
	res := &videoPinResolver{baseURL: upstreamURL}
	deps.Resolver = res
	js.owned = ownedVideoJob()
	return deps, js, prod, res
}

func doVideoFollow(h http.HandlerFunc, method, id string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/v1/videos/"+id, nil)
	req.SetPathValue("id", id)
	rr := httptest.NewRecorder()
	h(rr, req)
	return rr
}

// videoQuotaHarness returns an engine whose VK level enforces a cost cap, plus
// the usage cache the test reads the debit back from. limitCents is the cap.
func videoQuotaHarness(t *testing.T, limitCents int64) (*quota.Engine, *quota.UsageCache) {
	t.Helper()
	pc := quota.NewPolicyCache(nil, slog.Default())
	pc.SetPoliciesForTest(map[string][]quota.CachedPolicy{
		"virtual_key": {{
			ID: "p-1", Scope: "virtual_key", PeriodType: "monthly",
			CostLimitCents: limitCents, EnforcementMode: "reject", Priority: 100,
		}},
	})
	uc := quota.NewUsageCache(nil, slog.Default())
	return quota.NewEngine(pc, uc, slog.Default(), nil), uc
}

func vkUsageCents(t *testing.T, uc *quota.UsageCache) int64 {
	t.Helper()
	cents, err := uc.GetUsage(context.Background(), "virtual_key", "vk-1", quota.CurrentPeriodKey("monthly"))
	if err != nil {
		t.Fatalf("read usage: %v", err)
	}
	return cents
}

// --- poll: relay + cache discipline -----------------------------------------

// A non-terminal poll relays the provider's job object verbatim, updates the
// last-observed cache, never touches the reconcile claim, and stamps a $0
// traffic event (the submit row is the job's single cost-bearing row).
func TestServeVideoPoll_RelaysInProgressAndCaches(t *testing.T) {
	body := `{"id":"video_abc","object":"video","status":"in_progress","progress":40}`
	up := newFollowUpstream(t, http.StatusOK, body)
	deps, js, prod, _ := videoFollowDeps(t, up.srv.URL)
	h := NewHandler(deps).ServeVideoPoll(videoIngress())

	rr := doVideoFollow(h, http.MethodGet, "video_abc")
	if rr.Code != http.StatusOK || rr.Body.String() != body {
		t.Fatalf("relay = %d %q, want 200 verbatim body", rr.Code, rr.Body.String())
	}
	if up.method != http.MethodGet || up.uri != "/v1/videos/video_abc" {
		t.Errorf("upstream saw %s %s, want GET /v1/videos/video_abc", up.method, up.uri)
	}
	if len(js.ownedArgs) != 1 || js.ownedArgs[0] != [3]string{"video_abc", "vk-1", "video_generation"} {
		t.Errorf("owned lookup args = %v, want (video_abc, vk-1, video_generation)", js.ownedArgs)
	}
	if len(js.observed) != 1 || js.observed[0].Status != asyncjob.StatusInProgress {
		t.Errorf("observed = %+v, want one in_progress observation", js.observed)
	}
	if js.claims != 0 {
		t.Errorf("reconcile claims = %d, want 0 for a non-terminal poll", js.claims)
	}
	msg := lastAudit(t, deps, prod)
	if msg.EstimatedCostUsd != 0 {
		t.Errorf("poll row cost = %v, want $0 (single cost-bearing row is the submit row)", msg.EstimatedCostUsd)
	}
	if msg.EndpointType != string(typology.EndpointKindVideoGeneration) {
		t.Errorf("endpoint_type = %q, want video_generation", msg.EndpointType)
	}
}

// An unknown / foreign job id is a 404 NON-DISCLOSURE and the upstream is
// NEVER probed with it.
func TestServeVideoPoll_NotFoundNeverForwards(t *testing.T) {
	up := newFollowUpstream(t, http.StatusOK, `{}`)
	deps, js, _, _ := videoFollowDeps(t, up.srv.URL)
	js.owned = nil // no row (same sentinel a foreign VK's row produces)
	h := NewHandler(deps).ServeVideoPoll(videoIngress())

	rr := doVideoFollow(h, http.MethodGet, "video_unknown")
	if rr.Code != http.StatusNotFound || !strings.Contains(rr.Body.String(), "VIDEO_JOB_NOT_FOUND") {
		t.Fatalf("resp = %d %q, want 404 VIDEO_JOB_NOT_FOUND", rr.Code, rr.Body.String())
	}
	if up.hits != 0 {
		t.Errorf("upstream hits = %d, want 0 — unknown ids must never probe the provider", up.hits)
	}
}

// A cross-provider id collision fails loud (never guesses a provider).
func TestServeVideoPoll_AmbiguousFailsLoud(t *testing.T) {
	up := newFollowUpstream(t, http.StatusOK, `{}`)
	deps, js, _, _ := videoFollowDeps(t, up.srv.URL)
	js.ownedErr = asyncjob.ErrAmbiguous
	h := NewHandler(deps).ServeVideoPoll(videoIngress())

	rr := doVideoFollow(h, http.MethodGet, "video_abc")
	if rr.Code != http.StatusBadGateway || !strings.Contains(rr.Body.String(), "VIDEO_JOB_AMBIGUOUS") {
		t.Fatalf("resp = %d %q, want 502 VIDEO_JOB_AMBIGUOUS", rr.Code, rr.Body.String())
	}
	if up.hits != 0 {
		t.Errorf("upstream hits = %d, want 0", up.hits)
	}
}

func TestServeVideoPoll_StoreErrorAndNoStore(t *testing.T) {
	up := newFollowUpstream(t, http.StatusOK, `{}`)
	deps, js, _, _ := videoFollowDeps(t, up.srv.URL)
	js.ownedErr = errors.New("pg down")
	h := NewHandler(deps).ServeVideoPoll(videoIngress())
	if rr := doVideoFollow(h, http.MethodGet, "video_abc"); rr.Code != http.StatusServiceUnavailable {
		t.Errorf("store error → %d, want 503", rr.Code)
	}

	deps2, _, _, _ := videoFollowDeps(t, up.srv.URL)
	deps2.AsyncJobs = nil
	h2 := NewHandler(deps2).ServeVideoPoll(videoIngress())
	if rr := doVideoFollow(h2, http.MethodGet, "video_abc"); rr.Code != http.StatusServiceUnavailable {
		t.Errorf("no store → %d, want 503", rr.Code)
	}
	if up.hits != 0 {
		t.Errorf("upstream hits = %d, want 0", up.hits)
	}
}

// A retention-expired row is a 410 Gone — distinguishable from the 404
// non-disclosure — and is never relayed upstream.
func TestServeVideoPoll_ExpiredRow410(t *testing.T) {
	up := newFollowUpstream(t, http.StatusOK, `{}`)
	deps, js, _, _ := videoFollowDeps(t, up.srv.URL)
	js.owned.Status = asyncjob.StatusExpired
	h := NewHandler(deps).ServeVideoPoll(videoIngress())

	rr := doVideoFollow(h, http.MethodGet, "video_abc")
	if rr.Code != http.StatusGone || !strings.Contains(rr.Body.String(), "VIDEO_JOB_EXPIRED") {
		t.Fatalf("resp = %d %q, want 410 VIDEO_JOB_EXPIRED", rr.Code, rr.Body.String())
	}
	if up.hits != 0 {
		t.Errorf("upstream hits = %d, want 0", up.hits)
	}
}

// The follow-up resolve pins the job row's submit-time credential; a
// no-longer-resolvable credential is a 502 with the job row untouched.
func TestServeVideoPoll_CredentialPinAndResolveFailure(t *testing.T) {
	up := newFollowUpstream(t, http.StatusOK, `{"id":"video_abc","status":"queued"}`)
	deps, _, _, res := videoFollowDeps(t, up.srv.URL)
	h := NewHandler(deps).ServeVideoPoll(videoIngress())
	doVideoFollow(h, http.MethodGet, "video_abc")
	if len(res.hints) != 1 || res.hints[0].CredentialID != "cred-pin-1" {
		t.Fatalf("resolve hints = %+v, want CredentialID cred-pin-1 (the job row's pin)", res.hints)
	}

	deps2, js2, _, res2 := videoFollowDeps(t, up.srv.URL)
	res2.err = errors.New("credential disabled")
	h2 := NewHandler(deps2).ServeVideoPoll(videoIngress())
	rr := doVideoFollow(h2, http.MethodGet, "video_abc")
	if rr.Code != http.StatusBadGateway || !strings.Contains(rr.Body.String(), "PROVIDER_RESOLVE_FAILED") {
		t.Fatalf("resp = %d %q, want 502 PROVIDER_RESOLVE_FAILED", rr.Code, rr.Body.String())
	}
	if len(js2.observed) != 0 || js2.claims != 0 {
		t.Errorf("store mutated on resolve failure (observed %d, claims %d) — job row must stay intact",
			len(js2.observed), js2.claims)
	}
}

// --- poll: first-terminal-observer reconcile ---------------------------------

// The FIRST poll observing `completed` debits the live counters with
// realized = requested seconds × the model's per-second price. The upstream
// body carries a gamed token-usage payload — it must NOT move the debit
// (estimate-as-floor; the token-win formula is for token-shaped endpoints).
// The cap is set BELOW the realized cost to also pin that the debit lands
// regardless of the decision's Allowed — the render already happened.
func TestServeVideoPoll_FirstCompletedReconcilesSecondsFloor(t *testing.T) {
	body := `{"id":"video_abc","status":"completed","completed_at":1789000100,"expires_at":1789003600,` +
		`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	up := newFollowUpstream(t, http.StatusOK, body)
	deps, js, _, _ := videoFollowDeps(t, up.srv.URL)
	js.claimResult = true
	engine, uc := videoQuotaHarness(t, 10) // cap 10¢ < realized 80¢
	deps.QuotaEngine = engine
	h := NewHandler(deps).ServeVideoPoll(videoIngress())

	rr := doVideoFollow(h, http.MethodGet, "video_abc")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the relay must not depend on bookkeeping)", rr.Code)
	}
	if js.claims != 1 {
		t.Fatalf("reconcile claims = %d, want exactly 1", js.claims)
	}
	if len(js.observed) != 1 {
		t.Fatalf("observed %d, want 1", len(js.observed))
	}
	obs := js.observed[0]
	if obs.Status != asyncjob.StatusCompleted ||
		obs.CompletedAt == nil || obs.CompletedAt.Unix() != 1789000100 ||
		obs.ExpiresAt == nil || obs.ExpiresAt.Unix() != 1789003600 {
		t.Errorf("observation = %+v, want completed with provider timestamps", obs)
	}
	// 8 s × $0.10/s = $0.80 → 80 cents. Token usage in the body is ignored.
	if got := vkUsageCents(t, uc); got != 80 {
		t.Errorf("live debit = %d cents, want 80 (seconds × price floor, never the gamed usage payload)", got)
	}
}

// A later poll of an already-reconciled job must not double-debit.
func TestServeVideoPoll_SecondObserverNoDoubleDebit(t *testing.T) {
	up := newFollowUpstream(t, http.StatusOK, `{"id":"video_abc","status":"completed"}`)
	deps, js, _, _ := videoFollowDeps(t, up.srv.URL)
	js.claimResult = false // claim already taken by an earlier observer
	engine, uc := videoQuotaHarness(t, 1_000_000)
	deps.QuotaEngine = engine
	h := NewHandler(deps).ServeVideoPoll(videoIngress())

	doVideoFollow(h, http.MethodGet, "video_abc")
	if js.claims != 1 {
		t.Errorf("claims = %d, want 1 (attempted once, not granted)", js.claims)
	}
	if got := vkUsageCents(t, uc); got != 0 {
		t.Errorf("live debit = %d cents, want 0 — the claim guards double-counting", got)
	}
}

// A failed render reconciles $0: the claim is taken (no later poll re-tries)
// but nothing is debited.
func TestServeVideoPoll_FailedDebitsNothing(t *testing.T) {
	up := newFollowUpstream(t, http.StatusOK, `{"id":"video_abc","status":"failed","error":{"code":"render_failed"}}`)
	deps, js, _, _ := videoFollowDeps(t, up.srv.URL)
	js.claimResult = true
	engine, uc := videoQuotaHarness(t, 1_000_000)
	deps.QuotaEngine = engine
	h := NewHandler(deps).ServeVideoPoll(videoIngress())

	doVideoFollow(h, http.MethodGet, "video_abc")
	if js.claims != 1 {
		t.Errorf("claims = %d, want 1", js.claims)
	}
	if len(js.observed) != 1 || js.observed[0].Status != asyncjob.StatusFailed {
		t.Errorf("observed = %+v, want failed", js.observed)
	}
	if got := vkUsageCents(t, uc); got != 0 {
		t.Errorf("live debit = %d cents, want 0 for a failed render", got)
	}
}

// An unmapped provider status must leave the lifecycle cache alone (a guess
// could flip a terminal row non-terminal) while the body still relays.
func TestServeVideoPoll_UnknownStatusLeavesCache(t *testing.T) {
	body := `{"id":"video_abc","status":"processing"}`
	up := newFollowUpstream(t, http.StatusOK, body)
	deps, js, _, _ := videoFollowDeps(t, up.srv.URL)
	h := NewHandler(deps).ServeVideoPoll(videoIngress())

	rr := doVideoFollow(h, http.MethodGet, "video_abc")
	if rr.Code != http.StatusOK || rr.Body.String() != body {
		t.Fatalf("relay = %d %q, want verbatim 200", rr.Code, rr.Body.String())
	}
	if len(js.observed) != 0 || js.claims != 0 {
		t.Errorf("cache mutated on unmapped status (observed %d, claims %d), want untouched",
			len(js.observed), js.claims)
	}
}

// An unpriced model at completion skips the live debit (WARN) — the submit
// row remains the cost of record. The claim IS consumed: a later poll cannot
// price it any better, so retrying is pure noise.
func TestServeVideoPoll_UnpricedModelSkipsLiveDebit(t *testing.T) {
	up := newFollowUpstream(t, http.StatusOK, `{"id":"video_abc","status":"completed"}`)
	deps, js, _, _ := videoFollowDeps(t, up.srv.URL)
	js.claimResult = true
	deps.Models = videoUnpricedModels{}
	engine, uc := videoQuotaHarness(t, 1_000_000)
	deps.QuotaEngine = engine
	h := NewHandler(deps).ServeVideoPoll(videoIngress())

	doVideoFollow(h, http.MethodGet, "video_abc")
	if got := vkUsageCents(t, uc); got != 0 {
		t.Errorf("live debit = %d cents, want 0 for an unpriced model", got)
	}
	if js.claims != 1 {
		t.Errorf("claims = %d, want 1 (genuinely unpriced consumes the claim)", js.claims)
	}
}

// videoErrModels fails every catalog lookup — the transient-failure seam.
type videoErrModels struct{ sttStubModels }

func (videoErrModels) GetModel(context.Context, string) (*store.Model, error) {
	return nil, errors.New("catalog transiently unavailable")
}

// A TRANSIENT price-lookup failure at completion must NOT consume the
// once-only reconcile claim — the price is resolved BEFORE the claim so a
// later poll re-observes the terminal state and retries the whole reconcile.
// (Three review lenses independently found the inverse ordering losing the
// live debit permanently.)
func TestServeVideoPoll_TransientPriceFailureLeavesClaimUntaken(t *testing.T) {
	up := newFollowUpstream(t, http.StatusOK, `{"id":"video_abc","status":"completed"}`)
	deps, js, _, _ := videoFollowDeps(t, up.srv.URL)
	js.claimResult = true
	deps.Models = videoErrModels{}
	engine, uc := videoQuotaHarness(t, 1_000_000)
	deps.QuotaEngine = engine
	h := NewHandler(deps).ServeVideoPoll(videoIngress())

	rr := doVideoFollow(h, http.MethodGet, "video_abc")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (bookkeeping never fails the relay)", rr.Code)
	}
	if js.claims != 0 {
		t.Errorf("claims = %d, want 0 — a transient lookup failure must leave the reconcile retriable", js.claims)
	}
	if got := vkUsageCents(t, uc); got != 0 {
		t.Errorf("live debit = %d cents, want 0 (deferred, not lost)", got)
	}
	// The observation itself still lands (the cache update is independent).
	if len(js.observed) != 1 || js.observed[0].Status != asyncjob.StatusCompleted {
		t.Errorf("observed = %+v, want the completed observation recorded", js.observed)
	}
}

// Every relayed video response pins nosniff: the Content-Type is
// provider-controlled, and a hostile provider must not turn the authenticated
// gateway origin into a content-sniffed HTML page on a browser-navigable GET.
func TestServeVideoPoll_RelaySetsNosniff(t *testing.T) {
	up := newFollowUpstream(t, http.StatusOK, `{"id":"video_abc","status":"queued"}`)
	deps, _, _, _ := videoFollowDeps(t, up.srv.URL)
	h := NewHandler(deps).ServeVideoPoll(videoIngress())

	rr := doVideoFollow(h, http.MethodGet, "video_abc")
	if got := rr.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
}

// Provider errors relay verbatim (an expired-at-provider job's 404 reaches
// the caller as-is) and never touch the cache.
func TestServeVideoPoll_ProviderErrorRelaysVerbatim(t *testing.T) {
	body := `{"error":{"message":"video not found","type":"invalid_request_error"}}`
	up := newFollowUpstream(t, http.StatusNotFound, body)
	deps, js, _, _ := videoFollowDeps(t, up.srv.URL)
	h := NewHandler(deps).ServeVideoPoll(videoIngress())

	rr := doVideoFollow(h, http.MethodGet, "video_abc")
	if rr.Code != http.StatusNotFound || rr.Body.String() != body {
		t.Fatalf("relay = %d %q, want provider 404 verbatim", rr.Code, rr.Body.String())
	}
	if len(js.observed) != 0 || js.claims != 0 {
		t.Errorf("cache mutated on provider error, want untouched")
	}
}

// The relay body is bounded at the 1 MiB job-object ceiling.
func TestServeVideoPoll_ResponseCeiling(t *testing.T) {
	huge := strings.Repeat("x", videoFollowMaxResponseBytes+4096)
	up := newFollowUpstream(t, http.StatusOK, `{"id":"video_abc","status":"in_progress","pad":"`+huge+`"}`)
	deps, _, _, _ := videoFollowDeps(t, up.srv.URL)
	h := NewHandler(deps).ServeVideoPoll(videoIngress())

	rr := doVideoFollow(h, http.MethodGet, "video_abc")
	if got := rr.Body.Len(); got != videoFollowMaxResponseBytes {
		t.Errorf("relayed %d bytes, want the %d ceiling", got, videoFollowMaxResponseBytes)
	}
}

// --- poll: hostile job ids ----------------------------------------------------

func TestVideoJobIDUnsafe(t *testing.T) {
	for _, bad := range []string{".", "..", "a/b", `a\b`, "../x", "video/../../admin"} {
		if !videoJobIDUnsafe(bad) {
			t.Errorf("videoJobIDUnsafe(%q) = false, want true", bad)
		}
	}
	for _, good := range []string{"video_abc", "veo_bW9kZWxzL3Zlby0zLjE", "id-with.dots", "..." /* three dots is a plain segment */} {
		if videoJobIDUnsafe(good) {
			t.Errorf("videoJobIDUnsafe(%q) = true, want false", good)
		}
	}
}

// A hostile provider-planted job id must never be relayed into a URL.
func TestServeVideoPoll_HostileStoredIDNotRelayed(t *testing.T) {
	up := newFollowUpstream(t, http.StatusOK, `{}`)
	deps, js, _, _ := videoFollowDeps(t, up.srv.URL)
	js.owned.ID = "../../admin/keys"
	h := NewHandler(deps).ServeVideoPoll(videoIngress())

	rr := doVideoFollow(h, http.MethodGet, "whatever")
	if rr.Code != http.StatusBadGateway || !strings.Contains(rr.Body.String(), "VIDEO_JOB_ID_UNSAFE") {
		t.Fatalf("resp = %d %q, want 502 VIDEO_JOB_ID_UNSAFE", rr.Code, rr.Body.String())
	}
	if up.hits != 0 {
		t.Errorf("upstream hits = %d, want 0", up.hits)
	}
}

// Reserved characters in a stored id are escaped into the upstream URL — the
// request line carries the percent-encoded form, one path segment.
func TestServeVideoPoll_EscapesStoredIDInURL(t *testing.T) {
	up := newFollowUpstream(t, http.StatusOK, `{"id":"video a?b","status":"queued"}`)
	deps, js, _, _ := videoFollowDeps(t, up.srv.URL)
	js.owned.ID = "video a?b#c"
	h := NewHandler(deps).ServeVideoPoll(videoIngress())

	doVideoFollow(h, http.MethodGet, "whatever")
	if up.uri != "/v1/videos/video%20a%3Fb%23c" {
		t.Errorf("upstream RequestURI = %q, want /v1/videos/video%%20a%%3Fb%%23c", up.uri)
	}
}

// --- poll: admission parity ---------------------------------------------------

func TestServeVideoPoll_AuthAndRateLimit(t *testing.T) {
	up := newFollowUpstream(t, http.StatusOK, `{}`)
	deps, _, _, _ := videoFollowDeps(t, up.srv.URL)
	deps.VKAuth = videoAuthErr{err: errors.New("bad key")}
	h := NewHandler(deps).ServeVideoPoll(videoIngress())
	if rr := doVideoFollow(h, http.MethodGet, "video_abc"); rr.Code != http.StatusUnauthorized {
		t.Errorf("auth error → %d, want 401", rr.Code)
	}

	deps2, _, _, _ := videoFollowDeps(t, up.srv.URL)
	rpm := 5
	deps2.VKAuth = rpmVKAuth{rpm: &rpm}
	deps2.RateLimiter = denyAllRateLimiter{}
	h2 := NewHandler(deps2).ServeVideoPoll(videoIngress())
	if rr := doVideoFollow(h2, http.MethodGet, "video_abc"); rr.Code != http.StatusTooManyRequests {
		t.Errorf("rate limited → %d, want 429", rr.Code)
	}

	// A mount without the {id} pattern (route misconfiguration) yields an
	// empty PathValue — a 404, never an empty-id store query or relay.
	deps3, _, _, _ := videoFollowDeps(t, up.srv.URL)
	h3 := NewHandler(deps3).ServeVideoPoll(videoIngress())
	req := httptest.NewRequest(http.MethodGet, "/v1/videos/x", nil) // no SetPathValue
	rr := httptest.NewRecorder()
	h3(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("empty path id → %d, want 404", rr.Code)
	}

	if up.hits != 0 {
		t.Errorf("upstream hits = %d, want 0", up.hits)
	}
}

// An unreachable upstream is a 502 with no cache mutation — the client
// retries; the job row is untouched.
func TestServeVideoPoll_UpstreamUnreachable502(t *testing.T) {
	deps, js, _, _ := videoFollowDeps(t, "http://127.0.0.1:1") // nothing listens
	h := NewHandler(deps).ServeVideoPoll(videoIngress())
	rr := doVideoFollow(h, http.MethodGet, "video_abc")
	if rr.Code != http.StatusBadGateway || !strings.Contains(rr.Body.String(), "UPSTREAM_ERROR") {
		t.Fatalf("resp = %d %q, want 502 UPSTREAM_ERROR", rr.Code, rr.Body.String())
	}
	if len(js.observed) != 0 || js.claims != 0 {
		t.Errorf("cache mutated on upstream failure, want untouched")
	}
}

// Misconfiguration arms fail closed as 502s, before any upstream contact:
// no resolver, and no adapter registered for the resolved format.
func TestServeVideoPoll_MisconfigurationArms(t *testing.T) {
	up := newFollowUpstream(t, http.StatusOK, `{}`)
	deps, _, _, _ := videoFollowDeps(t, up.srv.URL)
	deps.Resolver = nil
	h := NewHandler(deps).ServeVideoPoll(videoIngress())
	if rr := doVideoFollow(h, http.MethodGet, "video_abc"); rr.Code != http.StatusBadGateway {
		t.Errorf("nil resolver → %d, want 502", rr.Code)
	}

	deps2, _, _, _ := videoFollowDeps(t, up.srv.URL)
	empty := provcore.NewRegistry()
	empty.Freeze()
	deps2.ProviderReg = empty
	h2 := NewHandler(deps2).ServeVideoPoll(videoIngress())
	if rr := doVideoFollow(h2, http.MethodGet, "video_abc"); rr.Code != http.StatusBadGateway {
		t.Errorf("no adapter → %d, want 502", rr.Code)
	}
	if up.hits != 0 {
		t.Errorf("upstream hits = %d, want 0", up.hits)
	}
}

// Bookkeeping failures never fail the client's poll: a MarkObserved error and
// a ReconcileOnce error each WARN and relay; the claim error leaves the
// reconcile for a later poll (fail toward retriable, never double-charge).
func TestServeVideoPoll_BookkeepingErrorsDoNotFailRelay(t *testing.T) {
	body := `{"id":"video_abc","status":"completed"}`
	up := newFollowUpstream(t, http.StatusOK, body)
	deps, js, _, _ := videoFollowDeps(t, up.srv.URL)
	js.observeErr = errors.New("pg down")
	js.claimErr = errors.New("pg down")
	engine, uc := videoQuotaHarness(t, 1_000_000)
	deps.QuotaEngine = engine
	h := NewHandler(deps).ServeVideoPoll(videoIngress())

	rr := doVideoFollow(h, http.MethodGet, "video_abc")
	if rr.Code != http.StatusOK || rr.Body.String() != body {
		t.Fatalf("relay = %d %q, want 200 verbatim despite bookkeeping failures", rr.Code, rr.Body.String())
	}
	if got := vkUsageCents(t, uc); got != 0 {
		t.Errorf("live debit = %d cents, want 0 (claim not taken — a later poll retries)", got)
	}
}

// A completed first observation with NO quota engine wired still relays and
// still claims (the persistent counters come from the submit row's estimate).
func TestServeVideoPoll_CompletedWithoutQuotaEngine(t *testing.T) {
	up := newFollowUpstream(t, http.StatusOK, `{"id":"video_abc","status":"completed"}`)
	deps, js, _, _ := videoFollowDeps(t, up.srv.URL)
	js.claimResult = true
	deps.QuotaEngine = nil
	h := NewHandler(deps).ServeVideoPoll(videoIngress())
	if rr := doVideoFollow(h, http.MethodGet, "video_abc"); rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}
	if js.claims != 1 {
		t.Errorf("claims = %d, want 1", js.claims)
	}
}

// --- delete -------------------------------------------------------------------

// A provider-accepted delete marks the row canceled with the VK predicate
// (defense-in-depth) and relays the provider response.
func TestServeVideoDelete_MarksCanceled(t *testing.T) {
	body := `{"id":"video_abc","object":"video","deleted":true}`
	up := newFollowUpstream(t, http.StatusOK, body)
	deps, js, prod, _ := videoFollowDeps(t, up.srv.URL)
	h := NewHandler(deps).ServeVideoDelete(videoIngress())

	rr := doVideoFollow(h, http.MethodDelete, "video_abc")
	if rr.Code != http.StatusOK || rr.Body.String() != body {
		t.Fatalf("relay = %d %q, want 200 verbatim", rr.Code, rr.Body.String())
	}
	if up.method != http.MethodDelete || up.uri != "/v1/videos/video_abc" {
		t.Errorf("upstream saw %s %s, want DELETE /v1/videos/video_abc", up.method, up.uri)
	}
	if len(js.canceled) != 1 || js.canceled[0] != [3]string{"p-openai", "video_abc", "vk-1"} {
		t.Errorf("canceled = %v, want one (p-openai, video_abc, vk-1) mark", js.canceled)
	}
	msg := lastAudit(t, deps, prod)
	if msg.EstimatedCostUsd != 0 {
		t.Errorf("delete row cost = %v, want $0", msg.EstimatedCostUsd)
	}
}

// A provider-rejected delete must NOT mark the row canceled — the render (and
// its render-bound slot) is still live at the provider.
func TestServeVideoDelete_ProviderErrorNoMark(t *testing.T) {
	up := newFollowUpstream(t, http.StatusConflict, `{"error":{"message":"cannot delete while in_progress"}}`)
	deps, js, _, _ := videoFollowDeps(t, up.srv.URL)
	h := NewHandler(deps).ServeVideoDelete(videoIngress())

	rr := doVideoFollow(h, http.MethodDelete, "video_abc")
	if rr.Code != http.StatusConflict {
		t.Fatalf("relay = %d, want provider 409 verbatim", rr.Code)
	}
	if len(js.canceled) != 0 {
		t.Errorf("canceled = %v, want none", js.canceled)
	}
}

// A failed local mark must not fail the relay: the provider already accepted
// the delete; the row stays non-terminal until the retention sweep (fails
// toward bounding spend, never toward losing the client's result).
func TestServeVideoDelete_MarkErrorStillRelays(t *testing.T) {
	up := newFollowUpstream(t, http.StatusOK, `{"deleted":true}`)
	deps, js, _, _ := videoFollowDeps(t, up.srv.URL)
	js.cancelErr = errors.New("pg down")
	h := NewHandler(deps).ServeVideoDelete(videoIngress())

	rr := doVideoFollow(h, http.MethodDelete, "video_abc")
	if rr.Code != http.StatusOK {
		t.Errorf("relay = %d, want 200 despite the mark failure", rr.Code)
	}
}

// A deliberately-unserved sub-route answers an OpenAI-shaped 404 ENVELOPE
// (JSON body + machine code + audit row), not the mux's bare-text 404 an SDK
// cannot distinguish from a wrong base URL.
func TestServeVideoUnsupported(t *testing.T) {
	deps, _, prod, _ := videoFollowDeps(t, "http://irrelevant")
	h := NewHandler(deps).ServeVideoUnsupported(videoIngress(),
		"VIDEO_LIST_UNSUPPORTED", "listing videos is not served", "poll by id")

	req := httptest.NewRequest(http.MethodGet, "/v1/videos", nil)
	rr := httptest.NewRecorder()
	h(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if !strings.Contains(rr.Body.String(), "VIDEO_LIST_UNSUPPORTED") ||
		!strings.Contains(rr.Body.String(), "poll by id") {
		t.Errorf("body %q missing the machine code / hint", rr.Body.String())
	}
	// It produces an audit row (unlike a bare mux 404).
	msg := lastAudit(t, deps, prod)
	if msg.EndpointType != string(typology.EndpointKindVideoGeneration) || msg.StatusCode != http.StatusNotFound {
		t.Errorf("audit row = (%q, %d), want (video_generation, 404)", msg.EndpointType, msg.StatusCode)
	}
}

// The delete arm shares the owned-lookup admission: unknown id → 404
// non-disclosure, expired → 410, without touching the provider.
func TestServeVideoDelete_NotFoundAndExpired(t *testing.T) {
	up := newFollowUpstream(t, http.StatusOK, `{}`)
	deps, js, _, _ := videoFollowDeps(t, up.srv.URL)
	js.owned = nil
	h := NewHandler(deps).ServeVideoDelete(videoIngress())
	if rr := doVideoFollow(h, http.MethodDelete, "video_x"); rr.Code != http.StatusNotFound {
		t.Errorf("unknown id → %d, want 404", rr.Code)
	}

	deps2, js2, _, _ := videoFollowDeps(t, up.srv.URL)
	js2.owned.Status = asyncjob.StatusExpired
	h2 := NewHandler(deps2).ServeVideoDelete(videoIngress())
	if rr := doVideoFollow(h2, http.MethodDelete, "video_abc"); rr.Code != http.StatusGone {
		t.Errorf("expired row → %d, want 410", rr.Code)
	}
	if up.hits != 0 {
		t.Errorf("upstream hits = %d, want 0", up.hits)
	}
}
