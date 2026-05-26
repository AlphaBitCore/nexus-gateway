package tlsbump

import (
	"crypto/tls"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/enums"
)

// Bump status constants used in audit events and metrics.
// These re-export shared configtypes constants for consistency across services.
const (
	BumpStatusSuccess           = string(enums.BumpStatusSuccess)
	BumpStatusFailedPassthrough = string(enums.BumpStatusFailedPassthrough)
	BumpStatusExemptPinned      = string(enums.BumpStatusExemptPinned)
	BumpStatusExemptConfigured  = string(enums.BumpStatusExemptConfigured)
)

// PinningConfig holds exemption configuration for the PinningTracker.
type PinningConfig struct {
	Exemptions []DomainExemption
	AutoExempt AutoExemptConfig
}

// DomainExemption represents an administratively configured host that should
// bypass TLS interception, along with the reason for the exemption.
type DomainExemption struct {
	Host   string
	Reason string
}

// AutoExemptConfig governs automatic exemption behaviour when repeated TLS
// bump failures are observed for the same host.
type AutoExemptConfig struct {
	Enabled           bool
	FailureThreshold  int
	WindowSeconds     int
	ExemptionDuration time.Duration
}

// PinningTracker manages certificate pinning detection, domain exemptions,
// and auto-exemption based on repeated TLS bump failures.
type PinningTracker struct {
	mu           sync.RWMutex
	configExempt map[string]string      // host → reason (admin-configured)
	autoExempt   map[string]time.Time   // host → exempt-until
	failures     map[string][]time.Time // host → failure timestamps
	config       AutoExemptConfig

	// nowFunc is used for time; defaults to time.Now but can be overridden in tests.
	nowFunc func() time.Time
}

// NewPinningTracker creates a tracker with admin-configured exemptions and
// auto-exempt settings.
func NewPinningTracker(cfg PinningConfig) *PinningTracker {
	configExempt := make(map[string]string, len(cfg.Exemptions))
	for _, ex := range cfg.Exemptions {
		host := strings.ToLower(ex.Host)
		// Strip port if present so lookups (which also strip port) match.
		if idx := strings.LastIndex(host, ":"); idx != -1 {
			host = host[:idx]
		}
		configExempt[host] = ex.Reason
	}

	return &PinningTracker{
		configExempt: configExempt,
		autoExempt:   make(map[string]time.Time),
		failures:     make(map[string][]time.Time),
		config:       cfg.AutoExempt,
		nowFunc:      time.Now,
	}
}

// IsExempt checks if a host is exempt from TLS bump (configured or auto-exempted).
// Returns (exempt, reason, bumpStatus).
func (t *PinningTracker) IsExempt(host string) (bool, string, string) {
	normalised := strings.ToLower(host)
	// Strip port if present.
	if idx := strings.LastIndex(normalised, ":"); idx != -1 {
		normalised = normalised[:idx]
	}

	t.mu.RLock()
	defer t.mu.RUnlock()

	// 1. Check admin-configured exact match.
	if reason, ok := t.configExempt[normalised]; ok {
		return true, reason, BumpStatusExemptConfigured
	}

	// 2. Check admin-configured wildcard match (*.example.com).
	parts := strings.SplitN(normalised, ".", 2)
	if len(parts) == 2 {
		wildcard := "*." + parts[1]
		if reason, ok := t.configExempt[wildcard]; ok {
			return true, reason, BumpStatusExemptConfigured
		}
	}

	// 3. Check auto-exemption.
	if until, ok := t.autoExempt[normalised]; ok {
		if t.nowFunc().Before(until) {
			return true, "auto-exempted: repeated TLS bump failures", BumpStatusExemptPinned
		}
		// Expired — clean up lazily (write lock needed, but we hold read lock).
		// Defer cleanup to the next RecordFailure or write path.
	}

	return false, "", ""
}

// cleanExpired removes auto-exemptions and failure records that have expired.
// Must be called under write lock.
func (t *PinningTracker) cleanExpired() {
	now := t.nowFunc()
	for host, until := range t.autoExempt {
		if now.After(until) {
			delete(t.autoExempt, host)
		}
	}
	if t.config.WindowSeconds > 0 {
		windowStart := now.Add(-time.Duration(t.config.WindowSeconds) * time.Second)
		for host, timestamps := range t.failures {
			pruned := timestamps[:0]
			for _, ts := range timestamps {
				if ts.After(windowStart) {
					pruned = append(pruned, ts)
				}
			}
			if len(pruned) == 0 {
				delete(t.failures, host)
			} else {
				t.failures[host] = pruned
			}
		}
	}
}

// RecordFailure records a TLS bump failure for a host. It may trigger
// auto-exemption if the failure threshold is reached within the configured
// window. Returns the bump status to use for auditing.
func (t *PinningTracker) RecordFailure(host string) string {
	normalised := strings.ToLower(host)
	if idx := strings.LastIndex(normalised, ":"); idx != -1 {
		normalised = normalised[:idx]
	}

	if !t.config.Enabled {
		return BumpStatusFailedPassthrough
	}

	now := t.nowFunc()
	windowStart := now.Add(-time.Duration(t.config.WindowSeconds) * time.Second)

	t.mu.Lock()
	defer t.mu.Unlock()

	// Periodically clean expired entries to prevent unbounded map growth.
	t.cleanExpired()

	// Append and prune timestamps outside the window.
	timestamps := t.failures[normalised]
	pruned := timestamps[:0]
	for _, ts := range timestamps {
		if ts.After(windowStart) {
			pruned = append(pruned, ts)
		}
	}
	pruned = append(pruned, now)
	t.failures[normalised] = pruned

	// Check threshold.
	if len(pruned) >= t.config.FailureThreshold {
		t.autoExempt[normalised] = now.Add(t.config.ExemptionDuration)
		// Clear failure timestamps; the host is now auto-exempted.
		delete(t.failures, normalised)
		return BumpStatusExemptPinned
	}

	return BumpStatusFailedPassthrough
}

// IsPinningError checks if a TLS handshake error indicates certificate pinning
// rejection by the client. It looks for well-known TLS alert descriptions that
// clients send when they reject an unexpected certificate.
//
// Detection strategy (in priority order):
//  1. tls.AlertError type assertion — available since Go 1.21, stable across Go
//     versions. Matches the wrapped alert code exactly with no string parsing.
//  2. String matching — fallback for errors wrapped by third-party TLS libraries
//     or older transports that do not surface tls.AlertError directly.
func IsPinningError(err error) bool {
	if err == nil {
		return false
	}

	// Prefer type assertion (stable across Go versions, no string parsing).
	// TLS alert codes that indicate certificate pinning rejection by the client:
	//   42  bad_certificate       — certificate was rejected outright
	//   48  unknown_ca            — issuer not trusted (self-signed / custom CA)
	//   49  access_denied         — access control on the certificate
	//   60  no_certificate        — (SSLv3 compat) client sent no certificate
	var alertErr tls.AlertError
	if errors.As(err, &alertErr) {
		switch uint8(alertErr) {
		case 42, 48, 49, 60: // bad_certificate, unknown_ca, access_denied, no_certificate
			return true
		}
	}

	// Fallback: string matching for wrapped errors or third-party TLS libraries
	// that do not propagate tls.AlertError through the error chain.
	//
	// These cover the standard TLS alert descriptions sent by pinning-aware clients:
	//   alert(48) unknown_ca              → "unknown authority" / "unknown certificate authority"
	//   alert(42) bad_certificate         → "bad certificate"
	//   alert(46) certificate_unknown     → "certificate unknown"
	//   alert(49) access_denied           → "access denied"
	//   alert(44) certificate_required    → "certificate required"
	// Note: the overly broad "tls: alert" was removed — it matched
	// legitimate TLS misconfigurations (expired cert, wrong cipher) and
	// could bypass compliance inspection.
	msg := strings.ToLower(err.Error())
	pinningIndicators := []string{
		"bad certificate",
		"unknown authority",
		"unknown certificate authority",
		"certificate unknown",
		"access denied",
		"certificate required",
	}

	for _, indicator := range pinningIndicators {
		if strings.Contains(msg, indicator) {
			return true
		}
	}

	return false
}
