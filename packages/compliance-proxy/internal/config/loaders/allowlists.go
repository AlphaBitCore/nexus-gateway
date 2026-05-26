// Package loaders — domain allowlist loader.
//
// Loads enabled InterceptionDomain host patterns from the Prisma-migrated
// PostgreSQL database. The compliance-proxy uses these to dynamically build
// its domain allowlist so that when an admin adds a new interception domain
// in the Control Plane UI, the proxy automatically allows traffic to that
// domain without a YAML change or restart.
package loaders

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// DomainAllowlistEntry represents a domain:port pair extracted from an
// InterceptionDomain row.
type DomainAllowlistEntry struct {
	HostPattern string // e.g. "api.openai.com", "*.anthropic.com"
	Port        string // default "443"
}

// AllowlistRow is the raw shape returned by the LoadDomainAllowlist query
// (host_pattern + host_match_type). Exposed so the pure deduplication +
// normalisation pipeline in buildAllowlistEntries can be unit-tested
// without driving a live *sql.DB.
type AllowlistRow struct {
	HostPattern string
	MatchType   string
}

// LoadDomainAllowlist queries enabled InterceptionDomain rows and returns
// their host patterns as allowlist entries suitable for access.NewDomainAllowlist.
// The patterns are returned in host:port format (defaulting to port 443).
// Row processing + dedup is delegated to buildAllowlistEntries so the
// interesting logic (normalisation drops, dedup, empty-pattern skip) is
// exercised by unit tests without needing a live database.
func LoadDomainAllowlist(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT host_pattern, host_match_type
		FROM "interception_domain"
		WHERE enabled = true
		ORDER BY priority DESC, created_at ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("load domain allowlist: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var scanned []AllowlistRow
	for rows.Next() {
		var row AllowlistRow
		if err := rows.Scan(&row.HostPattern, &row.MatchType); err != nil {
			return nil, fmt.Errorf("scan domain allowlist row: %w", err)
		}
		scanned = append(scanned, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return buildAllowlistEntries(scanned), nil
}

// buildAllowlistEntries converts already-scanned rows into the
// deduplicated host:port slice. Pure logic — no DB access — so unit
// tests can drive every branch (normalisation drop on REGEX,
// empty-host skip, repeated entries collapsed to one) without a live
// database. The ordering is preserved from the input slice (which the
// SQL caller pre-orders by priority DESC, created_at ASC).
func buildAllowlistEntries(rows []AllowlistRow) []string {
	var entries []string
	seen := make(map[string]bool)
	for _, row := range rows {
		// Normalize the host pattern to a domain allowlist entry.
		// The domain allowlist supports exact and prefix-wildcard
		// (*.example.com) entries. REGEX and PREFIX match types from
		// InterceptionDomain are converted to wildcard entries where
		// possible.
		entry := normalizeToAllowlistEntry(row.HostPattern, row.MatchType)
		if entry == "" {
			continue
		}
		if !seen[entry] {
			seen[entry] = true
			entries = append(entries, entry)
		}
	}
	return entries
}

// normalizeToAllowlistEntry converts an InterceptionDomain host pattern
// into a domain:port entry for the DomainAllowlist. Returns "" if the
// pattern cannot be represented as a simple domain allowlist entry.
func normalizeToAllowlistEntry(hostPattern, matchType string) string {
	hostPattern = strings.TrimSpace(hostPattern)
	if hostPattern == "" {
		return ""
	}

	switch strings.ToUpper(matchType) {
	case "EXACT":
		// "api.openai.com" → "api.openai.com:443"
		return ensurePort(hostPattern)

	case "GLOB":
		// "*.openai.com" → "*.openai.com:443"
		// Other globs (e.g. "api-*.openai.com") are not representable
		// in the simple allowlist format; admit them as-is for best effort.
		return ensurePort(hostPattern)

	case "PREFIX":
		// "api.openai" → "api.openai*" which maps to a wildcard suffix.
		// Prefix match is uncommon for domains; best-effort exact entry.
		return ensurePort(hostPattern)

	case "REGEX":
		// Regex patterns cannot be cleanly converted to allowlist entries.
		// Skip them — they require the full traffic matching layer.
		return ""

	default:
		return ensurePort(hostPattern)
	}
}

// ensurePort appends ":443" if the entry doesn't already contain a port.
func ensurePort(entry string) string {
	if strings.Contains(entry, ":") {
		return entry
	}
	return entry + ":443"
}
