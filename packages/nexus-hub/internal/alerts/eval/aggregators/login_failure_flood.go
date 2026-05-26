package aggregators

import (
	"fmt"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/eval"
)

// LoginFailureFlood fires when the count of admin.login.failed audit rows
// over params.windowSec, grouped by params.groupBy (ip|email|all), reaches
// params.thresholdCount.
type LoginFailureFlood struct{}

// NewLoginFailureFlood constructs the aggregator.
func NewLoginFailureFlood() *LoginFailureFlood { return &LoginFailureFlood{} }

// RuleID returns the AlertRule.id this aggregator implements.
func (a *LoginFailureFlood) RuleID() string { return "auth.login_failure_rate" }

// Sources lists the MQ subjects this aggregator subscribes to.
func (a *LoginFailureFlood) Sources() []alerteval.EventSource {
	return []alerteval.EventSource{alerteval.SourceAdminAudit}
}

// MinWarmupSec returns the cold-start gate duration.
func (a *LoginFailureFlood) MinWarmupSec(params map[string]any) int {
	return intParam(params, "windowSec", 300)
}

// OnEvent updates the per-group window for admin.login.failed events only.
// Hardcoded params handling: extracts groupBy at event-arrival time from a
// stash on Runtime is brittle; instead we redo the params read on every
// event (cheap — params is already parsed). Engine guarantees the same
// params snapshot is used in OnEvent and Tick within a single tick window
// because OnEvent doesn't read params (we hardcode the field used) — the
// groupBy choice is read inside Tick. To keep OnEvent symmetric, we tag
// the event with a per-target_key derived from each possible groupBy, then
// Tick decides which subset to inspect.
//
// Simpler approach (chosen): OnEvent indexes by all 3 possible groupBy
// keys (ip / email / all). At Tick time we compute targets matching the
// configured groupBy. The cost is 3x bucket writes per event, which is
// negligible (login events are very low volume).
func (a *LoginFailureFlood) OnEvent(rt *alerteval.Runtime, evt *alerteval.Event) {
	if evt.Kind != alerteval.EventAudit || evt.Audit == nil {
		return
	}
	if evt.Audit.Action != "admin.login.failed" {
		return
	}
	// Cap at 3600s so the window accommodates any reasonable windowSec param value.
	w := rt.Window("login:all", 3600)
	w.Add(evt.Timestamp, 1, 0)
	if ip := evt.Audit.SourceIP; ip != "" {
		w := rt.Window("login:ip:"+ip, 3600)
		w.Add(evt.Timestamp, 1, 0)
	}
	if email := evt.Audit.ActorLabel; email != "" {
		w := rt.Window("login:email:"+email, 3600)
		w.Add(evt.Timestamp, 1, 0)
	}
}

// Tick evaluates the rule for targets matching the configured groupBy.
func (a *LoginFailureFlood) Tick(rt *alerteval.Runtime, params map[string]any, now time.Time) []alerteval.Decision {
	windowSec := intParam(params, "windowSec", 300)
	thresholdCount := intParam(params, "thresholdCount", 20)
	groupBy := stringParam(params, "groupBy", "ip")

	var prefix string
	switch groupBy {
	case "all":
		prefix = "login:all"
	case "email":
		prefix = "login:email:"
	default: // "ip"
		prefix = "login:ip:"
	}

	var out []alerteval.Decision
	for _, target := range rt.Targets() {
		matchesAll := groupBy == "all" && target == prefix
		matchesPrefix := groupBy != "all" && len(target) > len(prefix) && target[:len(prefix)] == prefix
		if !matchesAll && !matchesPrefix {
			continue
		}
		msg := fmt.Sprintf("Login failure flood: %d failed logins / %ds (groupBy=%s)", thresholdCount, windowSec, groupBy)
		if d := EvalCountInWindow(rt, target, windowSec, thresholdCount, now, msg); d != nil {
			out = append(out, *d)
		}
	}
	return out
}
