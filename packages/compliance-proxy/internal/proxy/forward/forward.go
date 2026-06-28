// Package forward implements the compliance-pipeline + TLS-bump forwarding
// path for bumped CONNECT tunnels. It is called by proxy/server after tunnel
// establishment and connection-stage hook evaluation.
package forward

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/compliance"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/exemption"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/metrics"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/domain"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/enums"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	normalizecore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming"
	streampolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming/policy"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/tlsbump"
)

// StreamingTuning is the hot-swappable bundle of streaming-timeout
// tunables. There is no mode field — admin streaming
// policy (resolved via Config.StreamingPolicyStore) is the single
// source of truth for SSE mode dispatch.
type StreamingTuning struct {
	PerHookTimeout time.Duration
	TotalTimeout   time.Duration
}

// Config carries all dependencies the forward path needs from ProxyServer.
// The server layer builds one of these per-request from its own fields so
// forward/ has no import cycle back to server/.
type Config struct {
	// Connection identifiers
	SourceAddr string
	TargetHost string
	Host       string // parsed host without port
	ConnID     string // "<sourceAddr>-><targetHost>" or ""

	// Lifecycle
	ConnStart         time.Time
	KillSwitchChecker func() bool

	// TLS interception
	GetCert  func(*tls.ClientHelloInfo) (*tls.Certificate, error)
	Upstream *tlsbump.UpstreamTransport

	// Pinning
	PinningTracker *tlsbump.PinningTracker

	// Compliance kernel
	CompliancePipeline   *compliance.PolicyResolver
	AuditEmitter         *compliance.AuditEmitter
	StreamingTuning      StreamingTuning
	StreamingConfig      streaming.LiveConfig
	ParallelHooks        bool
	StreamingPolicyStore *streampolicy.Store

	// ExemptionStore holds temporarily exempted source/host pairs.
	ExemptionStore *exemption.Store

	// Compliance options
	RejectConfig        tlsbump.RejectConfig
	PayloadCaptureStore *payloadcapture.Store
	DomainEngine        *domain.Engine
	AdapterRegistry     *traffic.AdapterRegistry
	// NormalizeRegistry — Tier 1+2+3 chain shared with
	// agent + ai-gateway + nexus-hub agent_audit. Wired into
	// tlsbump via WithNormalizeRegistry so hookInput.Normalized
	// comes from the full fallback chain instead of nil when the
	// per-adapter Tier 1 spec misses.
	NormalizeRegistry *normalizecore.Registry

	// AttestationVerifier — when non-nil and the inner request carries a
	// valid X-Nexus-Attestation header, tlsbump's forward handler
	// short-circuits to pure passthrough (no hooks, no audit, no payload
	// capture). Nil disables the feature (no per-request peek cost). Wired
	// by the server layer from ProxyServer.attestationVerifier; see
	// WithAttestationVerifier in shared/transport/tlsbump.
	AttestationVerifier tlsbump.AttestationVerifierFunc

	// Logger (pre-populated with source/target fields)
	Logger *slog.Logger
}

// bumpConnFn is the package-level seam for tlsbump.BumpConnection so tests
// can inject a synthetic error without performing a real TLS handshake.
// Production code always uses the real implementation; tests override this
// variable before calling Run.
var bumpConnFn = tlsbump.BumpConnection

// LogRelayResult classifies the return of a tlsbump.PassThrough call. Dial
// failures (DNS, TCP refused, network unreachable) are real problems and stay
// at WARN. Copy-side errors are routine peer-close events on a transparent TCP
// relay (EOF, ECONNRESET, EPIPE, net.ErrClosed when one side hangs up) — logging
// them at ERROR drowns the channel in benign noise, so we drop those to DEBUG.
// Exported because the server package shares this exact classification for its
// unlisted-passthrough relay.
func LogRelayResult(logger *slog.Logger, label string, err error) {
	if err == nil {
		return
	}
	var ptErr *tlsbump.PassThroughError
	if errors.As(err, &ptErr) && ptErr.Op == "dial" {
		logger.Warn(label+" dial failed", "error", err)
		return
	}
	logger.Debug(label+" relay closed", "error", err)
}

// Run executes the full forward path on the established tunnel conn:
//  1. Kill-switch passthrough check
//  2. Pinning-exemption passthrough check
//  3. Temporary hook-exemption check
//  4. Build compliance BumpOptions
//  5. tlsbump.BumpConnection (TLS interception + compliance pipeline)
//
// Run returns after the tunnel is fully drained or on error. The caller
// is responsible for closing conn after Run returns.
func Run(ctx context.Context, conn net.Conn, cfg Config) {
	logger := cfg.Logger

	// Kill switch check — passthrough without bump, emit audit event.
	if cfg.KillSwitchChecker != nil && cfg.KillSwitchChecker() {
		logger.Warn("TLS bump disabled via kill switch, using passthrough")
		if metrics.PinningPassthroughTotal != nil {
			metrics.PinningPassthroughTotal.With(string(enums.BumpStatusDisabledEmergency)).Inc()
		}
		if cfg.AuditEmitter != nil {
			cfg.AuditEmitter.EmitKillSwitchPassthrough(cfg.SourceAddr, cfg.TargetHost)
		}
		LogRelayResult(logger, "kill switch passthrough", tlsbump.PassThrough(ctx, conn, cfg.TargetHost))
		return
	}

	// Pinning-exemption check — passthrough without bump.
	if cfg.PinningTracker != nil {
		if exempt, reason, bumpStatus := cfg.PinningTracker.IsExempt(cfg.Host); exempt {
			logger.Info("TLS bump exempt, using passthrough",
				"reason", reason,
				"bump_status", bumpStatus,
			)
			if metrics.PinningPassthroughTotal != nil {
				metrics.PinningPassthroughTotal.With(bumpStatus).Inc()
			}
			LogRelayResult(logger, "pinning exemption passthrough", tlsbump.PassThrough(ctx, conn, cfg.TargetHost))
			return
		}
	}

	// Temporary exemption: skip compliance hooks but still TLS bump.
	var hookExempted bool
	if cfg.ExemptionStore != nil && cfg.CompliancePipeline != nil {
		sourceIP, _, _ := net.SplitHostPort(cfg.SourceAddr)
		if exempt, matched := cfg.ExemptionStore.IsExempt(sourceIP, cfg.Host); exempt {
			hookExempted = true
			logger.Info("request exempt from compliance hooks",
				"exemptionId", matched.ID,
				"exemptionSourceIp", matched.SourceIP,
				"exemptionTargetHost", matched.TargetHost,
				"exemptionReason", matched.Reason,
				"exemptionCreatedBy", matched.CreatedBy,
				"exemptionExpiresAt", matched.ExpiresAt,
			)
			if cfg.AuditEmitter != nil {
				cfg.AuditEmitter.EmitExempted(sourceIP, cfg.Host, matched.ID, matched.Reason)
			}
		}
	}

	// Build compliance BumpOptions.
	var bumpOpts []tlsbump.BumpOption
	// WithIdentity stamps "compliance-proxy" on x-nexus-via for response markers.
	// Always set even when compliance is bypassed so operators can identify the proxy.
	bumpOpts = append(bumpOpts, tlsbump.WithIdentity("compliance-proxy"))
	// The compliance-proxy is a dedicated forward-proxy appliance that
	// already returns 403 for disallowed CONNECTs, so it can safely REFUSE. Opt in
	// to strict fail-closed: an admin-configured failBehavior="fail-closed" hook
	// that is UNBUILDABLE (unregistered implementationId on a partial deploy, or a
	// malformed pushed rule) makes BuildPipeline error → the request is refused
	// rather than forwarded uninspected. The agent NE host-packet path does NOT
	// set this and stays fail-open by design.
	bumpOpts = append(bumpOpts, tlsbump.WithStrictFailClosed())
	// The compliance-proxy owns a client-facing HTTP response (its callers are
	// API/browser clients expecting a structured 403), so a block writes the
	// rich attributed reject body (blocked_by_policy envelope + rule-ID reason)
	// rather than a bare "Forbidden".
	bumpOpts = append(bumpOpts, tlsbump.WithRichReject(true))
	// Install the attestation verifier so tlsbump's pre-pipeline peek can
	// short-circuit attested requests to pure passthrough. Nil-safe — no-op
	// when feature off.
	if cfg.AttestationVerifier != nil {
		bumpOpts = append(bumpOpts, tlsbump.WithAttestationVerifier(cfg.AttestationVerifier))
	}
	if cfg.CompliancePipeline != nil && !hookExempted {
		st := cfg.StreamingTuning
		bumpOpts = append(bumpOpts, tlsbump.WithCompliance(
			cfg.CompliancePipeline,
			cfg.AuditEmitter,
			cfg.StreamingConfig,
			st.PerHookTimeout,
			st.TotalTimeout,
			cfg.ParallelHooks,
		))
		if cfg.StreamingPolicyStore != nil {
			bumpOpts = append(bumpOpts, tlsbump.WithStreamingPolicyStore(cfg.StreamingPolicyStore))
		}
		sourceIP, _, _ := net.SplitHostPort(cfg.SourceAddr)
		bumpOpts = append(bumpOpts, tlsbump.WithSourceInfo(sourceIP, cfg.ConnID))
		bumpOpts = append(bumpOpts, tlsbump.WithRejectConfig(cfg.RejectConfig))
		if cfg.PayloadCaptureStore != nil {
			bumpOpts = append(bumpOpts, tlsbump.WithPayloadCapture(cfg.PayloadCaptureStore))
		}
		if cfg.DomainEngine != nil {
			bumpOpts = append(bumpOpts, tlsbump.WithDomainEngine(cfg.DomainEngine))
		}
		if cfg.AdapterRegistry != nil {
			bumpOpts = append(bumpOpts, tlsbump.WithAdapterRegistry(cfg.AdapterRegistry))
		}
		if cfg.NormalizeRegistry != nil {
			bumpOpts = append(bumpOpts, tlsbump.WithNormalizeRegistry(cfg.NormalizeRegistry))
		}
	}

	// TLS interception + compliance pipeline execution.
	if err := bumpConnFn(ctx, conn, cfg.TargetHost, cfg.GetCert, cfg.Upstream, logger, bumpOpts...); err != nil {
		if cfg.PinningTracker != nil && tlsbump.IsPinningError(err) {
			bumpStatus := cfg.PinningTracker.RecordFailure(cfg.Host)
			// FAIL_CLOSED enforcement (on_adapter_error). When the matched
			// interception domain is configured FAIL_CLOSED, a pinned flow we
			// cannot inspect MUST NOT be relayed uninspected — refuse it by
			// closing the connection (return without PassThrough). This is the
			// runtime consumer of domain.AdapterErrorFailClosed. Default is
			// unchanged: an unmatched host, a FAIL_OPEN domain, or a missing
			// engine all return false here and fall through to the passthrough.
			if cfg.DomainEngine.ShouldFailClosed(cfg.Host) {
				logger.Warn("TLS bump failed due to pinning and matched domain is FAIL_CLOSED — refusing flow (not relaying uninspected)",
					"error", err,
					"bump_status", bumpStatus,
					"host", cfg.Host,
					"on_adapter_error", "FAIL_CLOSED",
				)
				return
			}
			logger.Warn("TLS bump failed due to pinning, falling back to passthrough",
				"error", err,
				"bump_status", bumpStatus,
			)
			if metrics.PinningPassthroughTotal != nil {
				metrics.PinningPassthroughTotal.With(tlsbump.BumpStatusFailedPassthrough).Inc()
			}
			LogRelayResult(logger, "pinning fallback passthrough", tlsbump.PassThrough(ctx, conn, cfg.TargetHost))
			return
		}
		logger.Error("bump connection ended with error", "error", err)
		return
	}

	logger.Info("connection closed normally", "duration", fmt.Sprintf("%v", time.Since(cfg.ConnStart)))
}
