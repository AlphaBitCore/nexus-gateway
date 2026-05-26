// Package debug — debug_gap_test.go covers branches not reached by
// the existing test files.
//
// Named failure modes:
//   - ProviderTestHandler: invalid JSON, missing baseUrl, invalid adapterType,
//     unregistered adapter, adapter.Probe error, adapter.Probe success
//   - normalizeIngressBodyFormat: empty, valid, unknown (defaults to openai)
//   - schemaMode: nil bridge passthrough/translated/rejected; non-nil bridge
//   - simulateEndpointToProvider: all branches
//   - projectTargets: invalid adapter type (skips format), empty ingress (skips schemaMode)
//   - HooksTestHandler: empty implementationId, rulepack error, nil logger on execErr
//   - buildHookTestInput: nil body
//   - runHook: zero timeout falls back to 3 s default
package debug

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"log/slog"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/canonicalbridge"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/rulepack"
)

var dbgLogger = slog.Default()


// probeOnlyAdapter is a separate test double for ProviderTestHandler tests.
// Uses value receivers (not pointer) to avoid conflict with the pointer-receiver
// stubProbeAdapter in credential_probe_endpoint_test.go.
type probeOnlyAdapter struct {
	format provcore.Format
	result *provcore.ProbeResult
	probeE error
}

func (a probeOnlyAdapter) Format() provcore.Format      { return a.format }
func (a probeOnlyAdapter) SupportsShape(sh typology.WireShape) bool { return sh == typology.WireShapeOpenAIChat }
func (a probeOnlyAdapter) PrepareBody(req provcore.Request) ([]byte, []string, error) {
	return req.Body, nil, nil
}
func (a probeOnlyAdapter) Execute(_ context.Context, _ provcore.Request) (*provcore.Response, error) {
	return &provcore.Response{StatusCode: 200}, nil
}
func (a probeOnlyAdapter) ExecuteWithBody(_ context.Context, _ provcore.Request, _ []byte, _ []string) (*provcore.Response, error) {
	return &provcore.Response{StatusCode: 200}, nil
}
func (a probeOnlyAdapter) Probe(_ context.Context, _ provcore.CallTarget) (*provcore.ProbeResult, error) {
	return a.result, a.probeE
}

func newProviderTestReq(body string) *http.Request {
	return httptest.NewRequest(http.MethodPost, "/internal/provider-test",
		strings.NewReader(body))
}

func TestProviderTestHandler_invalidJSON_returns400(t *testing.T) {
	reg := provcore.NewRegistry()
	h := ProviderTestHandler(reg, dbgLogger)
	w := httptest.NewRecorder()
	h(w, newProviderTestReq(`{invalid`))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", w.Code)
	}
}

func TestProviderTestHandler_missingBaseURL_returns400(t *testing.T) {
	reg := provcore.NewRegistry()
	h := ProviderTestHandler(reg, dbgLogger)
	w := httptest.NewRecorder()
	h(w, newProviderTestReq(`{"providerName":"openai","adapterType":"openai","baseUrl":"","apiKey":"sk-x"}`))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", w.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["success"] != false {
		t.Errorf("success: got %v", resp["success"])
	}
}

func TestProviderTestHandler_invalidAdapterType_returns400(t *testing.T) {
	reg := provcore.NewRegistry()
	h := ProviderTestHandler(reg, dbgLogger)
	w := httptest.NewRecorder()
	h(w, newProviderTestReq(`{"providerName":"x","adapterType":"totally-unknown-format","baseUrl":"https://api.example.com"}`))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", w.Code)
	}
}

func TestProviderTestHandler_unregisteredAdapter_returns400(t *testing.T) {
	reg := provcore.NewRegistry()
	// Register nothing for anthropic → Get returns !ok
	h := ProviderTestHandler(reg, dbgLogger)
	body, _ := json.Marshal(map[string]any{
		"providerName": "anthropic",
		"adapterType":  "anthropic",
		"baseUrl":      "https://api.anthropic.com",
	})
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest(http.MethodPost, "/internal/provider-test", bytes.NewReader(body)))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", w.Code)
	}
}

func TestProviderTestHandler_probeError_returns200WithFailure(t *testing.T) {
	reg := provcore.NewRegistry()
	reg.MustRegister(probeOnlyAdapter{format: provcore.FormatOpenAI, probeE: context.DeadlineExceeded})
	h := ProviderTestHandler(reg, dbgLogger)
	body, _ := json.Marshal(map[string]any{
		"providerName": "openai",
		"adapterType":  "openai",
		"baseUrl":      "https://api.openai.com",
		"apiKey":       "sk-test",
	})
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest(http.MethodPost, "/internal/provider-test", bytes.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200", w.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["success"] != false {
		t.Errorf("success: got %v", resp["success"])
	}
	if resp["hasAPIKey"] != true {
		t.Errorf("hasAPIKey: got %v", resp["hasAPIKey"])
	}
}

func TestProviderTestHandler_probeSuccess_returns200WithSuccess(t *testing.T) {
	reg := provcore.NewRegistry()
	reg.MustRegister(probeOnlyAdapter{
		format: provcore.FormatOpenAI,
		result: &provcore.ProbeResult{OK: true, LatencyMs: 42, Detail: "pong"},
	})
	h := ProviderTestHandler(reg, dbgLogger)
	body, _ := json.Marshal(map[string]any{
		"providerName": "openai",
		"adapterType":  "openai",
		"baseUrl":      "https://api.openai.com",
		"apiKey":       "",
	})
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest(http.MethodPost, "/internal/provider-test", bytes.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200", w.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["success"] != true {
		t.Errorf("success: got %v", resp["success"])
	}
	if resp["latencyMs"].(float64) != 42 {
		t.Errorf("latencyMs: got %v", resp["latencyMs"])
	}
	if resp["hasAPIKey"] != false {
		t.Errorf("hasAPIKey: got %v", resp["hasAPIKey"])
	}
}


func TestNormalizeIngressBodyFormat_empty_returnsOpenAI(t *testing.T) {
	got := normalizeIngressBodyFormat("")
	if got != provcore.FormatOpenAI {
		t.Errorf("empty: got %q, want openai", got)
	}
}

func TestNormalizeIngressBodyFormat_valid_returnsAsIs(t *testing.T) {
	got := normalizeIngressBodyFormat("anthropic")
	if got != provcore.FormatAnthropic {
		t.Errorf("anthropic: got %q", got)
	}
}

func TestNormalizeIngressBodyFormat_unknown_returnsOpenAI(t *testing.T) {
	got := normalizeIngressBodyFormat("totally-unknown-wire-format")
	if got != provcore.FormatOpenAI {
		t.Errorf("unknown: got %q, want openai", got)
	}
}


func TestSchemaMode_nilBridge_sameFormats_passthrough(t *testing.T) {
	got := schemaMode(provcore.FormatOpenAI, provcore.FormatOpenAI, typology.WireShapeOpenAIChat, nil)
	if got != "passthrough" {
		t.Errorf("same format, nil bridge: got %q, want passthrough", got)
	}
}

func TestSchemaMode_nilBridge_openAIIngressOtherProvider_translated(t *testing.T) {
	got := schemaMode(provcore.FormatOpenAI, provcore.FormatAnthropic, typology.WireShapeOpenAIChat, nil)
	if got != "translated" {
		t.Errorf("openai→anthropic, nil bridge: got %q, want translated", got)
	}
}

func TestSchemaMode_nilBridge_nonOpenAIIngress_rejected(t *testing.T) {
	got := schemaMode(provcore.FormatAnthropic, provcore.FormatOpenAI, typology.WireShapeOpenAIChat, nil)
	if got != "rejected" {
		t.Errorf("anthropic→openai, nil bridge: got %q, want rejected", got)
	}
}

// stubBridgeForDebug implements canonicalbridge.API for schemaMode tests.
type stubBridgeForDebug struct {
	routable bool
}

func (s stubBridgeForDebug) EndpointRoutable(ep typology.WireShape, ingress, provider provcore.Format) bool {
	return s.routable
}

func (s stubBridgeForDebug) TargetNativelyServesResponsesAPI(target provcore.Format) bool {
	return false
}

func (s stubBridgeForDebug) IngressChatToCanonical(ingress provcore.Format, body []byte, target provcore.CallTarget) ([]byte, error) {
	return body, nil
}

func (s stubBridgeForDebug) ResponseCanonicalToIngress(ingress provcore.Format, canonical []byte) ([]byte, error) {
	return canonical, nil
}

func (s stubBridgeForDebug) ResponseAcrossFormats(from typology.WireShape, to typology.WireShape, body []byte) ([]byte, error) {
	return body, nil
}

func (s stubBridgeForDebug) NewStreamTranscoder(ingress, target provcore.Format, model string) canonicalbridge.StreamTranscoder {
	return nil
}

func TestSchemaMode_nonNilBridge_notRoutable_rejected(t *testing.T) {
	got := schemaMode(provcore.FormatAnthropic, provcore.FormatOpenAI, typology.WireShapeOpenAIChat, stubBridgeForDebug{routable: false})
	if got != "rejected" {
		t.Errorf("not routable: got %q, want rejected", got)
	}
}

func TestSchemaMode_nonNilBridge_routable_sameFormat_passthrough(t *testing.T) {
	got := schemaMode(provcore.FormatOpenAI, provcore.FormatOpenAI, typology.WireShapeOpenAIChat, stubBridgeForDebug{routable: true})
	if got != "passthrough" {
		t.Errorf("routable same format: got %q, want passthrough", got)
	}
}

func TestSchemaMode_nonNilBridge_routable_differentFormat_translated(t *testing.T) {
	got := schemaMode(provcore.FormatOpenAI, provcore.FormatAnthropic, typology.WireShapeOpenAIChat, stubBridgeForDebug{routable: true})
	if got != "translated" {
		t.Errorf("routable diff format: got %q, want translated", got)
	}
}


func TestSimulateEndpointToProvider_allCases(t *testing.T) {
	cases := []struct {
		in   string
		want typology.WireShape
	}{
		{"chat", typology.WireShapeOpenAIChat},
		{"CHAT", typology.WireShapeOpenAIChat}, // case-insensitive
		{"embeddings", typology.WireShapeOpenAIEmbeddings},
		{"models", typology.WireShapeNone},
		{"unknown_endpoint", typology.WireShapeOpenAIChat}, // default
		{"", typology.WireShapeOpenAIChat},                 // empty → default
	}
	for _, tc := range cases {
		got := simulateEndpointToProvider(tc.in)
		if got != tc.want {
			t.Errorf("input %q: got %q, want %q", tc.in, got, tc.want)
		}
	}
}


func TestProjectTargets_invalidAdapterType_skipsFormat(t *testing.T) {
	targets := []routingcore.RoutingTarget{
		{
			ProviderID:      "p1",
			ProviderName:    "bad-provider",
			ModelID:         "m1",
			ModelName:       "Model One",
			ProviderModelID: "model-one",
			Source:          "primary",
			AdapterType:     "not-a-valid-format",
		},
	}
	out := projectTargets(targets, provcore.FormatOpenAI, typology.WireShapeOpenAIChat, nil)
	if len(out) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(out))
	}
	if out[0].ProviderFormat != "" {
		t.Errorf("providerFormat: got %q, want empty for invalid adapter type", out[0].ProviderFormat)
	}
	if out[0].SchemaMode != "" {
		t.Errorf("schemaMode: got %q, want empty for invalid adapter type", out[0].SchemaMode)
	}
}

func TestProjectTargets_emptyIngress_skipsSchemaMode(t *testing.T) {
	targets := []routingcore.RoutingTarget{
		{
			ProviderID:      "p1",
			ProviderName:    "openai",
			ModelID:         "m1",
			ModelName:       "gpt-4o",
			ProviderModelID: "gpt-4o",
			Source:          "primary",
			AdapterType:     "openai",
		},
	}
	// ingress == "" → schemaMode is not called → SchemaMode stays empty.
	out := projectTargets(targets, "", typology.WireShapeOpenAIChat, nil)
	if len(out) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(out))
	}
	if out[0].ProviderFormat != "openai" {
		t.Errorf("providerFormat: got %q, want openai", out[0].ProviderFormat)
	}
	if out[0].SchemaMode != "" {
		t.Errorf("schemaMode: got %q, want empty when ingress is empty", out[0].SchemaMode)
	}
}

// HooksTestHandler additional branches

func TestHooksTestHandler_emptyImplementationID_returns400(t *testing.T) {
	reg := hookcore.NewHookRegistry()
	reg.Freeze()
	h := HooksTestHandler(reg, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/internal/hooks-test",
		strings.NewReader(`{"hookConfig":{"id":"x","implementationId":"","stage":"request"},"rawBody":""}`))
	w := httptest.NewRecorder()
	h(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", w.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if !strings.Contains(resp["error"].(string), "implementationId") {
		t.Errorf("error: got %v, want mention of implementationId", resp["error"])
	}
}

// errRulePackLister is a stub InstallLister that always returns an error.
type errRulePackLister struct{}

func (errRulePackLister) LoadEffectiveSetsForHook(_ context.Context, _ string) ([]rulepack.EffectiveRuleSet, error) {
	return nil, errors.New("db unreachable")
}

func TestHooksTestHandler_rulepackEnrichError_returns502(t *testing.T) {
	// content-safety is a RulePackConsumer, so Enrich will call
	// LoadEffectiveSetsForHook for it. The errRulePackLister returns an
	// error → Enrich returns error → handler returns 502.
	reg := buildTestRegistry(t)
	h := HooksTestHandler(reg, errRulePackLister{}, slog.Default())
	body, _ := json.Marshal(map[string]any{
		"hookConfig": map[string]any{
			"id":               "hc-err",
			"implementationId": "content-safety",
			"stage":            "request",
			"timeoutMs":        500,
			"config": map[string]any{
				"categories": map[string]any{"violence": true},
				"onMatch":    map[string]any{"inflightAction": "block-hard"},
			},
		},
		"rawBody": "",
	})
	req := httptest.NewRequest(http.MethodPost, "/internal/hooks-test", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h(w, req)
	if w.Code != http.StatusBadGateway {
		t.Errorf("status: got %d, want 502", w.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["code"] != "rulepack_enrich" {
		t.Errorf("code: got %v, want rulepack_enrich", resp["code"])
	}
}

// noopHook always approves without side effects.
type noopHook struct {
	hookcore.AnyEndpointAnyModality
}

func (noopHook) Execute(_ context.Context, _ *hookcore.HookInput) (*hookcore.HookResult, error) {
	return &hookcore.HookResult{Decision: "APPROVE"}, nil
}

func TestHooksTestHandler_nilLogger_execErr_noLogPanic(t *testing.T) {
	// Verifies the `if logger != nil` guard: passing a nil logger must not
	// panic when the hook execution returns an error.
	reg := hookcore.NewHookRegistry()
	reg.Register("always-err", func(cfg *hookcore.HookConfig) (hookcore.Hook, error) {
		return &alwaysErrHook{}, nil
	})
	reg.Freeze()
	h := HooksTestHandler(reg, nil, nil) // nil logger
	body, _ := json.Marshal(map[string]any{
		"hookConfig": map[string]any{
			"id":               "hc-fail",
			"implementationId": "always-err",
			"stage":            "request",
			"timeoutMs":        500,
			"config":           map[string]any{},
		},
		"rawBody": "",
	})
	req := httptest.NewRequest(http.MethodPost, "/internal/hooks-test", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h(w, req) // must not panic
	if w.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200 (runtime error envelope)", w.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["error"] == nil {
		t.Errorf("expected runtime error field, got %v", resp)
	}
}

// alwaysErrHook returns an error from Execute.
type alwaysErrHook struct {
	hookcore.AnyEndpointAnyModality
}

func (alwaysErrHook) Execute(_ context.Context, _ *hookcore.HookInput) (*hookcore.HookResult, error) {
	return nil, errors.New("hook always fails")
}

// buildHookTestInput nil body

func TestBuildHookTestInput_nilBody_returnsEmptyInput(t *testing.T) {
	input := buildHookTestInput("request", nil)
	if input == nil {
		t.Fatal("input should not be nil")
	}
	if input.Stage != "request" {
		t.Errorf("stage: got %q, want request", input.Stage)
	}
	if input.Normalized != nil {
		t.Errorf("normalized: expected nil for nil body, got %v", input.Normalized)
	}
}

// runHook zero timeout

func TestRunHook_zeroTimeout_usesDefault(t *testing.T) {
	// A zero timeoutMs should fall back to 3 s, not panic or immediate-cancel.
	hook := &noopHook{}
	input := &hookcore.HookInput{Stage: "request"}
	result, elapsed, err := runHook(context.Background(), hook, input, 0)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if result == nil {
		t.Error("result should not be nil for noopHook")
	}
	// Elapsed should be very fast (well under 3 s); just verify it's non-negative.
	if elapsed < 0 {
		t.Errorf("elapsed: got %d ms, want ≥0", elapsed)
	}
}

func TestRunHook_negativeTimeout_usesDefault(t *testing.T) {
	hook := &noopHook{}
	input := &hookcore.HookInput{Stage: "request"}
	result, _, err := runHook(context.Background(), hook, input, -1)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if result == nil {
		t.Error("result should not be nil for noopHook")
	}
}

// CredentialProbeHandler logger nil branch

func TestCredentialProbeHandler_missingID_noLoggerNoPanic(t *testing.T) {
	// Verifies that the nil-logger guard in writeJSON path doesn't panic.
	// The handler bails at credID=="" before touching the logger.
	reg := provcore.NewRegistry()
	// Pass nil for all concrete deps; handler bails at empty credID.
	h := CredentialProbeHandler(nil, reg, nil, nil)
	r := httptest.NewRequest(http.MethodPost, "/internal/v1/credentials//probe",
		strings.NewReader("{}"))
	r.SetPathValue("id", "")
	w := httptest.NewRecorder()
	h(w, r) // must not panic
	if w.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", w.Code)
	}
}

// Ensure io is used (buildHookTestInput takes io.Reader).
var _ io.Reader = strings.NewReader("")
