// Package exemption provides an in-memory store for TLS bump exemptions
// with sliding-window failure tracking and auto-exemption.
package exemption

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Source identifies how an exemption was created.
type Source string

const (
	SourceAuto  Source = "auto"
	SourceAdmin Source = "admin"
)

// Config controls the exemption store behavior.
type Config struct {
	Enabled              bool
	FailureThreshold     int
	WindowSeconds        int
	ExemptionDurationSec int
}

// DefaultConfig returns sensible defaults: 3 failures in 60s → 24h exemption.
func DefaultConfig() Config {
	return Config{
		Enabled:              true,
		FailureThreshold:     3,
		WindowSeconds:        60,
		ExemptionDurationSec: 86400,
	}
}

// Entry is a single exemption record.
type Entry struct {
	Host      string
	Reason    string
	Source    Source
	ExpiresAt time.Time
}

// Store holds the exemption set, denylist, and failure tracker.
type Store struct {
	mu       sync.RWMutex
	cfg      Config
	exempt   map[string]Entry       // host -> entry
	denylist map[string]struct{}    // hosts that may NEVER be auto-exempted
	failures map[string][]time.Time // host -> timestamps of recent failures
}

// NewStore creates an empty exemption store with the given configuration.
func NewStore(cfg Config) *Store {
	if cfg.FailureThreshold == 0 {
		cfg.FailureThreshold = 3
	}
	if cfg.WindowSeconds == 0 {
		cfg.WindowSeconds = 60
	}
	if cfg.ExemptionDurationSec == 0 {
		cfg.ExemptionDurationSec = 86400
	}
	return &Store{
		cfg:      cfg,
		exempt:   make(map[string]Entry),
		denylist: make(map[string]struct{}),
		failures: make(map[string][]time.Time),
	}
}

// SetConfig updates the runtime configuration (thread-safe).
func (s *Store) SetConfig(cfg Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cfg.FailureThreshold == 0 {
		cfg.FailureThreshold = 3
	}
	if cfg.WindowSeconds == 0 {
		cfg.WindowSeconds = 60
	}
	if cfg.ExemptionDurationSec == 0 {
		cfg.ExemptionDurationSec = 86400
	}
	s.cfg = cfg
}

// matchesHostList reports whether host matches any entry in the map, using
// both exact lookup and wildcard-suffix matching.  A wildcard entry is stored
// as "*.example.com" and matches any host whose name ends with ".example.com"
// and has at least one label before the suffix (so "*.example.com" does NOT
// match bare "example.com").  Deep subdomains ("foo.api.example.com") are also
// matched — semantics identical to the exempt-side wildcard path.
func matchesHostList(host string, list map[string]struct{}) bool {
	if _, ok := list[host]; ok {
		return true
	}
	for pattern := range list {
		if strings.HasPrefix(pattern, "*.") {
			suffix := pattern[1:] // e.g. ".example.com"
			if strings.HasSuffix(host, suffix) && len(host) > len(suffix) {
				return true
			}
		}
	}
	return false
}

// IsExempt reports whether the host is currently exempted.
// Order: denylist (always reject) → exact match → wildcard suffix match.
func (s *Store) IsExempt(host string) (bool, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if matchesHostList(host, s.denylist) {
		return false, "denied"
	}

	if e, ok := s.exempt[host]; ok && (e.ExpiresAt.IsZero() || time.Now().Before(e.ExpiresAt)) {
		return true, e.Reason
	}

	// Wildcard suffix match: pattern "*.example.com" matches "api.example.com"
	for pattern, e := range s.exempt {
		if strings.HasPrefix(pattern, "*.") {
			suffix := pattern[1:]
			if strings.HasSuffix(host, suffix) && len(host) > len(suffix) {
				if e.ExpiresAt.IsZero() || time.Now().Before(e.ExpiresAt) {
					return true, e.Reason
				}
			}
		}
	}

	return false, ""
}

// RecordFailure increments the failure counter for the host.
// Returns (autoExempted, expiresAt) — autoExempted is true when this call caused
// a new auto-exemption to be created.
func (s *Store) RecordFailure(host string) (bool, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.cfg.Enabled {
		return false, time.Time{}
	}

	// Denylist hosts are never auto-exempted, even after failures.
	// Uses wildcard matching so "*.openai.com" blocks "api.openai.com".
	if matchesHostList(host, s.denylist) {
		return false, time.Time{}
	}

	// Already exempted? skip.
	if e, ok := s.exempt[host]; ok && (e.ExpiresAt.IsZero() || time.Now().Before(e.ExpiresAt)) {
		return false, e.ExpiresAt
	}

	now := time.Now()
	cutoff := now.Add(-time.Duration(s.cfg.WindowSeconds) * time.Second)
	timestamps := s.failures[host]
	// Prune old entries
	pruned := timestamps[:0]
	for _, t := range timestamps {
		if t.After(cutoff) {
			pruned = append(pruned, t)
		}
	}
	pruned = append(pruned, now)
	s.failures[host] = pruned

	if len(pruned) >= s.cfg.FailureThreshold {
		expiresAt := now.Add(time.Duration(s.cfg.ExemptionDurationSec) * time.Second)
		s.exempt[host] = Entry{
			Host:      host,
			Reason:    "auto: TLS handshake failures",
			Source:    SourceAuto,
			ExpiresAt: expiresAt,
		}
		// Clear the failure counter so future failures don't immediately re-exempt
		delete(s.failures, host)
		slog.Info("auto-exempted host after TLS failures", "host", host, "expires", expiresAt)
		return true, expiresAt
	}
	return false, time.Time{}
}

// Add inserts or replaces an exemption entry.
// duration of 0 means no expiry.
func (s *Store) Add(host, reason string, source Source, duration time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	expiresAt := time.Time{}
	if duration != 0 {
		expiresAt = time.Now().Add(duration)
	}
	s.exempt[host] = Entry{Host: host, Reason: reason, Source: source, ExpiresAt: expiresAt}
}

// Remove deletes an exemption entry.
func (s *Store) Remove(host string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.exempt, host)
}

// List returns all current exemption entries.
func (s *Store) List() []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Entry, 0, len(s.exempt))
	for _, e := range s.exempt {
		out = append(out, e)
	}
	return out
}

// SetDenylist replaces the denylist with the provided hosts.
func (s *Store) SetDenylist(hosts []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.denylist = make(map[string]struct{}, len(hosts))
	for _, h := range hosts {
		s.denylist[h] = struct{}{}
	}
}

// ApplyShadowState implements shadow.ShadowApplier. It parses the
// Hub-pushed shadow payload — shape:
//
//	{
//	  "admin_exemptions": [host, ...],
//	  "denylist":         [host, ...]
//	}
//
// — and updates the allowlist / denylist accordingly. Empty or null raw
// is a no-op so an initial tick before P0-C does not wipe local defaults.
func (s *Store) ApplyShadowState(_ context.Context, raw json.RawMessage) error {
	if len(raw) == 0 || string(raw) == "null" || string(raw) == "{}" {
		return nil
	}
	var payload struct {
		AdminExemptions []string `json:"admin_exemptions"`
		Denylist        []string `json:"denylist"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return fmt.Errorf("exemption: parse shadow state: %w", err)
	}
	s.SetAllowlist(payload.AdminExemptions)
	s.SetDenylist(payload.Denylist)
	return nil
}

// SetAllowlist bulk-replaces the admin-configured allowlist entries.
// Auto-exempted entries are preserved.
func (s *Store) SetAllowlist(hosts []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Remove existing admin entries
	for k, v := range s.exempt {
		if v.Source == SourceAdmin {
			delete(s.exempt, k)
		}
	}
	// Add new admin entries with no expiry
	for _, h := range hosts {
		s.exempt[h] = Entry{Host: h, Reason: "admin allowlist", Source: SourceAdmin}
	}
}

// Cleanup removes expired entries. Returns the number of entries removed.
func (s *Store) Cleanup() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	removed := 0
	for k, e := range s.exempt {
		if !e.ExpiresAt.IsZero() && now.After(e.ExpiresAt) {
			delete(s.exempt, k)
			removed++
		}
	}
	return removed
}

// RunCleanupLoop periodically prunes expired entries until ctx is cancelled.
func (s *Store) RunCleanupLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if removed := s.Cleanup(); removed > 0 {
				slog.Info("exemption cleanup removed expired entries", "count", removed)
			}
		}
	}
}

// PendingAutoExemptions returns auto-exempted entries that haven't been reported
// to the gateway yet (the caller is expected to track which are uploaded).
func (s *Store) PendingAutoExemptions() []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Entry, 0)
	for _, e := range s.exempt {
		if e.Source == SourceAuto {
			out = append(out, e)
		}
	}
	return out
}
