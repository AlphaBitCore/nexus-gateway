package core

import (
	"fmt"
	"strings"
)

// ParseOnMatch reads the `onMatch` block from a hook's declarative
// configuration map and returns a validated OnMatchConfig. The shape
// every content-touching hook reads (pii-detector / keyword-filter /
// content-safety / rulepack-engine / quality-checker / webhook-forward /
// aiguard once re-platformed).
//
// Defaults when fields are absent:
//   - inflightAction = "block-hard"  (preserves the "block on match" default
//     for match-only hooks like pii-detector / keyword-filter / content-safety
//     where match → block is the right security default)
//   - storageAction  = "redact"       (compliance-default: never persist
//     sensitive content unless operator opts in)
//   - replacement    = "[REDACTED_<RULE_ID>]"
//
// Hook-specific override: `webhook-forward` re-derives its own
// inflightAction default to `approve` when the admin did not supply an
// explicit `onMatch.inflightAction` key, because the webhook's reply IS
// the decision (advisory ceiling, not enforcement). Admins who want
// webhook-bounded-by-ceiling behaviour opt in via an explicit value.
// See packages/shared/policy/hooks/webhook/webhook.go.
//
// Both axes accept the closed string sets defined by InflightAction /
// StorageAction.
//
// Returns an error when:
//   - cfg["onMatch"] is present but not a map
//   - inflightAction / storageAction is non-empty and not in the allowed set
//
// Absent `onMatch` block returns the defaults silently — backwards
// compatible bring-up; existing seeds without onMatch behave like
// "block-hard + redact-storage" (except webhook-forward, see above).
func ParseOnMatch(cfg map[string]any) (OnMatchConfig, error) {
	out := OnMatchConfig{
		InflightAction: InflightBlockHard,
		StorageAction:  StorageRedact,
		Replacement:    "[REDACTED_<RULE_ID>]",
	}
	raw, ok := cfg["onMatch"]
	if !ok || raw == nil {
		return out, nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return out, fmt.Errorf("onMatch must be an object, got %T", raw)
	}
	if v, ok := m["inflightAction"].(string); ok && v != "" {
		action, err := parseInflightAction(v)
		if err != nil {
			return out, fmt.Errorf("onMatch.inflightAction: %w", err)
		}
		out.InflightAction = action
	}
	if v, ok := m["storageAction"].(string); ok && v != "" {
		action, err := parseStorageAction(v)
		if err != nil {
			return out, fmt.Errorf("onMatch.storageAction: %w", err)
		}
		out.StorageAction = action
	}
	if v, ok := m["replacement"].(string); ok && v != "" {
		out.Replacement = v
	}
	return out, nil
}

func parseInflightAction(s string) (InflightAction, error) {
	switch strings.ToLower(s) {
	case string(InflightApprove):
		return InflightApprove, nil
	case string(InflightBlockHard):
		return InflightBlockHard, nil
	case string(InflightBlockSoft):
		return InflightBlockSoft, nil
	case string(InflightRedact):
		return InflightRedact, nil
	}
	return "", fmt.Errorf("unknown inflightAction %q (expected approve|block-hard|block-soft|redact)", s)
}

func parseStorageAction(s string) (StorageAction, error) {
	switch strings.ToLower(s) {
	case string(StorageKeep):
		return StorageKeep, nil
	case string(StorageRedact):
		return StorageRedact, nil
	case string(StorageDropContent):
		return StorageDropContent, nil
	}
	return "", fmt.Errorf("unknown storageAction %q (expected keep|redact|drop-content)", s)
}

// DecisionForInflight maps an InflightAction to the Decision enum.
// Used by content-touching hooks to translate operator policy into
// the pipeline's decision vocabulary on a match.
func DecisionForInflight(a InflightAction) Decision {
	switch a {
	case InflightApprove:
		return Approve
	case InflightBlockHard:
		return RejectHard
	case InflightBlockSoft:
		return BlockSoft
	case InflightRedact:
		return Modify
	}
	return RejectHard
}

// LabelForDecision is the inverse of DecisionForInflight: it maps a
// Decision back to the admin-configured InflightAction vocabulary
// (block-hard / block-soft / redact / approve) used in hook YAML.
//
// Used at reconcile / merge sites that render an operator-facing string
// describing the decision in the same language the operator wrote in
// the hook config. Mixing the internal Decision enum (`reject_hard`)
// with the YAML inflight strings (`block-hard`) in the same sentence
// confuses operators triaging an audit row.
//
// Falls back to the lowercased Decision string for any decision outside
// the reconcile-applicable set (Abstain or an unrecognised value).
func LabelForDecision(d Decision) string {
	switch d {
	case RejectHard:
		return string(InflightBlockHard)
	case BlockSoft:
		return string(InflightBlockSoft)
	case Modify:
		return string(InflightRedact)
	case Approve:
		return string(InflightApprove)
	}
	return strings.ToLower(string(d))
}

// ResolveReplacement returns the Replacement template with <RULE_ID>
// substituted for the supplied rule id. The default template is
// "[REDACTED_<RULE_ID>]"; operators can override with any string.
func ResolveReplacement(template, ruleID string) string {
	if template == "" {
		template = "[REDACTED_<RULE_ID>]"
	}
	return strings.ReplaceAll(template, "<RULE_ID>", strings.ToUpper(ruleID))
}

// StrictestStorageAction picks the more-aggressive storage policy
// between two StorageActions. Ordering: drop-content > redact > keep > "".
// Used to aggregate per-hook policies into the pipeline-level
// CompliancePipelineResult.StorageAction.
func StrictestStorageAction(a, b StorageAction) StorageAction {
	rank := func(s StorageAction) int {
		switch s {
		case StorageDropContent:
			return 3
		case StorageRedact:
			return 2
		case StorageKeep:
			return 1
		}
		return 0
	}
	if rank(a) >= rank(b) {
		return a
	}
	return b
}

// StrictestDecision picks the more-restrictive decision between two
// Decisions, used at the AI-Guard reconcile site where a webhook's
// suggested decision is bounded by the admin policy ceiling derived from
// OnMatchConfig.InflightAction via DecisionForInflight.
//
// Ordering (least → most restrictive):
//
//	Abstain (no opinion — strictness defers to the other side)
//	Approve
//	Modify
//	BlockSoft
//	RejectHard
//
// Rationale: Approve lets traffic through unchanged; Modify rewrites
// content inflight (the request still completes, just with a modified
// body); BlockSoft stops the request from reaching the upstream but
// returns a soft-block response to the caller (more disruptive than a
// silent rewrite — the caller sees the block); RejectHard stops the
// request entirely. Abstain ranks at 0 so Strictest(Abstain, X) == X.
//
// The BlockSoft > Modify ordering matches the pipeline aggregator in
// packages/shared/policy/pipeline/pipeline.go (its mergeResults prefers
// hasSoftReject over hasModify when both fire), so reconcile and
// aggregation agree on relative strictness.
//
// When the two arguments tie in rank, the first argument wins — the
// reconcile site passes the webhook's suggestion first so a tie does
// not gratuitously rewrite the decision label.
func StrictestDecision(a, b Decision) Decision {
	rank := func(d Decision) int {
		switch d {
		case RejectHard:
			return 4
		case BlockSoft:
			return 3
		case Modify:
			return 2
		case Approve:
			return 1
		}
		// Abstain and any unrecognised value rank at 0.
		return 0
	}
	if rank(a) >= rank(b) {
		return a
	}
	return b
}
