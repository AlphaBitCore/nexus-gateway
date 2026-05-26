// Package wiring wires all nexus-agent subsystems together. Each file in this
// package corresponds to one subsystem. main.go calls Init* functions in
// dependency order.
package wiring

import (
	"context"
	"log/slog"
	"sync"
	"time"

	auditqueue "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/audit/queue"
	agentcompliance "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/compliance"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/network/intercept"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/lifecycle/killswitch"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform"
	policy "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/policy/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/spilluploader"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
)

// ConnectionBridge implements platform.ConnectionHandler and platform.FlowAuditor.
// It bridges platform-level connection interception to the policy engine and
// audit queue.
type ConnectionBridge struct {
	PolicyEngine     *policy.Engine
	InterceptHandler *intercept.Handler
	AuditQueue       *auditqueue.Queue
	ThingID          string

	// ProviderTrafficNotifier is the live-traffic pulse hook (#69).
	// Called every time a provider-tagged audit event lands. nil
	// in tests / minimal bring-up; main wires it to
	// statusCollector.MarkProviderTraffic.
	ProviderTrafficNotifier func()

	// AgentPipeline holds the shared compliance pipeline so
	// HandleConnection can run the connection-stage hook pipeline before
	// the platform shim commits to MITM. Pre-MITM rejection is the
	// cheapest point to block a flow. When AgentPipeline is nil
	// (unit tests, minimal bring-up), the connection-stage call is
	// skipped — matching fail-open policy.
	AgentPipeline *agentcompliance.AgentPipeline

	// PayloadCaptureStore is the payload capture runtime config. The Store
	// is updated via shadow pushes of the "payload_capture" Cat B key.
	// InspectRequest / InspectResponse read the current snapshot to decide
	// whether to stamp pre-hook bytes onto the InspectionResult.
	// ReadBodyCap returns the per-flow inspection buffer ceiling
	// (sourced from spill.perObjectCap, NOT MaxInlineBodyBytes) so
	// MITMRelay can buffer SSE streams up to a generous OOM guard.
	PayloadCaptureStore *payloadcapture.Store

	// InspectBodyCap is the per-flow buffer ceiling MITMRelay uses for
	// request and response body inspection / capture. <= 0 means
	// "use default 256 MiB".
	InspectBodyCap int64

	// SpillUploader is the producer-owned spill uploader. When non-nil
	// and a captured body exceeds MaxInlineBodyBytes, applyPayloadCapture
	// asks Hub to mint a one-shot URL, PUTs the body, and stamps the
	// resulting SpillRef on the audit event. nil = inline-only (oversize
	// bodies fall back to inline-truncated).
	SpillUploader *spilluploader.Uploader

	// KillSwitch gates the policy engine. When the operator engages the
	// switch in CP UI, Hub pushes the "killswitch" config_key to every
	// agent and HandleConnection returns DecisionPassthrough without
	// consulting the policy engine. Nil in unit tests; the gate is a
	// no-op then.
	KillSwitch *killswitch.Switch

	policyMu      sync.Mutex
	policyResults map[string]string // flowID → matched policy pattern

	// killSwitchAuditMu + killSwitchAuditLast rate-limit the
	// "kill switch passthrough" log line to one entry per host per minute
	// so a chatty network does not flood the audit pipeline while the
	// switch is engaged.
	killSwitchAuditMu   sync.Mutex
	killSwitchAuditLast map[string]time.Time
}

// ReadBodyCap returns the per-flow inspection buffer ceiling sourced
// from spill.perObjectCap. Implements platform.BodyReadCapper so
// Linux/Windows MITMRelay can buffer the decrypted request/response
// body up to the generous OOM guard without being clamped to the
// inline cutoff.
func (b *ConnectionBridge) ReadBodyCap() int64 {
	return b.InspectBodyCap
}

// IsKillSwitchEngaged is the platform.KillSwitchGater hook the
// macOS NE plumbing consults before tracking a flow in activeFlows.
// The kill-switch wire semantic is the canonical one — engaged=true
// means engaged — so the bridge reads IsEngaged() directly with no
// inversion.
func (b *ConnectionBridge) IsKillSwitchEngaged() bool {
	return b.KillSwitch != nil && b.KillSwitch.IsEngaged()
}

// HandleConnection evaluates the policy engine for the destination host and
// returns the interception decision (inspect/passthrough/deny).
//
// Before returning, HandleConnection also runs the connection-stage
// compliance hook pipeline (when wired). A RejectHard decision from any
// connection-stage hook overrides the policy-engine outcome to
// DecisionDeny. Infrastructure errors inside the pipeline fail open.
func (b *ConnectionBridge) HandleConnection(conn platform.InterceptedConn) platform.Decision {
	// When the fleet kill switch is engaged, agent must stop intercepting
	// immediately. Returning DecisionPassthrough here skips the policy
	// engine, the connection-stage hooks, and the platform MITM relay
	// entirely — the connection is forwarded raw.
	if b.KillSwitch != nil && b.KillSwitch.IsEngaged() {
		b.recordKillSwitchPassthrough(conn.DstHost, conn.FlowID)
		return platform.DecisionPassthrough
	}

	// At flow_new time we only know the host (from SNI peek), not the
	// request path. The interception_domain → interception_path table is
	// the authoritative process/passthrough source — admin may have set
	// domain.default_path_action = PASSTHROUGH but configured e.g.
	// /v1/chat/completions = PROCESS underneath. We MUST bump the TLS
	// connection to see the path layer; forward_handler then calls
	// domain.Engine.PathAction() per request and runs hooks only for the
	// matching paths. Mirrors compliance-proxy's behaviour.
	//
	// Decision rule: domain matches interception_domains → INSPECT
	// (always bump). Otherwise fall through to the policy.Engine host-glob
	// rules (policy_rules table).
	var decision platform.Decision
	var result policy.PolicyResult
	if b.AgentPipeline != nil {
		if eng := b.AgentPipeline.DomainEngine(); eng != nil {
			if matched := eng.MatchHost(conn.DstHost); matched != nil {
				decision = platform.DecisionInspect
				result = policy.PolicyResult{
					Action:         "inspect",
					Matched:        true,
					MatchedPattern: "domain:" + matched.HostPattern,
					MatchedIndex:   -1,
				}
			}
		}
	}
	if !result.Matched {
		result = b.PolicyEngine.Evaluate(conn.DstHost)
		switch result.Action {
		case "inspect":
			decision = platform.DecisionInspect
		case "deny":
			decision = platform.DecisionDeny
		default:
			decision = platform.DecisionPassthrough
		}
	}

	if result.Matched && result.MatchedPattern != "" {
		b.policyMu.Lock()
		if b.policyResults == nil {
			b.policyResults = make(map[string]string)
		}
		b.policyResults[conn.FlowID] = result.MatchedPattern
		b.policyMu.Unlock()
	}

	// Connection-stage hook pipeline: overrides the policy engine's
	// decision to Deny when a hook rejects. Skipped when no pipeline is
	// wired (tests) or when the policy engine already rejected.
	if b.AgentPipeline != nil && decision != platform.DecisionDeny {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		blocked, reason := b.AgentPipeline.EvaluateConnection(ctx, agentcompliance.EvaluateConnectionInput{
			SourceIP:   conn.SrcIP,
			TargetHost: conn.DstHost,
			SNI:        conn.DstHost,
		})
		cancel()
		if blocked {
			slog.Warn("connection rejected by connection-stage hook",
				"host", conn.DstHost,
				"flowId", conn.FlowID,
				"reason", reason,
			)
			decision = platform.DecisionDeny
		}
	}

	slog.Info("connection decision",
		"flow_id", conn.FlowID,
		"host", conn.DstHost,
		"port", conn.DstPort,
		"action", result.Action,
		"decision", decision,
		"matched_pattern", result.MatchedPattern,
		"src_proc", conn.Process.Name,
		"src_bundle", conn.Process.BundleID,
	)
	return decision
}

// recordKillSwitchPassthrough emits an INFO log per host at most once
// per minute while the kill switch is engaged.
func (b *ConnectionBridge) recordKillSwitchPassthrough(host, flowID string) {
	now := time.Now()
	b.killSwitchAuditMu.Lock()
	if b.killSwitchAuditLast == nil {
		b.killSwitchAuditLast = make(map[string]time.Time)
	}
	last, ok := b.killSwitchAuditLast[host]
	emit := !ok || now.Sub(last) > time.Minute
	if emit {
		b.killSwitchAuditLast[host] = now
	}
	b.killSwitchAuditMu.Unlock()

	if emit {
		slog.Info("kill switch engaged, passing through",
			"event", "killswitch_passthrough",
			"host", host,
			"flowId", flowID,
		)
	}
}
