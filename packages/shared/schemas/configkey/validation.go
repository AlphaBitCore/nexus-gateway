package configkey

import (
	"context"
	"fmt"
)

// ValidByThingType lists the legal config_key strings for each Thing-type.
// Keys not in this map for their type are orphan rows or possible typos.
//
// Update this map in the SAME PR as any new config_key being added.
var ValidByThingType = map[string][]string{
	"nexus-hub":     {LogLevel, Observability},
	"control-plane": {LogLevel, Observability},
	"ai-gateway": {
		LogLevel, Observability, Cache, AIGuard, GatewayPassthrough,
		PayloadCapture, CredentialReliability,
		Providers, Models, Credentials, RoutingRules, VirtualKeys,
		QuotaPolicies, QuotaOverrides, Organizations, Hooks,
		// Dual-tier response-cache keys pushed to every ai-gateway Thing.
		// ResponseCacheTimeSensitivePatterns: cluster-wide freshness rule list.
		// SemanticCacheConfig: fleet-wide L1 embedding singleton config.
		ResponseCacheTimeSensitivePatterns, SemanticCacheConfig,
		// Extract (L1) cache fleet config — hot-swapped via atomic.Pointer.
		ResponseCacheExtractConfig,
	},
	"compliance-proxy": {
		LogLevel, Observability, Killswitch, Onboarding,
		PayloadCapture, StreamingCompliance,
		InterceptionDomains, Hooks, Exemptions,
	},
	"agent": {
		AgentSettings, DiagMode, Exemptions, Hooks,
		InterceptionDomains, PayloadCapture, StreamingCompliance,
		Killswitch,
		// Cat B Hub-pulled snapshots — also registered on agent side.
		InstalledRulePacks, UserContext,
	},
}

// OrphanRow describes a (type, key) tuple present in the DB but not
// in ValidByThingType.
type OrphanRow struct {
	Type string
	Key  string
}

// DBScanner is the narrow interface AuditTemplateRows needs. Kept
// minimal on purpose so this package stays free of a pgx import.
type DBScanner interface {
	Query(ctx context.Context, sql string, args ...any) (Rows, error)
}

// Rows is a minimal subset of pgx.Rows (Next + Scan + Close).
type Rows interface {
	Next() bool
	Scan(dest ...any) error
	Close()
}

// AuditTemplateRows scans thing_config_template and reports any
// (type, key) tuples not in ValidByThingType. Called once at Hub
// startup; logs WARN per orphan but does not fail boot — orphans
// can exist temporarily during a multi-PR migration.
func AuditTemplateRows(ctx context.Context, db DBScanner) ([]OrphanRow, error) {
	rows, err := db.Query(ctx, "SELECT type, config_key FROM thing_config_template")
	if err != nil {
		return nil, fmt.Errorf("query thing_config_template: %w", err)
	}
	defer rows.Close()

	var orphans []OrphanRow
	for rows.Next() {
		var t, k string
		if err := rows.Scan(&t, &k); err != nil {
			return nil, fmt.Errorf("scan template row: %w", err)
		}
		if !isValid(t, k) {
			orphans = append(orphans, OrphanRow{Type: t, Key: k})
		}
	}
	return orphans, nil
}

func isValid(thingType, key string) bool {
	valid, ok := ValidByThingType[thingType]
	if !ok {
		return false
	}
	for _, v := range valid {
		if v == key {
			return true
		}
	}
	return false
}
