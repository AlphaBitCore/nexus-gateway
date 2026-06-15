// Package vkauth implements virtual key authentication for the AI gateway.
// It extracts VK credentials from request headers, looks up the VK in the
// database (by HMAC hash) and validates access.
package vkauth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/hmackeyring"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/keyderive"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	"github.com/jackc/pgx/v5"
)

// ingressFormatKey is the context key the handler stamps its detected
// ingress body format under; the extractor reads it to unlock
// provider-conventional VK carriers on matching native routes.
type ingressFormatKey struct{}

// WithIngressFormat returns ctx with f attached. The handler calls
// this from ServeProxy before invoking the authenticator, so the
// extractor can tell e.g. "we are serving /v1/messages" from "we are
// serving /v1/chat/completions". Callers without a native route should
// leave it unset — the extractor falls back to the OpenAI-compat
// carrier set (x-nexus-virtual-key, Authorization: Bearer).
func WithIngressFormat(ctx context.Context, f provcore.Format) context.Context {
	return context.WithValue(ctx, ingressFormatKey{}, f)
}

// ingressFormatFromContext returns the stamped format or empty string.
func ingressFormatFromContext(ctx context.Context) provcore.Format {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(ingressFormatKey{}).(provcore.Format); ok {
		return v
	}
	return ""
}

// Sentinel errors for VK authentication failures.
var (
	ErrMissing  = errors.New("vkauth: virtual key missing")
	ErrInvalid  = errors.New("vkauth: virtual key invalid")
	ErrDisabled = errors.New("vkauth: virtual key disabled")
	ErrExpired  = errors.New("vkauth: virtual key expired")
)

// VKMeta holds validated virtual key metadata attached to a request context.
type VKMeta struct {
	ID               string
	Name             string
	OrganizationID   string
	OrganizationName string
	// OrganizationTimezone is the IANA TZ name from the owning org.
	// Empty when the VK has no project/org binding. Used to compute
	// business-rule windows (monthly quota, "yesterday").
	OrganizationTimezone string
	ProjectID            string
	ProjectName          string
	SourceApp            string
	OwnerID              string
	UserDisplayName      string
	RateLimitRpm         *int
	// CompareEndpointRateLimitRpm is the per-VK cap for POST /v1/estimate.
	// nil → default 30/min applied in code. Separate from RateLimitRpm because
	// estimate requests dispatch N provider calls internally.
	CompareEndpointRateLimitRpm *int
	AllowedModels               []store.AllowedModelRef
	VKType                      string // "personal" | "application"
	VKStatus                    string // "active" | "pending" | "expired" | "rejected" | "revoked"
	// Fingerprint is SHA256(presentedKey)[:8] as a 16-char lowercase hex
	// string — stable per presented key, non-reversible. Used by the
	// traffic-event pipeline to attribute cost without storing the raw VK.
	Fingerprint string
	// Class is the classification label for the presented key (e.g.
	// "nvk_" for Gateway-issued virtual keys). Empty when the caller
	// authenticated via a non-Gateway-issued token.
	Class string
}

// VKLookup is the per-key lookup surface the Authenticator depends on.
// Production wires *cachelayer.Layer; *store.DB also satisfies it for
// degraded paths.
type VKLookup interface {
	GetVirtualKeyByHash(ctx context.Context, keyHash string) (*store.VirtualKey, error)
}

// Authenticator validates virtual keys from HTTP requests.
//
// Revocation latency (acknowledged design decision): VK records resolve
// through the cache layer's virtual-key cache, whose per-entry TTL defaults
// to 30s (cachelayer.Config.VKTTL). On a revoke/disable, the worst-case
// window during which a stale VK is still honoured is therefore ≤ VKTTL.
// In practice it is usually instant: the Hub pushes an InvalidateVirtualKeys
// event on any VK change, which evicts the cached entry immediately, and the
// status/enabled/expiry checks below re-run on every request against whatever
// the cache currently holds. The bounded TTL is the fail-safe for the rare
// case where the invalidation push is missed.
type Authenticator struct {
	db VKLookup
	// vkHashKeys are the virtual-key-domain HMAC sub-keys, one per HMAC keyring
	// version, CURRENT FIRST. Each is HKDF-derived from that
	// version's secret under ClassAPIKeyVirtualKey — distinct from the
	// admin-API-key domain key, so a leak/oracle in one domain can't forge the
	// other. [MUST MATCH] the Control Plane's auth.HashVirtualKey derivation for
	// the same version's secret. Admission tries each in order; VKs are NOT
	// lazy-migrated (this path is read-only) and are pruned by re-issue/expiry.
	vkHashKeys [][]byte
	logger     *slog.Logger
}

// NewAuthenticator creates a VK authenticator from the HMAC keyring.
// It derives a virtual-key sub-key per keyring version (current-first)
// so a VK hashed under any version still admits. The keyring is guaranteed
// non-nil by config.validate() + wiring (which hard-fail boot when no HMAC secret
// is configured), so no dev-fallback substitution is performed here.
func NewAuthenticator(lookup VKLookup, keyring *hmackeyring.Keyring, logger *slog.Logger) *Authenticator {
	entries := keyring.All()
	keys := make([][]byte, 0, len(entries))
	for _, e := range entries {
		sub := keyderive.DeriveSubkey(e.Secret, keyderive.ClassAPIKeyVirtualKey)
		keys = append(keys, sub[:])
	}
	return &Authenticator{
		db:         lookup,
		vkHashKeys: keys,
		logger:     logger,
	}
}

// Authenticate extracts and validates a virtual key from the request.
// Returns VKMeta on success, or a sentinel error on failure.
//
// The extractor inspects the ingress context (stamped by
// handler.ServeProxy) to unlock provider-conventional VK carriers on
// the matching native route (e.g. `x-api-key` on `/v1/messages`,
// `?key=` on the Gemini routes, `api-key` on Azure). Routes without
// an ingress context — /v1/chat/completions called outside handler
// tests, /v1/ai-guard/classify, etc. — fall back to the default
// OpenAI-compat carrier set (Authorization: Bearer, x-nexus-virtual-key).
func (a *Authenticator) Authenticate(ctx context.Context, r *http.Request) (*VKMeta, error) {
	raw := extractVKToken(ctx, r)
	if raw == "" {
		return nil, ErrMissing
	}

	vk, err := a.lookupVK(ctx, raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if vk == nil {
		return nil, ErrInvalid
	}

	if !vk.Enabled {
		return nil, ErrDisabled
	}
	if vk.ExpiresAt != nil && vk.ExpiresAt.Before(time.Now()) {
		return nil, ErrExpired
	}
	if vk.VKStatus != nil && *vk.VKStatus != "" && *vk.VKStatus != "active" {
		return nil, fmt.Errorf("%w: status %s", ErrDisabled, *vk.VKStatus)
	}

	meta := &VKMeta{
		ID:                          vk.ID,
		Name:                        vk.Name,
		AllowedModels:               vk.AllowedModels,
		RateLimitRpm:                vk.RateLimitRpm,
		CompareEndpointRateLimitRpm: vk.CompareEndpointRateLimitRpm,
		Fingerprint:                 traffic.ApiKeyFingerprint(raw),
		Class:                       classifyVKToken(raw),
	}
	if vk.OrganizationID != nil {
		meta.OrganizationID = *vk.OrganizationID
	}
	if vk.ProjectID != nil {
		meta.ProjectID = *vk.ProjectID
	}
	if vk.SourceApp != nil {
		meta.SourceApp = *vk.SourceApp
	}
	if vk.OwnerID != nil {
		meta.OwnerID = *vk.OwnerID
	}
	if vk.OrganizationName != nil {
		meta.OrganizationName = *vk.OrganizationName
	}
	if vk.OrganizationTimezone != nil {
		meta.OrganizationTimezone = *vk.OrganizationTimezone
	}
	if vk.ProjectName != nil {
		meta.ProjectName = *vk.ProjectName
	}
	if vk.UserDisplayName != nil {
		meta.UserDisplayName = *vk.UserDisplayName
	}
	if vk.VKType != nil {
		meta.VKType = *vk.VKType
	}
	if vk.VKStatus != nil {
		meta.VKStatus = *vk.VKStatus
	}
	return meta, nil
}

// extractVKToken extracts the VK identifier from request headers,
// honouring the ingress format's provider-conventional carriers.
//
// Priority order (first non-empty wins):
//  1. `x-nexus-virtual-key` — always honoured, all routes.
//  2. `Authorization: Bearer <token>` — always honoured, all routes.
//  3. Format-specific carriers, accepted only on the matching native
//     route:
//     - Anthropic (`/v1/messages`): `x-api-key: <vk>`.
//     - Gemini (`/v1beta/…`): `x-goog-api-key: <vk>` header. The Google
//     `?key=<vk>` URL-query carrier is NOT accepted (a bearer
//     credential must never travel in the URL, where logs/history/Referer
//     capture it).
//     - Azure OpenAI (`/openai/deployments/…`): `api-key: <vk>` header.
//     - MiniMax and GLM: no extra carrier (their SDKs speak the
//     standard `Authorization: Bearer` convention already covered by #2).
func extractVKToken(ctx context.Context, r *http.Request) string {
	if vk := r.Header.Get("x-nexus-virtual-key"); vk != "" {
		return strings.TrimSpace(vk)
	}
	auth := r.Header.Get("Authorization")
	if after, ok := strings.CutPrefix(auth, "Bearer "); ok {
		if v := strings.TrimSpace(after); v != "" {
			return v
		}
	}

	switch ingressFormatFromContext(ctx) {
	case provcore.FormatAnthropic:
		if vk := strings.TrimSpace(r.Header.Get("x-api-key")); vk != "" {
			return vk
		}
	case provcore.FormatGemini:
		if vk := strings.TrimSpace(r.Header.Get("x-goog-api-key")); vk != "" {
			return vk
		}
		// The Google `?key=<vk>` URL-query carrier is intentionally
		// NOT accepted. A virtual key is a long-lived bearer credential; in the
		// URL it leaks verbatim into every fronting-proxy / LB / CDN access log,
		// browser history, and Referer header (the gateway stores only HMAC(key),
		// so that logged copy is the only plaintext copy of a live credential).
		// Real Gemini SDKs send the `x-goog-api-key` header above; raw-URL REST
		// callers must use that header too.
	case provcore.FormatAzureOpenAI:
		if vk := strings.TrimSpace(r.Header.Get("api-key")); vk != "" {
			return vk
		}
	}
	return ""
}

// lookupVK resolves a VK token to a database record. Tokens that don't
// look like a real API key (no nvk_ prefix and length <= 20) are rejected
// outright — only the hashed-key path is supported.
func (a *Authenticator) lookupVK(ctx context.Context, token string) (*store.VirtualKey, error) {
	if !looksLikeRealKey(token) {
		return nil, ErrInvalid
	}
	// Try every keyring version, current-first. The
	// steady-state common case is a one-hash hit under the current version; older
	// versions are tried only for VKs not yet re-issued after a rotation. No lazy
	// migration on this read-only path.
	for _, k := range a.vkHashKeys {
		hash := hashKeyWith(k, token)
		vk, err := a.db.GetVirtualKeyByHash(ctx, hash)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				continue // not this version; try an older one
			}
			return nil, fmt.Errorf("vkauth: hash lookup: %w", err)
		}
		if vk != nil {
			return vk, nil
		}
	}
	return nil, ErrInvalid
}

// looksLikeRealKey returns true if the token appears to be a real API key.
func looksLikeRealKey(token string) bool {
	return strings.HasPrefix(token, "nvk_") || len(token) > 20
}

// classifyVKToken returns the api_key_class tag for a presented virtual key
// token. Gateway-issued keys are always prefixed "nvk_"; other shapes get
// the empty class. This label surfaces on traffic_event rows and lets
// analytics filter by caller-credential type without seeing the key itself.
func classifyVKToken(token string) string {
	if strings.HasPrefix(token, "nvk_") {
		return "nvk_"
	}
	return ""
}

// hashKeyWith computes HMAC-SHA256 of the key under a specific keyring version's
// virtual-key sub-key.
func hashKeyWith(subkey []byte, key string) string {
	mac := hmac.New(sha256.New, subkey)
	mac.Write([]byte(key))
	return hex.EncodeToString(mac.Sum(nil))
}
