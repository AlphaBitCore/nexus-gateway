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
	"sync/atomic"
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
// tunables. The mode field was removed in #115 — admin streaming
// policy (resolved via Config.StreamingPolicyStore) is now the single
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
	ConnStart   time.Time
	KillSwitchChecker func() bool

	// TLS interception
	GetCert  func(*tls.ClientHelloInfo) (*tls.Certificate, error)
	Upstream *tlsbump.UpstreamTransport

	// Pinning
	PinningTracker *tlsbump.PinningTracker

	// Compliance kernel
	CompliancePipeline    *compliance.PolicyResolver
	DomainSnapshot        *atomic.Pointer[traffic.DomainSnapshot]
	AuditEmitter          *compliance.AuditEmitter
	StreamingTuning       StreamingTuning
	StreamingConfig       streaming.LiveConfig
	ParallelHooks         bool
	StreamingPolicyStore  *streampolicy.Store

	// ExemptionStore holds temporarily exempted source/host pairs.
	ExemptionStore *exemption.Store

	// Compliance options
	RejectConfig        tlsbump.RejectConfig
	PayloadCaptureStore *payloadcapture.Store
	DomainEngine        *domain.Engine
	AdapterRegistry     *traffic.AdapterRegistry
	// NormalizeRegistry — V2 #67 — Tier 1+2+3 chain shared with
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

// logRelayResult classifies the return of a tlsbump.PassThrough call.
// Dial failures stay at WARN; copy-side EOF/ECONNRESET errors drop to DEBUG.
func logRelayResult(logger *slog.Logger, label string, err error) {
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
		logRelayResult(logger, "kill switch passthrough", tlsbump.PassThrough(ctx, conn, cfg.TargetHost))
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
			logRelayResult(logger, "pinning exemption passthrough", tlsbump.PassThrough(ctx, conn, cfg.TargetHost))
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
			cfg.DomainSnapshot,
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
			logger.Warn("TLS bump failed due to pinning, falling back to passthrough",
				"error", err,
				"bump_status", bumpStatus,
			)
			if metrics.PinningPassthroughTotal != nil {
				metrics.PinningPassthroughTotal.With(tlsbump.BumpStatusFailedPassthrough).Inc()
			}
			logRelayResult(logger, "pinning fallback passthrough", tlsbump.PassThrough(ctx, conn, cfg.TargetHost))
			return
		}
		logger.Error("bump connection ended with error", "error", err)
		return
	}

	logger.Info("connection closed normally", "duration", fmt.Sprintf("%v", time.Since(cfg.ConnStart)))
}
