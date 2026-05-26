package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	auditqueue "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/audit/queue"
	agentTLS "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/network/tls"
	sharedaudit "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/bufconn"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/domain"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore"
	normalizecore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming"
	streampolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming/policy"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/tlsbump"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// BridgeDeps holds the per-process dependencies the agent's NE bridge ingress
// passes into each tlsbump.BumpConnection call. Constructed once in main.go
// after thingclient settles its initial config pull, then reused for every
// flow accepted from the Swift NE bridge listener.
type BridgeDeps struct {
	Logger              *slog.Logger
	TLSEngine           *agentTLS.Engine
	Upstream            *tlsbump.UpstreamTransport
	PolicyResolver      *pipeline.PolicyResolver
	DomainSnapshot      *atomic.Pointer[traffic.DomainSnapshot]
	DomainEngine        *domain.Engine
	AdapterRegistry     *traffic.AdapterRegistry
	// NormalizeRegistry — V2 #67 — Tier 1+2+3 shared chain. Wired via
	// tlsbump.WithNormalizeRegistry so runtimeNormalize / hook pipeline
	// see canonical NormalizedPayload even when Tier 1 per-adapter spec
	// match fails (chatgpt-web SSE delta_encoding, cursor protobuf
	// variants). Built once at boot via wiring.InitNormalizeRegistry.
	NormalizeRegistry   *normalizecore.Registry
	PayloadCaptureStore *payloadcapture.Store
	SpillStore          spillstore.SpillStore
	// StreamingPolicy is the live store the bridge reads on every flow,
	// so admin shadow updates take effect without restarting the
	// daemon. Nil store falls back to passthrough (the conservative
	// default tlsbump's resolveStreamingMode applies when the Store
	// is unwired). #115 deleted the legacy StreamingMode YAML field —
	// admin policy is the single source of truth across all three
	// services (agent / compliance-proxy / ai-gateway).
	StreamingPolicy *streampolicy.Store
	AuditQueue      *auditqueue.Queue

	// Per-hook + total timeouts feed into PolicyResolver.BuildPipeline so
	// the agent matches compliance-proxy's hook-execution budgets. Defaults
	// (5 s / 30 s) apply when zero.
	PerHookTimeout time.Duration
	TotalTimeout   time.Duration
}

// loggingQueueWriter wraps the agent's audit Queue writer so every
// per-request Enqueue lands an INFO log line with the audit row's
// identity (host + method + path + classification inputs). The chain
// is otherwise opaque end-to-end:
//
//	tlsbump.forward_handler.Emit → compliance.AuditEmitter.buildEvent
//	  → writer_adapter.Enqueue → Queue.Record → SQLite
//
// The log line is the canonical diagnostic anchor — every per-request
// emit produces exactly one INFO entry, queryable by host or txid.
type loggingQueueWriter struct {
	next   sharedaudit.Writer
	logger *slog.Logger
}

func (w *loggingQueueWriter) Enqueue(e sharedaudit.AuditEvent) {
	logger := w.logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Info("audit emit (per-request)",
		"event_id", e.ID,
		"trace_id", e.TraceID,
		"target_host", e.TargetHost,
		"method", e.Method,
		"path", e.Path,
		"hook_decision", e.RequestHookDecision,
		"bump_status", e.BumpStatus,
		"domain_rule_id", e.DomainRuleID,
		"path_action", e.PathAction,
		"source_process", e.SourceProcess,
		"source_bundle", e.SourceProcessBundle,
		"provider", e.Provider,
		"model", e.Model,
		"latency_ms", e.LatencyMs,
	)
	if w.next != nil {
		w.next.Enqueue(e)
	}
}

func (w *loggingQueueWriter) Flush(ctx context.Context) error {
	if w.next != nil {
		return w.next.Flush(ctx)
	}
	return nil
}

func (w *loggingQueueWriter) Close(ctx context.Context) error {
	if w.next != nil {
		return w.next.Close(ctx)
	}
	return nil
}

// FlowProcess carries the originating process attribution for a single
// bridged flow. Sourced from NEAppProxyFlow.metaData at flow_new and
// passed through here so EVERY emitted audit_event row (one per HTTP
// request) populates the "App" column for inspect traffic too. cp
// callers can't populate this — the proxy doesn't see callers as
// processes — and pass an empty FlowProcess.
type FlowProcess struct {
	Name   string // executable name (e.g. "Google Chrome Helper")
	Bundle string // macOS bundle identifier (e.g. "com.google.Chrome.helper")
	User   string // owning OS user
}

// BumpFlow is the agent's NE-bridge entry point that hands a Swift-relayed
// inspect-mode flow off to shared/tlsbump.BumpConnection.
//
// Audit model (cp-aligned, per-request): tlsbump's compliance.AuditEmitter
// is wired directly to the agent's local SQLite AuditQueue via
// auditqueue.NewQueueWriter(deps.AuditQueue). Every HTTP request inside
// the bumped TLS connection produces one Emit() call, which writes one
// SQLite row carrying method, path, hookDecision, provider, model,
// bodies, source process / bundle / user, domain rule id, path action,
// usage stats — everything the admin UI needs.
//
// Returns nil on clean completion or an error from
// tlsbump.BumpConnection. Per-request audit rows are written to the
// queue regardless of return value (writes happen inside the
// connection, not in this function's epilogue).
func BumpFlow(
	ctx context.Context,
	clientConn net.Conn,
	peekedClientHello []byte,
	dstHost string,
	dstPort int,
	flowID string,
	proc FlowProcess,
	deps BridgeDeps,
) error {
	if deps.TLSEngine == nil {
		return fmt.Errorf("BumpFlow: nil TLSEngine")
	}
	if deps.Upstream == nil {
		return fmt.Errorf("BumpFlow: nil Upstream")
	}
	if deps.AuditQueue == nil {
		return fmt.Errorf("BumpFlow: nil AuditQueue (per-request emit requires a queue sink)")
	}

	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger = logger.With("flow_id", flowID, "dst_host", dstHost, "dst_port", dstPort)

	// Port-filter: TLS-bump only makes sense on TLS ports. Non-TLS
	// ports (SSH 22, SMTP 25, plain HTTP 80, MySQL 3306, etc.) reach
	// the bridge when the daemon's policy engine returns INSPECT for
	// the host without checking dst_port — github.com matches an
	// inspect rule, then a `git push` over SSH to github.com:22
	// arrives here and the upstream-cert probe fails with "first
	// record does not look like a TLS handshake". Rather than
	// failing the flow (which kills SSH for the user), opaque-relay
	// it. Still log so operators can spot misconfigured rules.
	if dstPort != 443 && dstPort != 8443 {
		logger.Info("tlsbump: non-TLS port; opaque relay (no bump)",
			"dst_port", dstPort,
		)
		relayStart := time.Now()
		bytesUp, bytesDown, err := opaqueRelay(ctx, clientConn, peekedClientHello, dstHost, dstPort)
		if err != nil {
			logger.Warn("tlsbump: opaque relay failed",
				"error", err,
				"bytes_up", bytesUp, "bytes_down", bytesDown,
				"elapsed_ms", time.Since(relayStart).Milliseconds(),
			)
			return err
		}
		logger.Info("tlsbump: opaque relay completed (non-TLS port)",
			"bytes_up", bytesUp, "bytes_down", bytesDown,
			"elapsed_ms", time.Since(relayStart).Milliseconds(),
		)
		return nil
	}

	// Mint a leaf cert keyed only on the host. Mirrors compliance-proxy's
	// cert.Issuer.SignCert: CN={hostname}, SAN={hostname}, signed by the
	// device CA. The device CA is in the System Keychain trust store so
	// every local HTTPS client accepts the leaf — no need to probe the
	// upstream's actual cert. Eliminating the probe also eliminates the
	// anti-bot rejection class (Cursor api2.cursor.sh, some Cloudflare
	// endpoints) that was forcing us into opaque-relay fallback and
	// losing all inspection on those flows.
	mintStart := time.Now()
	cached, mintErr := deps.TLSEngine.IssueLeafCertByHostname(dstHost)
	if mintErr != nil {
		// Mint should never fail on a valid CA; log + opaque-relay fallback
		// only as defense-in-depth (so a corrupted device CA doesn't kill
		// the user's flow).
		logger.Warn("tlsbump: hostname-only leaf mint failed; falling open to opaque relay",
			"error", mintErr,
			"elapsed_ms", time.Since(mintStart).Milliseconds(),
		)
		relayStart := time.Now()
		bytesUp, bytesDown, relayErr := opaqueRelay(ctx, clientConn, peekedClientHello, dstHost, dstPort)
		if relayErr != nil {
			return relayErr
		}
		logger.Info("tlsbump: opaque relay completed (mint failed → passthrough)",
			"bytes_up", bytesUp, "bytes_down", bytesDown,
			"elapsed_ms", time.Since(relayStart).Milliseconds(),
		)
		return nil
	}
	logger.Debug("tlsbump: hostname-only leaf cert minted",
		"hostname", dstHost,
		"mint_ms", time.Since(mintStart).Milliseconds(),
	)
	mintedTLSCert := &tls.Certificate{
		Certificate: [][]byte{cached.CertDER},
		PrivateKey:  cached.Key,
	}

	// Replay peeked bytes before tls.Server reads the handshake.
	wrapped := bufconn.New(clientConn, peekedClientHello)

	// getCert just returns the pre-minted cert; no per-Hello probe.
	getCert := func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		return mintedTLSCert, nil
	}

	// Wrap the queue writer so every Enqueue logs at INFO with
	// host + method + path + classification inputs, making "rows
	// missing" reports diagnosable via grep on agent.log.
	queueWriter := &loggingQueueWriter{
		next:   auditqueue.NewQueueWriter(deps.AuditQueue),
		logger: logger,
	}
	emitter := pipeline.NewAuditEmitter(queueWriter, logger)
	if deps.PayloadCaptureStore != nil {
		emitter = emitter.WithPayloadCaptureStore(deps.PayloadCaptureStore)
	}
	if deps.SpillStore != nil {
		emitter = emitter.WithSpillStore(deps.SpillStore)
	}

	perHookTimeout := deps.PerHookTimeout
	if perHookTimeout <= 0 {
		perHookTimeout = 5 * time.Second
	}
	totalTimeout := deps.TotalTimeout
	if totalTimeout <= 0 {
		totalTimeout = 30 * time.Second
	}

	bumpOpts := []tlsbump.BumpOption{
		// WithIdentity stamps "agent" on x-nexus-via for response markers.
		tlsbump.WithIdentity("agent"),
		// WithProcessInfo stamps the originating process name/bundle/user
		// onto every emitted audit_event so the admin UI's App column
		// populates for inspect rows.
		tlsbump.WithProcessInfo(proc.Name, proc.Bundle, proc.User),
		tlsbump.WithCompliance(
			deps.PolicyResolver,
			deps.DomainSnapshot,
			emitter,
			// LiveConfig zero value is fine — chunked_async defaults apply.
			// Per-host overrides resolve through streampolicy.Resolve at
			// request time using the Store below.
			streaming.LiveConfig{},
			perHookTimeout,
			totalTimeout,
			false, // parallelHooks: keep agent single-threaded for now.
		),
		tlsbump.WithSourceInfo("127.0.0.1", flowID),
	}
	// Pass the live streampolicy.Store reference so tlsbump's
	// resolveStreamingMode reads the latest snapshot per-flow (Hub
	// shadow push lands via ApplyShadowState without rebuilding
	// bumpOpts). Nil-safe: missing Store falls back to passthrough
	// inside resolveStreamingMode.
	if deps.StreamingPolicy != nil {
		bumpOpts = append(bumpOpts, tlsbump.WithStreamingPolicyStore(deps.StreamingPolicy))
	}
	if deps.PayloadCaptureStore != nil {
		bumpOpts = append(bumpOpts, tlsbump.WithPayloadCapture(deps.PayloadCaptureStore))
	}
	if deps.AdapterRegistry != nil {
		bumpOpts = append(bumpOpts, tlsbump.WithAdapterRegistry(deps.AdapterRegistry))
	}
	if deps.DomainEngine != nil {
		bumpOpts = append(bumpOpts, tlsbump.WithDomainEngine(deps.DomainEngine))
	}
	// nil-safe in tlsbump: the option setter just stores the pointer
	// and forward_handler.runtimeNormalize gates the actual call on
	// reg != nil — so we forward unconditionally to keep the wiring
	// branch-free (and trivially coverable).
	bumpOpts = append(bumpOpts, tlsbump.WithNormalizeRegistry(deps.NormalizeRegistry))

	// Resolve streaming-mode for the log line by reading the live
	// snapshot — same Store tlsbump will read at flow entry, so the
	// log reflects the dispatch decision.
	streamMode := string(streampolicy.ModePassThrough)
	if deps.StreamingPolicy != nil {
		streamMode = string(deps.StreamingPolicy.Get().Mode)
	}
	startedAt := time.Now()
	logger.Info("tlsbump: handing flow off to BumpConnection",
		"peeked_bytes", len(peekedClientHello),
		"per_hook_timeout_ms", perHookTimeout.Milliseconds(),
		"total_timeout_ms", totalTimeout.Milliseconds(),
		"have_payload_capture", deps.PayloadCaptureStore != nil,
		"have_spillstore", deps.SpillStore != nil,
		"have_domain_engine", deps.DomainEngine != nil,
		"have_adapter_registry", deps.AdapterRegistry != nil,
		"streaming_mode", streamMode,
	)
	if err := tlsbump.BumpConnection(ctx, wrapped, dstHost, getCert, deps.Upstream, logger, bumpOpts...); err != nil {
		// Classify failure mode so logs distinguish pinning rejections
		// from other transport errors.
		errStr := err.Error()
		stage := "unknown"
		switch {
		case strings.Contains(errStr, "TLS handshake with client"):
			stage = "client_pin_check"
		case strings.Contains(errStr, "first record does not look like a TLS handshake"):
			stage = "upstream_not_tls"
		case strings.Contains(errStr, "utls handshake to"):
			stage = "upstream_utls_dial"
		}
		logger.Warn("tlsbump: BumpConnection returned error",
			"error", err,
			"failure_stage", stage,
			"dst_host", dstHost,
			"elapsed_ms", time.Since(startedAt).Milliseconds(),
		)
		// #86: cert-pin clients (Cursor / Slack / Notion / Linear /
		// WhatsApp Mac / iOS-style mobile apps using NSURLSession with
		// SecTrust evaluation) reject our MITM cert at TLS handshake.
		// Without fall-back, the user's app silently fails:
		// - Cursor chat / autocomplete returns "request failed"
		// - Slack sits on connecting indicator forever
		// - Notion shows blank workspace
		// Observed live 2026-05-24: 89+ flows to api2.cursor.sh failed
		// with `TLS handshake with client: EOF` (client closed
		// connection on cert validation). User's Cursor IDE chat was
		// silently broken until quic-bundles got cleared.
		//
		// On client_pin_check failure, fall back to raw TCP relay via
		// opaqueRelay so the user's app keeps working. We lose
		// HTTP-level audit (no headers / body / hooks) for these flows
		// but retain TCP-level audit (host/port/bytes/duration via
		// AuditEmitter). The connection-level audit row is already
		// emitted by the calling handleBridgeFlow path before BumpFlow
		// — opaqueRelay only adds the relay phase. The trade is:
		// (HTTP-level visibility for cert-pin clients) vs (user-
		// visible silent breakage). The latter is unacceptable.
		//
		// ALL BumpConnection failures fall back to opaqueRelay (raw TCP)
		// so the user's app keeps working. The failure_stage above is
		// preserved in the log for diagnostic signal but does NOT gate
		// the fallback — even "unknown" stage gets opaque relay because
		// the user's perceived UX (Cursor works / Slack works / Notion
		// works) outranks our HTTP-level visibility on a single flow.
		// True bugs still surface via the WARN log above and an
		// additional WARN below if the fallback dial also fails.
		fallbackStartedAt := time.Now()
		logger.Info("tlsbump: falling back to opaqueRelay (raw TCP) — preserves user UX, loses HTTP-level audit for this flow",
			"failure_stage", stage,
			"dst_host", dstHost,
			"dst_port", dstPort,
		)
		bytesUp, bytesDown, relayErr := opaqueRelay(ctx, wrapped, peekedClientHello, dstHost, dstPort)
		if relayErr != nil {
			logger.Warn("tlsbump: opaqueRelay fallback ALSO failed — user's flow is gone",
				"orig_error", err,
				"orig_failure_stage", stage,
				"relay_error", relayErr,
				"dst_host", dstHost,
				"elapsed_ms", time.Since(fallbackStartedAt).Milliseconds(),
			)
			return relayErr
		}
		logger.Info("tlsbump: opaqueRelay fallback completed",
			"orig_failure_stage", stage,
			"dst_host", dstHost,
			"bytes_up", bytesUp,
			"bytes_down", bytesDown,
			"elapsed_ms", time.Since(fallbackStartedAt).Milliseconds(),
		)
		return nil
	}
	logger.Info("tlsbump: BumpConnection completed",
		"elapsed_ms", time.Since(startedAt).Milliseconds(),
	)
	return nil
}


// opaqueRelay shuttles bytes between client and upstream without TLS
// termination. Used for non-TLS ports (SSH/SMTP/MySQL/...) and as a
// fail-open fallback when the upstream-cert probe is rejected (some
// hosts refuse vanilla TLS dials from non-browser fingerprints).
// Preserves user functionality at the cost of inspection — the only
// available signal in this case is the destination host:port (already
// captured at bridge handshake time) and byte counts.
//
// Returns (bytesClientToUpstream, bytesUpstreamToClient, error). Caller
// logs at INFO so prod can see "Cursor's flow went opaque + relayed
// 47 KB up / 312 KB down in 8.2 s" and distinguish a working
// passthrough from a wedged one.
func opaqueRelay(ctx context.Context, clientConn net.Conn, peeked []byte, dstHost string, dstPort int) (int64, int64, error) {
	upstream, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(
		ctx, "tcp", net.JoinHostPort(dstHost, strconv.Itoa(dstPort)),
	)
	if err != nil {
		return 0, 0, fmt.Errorf("opaque relay dial: %w", err)
	}
	defer upstream.Close() //nolint:errcheck
	// Replay any peeked bytes (the Swift NE side already read them off
	// the client socket; without replay the upstream sees a truncated
	// initial record).
	var peekedBytes int64
	if len(peeked) > 0 {
		n, werr := upstream.Write(peeked)
		peekedBytes = int64(n)
		if werr != nil {
			return peekedBytes, 0, fmt.Errorf("opaque relay write peeked: %w", werr)
		}
	}
	// Bidirectional copy. First side that errors wins; the other gets
	// io.Copy's close on the half-conn implicitly via defer.
	type copyResult struct {
		bytes int64
		err   error
	}
	done := make(chan copyResult, 2)
	go func() {
		n, e := io.Copy(upstream, clientConn)
		done <- copyResult{bytes: n, err: e}
	}()
	go func() {
		n, e := io.Copy(clientConn, upstream)
		done <- copyResult{bytes: n, err: e}
	}()

	first := <-done
	// Best-effort drain of the second half so its goroutine exits +
	// we get an accurate byte count for the return value. Bounded by
	// ctx so a half-closed peer doesn't pin us forever.
	var second copyResult
	select {
	case second = <-done:
	case <-ctx.Done():
	}

	// Map first/second to the right direction. We can't tell which
	// goroutine returned first deterministically, so just sum what we
	// have. ctoU + peekedBytes is "client-to-upstream side"; the other
	// io.Copy is "upstream-to-client". For diagnostics this is good
	// enough — caller reports total bytes per direction.
	bytesUp := peekedBytes + first.bytes
	bytesDown := second.bytes
	finalErr := first.err
	if finalErr == nil || errors.Is(finalErr, io.EOF) {
		finalErr = nil
	}
	return bytesUp, bytesDown, finalErr
}
