package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/governance/hooks/hookstore"
	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

// ProxyConfig holds the AI Gateway URL needed for hook test forwarding.
type ProxyConfig struct {
	AIGatewayURL string
}

// RegisterHookExtrasRoutes registers read-only hook metadata + test endpoints.
func (h *Handler) RegisterHookExtrasRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc, proxy ProxyConfig) {
	h.proxy = proxy
	g.GET("/hooks/implementations", h.HookImplementations, iamMW(iam.ResourceHook.Action(iam.VerbRead)))
	g.GET("/hooks/execution-chain", h.HookExecutionChain, iamMW(iam.ResourceHook.Action(iam.VerbRead)))
	g.POST("/hooks/:id/test", h.HookTest, iamMW(iam.ResourceHook.Action(iam.VerbRead)))
	g.POST("/hooks/:id/dry-run", h.HookTest, iamMW(iam.ResourceHook.Action(iam.VerbRead)))
}

// onMatchSchema is the JSON schema block shared by every content-touching
// hook implementation.
var onMatchSchema = map[string]any{
	"type":        "object",
	"description": "Unified policy shape. inflightAction controls what the upstream-bound copy of the body sees; storageAction controls the audit-log copy independently.",
	"properties": map[string]any{
		"inflightAction": map[string]any{"type": "string", "enum": []string{"approve", "block-hard", "block-soft", "redact"}},
		"storageAction":  map[string]any{"type": "string", "enum": []string{"keep", "redact", "drop-content"}},
		"replacement":    map[string]any{"type": "string"},
	},
}

// builtinHookImplementations is the static registry of built-in hook
// implementations advertised to the admin UI.
var builtinHookImplementations = []map[string]any{
	{
		"implementationId": "pii-detector",
		"hookType":         "builtin",
		"supportedStages":  []string{"request", "response"},
		"configSchema": map[string]any{
			"type":     "object",
			"required": []string{"patternDefinitions"},
			"properties": map[string]any{
				"patternDefinitions": map[string]any{
					"type":        "array",
					"description": "Named regex patterns. Each entry: id (label used in audit + replacement template), regex, flags (re2 inline flags such as `i`), optional luhn check, optional replacement override.",
					"items": map[string]any{
						"type":     "object",
						"required": []string{"id", "regex"},
						"properties": map[string]any{
							"id":          map[string]any{"type": "string"},
							"regex":       map[string]any{"type": "string"},
							"flags":       map[string]any{"type": "string", "default": ""},
							"luhn":        map[string]any{"type": "boolean", "default": false},
							"replacement": map[string]any{"type": "string"},
						},
					},
				},
				"onMatch": onMatchSchema,
			},
		},
	},
	{
		"implementationId": "keyword-filter",
		"hookType":         "builtin",
		"supportedStages":  []string{"request", "response"},
		"configSchema": map[string]any{
			"type":     "object",
			"required": []string{"patterns"},
			"properties": map[string]any{
				"patterns": map[string]any{
					"type":        "array",
					"description": "Keyword matchers. Each entry: pattern (literal or regex, case-folded per caseSensitive), optional category label used to tag matches in audit.",
					"items": map[string]any{
						"type":     "object",
						"required": []string{"pattern"},
						"properties": map[string]any{
							"pattern":  map[string]any{"type": "string"},
							"category": map[string]any{"type": "string"},
						},
					},
				},
				"caseSensitive": map[string]any{"type": "boolean", "default": false},
				"onMatch":       onMatchSchema,
			},
		},
	},
	{
		"implementationId": "content-safety",
		"hookType":         "builtin",
		"supportedStages":  []string{"request", "response"},
		"configSchema": map[string]any{
			"type":     "object",
			"required": []string{"categories"},
			"properties": map[string]any{
				"categories": map[string]any{
					"type":                 "object",
					"description":          "Per-category enable flags. Keys are category names (sexual, violence, hate_speech, illegal, self_harm, …); values are booleans that toggle the detector.",
					"additionalProperties": map[string]any{"type": "boolean"},
				},
				"onMatch": onMatchSchema,
			},
		},
	},
	{
		"implementationId": "rate-limiter",
		"hookType":         "builtin",
		"supportedStages":  []string{"request"},
		"configSchema": map[string]any{
			"type":     "object",
			"required": []string{"maxRequests", "windowSeconds"},
			"properties": map[string]any{
				"maxRequests":   map[string]any{"type": "integer", "minimum": 1},
				"windowSeconds": map[string]any{"type": "integer", "minimum": 1},
				"keyType":       map[string]any{"type": "string", "enum": []string{"source_ip", "target_host"}, "default": "source_ip"},
			},
		},
	},
	{
		"implementationId": "request-size-validator",
		"hookType":         "builtin",
		"supportedStages":  []string{"request"},
		"configSchema": map[string]any{
			"type":     "object",
			"required": []string{"maxSizeBytes"},
			"properties": map[string]any{
				"maxSizeBytes":        map[string]any{"type": "integer", "minimum": 1},
				"excludeContentTypes": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			},
		},
	},
	{
		"implementationId": "ip-access-filter",
		"hookType":         "builtin",
		"supportedStages":  []string{"request"},
		"configSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"allowlist": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"blocklist": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"mode":      map[string]any{"type": "string", "enum": []string{"allowlist", "blocklist", "both"}, "default": "blocklist"},
			},
		},
	},
	{
		"implementationId": "data-residency",
		"hookType":         "builtin",
		"supportedStages":  []string{"request", "response"},
		"configSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"policies": map[string]any{
					"type":        "array",
					"description": "Per-classification residency rules. Each entry: classification (CONFIDENTIAL / RESTRICTED / …), allowedRegions (provider regions where this data may be served).",
					"items": map[string]any{
						"type":     "object",
						"required": []string{"classification", "allowedRegions"},
						"properties": map[string]any{
							"classification": map[string]any{"type": "string"},
							"allowedRegions": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						},
					},
				},
			},
		},
	},
	{
		"implementationId": "rulepack-engine",
		"hookType":         "builtin",
		"supportedStages":  []string{"request", "response"},
		"configSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"onMatch": onMatchSchema,
			},
		},
	},
	{
		"implementationId": "quality-checker",
		"hookType":         "builtin",
		"supportedStages":  []string{"response"},
		"configSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"minResponseLength":     map[string]any{"type": "integer", "default": 10, "minimum": 0},
				"expectedFinishReasons": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "default": []string{"stop", "end_turn"}},
				"detectRefusals":        map[string]any{"type": "boolean", "default": true},
				"refusalPatterns":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Override the built-in refusal regex list."},
				"onMatch":               onMatchSchema,
			},
		},
	},
	{
		"implementationId": "webhook-forward",
		"hookType":         "webhook",
		"supportedStages":  []string{"request", "response"},
		"configSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"timeoutMs":   map[string]any{"type": "integer", "minimum": 1, "default": 5000},
				"payloadMode": map[string]any{"type": "string", "enum": []string{"full", "redacted", "metadata-only"}, "default": "redacted"},
				"onMatch":     onMatchSchema,
			},
		},
	},
	{
		"implementationId": "noop",
		"hookType":         "builtin",
		"supportedStages":  []string{"request", "response"},
		"configSchema": map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	},
}

// hookCategoryDefinitions advertises the category codes the UI recognises.
// Keep in lock-step with packages/control-plane-ui/src/constants/hooks.ts
// HOOK_CATEGORY enum.
var hookCategoryDefinitions = []map[string]any{
	{"code": "compliance", "name": "Compliance & content safety"},
	{"code": "traffic_control", "name": "Traffic & limits"},
	{"code": "quality", "name": "Quality & signals"},
	{"code": "observability", "name": "Observability"},
	{"code": "custom", "name": "Custom / other"},
}

// HookImplementations returns the built-in hook implementation registry and
// category definitions for the admin UI.
func (h *Handler) HookImplementations(c echo.Context) error {
	return c.JSON(http.StatusOK, map[string]any{
		"data":           builtinHookImplementations,
		"hookCategories": hookCategoryDefinitions,
	})
}

// HookExecutionChain returns a structured view of the full request + response
// hook execution order for the pipeline visualiser UI.
func (h *Handler) HookExecutionChain(c echo.Context) error {
	allHooks, _, _ := h.hooks.ListHookConfigs(c.Request().Context(), hookstore.HookConfigListParams{Limit: 1000})

	type step struct {
		Order          int            `json:"order"`
		HookConfigID   string         `json:"hookConfigId"`
		Name           string         `json:"name"`
		Priority       int            `json:"priority"`
		Enabled        bool           `json:"enabled"`
		Type           string         `json:"type"`
		Wired          bool           `json:"wired"`
		ExecutionMode  string         `json:"executionMode"`
		Classification map[string]any `json:"classification"`
	}
	type flowNode struct {
		Kind  string `json:"kind"`
		ID    string `json:"id"`
		Label string `json:"label"`
		Phase string `json:"phase,omitempty"`
		Steps []step `json:"steps,omitempty"`
	}

	var requestHooks, responseHooks []step
	for i, hc := range allHooks {
		s := step{
			Order:         i,
			HookConfigID:  hc.ID,
			Name:          hc.Name,
			Priority:      hc.Priority,
			Enabled:       hc.Enabled,
			Type:          hc.Type,
			Wired:         true,
			ExecutionMode: hc.FailBehavior,
			Classification: map[string]any{
				"category":            derefStr(hc.Category),
				"implementationLabel": hc.ImplementationID,
				"dualPhaseCapable":    false,
			},
		}
		switch hc.Stage {
		case "request":
			requestHooks = append(requestHooks, s)
		case "response":
			responseHooks = append(responseHooks, s)
		default:
			requestHooks = append(requestHooks, s)
		}
	}

	flow := []flowNode{
		{Kind: "milestone", ID: "request-start", Label: "request-start"},
		{Kind: "hook_segment", ID: "request-hooks", Label: "request-hooks", Phase: "request", Steps: requestHooks},
		{Kind: "milestone", ID: "upstream", Label: "upstream"},
		{Kind: "hook_segment", ID: "response-hooks", Label: "response-hooks", Phase: "response", Steps: responseHooks},
		{Kind: "milestone", ID: "response-end", Label: "response-end"},
	}

	enabledCount := 0
	for _, hc := range allHooks {
		if hc.Enabled {
			enabledCount++
		}
	}

	return c.JSON(http.StatusOK, map[string]any{
		"totalHooks":    len(allHooks),
		"enabledHooks":  enabledCount,
		"requestHooks":  requestHooks,
		"responseHooks": responseHooks,
		"flow":          flow,
	})
}

// derefStr safely dereferences a *string, returning "" if nil.
func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// HookTest runs a dry-run of the stored hook config against a sample body,
// delegating to the AI Gateway for builtin hooks.
func (h *Handler) HookTest(c echo.Context) error {
	id := c.Param("id")
	hc, err := h.hooks.GetHookConfig(c.Request().Context(), id)
	if err != nil || hc == nil {
		return c.JSON(http.StatusNotFound, errJSON("Hook config not found", "not_found", ""))
	}

	if hc.Type == "webhook" && hc.Endpoint != nil && *hc.Endpoint != "" {
		output, elapsed, execErr := runWebhookHookTest(c.Request().Context(), hc)
		if execErr != nil {
			return c.JSON(http.StatusOK, map[string]any{
				"error": execErr.Error(), "executionTimeMs": elapsed, "stage": hc.Stage,
			})
		}
		return c.JSON(http.StatusOK, map[string]any{
			"output": output, "executionTimeMs": elapsed, "stage": hc.Stage,
		})
	}

	return h.forwardHookTest(c, hc)
}

// forwardHookTest proxies an admin hook test to the AI gateway's internal
// endpoint.
func (h *Handler) forwardHookTest(c echo.Context, hc *hookstore.HookConfig) error {
	gwURL := trimRight(h.proxy.AIGatewayURL, "/") + "/internal/hooks-test"

	rawBody, _ := io.ReadAll(io.LimitReader(c.Request().Body, 256*1024))

	payload, err := json.Marshal(map[string]any{
		"hookConfig": hc,
		"rawBody":    string(rawBody),
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to encode payload", "server_error", ""))
	}

	clientTimeout := time.Duration(hc.TimeoutMs)*time.Millisecond + 2*time.Second
	if clientTimeout < 5*time.Second {
		clientTimeout = 5 * time.Second
	}
	client := nexushttp.New(nexushttp.Config{
		Timeout:        clientTimeout,
		Caller:         "cp-hooks-hook-test",
		PropagateReqID: true,
	})

	req, err := http.NewRequestWithContext(c.Request().Context(), http.MethodPost, gwURL, bytes.NewReader(payload))
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

// runWebhookHookTest POSTs a fixed sample body to the hook's configured
// endpoint and returns the parsed response body, elapsed time, and any
// transport error.
func runWebhookHookTest(ctx context.Context, hc *hookstore.HookConfig) (any, int64, error) {
	client := nexushttp.New(nexushttp.Config{
		Timeout:        time.Duration(hc.TimeoutMs) * time.Millisecond,
		Caller:         "cp-hooks-webhook-test",
		PropagateReqID: true,
	})
	sampleBody := []byte(`{
		"stage":"request",
		"method":"POST",
		"path":"/v1/chat/completions",
		"targetHost":"example.com",
		"sourceIP":"127.0.0.1",
		"bodySize":32,
		"contentType":"application/json",
		"model":"test",
		"ingressType":"AI_GATEWAY",
		"normalizedContent":["test"]
	}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, *hc.Endpoint, bytes.NewReader(sampleBody))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := client.Do(req)
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		return nil, elapsed, err
	}
	defer resp.Body.Close() //nolint:errcheck
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	var output any
	if json.Unmarshal(respBody, &output) != nil {
		output = string(respBody)
	}
	return output, elapsed, nil
}

// trimRight removes trailing occurrences of suffix from s.
func trimRight(s, suffix string) string {
	for len(s) > 0 && s[len(s)-len(suffix):] == suffix {
		s = s[:len(s)-len(suffix)]
	}
	return s
}
