package debug

import (
	"context"
	"github.com/goccy/go-json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/canonicalbridge"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// routeResolver is the subset of *routingcore.Resolver the simulate handler needs.
// Keeping it as an interface lets tests substitute a fake without building a
// real Resolver with a real DB.
//
// Explain is the resolve-plus-branch-enumeration variant: in addition to the
// single stochastic pick that Resolve would make for live traffic, it
// populates plan.Branches with every terminal target reachable from the
// matched primary rule and its cumulative selection probability. Simulate
// uses this so operators can see the full distribution of a loadbalance /
// ab_split / conditional rule, not just the branch one roll happened to hit.
// Explain also HYDRATES rctx in place — it runs Resolve, which resolves the
// request's model string against the catalogue onto RequestedModel.CandidateIDs
// (empty for a routing keyword, which names no catalogue row). The handler
// reads that back to word its warnings, so a fake standing in for a caller who
// named a real model must fill it in; one modelling a keyword must leave it
// empty.
type routeResolver interface {
	Explain(ctx context.Context, rctx *routingcore.RoutingContext) (*routingcore.RoutingPlan, error)
}

// simulateRequest is the JSON body for POST /internal/routing-simulate.
type simulateRequest struct {
	ModelID           string           `json:"modelId"`
	EndpointType      string           `json:"endpointType"`
	IngressBodyFormat string           `json:"ingressBodyFormat,omitempty"`
	Messages          []map[string]any `json:"messages,omitempty"`
}

// simulateRequestEcho echoes the normalized request back in the response.
type simulateRequestEcho struct {
	ModelID           string `json:"modelId"`
	EndpointType      string `json:"endpointType"`
	IngressBodyFormat string `json:"ingressBodyFormat"`
}

// targetEntry is the JSON-tagged projection of routingcore.RoutingTarget.
// routingcore.RoutingTarget has no JSON tags (the engine is used over Go APIs);
// we keep the DTO local so the engine type stays untouched.
//
// ProviderFormat + SchemaMode let simulate consumers see whether each
// target would passthrough, be translated via SchemaCodec, or be
// rejected by the cross-format compatibility check.
type targetEntry struct {
	ProviderID   string `json:"providerId"`
	ProviderName string `json:"providerName"`
	ModelID      string `json:"modelId"`
	// ModelCode is the customer-facing identifier ("gpt-4o") that clients
	// send in `{model: "..."}` requests. Surfaced here so operators can
	// correlate simulate output with their gateway logs without needing
	// to look up the UUID. Distinct from ProviderModelID, which is the
	// upstream vendor's id sent over the wire.
	ModelCode       string `json:"modelCode"`
	ModelName       string `json:"modelName"`
	ProviderModelID string `json:"providerModelId"`
	Source          string `json:"source"`
	ProviderFormat  string `json:"providerFormat,omitempty"`
	SchemaMode      string `json:"schemaMode,omitempty"`
}

// branchEntry projects routingcore.BranchedTarget for the simulate JSON response.
// Branches are the full deterministic enumeration of terminal targets
// reachable from the matched primary rule (loadbalance / ab_split /
// conditional show every branch with its probability); Targets above remains
// the single stochastic pick that live traffic would actually take.
type branchEntry struct {
	ProviderID   string `json:"providerId"`
	ProviderName string `json:"providerName"`
	ModelID      string `json:"modelId"`
	// ModelCode mirrors targetEntry.ModelCode; see comment there.
	ModelCode       string  `json:"modelCode"`
	ModelName       string  `json:"modelName"`
	ProviderModelID string  `json:"providerModelId"`
	Probability     float64 `json:"probability"`
	Path            string  `json:"path"`
	Matched         bool    `json:"matched"`
	Note            string  `json:"note,omitempty"`
}

// simulateResponse is the full decision trace returned to callers.
type simulateResponse struct {
	Request         simulateRequestEcho              `json:"request"`
	OriginalModelID string                           `json:"originalModelId"`
	Substituted     bool                             `json:"substituted"`
	RuleID          string                           `json:"ruleId,omitempty"`
	RuleName        string                           `json:"ruleName,omitempty"`
	Stages          []routingcore.PipelineTraceEntry `json:"stages"`
	Trace           []routingcore.TraceEntry         `json:"trace"`
	Targets         []targetEntry                    `json:"targets"`
	RecoveryTargets []targetEntry                    `json:"recoveryTargets"`
	Branches        []branchEntry                    `json:"branches"`
	Warnings        []string                         `json:"warnings,omitempty"`
}

// simulateMessagesToNormalizedPayload builds a synthetic
// *normcore.NormalizedPayload from the simulate endpoint's free-form
// messages array. Each entry is expected to have string "role" and
// string "content" keys (matching the OpenAI chat shape an operator
// would paste in); missing role defaults to "user", missing or non-string
// content yields an empty Content block.
//
// Returns nil for an empty slice so consumers that probe rctx.Request
// for "is this an AI-shape request" see the same answer they would for
// a non-AI endpoint.
func simulateMessagesToNormalizedPayload(msgs []map[string]any) *normcore.NormalizedPayload {
	if len(msgs) == 0 {
		return nil
	}
	out := &normcore.NormalizedPayload{
		Kind:             normcore.KindAIChat,
		NormalizeVersion: normcore.SchemaVersion,
		Protocol:         "synthetic",
		Messages:         make([]normcore.Message, 0, len(msgs)),
	}
	for _, m := range msgs {
		role := normcore.RoleUser
		if r, ok := m["role"].(string); ok && r != "" {
			role = normcore.Role(r)
		}
		text := ""
		if c, ok := m["content"].(string); ok {
			text = c
		}
		out.Messages = append(out.Messages, normcore.Message{
			Role:    role,
			Content: []normcore.ContentBlock{{Type: normcore.ContentText, Text: text}},
		})
	}
	return out
}

// normalizeEndpointType clamps the admin-supplied endpointType to a
// canonical typology.EndpointKind string. Empty defaults to "chat" (the
// kind admins simulate most). Unknown values pass through so the UI can
// still render the plan for any future endpoint kind the proxy adds.
func normalizeEndpointType(in string) string {
	if strings.TrimSpace(in) == "" {
		return string(typology.EndpointKindChat)
	}
	return in
}

// normalizeIngressBodyFormat returns a valid [provcore.Format] or
// the OpenAI-compat default when the caller left the field empty.
// Unknown values fall back to OpenAI rather than failing the simulate
// so the UI can still render the plan; the per-target `schemaMode`
// column will reflect the effective choice.
func normalizeIngressBodyFormat(in string) provcore.Format {
	f := provcore.Format(strings.ToLower(strings.TrimSpace(in)))
	if f == "" {
		return provcore.FormatOpenAI
	}
	if f.Valid() {
		return f
	}
	return provcore.FormatOpenAI
}

func projectTargets(in []routingcore.RoutingTarget, ingress provcore.Format, ep typology.WireShape, bridge canonicalbridge.API) []targetEntry {
	out := make([]targetEntry, 0, len(in))
	for _, t := range in {
		var providerFormat, mode string
		if pf := provcore.Format(t.AdapterType); pf.Valid() {
			providerFormat = string(pf)
			if ingress != "" {
				mode = schemaMode(ingress, pf, ep, bridge)
			}
		}
		out = append(out, targetEntry{
			ProviderID:      t.ProviderID,
			ProviderName:    t.ProviderName,
			ModelID:         t.ModelID,
			ModelCode:       t.ModelCode,
			ModelName:       t.ModelName,
			ProviderModelID: t.ProviderModelID,
			Source:          t.Source,
			ProviderFormat:  providerFormat,
			SchemaMode:      mode,
		})
	}
	return out
}

func projectBranches(in []routingcore.BranchedTarget) []branchEntry {
	out := make([]branchEntry, 0, len(in))
	for _, b := range in {
		out = append(out, branchEntry{
			ProviderID:      b.Target.ProviderID,
			ProviderName:    b.Target.ProviderName,
			ModelID:         b.Target.ModelID,
			ModelCode:       b.Target.ModelCode,
			ModelName:       b.Target.ModelName,
			ProviderModelID: b.Target.ProviderModelID,
			Probability:     b.Probability,
			Path:            b.Path,
			Matched:         b.Matched,
			Note:            b.Note,
		})
	}
	return out
}

// RoutingSimulateHandler drives the live routing engine against a hypothetical
// request and returns the full decision trace. Side-effect free: does not
// perform upstream HTTP, does not write traffic_event, does not emit MQ
// events, does not increment quota counters, does not mutate health tracker
// state.
func RoutingSimulateHandler(resolver routeResolver, bridge canonicalbridge.API, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req simulateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
			return
		}
		if req.ModelID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "modelId is required"})
			return
		}

		endpoint := normalizeEndpointType(req.EndpointType)
		ingressFormat := normalizeIngressBodyFormat(req.IngressBodyFormat)
		providerEP := simulateEndpointToProvider(endpoint)

		rctx := &routingcore.RoutingContext{
			RequestedModel: routingcore.RequestedModel{ID: req.ModelID},
			EndpointType:   typology.EndpointKind(endpoint),
			Request:        simulateMessagesToNormalizedPayload(req.Messages),
		}

		plan, err := resolver.Explain(r.Context(), rctx)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}

		resp := simulateResponse{
			Request: simulateRequestEcho{
				ModelID:           req.ModelID,
				EndpointType:      endpoint,
				IngressBodyFormat: string(ingressFormat),
			},
			OriginalModelID: plan.OriginalModelID,
			Substituted:     plan.Substituted,
			RuleID:          plan.RuleID,
			RuleName:        plan.RuleName,
			Stages:          plan.PipelineTrace,
			Trace:           plan.Trace,
			Targets:         projectTargets(plan.Targets, ingressFormat, providerEP, bridge),
			RecoveryTargets: projectTargets(plan.RecoveryTargets, ingressFormat, providerEP, bridge),
			Branches:        projectBranches(plan.Branches),
		}
		if resp.Stages == nil {
			resp.Stages = []routingcore.PipelineTraceEntry{}
		}
		if resp.Trace == nil {
			resp.Trace = []routingcore.TraceEntry{}
		}

		// Explain ran Resolve, which hydrated RequestedModel in place, so
		// CandidateIDs now answers the question these warnings turn on: did the
		// caller name a model the CATALOGUE knows, or a routing keyword?
		//
		// It used to be asked as `modelId == "auto"`. That was the same question
		// only while "auto" was the sole keyword a smart rule could match. Once a
		// deployment can trigger smart routing on its own words, a preview of
		// `fast` answered with the passthrough paragraph below — telling an
		// operator the live gateway would serve a string the catalogue has never
		// heard of, when it would 404. This is the screen they opened BECAUSE
		// routing is not doing what they expected, so a confident wrong answer
		// here is the most expensive kind.
		//
		// `auto` is unaffected: hydrateRequestedModel skips it deliberately, so
		// it has no candidates and lands on the same branch it always did.
		callerNamedACatalogModel := len(rctx.RequestedModel.CandidateIDs) > 0

		if plan.RuleID == "" && len(plan.Targets) == 0 {
			// No rule matched. For a caller who NAMED a model that is no
			// rejection: the live gateway serves it through the explicit-model
			// passthrough (the resolver's Explain does not model that handler
			// stage, so its empty target list here is NOT what the client would
			// see). Routing rules only REDIRECT a named model; they are not
			// required to serve one. A keyword has no such fallback — nothing
			// downstream can serve a string that names no model.
			switch {
			case rctx.RequestedModel.HydrationFailed:
				resp.Warnings = append(resp.Warnings, "no stage-1 rule matched, and the model catalogue could not be read for `"+req.ModelID+"` — this preview cannot tell a routing keyword from a named model, so treat the empty target list as unknown rather than as a rejection")
			case callerNamedACatalogModel:
				resp.Warnings = append(resp.Warnings, "no routing rule matched — `"+req.ModelID+"` does name a catalogue model, so if it is servable (its provider enabled and its status not disabled — which candidate resolution deliberately does not check) the live gateway serves it directly through the explicit-model passthrough. Routing rules only REDIRECT a named model; they are not required to serve one. This preview evaluates only the rule engine and does not model that passthrough, so an empty target list here is not what the client receives.")
			default:
				resp.Warnings = append(resp.Warnings, "no stage-1 rule matched — `"+req.ModelID+"` names no model in the catalogue, so it can only be served by a routing rule that claims it under matchConditions.requestedModelLiterals. As authored, this request has nothing to route and would be rejected.")
			}
		}
		resp.Warnings = append(resp.Warnings, "simulate runs without virtual-key context; project/organization/virtual-key matchConditions are evaluated as empty")
		// Gated on a rule having MATCHED, not merely on the choice being
		// delegated. "No candidates" is also true of a typo and of a pasted
		// model UUID, and for those this fired directly after the warning above
		// saying the request would be rejected — two readings of one request on
		// one screen. A rule that claimed the request is when "the rule may need
		// to read the prompt" is actually true, whatever keyword carried it.
		if plan.RuleID != "" && !callerNamedACatalogModel &&
			!rctx.RequestedModel.HydrationFailed && len(req.Messages) == 0 {
			resp.Warnings = append(resp.Warnings, "smart routing requested but messages is empty")
		}

		logger.Info("routing simulate",
			"modelId", req.ModelID,
			"endpointType", endpoint,
			"ruleId", plan.RuleID,
			"targets", len(resp.Targets),
		)

		writeJSON(w, http.StatusOK, resp)
	}
}

// schemaMode returns the per-target mode string used by the simulate
// endpoint: "passthrough" when the formats match, "translated" when
// the gateway uses the hub, "rejected" when no compatibility path exists.
func schemaMode(ingress, provider provcore.Format, ep typology.WireShape, bridge canonicalbridge.API) string {
	if bridge != nil {
		if !bridge.EndpointRoutable(ep, ingress, provider) {
			return "rejected"
		}
		if ingress == provider {
			return "passthrough"
		}
		return "translated"
	}
	switch ingress {
	case provider:
		return "passthrough"
	case provcore.FormatOpenAI:
		return "translated"
	default:
		return "rejected"
	}
}

// simulateEndpointToProvider maps a canonical typology.EndpointKind
// string to the matching OpenAI-family WireShape used for the schema
// mode / compatibility checks the simulator renders. Unknown kinds
// default to WireShapeOpenAIChat so the UI can still surface a plan.
func simulateEndpointToProvider(endpointType string) typology.WireShape {
	switch typology.EndpointKind(strings.TrimSpace(strings.ToLower(endpointType))) {
	case typology.EndpointKindChat:
		return typology.WireShapeOpenAIChat
	case typology.EndpointKindEmbeddings:
		return typology.WireShapeOpenAIEmbeddings
	case typology.EndpointKindModels:
		return typology.WireShapeNone
	default:
		return typology.WireShapeOpenAIChat
	}
}
