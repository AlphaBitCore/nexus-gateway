package proxy

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/builtins"
	goHooks "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	compliance "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
)

// newIPAllowlistHookCache builds the shipped ip-access-filter hook in allowlist
// mode. It is bound to every endpoint (AnyEndpointAnyModality), it ships in the
// seed disabled, and an operator can turn it on from the admin UI — which is
// what makes an endpoint that never populates SourceIP a live hazard rather
// than a theoretical one.
func newIPAllowlistHookCache(t *testing.T, cidr string) *compliance.HookConfigCache {
	t.Helper()
	loader := func(_ context.Context) ([]goHooks.HookConfig, error) {
		return []goHooks.HookConfig{{
			ID:                "ip-1",
			ImplementationID:  "ip-access-filter",
			Name:              "ip-allowlist",
			Priority:          10,
			Enabled:           true,
			Stage:             "request",
			FailBehavior:      "fail-closed",
			TimeoutMs:         1000,
			ApplicableIngress: []string{"ALL"},
			Config: map[string]any{
				"mode":      "allowlist",
				"allowlist": []any{cidr},
			},
		}}, nil
	}
	cache := compliance.NewHookConfigCache(loader, builtins.Registry, 0, slog.Default())
	if err := cache.Start(context.Background()); err != nil {
		t.Fatalf("cache.Start: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	return cache
}

// The guardrail endpoint builds its own HookInput, and it used to populate only
// the content half. Metadata hooks do not abstain on a zero value — they run and
// read it. ip-access-filter treats a source IP it cannot parse as a hard reject,
// so with an empty SourceIP the shipped, admin-toggleable allowlist hook rejects
// every guardrail request while the inline path, which populates the field, is
// unaffected. An operator enabling one checkbox would have taken the endpoint
// down and seen nothing wrong with the configuration.
//
// The test drives the real hook rather than asserting on the struct, because the
// defect is not "a field is empty" — it is what a real policy does with it.
func TestServeGuardrail_PopulatesRequestMetadataForPolicyHooks(t *testing.T) {
	const clientCIDR = "192.0.2.0/24"
	const clientIP = "192.0.2.10:41000"

	deps, _ := sttDeps(t, "http://127.0.0.1:0")
	deps.HookConfigCache = newIPAllowlistHookCache(t, clientCIDR)
	h := NewHandler(deps).ServeGuardrail()

	req := httptest.NewRequest(http.MethodPost, "/v1/guardrail",
		strings.NewReader(`{"stage":"input","content":"hello world"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = clientIP
	rr := httptest.NewRecorder()
	h(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	resp := decodeGuardrail(t, rr)
	if resp.Action == "block" {
		t.Fatalf("a request from an allowlisted address was blocked: action=%q reason=%q\n"+
			"the allowlist is %s and the caller is %s, so the only way to reject is a source IP "+
			"the hook could not parse — i.e. the endpoint handed the policy an empty one",
			resp.Action, resp.Reason, clientCIDR, clientIP)
	}
}

// Anti-vacuity: the same hook on the same endpoint must still reject an address
// outside the allowlist. Without this, populating SourceIP with anything at all
// — including a constant — would satisfy the test above while disabling the
// control.
func TestServeGuardrail_IPAllowlistStillRejectsOutsideAddresses(t *testing.T) {
	deps, _ := sttDeps(t, "http://127.0.0.1:0")
	deps.HookConfigCache = newIPAllowlistHookCache(t, "192.0.2.0/24")
	h := NewHandler(deps).ServeGuardrail()

	req := httptest.NewRequest(http.MethodPost, "/v1/guardrail",
		strings.NewReader(`{"stage":"input","content":"hello world"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "198.51.100.7:41000"
	rr := httptest.NewRecorder()
	h(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	resp := decodeGuardrail(t, rr)
	if resp.Action != "block" {
		t.Fatalf("an address outside the allowlist was not blocked: action=%q — the endpoint "+
			"is passing the policy an address that always matches, which is worse than "+
			"passing none", resp.Action)
	}
}
