// rule_conditions.go — the STRUCTURED match conditions an admin fills in on the
// rule form. matcher.go holds the expression evaluator, which answers the same
// question in a different notation, plus the shared regex cache and MatchGlob
// that both notations compile through. Both read request facts only.
package matcher

import (
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

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
	// Globs, because the field is a list of KEYWORDS matched against the raw
	// `model` string before the catalogue resolves it — and the raw string is
	// where version suffixes live (`gpt-4o-2024-11-20`,
	// `claude-sonnet-4-5-20250929`), so a pattern is the only way to write a
	// rule that survives the next release. The admin form has always said so,
	// offering `gpt-4-*` as its example, while the comparison was exact: a rule
	// written from that example never matched anything, and nothing said why.
	//
	// A pattern with no `*` still compares exactly, so `auto` — which every
	// smart rule is required to pin — is untouched.
	if len(conds.RequestedModelLiterals) > 0 {
		matched := false
		for _, pattern := range conds.RequestedModelLiterals {
			if MatchGlob(pattern, modelID) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	// Model types are matched against the ENDPOINT the request arrived on, not
	// against the catalogue type of whatever model the caller happened to name.
	//
	// The endpoint is a fact of the request and is always there. The named
	// model's catalogue type is not: an `auto` request names no model, so the
	// field was empty and the comparison failed — every rule carrying a
	// modelTypes condition silently never matched the requests it was most
	// obviously written for, with nothing to read.
	//
	// The stored values are catalogue types (`chat`, `embedding`, `image`) and
	// the endpoint kinds spell things differently (`embeddings`,
	// `image_generation`). Nothing is migrated: the translation is
	// EndpointKindAcceptsModelType, which is the one place that question is
	// answered, so an admin's stored `embedding` keeps meaning the embeddings
	// endpoint and the audio sub-types keep their coarse-`audio` compatibility.
	if len(conds.ModelTypes) > 0 {
		matched := false
		for _, t := range conds.ModelTypes {
			if typology.EndpointKindAcceptsModelType(ctx.EndpointType, t) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	// Providers is INAPPLICABLE when the caller named no model: it asks whether
	// the named model belongs to a provider this rule handles, and reading the
	// absent answer as "no" made a provider-only rule invisible to exactly the
	// `auto` requests it exists for. A request that DID name a model is still
	// compared, against EVERY provider serving that code rather than one of
	// them — the same open-weights model on two hosts is ordinary, the
	// catalogue query states no order, and comparing the first row made the
	// rule fire by that order.
	if len(conds.Providers) > 0 && ctx.RequestedModel.ProviderIsUnknowable() {
		// A condition we cannot evaluate is not a condition that passed. The
		// caller named a model and the catalogue did not answer; treating that
		// like "named nothing" let a rule scoped to one provider match models
		// on every other one and redirect them there.
		return false
	}
	if serving := ctx.RequestedModel.ProviderIDs(); len(conds.Providers) > 0 && len(serving) > 0 {
		matched := false
		for _, p := range serving {
			if stringSliceContains(conds.Providers, p) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
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
