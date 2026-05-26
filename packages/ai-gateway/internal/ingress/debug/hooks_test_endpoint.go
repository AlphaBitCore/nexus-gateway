package debug

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/rulepack"
)

// storedHookConfig mirrors the JSON shape of
// packages/control-plane/internal/store.HookConfig. It is redeclared here
// rather than imported so the AI gateway does not depend on the
// control-plane package. JSON tags match exactly.
type storedHookConfig struct {
	ID                string          `json:"id"`
	Name              string          `json:"name"`
	Type              string          `json:"type"`
	ImplementationID  string          `json:"implementationId"`
	Stage             string          `json:"stage"`
	Config            json.RawMessage `json:"config"`
	Priority          int             `json:"priority"`
	TimeoutMs         int             `json:"timeoutMs"`
	FailBehavior      string          `json:"failBehavior"`
	Enabled           bool            `json:"enabled"`
	ApplicableIngress []string        `json:"applicableIngress"`
}

// hooksTestRequest is the JSON body for POST /internal/hooks-test. The
// control-plane fetches the stored hook config, packages it with the
// caller-supplied sample body, and posts it here.
type hooksTestRequest struct {
	HookConfig storedHookConfig `json:"hookConfig"`
	RawBody    string           `json:"rawBody"`
}

// HooksTestHandler runs a configured hook against sample input without
// touching live traffic: no traffic_event, no MQ emission, no audit row.
// Factory resolution uses the AI gateway's registry (shared factories plus
// AI-gateway-local ones such as quality-checker and webhook-forward), so
// any implementationId the data plane can run is also testable here.
//
// When rulePackLister is non-nil (production: the DB-backed rulepack.Store),
// hook configs for rule-pack consumer implementations are enriched the same
// way as the live hook pipeline so admin tests exercise installed packs and
// overrides — not only the inline JSON stored on HookConfig.
func HooksTestHandler(registry *hookcore.HookRegistry, rulePackLister rulepack.InstallLister, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req hooksTestRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
			return
		}
		if req.HookConfig.ImplementationID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "hookConfig.implementationId is required"})
			return
		}

		factory := registry.Get(req.HookConfig.ImplementationID)
		if factory == nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "unknown builtin implementationId \"" + req.HookConfig.ImplementationID + "\"",
				"code":  "unknown_implementation",
			})
			return
		}

		var cfgMap map[string]any
		if len(req.HookConfig.Config) > 0 {
			if err := json.Unmarshal(req.HookConfig.Config, &cfgMap); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{
					"error": "hook config JSON invalid: " + err.Error(),
					"code":  "invalid_config",
				})
				return
			}
		}

		runtimeCfg := &hookcore.HookConfig{
			ID:                req.HookConfig.ID,
			ImplementationID:  req.HookConfig.ImplementationID,
			Name:              req.HookConfig.Name,
			Priority:          req.HookConfig.Priority,
			Enabled:           req.HookConfig.Enabled,
			Stage:             req.HookConfig.Stage,
			FailBehavior:      req.HookConfig.FailBehavior,
			TimeoutMs:         req.HookConfig.TimeoutMs,
			ApplicableIngress: req.HookConfig.ApplicableIngress,
			Config:            cfgMap,
		}

		if rulePackLister != nil {
			hookCfgs := []hookcore.HookConfig{*runtimeCfg}
			if _, err := rulepack.Enrich(r.Context(), rulePackLister, hookCfgs); err != nil {
				writeJSON(w, http.StatusBadGateway, map[string]any{
					"error": "rule pack resolution failed: " + err.Error(),
					"code":  "rulepack_enrich",
				})
				return
			}
			runtimeCfg = &hookCfgs[0]
		}

		hook, err := factory(runtimeCfg)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "hook factory failed: " + err.Error(),
				"code":  "factory_error",
			})
			return
		}

		input := buildHookTestInput(req.HookConfig.Stage, strings.NewReader(req.RawBody))
		if req.HookConfig.ImplementationID == "ip-access-filter" && strings.TrimSpace(input.SourceIP) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "input.sourceIp is required for ip-access-filter test",
				"code":  "invalid_test_input",
			})
			return
		}
		result, elapsed, execErr := runHook(r.Context(), hook, input, req.HookConfig.TimeoutMs)
		if execErr != nil {
			if logger != nil {
				logger.Debug("admin hook test: hook Execute returned error",
					"implementationId", req.HookConfig.ImplementationID,
					"hookId", req.HookConfig.ID,
					"error", execErr.Error())
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"error":           execErr.Error(),
				"executionTimeMs": elapsed,
				"stage":           req.HookConfig.Stage,
			})
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"output":          result,
			"executionTimeMs": elapsed,
			"stage":           req.HookConfig.Stage,
		})
	}
}

// buildHookTestInput parses an optional test request body into a HookInput.
// Body shape: { input?: { prompt?, messages? } }. Messages take precedence
// over prompt when both are present. An empty or unparseable body yields a
// HookInput with empty content.
func buildHookTestInput(stage string, body io.Reader) *hookcore.HookInput {
	input := &hookcore.HookInput{
		RequestID:   uuid.NewString(),
		Stage:       stage,
		IngressType: "AI_GATEWAY",
		Method:      http.MethodPost,
		Path:        "/v1/chat/completions",
	}

	if body == nil {
		return input
	}

	var testBody struct {
		Input *struct {
			Prompt   string           `json:"prompt"`
			Messages []map[string]any `json:"messages"`
			SourceIP string           `json:"sourceIp"`
		} `json:"input"`
	}
	_ = json.NewDecoder(body).Decode(&testBody)

	switch {
	case testBody.Input != nil && len(testBody.Input.Messages) > 0:
		segs := make([]string, 0, len(testBody.Input.Messages))
		for _, m := range testBody.Input.Messages {
			content, _ := m["content"].(string)
			if content != "" {
				segs = append(segs, content)
			}
		}
		input.Normalized = hookcore.PayloadFromTextSegments(segs)
	case testBody.Input != nil && testBody.Input.Prompt != "":
		input.Normalized = hookcore.PayloadFromTextSegments([]string{testBody.Input.Prompt})
	}
	if testBody.Input != nil {
		input.SourceIP = strings.TrimSpace(testBody.Input.SourceIP)
	}

	return input
}

// runHook executes a hook against input under a timeout derived from
// timeoutMs. A zero or negative timeoutMs falls back to 3 seconds. The
// returned error (if any) is a runtime execution error to surface in the
// response body.
func runHook(parent context.Context, hook hookcore.Hook, input *hookcore.HookInput, timeoutMs int) (*hookcore.HookResult, int64, error) {
	timeout := time.Duration(timeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	start := time.Now()
	result, err := hook.Execute(ctx, input)
	elapsed := time.Since(start).Milliseconds()
	return result, elapsed, err
}
