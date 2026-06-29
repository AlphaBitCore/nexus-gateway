package matcher

import (
	"fmt"
	"github.com/goccy/go-json"
	"regexp"
	"strings"
	"sync"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
)

const (
	maxRegexLen   = 200
	maxRegexCache = 512
)

var (
	regexMu    sync.RWMutex
	regexCache = make(map[string]*regexp.Regexp)
)

// EvaluateExpression evaluates a MongoDB-style match expression against a
// routing context. All top-level fields are AND'd. Returns true if all match.
func EvaluateExpression(expr map[string]any, ctx *core.RoutingContext) bool {
	for field, condition := range expr {
		if field == "$and" {
			if !evalLogicalAnd(condition, ctx) {
				return false
			}
			continue
		}
		if field == "$or" {
			if !evalLogicalOr(condition, ctx) {
				return false
			}
			continue
		}

		fieldValue := resolveField(field, ctx)

		switch cond := condition.(type) {
		case map[string]any:
			if !evalOperators(fieldValue, cond, ctx) {
				return false
			}
		default:
			// Implicit $eq.
			if !equals(fieldValue, cond) {
				return false
			}
		}
	}
	return true
}

// resolveField extracts a value from core.RoutingContext using a dotted path.
// Uses direct field access instead of building a temporary map.
func resolveField(path string, ctx *core.RoutingContext) any {
	switch path {
	case "requestedModel.id":
		return ctx.RequestedModel.ID
	case "requestedModel.type":
		return ctx.RequestedModel.Type
	case "requestedModel.providerId":
		return ctx.RequestedModel.ProviderID
	case "requestedModel.providerModelId":
		return ctx.RequestedModel.ProviderModelID
	case "endpointType":
		// Returns the canonical typology.EndpointKind value (e.g. "chat",
		// "embeddings"). Routing rule conditions that filter on endpoint
		// type compare against these canonical kind strings.
		return string(ctx.EndpointType)
	case "virtualKey.id":
		if ctx.VirtualKey != nil {
			return ctx.VirtualKey.ID
		}
		return nil
	case "virtualKey.name":
		if ctx.VirtualKey != nil {
			return ctx.VirtualKey.Name
		}
		return nil
	case "virtualKey.projectId":
		if ctx.VirtualKey != nil {
			return ctx.VirtualKey.ProjectID
		}
		return nil
	case "virtualKey.organizationId":
		if ctx.VirtualKey != nil {
			return ctx.VirtualKey.OrganizationID
		}
		return nil
	case "virtualKey.sourceApp":
		if ctx.VirtualKey != nil {
			return ctx.VirtualKey.SourceApp
		}
		return nil
	default:
		if after, ok := strings.CutPrefix(path, "headers."); ok {
			return ctx.Headers.Get(after)
		}
		return nil
	}
}

func evalOperators(fieldValue any, ops map[string]any, ctx *core.RoutingContext) bool {
	for op, opVal := range ops {
		switch op {
		case "$eq":
			if !equals(fieldValue, opVal) {
				return false
			}
		case "$ne":
			if equals(fieldValue, opVal) {
				return false
			}
		case "$gt":
			if !numCompare(fieldValue, opVal, func(a, b float64) bool { return a > b }) {
				return false
			}
		case "$gte":
			if !numCompare(fieldValue, opVal, func(a, b float64) bool { return a >= b }) {
				return false
			}
		case "$lt":
			if !numCompare(fieldValue, opVal, func(a, b float64) bool { return a < b }) {
				return false
			}
		case "$lte":
			if !numCompare(fieldValue, opVal, func(a, b float64) bool { return a <= b }) {
				return false
			}
		case "$in":
			if !evalIn(fieldValue, opVal) {
				return false
			}
		case "$nin":
			if evalIn(fieldValue, opVal) {
				return false
			}
		case "$regex":
			if !evalRegex(fieldValue, opVal) {
				return false
			}
		case "$not":
			if subOps, ok := opVal.(map[string]any); ok {
				if evalOperators(fieldValue, subOps, ctx) {
					return false
				}
			}
		case "$and":
			if !evalLogicalAnd(opVal, ctx) {
				return false
			}
		case "$or":
			if !evalLogicalOr(opVal, ctx) {
				return false
			}
		}
	}
	return true
}

func evalLogicalAnd(val any, ctx *core.RoutingContext) bool {
	exprs, ok := val.([]any)
	if !ok {
		return false
	}
	for _, e := range exprs {
		if expr, ok := e.(map[string]any); ok {
			if !EvaluateExpression(expr, ctx) {
				return false
			}
		}
	}
	return true
}

func evalLogicalOr(val any, ctx *core.RoutingContext) bool {
	exprs, ok := val.([]any)
	if !ok {
		return false
	}
	for _, e := range exprs {
		if expr, ok := e.(map[string]any); ok {
			if EvaluateExpression(expr, ctx) {
				return true
			}
		}
	}
	return false
}

func evalIn(fieldValue, arr any) bool {
	items, ok := arr.([]any)
	if !ok {
		return false
	}
	for _, item := range items {
		if equals(fieldValue, item) {
			return true
		}
	}
	return false
}

func evalRegex(fieldValue, pattern any) bool {
	patStr, ok := pattern.(string)
	if !ok || len(patStr) > maxRegexLen {
		return false
	}
	fieldStr := fmt.Sprintf("%v", fieldValue)
	re := getCachedRegex(patStr)
	if re == nil {
		return false
	}
	return re.MatchString(fieldStr)
}

func getCachedRegex(pattern string) *regexp.Regexp {
	regexMu.RLock()
	if r, ok := regexCache[pattern]; ok {
		regexMu.RUnlock()
		return r
	}
	regexMu.RUnlock()

	r, err := regexp.Compile(pattern)
	if err != nil {
		return nil
	}

	regexMu.Lock()
	if len(regexCache) >= maxRegexCache {
		regexCache = make(map[string]*regexp.Regexp)
	}
	regexCache[pattern] = r
	regexMu.Unlock()
	return r
}

func equals(a, b any) bool {
	switch av := a.(type) {
	case string:
		if bv, ok := b.(string); ok {
			return av == bv
		}
	case float64:
		if bv, ok := b.(float64); ok {
			return av == bv
		}
	case bool:
		if bv, ok := b.(bool); ok {
			return av == bv
		}
	case int:
		if bv, ok := b.(int); ok {
			return av == bv
		}
	case int64:
		if bv, ok := b.(int64); ok {
			return av == bv
		}
	}
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}

func numCompare(a, b any, cmp func(float64, float64) bool) bool {
	af, aok := toFloat(a)
	bf, bok := toFloat(b)
	if !aok || !bok {
		return false
	}
	return cmp(af, bf)
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

// MatchGlob checks if a string matches a glob pattern (only * is supported).
// Uses the shared regexCache to avoid recompiling on every request.
func MatchGlob(pattern, value string) bool {
	if pattern == "*" {
		return true
	}
	if !strings.Contains(pattern, "*") {
		return pattern == value
	}
	escaped := regexp.QuoteMeta(pattern)
	reStr := "^" + strings.ReplaceAll(escaped, `\*`, ".*") + "$"
	re := getCachedRegex(reStr)
	if re == nil {
		return false
	}
	return re.MatchString(value)
}

// RuleMatchesContext checks if a routing rule's match conditions apply.
func RuleMatchesContext(conds *core.MatchConditions, modelID string, ctx *core.RoutingContext) bool {
	if conds == nil {
		return true
	}

	if len(conds.Models) > 0 {
		if !anyStringSliceContains(conds.Models, ctx.RequestedModel.CandidateIDs) {
			return false
		}
	}
	if len(conds.RequestedModelLiterals) > 0 {
		if !stringSliceContains(conds.RequestedModelLiterals, modelID) {
			return false
		}
	}
	if len(conds.ModelTypes) > 0 && !stringSliceContains(conds.ModelTypes, ctx.RequestedModel.Type) {
		return false
	}
	if len(conds.Providers) > 0 && !stringSliceContains(conds.Providers, ctx.RequestedModel.ProviderID) {
		return false
	}
	if len(conds.Projects) > 0 {
		if ctx.VirtualKey == nil {
			return false
		}
		projectID := ctx.VirtualKey.ProjectID
		if !stringSliceContains(conds.Projects, projectID) {
			return false
		}
	}
	if len(conds.VirtualKeys) > 0 {
		name := ""
		if ctx.VirtualKey != nil {
			name = ctx.VirtualKey.Name
		}
		matched := false
		for _, pattern := range conds.VirtualKeys {
			if MatchGlob(pattern, name) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}

	return true
}

func stringSliceContains(slice []string, val string) bool {
	for _, s := range slice {
		if s == val {
			return true
		}
	}
	return false
}

func anyStringSliceContains(slice []string, candidates []string) bool {
	for _, c := range candidates {
		if c != "" && stringSliceContains(slice, c) {
			return true
		}
	}
	return false
}
