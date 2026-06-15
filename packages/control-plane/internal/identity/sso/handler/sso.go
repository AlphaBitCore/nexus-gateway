package sso

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/labstack/echo/v4"

	"github.com/jackc/pgx/v5/pgxpool"

	authserver_store "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/token"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/iam"
	systemmetastore "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store/systemmetastore"
	sharediam "github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/pkce"
)

// systemMetaReader is the narrow interface AgentEnrollHandler uses to read
// system metadata (device auth mode). Satisfied by *systemmetastore.Store.
type systemMetaReader interface {
	GetSystemMetadata(ctx context.Context, key string) (json.RawMessage, error)
}

// enrollUserChecker verifies a user is still active before issuing an
// enrollment JWT. Satisfied by *authserver_store.UserStore.
type enrollUserChecker interface {
	GetByID(ctx context.Context, id string) (*authserver_store.User, error)
}

const (
	enrollJWTTTL    = 5 * time.Minute
	enrollAudience  = "nexus-hub-enrollment"
	enrollPurpose   = "enrollment"
	enrollMaxPerMin = 10
)

// AgentEnrollHandler issues enrollment JWTs after validating a CP-originated
// OAuth authorization code + PKCE verifier.
// The endpoint is unauthenticated: pre-enrollment devices have no cert.
// Authorization is performed in-handler via the IAM Engine using the
// auth-code's owning user as the principal — see SSOEnroll for the
// `admin:device-enrollment.enroll` check that gates both
// enterprise-login and local-login device-auth modes.
type AgentEnrollHandler struct {
	AuthCodes *authserver_store.AuthCodeStore
	Signer    *token.Signer
	Pool      *pgxpool.Pool          // used to construct NewUserStore; nil-safe (kept for callers; see userChecker)
	Meta      *systemmetastore.Store // used to read device.auth.mode; nil-safe (kept for callers; see metaReader)
	IAM       *iam.Engine
	Issuer    string
	Logger    *slog.Logger
	// metaReader is the test seam for system-metadata lookups. When nil,
	// the handler falls back to Meta. Tests inject a stub; production callers
	// leave this nil and set Meta instead.
	metaReader systemMetaReader
	// userChecker is the test seam for user-active validation. When nil,
	// the handler constructs a UserStore from Pool. Tests inject a stub;
	// production callers leave this nil and set Pool instead.
	userChecker enrollUserChecker

	rateMu  sync.Mutex
	rateBkt map[string]*rateBucket

	gcOnce sync.Once
	stopCh chan struct{}
}

type rateBucket struct {
	count   int
	resetAt time.Time
}

// Init starts the background sweep that evicts expired rate buckets
// every minute. Without it the per-IP map grows unboundedly under
// attack traffic. Idempotent.
func (h *AgentEnrollHandler) Init() {
	h.gcOnce.Do(func() {
		h.stopCh = make(chan struct{})
		go h.gcLoop(time.Minute)
	})
}

// Close stops the background sweep goroutine. Idempotent.
func (h *AgentEnrollHandler) Close() {
	if h.stopCh == nil {
		return
	}
	select {
	case <-h.stopCh:
		// already closed
	default:
		close(h.stopCh)
	}
}

func (h *AgentEnrollHandler) gcLoop(interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-h.stopCh:
			return
		case <-t.C:
			h.gc()
		}
	}
}

func (h *AgentEnrollHandler) gc() {
	now := time.Now()
	h.rateMu.Lock()
	defer h.rateMu.Unlock()
	for ip, b := range h.rateBkt {
		if now.After(b.resetAt) {
			delete(h.rateBkt, ip)
		}
	}
}

// allow returns (ok, retryAfterSeconds). When ok is false the caller
// should respond 429 with a Retry-After header equal to retryAfterSeconds.
func (h *AgentEnrollHandler) allow(ip string) (bool, int) {
	h.rateMu.Lock()
	defer h.rateMu.Unlock()
	if h.rateBkt == nil {
		h.rateBkt = make(map[string]*rateBucket)
	}
	now := time.Now()
	b, ok := h.rateBkt[ip]
	if !ok || now.After(b.resetAt) {
		h.rateBkt[ip] = &rateBucket{count: 1, resetAt: now.Add(time.Minute)}
		return true, 0
	}
	if b.count >= enrollMaxPerMin {
		retry := int(time.Until(b.resetAt).Seconds()) + 1
		if retry < 1 {
			retry = 1
		}
		return false, retry
	}
	b.count++
	return true, 0
}

// enrollmentClaims is the claim set for SSO enrollment JWTs.
type enrollmentClaims struct {
	jwt.RegisteredClaims
	Email   string `json:"email,omitempty"`
	Purpose string `json:"purpose"`
}

// SSOEnroll handles POST /api/agent/sso-enroll.
func (h *AgentEnrollHandler) SSOEnroll(c echo.Context) error {
	ip := c.RealIP()
	if ok, retryAfter := h.allow(ip); !ok {
		c.Response().Header().Set("Retry-After", fmt.Sprintf("%d", retryAfter))
		return c.JSON(http.StatusTooManyRequests, map[string]string{"error": "rate_limited"})
	}

	var req struct {
		Code         string `json:"code"`
		CodeVerifier string `json:"code_verifier"`
		RedirectURI  string `json:"redirect_uri"`
	}
	if err := c.Bind(&req); err != nil || req.Code == "" || req.CodeVerifier == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid_code"})
	}

	ctx := c.Request().Context()

	// Reject when device auth mode is mtls-only.
	// metaReader is the test seam; fall back to Meta for production callers.
	metaRdr := h.metaReader
	if metaRdr == nil && h.Meta != nil {
		metaRdr = h.Meta
	}
	if metaRdr != nil {
		// Key string is duplicated from handler/settings/device_auth.go
		// (deviceAuthModeKey const), which is the canonical writer. SSO
		// enrollment is a read-only consumer; if a future refactor
		// promotes this constant to handler/util/ the value stays
		// "device.auth.mode" (the system_metadata row both ends share).
		raw, _ := metaRdr.GetSystemMetadata(ctx, "device.auth.mode")
		if raw != nil {
			var mode string
			if json.Unmarshal(raw, &mode) == nil && mode == "mtls-only" {
				return c.JSON(http.StatusBadRequest, map[string]string{"error": "enrollment_mode_mtls"})
			}
		}
	}

	// Consume auth code (single-use; Get deletes the entry on hit).
	entry, ok := h.AuthCodes.Get(req.Code)
	if !ok {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid_code"})
	}

	// Validate redirect_uri matches what was recorded in the authorize request.
	if req.RedirectURI != entry.RedirectURI {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid_code"})
	}

	// Validate PKCE S256 verifier.
	if !pkce.VerifyS256(req.CodeVerifier, entry.PKCEChallenge) {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid_verifier"})
	}

	// Check user is still active.
	// userChecker is the test seam; fall back to constructing a UserStore from Pool.
	checker := h.userChecker
	if checker == nil && h.Pool != nil {
		checker = authserver_store.NewUserStore(h.Pool)
	}
	if checker != nil {
		user, err := checker.GetByID(ctx, entry.UserID)
		if err != nil {
			h.Logger.Warn("sso-enroll: user lookup", "user_id", entry.UserID, "error", err)
			return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid_code"})
		}
		if user.DisabledAt != nil {
			return c.JSON(http.StatusForbidden, map[string]string{"error": "user_disabled"})
		}
	}

	// IAM gate — applied uniformly to both enterprise-login and
	// local-login device-auth modes. The endpoint has no Bearer
	// session (the agent is pre-enrollment), so the principal is the
	// user who minted the OAuth auth code, not an admin session. The
	// check matches the canonical pattern used by RequireIAMPermission
	// middleware: principal type "nexus_user", action
	// "admin:device-enrollment.enroll", resource derived from action.
	if h.IAM != nil {
		action := sharediam.ResourceDeviceEnrollment.Action(sharediam.VerbEnroll)
		resource := iam.BuildRequestNRNForAction(action)
		condCtx := iam.ConditionContext{
			"nexus:SourceIp": c.RealIP(),
		}
		result, err := h.IAM.Evaluate(ctx, "nexus_user", entry.UserID, action, resource, condCtx)
		if err != nil {
			h.Logger.Error("sso-enroll: iam evaluate", "user_id", entry.UserID, "error", err)
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "internal_error"})
		}
		if result.Decision != "Allow" {
			h.Logger.Warn("sso-enroll: iam denied",
				"user_id", entry.UserID,
				"email", entry.Email,
				"action", action,
				"reason", result.Reason,
			)
			return c.JSON(http.StatusForbidden, map[string]string{"error": "iam_denied"})
		}
	}

	// Issue enrollment JWT (5-min TTL, single-use via Hub's JTI replay guard).
	jti := newEnrollJTI()
	now := time.Now()
	exp := now.Add(enrollJWTTTL)
	claims := enrollmentClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    h.Issuer,
			Subject:   entry.UserID,
			Audience:  jwt.ClaimStrings{enrollAudience},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
			ID:        jti,
		},
		Email:   entry.Email,
		Purpose: enrollPurpose,
	}

	signed, err := h.Signer.Sign(claims)
	if err != nil {
		h.Logger.Error("sso-enroll: sign JWT", "error", err)
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "internal_error"})
	}

	// Log only non-sensitive identifiers per security policy.
	h.Logger.Debug("sso-enroll: issued enrollment JWT", "sub", entry.UserID, "jti", jti)

	return c.JSON(http.StatusOK, map[string]any{
		"enrollment_jwt": signed,
		"user_email":     entry.Email,
		"expires_at":     exp.UTC().Format(time.RFC3339),
	})
}

func newEnrollJTI() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
