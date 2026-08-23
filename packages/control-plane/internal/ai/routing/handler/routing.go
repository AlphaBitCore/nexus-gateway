package routing

import (
	"bytes"
	"fmt"
	"github.com/goccy/go-json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/ai/routing/routingstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/hub"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/peer"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
)

// RegisterRoutingRoutes registers routing rule CRUD routes.
// validatePipelineStage accepts only stage 1, the route stage.
//
// Stage 0 was a policy-narrowing pipeline of our own invention — allow/deny
// lists evaluated before routing. It is gone: no UI control ever offered it, no
// production rule used it, and the gateway no longer evaluates it. The product's
// model-access rule is the virtual key's ALLOW list, enforced on both selection
// paths, and there is no deny concept.
//
// Rejected rather than coerced to 1. Silently rewriting the caller's stage would
// store a rule that is not the one they asked for, and they would learn that
// from behaviour instead of from the response. The admin UI never offered stage
// 0, and the UI defines the supported surface — the API must not accept more
// than the UI can express, or an operator ends up with configuration nobody can
// see or edit.
func validatePipelineStage(stage *int) (string, bool) {
	if stage == nil || *stage == 1 {
		return "", true
	}
	return "pipelineStage must be 1 (route). Stage 0 (policy narrowing) has been removed; " +
		"model access is governed by the virtual key's allowed-models list.", false
}

func (h *Handler) RegisterRoutingRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	g.GET("/routing-rules", h.ListRoutingRules, iamMW(iam.ResourceRoutingRule.Action(iam.VerbRead)))
	g.POST("/routing-rules", h.CreateRoutingRule, iamMW(iam.ResourceRoutingRule.Action(iam.VerbCreate)))
	g.POST("/routing-rules/simulate", h.RoutingSimulate, iamMW(iam.ResourceRoutingRule.Action(iam.VerbSimulate)))
	g.GET("/routing-rules/:id", h.GetRoutingRule, iamMW(iam.ResourceRoutingRule.Action(iam.VerbRead)))
	g.PUT("/routing-rules/:id", h.UpdateRoutingRule, iamMW(iam.ResourceRoutingRule.Action(iam.VerbUpdate)))
	g.PATCH("/routing-rules/:id", h.UpdateRoutingRule, iamMW(iam.ResourceRoutingRule.Action(iam.VerbUpdate)))
	g.DELETE("/routing-rules/:id", h.DeleteRoutingRule, iamMW(iam.ResourceRoutingRule.Action(iam.VerbDelete)))
}

// extractJSONFieldForUpdate inspects the raw request body to distinguish three
// caller intents on PUT/PATCH for a nullable JSONB column:
//
//	field absent       → present=false (do not change column)
//	field == null      → present=true, raw is nil (clear column to SQL NULL)
//	field == {…}/[…]  → present=true, raw holds the JSON to persist
//
// errMsg is non-empty only when the outer body itself fails to parse as a JSON
// object — callers treat that as "no change" (best-effort presence detection).
func extractJSONFieldForUpdate(body []byte, field string) (raw json.RawMessage, present bool) {
	if len(body) == 0 {
		return nil, false
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, false
	}
	v, ok := m[field]
	if !ok {
		return nil, false
	}
	if len(v) == 0 || string(v) == "null" {
		return nil, true
	}
	return v, true
}

// extractRetryPolicyForUpdate is a wrapper around extractJSONFieldForUpdate
// kept for backward compatibility. Returns (rawJSON, present, errMsg).
func extractRetryPolicyForUpdate(body []byte) (raw json.RawMessage, present bool, errMsg string) {
	r, p := extractJSONFieldForUpdate(body, "retryPolicy")
	return r, p, ""
}

// validStrategyTypes is the closed set of routing-rule strategy types the
// AI Gateway resolver can dispatch. It is kept in lockstep with the
// StrategyNode discriminated union (packages/ai-gateway/internal/routing/
// core/types.go) and the strategies registered by RegisterAllStrategies
// (packages/ai-gateway/internal/routing/strategies/strategy.go). A rule
// persisted with any other strategyType would broadcast fleet-wide via
// InvalidateConfig and then fail to resolve on every gateway, so the admin
// write path rejects unknown values up front rather than letting them land.
//
// `policy` was in this set and has no implementation. A rule carrying it was
// accepted, broadcast, and then yielded the primary slot on every gateway — the
// admin saw a rule listed and enabled that never fired. Stored rows are
// disabled by the upgrade migration; new ones are refused here.
var validStrategyTypes = map[string]struct{}{
	"single":      {},
	"fallback":    {},
	"loadbalance": {},
	"conditional": {},
	"ab_split":    {},
	"smart":       {},
	"latency":     {},
}

// strategyTypeList renders validStrategyTypes as a stable, comma-separated
// string for the 400 error body so an operator sees the accepted set.
func strategyTypeList() string {
	ordered := []string{"single", "fallback", "loadbalance", "conditional", "ab_split", "smart", "latency"}
	return strings.Join(ordered, ", ")
}

// validateStrategyType rejects any strategyType outside the known set.
// Returns ("", true) when valid; (operator-facing message, false) otherwise.
func validateStrategyType(strategyType string) (string, bool) {
	if _, ok := validStrategyTypes[strategyType]; !ok {
		return fmt.Sprintf("strategyType %q is not a recognized routing strategy (allowed: %s)", strategyType, strategyTypeList()), false
	}
	return "", true
}

// validateMatchConditions rejects the legacy field name "organizations" in
// favor of "projects". Returns a non-nil echo error response
// when the payload uses the legacy key.
func validateMatchConditions(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", true
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", true // bind-time validation handles shape errors
	}
	if _, legacy := m["organizations"]; legacy {
		return "matchConditions.organizations has been renamed to matchConditions.projects", false
	}
	return "", true
}

// smartGuardEmptyMsg is the operator-facing error when a smart-strategy
// rule's matchConditions does not pin requestedModelLiterals = ["auto"].
// The runbook reference is part of the message so an operator hitting
// this in CI has a single link to follow.
const smartGuardEmptyMsg = `matchConditions must include "requestedModelLiterals": ["auto"] for strategyType=smart — empty or unrestricted matchConditions can route non-auto traffic into smart routing and produce non-grounded decisions; see docs/operators/ops/runbooks/r-routing-rule-matchconditions-audit.md`

// validateSmartRuleMatchConditions enforces the operator-side guard: a
// smart-strategy RoutingRule must pin matchConditions to match only the
// "auto" sentinel. Empty or non-"auto" matchConditions are rejected to
// prevent a broadly-matched smart rule from firing on every request.
//
// No-op for strategies other than "smart". Returns ("", true) on success;
// returns (operator-facing message, false) on rejection.
func validateSmartRuleMatchConditions(strategyType string, raw json.RawMessage) (string, bool) {
	if strategyType != "smart" {
		return "", true
	}
	if len(raw) == 0 || string(raw) == "null" {
		return smartGuardEmptyMsg, false
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", true // bind-time validation handles shape errors
	}
	if len(m) == 0 {
		return smartGuardEmptyMsg, false
	}
	litRaw, ok := m["requestedModelLiterals"]
	if !ok {
		return smartGuardEmptyMsg, false
	}
	var literals []string
	if err := json.Unmarshal(litRaw, &literals); err != nil {
		return "", true
	}
	if len(literals) == 0 {
		return smartGuardEmptyMsg, false
	}
	for _, lit := range literals {
		if lit != "auto" {
			return fmt.Sprintf(`matchConditions.requestedModelLiterals[*]=%q is not safe for strategyType=smart; smart rules must match "auto" only — see docs/operators/ops/runbooks/r-routing-rule-matchconditions-audit.md`, lit), false
		}
	}
	return "", true
}

// RoutingSimulate forwards a simulate request to the AI Gateway internal
// endpoint and streams the response back verbatim. Mirrors forwardProviderTest.
func (h *Handler) RoutingSimulate(c echo.Context) error {
	var body map[string]any
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}

	gwBase, gwErr := h.proxy.AIGatewayBase(c.Request().Context())
	if gwErr != nil {
		return peer.ServiceUnavailable(c, "ai-gateway", gwErr)
	}
	gwURL := strings.TrimRight(gwBase, "/") + "/internal/routing-simulate"
	payload, err := json.Marshal(body)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to encode payload", "server_error", ""))
	}

	client := nexushttp.New(nexushttp.Config{
		Timeout:        15 * time.Second,
		Caller:         "cp-admin-routing",
		PropagateReqID: true,
	})
	req, err := http.NewRequestWithContext(c.Request().Context(), http.MethodPost, gwURL, strings.NewReader(string(payload)))
	if err != nil {
		return c.JSON(http.StatusBadGateway, map[string]any{"error": "Failed to build request: " + err.Error()})
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.proxy.AIGatewayInternalToken)

	resp, err := client.Do(req)
	if err != nil {
		return c.JSON(http.StatusBadGateway, map[string]any{"error": "AI Gateway unreachable: " + err.Error()})
	}
	defer resp.Body.Close() //nolint:errcheck

	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	c.Response().Header().Set("Content-Type", "application/json")
	c.Response().WriteHeader(resp.StatusCode)
	_, _ = c.Response().Write(bodyBytes)
	return nil
}

func (h *Handler) ListRoutingRules(c echo.Context) error {
	pg := parsePagination(c)
	params := routingstore.RoutingRuleListParams{
		Q:            c.QueryParam("q"),
		StrategyType: c.QueryParam("strategyType"),
		Limit:        pg.Limit,
		Offset:       pg.Offset,
	}
	if v := c.QueryParam("enabled"); v == "true" {
		t := true
		params.Enabled = &t
	} else if v == "false" {
		f := false
		params.Enabled = &f
	}

	rules, total, err := h.routing.ListRoutingRules(c.Request().Context(), params)
	if err != nil {
		h.logger.Error("list routing rules", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to list routing rules", "server_error", ""))
	}
	return c.JSON(http.StatusOK, map[string]any{"data": rules, "total": total})
}

func (h *Handler) GetRoutingRule(c echo.Context) error {
	r, err := h.routing.GetRoutingRule(c.Request().Context(), c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to get routing rule", "server_error", ""))
	}
	if r == nil {
		return c.JSON(http.StatusNotFound, errJSON("Routing rule not found", "not_found", ""))
	}
	return c.JSON(http.StatusOK, r)
}

func (h *Handler) CreateRoutingRule(c echo.Context) error {
	var body struct {
		Name            string          `json:"name"`
		Description     *string         `json:"description"`
		StrategyType    string          `json:"strategyType"`
		Config          json.RawMessage `json:"config"`
		MatchConditions json.RawMessage `json:"matchConditions"`
		Priority        int             `json:"priority"`
		PipelineStage   *int            `json:"pipelineStage"`
		FallbackChain   json.RawMessage `json:"fallbackChain"`
		RetryPolicy     json.RawMessage `json:"retryPolicy"`
		Enabled         *bool           `json:"enabled"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}
	if body.Name == "" || body.StrategyType == "" || body.Config == nil {
		return c.JSON(http.StatusBadRequest, errJSON("Missing required fields: name, strategyType, config", "validation_error", ""))
	}
	if msg, ok := validateStrategyType(body.StrategyType); !ok {
		return c.JSON(http.StatusBadRequest, errJSON(msg, "strategy_type_invalid", ""))
	}
	if msg, ok := validateStrategyConfig(body.Config); !ok {
		return c.JSON(http.StatusBadRequest, errJSON(msg, "strategy_config_invalid", ""))
	}
	if msg, ok := validateMatchConditions(body.MatchConditions); !ok {
		return c.JSON(http.StatusUnprocessableEntity, errJSON(msg, "match_conditions_legacy_field", ""))
	}
	if msg, ok := validateSmartRuleMatchConditions(body.StrategyType, body.MatchConditions); !ok {
		return c.JSON(http.StatusBadRequest, errJSON(msg, "smart_rule_match_conditions_unsafe", ""))
	}
	if msg, ok := validateRetryPolicyJSON(body.RetryPolicy); !ok {
		return c.JSON(http.StatusBadRequest, errJSON(msg, "retry_policy_invalid", ""))
	}
	if msg, ok := validateFallbackChain(body.FallbackChain); !ok {
		return c.JSON(http.StatusBadRequest, errJSON(msg, "fallback_chain_invalid", ""))
	}

	// Stage 0 was a policy-narrowing pipeline of our own invention: allow/deny
	// lists evaluated before routing. It is gone. The product's model-access
	// rule is the virtual key's ALLOW list, enforced in the gateway on both
	// selection paths, and there is no deny concept.
	//
	// Rejected rather than coerced to 1. Silently rewriting the caller's stage
	// would store a rule that is not the one they asked for, and they would find
	// out from behaviour instead of from the response. The admin UI never
	// offered stage 0 — it defines the supported surface, so the API must not
	// accept more than it can express.
	if msg, ok := validatePipelineStage(body.PipelineStage); !ok {
		return c.JSON(http.StatusBadRequest, errJSON(msg, "pipeline_stage_invalid", ""))
	}
	stage := 1
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}

	r, err := h.routing.CreateRoutingRule(c.Request().Context(), routingstore.CreateRoutingRuleParams{
		Name:            body.Name,
		Description:     body.Description,
		StrategyType:    body.StrategyType,
		Config:          body.Config,
		MatchConditions: body.MatchConditions,
		Priority:        body.Priority,
		PipelineStage:   stage,
		FallbackChain:   body.FallbackChain,
		RetryPolicy:     body.RetryPolicy,
		Enabled:         enabled,
	})
	if err != nil {
		h.logger.Error("create routing rule", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to create routing rule", "server_error", ""))
	}

	if h.hub != nil {
		if err := h.hub.InvalidateConfigE(c.Request().Context(), "ai-gateway", "routing_rules"); err != nil {
			h.logger.Error("routing rule: hub invalidate failed", "id", c.Param("id"), "error", err)
			return hub.RespondPropagationFailure(c, err)
		}
	}

	ae := audit.EntryFor(c, iam.ResourceRoutingRule, iam.VerbCreate)
	ae.EntityID = r.ID
	ae.AfterState = r
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusCreated, r)
}

func (h *Handler) UpdateRoutingRule(c echo.Context) error {
	id := c.Param("id")
	existing, err := h.routing.GetRoutingRule(c.Request().Context(), id)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to get routing rule", "server_error", ""))
	}
	if existing == nil {
		return c.JSON(http.StatusNotFound, errJSON("Routing rule not found", "not_found", ""))
	}

	// Buffer the body so we can both Bind (struct decode) and inspect raw
	// keys (presence detection for retryPolicy: distinguishes "field
	// absent" from "field == null").
	rawBody, err := io.ReadAll(c.Request().Body)
	if err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Failed to read request body", "validation_error", ""))
	}
	c.Request().Body = io.NopCloser(bytes.NewReader(rawBody))

	var body struct {
		Name            *string `json:"name"`
		Description     *string `json:"description"`
		StrategyType    *string `json:"strategyType"`
		Config          any     `json:"config"`
		MatchConditions any     `json:"matchConditions"`
		Priority        *int    `json:"priority"`
		PipelineStage   *int    `json:"pipelineStage"`
		FallbackChain   any     `json:"fallbackChain"`
		Enabled         *bool   `json:"enabled"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}

	// The STORED type is validated too, not only a supplied one.
	//
	// A partial patch that omits strategyType used to skip the check entirely,
	// so the one-click enable toggle on the rules list re-enabled a rule the
	// upgrade migration had just disabled — a green "Rule updated" and a rule
	// shown Enabled that the resolver yields on every request forever. The
	// migration's whole purpose was undone by a control that never mentions
	// strategies.
	//
	// The patch may still FIX such a rule: supplying a valid strategyType
	// replaces the stored one, and that is exactly the way out.
	effectiveType := existing.StrategyType
	if body.StrategyType != nil {
		effectiveType = *body.StrategyType
	}
	if msg, ok := validateStrategyType(effectiveType); !ok {
		if body.StrategyType == nil {
			msg += ". This rule is stored with a strategy the gateway cannot dispatch; " +
				"change strategyType in the same request, or delete the rule"
		}
		return c.JSON(http.StatusBadRequest, errJSON(msg, "strategy_type_invalid", ""))
	}

	params := routingstore.UpdateRoutingRuleParams{
		Name:         body.Name,
		Description:  body.Description,
		StrategyType: body.StrategyType,
		Priority:     body.Priority,
		Enabled:      body.Enabled,
	}
	if body.Config != nil {
		raw, _ := json.Marshal(body.Config)
		if msg, ok := validateStrategyConfig(raw); !ok {
			return c.JSON(http.StatusBadRequest, errJSON(msg, "strategy_config_invalid", ""))
		}
		params.Config = raw
	}
	if body.MatchConditions != nil {
		raw, _ := json.Marshal(body.MatchConditions)
		if msg, ok := validateMatchConditions(raw); !ok {
			return c.JSON(http.StatusUnprocessableEntity, errJSON(msg, "match_conditions_legacy_field", ""))
		}
		// Smart-rule guard, against the rule's EFFECTIVE strategy rather than
		// the body's. An update that supplies matchConditions and no
		// strategyType is still an update to a smart rule when the stored one
		// is smart — and gating on the body let exactly that request save a
		// rule into a state it could not have been created in, with the
		// create path refusing what the edit path accepted.
		if msg, ok := validateSmartRuleMatchConditions(effectiveType, raw); !ok {
			return c.JSON(http.StatusBadRequest, errJSON(msg, "smart_rule_match_conditions_unsafe", ""))
		}
		params.MatchConditions = raw
	}
	if body.FallbackChain != nil {
		raw, _ := json.Marshal(body.FallbackChain)
		if msg, ok := validateFallbackChain(raw); !ok {
			return c.JSON(http.StatusBadRequest, errJSON(msg, "fallback_chain_invalid", ""))
		}
		params.FallbackChain = raw
	}
	if body.PipelineStage != nil {
		if msg, ok := validatePipelineStage(body.PipelineStage); !ok {
			return c.JSON(http.StatusBadRequest, errJSON(msg, "pipeline_stage_invalid", ""))
		}
		stage := 1
		params.PipelineStage = &stage
	}

	// retryPolicy: explicit presence detection so "absent" means "leave
	// column unchanged" and "null" means "clear override / inherit YAML".
	rpRaw, rpPresent, _ := extractRetryPolicyForUpdate(rawBody)
	if rpPresent {
		if msg, ok := validateRetryPolicyJSON(rpRaw); !ok {
			return c.JSON(http.StatusBadRequest, errJSON(msg, "retry_policy_invalid", ""))
		}
		// non-nil pointer signals "change". Empty raw inside means "set NULL".
		params.RetryPolicy = &rpRaw
	}

	updated, err := h.routing.UpdateRoutingRule(c.Request().Context(), id, params)
	if err != nil {
		h.logger.Error("update routing rule", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to update routing rule", "server_error", ""))
	}

	if h.hub != nil {
		if err := h.hub.InvalidateConfigE(c.Request().Context(), "ai-gateway", "routing_rules"); err != nil {
			h.logger.Error("routing rule: hub invalidate failed", "id", c.Param("id"), "error", err)
			return hub.RespondPropagationFailure(c, err)
		}
	}

	ae := audit.EntryFor(c, iam.ResourceRoutingRule, iam.VerbUpdate)
	ae.EntityID = id
	ae.BeforeState = existing
	ae.AfterState = updated
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusOK, updated)
}

func (h *Handler) DeleteRoutingRule(c echo.Context) error {
	id := c.Param("id")
	existing, err := h.routing.GetRoutingRule(c.Request().Context(), id)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to get routing rule", "server_error", ""))
	}
	if existing == nil {
		return c.JSON(http.StatusNotFound, errJSON("Routing rule not found", "not_found", ""))
	}

	if err := h.routing.DeleteRoutingRule(c.Request().Context(), id); err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to delete routing rule", "server_error", ""))
	}

	if h.hub != nil {
		if err := h.hub.InvalidateConfigE(c.Request().Context(), "ai-gateway", "routing_rules"); err != nil {
			h.logger.Error("routing rule: hub invalidate failed", "id", c.Param("id"), "error", err)
			return hub.RespondPropagationFailure(c, err)
		}
	}

	ae := audit.EntryFor(c, iam.ResourceRoutingRule, iam.VerbDelete)
	ae.EntityID = id
	ae.BeforeState = existing
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.NoContent(http.StatusNoContent)
}
