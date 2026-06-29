package debug

import (
	"context"
	"github.com/goccy/go-json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/builtins"
	hooks "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/rulepack"
)

// buildTestRegistry returns a fresh registry that includes both the shared
// factories and the AI-gateway-local quality-checker, mirroring what
// cmd/ai-gateway/main.go assembles at runtime. A sleeper factory is added
// under the id "test:sleeper" for the execute-timeout case.
func buildTestRegistry(t *testing.T) *hooks.HookRegistry {
	t.Helper()
	r := builtins.Registry.Clone()
	// quality-checker is already registered in hooks.Registry (the
	// Clone above carries it); the explicit re-registration that lived
	// here was a leftover from when NewQualityChecker still resided in
	// ai-gateway/internal/hooks before the 59b286b3 three-side-consistency
	// refactor moved it into shared/hooks. Re-registering would panic on
	// duplicate; skipping it is correct.
	r.Register("test:sleeper", func(cfg *hooks.HookConfig) (hooks.Hook, error) {
		return &sleeperHook{cfg: cfg}, nil
	})
	r.Freeze()
	return r
}

// sleeperHook's Execute blocks on the context until the test timeout fires,
// then returns the context's error. Used to exercise the runHook timeout
// guard in HooksTestHandler without relying on wall-clock sleeps.
type sleeperHook struct {
	hooks.AnyEndpointAnyModality
	cfg *hooks.HookConfig
}

func (s *sleeperHook) Execute(ctx context.Context, _ *hooks.HookInput) (*hooks.HookResult, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// testRulePackLister injects a single incest/sexual fantasy rule so admin hook
// tests mirror production rule-pack enrichment (regression: inline-only path
// would Approve phrases the pack is meant to catch).
type testRulePackLister struct{}

func (testRulePackLister) LoadEffectiveSetsForHook(ctx context.Context, hookID string) ([]rulepack.EffectiveRuleSet, error) {
	return []rulepack.EffectiveRuleSet{
		{
			Install: rulepack.Install{
				ID: "install-test", PackName: "test-pack", PinVersion: "1",
				BoundHookID: hookID, Enabled: true,
			},
			Pack: rulepack.Pack{
				Name: "test-pack", Version: "1", Maintainer: "test",
				Rules: []rulepack.Rule{{
					RuleID:   "cs-x-006",
					Category: "content_safety.sexual",
					Severity: "soft",
					Pattern:  `(?i)\b(?:incest(?:uous)?|sex\s+with\s+(?:my|her|his)\s+(?:sister|brother|mother|father|daughter|son))\s+(?:story|stories|fantas(?:y|ies)|roleplay)\b`,
				}},
			},
		},
	}, nil
}

func postJSON(t *testing.T, handler http.HandlerFunc, body any) *httptest.ResponseRecorder {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/internal/hooks-test", strings.NewReader(string(buf)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

func decodeJSON(t *testing.T, r io.Reader) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.NewDecoder(r).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

// TestHooksTestHandler_ContentSafety_RejectHard drives a shared builtin
// through the handler. This locks the no-regression contract for every
// shared factory now that they reach execution via the proxy path.
func TestHooksTestHandler_ContentSafety_RejectHard(t *testing.T) {
	h := HooksTestHandler(buildTestRegistry(t), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	body := map[string]any{
		"hookConfig": map[string]any{
			"id":               "hc-cs",
			"name":             "content-safety",
			"type":             "builtin",
			"implementationId": "content-safety",
			"stage":            "request",
			"timeoutMs":        500,
			"config": map[string]any{
				"categories": map[string]any{"violence": true},
				// Canonical onMatch shape — content-safety with
				// inflightAction=block-hard maps to REJECT_HARD decision.
				"onMatch": map[string]any{
					"inflightAction": "block-hard",
					"storageAction":  "redact",
				},
			},
		},
		"rawBody": `{"input":{"messages":[{"role":"user","content":"I want to kill the process"}]}}`,
	}

	rec := postJSON(t, h, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	out := decodeJSON(t, rec.Body)
	if out["stage"] != "request" {
		t.Errorf("stage: want request, got %v", out["stage"])
	}
	output, ok := out["output"].(map[string]any)
	if !ok {
		t.Fatalf("output is not an object: %v", out)
	}
	// hooks.HookResult uses lowerCamelCase JSON tags (decision, reasonCode,
	// hookId, etc.); the wire shape is what the UI deserialises directly.
	if output["decision"] != "REJECT_HARD" {
		t.Errorf("decision: want REJECT_HARD, got %v (output=%v)", output["decision"], output)
	}
	if output["reasonCode"] != "CONTENT_SAFETY_VIOLATION" {
		t.Errorf("reasonCode: want CONTENT_SAFETY_VIOLATION, got %v", output["reasonCode"])
	}
}

func TestHooksTestHandler_ContentSafety_RulePackEnrichMatches(t *testing.T) {
	h := HooksTestHandler(buildTestRegistry(t), testRulePackLister{}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	body := map[string]any{
		"hookConfig": map[string]any{
			"id":               "hc-cs-pack",
			"name":             "content-safety",
			"type":             "builtin",
			"implementationId": "content-safety",
			"stage":            "request",
			"timeoutMs":        500,
			"config": map[string]any{
				"categories": map[string]any{"sexual": true},
				// Legacy onMatch shape (backward-compat parse): the deprecated
				// block-soft inflightAction now folds into the single block
				// action, which maps to a REJECT_HARD decision (soft-block was
				// merged into block).
				"onMatch": map[string]any{
					"inflightAction": "block-soft",
					"storageAction":  "redact",
				},
			},
		},
		"rawBody": `{"input":{"prompt":"Hello world email: user@example.com sex with his mother fantasies"}}`,
	}

	rec := postJSON(t, h, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	out := decodeJSON(t, rec.Body)
	output, ok := out["output"].(map[string]any)
	if !ok {
		t.Fatalf("output is not an object: %v", out)
	}
	if output["decision"] != "REJECT_HARD" {
		t.Errorf("decision: want REJECT_HARD from pack rule (block-soft folded into block), got %v (output=%v)", output["decision"], output)
	}
}

// TestHooksTestHandler_QualityChecker_Flags_Short verifies that the factory
// resolves against gwHookRegistry (not the shared registry) and executes,
// returning a valid block (REJECT_HARD) response for a known builtin
// implementationId.
func TestHooksTestHandler_QualityChecker_Flags_Short(t *testing.T) {
	h := HooksTestHandler(buildTestRegistry(t), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// QualityChecker treats a very short assistant response as anomalous.
	// The legacy lever onMatch.inflightAction=block-soft now folds into the
	// single block action → REJECT_HARD decision with reasonCode=QUALITY_ANOMALY.
	body := map[string]any{
		"hookConfig": map[string]any{
			"id":               "hc-qc",
			"name":             "quality-checker",
			"type":             "builtin",
			"implementationId": "quality-checker",
			"stage":            "response",
			"timeoutMs":        500,
			"config": map[string]any{
				"onMatch": map[string]any{
					"inflightAction": "block-soft",
					"storageAction":  "keep",
				},
			},
		},
		// buildHookTestInput reads input.messages; for response-stage quality
		// check we supply a short assistant reply as the sample.
		"rawBody": `{"input":{"messages":[{"role":"assistant","content":"Hi"}]}}`,
	}

	rec := postJSON(t, h, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	out := decodeJSON(t, rec.Body)
	output, ok := out["output"].(map[string]any)
	if !ok {
		t.Fatalf("output is not an object: %v", out)
	}
	// QualityChecker returns a non-APPROVE decision with ReasonCode
	// "QUALITY_ANOMALY" for short responses under anomalyAction=block.
	// QualityChecker does not populate HookID/ImplementationID/HookName
	// on its returned HookResult (unlike ContentSafety), so we only
	// assert on the fields the checker actually sets — and rely on the
	// fact that we got here at all as proof the factory was resolved
	// through gwHookRegistry (regression guard).
	if output["decision"] != "REJECT_HARD" {
		t.Errorf("decision: want REJECT_HARD for short response (block-soft folded into block), got %v (output=%v)", output["decision"], output)
	}
	if output["reasonCode"] != "QUALITY_ANOMALY" {
		t.Errorf("reasonCode: want QUALITY_ANOMALY, got %v", output["reasonCode"])
	}
}

func TestHooksTestHandler_UnknownImplementation(t *testing.T) {
	h := HooksTestHandler(buildTestRegistry(t), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	rec := postJSON(t, h, map[string]any{
		"hookConfig": map[string]any{
			"id":               "hc-bad",
			"implementationId": "no-such-hook",
			"stage":            "request",
			"timeoutMs":        500,
		},
		"rawBody": "",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	out := decodeJSON(t, rec.Body)
	if out["code"] != "unknown_implementation" {
		t.Errorf("code: want unknown_implementation, got %v", out["code"])
	}
}

func TestHooksTestHandler_InvalidConfigJSON(t *testing.T) {
	h := HooksTestHandler(buildTestRegistry(t), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// Send raw bytes that look like JSON for the outer envelope but put a
	// broken fragment in `config`. The raw JSON path requires hand-crafting
	// because json.Marshal of a Go map always produces valid JSON.
	rawEnvelope := `{
		"hookConfig": {
			"id": "hc-bad",
			"implementationId": "content-safety",
			"stage": "request",
			"timeoutMs": 500,
			"config": not-valid-json
		},
		"rawBody": ""
	}`
	req := httptest.NewRequest(http.MethodPost, "/internal/hooks-test", strings.NewReader(rawEnvelope))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h(rec, req)

	// The outer envelope itself is malformed (config value is bare tokens),
	// so the handler rejects it at decode time. Either error code is
	// acceptable; the important thing is it does not 500.
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHooksTestHandler_FactoryError(t *testing.T) {
	h := HooksTestHandler(buildTestRegistry(t), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// content-safety requires `categories` — omit it to force factory error.
	rec := postJSON(t, h, map[string]any{
		"hookConfig": map[string]any{
			"id":               "hc-cs",
			"implementationId": "content-safety",
			"stage":            "request",
			"timeoutMs":        500,
			"config":           map[string]any{},
		},
		"rawBody": "",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	out := decodeJSON(t, rec.Body)
	if out["code"] != "factory_error" {
		t.Errorf("code: want factory_error, got %v", out["code"])
	}
}

func TestHooksTestHandler_ExecuteTimeout(t *testing.T) {
	h := HooksTestHandler(buildTestRegistry(t), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	start := time.Now()
	rec := postJSON(t, h, map[string]any{
		"hookConfig": map[string]any{
			"id":               "hc-sleep",
			"implementationId": "test:sleeper",
			"stage":            "request",
			"timeoutMs":        50,
			"config":           map[string]any{},
		},
		"rawBody": "",
	})
	elapsed := time.Since(start)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 (runtime error envelope), got %d body=%s", rec.Code, rec.Body.String())
	}
	if elapsed > 2*time.Second {
		t.Fatalf("timeout guard did not fire: elapsed=%v", elapsed)
	}
	out := decodeJSON(t, rec.Body)
	if out["error"] == nil {
		t.Errorf("expected runtime error field, got %v", out)
	}
	if out["stage"] != "request" {
		t.Errorf("stage: want request, got %v", out["stage"])
	}
}

func TestHooksTestHandler_IPAccessFilter_RequiresSourceIP(t *testing.T) {
	h := HooksTestHandler(buildTestRegistry(t), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	rec := postJSON(t, h, map[string]any{
		"hookConfig": map[string]any{
			"id":               "hc-ip",
			"name":             "ip-access-filter",
			"type":             "builtin",
			"implementationId": "ip-access-filter",
			"stage":            "request",
			"timeoutMs":        500,
			"config": map[string]any{
				"mode":      "allowlist",
				"allowlist": []string{"127.0.0.1/32"},
			},
		},
		"rawBody": `{"input":{"prompt":"Hello world"}}`,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	out := decodeJSON(t, rec.Body)
	if out["code"] != "invalid_test_input" {
		t.Errorf("code: want invalid_test_input, got %v", out["code"])
	}
}

func TestHooksTestHandler_IPAccessFilter_ApprovesWhenSourceIPAllowed(t *testing.T) {
	h := HooksTestHandler(buildTestRegistry(t), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	rec := postJSON(t, h, map[string]any{
		"hookConfig": map[string]any{
			"id":               "hc-ip",
			"name":             "ip-access-filter",
			"type":             "builtin",
			"implementationId": "ip-access-filter",
			"stage":            "request",
			"timeoutMs":        500,
			"config": map[string]any{
				"mode":      "allowlist",
				"allowlist": []string{"127.0.0.1/32"},
			},
		},
		"rawBody": `{"input":{"prompt":"Hello world","sourceIp":"127.0.0.1"}}`,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	out := decodeJSON(t, rec.Body)
	output, ok := out["output"].(map[string]any)
	if !ok {
		t.Fatalf("output is not an object: %v", out)
	}
	if output["decision"] != "APPROVE" {
		t.Errorf("decision: want APPROVE, got %v (output=%v)", output["decision"], output)
	}
}
