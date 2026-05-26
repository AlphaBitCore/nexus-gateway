package core

import "fmt"

// formatTargetFriendly renders a RoutingTarget for trace decision strings.
// The output combines the customer-facing identifiers operators recognise
// (provider display name + model code + display name) with no UUIDs — the
// caller is expected to append the UUID pair in `[<providerID>/<modelID>]`
// form for FK debugging.
//
// Format: `<providerName>/<modelCode> ("<modelName>")`. Empty fields are
// substituted with `?` so the structure stays parseable when a target
// row is partial (e.g. an unhealthy lookup that only filled IDs).
// FormatTargetFriendly renders a RoutingTarget for trace decision strings.
func FormatTargetFriendly(t *RoutingTarget) string {
	if t == nil {
		return "?/? (\"?\")"
	}
	provider := orQuestion(t.ProviderName)
	code := orQuestion(t.ModelCode)
	name := orQuestion(t.ModelName)
	return fmt.Sprintf("%s/%s (%q)", provider, code, name)
}

// FormatTargetPath renders a RoutingTarget for branches[].path. Friendly
// only: the surrounding BranchedTarget JSON object already exposes
// providerId / modelId at the top level, so duplicating UUIDs in the path
// is just noise.
func FormatTargetPath(t *RoutingTarget) string {
	if t == nil {
		return "?/?"
	}
	return fmt.Sprintf("%s/%s", orQuestion(t.ProviderName), orQuestion(t.ModelCode))
}

func orQuestion(s string) string {
	if s == "" {
		return "?"
	}
	return s
}
