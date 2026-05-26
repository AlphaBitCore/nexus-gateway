package routing

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/ai/routing/routingstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	cfgpolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/policy"
	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

// RegisterRoutingRoutes registers routing rule CRUD routes.
func (h *Handler) RegisterRoutingRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	g.GET("/routing-rules", h.ListRoutingRules, iamMW(iam.ResourceRoutingRule.Action(iam.VerbRead)))
	g.POST("/routing-rules", h.CreateRoutingRule, iamMW(iam.ResourceRoutingRule.Action(iam.VerbCreate)))
	g.POST("/routing-rules/simulate", h.RoutingSimulate, iamMW(iam.ResourceRoutingRule.Action(iam.VerbSimulate)))
	g.GET("/routing-rules/:id", h.GetRoutingRule, iamMW(iam.ResourceRoutingRule.Action(iam.VerbRead)))
	g.PUT("/routing-rules/:id", h.UpdateRoutingRule, iamMW(iam.ResourceRoutingRule.Action(iam.VerbUpdate)))
	g.PATCH("/routing-rules/:id", h.UpdateRoutingRule, iamMW(iam.ResourceRoutingRule.Action(iam.VerbUpdate)))
	g.DELETE("/routing-rules/:id", h.DeleteRoutingRule, iamMW(iam.ResourceRoutingRule.Action(iam.VerbDelete)))
}

// validRetryOnClasses enumerates the acceptable RetryOn enum values per
// design spec §6.2. Kept in sync with configtypes.ErrorClass*.
var validRetryOnClasses = map[cfgpolicy.ErrorClass]struct{}{
	cfgpolicy.ErrorClassNetwork: {},
	cfgpolicy.ErrorClassTimeout: {},
	cfgpolicy.ErrorClassRate429: {},
	cfgpolicy.ErrorClass5xx:     {},
}

// validateRetryPolicyJSON enforces the admin-input bounds on a RetryPolicy
// before it is persisted. raw == nil or `null` is allowed (means "clear /
// inherit YAML default"). Returns ("", true) when valid; (msg, false)
// otherwise. Backoff fields are intentionally not validated — the UI does
// not expose them and the YAML default loader clamps shape errors there.
func validateRetryPolicyJSON(raw json.RawMessage) (string, bool) {
	s := string(raw)
	if len(raw) == 0 || s == "null" {
		return "", true
	}
	var p cfgpolicy.RetryPolicy
	if err := json.Unmarshal(raw, &p); err != nil {
		return fmt.Sprintf("retryPolicy is not valid JSON: %v", err), false
	}
	if p.MaxAttemptsPerTarget != 0 {
		if p.MaxAttemptsPerTarget < 1 || p.MaxAttemptsPerTarget > 5 {
			return fmt.Sprintf("retryPolicy.maxAttemptsPerTarget must be in [1,5]; got %d", p.MaxAttemptsPerTarget), false
		}
	}
	for _, cls := range p.RetryOn {
		if _, ok := validRetryOnClasses[cls]; !ok {
			return fmt.Sprintf("retryPolicy.retryOn[]: %q is not a valid error class (allowed: network, timeout, 429, 5xx)", cls), false
		}
	}
	return "", true
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

	gwURL := strings.TrimRight(h.proxy.AIGatewayURL, "/") + "/internal/routing-simulate"
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
	if msg, ok := validateMatchConditions(body.MatchConditions); !ok {
		return c.JSON(http.StatusUnprocessableEntity, errJSON(msg, "match_conditions_legacy_field", ""))
	}
	if msg, ok := validateSmartRuleMatchConditions(body.StrategyType, body.MatchConditions); !ok {
		return c.JSON(http.StatusBadRequest, errJSON(msg, "smart_rule_match_conditions_unsafe", ""))
	}
	if msg, ok := validateRetryPolicyJSON(body.RetryPolicy); !ok {
		return c.JSON(http.StatusBadRequest, errJSON(msg, "retry_policy_invalid", ""))
	}

	stage := 1
	if body.PipelineStage != nil && *body.PipelineStage == 0 {
		stage = 0
	}
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
		h.hub.InvalidateConfig(c.Request().Context(), "ai-gateway", "routing_rules")
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

	params := routingstore.UpdateRoutingRuleParams{
		Name:         body.Name,
		Description:  body.Description,
		StrategyType: body.StrategyType,
		Priority:     body.Priority,
		Enabled:      body.Enabled,
	}
	if body.Config != nil {
		raw, _ := json.Marshal(body.Config)
		params.Config = raw
	}
	if body.MatchConditions != nil {
		raw, _ := json.Marshal(body.MatchConditions)
		if msg, ok := validateMatchConditions(raw); !ok {
			return c.JSON(http.StatusUnprocessableEntity, errJSON(msg, "match_conditions_legacy_field", ""))
		}
		// Smart-rule guard: when the update supplies both strategyType=smart
		// and matchConditions, ensure matchConditions pins
		// requestedModelLiterals=["auto"]. The edge case where the operator
		// updates only matchConditions on a pre-existing smart rule is covered
		// by the audit runbook rather than blocked here.
		if body.StrategyType != nil {
			if msg, ok := validateSmartRuleMatchConditions(*body.StrategyType, raw); !ok {
				return c.JSON(http.StatusBadRequest, errJSON(msg, "smart_rule_match_conditions_unsafe", ""))
			}
		}
		params.MatchConditions = raw
	}
	if body.FallbackChain != nil {
		raw, _ := json.Marshal(body.FallbackChain)
		params.FallbackChain = raw
	}
	if body.PipelineStage != nil {
		stage := 1
		if *body.PipelineStage == 0 {
			stage = 0
		}
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
		h.hub.InvalidateConfig(c.Request().Context(), "ai-gateway", "routing_rules")
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
		h.hub.InvalidateConfig(c.Request().Context(), "ai-gateway", "routing_rules")
	}

	ae := audit.EntryFor(c, iam.ResourceRoutingRule, iam.VerbDelete)
	ae.EntityID = id
	ae.BeforeState = existing
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.NoContent(http.StatusNoContent)
}
