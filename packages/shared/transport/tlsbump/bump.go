package tlsbump

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"sync"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/domain"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	compliance "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming"
	streampolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming/policy"

	"golang.org/x/net/http2"
)

// BumpConnection performs TLS interception on a hijacked CONNECT tunnel.
// It terminates TLS toward the client using a dynamically issued certificate
// (obtained via getCert), negotiates HTTP/2 or HTTP/1.1 via ALPN, and forwards
// each request to the upstream through the provided UpstreamTransport.
//
// The raw TLS ClientHello bytes are captured before the handshake and threaded
// into each request context so UpstreamTransport can replay the client's exact
// TLS fingerprint on the upstream dial (Plan B fingerprint passthrough).
//
// This function blocks until the client connection is closed or an error occurs.
// The caller is responsible for closing clientConn after this function returns.
func BumpConnection(
	ctx context.Context,
	clientConn net.Conn,
	targetHost string,
	getCert func(hello *tls.ClientHelloInfo) (*tls.Certificate, error),
	upstream *UpstreamTransport,
	logger *slog.Logger,
	opts ...BumpOption,
) error {
	// Wrap clientConn to capture the raw ClientHello before tls.Server reads it.
	capture := &helloCapture{Conn: clientConn}

	tlsConfig := &tls.Config{
		GetCertificate: getCert,
		NextProtos:     []string{"h2", "http/1.1"},
		MinVersion:     tls.VersionTLS12,
	}

	tlsConn := tls.Server(capture, tlsConfig)

	// Perform the TLS handshake with a timeout derived from ctx or a default.
	handshakeStart := time.Now()
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		handshakeMs := float64(time.Since(handshakeStart).Milliseconds())
		if TLSHandshakeMs != nil {
			TLSHandshakeMs.With().Observe(handshakeMs)
		}
		logger.Warn("TLS handshake failed",
			"target", targetHost,
			"error", err,
			"duration_ms", handshakeMs,
		)
		return fmt.Errorf("TLS handshake with client for %s: %w", targetHost, err)
	}
	handshakeMs := float64(time.Since(handshakeStart).Milliseconds())
	if TLSHandshakeMs != nil {
		TLSHandshakeMs.With().Observe(handshakeMs)
	}
	handshakeMsInt := int(handshakeMs) // stash for first forward_handler request

	negotiatedProto := tlsConn.ConnectionState().NegotiatedProtocol
	logger.Debug("TLS handshake completed",
		"target", targetHost,
		"protocol", negotiatedProto,
		"handshake_ms", handshakeMs,
	)

	// Merge bump options (compliance deps injected by ProxyServer).
	var bo bumpOptions
	for _, o := range opts {
		o(&bo)
	}
	// Store the captured ClientHello bytes so the forward handler can thread
	// them into each request context for UpstreamTransport.DialTLSContext.
	bo.clientHelloRaw = capture.buf
	// Stash the handshake duration so forward_handler stamps it on the
	// first request's traffic_event row (subsequent keep-alive requests
	// on the same bumped tunnel skip the stamp via tlsHandshakeOnce).
	bo.tlsHandshakeMs = handshakeMsInt

	// Build the handler that forwards each request to the upstream.
	handler := buildForwardHandler(ctx, targetHost, upstream, logger, &bo)

	if negotiatedProto == "h2" {
		return serveHTTP2(tlsConn, handler, logger)
	}
	return serveHTTP1(tlsConn, handler, logger)
}

// bumpOptions holds optional compliance dependencies injected into the bump handler.
type bumpOptions struct {
	policyResolver      *compliance.PolicyResolver
	auditEmitter        *compliance.AuditEmitter
	streamingConfig     streaming.LiveConfig
	perHookTimeout      time.Duration
	totalTimeout        time.Duration
	parallelHooks       bool
	sourceIP            string
	connectionID        string
	rejectConfig        RejectConfig
	payloadCaptureStore *payloadcapture.Store
	// e31-s13 path-policy engine. Forward handler consults this to
	// resolve PROCESS / PASSTHROUGH / BLOCK before invoking the
	// compliance pipeline. Nil falls back to existing behavior
	// (process every bumped request).
	domainEngine *domain.Engine
	// adapterRegistry maps adapter IDs (from interception_domain.adapter_id) to
	// factories. Used by forward_handler to resolve the traffic adapter for a
	// matched domain so DetectRequestMeta / ExtractRequest / DetectResponseUsage
	// / ExtractResponse are called with the correct per-provider logic.
	adapterRegistry *traffic.AdapterRegistry
	// normalizeRegistry is the Tier 1+2+3 normalize chain —
	// runtimeNormalize's only wire-format decode route. Non-standard
	// wire formats (chatgpt-web SSE delta_encoding, etc.) fall through
	// to the Tier 2 pattern probe and Tier 3 verbatim http-text. Nil
	// skips structured decoding entirely; only the adapter's segment
	// extraction runs.
	normalizeRegistry *normalize.Registry
	// streamingPolicyStore is the hot-swappable streaming compliance
	// policy Store, populated at startup (shared streampolicy.BootStore
	// helper) and refreshed via the configdispatch shadow handler that
	// calls ApplyShadowState on every push of the
	// streaming_compliance shadow key. Three-service alignment:
	// agent, compliance-proxy, and ai-gateway all hold a *Store the
	// same way; tlsbump reads the live snapshot via Store.Get() at
	// SSE-handler entry so per-flow mode dispatch sees the latest
	// admin policy without re-injecting it per-request.
	//
	// Nil signals "no Store wired by the caller" — handleSSEResponse
	// then falls back to passthrough (the conservative default that
	// avoids silent admin-policy drop).
	streamingPolicyStore *streampolicy.Store
	// clientHelloRaw holds the raw TLS ClientHello bytes captured before
	// tls.Server processes the handshake. Threaded into each request
	// context so UpstreamTransport.DialTLSContext can replay the client's
	// exact fingerprint via uTLS (Plan B fingerprint passthrough).
	clientHelloRaw []byte
	// tlsHandshakeMs is the server-side TLS-bump handshake duration
	// captured once when BumpConnection completes the handshake. Consumed
	// by the FIRST forward_handler request on this tunnel via
	// tlsHandshakeOnce, then zeroed so subsequent keep-alive requests
	// on the same bumped session don't double-count the handshake on
	// their traffic_event rows.
	tlsHandshakeMs   int
	tlsHandshakeOnce sync.Once

	// attestationVerifier — when non-nil, the forward handler peeks
	// every inner HTTPS request for the X-Nexus-Attestation header
	// BEFORE invoking the compliance pipeline. A valid signature
	// short-circuits the rest of the bumped flow: skip request +
	// response hook pipelines, skip payload capture, skip audit
	// emission entirely — pure passthrough. The Prometheus counter
	// still increments (operational signal). CP wires this via
	// WithAttestationVerifier; agent does not.
	attestationVerifier AttestationVerifierFunc

	// identity is the via-name stamped on x-nexus-via and x-nexus-cp-* /
	// x-nexus-agent-* response headers. Caller-supplied via WithIdentity:
	// "compliance-proxy" for cp listener, "agent" for the macOS agent
	// bridge. Defaults to "compliance-proxy" when unset; call sites
	// should always pass it explicitly.
	identity string

	// Process attribution. Only the agent populates these
	// (NEAppProxyFlow.metaData); cp leaves them empty. Forwarded into
	// AuditInfo so audit_event.source_process / source_user populate
	// for inspect-path rows where cp's emitter would otherwise not see
	// the originating process.
	procName   string
	procBundle string
	procUser   string

	// richReject selects how a block decision is returned to the client.
	// When true (the compliance-proxy / ai-gateway, which own a
	// client-facing HTTP response), a block writes the rich attributed
	// reject body via WriteRejectResponse (blocked_by_policy envelope +
	// rule-ID reason). When false (the zero value — the agent, an on-host
	// transparent interceptor that must not synthesize a rich error page),
	// a block writes a minimal "Forbidden" 403 with no attribution body.
	// Set via WithRichReject; defaults false so the agent's safe minimal
	// behavior is the unconfigured default.
	richReject bool

	// strictFailClosed selects the BUILD-time treatment of an unbuildable
	// fail-closed compliance hook. When false (default — the agent
	// NE proxy, which sits in the host outbound packet path), a fail-closed hook
	// that cannot be built is silently skipped so refusing never bricks host
	// networking (DNS/DHCP/etc). When true (the compliance-proxy appliance, a
	// dedicated forward proxy that already 403s disallowed CONNECTs and can
	// safely refuse), BuildPipeline errors on an unbuildable fail-closed hook so
	// the request is REFUSED rather than forwarded uninspected — honouring the
	// admin's "this scanner is mandatory" intent on build errors, not just
	// runtime errors. Set via WithStrictFailClosed; threaded to
	// PolicyResolver.BuildPipeline.
	strictFailClosed bool
}

// AttestationVerifierFunc verifies an inbound X-Nexus-Attestation
// header on the inner HTTP request. Returns true + the attesting
// agent id on success; false + empty id on any failure path
// (missing / invalid / replayed / expired / unknown agent / cache
// miss). The function MUST be fail-open — never abort the request.
//
// Defined as a function-typed shape in tlsbump (rather than an
// interface) so the package stays import-direction-safe: the
// compliance-proxy/internal/proxy/server package implements the
// concrete AttestationVerifier and wires its Verify method as a
// closure to WithAttestationVerifier. tlsbump never imports
// compliance-proxy.
type AttestationVerifierFunc func(ctx context.Context, headerValue string) (valid bool, agentID string)

// WithAttestationVerifier installs the attestation verifier
// the forward handler calls per-request. When the verifier reports
// valid, the bumped request is pure-passthrough: hook pipelines
// skipped, payload capture skipped, audit emission skipped. CP
// wires this; agent passes nothing.
func WithAttestationVerifier(fn AttestationVerifierFunc) BumpOption {
	return func(o *bumpOptions) { o.attestationVerifier = fn }
}

// BumpOption configures optional behavior for BumpConnection.
type BumpOption func(*bumpOptions)

// WithCompliance injects compliance pipeline dependencies. The streaming
// mode is sourced exclusively from the *streampolicy.Store wired via
// WithStreamingPolicyStore — there is no YAML / per-call mode parameter
// anymore (admin policy is the single source of truth for SSE mode,
// resolved through the shared Store across all three services).
func WithCompliance(
	resolver *compliance.PolicyResolver,
	emitter *compliance.AuditEmitter,
	streamingConfig streaming.LiveConfig,
	perHookTimeout, totalTimeout time.Duration,
	parallelHooks bool,
) BumpOption {
	return func(o *bumpOptions) {
		o.policyResolver = resolver
		o.auditEmitter = emitter
		o.streamingConfig = streamingConfig
		o.perHookTimeout = perHookTimeout
		o.totalTimeout = totalTimeout
		o.parallelHooks = parallelHooks
	}
}

// WithStreamingPolicyStore injects the hot-swappable streaming
// compliance policy Store. The SSE handler reads Store.Get() at flow
// entry to resolve the effective mode (per-host overrides merged via
// streampolicy.Resolve), so a Hub shadow push of the
// streaming_compliance key takes effect on the next request without
// rebuilding bumpOptions. Three-service alignment — agent /
// compliance-proxy / ai-gateway all wire the same *Store via this
// option.
//
// Nil store = handleSSEResponse falls back to passthrough (the
// conservative default that avoids silent admin-policy drop).
func WithStreamingPolicyStore(s *streampolicy.Store) BumpOption {
	return func(o *bumpOptions) { o.streamingPolicyStore = s }
}

// WithRejectConfig injects the reject response configuration.
func WithRejectConfig(cfg RejectConfig) BumpOption {
	return func(o *bumpOptions) {
		o.rejectConfig = cfg
	}
}

// WithIdentity sets the via-name stamped on x-nexus-via and x-nexus-cp-*
// response markers. cp callers pass "compliance-proxy"; the macOS agent
// bridge passes "agent". When unset the marker hook falls back to
// "compliance-proxy" for back-compat with cp's older behaviour.
func WithIdentity(name string) BumpOption {
	return func(o *bumpOptions) { o.identity = name }
}

// WithStrictFailClosed makes the bumped flow treat an UNBUILDABLE fail-closed
// compliance hook as a hard refusal at pipeline-build time instead
// of silently skipping it. Only callers that can safely refuse traffic should
// set this — the compliance-proxy appliance (a dedicated forward proxy that
// already returns 403 for disallowed CONNECTs). The agent NE proxy MUST leave
// it unset: it is in the host outbound packet path where refusing would brick
// host networking.
func WithStrictFailClosed() BumpOption {
	return func(o *bumpOptions) { o.strictFailClosed = true }
}

// WithRichReject selects how a block decision is returned to the client.
// Pass true on callers that own a client-facing HTTP response and may
// safely synthesize an attributed reject body — the compliance-proxy /
// ai-gateway appliances, whose clients are API/browser callers expecting
// a structured 403. Pass false (or leave unset) on the agent's on-host
// transparent interceptor, which must not synthesize a rich error page in
// the host's outbound path: a block then writes a minimal "Forbidden" 403
// with no attribution body. The reason carried into the rich body is the
// hook's rule-ID/label only — never the upstream's original sensitive
// value.
func WithRichReject(v bool) BumpOption {
	return func(o *bumpOptions) { o.richReject = v }
}

// WithProcessInfo records the originating process attribution for the
// bumped flow. Stamped onto every emitted compliance.AuditInfo so the
// admin Traffic UI can show the source app's name + bundle ID even on
// inspect rows. cp callers pass empty strings (the proxy doesn't know
// the originating process); the agent bridge populates from
// NEAppProxyFlow.metaData.
func WithProcessInfo(name, bundle, user string) BumpOption {
	return func(o *bumpOptions) {
		o.procName = name
		o.procBundle = bundle
		o.procUser = user
	}
}

// WithSourceInfo sets the source IP and connection ID for compliance transactions.
func WithSourceInfo(sourceIP, connectionID string) BumpOption {
	return func(o *bumpOptions) {
		o.sourceIP = sourceIP
		o.connectionID = connectionID
	}
}

// WithPayloadCapture injects the atomically swappable payload-capture
// config store so the forward handler can read the current max-body-bytes
// cap and request/response capture flags on every request. A nil store
// leaves the handler using payloadcapture.DefaultConfig.
// WithDomainEngine injects the domainpolicy engine so the forward
// handler can enforce per-path actions (PROCESS / PASSTHROUGH / BLOCK)
// derived from the InterceptionDomain + InterceptionPath admin config.
func WithDomainEngine(eng *domain.Engine) BumpOption {
	return func(o *bumpOptions) {
		o.domainEngine = eng
	}
}

func WithPayloadCapture(store *payloadcapture.Store) BumpOption {
	return func(o *bumpOptions) {
		o.payloadCaptureStore = store
	}
}

// WithNormalizeRegistry injects the Tier 1+2+3 normalize chain so
// runtimeNormalize uses reg.Normalize (same code path Hub agent_audit
// BuildAuditFn uses). Every production consumer wires this; without
// it, no structured normalized payload is produced and only the
// adapter's segment extraction feeds the hook pipeline.
func WithNormalizeRegistry(reg *normalize.Registry) BumpOption {
	return func(o *bumpOptions) {
		o.normalizeRegistry = reg
	}
}

// WithAdapterRegistry injects the traffic adapter registry so the forward
// handler can resolve the correct per-provider adapter (ExtractRequest,
// DetectRequestMeta, DetectResponseUsage, ExtractResponse) from the
// adapter_id stored in the matched interception_domain row.
func WithAdapterRegistry(reg *traffic.AdapterRegistry) BumpOption {
	return func(o *bumpOptions) {
		o.adapterRegistry = reg
	}
}

// serveHTTP2 serves the TLS connection using HTTP/2.
// http2.Server.ServeConn blocks until the connection is done; each HTTP/2
// stream gets its own goroutine automatically.
func serveHTTP2(tlsConn *tls.Conn, handler http.Handler, logger *slog.Logger) error {
	h2Server := &http2.Server{
		MaxConcurrentStreams: 100,
	}
	h2Server.ServeConn(tlsConn, &http2.ServeConnOpts{
		Handler: handler,
	})
	// ServeConn returns when the connection is done; this is not an error.
	logger.Debug("HTTP/2 connection closed")
	return nil
}

// singleConnListener is a net.Listener that yields exactly one connection
// and then blocks on Accept until closed. This lets us use http.Server
// to serve a single hijacked connection using HTTP/1.1.
type singleConnListener struct {
	conn net.Conn
	ch   chan net.Conn
	done chan struct{}
}

// newSingleConnListener creates a listener that yields conn exactly once.
func newSingleConnListener(conn net.Conn) *singleConnListener {
	ch := make(chan net.Conn, 1)
	ch <- conn
	return &singleConnListener{
		conn: conn,
		ch:   ch,
		done: make(chan struct{}),
	}
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		if c != nil {
			return c, nil
		}
		return nil, fmt.Errorf("listener closed")
	case <-l.done:
		return nil, fmt.Errorf("listener closed")
	}
}

func (l *singleConnListener) Close() error {
	select {
	case <-l.done:
		// Already closed.
	default:
		close(l.done)
	}
	return nil
}

func (l *singleConnListener) Addr() net.Addr {
	return l.conn.LocalAddr()
}

// serveHTTP1 serves the TLS connection using HTTP/1.1 via http.Server
// with a single-connection listener. The server reads requests one at a time
// and calls the handler for each.
func serveHTTP1(tlsConn *tls.Conn, handler http.Handler, logger *slog.Logger) error {
	listener := newSingleConnListener(tlsConn)

	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// Disable HTTP/2 on this server since we are explicitly in HTTP/1.1 mode.
		TLSNextProto: make(map[string]func(*http.Server, *tls.Conn, http.Handler)),
	}

	// Serve blocks until the connection is closed or an error occurs.
	err := server.Serve(listener)
	// http.ErrServerClosed is expected when the listener is closed normally.
	if err != nil && err != http.ErrServerClosed {
		logger.Debug("HTTP/1.1 serve ended", "error", err)
		return fmt.Errorf("HTTP/1.1 serve: %w", err)
	}

	logger.Debug("HTTP/1.1 connection closed")
	return nil
}
