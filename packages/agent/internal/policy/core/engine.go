// Package core implements domain-based policy evaluation.
//
// Post-PR-0 the engine no longer carries an admin-pushed rule set: the
// policy_rules shadow key was retired with no live publisher. Two paths
// remain that decide whether a flow is inspected:
//
//   - Exemption check (when an exemption.Store is attached): exempt hosts
//     short-circuit to passthrough so cert-pin loops don't keep tripping
//     the bump.
//   - Interception-domains fallback (when an interception_domains snapshot
//     callback is wired): hosts matching the admin-pushed Cat B
//     interception list return "inspect" so the daemon TLS-bumps the flow
//     and downstream per-path matching can take over.
//
// Anything else returns the engine's defaultAction (typically "passthrough"
// for the agent — the safe-default fail-open path).
package core

import (
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/policy/exemption"
)

// PolicyResult contains the evaluation outcome.
type PolicyResult struct {
	Action         string
	Matched        bool
	MatchedPattern string
	MatchedIndex   int
}

// Engine evaluates domain names against the exemption store + the
// interception-domains fallback.
type Engine struct {
	defaultAction  string
	exemptionStore atomic.Pointer[exemption.Store]
	// interceptionHostsFn returns the current list of host patterns
	// pulled from the interception_domains shadow key. When set, any
	// host matching one of these patterns returns the "inspect"
	// decision so the daemon TLS-bumps the flow and InspectRequest
	// can do per-path matching. Without this hook the domain config
	// pushed by Hub is purely cosmetic. Set to nil in tests /
	// pre-Hub-rollout to disable the fallback.
	interceptionHostsFn atomic.Value // func() []string
}

// SetExemptionStore attaches an exemption store. When set, the engine will
// check the store first and return passthrough for any exempted host.
// Safe for concurrent use.
func (e *Engine) SetExemptionStore(s *exemption.Store) {
	e.exemptionStore.Store(s)
}

// SetInterceptionHostsFn registers a callback returning the current
// host patterns from the interception_domains shadow key. Engine
// returns "inspect" for any host matching one of those patterns.
// Pass nil to clear (disables the fallback). Safe for concurrent use.
func (e *Engine) SetInterceptionHostsFn(fn func() []string) {
	if fn == nil {
		e.interceptionHostsFn.Store((func() []string)(nil))
		return
	}
	e.interceptionHostsFn.Store(fn)
}

// NewEngine creates a policy engine with the given default action.
func NewEngine(defaultAction string) *Engine {
	return &Engine{defaultAction: defaultAction}
}

// Evaluate checks a hostname:
//
//  1. Exemption store hit -> passthrough.
//  2. Interception-domains hit -> inspect.
//  3. Otherwise -> defaultAction.
func (e *Engine) Evaluate(host string) PolicyResult {
	if es := e.exemptionStore.Load(); es != nil {
		if exempt, reason := es.IsExempt(host); exempt {
			return PolicyResult{
				Action:         "passthrough",
				Matched:        true,
				MatchedPattern: "exempt:" + reason,
				MatchedIndex:   -1,
			}
		}
	}
	if v := e.interceptionHostsFn.Load(); v != nil {
		if fn, ok := v.(func() []string); ok && fn != nil {
			for _, p := range fn() {
				if p == "" {
					continue
				}
				if matchGlob(p, host) {
					return PolicyResult{
						Action:         "inspect",
						Matched:        true,
						MatchedPattern: "interception_domain:" + p,
						MatchedIndex:   -1,
					}
				}
			}
		}
	}
	return PolicyResult{Action: e.defaultAction, Matched: false}
}

func matchGlob(pattern, host string) bool {
	if pattern == host {
		return true
	}
	// Domain-aware wildcard: *.example.com matches sub.example.com
	// AND deep.sub.example.com (multi-level subdomains).
	if strings.HasPrefix(pattern, "*.") {
		suffix := pattern[1:] // ".example.com"
		return strings.HasSuffix(host, suffix) && len(host) > len(suffix)
	}
	// Fall back to Go's filepath.Match for other patterns
	matched, err := filepath.Match(pattern, host)
	if err == nil && matched {
		return true
	}
	return false
}
