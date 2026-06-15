// Package aiguard owns the Control Plane admin API for the AI Guard
// built-in hook config + dry-run proxy. Writes hit the singleton
// configstore row directly; the Hub-pushed ai_guard invalidation
// causes ai-gateway to drop its in-process snapshot on the next
// request. R6 (R8-B1 retry) — one of four compliance sub-clusters
// per r6-handler-decomp-runbook.md §5.
package aiguard

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configkey"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/configstore"
	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
)

// ConfigStore is the narrow seam the handler needs from
// configstore.AIGuardStore. Defined here so unit tests can substitute
// an in-memory double without reaching a real pool.
type ConfigStore interface {
	Load(ctx context.Context) (*configstore.AIGuardConfig, error)
	Save(ctx context.Context, cfg *configstore.AIGuardConfig) error
}

// DryRunRequest mirrors the /v1/ai-guard/classify request body. Admin
// UI posts the same JSON shape; the dispatcher forwards it verbatim
// to ai-gateway. Fields duplicated here (not imported from ai-gateway)
// because ai-gateway/internal/aiguard is not reachable from this
// module.
type DryRunRequest struct {
	DetectorType string        `json:"detector_type"`
	Content      string        `json:"content"`
	Context      DryRunContext `json:"context"`
}

// DryRunContext mirrors aiguard.Context.
type DryRunContext struct {
	Ingress        string   `json:"ingress,omitempty"`
	TargetProvider string   `json:"target_provider,omitempty"`
	TargetModel    string   `json:"target_model,omitempty"`
	UpstreamTags   []string `json:"upstream_tags,omitempty"`
	HookName       string   `json:"hook_name,omitempty"`
}

// DryRunResponse mirrors aiguard.Response.
type DryRunResponse struct {
	Decision   string            `json:"decision"`
	Confidence float64           `json:"confidence,omitempty"`
	Reason     string            `json:"reason,omitempty"`
	Labels     []string          `json:"labels,omitempty"`
	Redactions []DryRunRedaction `json:"redactions,omitempty"`
	Metadata   DryRunMetadata    `json:"metadata"`
}

// DryRunRedaction mirrors aiguard.Redaction.
type DryRunRedaction struct {
	Start       int    `json:"start"`
	End         int    `json:"end"`
	Replacement string `json:"replacement,omitempty"`
	Action      string `json:"action,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

// DryRunMetadata mirrors aiguard.Metadata.
type DryRunMetadata struct {
	JudgeModel     string `json:"judge_model,omitempty"`
	JudgeLatencyMs int    `json:"judge_latency_ms"`
	CacheHit       bool   `json:"cache_hit"`
	BackendMode    string `json:"backend_mode,omitempty"`
}

// DryRunDispatcher forwards a classify request to the ai-gateway
// /v1/ai-guard/classify endpoint. Nil in unit tests; wired to
// HTTPDispatcher in main.go.
type DryRunDispatcher interface {
	Dispatch(ctx context.Context, req DryRunRequest) (*DryRunResponse, error)
}

// HubConfigChanger is the narrow Hub surface aiguard/ needs:
// InvalidateConfig fires the ai_guard shadow-key invalidation
// (Category B — state bytes are ignored by the ai-gateway
// receiver, which always rereads its in-process snapshot on
// the next request). Matches the hooks / interception
// InvalidateConfig contract.
type HubConfigChanger interface {
	InvalidateConfig(ctx context.Context, thingType, configKey string)
}

// Deps is the construction-time arg shape.
type Deps struct {
	Store      ConfigStore
	Hub        HubConfigChanger // may be nil — invalidation skipped, reconcile job recovers within 60s
	Dispatcher DryRunDispatcher // may be nil — /dry-run then returns 503
	Audit      *audit.Writer    // may be nil (tests) — audit emission skipped silently
	Logger     *slog.Logger
}

// Handler owns /api/admin/ai-guard/* routes: the singleton config
// GET/PUT and the dry-run proxy.
type Handler struct {
	store      ConfigStore
	hub        HubConfigChanger
	dispatcher DryRunDispatcher
	audit      *audit.Writer
	logger     *slog.Logger
}

// New constructs the handler from its narrow Deps. hub may be nil
// (e.g. when Hub coordination is not configured); the invalidation
// push becomes a no-op and the data plane converges on the next
// aiguard.ConfigCache TTL expiry. dispatcher may be nil during early
// startup / tests; /dry-run then returns 503. Audit may be nil
// (tests); when nil, mutating endpoints skip audit emission silently.
func New(d Deps) *Handler {
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{
		store:      d.Store,
		hub:        d.Hub,
		dispatcher: d.Dispatcher,
		audit:      d.Audit,
		logger:     logger,
	}
}

// emitAudit publishes an admin-audit entry on successful mutation.
// nil-safe on the writer pointer so test construction stays ergonomic.
func (h *Handler) emitAudit(c echo.Context, e audit.Entry) {
	if h.audit == nil {
		return
	}
	h.audit.LogObserved(c.Request().Context(), e)
}

// RegisterRoutes mounts the three AI Guard endpoints under the
// caller-supplied admin group. iamMW gates each route per the
// AIGuardConfig resource's verb taxonomy.
func (h *Handler) RegisterRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	g.GET("/ai-guard/config", h.GetConfig, iamMW(iam.ResourceAIGuardConfig.Action(iam.VerbRead)))
	g.PUT("/ai-guard/config", h.PutConfig, iamMW(iam.ResourceAIGuardConfig.Action(iam.VerbUpdate)))
	g.POST("/ai-guard/dry-run", h.DryRun, iamMW(iam.ResourceAIGuardConfig.Action(iam.VerbUpdate)))
}

// GetConfig returns the current singleton AIGuardConfig JSON.
func (h *Handler) GetConfig(c echo.Context) error {
	cfg, err := h.store.Load(c.Request().Context())
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]any{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, cfg)
}

// PutConfig upserts the singleton config. Recomputes BackendFingerprint
// server-side so the admin UI cannot stamp a stale value. Pushes an
// `ai_guard` invalidation through Hub so ai-gateway drops its
// in-process cache on the next request.
func (h *Handler) PutConfig(c echo.Context) error {
	var in configstore.AIGuardConfig
	if err := c.Bind(&in); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]any{"error": "malformed_json", "detail": err.Error()})
	}
	in.ID = "singleton"

	before, _ := h.store.Load(c.Request().Context())

	switch in.BackendMode {
	case "configured_provider":
		if in.ProviderID == nil || *in.ProviderID == "" || in.ModelID == nil || *in.ModelID == "" {
			return c.JSON(http.StatusBadRequest, map[string]any{
				"error": "providerId and modelId required for configured_provider",
			})
		}
	case "external_url":
		if in.ExternalURL == nil || *in.ExternalURL == "" {
			return c.JSON(http.StatusBadRequest, map[string]any{
				"error": "externalUrl required for external_url",
			})
		}
		// Require https for the external judge endpoint. The
		// external backend authenticates via operator-supplied CustomHeaders
		// (its only auth channel — no provider Credential is ever attached),
		// so a plaintext http:// URL would leak that operator token on the
		// wire. Rejecting non-https is cheap defence-in-depth against an
		// on-path attacker and the most common SSRF-to-internal-service
		// misconfiguration (http://169.254.169.254/...).
		if u, perr := url.Parse(*in.ExternalURL); perr != nil || u.Scheme != "https" || u.Host == "" {
			return c.JSON(http.StatusBadRequest, map[string]any{
				"error": "externalUrl must be a valid https:// URL",
			})
		}
	default:
		return c.JSON(http.StatusBadRequest, map[string]any{"error": "unknown backendMode"})
	}
	if in.PromptTemplate == "" {
		return c.JSON(http.StatusBadRequest, map[string]any{"error": "promptTemplate required"})
	}
	if in.TimeoutMs <= 0 {
		// 30s covers a single judge-model call on slow upstreams (OpenAI gpt-*,
		// Anthropic sonnet, etc.) without keeping the gateway-side request open
		// indefinitely. Operators can lower this via the admin UI.
		in.TimeoutMs = 30000
	}
	if in.CacheTTLSeconds < 0 {
		in.CacheTTLSeconds = 0
	}
	// Validate and default InputStrategy.
	validStrategies := map[string]bool{
		"last_user": true, "system_plus_last_user": true,
		"recent_turns": true, "head_plus_tail": true, "full_truncated": true,
	}
	if !validStrategies[in.InputStrategy] {
		in.InputStrategy = "system_plus_last_user"
	}
	if in.ModelContextLimit < 0 {
		in.ModelContextLimit = 0
	}

	// Fingerprint inputs: mode | provider-or-url | model | sha256(prompt).
	// Mirror of aiguard.BackendFingerprint — reproduced here because the
	// ai-gateway package lives under an internal/ tree that CP cannot import.
	providerOrURL := ""
	modelStr := ""
	if in.ProviderID != nil {
		providerOrURL = *in.ProviderID
	}
	if in.ExternalURL != nil && providerOrURL == "" {
		providerOrURL = *in.ExternalURL
	}
	if in.ModelID != nil {
		modelStr = *in.ModelID
	}
	in.BackendFingerprint = backendFingerprint(
		in.BackendMode, providerOrURL, modelStr,
		promptTemplateSHA(in.PromptTemplate),
	)

	if err := h.store.Save(c.Request().Context(), &in); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]any{"error": err.Error()})
	}

	// Fire invalidation so ai-gateway reloads its cached snapshot.
	// Category B: the receiver ignores any state bytes and rereads
	// from configstore on next request, so InvalidateConfig (no
	// state payload) is the correct verb. Propagation failure is
	// logged inside InvalidateConfig (fire-and-forget); the row
	// write has already succeeded. The reconcile job watches
	// `ai_guard` and re-emits within 60s if Hub was transiently
	// unreachable, so we don't escalate to 502 here.
	if h.hub != nil {
		h.hub.InvalidateConfig(c.Request().Context(), "ai-gateway", configkey.AIGuard)
	}

	ae := audit.EntryFor(c, iam.ResourceAIGuardConfig, iam.VerbUpdate)
	ae.EntityID = in.ID
	if before != nil {
		ae.BeforeState = configAuditSummary(before)
	}
	ae.AfterState = configAuditSummary(&in)
	h.emitAudit(c, ae)

	return c.JSON(http.StatusOK, in)
}

// configAuditSummary returns a compact projection of the AI Guard
// singleton config suitable for admin-audit BeforeState / AfterState
// payloads. The full prompt template text is replaced with its SHA
// so audit entries stay bounded even for long templates.
func configAuditSummary(cfg *configstore.AIGuardConfig) map[string]any {
	if cfg == nil {
		return nil
	}
	providerID := ""
	if cfg.ProviderID != nil {
		providerID = *cfg.ProviderID
	}
	modelID := ""
	if cfg.ModelID != nil {
		modelID = *cfg.ModelID
	}
	externalURL := ""
	if cfg.ExternalURL != nil {
		externalURL = *cfg.ExternalURL
	}
	return map[string]any{
		"backendMode":        cfg.BackendMode,
		"providerId":         providerID,
		"modelId":            modelID,
		"externalUrl":        externalURL,
		"promptTemplateSha":  promptTemplateSHA(cfg.PromptTemplate),
		"timeoutMs":          cfg.TimeoutMs,
		"cacheTtlSeconds":    cfg.CacheTTLSeconds,
		"backendFingerprint": cfg.BackendFingerprint,
		"inputStrategy":      cfg.InputStrategy,
		"modelContextLimit":  cfg.ModelContextLimit,
	}
}

// DryRun proxies a classify request to ai-gateway and returns both
// the original request and the response so the UI can render them
// side by side. Returns 503 when no dispatcher is configured, 400 on
// malformed input, 502 on downstream dispatcher failure.
func (h *Handler) DryRun(c echo.Context) error {
	if h.dispatcher == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]any{
			"error":  "dry-run not available",
			"detail": "dispatcher not configured",
		})
	}
	var req DryRunRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]any{"error": "malformed_json", "detail": err.Error()})
	}
	if req.DetectorType == "" || req.Content == "" {
		return c.JSON(http.StatusBadRequest, map[string]any{
			"error":  "missing_required_field",
			"detail": "detector_type and content are required",
		})
	}
	resp, err := h.dispatcher.Dispatch(c.Request().Context(), req)
	if err != nil {
		return c.JSON(http.StatusBadGateway, map[string]any{
			"error":   "dispatch_failed",
			"detail":  err.Error(),
			"request": req,
		})
	}
	return c.JSON(http.StatusOK, map[string]any{
		"request":  req,
		"response": resp,
	})
}

// HTTPDispatcher implements DryRunDispatcher by posting the request
// to the ai-gateway /v1/ai-guard/classify endpoint with the shared
// service token header the rstokenauth middleware expects.
type HTTPDispatcher struct {
	BaseURL    string
	Token      string
	HTTPClient *http.Client
}

// Dispatch forwards req to the ai-gateway and parses the response.
func (d *HTTPDispatcher) Dispatch(ctx context.Context, req DryRunRequest) (*DryRunResponse, error) {
	buf, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, d.BaseURL+"/v1/ai-guard/classify", bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-RS-Token", d.Token)
	client := d.HTTPClient
	if client == nil {
		// Dry-run is interactive and may sit on top of a 30s judge call,
		// plus marshal/unmarshal + network. 60s leaves a comfortable margin.
		client = nexushttp.New(nexushttp.Config{
			Timeout:        60 * time.Second,
			Caller:         "cp-admin-aiguard",
			PropagateReqID: true,
		})
	}
	httpResp, err := client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close() //nolint:errcheck
	if httpResp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(httpResp.Body, 1024))
		return nil, fmt.Errorf("ai-gateway classify status=%d body=%s", httpResp.StatusCode, body)
	}
	var resp DryRunResponse
	if err := json.NewDecoder(httpResp.Body).Decode(&resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// backendFingerprint mirrors
// packages/ai-gateway/internal/policy/aiguard.BackendFingerprint.
// Reproduced here because the source lives under an internal/ tree
// this module cannot import. Identical algorithm, covered by a
// cross-package sync test.
func backendFingerprint(mode, providerOrURL, model, promptTemplateSHA string) string {
	h := sha256.New()
	h.Write([]byte(mode))
	h.Write([]byte{'|'})
	h.Write([]byte(providerOrURL))
	h.Write([]byte{'|'})
	h.Write([]byte(model))
	h.Write([]byte{'|'})
	h.Write([]byte(promptTemplateSHA))
	return hex.EncodeToString(h.Sum(nil))
}

// promptTemplateSHA mirrors
// packages/ai-gateway/internal/policy/aiguard.PromptTemplateSHA.
func promptTemplateSHA(template string) string {
	sum := sha256.Sum256([]byte(template))
	return hex.EncodeToString(sum[:])
}
