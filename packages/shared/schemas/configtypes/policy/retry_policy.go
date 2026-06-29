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
	MaxAttemptsPerTarget int           `json:"maxAttemptsPerTarget,omitempty" yaml:"maxAttemptsPerTarget"`
	RetryOn              []ErrorClass  `json:"retryOn,omitempty" yaml:"retryOn"`
	BackoffInitial       time.Duration `json:"backoffInitial,omitempty" yaml:"backoffInitial"`
	BackoffMax           time.Duration `json:"backoffMax,omitempty" yaml:"backoffMax"`
	BackoffJitter        float64       `json:"backoffJitter,omitempty" yaml:"backoffJitter"`
}

const (
	minMaxAttempts = 1
	maxMaxAttempts = 5
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
	return out
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
