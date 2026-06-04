package assistant

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore"
	"github.com/labstack/echo/v4"
	"github.com/redis/go-redis/v9"
)

// Config holds the backend-side settings for the web assistant. The system VK is a
// secret (env, never yaml) used only for inference and never sent to the browser.
type Config struct {
	AIGatewayURL string   // AI Gateway base URL (inference target)
	CPBaseURL    string   // this CP's own base URL (admin self-call target)
	SystemVK     string   // backend system Virtual Key for inference
	Model        string   // default inference model slug
	Models       []string // optional allow-list of client-selectable models; empty → only Model
	IsProd       bool
	// DisableBodyReads withholds the raw-body read tools (observe_traffic_event /
	// observe_traffic_list / resource_read / resource_invoke) from the agent — the
	// §8 governance posture for deployments that do not want raw traffic bodies
	// reachable by the assistant at all. The aggregate/analysis tools stay.
	DisableBodyReads bool
	// TurnDeadline overrides the wall-clock turn backstop. Zero → the default
	// (turnDeadline). Set it below the ingress idle/read timeout so the clean
	// `turn_deadline` SSE error fires before the proxy severs the stream.
	TurnDeadline time.Duration
	Pool         pgxPool               // DB pool for metadata persistence; nil → in-memory stores (tests / pool-less dev)
	Spill        spillstore.SpillStore // object storage for transcript content (shared spill backend); nil → in-memory sessions
	// Redis + OwnerID drive the multi-replica session-owner registry (the 421
	// affinity safety net). nil Redis or blank OwnerID → single-replica behavior
	// (no ownership tracking, no 421).
	Redis   redis.UniversalClient
	OwnerID string
	// Dispatcher is the CP echo router. When set, the agent's admin self-calls are
	// dispatched in-process (R3): no loopback HTTP, an unforgeable AI-initiated audit
	// stamp, and the caller's IP preserved for the audit actor. nil → the agent's
	// core.Client falls back to a network transport (pool-less dev / tests).
	Dispatcher http.Handler
}

// turnDeadline is the wall-clock backstop on a single chat turn. The agent loop's
// StepCap already bounds runaway TOOL ROUNDS; this bounds total wall-clock so a
// hung upstream (an inference call that never returns) cannot wedge the turn
// forever. It is deliberately longer than confirmTimeout (a turn may legitimately
// wait on one human confirm) — it is a backstop, not a tight SLA.
const turnDeadline = 10 * time.Minute

// Handler serves the web assistant endpoints under the admin group.
type Handler struct {
	cfg            Config
	confirms       *confirmRegistry
	confirmTimeout time.Duration   // injectable for tests; defaults to confirmTimeout
	impactTimeout  time.Duration   // bounds the FR-22 impact-preview read; defaults to impactTimeout
	turnDeadline   time.Duration   // wall-clock backstop on one turn; injectable for tests
	situations     *situationCache // per-caller TTL cache for the ~8-call situation snapshot (NFR-11)
	redactor       agent.Redactor  // §8: scrubs PII from tool output before prompt entry; nil only on construction failure
	owners         *ownerRegistry  // multi-replica session-owner registry (421 safety net); nil → single-replica
	bus            *sessionBus     // P2b command/data-stream split: detached turns + reconnectable SSE
}

// New builds the assistant handler.
func New(cfg Config) *Handler {
	// The PII redactor is built from a static canonical pattern set, so
	// construction is deterministic; a non-nil error is a programming error in
	// piiPatternDefinitions (caught by TestNewPIIRedactor). Fail CLOSED — panic at
	// startup rather than ship an assistant that silently relays raw PII into the
	// prompt. This is unreachable in a built binary.
	redactor, err := newPIIRedactor()
	if err != nil {
		panic(fmt.Sprintf("assistant: PII redactor construction failed (static config bug): %v", err))
	}
	td := turnDeadline
	if cfg.TurnDeadline > 0 {
		td = cfg.TurnDeadline
	}
	// Owner TTL covers a full turn with margin so ownership never expires while a
	// confirm is parked; a dead pod's ownership then lapses (takeover via TTL).
	return &Handler{
		cfg:            cfg,
		confirms:       newConfirmRegistry(),
		confirmTimeout: confirmTimeout,
		impactTimeout:  impactTimeout,
		turnDeadline:   td,
		situations:     newSituationCache(situationTTL),
		redactor:       redactor,
		owners:         newOwnerRegistry(cfg.Redis, cfg.OwnerID, td+5*time.Minute),
		bus:            newSessionBus(),
	}
}

// RegisterAssistantRoutes mounts the assistant endpoints on the admin group
// (already behind AdminAuth). No new IAM action is minted: the endpoint is
// login-only and every tool the agent runs is IAM-checked at the admin API it
// self-calls (I1).
//
// The name is unique (not the generic `RegisterRoutes`) so the structural
// OpenAPI generator (`nexus openapi-gen`) can use it as an additional walk root:
// the assistant is runtime-wired in cmd/control-plane/wiring (outside
// `RegisterAdminRoutes`, the generator's default root), so without a distinct
// registrar name its routes would be invisible to the generated admin-API spec.
func (h *Handler) RegisterAssistantRoutes(g *echo.Group) {
	// P2b command/data-stream split: a turn is STARTED by POST .../chat (runs detached
	// in the background) and OBSERVED over the long-lived GET .../stream SSE channel,
	// which can reconnect with ?lastSeq= to replay missed events. Stop = POST
	// .../interrupt. confirm stays a separate POST (the turn parks server-side on it).
	g.POST("/assistant/sessions/:id/chat", h.StartChat)
	g.GET("/assistant/sessions/:id/stream", h.StreamSession)
	g.POST("/assistant/sessions/:id/interrupt", h.InterruptSession)
	g.POST("/assistant/confirm", h.Confirm)
	g.GET("/assistant/sessions", h.ListSessions)
	g.GET("/assistant/sessions/:id", h.GetSession)
	g.DELETE("/assistant/sessions/:id", h.DeleteSession)
	g.GET("/assistant/files/:id", h.DownloadFile)
	g.GET("/assistant/models", h.ListModels)
}

func errJSON(c echo.Context, status int, typ, msg string) error {
	return c.JSON(status, map[string]any{"error": map[string]any{"message": msg, "type": typ}})
}

// validSessionID bounds the client-supplied session id (now a path param, since the
// command/data-stream split needs the id BEFORE the turn's events exist). It is only
// ever resolved within the caller's own userId namespace (I3), so this is an input-
// hygiene guard, not an authorization check: non-empty, ≤128 chars, and a safe id
// charset (letters/digits/-_.: — covers the server's hex ids and client UUIDs).
func validSessionID(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.' || r == ':':
		default:
			return false
		}
	}
	return true
}

// callerBearer extracts the forwardable bearer + userId, or returns ok=false with a
// written HTTP error. The agent self-calls admin APIs AS THE CALLER (R3/I1), which
// requires a real bearer; a non-bearer principal (x-admin-key / bootstrap / dev /
// delegated API key) has none, so its tools would all 401 while still billing the
// system VK — reject before any inference. The adminGroup's AdminAuth already
// authenticated the request; this gates whether the assistant is usable for it.
func (h *Handler) callerBearer(c echo.Context) (authorization, userID string, ok bool) {
	authorization = c.Request().Header.Get("Authorization")
	if !strings.HasPrefix(authorization, "Bearer ") {
		_ = errJSON(c, http.StatusUnprocessableEntity, "unsupported_auth",
			"the assistant requires an interactive bearer admin session; API-key/service principals are not supported")
		return "", "", false
	}
	if aa := middleware.AdminAuthFromContext(c); aa != nil {
		userID = aa.KeyID
	}
	if userID == "" {
		_ = errJSON(c, http.StatusUnprocessableEntity, "unsupported_auth", "an interactive admin session is required")
		return "", "", false
	}
	return authorization, userID, true
}
