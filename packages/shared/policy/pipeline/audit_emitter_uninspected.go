// Package pipeline: audit_emitter_uninspected.go — the rows that record traffic
// the proxy did NOT inspect.
//
// Three ways a bumped flow can be relayed without compliance running: an
// emergency kill-switch bypass, an operator-granted exemption, and an
// interception-path PASSTHROUGH rule. They are kept together because they are
// one family with one purpose — making the ABSENCE of inspection auditable — and
// because keeping them apart is how the third one came to record nothing at all
// while the other two recorded everything (R-6).
package pipeline

import (
	"fmt"
	"net"
	"time"

	"github.com/google/uuid"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
)

// EmitKillSwitchPassthrough records an audit event for a connection that
// bypassed TLS bump because the kill switch was engaged. This ensures the
// compliance gap is visible in dashboards and analytics.
func (e *AuditEmitter) EmitKillSwitchPassthrough(sourceAddr, targetHost string) {
	sourceIP, _, _ := net.SplitHostPort(sourceAddr)
	if sourceIP == "" {
		sourceIP = sourceAddr
	}

	reason := "kill switch engaged — TLS bump bypassed"
	reasonCode := "KILLSWITCH_ENGAGED"

	event := audit.AuditEvent{
		ID:                    uuid.NewString(),
		TransactionID:         uuid.NewString(),
		TrafficSource:         "COMPLIANCE_PROXY",
		IngressType:           "COMPLIANCE_PROXY",
		BumpStatus:            "BUMP_DISABLED_EMERGENCY",
		SourceIP:              sourceIP,
		TargetHost:            targetHost,
		RequestHookDecision:   "PASSTHROUGH",
		RequestHookReason:     &reason,
		RequestHookReasonCode: &reasonCode,
		Timestamp:             time.Now().UTC(),
	}

	e.writer.Enqueue(event)
}

// EmitPathPassthrough records an audit event for a request that was BUMPED —
// its TLS was intercepted and its URL is known — but then relayed without
// compliance inspection because an explicit interception-path rule said
// PASSTHROUGH.
//
// It exists because that flow used to leave no trace at all. The path-policy
// branch sets complianceEnabled=false and runResponseStage returns immediately,
// so traffic_event got nothing while the proxy log alone recorded the decision.
// Its two siblings both emit — an exemption grant (EmitExempted) and an
// emergency bypass (BUMP_DISABLED_EMERGENCY) — precisely so the ABSENCE of
// inspection is itself auditable. This was the only member of that family that
// recorded nothing, so an auditor asking "what passed through uninspected, and
// why" got two answers out of three.
//
// Only EXPLICIT path rules reach here, never a domain's DefaultPathAction: every
// seeded domain defaults to PASSTHROUGH, so auditing the default would emit a
// row for every script and image fetched from an intercepted web-chat host and
// bury the decisions under asset traffic.
func (e *AuditEmitter) EmitPathPassthrough(sourceIP, targetHost, method, path, domainName string) {
	reason := fmt.Sprintf("interception path rule on %s: PASSTHROUGH for %s %s", domainName, method, path)
	reasonCode := "PATH_PASSTHROUGH"

	event := audit.AuditEvent{
		ID:            uuid.NewString(),
		TransactionID: uuid.NewString(),
		TrafficSource: "COMPLIANCE_PROXY",
		IngressType:   "COMPLIANCE_PROXY",
		// The bump DID happen — this is not BUMP_DISABLED_EMERGENCY. What was
		// skipped is inspection, not interception.
		BumpStatus:            "BUMP_SUCCESS",
		SourceIP:              sourceIP,
		TargetHost:            targetHost,
		Method:                method,
		Path:                  path,
		RequestHookDecision:   "PATH_PASSTHROUGH",
		RequestHookReason:     &reason,
		RequestHookReasonCode: &reasonCode,
		Timestamp:             time.Now().UTC(),
	}

	e.writer.Enqueue(event)
}

// EmitExempted records an audit event for a request exempted from compliance
// hooks. The hookDecision is "EXEMPTED" so dashboards can distinguish these
// from normal APPROVE/REJECT decisions.
func (e *AuditEmitter) EmitExempted(sourceIP, targetHost, exemptionID, exemptionReason string) {
	reason := fmt.Sprintf("temporary exemption %s: %s", exemptionID, exemptionReason)
	reasonCode := "EXEMPTED"

	event := audit.AuditEvent{
		ID:                    uuid.NewString(),
		TransactionID:         uuid.NewString(),
		TrafficSource:         "COMPLIANCE_PROXY",
		IngressType:           "COMPLIANCE_PROXY",
		BumpStatus:            "BUMP_SUCCESS",
		SourceIP:              sourceIP,
		TargetHost:            targetHost,
		RequestHookDecision:   "EXEMPTED",
		RequestHookReason:     &reason,
		RequestHookReasonCode: &reasonCode,
		Timestamp:             time.Now().UTC(),
	}

	e.writer.Enqueue(event)
}
