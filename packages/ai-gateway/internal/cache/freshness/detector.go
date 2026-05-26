package freshness

import (
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
)

// ChatMessage is the minimal message representation used by the Detector.
//
// Callers project their own canonical message type (e.g. a shared.Message or
// an OpenAI-shaped struct) into ChatMessage before calling IsTimeSensitive.
// This keeps the freshness package free of upstream dependencies.
type ChatMessage struct {
	// Role is the message role: "user", "assistant", "system", "tool", etc.
	Role string
	// Content is the plain-text content of the message.
	Content string
}

// Detector evaluates a conversation's time-sensitivity using a compiled rule
// set. Construct with NewDetector; use IsTimeSensitive on the hot path; use
// Reload to atomically swap in a new rule set without restarting.
//
// A nil *Detector is NOT safe to use. Always construct via NewDetector.
type Detector struct {
	// rules is an atomic pointer to the compiled rule set so Reload can swap
	// it without holding a mutex on the hot path. The pointer value is always
	// non-nil after construction.
	rules atomic.Pointer[ruleSet]

	log     *slog.Logger
	metrics *freshnessMetrics
}

// ruleSet is the immutable compiled version of a []Rule snapshot.
type ruleSet struct {
	compiled []*compiledRule
}

// NewDetector compiles rules and constructs a Detector. Disabled rules are
// excluded from the compiled set. Returns an error if any enabled rule is
// invalid (e.g. empty keywords, blank rule ID).
//
// Parameters:
//   - rules: the initial rule list. Pass nil at boot when no rules are yet
//     known; Reload installs the real list once the Hub shadow arrives.
//   - log: structured logger; must not be nil.
//   - namespace: Prometheus namespace for nexus_cache_freshness_skips_total.
//   - reg: Prometheus registerer. Pass prometheus.NewRegistry() in tests to
//     isolate from the default registry; pass nil to use prometheus.DefaultRegisterer.
func NewDetector(rules []Rule, log *slog.Logger, namespace string, reg prometheus.Registerer) (*Detector, error) {
	if log == nil {
		return nil, fmt.Errorf("freshness.NewDetector: logger must not be nil")
	}
	if namespace == "" {
		return nil, fmt.Errorf("freshness.NewDetector: namespace must not be empty")
	}
	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}

	compiled, err := compileAll(rules)
	if err != nil {
		return nil, fmt.Errorf("freshness.NewDetector: %w", err)
	}

	d := &Detector{
		log:     log,
		metrics: newFreshnessMetrics(namespace, reg),
	}
	d.rules.Store(&ruleSet{compiled: compiled})
	return d, nil
}

// IsTimeSensitive returns (true, ruleID) when the first matching rule fires for
// the last user-role message in messages. Returns (false, "") when no rule
// fires or when there is no user message in the conversation.
//
// The method is safe for concurrent use: it reads an immutable ruleSet snapshot
// from an atomic pointer, so a concurrent Reload does not require locking.
//
// Algorithm:
//  1. Extract the last message where Role == "user".
//  2. For each compiled rule, call compiledRule.matches(text).
//  3. On first match: increment the Prometheus counter and return (true, ruleID).
//  4. Default: return (false, "").
func (d *Detector) IsTimeSensitive(messages []ChatMessage) (matched bool, ruleID string) {
	text := lastUserText(messages)
	if text == "" {
		return false, ""
	}

	rs := d.rules.Load()
	for _, cr := range rs.compiled {
		if cr.matches(text) {
			lang := ruleLanguageLabel(cr.rule.Languages)
			d.metrics.recordSkip(cr.rule.ID, lang)
			d.log.Debug("freshness detector matched",
				"rule_id", cr.rule.ID,
				"language", lang,
			)
			return true, cr.rule.ID
		}
	}
	return false, ""
}

// Reload atomically replaces the active rule set. The swap is lock-free on the
// read path: IsTimeSensitive callers that are mid-evaluation continue to use
// the previous snapshot; new callers pick up the new snapshot immediately after
// Store returns.
//
// Reload returns an error if any enabled rule in rules is invalid. On error the
// existing rule set is left intact.
func (d *Detector) Reload(rules []Rule) error {
	compiled, err := compileAll(rules)
	if err != nil {
		return fmt.Errorf("freshness.Detector.Reload: %w", err)
	}
	d.rules.Store(&ruleSet{compiled: compiled})
	d.log.Info("freshness detector rules reloaded", "count", len(compiled))
	return nil
}

// lastUserText extracts the plain-text content of the last message whose Role
// is "user" (case-insensitive). Returns "" when no such message exists.
//
// Only the last user message is evaluated because it reflects the current
// intent. Earlier turns may have used time-sensitive keywords in a context
// that has since resolved (e.g. "Back then, what was the stock price?").
func lastUserText(messages []ChatMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if strings.EqualFold(messages[i].Role, "user") {
			return messages[i].Content
		}
	}
	return ""
}
