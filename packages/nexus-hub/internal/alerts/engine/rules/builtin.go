package rules

import "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine"

// BuiltinRules is the Go-side source of truth for built-in alerting rule
// definitions used by `rules.NewRegistry` at Hub startup. The AlertRule rows
// are seeded from tools/db-migrate/seed/fixtures/AlertRule.json;
// TestSeedRulesAppearInBuiltin (builtin_seed_lockstep_test.go) enforces
// the reverse direction so every seed AlertRule has a matching RuleDef
// here — without that gate, admin "Reset Rule" silently no-ops on
// seed-only entries.
//
// Note on severity casing: AlertRule rows stored in AlertRule.json use
// uppercase strings ("HIGH", "CRITICAL") because the Prisma enum is
// defined in upper case. The Go domain model uses lowercase Severity
// constants; the reset handler is responsible for normalizing when it
// writes back to the DB.
var BuiltinRules = []RuleDef{
	{
		ID:              "quota.threshold",
		DisplayName:     "Quota Threshold Crossed",
		SourceType:      "quota",
		DefaultSeverity: alerting.SeverityHigh,
		RequiresAck:     true,
		Enabled:         true,
		CooldownSec:     300,
		Params:          mustJSON(map[string]any{"thresholds": []int{80, 95}}),
		ParamsSchema: mustJSON(map[string]any{
			"type": "object",
			"properties": map[string]any{
				"thresholds": map[string]any{
					"type":     "array",
					"items":    map[string]any{"type": "integer", "minimum": 1, "maximum": 100},
					"minItems": 1,
				},
			},
			"required": []string{"thresholds"},
		}),
	},
	{
		ID:              "quota.vk_expiring",
		DisplayName:     "Virtual Key Expiring",
		SourceType:      "quota",
		DefaultSeverity: alerting.SeverityMedium,
		RequiresAck:     false,
		Enabled:         true,
		CooldownSec:     86400,
		Params:          mustJSON(map[string]any{"warnDays": []int{30, 15, 7, 1}}),
		ParamsSchema: mustJSON(map[string]any{
			"type": "object",
			"properties": map[string]any{
				"warnDays": map[string]any{
					"type":  "array",
					"items": map[string]any{"type": "integer", "minimum": 1},
				},
			},
			"required": []string{"warnDays"},
		}),
	},
	{
		ID:              "proxy.hook_failure_rate",
		DisplayName:     "Proxy Hook Failure Rate",
		SourceType:      "proxy",
		DefaultSeverity: alerting.SeverityHigh,
		RequiresAck:     false,
		Enabled:         true,
		CooldownSec:     300,
		Params:          mustJSON(map[string]any{"thresholdPct": 20, "windowSec": 300, "minSamples": 10}),
		ParamsSchema: mustJSON(map[string]any{
			"type": "object",
			"properties": map[string]any{
				"thresholdPct": map[string]any{"type": "integer", "minimum": 1, "maximum": 100},
				"windowSec":    map[string]any{"type": "integer", "minimum": 60},
				"minSamples":   map[string]any{"type": "integer", "minimum": 1},
			},
			"required": []string{"thresholdPct", "windowSec", "minSamples"},
		}),
	},
	{
		ID:              "proxy.hook_timeout_rate",
		DisplayName:     "Proxy Hook Timeout Rate",
		SourceType:      "proxy",
		DefaultSeverity: alerting.SeverityMedium,
		RequiresAck:     false,
		Enabled:         true,
		CooldownSec:     300,
		Params:          mustJSON(map[string]any{"thresholdPct": 10, "windowSec": 300, "minSamples": 10}),
		ParamsSchema: mustJSON(map[string]any{
			"type": "object",
			"properties": map[string]any{
				"thresholdPct": map[string]any{"type": "integer", "minimum": 1, "maximum": 100},
				"windowSec":    map[string]any{"type": "integer", "minimum": 60},
				"minSamples":   map[string]any{"type": "integer", "minimum": 1},
			},
			"required": []string{"thresholdPct", "windowSec", "minSamples"},
		}),
	},
	{
		ID:              "proxy.high_error_rate",
		DisplayName:     "High 5xx Error Rate",
		SourceType:      "proxy",
		DefaultSeverity: alerting.SeverityHigh,
		RequiresAck:     false,
		Enabled:         true,
		CooldownSec:     300,
		Params:          mustJSON(map[string]any{"thresholdPct": 10, "windowSec": 300, "minSamples": 10}),
		ParamsSchema: mustJSON(map[string]any{
			"type": "object",
			"properties": map[string]any{
				"thresholdPct": map[string]any{"type": "integer", "minimum": 1, "maximum": 100},
				"windowSec":    map[string]any{"type": "integer", "minimum": 60},
				"minSamples":   map[string]any{"type": "integer", "minimum": 1},
			},
			"required": []string{"thresholdPct", "windowSec", "minSamples"},
		}),
	},
	{
		ID:              "proxy.cost_spike",
		DisplayName:     "Cost Spike",
		SourceType:      "proxy",
		DefaultSeverity: alerting.SeverityCritical,
		RequiresAck:     true,
		Enabled:         true,
		CooldownSec:     3600,
		Params:          mustJSON(map[string]any{"thresholdUsd": 100, "windowSec": 3600}),
		ParamsSchema: mustJSON(map[string]any{
			"type": "object",
			"properties": map[string]any{
				"thresholdUsd": map[string]any{"type": "number", "minimum": 0.01},
				"windowSec":    map[string]any{"type": "integer", "minimum": 60},
			},
			"required": []string{"thresholdUsd", "windowSec"},
		}),
	},
	{
		ID:              "thing.offline",
		DisplayName:     "Thing Offline",
		SourceType:      "thing",
		DefaultSeverity: alerting.SeverityHigh,
		RequiresAck:     false,
		Enabled:         true,
		CooldownSec:     300,
		Params:          mustJSON(map[string]any{"offlineAfterSec": 300, "excludeKinds": []string{}}),
		ParamsSchema: mustJSON(map[string]any{
			"type": "object",
			"properties": map[string]any{
				"offlineAfterSec": map[string]any{"type": "integer", "minimum": 60},
				"excludeKinds":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			},
			"required": []string{"offlineAfterSec"},
		}),
	},
	{
		ID:              "provider.unavailable",
		DisplayName:     "Provider Unavailable",
		SourceType:      "provider",
		DefaultSeverity: alerting.SeverityCritical,
		RequiresAck:     true,
		Enabled:         true,
		CooldownSec:     600,
		Params:          mustJSON(map[string]any{"minDownSec": 120, "recoverySec": 60}),
		ParamsSchema: mustJSON(map[string]any{
			"type": "object",
			"properties": map[string]any{
				"minDownSec":  map[string]any{"type": "integer", "minimum": 30},
				"recoverySec": map[string]any{"type": "integer", "minimum": 30},
			},
			"required": []string{"minDownSec", "recoverySec"},
		}),
	},
	{
		ID:              "system.channel_test",
		DisplayName:     "Channel Test (synthetic)",
		SourceType:      "system",
		DefaultSeverity: alerting.SeverityInfo,
		RequiresAck:     false,
		Enabled:         true,
		CooldownSec:     0,
		Params:          mustJSON(map[string]any{}),
		ParamsSchema:    mustJSON(map[string]any{"type": "object"}),
	},
	{
		ID:              "hook.reject_rate",
		DisplayName:     "Hook Reject Rate",
		SourceType:      "proxy",
		DefaultSeverity: alerting.SeverityHigh,
		RequiresAck:     false,
		Enabled:         true,
		CooldownSec:     300,
		Params: mustJSON(map[string]any{
			"thresholdPct":  5,
			"windowSec":     300,
			"minSamples":    20,
			"decisionTypes": []string{"REJECT_HARD", "BLOCK_SOFT"},
		}),
		ParamsSchema: mustJSON(map[string]any{
			"type": "object",
			"properties": map[string]any{
				"thresholdPct":  map[string]any{"type": "integer", "minimum": 1, "maximum": 100},
				"windowSec":     map[string]any{"type": "integer", "minimum": 60},
				"minSamples":    map[string]any{"type": "integer", "minimum": 1},
				"decisionTypes": map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": []string{"REJECT_HARD", "BLOCK_SOFT"}}},
			},
			"required": []string{"thresholdPct", "windowSec", "minSamples"},
		}),
	},
	{
		ID:              "vk.traffic_spike",
		DisplayName:     "VK Traffic Spike",
		SourceType:      "proxy",
		DefaultSeverity: alerting.SeverityCritical,
		RequiresAck:     true,
		Enabled:         true,
		CooldownSec:     600,
		Params: mustJSON(map[string]any{
			"spikeMultiplier":   10,
			"baselineWindowSec": 3600,
			"alertWindowSec":    300,
			"absFloorReq":       50,
			"coldStartHours":    24,
		}),
		ParamsSchema: mustJSON(map[string]any{
			"type": "object",
			"properties": map[string]any{
				"spikeMultiplier":   map[string]any{"type": "number", "minimum": 2},
				"baselineWindowSec": map[string]any{"type": "integer", "minimum": 300},
				"alertWindowSec":    map[string]any{"type": "integer", "minimum": 60},
				"absFloorReq":       map[string]any{"type": "integer", "minimum": 1},
				"coldStartHours":    map[string]any{"type": "integer", "minimum": 0},
			},
			"required": []string{"spikeMultiplier", "baselineWindowSec", "alertWindowSec", "absFloorReq"},
		}),
	},
	{
		ID:              "auth.login_failure_rate",
		DisplayName:     "Login Failure Flood",
		SourceType:      "auth",
		DefaultSeverity: alerting.SeverityHigh,
		RequiresAck:     false,
		Enabled:         true,
		CooldownSec:     300,
		Params: mustJSON(map[string]any{
			"thresholdCount": 20,
			"windowSec":      300,
			"groupBy":        "ip",
		}),
		ParamsSchema: mustJSON(map[string]any{
			"type": "object",
			"properties": map[string]any{
				"thresholdCount": map[string]any{"type": "integer", "minimum": 1},
				"windowSec":      map[string]any{"type": "integer", "minimum": 60},
				"groupBy":        map[string]any{"type": "string", "enum": []string{"ip", "email", "all"}},
			},
			"required": []string{"thresholdCount", "windowSec", "groupBy"},
		}),
	},
	{
		ID:              "proxy.rate_limit_exceeded",
		DisplayName:     "Rate Limit Exceeded",
		SourceType:      "proxy",
		DefaultSeverity: alerting.SeverityHigh,
		RequiresAck:     false,
		Enabled:         true,
		CooldownSec:     300,
		Params:          mustJSON(map[string]any{"thresholdCount": 30, "windowSec": 300, "groupBy": "vk"}),
		ParamsSchema: mustJSON(map[string]any{
			"type": "object",
			"properties": map[string]any{
				"thresholdCount": map[string]any{"type": "integer", "minimum": 1},
				"windowSec":      map[string]any{"type": "integer", "minimum": 60},
				"groupBy":        map[string]any{"type": "string", "enum": []string{"vk", "ip", "all"}},
			},
			"required": []string{"thresholdCount", "windowSec", "groupBy"},
		}),
	},
	{
		ID:              "proxy.quota_runtime_exceeded",
		DisplayName:     "Quota Runtime Exceeded",
		SourceType:      "proxy",
		DefaultSeverity: alerting.SeverityHigh,
		RequiresAck:     false,
		Enabled:         true,
		CooldownSec:     300,
		Params:          mustJSON(map[string]any{"thresholdCount": 10, "windowSec": 300}),
		ParamsSchema: mustJSON(map[string]any{
			"type": "object",
			"properties": map[string]any{
				"thresholdCount": map[string]any{"type": "integer", "minimum": 1},
				"windowSec":      map[string]any{"type": "integer", "minimum": 60},
			},
			"required": []string{"thresholdCount", "windowSec"},
		}),
	},
	{
		ID:              "proxy.routing_no_match",
		DisplayName:     "Routing No-Match",
		SourceType:      "proxy",
		DefaultSeverity: alerting.SeverityMedium,
		RequiresAck:     false,
		Enabled:         true,
		CooldownSec:     600,
		Params:          mustJSON(map[string]any{"thresholdCount": 20, "windowSec": 600}),
		ParamsSchema: mustJSON(map[string]any{
			"type": "object",
			"properties": map[string]any{
				"thresholdCount": map[string]any{"type": "integer", "minimum": 1},
				"windowSec":      map[string]any{"type": "integer", "minimum": 60},
			},
			"required": []string{"thresholdCount", "windowSec"},
		}),
	},
	{
		ID:              "auth.invalid_key_burst",
		DisplayName:     "Invalid API Key Burst",
		SourceType:      "auth",
		DefaultSeverity: alerting.SeverityHigh,
		RequiresAck:     false,
		Enabled:         true,
		CooldownSec:     300,
		Params:          mustJSON(map[string]any{"thresholdCount": 20, "windowSec": 300}),
		ParamsSchema: mustJSON(map[string]any{
			"type": "object",
			"properties": map[string]any{
				"thresholdCount": map[string]any{"type": "integer", "minimum": 1},
				"windowSec":      map[string]any{"type": "integer", "minimum": 60},
			},
			"required": []string{"thresholdCount", "windowSec"},
		}),
	},
	{
		ID:              "provider.upstream_error",
		DisplayName:     "Upstream Provider Error Rate",
		SourceType:      "provider",
		DefaultSeverity: alerting.SeverityHigh,
		RequiresAck:     false,
		Enabled:         true,
		CooldownSec:     300,
		Params:          mustJSON(map[string]any{"thresholdPct": 10, "windowSec": 300, "minSamples": 20}),
		ParamsSchema: mustJSON(map[string]any{
			"type": "object",
			"properties": map[string]any{
				"thresholdPct": map[string]any{"type": "integer", "minimum": 1, "maximum": 100},
				"windowSec":    map[string]any{"type": "integer", "minimum": 60},
				"minSamples":   map[string]any{"type": "integer", "minimum": 1},
			},
			"required": []string{"thresholdPct", "windowSec", "minSamples"},
		}),
	},
	{
		ID:              "provider.high_latency_percentile",
		DisplayName:     "Provider Latency p95 Spike",
		SourceType:      "provider",
		DefaultSeverity: alerting.SeverityMedium,
		RequiresAck:     false,
		Enabled:         true,
		CooldownSec:     600,
		Params: mustJSON(map[string]any{
			"percentile": 95, "alertWindowSec": 300, "baselineWindowSec": 3600,
			"multiplier": 2.0, "absFloorMs": 1000, "minSamples": 50,
		}),
		ParamsSchema: mustJSON(map[string]any{
			"type": "object",
			"properties": map[string]any{
				"percentile":        map[string]any{"type": "number", "minimum": 50, "maximum": 99},
				"alertWindowSec":    map[string]any{"type": "integer", "minimum": 60},
				"baselineWindowSec": map[string]any{"type": "integer", "minimum": 300},
				"multiplier":        map[string]any{"type": "number", "minimum": 1.1},
				"absFloorMs":        map[string]any{"type": "number", "minimum": 0},
				"minSamples":        map[string]any{"type": "integer", "minimum": 1},
			},
			"required": []string{"percentile", "alertWindowSec", "baselineWindowSec", "multiplier", "absFloorMs"},
		}),
	},
	{
		ID:              "model.rate_limited_responses",
		DisplayName:     "Upstream 429 Throttle",
		SourceType:      "provider",
		DefaultSeverity: alerting.SeverityMedium,
		RequiresAck:     false,
		Enabled:         true,
		CooldownSec:     300,
		Params:          mustJSON(map[string]any{"thresholdCount": 10, "windowSec": 300}),
		ParamsSchema: mustJSON(map[string]any{
			"type": "object",
			"properties": map[string]any{
				"thresholdCount": map[string]any{"type": "integer", "minimum": 1},
				"windowSec":      map[string]any{"type": "integer", "minimum": 60},
			},
			"required": []string{"thresholdCount", "windowSec"},
		}),
	},
	{
		ID:              "credential.auth_failures_cascade",
		DisplayName:     "Credential 401/403 Cascade",
		SourceType:      "provider",
		DefaultSeverity: alerting.SeverityHigh,
		RequiresAck:     false,
		Enabled:         true,
		CooldownSec:     600,
		Params:          mustJSON(map[string]any{"thresholdPct": 20, "windowSec": 600, "minSamples": 10}),
		ParamsSchema: mustJSON(map[string]any{
			"type": "object",
			"properties": map[string]any{
				"thresholdPct": map[string]any{"type": "integer", "minimum": 1, "maximum": 100},
				"windowSec":    map[string]any{"type": "integer", "minimum": 60},
				"minSamples":   map[string]any{"type": "integer", "minimum": 1},
			},
			"required": []string{"thresholdPct", "windowSec", "minSamples"},
		}),
	},
	{
		ID:              "vk.latency_degradation",
		DisplayName:     "VK Latency p95 Degradation",
		SourceType:      "proxy",
		DefaultSeverity: alerting.SeverityMedium,
		RequiresAck:     false,
		Enabled:         true,
		CooldownSec:     600,
		Params: mustJSON(map[string]any{
			"percentile": 95, "alertWindowSec": 300, "baselineWindowSec": 3600,
			"multiplier": 2.0, "absFloorMs": 1000, "minSamples": 30,
		}),
		ParamsSchema: mustJSON(map[string]any{
			"type": "object",
			"properties": map[string]any{
				"percentile":        map[string]any{"type": "number", "minimum": 50, "maximum": 99},
				"alertWindowSec":    map[string]any{"type": "integer", "minimum": 60},
				"baselineWindowSec": map[string]any{"type": "integer", "minimum": 300},
				"multiplier":        map[string]any{"type": "number", "minimum": 1.1},
				"absFloorMs":        map[string]any{"type": "number", "minimum": 0},
				"minSamples":        map[string]any{"type": "integer", "minimum": 1},
			},
			"required": []string{"percentile", "alertWindowSec", "baselineWindowSec", "multiplier", "absFloorMs"},
		}),
	},
	{
		ID:              "vk.token_usage_spike",
		DisplayName:     "VK Token Usage Spike",
		SourceType:      "proxy",
		DefaultSeverity: alerting.SeverityMedium,
		RequiresAck:     false,
		Enabled:         true,
		CooldownSec:     600,
		Params:          mustJSON(map[string]any{"thresholdTokens": 1000000, "windowSec": 3600}),
		ParamsSchema: mustJSON(map[string]any{
			"type": "object",
			"properties": map[string]any{
				"thresholdTokens": map[string]any{"type": "integer", "minimum": 1},
				"windowSec":       map[string]any{"type": "integer", "minimum": 60},
			},
			"required": []string{"thresholdTokens", "windowSec"},
		}),
	},
	{
		ID:              "compliance.hook_execution_timeout_surge",
		DisplayName:     "Hook Execution Timeout Surge",
		SourceType:      "proxy",
		DefaultSeverity: alerting.SeverityMedium,
		RequiresAck:     false,
		Enabled:         true,
		CooldownSec:     300,
		Params:          mustJSON(map[string]any{"thresholdCount": 20, "windowSec": 300}),
		ParamsSchema: mustJSON(map[string]any{
			"type": "object",
			"properties": map[string]any{
				"thresholdCount": map[string]any{"type": "integer", "minimum": 1},
				"windowSec":      map[string]any{"type": "integer", "minimum": 60},
			},
			"required": []string{"thresholdCount", "windowSec"},
		}),
	},
	{
		ID:              "compliance.payload_capture_failure_rate",
		DisplayName:     "Payload Capture Truncation Rate",
		SourceType:      "proxy",
		DefaultSeverity: alerting.SeverityMedium,
		RequiresAck:     false,
		Enabled:         true,
		CooldownSec:     600,
		Params:          mustJSON(map[string]any{"thresholdPct": 10, "windowSec": 600, "minSamples": 20}),
		ParamsSchema: mustJSON(map[string]any{
			"type": "object",
			"properties": map[string]any{
				"thresholdPct": map[string]any{"type": "integer", "minimum": 1, "maximum": 100},
				"windowSec":    map[string]any{"type": "integer", "minimum": 60},
				"minSamples":   map[string]any{"type": "integer", "minimum": 1},
			},
			"required": []string{"thresholdPct", "windowSec", "minSamples"},
		}),
	},
	{
		ID:              "agent.cert_expiration_imminent",
		DisplayName:     "Agent mTLS Cert Expiring",
		SourceType:      "thing",
		DefaultSeverity: alerting.SeverityMedium,
		RequiresAck:     false,
		Enabled:         true,
		CooldownSec:     86400,
		Params:          mustJSON(map[string]any{"warnDays": []int{30, 14, 7, 1}}),
		ParamsSchema: mustJSON(map[string]any{
			"type": "object",
			"properties": map[string]any{
				"warnDays": map[string]any{"type": "array", "items": map[string]any{"type": "integer", "minimum": 1}},
			},
			"required": []string{"warnDays"},
		}),
	},
	{
		ID:              "credential.expiring",
		DisplayName:     "Credential Expiring",
		SourceType:      "provider",
		DefaultSeverity: alerting.SeverityMedium,
		RequiresAck:     false,
		Enabled:         true,
		CooldownSec:     86400,
		Params:          mustJSON(map[string]any{"warnDays": []int{14, 7, 1}}),
		ParamsSchema: mustJSON(map[string]any{
			"type": "object",
			"properties": map[string]any{
				"warnDays": map[string]any{"type": "array", "items": map[string]any{"type": "integer", "minimum": 1}},
			},
			"required": []string{"warnDays"},
		}),
	},
	{
		ID:              "credential.stale_last_success",
		DisplayName:     "Credential Idle (no recent success)",
		SourceType:      "provider",
		DefaultSeverity: alerting.SeverityLow,
		RequiresAck:     false,
		Enabled:         true,
		CooldownSec:     86400,
		Params:          mustJSON(map[string]any{"staleAfterDays": 7}),
		ParamsSchema: mustJSON(map[string]any{
			"type": "object",
			"properties": map[string]any{
				"staleAfterDays": map[string]any{"type": "integer", "minimum": 1},
			},
			"required": []string{"staleAfterDays"},
		}),
	},
	// credential.circuit_open / credential.health_unavailable /
	// credential.health_degraded_sustained are raised by the
	// credential-reliability-alerts job (packages/nexus-hub/internal/jobs/defs/credential)
	// from the persisted reliability state on the Credential table. Per
	// docs/developers/architecture/control-plane/credentials-architecture.md
	// they take no operator-tunable params today; the schema is intentionally
	// empty so the admin UI's "Reset Rule" button does not surface knobs the
	// job ignores. Three rules kept symmetric (sourceType=provider,
	// requiresAck=false, enabled=true) — only severity + cooldownSec differ.
	{
		ID:              "credential.circuit_open",
		DisplayName:     "Credential Circuit Breaker Open",
		SourceType:      "provider",
		DefaultSeverity: alerting.SeverityHigh,
		RequiresAck:     false,
		Enabled:         true,
		CooldownSec:     300,
		Params:          mustJSON(map[string]any{}),
		ParamsSchema: mustJSON(map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		}),
	},
	{
		ID:              "credential.health_unavailable",
		DisplayName:     "Credential Unavailable",
		SourceType:      "provider",
		DefaultSeverity: alerting.SeverityHigh,
		RequiresAck:     false,
		Enabled:         true,
		CooldownSec:     300,
		Params:          mustJSON(map[string]any{}),
		ParamsSchema: mustJSON(map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		}),
	},
	{
		ID:              "credential.health_degraded_sustained",
		DisplayName:     "Credential Degraded (sustained)",
		SourceType:      "provider",
		DefaultSeverity: alerting.SeverityMedium,
		RequiresAck:     false,
		Enabled:         true,
		CooldownSec:     1800,
		Params:          mustJSON(map[string]any{}),
		ParamsSchema: mustJSON(map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		}),
	},
}
