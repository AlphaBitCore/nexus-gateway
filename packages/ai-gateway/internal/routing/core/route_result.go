package core

import "github.com/goccy/go-json"

// RouteResult is the output of Resolver.ResolveTargets.
// Targets is a flat ordered list: primary + fallback + recovery, already filtered and health-ranked.
type RouteResult struct {
	// Dispatch is the order the walk will try, health-ranked across the whole
	// plan. Role — the strategy's answer, the rule's chain, a lower-priority
	// rule's — travels on each target as Source, which the audit projection
	// reads. No accessor splits the list by role: two were written, called by
	// nothing, and deleted.
	//
	// Stage D of the target design specified two stored lists rather than one
	// list with a role label. That does not survive contact with health
	// ordering: a primary on a provider that is answering nothing has to be
	// demoted BELOW the chain, and a boundary between two stored lists is
	// exactly what a cross-boundary demotion cannot cross. Storing both an
	// order and a boundary means storing the same plan twice, and the two
	// disagree the moment anything reorders.
	//
	// So the plan stores the order and carries the role on each target. Role
	// views were offered as accessors too and deleted unused — what the design
	// was really protecting is preserved by Primary/AllTargets forcing a caller
	// to say whether it wants the decision or everything, and by Promote/Narrow
	// being the only ways to change the list. A reader that genuinely needs one
	// role reads Source, as the audit projection does.
	Dispatch        []RoutingTarget
	Trace           []TraceEntry
	PipelineTrace   []PipelineTraceEntry
	RuleID          string
	RuleName        string
	Substituted     bool   // true when the routing engine replaced the requested model
	OriginalModelID string // model ID as requested by the client before substitution
	// Requested-side identity for the traffic_event REQUESTED columns
	// (model_id / provider_id / provider_name). Populated ONLY when the
	// client asked for a specific model that resolves unambiguously to
	// exactly one catalog model (not "auto", not a code that fans out to
	// multiple providers). Empty otherwise — for "auto" / multi-candidate /
	// unresolved the client did not pin a model, so the requested columns
	// stay NULL and the routed_* columns carry the actually-served target.
	RequestedModelID      string
	RequestedProviderID   string
	RequestedProviderName string

	// RuleRetryPolicyJSON carries the matched primary rule's
	// RoutingRule.retryPolicy JSONB column verbatim (may be empty/null).
	// The proxy handler field-merges it on top of cfg.Routing.DefaultRetryPolicy
	// before passing the effective policy to the executor.
	RuleRetryPolicyJSON json.RawMessage

	// RuleMatchedAndResolvedNothing carries the plan's field of the same name to
	// the handler, which needs it to decide whether the passthrough's
	// precondition holds.
	RuleMatchedAndResolvedNothing bool
}

// Narrow keeps only the given targets, in the order they are given.
//
// The wire-compatibility filter is the caller: an ingress format the target's
// adapter cannot serve is not a routing decision to revisit, it is a target
// that cannot be dispatched at all. Narrowing through a method rather than
// assigning the slice keeps the plan's shape known in one place — a caller that
// rebuilds the list itself is a second assembler of the thing the pipeline just
// produced, and the two diverge silently because both are lists of targets.
func (r *RouteResult) Narrow(kept []RoutingTarget) { r.Dispatch = kept }

// Primary is the target the request will actually reach first: the strategy's
// answer when it has one, and the chain's head when every ranked target was
// filtered away. Returns the zero value on an empty plan, which every caller
// already guards for.
func (r *RouteResult) Primary() RoutingTarget {
	if len(r.Dispatch) > 0 {
		return r.Dispatch[0]
	}
	return RoutingTarget{}
}

// Promote moves a target to the head of the ranked answer, keeping every other
// target — from either list — behind it in its existing order.
//
// The quota downgrade is the caller: a key near its cap is served the cheapest
// affordable model the plan can reach, which is frequently an entry from the
// rule's chain rather than the strategy's first pick. Promoting rather than
// truncating is deliberate; an earlier version cut the plan down to the single
// downgraded target and made the next transient failure terminal, which is a
// second penalty for being near a quota that nobody chose to impose.
//
// It lives here because the plan's shape is the pipeline's to know. The
// downgrade used to rebuild the flat list itself, which made it a second
// constructor of the thing the pipeline had just assembled — the divergence
// that produces is silent, because both versions look like a list of targets.
func (r *RouteResult) Promote(t RoutingTarget) {
	out := make([]RoutingTarget, 0, len(r.Dispatch))
	out = append(out, t)
	for _, x := range r.Dispatch {
		if x.ModelID != t.ModelID {
			out = append(out, x)
		}
	}
	r.Dispatch = out
}

// AllTargets is the dispatch order: the strategy's answer, then the rule's
// chain. Callers that genuinely walk everything use this; callers that want
// only the decision use Primary, and the compiler makes them say which.
func (r *RouteResult) AllTargets() []RoutingTarget { return r.Dispatch }
