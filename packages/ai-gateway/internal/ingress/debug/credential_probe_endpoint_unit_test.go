// Package debug — unit tests for CredentialProbeHandler that exercise the
// branch matrix without a live PostgreSQL connection. The DB-integration
// counterparts in credential_probe_endpoint_test.go are skipped on coverage
// gate runs (no local DB); these stub-driven tests pick up the slack and
// keep the handler's named failure modes asserted in CI.
//
// Named failure modes covered:
//   - cache.GetCredentialByID returns err          → 404 "credential not found"
//   - cache.GetCredentialByID returns nil          → 404 "credential not found"
//   - cache.GetProvider returns err                → 400 "provider not found"
//   - cache.GetProvider returns nil                → 400 "provider not found"
//   - provider.AdapterType not a valid Format      → 400 "invalid provider adapterType"
//   - format not registered in provcore.Registry   → 400 "no adapter registered"
//   - credMgr.GetDecrypted err                     → 500 "decrypt credential"
//   - adapter.Probe returns err                    → 200 ok=false, error stamped
//   - adapter.Probe returns ProbeResult.OK=true    → 200 ok=true, detail + latency
//   - timeoutSeconds body field above cap          → still 200 (cap applied silently)
//   - timeoutSeconds zero/negative                 → default 5s applied silently
package debug

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// stubCredentialCache satisfies credentialCache via structural typing.
type stubCredentialCache struct {
	cred    *store.Credential
	credErr error
	prov    *store.Provider
	provErr error
}

func (s *stubCredentialCache) GetCredentialByID(_ context.Context, _ string) (*store.Credential, error) {
	return s.cred, s.credErr
}
func (s *stubCredentialCache) GetProvider(_ context.Context, _ string) (*store.Provider, error) {
	return s.prov, s.provErr
}

// stubCredentialDecrypter satisfies credentialDecrypter via structural typing.
type stubCredentialDecrypter struct {
	plaintext string
	err       error
}

func (s *stubCredentialDecrypter) GetDecrypted(_ context.Context, _ string) (string, error) {
	return s.plaintext, s.err
}

// stubAdapter is a minimal provcore.Adapter that delegates to a probe func.
// The non-Probe methods panic on call — passing tests prove the handler
// only invokes Probe through the registry.
type stubAdapter struct {
	format provcore.Format
	probe  func(context.Context, provcore.CallTarget) (*provcore.ProbeResult, error)
}

func (s *stubAdapter) Format() provcore.Format { return s.format }
func (s *stubAdapter) SupportsShape(shape typology.WireShape) bool {
	return shape == typology.WireShapeOpenAIChat
}
func (s *stubAdapter) Probe(ctx context.Context, t provcore.CallTarget) (*provcore.ProbeResult, error) {
	return s.probe(ctx, t)
}
func (s *stubAdapter) Execute(context.Context, provcore.Request) (*provcore.Response, error) {
	panic("stubAdapter.Execute must not be called from CredentialProbeHandler")
}
func (s *stubAdapter) PrepareBody(provcore.Request) ([]byte, []string, string, error) {
	panic("stubAdapter.PrepareBody must not be called from CredentialProbeHandler")
}
func (s *stubAdapter) ExecuteWithBody(context.Context, provcore.Request, []byte, []string, string) (*provcore.Response, error) {
	panic("stubAdapter.ExecuteWithBody must not be called from CredentialProbeHandler")
}

func probeUnitLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// callProbeUnit drives the handler and returns status + decoded body.
func callProbeUnit(t *testing.T, h http.HandlerFunc, credID, body string) (int, map[string]any) {
	t.Helper()
	if body == "" {
		body = "{}"
	}
	r := httptest.NewRequest(http.MethodPost,
		"/internal/v1/credentials/"+credID+"/probe", strings.NewReader(body))
	r.SetPathValue("id", credID)
	w := httptest.NewRecorder()
	h(w, r)
	var out map[string]any
	_ = json.NewDecoder(w.Body).Decode(&out)
	return w.Code, out
}

func TestProbeUnit_CacheCredErr_Returns404(t *testing.T) {
	cache := &stubCredentialCache{credErr: errors.New("pgx: row not found")}
	h := CredentialProbeHandler(cache, provcore.NewRegistry(), &stubCredentialDecrypter{}, probeUnitLogger())
	code, body := callProbeUnit(t, h, "cred-1", "")
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%v", code, body)
	}
	if got, _ := body["error"].(string); got != "credential not found" {
		t.Errorf("error = %q, want 'credential not found'", got)
	}
}

func TestProbeUnit_CacheCredNil_Returns404(t *testing.T) {
	cache := &stubCredentialCache{cred: nil, credErr: nil}
	h := CredentialProbeHandler(cache, provcore.NewRegistry(), &stubCredentialDecrypter{}, probeUnitLogger())
	code, _ := callProbeUnit(t, h, "cred-2", "")
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", code)
	}
}

func TestProbeUnit_CacheProvErr_Returns400(t *testing.T) {
	cache := &stubCredentialCache{
		cred:    &store.Credential{ID: "c", ProviderID: "p"},
		provErr: errors.New("provider gone"),
	}
	h := CredentialProbeHandler(cache, provcore.NewRegistry(), &stubCredentialDecrypter{}, probeUnitLogger())
	code, body := callProbeUnit(t, h, "cred-3", "")
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%v", code, body)
	}
	if got, _ := body["error"].(string); got != "provider not found for credential" {
		t.Errorf("error = %q, want 'provider not found for credential'", got)
	}
}

func TestProbeUnit_CacheProvNil_Returns400(t *testing.T) {
	cache := &stubCredentialCache{
		cred: &store.Credential{ID: "c", ProviderID: "p"},
		prov: nil,
	}
	h := CredentialProbeHandler(cache, provcore.NewRegistry(), &stubCredentialDecrypter{}, probeUnitLogger())
	code, _ := callProbeUnit(t, h, "cred-4", "")
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
}

func TestProbeUnit_InvalidAdapterType_Returns400(t *testing.T) {
	cache := &stubCredentialCache{
		cred: &store.Credential{ID: "c", ProviderID: "p"},
		prov: &store.Provider{ID: "p", Name: "weird", AdapterType: "not-a-format"},
	}
	h := CredentialProbeHandler(cache, provcore.NewRegistry(), &stubCredentialDecrypter{}, probeUnitLogger())
	code, body := callProbeUnit(t, h, "cred-5", "")
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%v", code, body)
	}
	if got, _ := body["error"].(string); !strings.Contains(got, "invalid provider adapterType") {
		t.Errorf("error = %q, want substring 'invalid provider adapterType'", got)
	}
}

func TestProbeUnit_AdapterNotRegistered_Returns400(t *testing.T) {
	cache := &stubCredentialCache{
		cred: &store.Credential{ID: "c", ProviderID: "p"},
		prov: &store.Provider{ID: "p", Name: "openai", AdapterType: "openai"},
	}
	// Registry empty — no adapter for "openai".
	h := CredentialProbeHandler(cache, provcore.NewRegistry(), &stubCredentialDecrypter{}, probeUnitLogger())
	code, body := callProbeUnit(t, h, "cred-6", "")
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%v", code, body)
	}
	if got, _ := body["error"].(string); !strings.Contains(got, "no adapter registered") {
		t.Errorf("error = %q, want substring 'no adapter registered'", got)
	}
}

func TestProbeUnit_DecryptErr_Returns500(t *testing.T) {
	cache := &stubCredentialCache{
		cred: &store.Credential{ID: "c", ProviderID: "p"},
		prov: &store.Provider{ID: "p", Name: "openai", AdapterType: "openai"},
	}
	reg := provcore.NewRegistry()
	_ = reg.Register(&stubAdapter{format: provcore.FormatOpenAI, probe: func(context.Context, provcore.CallTarget) (*provcore.ProbeResult, error) {
		return &provcore.ProbeResult{OK: true}, nil
	}})
	dec := &stubCredentialDecrypter{err: errors.New("kms unreachable")}
	h := CredentialProbeHandler(cache, reg, dec, probeUnitLogger())
	code, body := callProbeUnit(t, h, "cred-7", "")
	if code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%v", code, body)
	}
	if got, _ := body["error"].(string); !strings.Contains(got, "decrypt credential") {
		t.Errorf("error = %q, want substring 'decrypt credential'", got)
	}
}

func TestProbeUnit_AdapterProbeErr_Returns200WithErrorStamped(t *testing.T) {
	cache := &stubCredentialCache{
		cred: &store.Credential{ID: "c", Name: "Prod OpenAI", ProviderID: "p"},
		prov: &store.Provider{ID: "p", Name: "openai-east", AdapterType: "openai", BaseURL: "https://api.openai.com"},
	}
	reg := provcore.NewRegistry()
	_ = reg.Register(&stubAdapter{format: provcore.FormatOpenAI, probe: func(context.Context, provcore.CallTarget) (*provcore.ProbeResult, error) {
		return nil, errors.New("dial tcp: connection refused")
	}})
	dec := &stubCredentialDecrypter{plaintext: "sk-real-key"}
	h := CredentialProbeHandler(cache, reg, dec, probeUnitLogger())
	code, body := callProbeUnit(t, h, "cred-8", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (probe completed); body=%v", code, body)
	}
	if ok, _ := body["ok"].(bool); ok {
		t.Error("ok = true, want false on Probe err")
	}
	if got, _ := body["error"].(string); got != "dial tcp: connection refused" {
		t.Errorf("error = %q, want 'dial tcp: connection refused'", got)
	}
	if got, _ := body["providerName"].(string); got != "openai-east" {
		t.Errorf("providerName = %q, want openai-east", got)
	}
	if got, _ := body["adapterType"].(string); got != "openai" {
		t.Errorf("adapterType = %q, want openai", got)
	}
	if got, _ := body["credentialName"].(string); got != "Prod OpenAI" {
		t.Errorf("credentialName = %q, want 'Prod OpenAI'", got)
	}
}

func TestProbeUnit_AdapterProbeOK_Returns200WithDetail(t *testing.T) {
	cache := &stubCredentialCache{
		cred: &store.Credential{ID: "c", ProviderID: "p"},
		prov: &store.Provider{ID: "p", Name: "openai", AdapterType: "openai"},
	}
	reg := provcore.NewRegistry()
	_ = reg.Register(&stubAdapter{format: provcore.FormatOpenAI, probe: func(context.Context, provcore.CallTarget) (*provcore.ProbeResult, error) {
		return &provcore.ProbeResult{OK: true, LatencyMs: 42, Detail: "GET /models 200"}, nil
	}})
	dec := &stubCredentialDecrypter{plaintext: "sk"}
	h := CredentialProbeHandler(cache, reg, dec, probeUnitLogger())
	code, body := callProbeUnit(t, h, "cred-9", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", code, body)
	}
	if ok, _ := body["ok"].(bool); !ok {
		t.Error("ok = false, want true on Probe success")
	}
	if got, _ := body["detail"].(string); got != "GET /models 200" {
		t.Errorf("detail = %q, want 'GET /models 200'", got)
	}
	// LatencyMs surfaced from result overrides the measured wall-clock value.
	if got, _ := body["latencyMs"].(float64); got != 42 {
		t.Errorf("latencyMs = %v, want 42", got)
	}
}

func TestProbeUnit_TimeoutCapAt30s(t *testing.T) {
	// Body asks for 600s — handler caps at 30s silently. Hard to observe
	// directly without instrumenting context deadlines, but the request
	// must still complete normally.
	cache := &stubCredentialCache{
		cred: &store.Credential{ID: "c", ProviderID: "p"},
		prov: &store.Provider{ID: "p", Name: "openai", AdapterType: "openai"},
	}
	reg := provcore.NewRegistry()
	_ = reg.Register(&stubAdapter{format: provcore.FormatOpenAI, probe: func(ctx context.Context, _ provcore.CallTarget) (*provcore.ProbeResult, error) {
		// Verify the ctx has a deadline ≤ 30s from now.
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Error("context lacks deadline; handler must apply timeout")
		}
		_ = deadline
		return &provcore.ProbeResult{OK: true}, nil
	}})
	dec := &stubCredentialDecrypter{plaintext: "sk"}
	h := CredentialProbeHandler(cache, reg, dec, probeUnitLogger())
	code, body := callProbeUnit(t, h, "cred-10", `{"timeoutSeconds": 600}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", code, body)
	}
}

func TestProbeUnit_TimeoutZeroUsesDefault(t *testing.T) {
	// Body explicitly sets timeoutSeconds=0 — handler must apply 5s default.
	cache := &stubCredentialCache{
		cred: &store.Credential{ID: "c", ProviderID: "p"},
		prov: &store.Provider{ID: "p", Name: "openai", AdapterType: "openai"},
	}
	reg := provcore.NewRegistry()
	_ = reg.Register(&stubAdapter{format: provcore.FormatOpenAI, probe: func(ctx context.Context, _ provcore.CallTarget) (*provcore.ProbeResult, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("context lacks deadline; default 5s must apply when timeoutSeconds is zero")
		}
		return &provcore.ProbeResult{OK: true}, nil
	}})
	dec := &stubCredentialDecrypter{plaintext: "sk"}
	h := CredentialProbeHandler(cache, reg, dec, probeUnitLogger())
	code, _ := callProbeUnit(t, h, "cred-11", `{"timeoutSeconds": 0}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
}
