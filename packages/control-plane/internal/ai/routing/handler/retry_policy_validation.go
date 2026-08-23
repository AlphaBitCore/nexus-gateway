// retry_policy_validation.go — the admin-input bounds on a rule's RetryPolicy.
//
// Separate from the route handlers because it is one concern with one audience:
// what an integrator posting the published contract may set, and the specific
// misconception each refusal names. The handlers call it and do nothing else
// with it.
package routing

import (
	"fmt"
	"time"

	"github.com/goccy/go-json"

	cfgpolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/policy"
)

// validRetryOnClasses enumerates the acceptable RetryOn enum values per
// design spec §6.2. Kept in sync with configtypes.ErrorClass*.
var validRetryOnClasses = map[cfgpolicy.ErrorClass]struct{}{
	cfgpolicy.ErrorClassNetwork: {},
	cfgpolicy.ErrorClassTimeout: {},
	cfgpolicy.ErrorClassRate429: {},
	cfgpolicy.ErrorClass5xx:     {},
}

// validateRetryPolicyJSON enforces the admin-input bounds on a RetryPolicy
// before it is persisted. raw == nil or `null` is allowed (means "clear /
// inherit YAML default"). Returns ("", true) when valid; (msg, false)
// otherwise.
//
// Every field is bounded here, including the three backoff fields the admin UI
// does not surface. Reaching the API is enough: an integrator posting the
// published contract can set them, and nothing downstream would catch a value
// that disables backoff or stalls the walk.
func validateRetryPolicyJSON(raw json.RawMessage) (string, bool) {
	s := string(raw)
	if len(raw) == 0 || s == "null" {
		return "", true
	}
	var p cfgpolicy.RetryPolicy
	if err := json.Unmarshal(raw, &p); err != nil {
		return fmt.Sprintf("retryPolicy is not valid JSON: %v", err), false
	}
	if p.MaxAttemptsPerTarget != 0 {
		if p.MaxAttemptsPerTarget < 1 || p.MaxAttemptsPerTarget > 5 {
			return fmt.Sprintf("retryPolicy.maxAttemptsPerTarget must be in [1,5]; got %d", p.MaxAttemptsPerTarget), false
		}
	}
	// Bounded here as well as at read time. The gateway clamps whatever it
	// finds, so an out-of-range value would be silently rewritten into
	// something the admin did not ask for and cannot see — and a caller who
	// wrote 500 expecting 500 upstream calls has a misconception worth naming
	// rather than quietly correcting.
	if p.MaxUpstreamCalls != 0 {
		if p.MaxUpstreamCalls < 1 || p.MaxUpstreamCalls > 20 {
			return fmt.Sprintf("retryPolicy.maxUpstreamCalls must be in [1,20]; got %d", p.MaxUpstreamCalls), false
		}
	}
	// A duration field unmarshals a bare JSON number as NANOSECONDS, so
	// `"maxWalkDuration": 600` — plainly meant as ten minutes — expires the
	// walk before its first dispatch and turns every request on the rule into
	// a 502 with all targets skipped. A negative value passes the merge (it is
	// non-zero) and disables the deadline entirely. Both are refused rather
	// than corrected: the caller who wrote 600 has a misconception worth
	// naming, and silently reading it as ten minutes would be guessing.
	if p.MaxWalkDuration != 0 {
		if p.MaxWalkDuration < time.Second || p.MaxWalkDuration > time.Hour {
			return fmt.Sprintf("retryPolicy.maxWalkDuration must be between 1s and 1h; got %s "+
				"(a bare number is read as nanoseconds — write \"10m\" or 600000000000)",
				p.MaxWalkDuration), false
		}
	}
	// The backoff durations carry the same nanoseconds trap as maxWalkDuration,
	// and it bites in the opposite direction: 250 means 250ns, which is not a
	// pause at all, so a rule written to slow its retries down speeds them up
	// against an upstream that asked to be left alone. Nothing downstream
	// catches it — computeBackoff clamps the doubling at whatever BackoffMax the
	// rule itself carries, so the rule's own value IS the ceiling.
	//
	// Bounded per field and never against each other: a rule may set one and
	// inherit the other from the gateway default, so a pair compared here is not
	// the pair the walk will use.
	for _, f := range []struct {
		name string
		val  time.Duration
	}{
		{"backoffInitial", p.BackoffInitial},
		{"backoffMax", p.BackoffMax},
	} {
		if f.val == 0 {
			continue
		}
		if f.val < time.Millisecond || f.val > time.Minute {
			return fmt.Sprintf("retryPolicy.%s must be between 1ms and 1m; got %s "+
				"(a bare number is read as nanoseconds — write \"250ms\" or 250000000)",
				f.name, f.val), false
		}
	}
	// A negative jitter passes the merge and reads as no jitter at all; above 1
	// the swing exceeds the wait it jitters, so a pause is as likely to be
	// nothing as to be the configured value.
	if p.BackoffJitter < 0 || p.BackoffJitter > 1 {
		return fmt.Sprintf("retryPolicy.backoffJitter must be between 0 and 1; got %v "+
			"(it is a FRACTION of the wait, so 0.2 means +/-20%%)", p.BackoffJitter), false
	}
	for _, cls := range p.RetryOn {
		if _, ok := validRetryOnClasses[cls]; !ok {
			return fmt.Sprintf("retryPolicy.retryOn[]: %q is not a valid error class (allowed: network, timeout, 429, 5xx)", cls), false
		}
	}
	return "", true
}
