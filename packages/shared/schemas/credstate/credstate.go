// Package credstate is the canonical single source of truth for
// per-credential runtime state shared by AI Gateway, Control Plane, and
// Nexus Hub. It owns:
//
//   - Redis key names and field names (stats hash, circuit hash, dirty set,
//     in-flight set).
//   - Circuit-breaker state values (closed / open / half_open) and
//     open-reason values (auth_fail / rate_limit).
//   - Health classification values (healthy / degraded / unavailable /
//     unknown / collecting) and dominant-error values.
//   - Default reliability thresholds used when neither global Hub config
//     nor a per-credential override is set.
//
// All call-sites import from this package — there are no duplicated
// constants or "circular import" workarounds. The package itself depends
// on nothing outside the standard library so every service may import it
// freely.
package credstate

import "fmt"

// Redis layout

const (
	// StatsKeyPrefix is the per-credential usage-stats hash prefix.
	// Full key: cred:stats:{credentialID}. See StatsKey().
	StatsKeyPrefix = "cred:stats:"

	// StatsDirtySet is the unprocessed-stats-changes set. Members are
	// credential IDs awaiting flush by the Hub credential-stats-flush job.
	StatsDirtySet = "cred:stats:dirty"

	// CircuitKeyPrefix is the per-credential circuit-breaker hash prefix.
	// Full key: cred:circuit:{credentialID}. See CircuitKey().
	CircuitKeyPrefix = "cred:circuit:"

	// CircuitDirtySet collects credential IDs whose circuit state has
	// transitioned since the last flush. Drained by the Hub
	// credential-circuit-flush job. Only state transitions mark dirty;
	// increments to the live auth_fails counter below the open threshold
	// do not.
	CircuitDirtySet = "cred:circuit:dirty"

	// InFlightKeyPrefix is the per-Hub in-flight working-set hash prefix.
	// At each flush cycle the job atomically SMOVEs from the dirty set to
	// in-flight, processes, then DELs the in-flight set. Crashed Hub
	// instances leave entries in their own in-flight set; a watchdog
	// reclaims them after InFlightStaleAfter.
	InFlightKeyPrefix = "cred:circuit:in_flight:"
)

// StatsKey returns the Redis key holding usage stats for a credential.
func StatsKey(credentialID string) string {
	return StatsKeyPrefix + credentialID
}

// CircuitKey returns the Redis key holding circuit-breaker state for a
// credential.
func CircuitKey(credentialID string) string {
	return CircuitKeyPrefix + credentialID
}

// InFlightSet returns the per-Hub in-flight working-set key.
func InFlightSet(hubID string) string {
	return InFlightKeyPrefix + hubID
}

// Stats-hash field names.
const (
	StatsFieldCount      = "cnt"
	StatsFieldUsedAt     = "used_at"
	StatsFieldOkAt       = "ok_at"
	StatsFieldFailAt     = "fail_at"
	StatsFieldFailReason = "fail_reason"
)

// Circuit-hash field names.
const (
	CircuitFieldState      = "state"
	CircuitFieldAuthFails  = "auth_fails"
	CircuitFieldOpenedAt   = "opened_at"
	CircuitFieldNextProbe  = "next_probe_at"
	CircuitFieldOpenReason = "open_reason"
)

// Enum values — keep in lockstep with the Credential CHECK constraints and
// the OpenAPI schema in docs/users/api/openapi/admin/e41-s5-admin-credentials-state.yaml.

// CircuitState values.
const (
	CircuitClosed   = "closed"
	CircuitOpen     = "open"
	CircuitHalfOpen = "half_open"
)

// CircuitReason (a.k.a. open_reason) values.
const (
	ReasonAuthFail  = "auth_fail"
	ReasonRateLimit = "rate_limit"
)

// HealthStatus values.
const (
	HealthHealthy     = "healthy"
	HealthDegraded    = "degraded"
	HealthUnavailable = "unavailable"
	HealthUnknown     = "unknown"    // never had traffic
	HealthCollecting  = "collecting" // had traffic but < min samples
)

// DominantError values — the most prevalent failure category in the recent
// health window. None means "no failures observed".
const (
	DominantNone        = "none"
	DominantAuthFail    = "auth_fail"    // 401/403
	DominantRateLimit   = "rate_limit"   // 429
	DominantUpstream5xx = "upstream_5xx" // 5xx from provider
	DominantTimeout     = "timeout"      // 0 status / network errors
	DominantClientError = "client_error" // 4xx except 401/403/429
	DominantMixed       = "mixed"        // no single category > 50%
)

// HealthTrend values.
const (
	TrendImproving = "improving"
	TrendStable    = "stable"
	TrendDegrading = "degrading"
)

// Reliability thresholds — defaults used when neither Hub-shadow global
// config nor a per-credential override applies.
//
// Operators tune these at runtime through the Settings → Credential
// Reliability page (writes Hub shadow Category A inline config); the
// values flow back to AI Gateway via the standard thingclient WebSocket
// push. Per-credential overrides live on Credential.reliabilityOverrides.

// Thresholds is the bag of all admin-tunable reliability parameters.
// Effective thresholds are resolved as: per-credential override (if set)
// ?? global Hub shadow value (if set) ?? these defaults.
type Thresholds struct {
	// AuthFailThreshold is the number of consecutive 401/403 responses
	// that opens the circuit with reason=auth_fail.
	AuthFailThreshold int `json:"authFailThreshold" yaml:"authFailThreshold"`

	// RateLimitCooldownSeconds is how long a rate_limit OPEN circuit
	// stays open before auto-transitioning to half_open on the next
	// selection read.
	RateLimitCooldownSeconds int `json:"rateLimitCooldownSeconds" yaml:"rateLimitCooldownSeconds"`

	// HealthyThresholdPct (0-100) is the minimum success rate to classify
	// a credential as healthy. Below HealthyThresholdPct but at or above
	// DegradedThresholdPct → degraded. Below DegradedThresholdPct →
	// unavailable.
	HealthyThresholdPct  int `json:"healthyThresholdPct" yaml:"healthyThresholdPct"`
	DegradedThresholdPct int `json:"degradedThresholdPct" yaml:"degradedThresholdPct"`

	// HealthMinSamples is the smallest sample count over the health window
	// at which a status other than collecting is assigned. Below this
	// floor, status = collecting (with samplesObserved exposed for UX).
	HealthMinSamples int `json:"healthMinSamples" yaml:"healthMinSamples"`

	// HealthWindowSeconds is the rolling window length for the "current"
	// health classification. A second window 12× this length feeds the
	// trend computation.
	HealthWindowSeconds int `json:"healthWindowSeconds" yaml:"healthWindowSeconds"`

	// HealthSustainedDegradedSeconds is how long a credential must remain
	// in degraded or unavailable state before the
	// credential.health_degraded_sustained alert fires.
	HealthSustainedDegradedSeconds int `json:"healthSustainedDegradedSeconds" yaml:"healthSustainedDegradedSeconds"`
}

// DefaultThresholds is the conservative bootstrap configuration shipped in
// the binary. Hub YAML overrides at startup; admin UI overrides at runtime.
var DefaultThresholds = Thresholds{
	AuthFailThreshold:              3,
	RateLimitCooldownSeconds:       60,
	HealthyThresholdPct:            95,
	DegradedThresholdPct:           50,
	HealthMinSamples:               5,
	HealthWindowSeconds:            300, // 5 min
	HealthSustainedDegradedSeconds: 900, // 15 min
}

// Merge folds a (possibly partial) override on top of the receiver and
// returns the resolved set. Fields with the zero value in override leave
// the base value untouched. Use Merge to layer per-credential overrides
// over global Hub-shadow values.
func (t Thresholds) Merge(override Thresholds) Thresholds {
	if override.AuthFailThreshold > 0 {
		t.AuthFailThreshold = override.AuthFailThreshold
	}
	if override.RateLimitCooldownSeconds > 0 {
		t.RateLimitCooldownSeconds = override.RateLimitCooldownSeconds
	}
	if override.HealthyThresholdPct > 0 {
		t.HealthyThresholdPct = override.HealthyThresholdPct
	}
	if override.DegradedThresholdPct > 0 {
		t.DegradedThresholdPct = override.DegradedThresholdPct
	}
	if override.HealthMinSamples > 0 {
		t.HealthMinSamples = override.HealthMinSamples
	}
	if override.HealthWindowSeconds > 0 {
		t.HealthWindowSeconds = override.HealthWindowSeconds
	}
	if override.HealthSustainedDegradedSeconds > 0 {
		t.HealthSustainedDegradedSeconds = override.HealthSustainedDegradedSeconds
	}
	return t
}

// Validate returns nil when all thresholds satisfy their invariants.
// HealthyThresholdPct must be strictly greater than DegradedThresholdPct;
// every numeric field must be positive. Callers should reject ingest
// (admin UI, YAML) that fails Validate rather than silently coercing.
func (t Thresholds) Validate() error {
	if t.AuthFailThreshold <= 0 {
		return fmt.Errorf("authFailThreshold must be > 0")
	}
	if t.RateLimitCooldownSeconds <= 0 {
		return fmt.Errorf("rateLimitCooldownSeconds must be > 0")
	}
	if t.HealthyThresholdPct <= 0 || t.HealthyThresholdPct > 100 {
		return fmt.Errorf("healthyThresholdPct must be in (0, 100]")
	}
	if t.DegradedThresholdPct <= 0 || t.DegradedThresholdPct >= t.HealthyThresholdPct {
		return fmt.Errorf("degradedThresholdPct must be in (0, healthyThresholdPct)")
	}
	if t.HealthMinSamples <= 0 {
		return fmt.Errorf("healthMinSamples must be > 0")
	}
	if t.HealthWindowSeconds <= 0 {
		return fmt.Errorf("healthWindowSeconds must be > 0")
	}
	if t.HealthSustainedDegradedSeconds <= 0 {
		return fmt.Errorf("healthSustainedDegradedSeconds must be > 0")
	}
	return nil
}

// Operational constants — not admin-tunable.

const (
	// WriteTimeoutMillis caps each fire-and-forget Redis write from the
	// AI Gateway hot path. The buffer never blocks request handling.
	WriteTimeoutMillis = 200

	// InFlightStaleAfterSeconds is how long an unattended in-flight set
	// (from a crashed Hub) persists before being reclaimed. The current
	// active Hub looks for stale in-flight sets on each cycle and merges
	// their members back into the dirty set.
	InFlightStaleAfterSeconds = 300
)
