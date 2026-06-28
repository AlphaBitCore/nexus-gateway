package core

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/decision"
)

// legacyOnMatchWarnOnce ensures the deprecation warning for the old
// inflightAction/storageAction keys is emitted at most once per process.
var legacyOnMatchWarnOnce sync.Once

// ParseOnMatch reads the `onMatch` block from a hook's declarative
// configuration map and returns a validated OnMatchConfig. The shape
// every content-touching hook reads (pii-detector / keyword-filter /
// content-safety / rulepack-engine / quality-checker / webhook-forward /
// aiguard once re-platformed).
//
// The current shape is a single `action` key (approve | redact | block):
//   - approve: forward / return unchanged, store as-is
//   - redact:  rewrite the payload; the same masked body is forwarded,
//     returned, and stored
//   - block:   reject (403 proxy / connection drop agent) and store the
//     redacted copy
//
// Defaults when `action` is absent:
//   - action      = "block"  (preserves the "block on match" security default
//     for match-only hooks like pii-detector / keyword-filter / content-safety)
//   - replacement = "[REDACTED_<RULE_ID>]"
//
// Hook-specific override: `webhook-forward` re-derives its own action default
// to `approve` when the admin did not supply an explicit `onMatch.action`,
// because the webhook's reply IS the decision (advisory ceiling, not
// enforcement). See packages/shared/policy/hooks/webhook/webhook.go.
//
// Deprecation window: when `action` is absent but the legacy
// `inflightAction` / `storageAction` keys are present, they are mapped to the
// single action via decision.ActionFromLegacy and a one-shot deprecation
// warning is logged. The legacy keys are removed from the schema after the
// window.
//
// Returns an error when:
//   - cfg["onMatch"] is present but not a map
//   - `action` is non-empty and not in {approve, redact, block}
//   - a present legacy key is non-empty and not in its allowed set
func ParseOnMatch(cfg map[string]any) (OnMatchConfig, error) {
	out := OnMatchConfig{
		Action:      ActionBlock,
		Replacement: "[REDACTED_<RULE_ID>]",
	}
	raw, ok := cfg["onMatch"]
	if !ok || raw == nil {
		return out, nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return out, fmt.Errorf("onMatch must be an object, got %T", raw)
	}

	if v, ok := m["action"].(string); ok && v != "" {
		a := decision.Action(strings.ToLower(strings.TrimSpace(v)))
		if !a.Valid() {
			return out, fmt.Errorf("onMatch.action: unknown action %q (expected approve|redact|block)", v)
		}
		out.Action = a
	} else if action, mapped, err := parseLegacyOnMatch(m); err != nil {
		return out, err
	} else if mapped {
		out.Action = action
	}

	if v, ok := m["replacement"].(string); ok && v != "" {
		out.Replacement = v
	}
	return out, nil
}

// parseLegacyOnMatch maps the deprecated inflightAction/storageAction pair to
// the single Action for the back-compat window. Returns mapped=false when
// neither legacy key is present (so the caller keeps the block default).
func parseLegacyOnMatch(m map[string]any) (action decision.Action, mapped bool, err error) {
	inflightRaw, hasInflight := m["inflightAction"].(string)
	storageRaw, hasStorage := m["storageAction"].(string)
	if (!hasInflight || inflightRaw == "") && (!hasStorage || storageRaw == "") {
		return "", false, nil
	}
	var inflight InflightAction
	var storage StorageAction
	if inflightRaw != "" {
		inflight, err = parseInflightAction(inflightRaw)
		if err != nil {
			return "", false, fmt.Errorf("onMatch.inflightAction: %w", err)
		}
	}
	if storageRaw != "" {
		storage, err = parseStorageAction(storageRaw)
		if err != nil {
			return "", false, fmt.Errorf("onMatch.storageAction: %w", err)
		}
	}
	legacyOnMatchWarnOnce.Do(func() {
		slog.Warn("hook onMatch uses deprecated inflightAction/storageAction keys; migrate to a single action (approve|redact|block)",
			"inflightAction", inflightRaw, "storageAction", storageRaw)
	})
	return decision.ActionFromLegacy(inflight, storage), true, nil
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

// LabelForDecision maps a Decision back to the admin-configured action
// vocabulary (block-hard / block-soft / redact / approve) used in hook YAML.
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

// DecisionForAction maps the single Action axis to the Decision enum used
// by the pipeline and downstream dispatch:
//
//	approve → Approve    (forward / return unchanged)
//	redact  → Modify     (rewrite the payload inflight)
//	block   → RejectHard (reject the transaction)
//
// An unrecognised action defaults to RejectHard (fail-closed).
func DecisionForAction(a decision.Action) Decision {
	switch a {
	case decision.ActionApprove:
		return Approve
	case decision.ActionRedact:
		return Modify
	case decision.ActionBlock:
		return RejectHard
	}
	return RejectHard
}

// ActionFromDecision is the inverse of DecisionForAction: it folds the
// internal Decision enum back to the single Action axis.
//
//	RejectHard / BlockSoft → block
//	Modify                 → redact
//	Approve / Abstain      → approve
//
// Used at the pipeline boundary so the aggregated CompliancePipelineResult
// carries a single Action driving both inflight dispatch and storage.
func ActionFromDecision(d Decision) decision.Action {
	switch d {
	case RejectHard, BlockSoft:
		return decision.ActionBlock
	case Modify:
		return decision.ActionRedact
	}
	return decision.ActionApprove
}

// StrictestAction picks the more-aggressive action between two actions,
// ranked block > redact > approve > "". Used to aggregate per-hook actions
// into the pipeline-level result.
func StrictestAction(a, b decision.Action) decision.Action {
	rank := func(x decision.Action) int {
		switch x {
		case decision.ActionBlock:
			return 3
		case decision.ActionRedact:
			return 2
		case decision.ActionApprove:
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
// OnMatchConfig.Action via DecisionForAction.
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
