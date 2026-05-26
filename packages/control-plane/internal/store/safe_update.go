package store

import (
	"fmt"
	"strings"
)

// SafeUpdateBuilder builds a parameterized SET clause from a map of updates,
// validating every key against an explicit allowlist to prevent SQL injection.
type SafeUpdateBuilder struct {
	allowed map[string]string // JSON key → SQL column expression
}

// NewSafeUpdateBuilder creates a builder with the given allowed columns.
// Keys are the JSON field names callers use; values are the SQL column expressions
// (e.g. `"displayName"` with quotes for camelCase columns, or `name` without).
func NewSafeUpdateBuilder(allowed map[string]string) *SafeUpdateBuilder {
	return &SafeUpdateBuilder{allowed: allowed}
}

// Build filters the updates map through the allowlist and produces a SET clause.
// Returns the SET clause (without leading "SET"), the args slice (starting after idArg),
// the next arg index, and an error if any key is not in the allowlist.
// If strict is true, unknown keys cause an error; if false, they are silently skipped.
func (b *SafeUpdateBuilder) Build(updates map[string]any, startArgIdx int, strict bool) (setClause string, args []any, nextArgIdx int, err error) {
	var parts []string
	argIdx := startArgIdx

	for jsonKey, val := range updates {
		sqlCol, ok := b.allowed[jsonKey]
		if !ok {
			if strict {
				return "", nil, argIdx, fmt.Errorf("invalid update field: %q", jsonKey)
			}
			continue
		}
		parts = append(parts, fmt.Sprintf(`%s = $%d`, sqlCol, argIdx))
		args = append(args, val)
		argIdx++
	}

	return strings.Join(parts, ", "), args, argIdx, nil
}

// --- Per-entity column allowlists ---

var ModelUpdateColumns = map[string]string{
	"name": "name", "description": "description", "type": "type",
	"inputPricePerMillion": `"inputPricePerMillion"`, "outputPricePerMillion": `"outputPricePerMillion"`,
	"maxContextTokens": `"maxContextTokens"`, "maxOutputTokens": `"maxOutputTokens"`,
	"status": "status", "aliases": "aliases", "enabled": "enabled", "features": "features",
}

var CredentialUpdateColumns = map[string]string{
	"name": "name", "enabled": "enabled",
	"rotationState": `"rotationState"`, "lastRotatedAt": `"lastRotatedAt"`,
}

var VirtualKeyUpdateColumns = map[string]string{
	"projectId": `"projectId"`, "sourceApp": `"sourceApp"`,
	"enabled": "enabled", "rateLimitRpm": `"rateLimitRpm"`,
	"allowedModels": `"allowedModels"`,
}

var RoutingRuleUpdateColumns = map[string]string{
	"name": "name", "description": "description", "strategyType": `"strategyType"`,
	"config": "config", "matchConditions": `"matchConditions"`, "modelId": `"modelId"`,
	"priority": "priority", "enabled": "enabled", "pipelineStage": `"pipelineStage"`,
	"fallbackChain": `"fallbackChain"`,
}

var QuotaUpdateColumns = map[string]string{
	"tokenLimit": `"tokenLimit"`, "costLimitUsd": `"costLimitUsd"`,
	"enforcementMode": `"enforcementMode"`, "periodType": `"periodType"`,
	"resetAt": `"resetAt"`,
}

var HookConfigUpdateColumns = map[string]string{
	"name": "name", "type": "type", "implementationId": `"implementationId"`,
	"stage": "stage", "category": "category", "endpoint": "endpoint",
	"script": "script", "config": "config", "priority": "priority",
	"timeoutMs": `"timeoutMs"`, "failBehavior": `"failBehavior"`, "enabled": "enabled",
}

var OrganizationUpdateColumns = map[string]string{
	"name": "name", "code": "code", "description": "description",
	"enabled": "enabled", "parentId": `"parentId"`,
	"contactName": `"contactName"`, "contactEmail": `"contactEmail"`, "contactPhone": `"contactPhone"`,
}

var ProjectUpdateColumns = map[string]string{
	"name": "name", "code": "code", "description": "description", "status": "status",
	"organizationId": `"organizationId"`, "contactName": `"contactName"`, "contactEmail": `"contactEmail"`,
}

var NexusUserUpdateColumns = map[string]string{
	"email": "email", "status": "status",
	"passwordHash": `"passwordHash"`,
}

var AdminAPIKeyUpdateColumns = map[string]string{
	"name": "name", "enabled": "enabled", "expiresAt": `"expiresAt"`,
}

var IamPolicyUpdateColumns = map[string]string{
	"name": "name", "description": "description", "enabled": "enabled", "document": "document",
}

var IamGroupUpdateColumns = map[string]string{
	"name": "name", "description": "description",
}

var DeviceGroupUpdateColumns = map[string]string{
	"name": "name", "description": "description",
}

var DSARUpdateColumns = map[string]string{
	"status": "status", "notes": "notes", "completed_at": "completed_at",
	"outcome": "outcome", "updated_by": "updated_by",
}

// AnalyticsGroupByColumns is the allowlist for groupBy column names in analytics queries.
var AnalyticsGroupByColumns = map[string]bool{
	"provider": true, `"modelUsed"`: true, `"projectId"`: true,
	`"organizationId"`: true, `"userId"`: true,
	`"virtualKeyId"`: true, `"routedProvider"`: true, `"routingRuleId"`: true,
}
