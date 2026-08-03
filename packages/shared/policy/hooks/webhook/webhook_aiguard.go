package webhook

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
)

// rsTokenHeader is the internal-service auth header AI-Guard validates. It MUST
// stay equal to rstokenauth.Header ("X-RS-Token"); the literal is duplicated
// here on purpose so this broadly-imported hook package (pulled into every
// data-plane binary, agent included) does not drag the rstokenauth package's
// echo dependency into their closures.
const rsTokenHeader = "X-RS-Token"

// AIGuardComplianceWebhookPath is the fixed path of the AI-Gateway endpoint
// that lets a webhook-forward hook route captured traffic through AI-Guard
// (packages/ai-gateway .../classify.ServeComplianceWebhookHTTP, mounted at
// "POST /v1/ai-guard/compliance-webhook"). That endpoint is gated by the
// internal-service X-RS-Token; a webhook-forward hook only authenticates to
// it when its endpoint matches this path on a trusted AI-Gateway host — see
// Options.TrustedAIGuardBases.
const AIGuardComplianceWebhookPath = "/v1/ai-guard/compliance-webhook"

// Options carries service-supplied wiring for the webhook-forward hook. The
// data-plane services (ai-gateway, compliance-proxy) inject their shared HTTP
// client plus the internal-service credentials needed to authenticate to the
// AI-Gateway's AI-Guard compliance-webhook.
type Options struct {
	// Client is the shared HTTP client; nil creates a per-hook SSRF-guarded
	// client (blocks non-public addresses at dial time).
	Client *http.Client
	// InternalToken is the X-RS-Token injected on requests whose endpoint is
	// the trusted AI-Guard compliance-webhook (a trusted base + the fixed
	// compliance-webhook path). Sourced from the service's
	// INTERNAL_SERVICE_TOKEN env, never from admin hook config. Empty disables
	// injection — the hook then posts unauthenticated, as before.
	InternalToken string
	// TrustedAIGuardBases returns the scheme://host bases of the AI-Gateway
	// that serves the AI-Guard compliance-webhook this service may
	// authenticate to — typically its reported public AND private URLs (the
	// admin-configured hook endpoint uses the public host in fronted
	// deployments, while the private host covers direct-dial topologies).
	// Called at request time so a Hub-resolved base stays fresh without hook
	// reconstruction; implementations must be cheap (in-memory cached — e.g.
	// the ai-gateway returns its own static URLs, the compliance-proxy wraps
	// the peerurl resolver) and return nil when nothing is resolved yet —
	// which simply means no token this request (fail-safe), retried on the
	// next one. A token is injected ONLY when the hook endpoint's scheme+host
	// match one of the returned bases AND its path is
	// AIGuardComplianceWebhookPath, so the internal token is never leaked to
	// an arbitrary admin-typed webhook URL. Nil disables injection.
	TrustedAIGuardBases func(ctx context.Context) []string
}

// NewWebhookForwardWithOptions creates a webhook-forward hook with service-
// supplied wiring (shared client + AI-Guard internal-auth credentials). See
// Options. The credentials authenticate ONLY to the trusted AI-Guard
// compliance-webhook; every other endpoint is posted unauthenticated exactly
// as NewWebhookForward does.
func NewWebhookForwardWithOptions(cfg *core.HookConfig, opts Options) (core.Hook, error) {
	endpoint, _ := cfg.Config["endpoint"].(string)
	if endpoint == "" {
		return nil, fmt.Errorf("webhook-forward: endpoint is required")
	}
	client := opts.Client

	timeout := 5 * time.Second
	if ms, ok := cfg.Config["timeoutMs"].(float64); ok && ms > 0 {
		timeout = time.Duration(ms) * time.Millisecond
	}

	if client == nil {
		client = nexushttp.New(nexushttp.Config{
			Timeout:        timeout,
			Caller:         "webhook-hook",
			PropagateReqID: true,
			// The endpoint is an admin-configured URL the compliance
			// pipeline POSTs captured traffic to — external by nature and an
			// SSRF primitive. Block every non-public address at dial time
			// (loopback / RFC-1918 / link-local / metadata); the guard runs on
			// the resolved IP so it also defeats DNS-rebinding.
			DialControl: nexushttp.AdminEgressDialControl(nexushttp.AdminEgressExternalOnly),
		})
	}

	mode := WebhookPayloadRedacted
	if v, ok := cfg.Config["payloadMode"].(string); ok && v != "" {
		switch WebhookPayloadMode(v) {
		case WebhookPayloadFull, WebhookPayloadRedacted, WebhookPayloadMetadataOnly:
			mode = WebhookPayloadMode(v)
		default:
			return nil, fmt.Errorf("webhook-forward: unknown payloadMode %q (expected full|redacted|metadata-only)", v)
		}
	}

	onMatch, err := core.ParseOnMatch(cfg.Config)
	if err != nil {
		return nil, fmt.Errorf("webhook-forward: %w", err)
	}
	// webhook-forward is uniquely advisory: unlike pii-detector /
	// keyword-filter / content-safety (where "match → block by default" is
	// the right security default), the webhook's reply IS the decision.
	// If the admin did not configure an explicit `action`, treat the ceiling
	// as `approve` so the webhook's suggestion flows through without being
	// silently clobbered to block by ParseOnMatch's default. Admins who want
	// webhook-bounded-by-ceiling behavior must opt in via an explicit
	// `onMatch.action`.
	if !hasActionConfigured(cfg.Config) {
		onMatch.Action = core.ActionApprove
	}

	// The endpoint's shape is fixed at construction (admin config); whether a
	// trusted base matches is decided per request via opts.TrustedAIGuardBases
	// (the base may be Hub-resolved after this hook is built). Pre-parse the
	// endpoint once so the per-request work is a cheap host compare. A
	// malformed endpoint is admin data and simply never matches (no token).
	epScheme, epHost, epPathIsAIGuard := "", "", false
	if u, err := url.Parse(endpoint); err == nil {
		epScheme, epHost = u.Scheme, u.Host
		epPathIsAIGuard = strings.TrimRight(u.Path, "/") == AIGuardComplianceWebhookPath
	}

	return &WebhookForward{
		endpoint:              endpoint,
		timeout:               timeout,
		client:                client,
		payloadMode:           mode,
		onMatch:               onMatch,
		projectionOpts:        cfg.ProjectionOptions(),
		internalToken:         opts.InternalToken,
		trustedAIGuardBases:   opts.TrustedAIGuardBases,
		endpointScheme:        epScheme,
		endpointHost:          epHost,
		endpointPathIsAIGuard: epPathIsAIGuard,
	}, nil
}

// endpointMatchesTrustedBase reports whether the pre-parsed endpoint
// scheme+host equal trustedBase's. A malformed or empty base never matches.
// Default ports are normalized on both sides (https://h:443 ≡ https://h,
// http://h:80 ≡ http://h) — the base is a reported/derived value the operator
// no longer hand-matches to the endpoint, so an explicit default port must
// not silently break the trust match.
func endpointMatchesTrustedBase(epScheme, epHost, trustedBase string) bool {
	base, err := url.Parse(trustedBase)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return false
	}
	return strings.EqualFold(epScheme, base.Scheme) &&
		strings.EqualFold(normalizeHostPort(epScheme, epHost), normalizeHostPort(base.Scheme, base.Host))
}

// normalizeHostPort strips an explicit default port for the scheme.
func normalizeHostPort(scheme, host string) string {
	switch strings.ToLower(scheme) {
	case "https":
		return strings.TrimSuffix(host, ":443")
	case "http":
		return strings.TrimSuffix(host, ":80")
	}
	return host
}
