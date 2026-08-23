package policy

import "time"

// ErrorClass identifies why an attempt failed, for retry classification.
type ErrorClass string

const (
	ErrorClassNetwork ErrorClass = "network"
	ErrorClassTimeout ErrorClass = "timeout"
	ErrorClassRate429 ErrorClass = "429"
	ErrorClass5xx     ErrorClass = "5xx"
)

// RetryPolicy controls per-target retry behavior. Rule overrides field-merge
// with the YAML default; absent fields fall back, and a length-0 RetryOn
// slice is honored as "retry nothing" (vs nil which means "fall back").
type RetryPolicy struct {
	// MaxAttemptsPerTarget is how many times ONE target may be dispatched to on
	// this REQUEST — not within one turn of the walk.
	//
	// Two mechanisms try a target again: the retry loop inside a turn, and the
	// walk coming back to it later. They answer different questions, so both
	// exist; they draw from this ONE allowance, so a turn granted later cannot
	// hand a target a fresh one. With the limit living inside the retry loop, a
	// rule capped at two dispatched four times to a single provider and the
	// request-level budget silently outranked this number.
	MaxAttemptsPerTarget int           `json:"maxAttemptsPerTarget,omitempty" yaml:"maxAttemptsPerTarget"`
	RetryOn              []ErrorClass  `json:"retryOn,omitempty" yaml:"retryOn"`
	BackoffInitial       time.Duration `json:"backoffInitial,omitempty" yaml:"backoffInitial"`
	BackoffMax           time.Duration `json:"backoffMax,omitempty" yaml:"backoffMax"`
	BackoffJitter        float64       `json:"backoffJitter,omitempty" yaml:"backoffJitter"`

	// MaxUpstreamCalls bounds how many upstream calls ONE request may make,
	// counting every dispatch across every target and every rule — including
	// the same-target attempts MaxAttemptsPerTarget governs.
	//
	// Calls are the unit because calls are what cost money. Counting rounds
	// instead would make one configured number mean wildly different spend
	// depending on how many targets and fallback entries a rule happens to
	// carry, and exempting same-target attempts would let a budget of 1 permit
	// five billed generations — defeating the only knob that bounds spend.
	//
	// Zero means unset, and unset is NOT a constant: see EffectiveCallBudget.
	// The derived value is also the most the walk can spend at ANY setting,
	// because each target's allowance is its own and no other target can spend
	// what it leaves behind. A set value takes attempts away; a value above the
	// derived ceiling buys nothing. To grant a target more attempts, raise
	// MaxAttemptsPerTarget.
	//
	// Not surfaced in the admin UI. That form already carries a "max attempts"
	// input, and a second similar-sounding number beside it is where the two
	// get confused; the backoff fields set the precedent for policy fields that
	// live in YAML and the API only.
	MaxUpstreamCalls int `json:"maxUpstreamCalls,omitempty" yaml:"maxUpstreamCalls"`

	// MaxWalkDuration bounds the wall-clock time one request may spend being
	// walked across targets, checked before each dispatch.
	//
	// The call budget bounds spend; this bounds patience, and they fail in
	// different directions. Nothing bounded patience before: the executor is
	// handed the request context, on which no deadline is set, so the "bail
	// out when the deadline is close" guard inside the retry loop never ran.
	// With a per-attempt timeout measured in minutes, a walk over several
	// targets could keep dispatching for hours against a client that left
	// long ago — each dispatch billable, none deliverable.
	//
	// The default gives the whole walk what a single slow upstream call
	// gets. A walk that has already spent that has not been unlucky; it has
	// been walking through something that is not going to answer.
	MaxWalkDuration time.Duration `json:"maxWalkDuration,omitempty" yaml:"maxWalkDuration"`
}

const (
	minMaxAttempts = 1
	maxMaxAttempts = 5

	minUpstreamCalls = 1
	maxUpstreamCalls = 20
)

// DefaultRetryPolicy returns the platform default. YAML config and rule
// overrides build on top.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		// 2 = one same-target retry before L3 failover. Transient upstream faults
		// (network blip, timeout, 429, 5xx) recover in place instead of surfacing
		// as a hard error or burning an immediate failover — the right default for
		// flaky provider endpoints. Bounded to one retry so a non-idempotent
		// generation is re-sent at most once.
		MaxAttemptsPerTarget: 2,
		RetryOn:              []ErrorClass{ErrorClassNetwork, ErrorClassTimeout, ErrorClassRate429, ErrorClass5xx},
		BackoffInitial:       250 * time.Millisecond,
		BackoffMax:           5 * time.Second,
		BackoffJitter:        0.2,
		// One slow upstream call's worth of patience for the whole walk.
		// Long enough that a legitimate failover over several targets
		// completes; short enough that a walk through unresponsive
		// upstreams cannot outlive the client by hours.
		MaxWalkDuration: 10 * time.Minute,
	}
}

// MergedWith returns the receiver overlaid with override's set fields.
// Fields treated as "absent" on override:
//   - MaxAttemptsPerTarget == 0
//   - RetryOn == nil (length 0 is meaningful — "retry nothing")
//   - BackoffInitial == 0
//   - BackoffMax == 0
//   - BackoffJitter == 0
//
// nil override returns the receiver unchanged.
func (p RetryPolicy) MergedWith(o *RetryPolicy) RetryPolicy {
	if o == nil {
		return p
	}
	out := p
	if o.MaxAttemptsPerTarget != 0 {
		out.MaxAttemptsPerTarget = o.MaxAttemptsPerTarget
	}
	if o.RetryOn != nil {
		out.RetryOn = o.RetryOn
	}
	if o.BackoffInitial != 0 {
		out.BackoffInitial = o.BackoffInitial
	}
	if o.BackoffMax != 0 {
		out.BackoffMax = o.BackoffMax
	}
	if o.BackoffJitter != 0 {
		out.BackoffJitter = o.BackoffJitter
	}
	if o.MaxUpstreamCalls != 0 {
		out.MaxUpstreamCalls = o.MaxUpstreamCalls
	}
	if o.MaxWalkDuration != 0 {
		out.MaxWalkDuration = o.MaxWalkDuration
	}
	return out
}

// IsZero reports whether nothing on the policy was set — the signal callers use
// to tell "no policy was wired" from "a policy that happens to be conservative".
//
// It lives beside the struct so a new field is added within sight of the check
// that has to know about it. The previous arrangement enumerated the fields from
// another package, and the first field added after it was written was silently
// omitted: a deployment configuring only a call budget read as unwired and had
// its policy discarded. TestRetryPolicy_IsZeroCoversEveryField fails if a field
// is added without being accounted for here.
func (p RetryPolicy) IsZero() bool {
	return p.MaxAttemptsPerTarget == 0 &&
		p.RetryOn == nil &&
		p.BackoffInitial == 0 &&
		p.BackoffMax == 0 &&
		p.BackoffJitter == 0 &&
		p.MaxUpstreamCalls == 0 &&
		p.MaxWalkDuration == 0
}

// ClampMaxAttempts coerces a value into [1,5].
func ClampMaxAttempts(n int) int {
	if n < minMaxAttempts {
		return minMaxAttempts
	}
	if n > maxMaxAttempts {
		return maxMaxAttempts
	}
	return n
}

// EffectiveCallBudget resolves how many upstream calls a request may make,
// given the policy and the size of the plan being walked.
//
// An unset budget derives the bound the walk already has rather than inventing
// one: the walk visits every target and each may be dispatched to up to
// MaxAttemptsPerTarget times on this request, so the implicit bound is exactly
// targetCount × attempts. Deriving it means adding this field changes no
// request's outcome while nobody sets it.
//
// That derived value is also the CEILING at any setting. Each target's
// allowance is its own and no other target can spend what it leaves behind, so
// a configured number above targetCount × attempts buys nothing — it is not
// refused, there is simply no target with an attempt left to give. Raising
// MaxAttemptsPerTarget is what grants more attempts.
//
// Downward it does what it says. A two-target rule at the default two attempts
// has a derived ceiling of four; writing 3 stops the walk one dispatch earlier.
//
// A fixed default would not be neutral. Five was the first proposal, and a rule
// balancing three models with a four-entry fallback chain legitimately reaches
// its sixth distinct target during a multi-provider incident; that request
// succeeds today and would have been cut off.
//
// An explicitly configured budget is clamped to [1,20]. It may sit below the
// derived value — choosing a lower ceiling is the whole point — and twenty
// upstream calls is already far more than any healthy request needs.
func EffectiveCallBudget(p RetryPolicy, targetCount int) int {
	if p.MaxUpstreamCalls != 0 {
		if p.MaxUpstreamCalls < minUpstreamCalls {
			return minUpstreamCalls
		}
		if p.MaxUpstreamCalls > maxUpstreamCalls {
			return maxUpstreamCalls
		}
		return p.MaxUpstreamCalls
	}
	if targetCount < 1 {
		targetCount = 1
	}
	return targetCount * ClampMaxAttempts(p.MaxAttemptsPerTarget)
}
