// Package classify derives per-flow upload classifications from audit event fields.
package classify

import (
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/audit/event"
)

// Classification is the user-facing status assigned to a captured flow.
// Derived from the four orthogonal audit fields (DomainRuleID, PathAction,
// HookDecision, BumpStatus, ErrorCode) so the agent UI and the upload
// filter speak the same vocabulary.
//
// The mapping intentionally collapses Untracked + Inspect into "kept
// local-only at the default upload level" so the Hub-side traffic_event
// table only carries flows where the admin's interception_domain config
// actively did something.
type Classification string

const (
	// ClassUntracked means the agent saw the flow but the host did NOT
	// match any interception_domain row. No domain rule applied; no
	// hook ran; no bump occurred. Local-only at default level.
	ClassUntracked Classification = "untracked"

	// ClassInspect means the host matched an interception_domain but
	// the resolved per-path / per-domain action was PASSTHROUGH, so
	// hooks did NOT run. The flow may have been TLS-bumped (forward
	// handler still saw the HTTP layer) but the admin asked us not to
	// process it. Local-only at default level.
	ClassInspect Classification = "inspect"

	// ClassProcessed means the host matched an interception_domain,
	// the path action resolved to PROCESS, and hooks ran with an
	// approve outcome. The pre/post bytes were exposed to the hook
	// pipeline. Always uploaded to Hub.
	ClassProcessed Classification = "processed"

	// ClassBlocked means hooks ran and rejected the flow (REJECT_HARD
	// or BLOCK_SOFT). Compliance evidence — always uploaded.
	ClassBlocked Classification = "blocked"

	// ClassBumpFailed means the agent attempted to TLS-bump (the
	// domain matched + path needed processing) but bumping failed
	// — most often because the upstream pinned a cert or the
	// client rejected our leaf. Hooks could not run; the flow was
	// raw-relayed. Always uploaded so admins can see when an
	// inspect target is silently bypassing the gateway.
	ClassBumpFailed Classification = "bump_failed"
)

// Classify derives the Classification from the captured event's fields.
//
// Decision tree (first match wins):
//
//  1. DomainRuleID empty             → Untracked (host wasn't in admin's
//     interception_domain table — no policy ever applied).
//  2. ErrorCode set OR BumpStatus contains FAILED/PASSTHROUGH while
//     DomainRuleID is set             → BumpFailed (we wanted to inspect
//     but the bump didn't take; usually anti-pinning).
//  3. HookDecision is reject_hard /
//     block_soft / deny               → Blocked.
//  4. HookDecision is approve         → Processed (hook pipeline ran
//     successfully).
//  5. PathAction == PASSTHROUGH       → Inspect (matched + admin asked
//     to skip hooks).
//  6. fallthrough                     → Inspect (matched but neither
//     PROCESS nor explicit hook outcome — treat as bumped-but-no-hook).
//
// The ordering matters: BumpFailed beats hook outcome (a bump failure
// means hooks could not have run at all even if HookDecision was
// stamped from a stale earlier branch).
func Classify(e event.Event) Classification {
	if e.DomainRuleID == "" {
		return ClassUntracked
	}
	// Bump failure beats every other signal — a non-bumped flow can't
	// have run hooks regardless of what HookDecision says.
	if e.ErrorCode != "" {
		return ClassBumpFailed
	}
	switch e.BumpStatus {
	case "BUMP_FAILED", "BUMP_FAILED_PASSTHROUGH":
		return ClassBumpFailed
	}
	// hooks.Decision constants are stored UPPER-CASE on the wire
	// ("APPROVE" / "REJECT_HARD" / "BLOCK_SOFT") because that's how
	// shared/hooks/types.go defines them. classify.ts (TS mirror)
	// also lowercases before switch — keep parity here so the Go-
	// side ShouldUpload + the TS-side render badge both classify
	// the same row identically. Pre-fix this was lower-case-only
	// and uploaded "APPROVE" rows as Inspect → never reached Hub
	// at default "processed" upload level.
	switch strings.ToLower(e.HookDecision) {
	case "reject_hard", "block_soft", "deny":
		return ClassBlocked
	case "approve":
		return ClassProcessed
	}
	if e.Action == "deny" {
		return ClassBlocked
	}
	// PathAction PASSTHROUGH explicitly means admin asked us to skip
	// hooks; otherwise we fell through with no hook output for an
	// unknown reason — both surface as Inspect.
	return ClassInspect
}

// ShouldUpload returns true when an event with the given classification
// should be uploaded to Hub at the configured trafficUploadLevel.
//
// Levels:
//   - "all"       — Untracked + Inspect + Processed + Blocked + BumpFailed
//   - "processed" — Processed + Blocked + BumpFailed (default)
//   - "blocked"   — Blocked + BumpFailed only
//
// BumpFailed is included in "blocked" because a bump failure on a
// configured inspect target IS a compliance signal — operators need to
// see when an admin-targeted host is silently dodging the gateway.
func ShouldUpload(c Classification, level string) bool {
	switch level {
	case "all":
		return true
	case "blocked":
		return c == ClassBlocked || c == ClassBumpFailed
	default: // "processed" + any unknown value
		return c == ClassProcessed || c == ClassBlocked || c == ClassBumpFailed
	}
}
