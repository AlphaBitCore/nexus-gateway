// Package server provides the ProxyServer type that handles incoming CONNECT
// requests, enforces access control, manages connection lifecycle, and
// delegates tunnel establishment + compliance forwarding to the connect/ and
// forward/ sub-packages respectively.
package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/access"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/compliance"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/exemption"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/metrics"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/proxy/conn"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/proxy/connect"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/proxy/forward"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/domain"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
	normalizecore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming"
	streampolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming/policy"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/tlsbump"
	"github.com/google/uuid"
)

// ProxyConfig holds all optional dependencies and tuning knobs for ProxyServer.
// Pass it to NewProxyServer to configure the server in one shot.
type ProxyConfig struct {
	// OnboardingEnabled activates the 407 intercept path. When true,
	// CONNECT requests for monitored domains return 407 + HTML guide instead
	// of 200. Toggle at runtime via ProxyServer.SetOnboardingEnabled.
	OnboardingEnabled     bool
	OnboardingCPUIBaseURL string
	Checker               *access.Checker
	ConnManager           *conn.Manager
	IdleTimeout           time.Duration // defaults to 300s if zero
	ShutdownCord          *conn.ShutdownCoordinator
	PinningTracker        *tlsbump.PinningTracker

	// Compliance kernel
	CompliancePipeline *compliance.PolicyResolver
	AuditEmitter       *compliance.AuditEmitter
	StreamingConfig    streaming.LiveConfig
	PerHookTimeout     time.Duration
	TotalTimeout       time.Duration
	ParallelHooks      bool
	// StreamingPolicyStore is the hot-swappable streaming compliance
	// policy Store. Per-host override columns on interception_domain
	// merge with Store.Get() at request time. Configdispatch handler
	// re-applies admin updates via Store.ApplyShadowState — no
	// SetStreamingPolicyGlobal wrapper needed (unified
	// three-service Store model).
	StreamingPolicyStore *streampolicy.Store

	// DomainEngine resolves per-path actions (PROCESS / PASSTHROUGH / BLOCK)
	// before invoking the compliance pipeline. When nil every bumped request
	// runs hooks unconditionally.
	DomainEngine *domain.Engine

	// AdapterRegistry maps interception_domain.adapter_id values to
	// AdapterFactory functions. The forward handler uses this to resolve
	// the per-provider adapter for DetectRequestMeta / ExtractRequest /
	// DetectResponseUsage / ExtractResponse on bumped requests.
	AdapterRegistry *traffic.AdapterRegistry

	// NormalizeRegistry — the Tier 1+2+3 normalize chain. Wired into
	// tlsbump via WithNormalizeRegistry so hookInput.Normalized comes
	// from the full fallback chain.
	NormalizeRegistry *normalizecore.Registry

	// Reject response configuration.
	RejectConfig tlsbump.RejectConfig

	// ExemptionStore holds temporarily exempted source/host pairs.
	ExemptionStore *exemption.Store

	// KillSwitchChecker returns false when TLS bump is disabled via the
	// kill switch. When nil the proxy always bumps.
	KillSwitchChecker func() bool

	// PayloadCaptureStore holds the runtime payload-capture configuration
	// (max body bytes + request/response storage flags). When nil the
	// forward handler falls back to payloadcapture.DefaultConfig (off,
	// 64 KiB cap). Admin edits arrive via the "payload_capture" shadow
	// reducer in cmd/compliance-proxy/main.go.
	PayloadCaptureStore *payloadcapture.Store

	// AllowUnlistedPassthrough downgrades the proxy to a transparent TCP
	// relay for CONNECTs whose target is not in the domain allowlist. When
	// false (default) such CONNECTs return 403, matching production
	// compliance-gate behavior.
	AllowUnlistedPassthrough bool

	// AttestationVerifier decides whether an incoming CONNECT carries a
	// verified X-Nexus-Attestation header. Nil disables the feature (no
	// header peek, no metric increment). On Outcome=Valid the CONNECT is
	// transparently tunneled (PassThrough), bypassing every gate below
	// (kill switch, pinning, hook pipeline). On any non-Valid outcome the
	// request flows through the normal MITM path — fail-open.
	AttestationVerifier *AttestationVerifier
}

// ProxyServer handles incoming CONNECT requests, enforces access control,
// manages connection lifecycle, establishes tunnels, and performs TLS interception.
type ProxyServer struct {
	upstream       *tlsbump.UpstreamTransport
	getCert        func(*tls.ClientHelloInfo) (*tls.Certificate, error)
	logger         *slog.Logger
	checker        *access.Checker
	connManager    *conn.Manager
	idleTimeout    time.Duration
	shutdownCord   *conn.ShutdownCoordinator
	pinningTracker *tlsbump.PinningTracker

	// Compliance kernel
	compliancePipeline *compliance.PolicyResolver
	auditEmitter       *compliance.AuditEmitter
	// streamingTuning is the hot-swappable bundle of streaming-mode tunables:
	// mode string + per-hook timeout + total timeout. Each CONNECT loads via
	// atomic.Pointer so the compliance_streaming shadow key takes effect on
	// the next bumped request without rebuilding the proxy server. Collapsed
	// into a snapshot type so the live pipeline reads a single coherent value
	// (no torn read where mode was updated but timeouts weren't).
	streamingTuning atomic.Pointer[streamingTuningSnapshot]
	streamingConfig streaming.LiveConfig
	parallelHooks   bool
	// streamingPolicyStore is the hot-swappable streaming compliance
	// policy Store. configdispatch's streaming_compliance
	// shadow handler calls Store.ApplyShadowState directly — no
	// per-server wrapper. Hot-path reads use Store.Get(), which is
	// lock-free via atomic.Pointer internally.
	streamingPolicyStore *streampolicy.Store

	// Reject response configuration.
	rejectConfig tlsbump.RejectConfig

	// exemptionStore holds temporarily exempted source/host pairs.
	exemptionStore *exemption.Store

	// killSwitchChecker returns false when TLS bump is disabled via the
	// kill switch. When nil the proxy always bumps.
	killSwitchChecker func() bool

	// payloadCaptureStore carries the runtime payload-capture knobs into
	// the forward handler. Nil is tolerated and behaves like the
	// conservative default (off, 64 KiB cap).
	payloadCaptureStore *payloadcapture.Store

	// allowUnlistedPassthrough — see ProxyConfig.AllowUnlistedPassthrough.
	allowUnlistedPassthrough bool

	// domainEngine — see ProxyConfig.DomainEngine.
	domainEngine *domain.Engine

	// normalizeRegistry — see ProxyConfig.NormalizeRegistry.
	normalizeRegistry *normalizecore.Registry

	// adapterRegistry — see ProxyConfig.AdapterRegistry.
	adapterRegistry *traffic.AdapterRegistry

	// onboardingEnabled gates the 407 intercept path. Hot-swappable
	// via SetOnboardingEnabled without locking the hot CONNECT path.
	onboardingEnabled     atomic.Bool
	onboardingCPUIBaseURL string

	// attestationVerifier — see ProxyConfig.AttestationVerifier.
	attestationVerifier *AttestationVerifier
}

// NewProxyServer creates a new proxy server from the given config, upstream
// transport, certificate callback, and logger. IdleTimeout defaults to 300s
// when zero. StreamingMode is normalized to lowercase at construction time.
func NewProxyServer(
	cfg ProxyConfig,
	upstream *tlsbump.UpstreamTransport,
	getCert func(*tls.ClientHelloInfo) (*tls.Certificate, error),
	logger *slog.Logger,
) *ProxyServer {
	idle := cfg.IdleTimeout
	if idle == 0 {
		idle = 300 * time.Second
	}
	// Endpoint classification moved inside the
	// forward path — it now consults typology.ClassifyPath directly,
	// removing the per-proxy classifier injection seam.
	ps := &ProxyServer{
		upstream:                 upstream,
		getCert:                  getCert,
		logger:                   logger,
		checker:                  cfg.Checker,
		connManager:              cfg.ConnManager,
		idleTimeout:              idle,
		shutdownCord:             cfg.ShutdownCord,
		pinningTracker:           cfg.PinningTracker,
		compliancePipeline:       cfg.CompliancePipeline,
		auditEmitter:             cfg.AuditEmitter,
		streamingConfig:          cfg.StreamingConfig,
		parallelHooks:            cfg.ParallelHooks,
		rejectConfig:             cfg.RejectConfig,
		exemptionStore:           cfg.ExemptionStore,
		killSwitchChecker:        cfg.KillSwitchChecker,
		payloadCaptureStore:      cfg.PayloadCaptureStore,
		allowUnlistedPassthrough: cfg.AllowUnlistedPassthrough,
		domainEngine:             cfg.DomainEngine,
		adapterRegistry:          cfg.AdapterRegistry,
		normalizeRegistry:        cfg.NormalizeRegistry,
		onboardingCPUIBaseURL:    cfg.OnboardingCPUIBaseURL,
		attestationVerifier:      cfg.AttestationVerifier,
	}
	ps.onboardingEnabled.Store(cfg.OnboardingEnabled)
	ps.streamingTuning.Store(&streamingTuningSnapshot{
		PerHookTimeout: cfg.PerHookTimeout,
		TotalTimeout:   cfg.TotalTimeout,
	})
	// Hold the *Store directly — Hub shadow pushes call
	// store.ApplyShadowState on this exact instance, so the hot path
	// reads via Get() always see the latest admin policy. No
	// SetStreamingPolicyGlobal wrapper needed.
	ps.streamingPolicyStore = cfg.StreamingPolicyStore
	return ps
}

// streamingTuningSnapshot is the atomic-pointer payload that captures
// the hot-swappable streaming-timeout tunables in one coherent value.
// (The mode field was removed — admin streaming policy is now
// the single source of truth, read via streamingPolicyStore.)
type streamingTuningSnapshot struct {
	PerHookTimeout time.Duration // per-hook budget; zero falls back to the package default
	TotalTimeout   time.Duration // whole-request budget across all hooks
}

// SetStreamingTuning atomically replaces the streaming timeout tunables.
// Called by the compliance-proxy thingclient.OnConfigChanged callback
// when Hub pushes the compliance_streaming shadow key. Zero fields are
// ignored — the previous value stays put — so a partial payload
// updates just the supplied knobs.
func (p *ProxyServer) SetStreamingTuning(perHookTimeout, totalTimeout time.Duration) {
	cur := p.streamingTuning.Load()
	next := *cur
	if perHookTimeout > 0 {
		next.PerHookTimeout = perHookTimeout
	}
	if totalTimeout > 0 {
		next.TotalTimeout = totalTimeout
	}
	p.streamingTuning.Store(&next)
}

// SetOnboardingEnabled toggles onboarding mode at runtime. Safe to call
// concurrently — the underlying atomic.Bool provides the memory ordering.
func (p *ProxyServer) SetOnboardingEnabled(enabled bool) {
	p.onboardingEnabled.Store(enabled)
}

// onboardingHTML is the 407 response body rendered when onboarding mode is
// active. {{.SetupURL}} is replaced with the CP-UI setup page link.
var onboardingHTMLTmpl = template.Must(template.New("onboarding").Parse(`<!DOCTYPE html>
<html lang="en">
<head><meta charset="UTF-8"><title>Nexus Gateway — Setup Required</title>
<style>body{font-family:sans-serif;max-width:600px;margin:60px auto;color:#333}
h1{color:#1a56db}a{color:#1a56db}</style></head>
<body>
<h1>Nexus Gateway Setup Required</h1>
<p>Your device needs the Nexus Proxy CA certificate installed before AI traffic
can be inspected. Without it, HTTPS connections to AI providers will fail.</p>
<p><a href="{{.SetupURL}}">Open the Setup Guide →</a></p>
<p style="color:#666;font-size:.9em">If you have already installed the certificate,
ask your IT administrator to disable onboarding mode in the Control Plane.</p>
</body></html>
`))

// ServeHTTP handles incoming HTTP requests. Only the CONNECT method is accepted;
// all other methods return 405 Method Not Allowed. HTTP/2 CONNECT requests
// (r.ProtoMajor >= 2) return 505 HTTP Version Not Supported.
func (p *ProxyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Only CONNECT is supported
	if r.Method != http.MethodConnect {
		http.Error(w, "only CONNECT method is supported", http.StatusMethodNotAllowed)
		return
	}

	// Reject HTTP/2 CONNECT (RFC 9113 §8.5 — not supported).
	if r.ProtoMajor >= 2 {
		p.logger.Warn("HTTP/2 CONNECT not supported",
			"source", r.RemoteAddr,
			"target", r.Host,
			"proto", r.Proto,
		)
		if metrics.ConnectionsTotal != nil {
			metrics.ConnectionsTotal.With("rejected_h2_connect").Inc()
		}
		http.Error(w, "HTTP/2 CONNECT is not supported; use HTTP/1.1 CONNECT", http.StatusHTTPVersionNotSupported)
		return
	}

	// Onboarding intercept — fires before 200 Connection Established so the
	// browser sees the HTML guide regardless of CA trust state. Only
	// monitored domains (those in the domain allowlist) are intercepted;
	// unlisted domains pass through unaffected.
	if p.onboardingEnabled.Load() && p.checker != nil {
		host, _, _ := net.SplitHostPort(r.Host)
		if host == "" {
			host = r.Host
		}
		sourceIP, _, _ := net.SplitHostPort(r.RemoteAddr)
		ip := net.ParseIP(sourceIP)
		if ip == nil {
			ip = net.ParseIP(r.RemoteAddr)
		}
		if err := p.checker.CheckConnect(r.Context(), ip, host, "443"); err == nil {
			// Domain is monitored — intercept with onboarding guide.
			setupURL := p.onboardingCPUIBaseURL + "/setup/proxy"
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Proxy-Authenticate", `Basic realm="Nexus Gateway setup required — visit `+setupURL+`"`)
			w.WriteHeader(http.StatusProxyAuthRequired)
			_ = onboardingHTMLTmpl.Execute(w, struct{ SetupURL string }{SetupURL: setupURL})
			p.logger.Info("onboarding intercept",
				"source", r.RemoteAddr,
				"target", r.Host,
				"setup_url", setupURL,
			)
			return
		}
	}

	// Seed the nexus request id into the request context so any outbound
	// httpclient call made while processing this CONNECT can log (and, when
	// opted in, propagate) it. Honor an id supplied by an upstream Nexus
	// service (ai-gateway / agent); mint a UUID only when none was supplied
	// so every CONNECT carries a correlation id.
	reqID := r.Header.Get("X-Nexus-Request-Id")
	if reqID == "" {
		reqID = uuid.New().String()
		r.Header.Set("X-Nexus-Request-Id", reqID)
	}
	r = r.WithContext(nexushttp.WithRequestID(r.Context(), reqID))

	// Parse host and port. Default to port 443 if not specified.
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
		port = "443"
	}
	targetHost := net.JoinHostPort(host, port)
	sourceAddr := r.RemoteAddr

	// Stamp the canonical trace_id key onto the connection-scoped logger.
	// The shared SlogSink lifts this attr into DiagEvent.TraceID so every
	// thing_diag_event row emitted during this CONNECT carries the typed
	// trace correlation column. Empty when upstream didn't supply a
	// header — left as "" so the typed column lands NULL (the Hub writer
	// pointer-indirects "" → NULL).
	connLogger := p.logger.With(
		"source", sourceAddr,
		"target", targetHost,
		"trace_id", reqID,
	)

	// Access control checks
	if p.checker != nil {
		sourceIP, _, _ := net.SplitHostPort(sourceAddr)
		ip := net.ParseIP(sourceIP)
		if ip == nil {
			ip = net.ParseIP(sourceAddr)
		}

		if err := p.checker.CheckConnect(r.Context(), ip, host, port); err != nil {
			// Unlisted-passthrough mode (DEV deploy posture): when the only
			// reason to reject is "destination not in domain allowlist",
			// downgrade to a transparent TCP relay instead of 403. Other
			// gates (IP allowlist, private IP) still reject — they are
			// security checks, not allowlist misses, and must remain
			// enforced even in this mode.
			if p.allowUnlistedPassthrough && errors.Is(err, access.ErrDomainDenied) {
				p.serveUnlistedPassthrough(w, r, targetHost, connLogger)
				return
			}

			reason := categorizeAccessError(err)
			if metrics.ConnectionsTotal != nil {
				metrics.ConnectionsTotal.With(reason).Inc()
			}
			connLogger.Warn("CONNECT rejected by access control",
				"reason", reason,
				"error", err,
			)
			http.Error(w, fmt.Sprintf("connection denied: %s", reason), http.StatusForbidden)
			return
		}
	}

	// Connection limit check
	if p.connManager != nil {
		sourceIP, _, _ := net.SplitHostPort(sourceAddr)
		connID, err := p.connManager.AcquireWithInfo(sourceIP, targetHost)
		if err != nil {
			connLogger.Warn("CONNECT rejected: at capacity")
			http.Error(w, "service at capacity", http.StatusServiceUnavailable)
			return
		}
		defer p.connManager.Release(connID)
		connLogger = connLogger.With("connectionId", connID)
	}

	// Track for graceful shutdown
	if p.shutdownCord != nil {
		p.shutdownCord.TrackConnection()
		defer p.shutdownCord.UntrackConnection()
	}

	connStart := time.Now()

	// Connection-stage hook pipeline. Evaluated after access control and the
	// connection-limit gate but before tunnel establishment. Errors building
	// the pipeline fail open so an infrastructure glitch in the policy layer
	// cannot take down every CONNECT. A RejectHard decision yields 403.
	//
	// CONNECT predates the TLS handshake, so only target host is available
	// here; SNI and client certificate fingerprints are observed later by
	// the TLS-bump path and re-evaluated at the request stage.
	if p.compliancePipeline != nil {
		// Connection-stage: endpoint type is not yet known (CONNECT predates
		// TLS handshake). Pass "" and nil modalities so all hooks that
		// SupportsEndpoint("") continue to run, preserving existing behavior.
		pipe, pipeErr := p.compliancePipeline.BuildPipeline(
			"connection",
			"COMPLIANCE_PROXY",
			"", nil, // endpoint/modality unknown at CONNECT time
			5*time.Second,
			30*time.Second,
			false, // connection-stage pipeline is sequential
			true,  // strictFailClosed: the compliance-proxy appliance is a DEDICATED forward proxy (already 403s disallowed CONNECTs), NOT the host outbound packet path — an unbuildable fail-closed hook MUST refuse the connection, never forward uninspected. (The agent NE host path stays fail-open via WithStrictFailClosed-unset.)
			p.logger,
		)
		if pipeErr != nil {
			// The connection-stage build above passes strictFailClosed=true,
			// so an unbuildable fail-closed hook MUST refuse the CONNECT rather than
			// establish an uninspected tunnel. (The agent NE host-packet path never
			// runs this appliance connection stage.)
			// Distinct metric label + non-policy body: this refusal is an
			// INFRASTRUCTURE failure (a mandatory hook cannot be built — bad
			// rule config / unknown implementationId), not a policy decision.
			// Labeling it rejected_policy would send an admin triaging a
			// CONNECT-403 storm to the policy rules instead of the hook config.
			if metrics.ConnectionsTotal != nil {
				metrics.ConnectionsTotal.With("rejected_build_failed").Inc()
			}
			connLogger.Warn("connection-stage pipeline build failed; refusing CONNECT (fail-closed)", "error", pipeErr)
			http.Error(w, "compliance pipeline unavailable", http.StatusForbidden)
			return
		} else if pipe != nil {
			sourceIP, _, _ := net.SplitHostPort(sourceAddr)
			input := &core.HookInput{
				RequestID:   r.Header.Get("X-Nexus-Request-Id"),
				Stage:       "connection",
				SourceIP:    sourceIP,
				TargetHost:  host,
				Method:      r.Method,
				IngressType: "COMPLIANCE_PROXY",
			}
			res := pipe.Execute(r.Context(), input)
			if res != nil && res.Decision == core.RejectHard {
				reason := res.Reason
				if reason == "" {
					reason = "connection blocked by compliance policy"
				}
				if metrics.ConnectionsTotal != nil {
					metrics.ConnectionsTotal.With("rejected_policy").Inc()
				}
				connLogger.Warn("CONNECT rejected by connection-stage hook", "reason", reason)
				http.Error(w, reason, http.StatusForbidden)
				return
			}
		}
	}

	// Emit the accepted counter + active gauge AFTER the connection-stage
	// gate so the metric reflects the final admission decision. Ordering
	// matters: rejects above increment "rejected_policy" instead.
	if metrics.ConnectionsTotal != nil {
		metrics.ConnectionsTotal.With("accepted").Inc()
	}
	if p.connManager == nil && metrics.ConnectionsActive != nil {
		// Only track via Inc/Dec when there is no conn manager.
		// When connManager is set, it drives ConnectionsActive via Set().
		metrics.ConnectionsActive.With().Inc()
		defer metrics.ConnectionsActive.With().Dec()
	}

	connLogger.Info("CONNECT accepted")

	// Establish the tunnel by hijacking the connection
	rawConn, err := connect.EstablishTunnel(w, r)
	if err != nil {
		connLogger.Error("failed to establish tunnel", "error", err)
		return
	}

	// Wrap with idle timeout
	var tunnelConn = rawConn
	if p.idleTimeout > 0 {
		tunnelConn = conn.NewIdleConn(rawConn, p.idleTimeout)
	}
	defer func() {
		_ = tunnelConn.Close()
	}()

	// Build the ConnID for source-info stamping in the forward path.
	connID := ""
	if p.connManager != nil {
		connID = fmt.Sprintf("%s->%s", sourceAddr, targetHost)
	}

	// Load the streaming tuning snapshot once per CONNECT so the compliance
	// pipeline sees a coherent value even if SetStreamingTuning fires mid-request.
	// Guard against nil: ProxyServer may be constructed directly in tests without
	// calling NewProxyServer, so the atomic pointer may be uninitialized.
	var st streamingTuningSnapshot
	if snap := p.streamingTuning.Load(); snap != nil {
		st = *snap
	}
	// streamingPolicyStore is wired in via ProxyConfig.StreamingPolicyStore;
	// forward.Run hands the *Store off to tlsbump.WithStreamingPolicyStore
	// which resolves the live snapshot at SSE-handler entry. Nil-safe:
	// tlsbump's resolveStreamingMode falls back to passthrough when the
	// Store pointer is nil.

	// Delegate compliance forwarding (kill-switch / pinning / exemption /
	// bump) to the forward sub-package.
	// Build the verifier closure for the inner-request peek inside tlsbump's
	// forward handler. Nil verifier → option not installed → no per-request
	// peek cost. The closure adapter avoids leaking AttestationVerifier into
	// the tlsbump shared package.
	var attestationVerifierFn tlsbump.AttestationVerifierFunc
	if p.attestationVerifier != nil && p.attestationVerifier.Enabled() {
		v := p.attestationVerifier
		attestationVerifierFn = func(ctx context.Context, headerValue string) (bool, string) {
			res := v.Verify(ctx, headerValue)
			return res.Outcome == AttestationOutcomeValid, res.AgentID
		}
	}

	forward.Run(r.Context(), tunnelConn, forward.Config{
		SourceAddr:           sourceAddr,
		TargetHost:           targetHost,
		Host:                 host,
		ConnID:               connID,
		ConnStart:            connStart,
		KillSwitchChecker:    p.killSwitchChecker,
		GetCert:              p.getCert,
		Upstream:             p.upstream,
		PinningTracker:       p.pinningTracker,
		CompliancePipeline:   p.compliancePipeline,
		AuditEmitter:         p.auditEmitter,
		StreamingTuning:      forward.StreamingTuning{PerHookTimeout: st.PerHookTimeout, TotalTimeout: st.TotalTimeout},
		StreamingConfig:      p.streamingConfig,
		ParallelHooks:        p.parallelHooks,
		StreamingPolicyStore: p.streamingPolicyStore,
		ExemptionStore:       p.exemptionStore,
		RejectConfig:         p.rejectConfig,
		PayloadCaptureStore:  p.payloadCaptureStore,
		DomainEngine:         p.domainEngine,
		AdapterRegistry:      p.adapterRegistry,
		NormalizeRegistry:    p.normalizeRegistry,
		AttestationVerifier:  attestationVerifierFn,
		Logger:               connLogger,
	})
}

// serveUnlistedPassthrough handles a CONNECT whose target is outside the
// domain allowlist when AllowUnlistedPassthrough is enabled. It hijacks the
// connection, sends "200 Connection Established", and bidirectionally relays
// raw TCP via tlsbump.PassThrough. No MITM, no audit, no compliance hooks — by design,
// since the target was opted out by configuration.
//
// Connection-manager / shutdown-coordinator tracking is intentionally skipped
// on this branch: the flag is meant for dev posture, and production
// deployments keep it off, so the accounting gap never materializes.
func (p *ProxyServer) serveUnlistedPassthrough(
	w http.ResponseWriter,
	r *http.Request,
	targetHost string,
	connLogger *slog.Logger,
) {
	if metrics.ConnectionsTotal != nil {
		metrics.ConnectionsTotal.With("unlisted_passthrough").Inc()
	}
	// DEBUG, not INFO: a single browser-using-this-proxy host produces tens
	// of CONNECTs per minute (OCSP, telemetry, CDNs, etc.), and we already
	// expose the count via tunnels.total{result="unlisted_passthrough"} for
	// observability. We deliberately do NOT emit a traffic_event — the
	// payload is never decrypted, so there is nothing meaningful to audit.
	connLogger.Debug("unlisted CONNECT passthrough (compliance gate downgraded by config)")

	rawConn, err := connect.EstablishTunnel(w, r)
	if err != nil {
		connLogger.Error("failed to establish unlisted passthrough tunnel", "error", err)
		return
	}

	tunnelConn := rawConn
	if p.idleTimeout > 0 {
		tunnelConn = conn.NewIdleConn(rawConn, p.idleTimeout)
	}
	defer func() {
		_ = tunnelConn.Close()
	}()

	forward.LogRelayResult(connLogger, "unlisted passthrough", tlsbump.PassThrough(r.Context(), tunnelConn, targetHost))
}

// categorizeAccessError maps access control errors to metric label values.
func categorizeAccessError(err error) string {
	switch {
	case errors.Is(err, access.ErrIPDenied):
		return "rejected_ip"
	case errors.Is(err, access.ErrDomainDenied):
		return "rejected_domain"
	case errors.Is(err, access.ErrPrivateIP):
		return "rejected_private_ip"
	default:
		return "rejected_unknown"
	}
}

// Start starts the proxy server on the given address and blocks until the
// context is cancelled, at which point it performs a graceful shutdown.
func Start(ctx context.Context, addr string, handler http.Handler) error {
	server := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("proxy server listen: %w", err)
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), defaultShutdownGrace)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("proxy server shutdown: %w", err)
		}
		return nil
	case err := <-errCh:
		return err
	}
}

const defaultShutdownGrace = 30 * time.Second

// RejectConfig exposes the reject-body verbosity this server was wired with.
// Read-only; pins the yaml-to-server threading in wiring tests — the zero
// value silently downgrades every refusal body to the stealth "Forbidden",
// turning the configured level into a dead knob.
func (p *ProxyServer) RejectConfig() tlsbump.RejectConfig { return p.rejectConfig }
