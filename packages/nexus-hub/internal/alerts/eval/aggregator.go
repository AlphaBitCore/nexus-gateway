package alerteval

import "time"

// EventSource enumerates which MQ stream an Aggregator subscribes to.
type EventSource string

const (
	SourceAITraffic  EventSource = "ai-traffic"
	SourceCompliance EventSource = "compliance"
	SourceAgent      EventSource = "agent"
	SourceAdminAudit EventSource = "admin-audit"
)

// Aggregator is one rule's evaluator. The Engine routes events to it and
// invokes Tick on a regular cadence. Aggregator implementations own no
// per-target state — they delegate all bookkeeping to *Runtime via the
// helper methods (rt.Window, rt.SetCooldown, etc.).
type Aggregator interface {
	// RuleID matches AlertRule.id (e.g. "hook.reject_rate"). The Engine
	// looks up this rule on every tick to read params and check enabled.
	RuleID() string

	// Sources lists which MQ subjects this aggregator wants events from.
	// The Engine only invokes OnEvent with events whose source matches.
	Sources() []EventSource

	// OnEvent is called for every matching event. Implementations extract
	// the target_key, decide whether the event "counts", and update their
	// internal Window via rt.Window(targetKey, capSeconds).
	OnEvent(rt *Runtime, evt *Event)

	// MinWarmupSec returns how long the aggregator must run before firing
	// is allowed (cold-start gate). Reads rule.Params for dynamic windows.
	// 0 disables the gate.
	MinWarmupSec(params map[string]any) int

	// Tick is invoked every Engine tick (default 5s). Aggregator inspects
	// its window state per target_key, evaluates threshold, and emits
	// Decisions. Engine handles the actual Raiser call (including cooldown
	// gating for Fire decisions).
	Tick(rt *Runtime, params map[string]any, now time.Time) []Decision
}

// DecisionAction is the operation an Aggregator wants the Engine to perform.
type DecisionAction string

const (
	Fire    DecisionAction = "fire"
	Resolve DecisionAction = "resolve"
)

// Decision is one Aggregator's per-tick output for one target_key. The
// Engine turns it into a Raiser.Raise or Raiser.Resolve call, with cooldown
// gating applied to Fire decisions.
type Decision struct {
	Action    DecisionAction
	TargetKey string
	// Severity optionally overrides the rule's DefaultSeverity. Empty string
	// means "use rule.DefaultSeverity".
	Severity string
	Message  string
	Details  map[string]any
}
