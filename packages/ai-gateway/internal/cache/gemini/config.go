// Package geminicache manages the Gemini cachedContent lifecycle for the AI
// Gateway. When a request carries a large systemInstruction, the manager
// creates a cachedContent object via the Gemini v1beta REST API and rewrites
// subsequent requests to reference it instead, reducing prompt token costs.
package geminicache

import "sync/atomic"

// Config controls the Gemini cachedContent lifecycle manager.
// The zero value (Enabled=false) disables all caching activity.
type Config struct {
	// Enabled gates all cachedContent activity. When false, Inject is a no-op.
	Enabled bool `json:"enabled"`

	// MinSystemChars is the minimum byte length of the serialised
	// systemInstruction JSON before a cachedContent is created.
	// Requests shorter than this threshold are passed through unchanged.
	// Defaults to 4096 when zero (approximately 1k tokens, Gemini's minimum).
	MinSystemChars int `json:"min_system_chars"`

	// TTLSeconds is the requested TTL (in seconds) for new cachedContent
	// objects. Defaults to 3600 (60 minutes) when zero.
	TTLSeconds int `json:"ttl_seconds"`

	// CircuitBreakerThreshold is the number of consecutive creation failures
	// that trip the circuit breaker. Defaults to 5 when zero.
	CircuitBreakerThreshold int `json:"circuit_breaker_threshold"`

	// CircuitBreakerOpenSecs is how long the circuit stays open (in seconds)
	// after tripping. Defaults to 300 when zero.
	CircuitBreakerOpenSecs int `json:"circuit_breaker_open_secs"`
}

func (c Config) minSystemChars() int {
	if c.MinSystemChars <= 0 {
		return 4096
	}
	return c.MinSystemChars
}

func (c Config) ttlSeconds() int {
	if c.TTLSeconds <= 0 {
		return 3600
	}
	return c.TTLSeconds
}

func (c Config) cbThreshold() int {
	if c.CircuitBreakerThreshold <= 0 {
		return 5
	}
	return c.CircuitBreakerThreshold
}

func (c Config) cbOpenSecs() int {
	if c.CircuitBreakerOpenSecs <= 0 {
		return 300
	}
	return c.CircuitBreakerOpenSecs
}

// configHolder wraps Config in an atomic pointer for lock-free hot-swap.
type configHolder struct {
	v atomic.Pointer[Config]
}

func newConfigHolder(cfg Config) *configHolder {
	h := &configHolder{}
	h.v.Store(&cfg)
	return h
}

func (h *configHolder) get() Config {
	if p := h.v.Load(); p != nil {
		return *p
	}
	return Config{}
}

func (h *configHolder) set(cfg Config) {
	h.v.Store(&cfg)
}
