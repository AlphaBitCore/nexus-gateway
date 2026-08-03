package trafficstore

import (
	"fmt"
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/domain"
)

// buildTrafficEventWhere translates TrafficEventListParams into the WHERE
// clause + bind args shared by the traffic list's COUNT and page queries.
// Every predicate is parameterized; the only literals interpolated into the
// SQL text are placeholder indexes.
func buildTrafficEventWhere(p TrafficEventListParams) (string, []any, int) {
	args := []any{}
	argIdx := 1

	// Source filter — handler supplies DB values already mapped from UI domain.
	// Empty = all data-plane sources (every CHECK-allowed value).
	sources := p.DBSources
	if len(sources) == 0 {
		sources = domain.AllDataPlaneDBSources()
	}
	placeholders := make([]string, len(sources))
	for i, s := range sources {
		placeholders[i] = fmt.Sprintf("$%d", argIdx)
		args = append(args, s)
		argIdx++
	}
	where := fmt.Sprintf("WHERE a.source IN (%s)", strings.Join(placeholders, ","))

	if p.Provider == FilterNone {
		// Error-governance drill-down: rows that never resolved a provider
		// (early rejections). Without the sentinel an empty-provider class
		// would have to omit the filter and match EVERY provider.
		where += ` AND COALESCE(a.routed_provider_name, a.provider_name, '') = ''`
	} else if p.Provider != "" {
		// Filter by the provider that actually SERVED the request (routed),
		// falling back to the requested provider for non-ai-gateway rows. The
		// requested provider_name is NULL for OpenAI-style / "auto" traffic, so
		// matching it would drop exactly the rows the served provider handled —
		// and it would disagree with the analytics layer, which attributes by
		// routed_provider.
		where += fmt.Sprintf(` AND COALESCE(a.routed_provider_name, a.provider_name) = $%d`, argIdx)
		args = append(args, p.Provider)
		argIdx++
	}
	if p.EntityID != "" {
		where += fmt.Sprintf(` AND a.entity_id = $%d`, argIdx)
		args = append(args, p.EntityID)
		argIdx++
	}
	if p.OrgID != "" {
		where += fmt.Sprintf(` AND a.org_id = $%d`, argIdx)
		args = append(args, p.OrgID)
		argIdx++
	}
	if p.EntityType != "" {
		where += fmt.Sprintf(` AND a.entity_type = $%d`, argIdx)
		args = append(args, p.EntityType)
		argIdx++
	}
	if p.ProjectID != "" {
		where += fmt.Sprintf(` AND a.identity->'project'->>'id' = $%d`, argIdx)
		args = append(args, p.ProjectID)
		argIdx++
	}
	if p.VirtualKeyID != "" {
		// identity.vk.id is the Virtual Key the client presented (what the
		// `?virtualKeyId=` filter is meant to match). identity.apiCredential
		// is something else entirely — the upstream provider's API key
		// Nexus used to make the upstream call (real OpenAI / Anthropic
		// token, totally different identifier). Producers renamed the
		// VK key from "credential" to "vk" (see
		// ai-gateway audit_test.go assertion 'Identity.credential should
		// not exist — renamed to identity.vk'); this query was missed in
		// that rename, so filtering by virtualKeyId silently returned
		// zero rows. Fix: query the right JSON path.
		where += fmt.Sprintf(` AND a.identity->'vk'->>'id' = $%d`, argIdx)
		args = append(args, p.VirtualKeyID)
		argIdx++
	}
	if p.ModelUsed != "" {
		// Match the served model first (what cost/usage attribute to), falling
		// back to the requested literal so a search still finds rows where
		// routing did not substitute.
		where += fmt.Sprintf(` AND COALESCE(a.routed_model_name, a.model_name) ILIKE $%d`, argIdx)
		args = append(args, "%"+escapeILIKE(p.ModelUsed)+"%")
		argIdx++
	}
	if p.ModelExact == FilterNone {
		where += ` AND COALESCE(a.routed_model_name, a.model_name, '') = ''`
	} else if p.ModelExact != "" {
		where += fmt.Sprintf(` AND COALESCE(a.routed_model_name, a.model_name) = $%d`, argIdx)
		args = append(args, p.ModelExact)
		argIdx++
	}
	if p.EndpointType != "" {
		where += fmt.Sprintf(` AND a.endpoint_type = $%d`, argIdx)
		args = append(args, p.EndpointType)
		argIdx++
	}
	if p.RequestID != "" {
		where += fmt.Sprintf(` AND a.id = $%d`, argIdx)
		args = append(args, p.RequestID)
		argIdx++
	}
	if p.EndUserID != "" {
		where += fmt.Sprintf(` AND a.end_user_id = $%d`, argIdx)
		args = append(args, p.EndUserID)
		argIdx++
	}
	if p.SessionID != "" {
		where += fmt.Sprintf(` AND a.session_id = $%d`, argIdx)
		args = append(args, p.SessionID)
		argIdx++
	}
	if p.HookDecision != "" {
		where += fmt.Sprintf(` AND a.request_hook_decision = $%d`, argIdx)
		args = append(args, p.HookDecision)
		argIdx++
	}
	if p.ResponseHookDecision != "" {
		where += fmt.Sprintf(` AND a.response_hook_decision = $%d`, argIdx)
		args = append(args, p.ResponseHookDecision)
		argIdx++
	}
	if p.StatusCode != nil {
		where += fmt.Sprintf(` AND a.status_code = $%d`, argIdx)
		args = append(args, *p.StatusCode)
		argIdx++
	} else if p.StatusRange != "" {
		switch p.StatusRange {
		case "2xx":
			where += ` AND a.status_code >= 200 AND a.status_code <= 299`
		case "4xx":
			where += ` AND a.status_code >= 400 AND a.status_code <= 499`
		case "5xx":
			where += ` AND a.status_code >= 500 AND a.status_code <= 599`
		}
	}
	if p.CacheStatus != nil {
		where += fmt.Sprintf(` AND a.cache_status = $%d`, argIdx)
		args = append(args, *p.CacheStatus)
		argIdx++
	}
	if p.TargetHost != "" {
		where += fmt.Sprintf(` AND a.target_host ILIKE $%d`, argIdx)
		args = append(args, "%"+escapeILIKE(p.TargetHost)+"%")
		argIdx++
	}
	if p.Path != "" {
		where += fmt.Sprintf(` AND a.path ILIKE $%d`, argIdx)
		args = append(args, "%"+escapeILIKE(p.Path)+"%")
		argIdx++
	}
	if p.SourceProcess != "" {
		where += fmt.Sprintf(` AND a.source_process ILIKE $%d`, argIdx)
		args = append(args, "%"+escapeILIKE(p.SourceProcess)+"%")
		argIdx++
	}
	if p.BumpStatus != "" {
		where += fmt.Sprintf(` AND a.bump_status = $%d`, argIdx)
		args = append(args, p.BumpStatus)
		argIdx++
	}
	if len(p.ComplianceTags) > 0 {
		// compliance_tags @> $N::text[] matches rows whose tag array
		// contains every supplied tag. Callers pass repeated `?tag=...`
		// query params — the filter behaves as AND across tags, so a
		// row must carry all of them to match.
		where += fmt.Sprintf(` AND a.compliance_tags @> $%d::text[]`, argIdx)
		args = append(args, p.ComplianceTags)
		argIdx++
	}
	if p.APIKeyFingerprint != "" {
		where += fmt.Sprintf(` AND a.api_key_fingerprint = $%d`, argIdx)
		args = append(args, p.APIKeyFingerprint)
		argIdx++
	}
	if p.UsageExtractionStatus != "" {
		where += fmt.Sprintf(` AND a.usage_extraction_status = $%d`, argIdx)
		args = append(args, p.UsageExtractionStatus)
		argIdx++
	}
	if p.ThingID != "" {
		where += fmt.Sprintf(` AND a.thing_id = $%d`, argIdx)
		args = append(args, p.ThingID)
		argIdx++
	}
	if p.RoutingRuleID != "" {
		where += fmt.Sprintf(` AND a.routing_rule_id = $%d`, argIdx)
		args = append(args, p.RoutingRuleID)
		argIdx++
	}
	if p.ErrorCode == ErrorCodeUnclassified {
		// Sentinel from the error-governance drill-down: rows whose
		// producer did not classify the failure (NULL or empty). A plain
		// exact match cannot express IS NULL, and without this the
		// unclassified group's drill-down over-included classified rows.
		where += ` AND (a.error_code IS NULL OR a.error_code = '')`
	} else if p.ErrorCode != "" {
		where += fmt.Sprintf(` AND a.error_code = $%d`, argIdx)
		args = append(args, p.ErrorCode)
		argIdx++
	}
	if p.ExcludeInternal {
		// internal_purpose is nullable; treat empty strings as "not internal"
		// too so a buggy producer that sends '' instead of omitting the field
		// still routes into the customer view.
		where += ` AND (a.internal_purpose IS NULL OR a.internal_purpose = '')`
	}
	if p.StartTime != nil {
		where += fmt.Sprintf(` AND a.timestamp >= $%d`, argIdx)
		args = append(args, *p.StartTime)
		argIdx++
	}
	if p.EndTime != nil {
		where += fmt.Sprintf(` AND a.timestamp <= $%d`, argIdx)
		args = append(args, *p.EndTime)
		argIdx++
	}

	return where, args, argIdx
}
