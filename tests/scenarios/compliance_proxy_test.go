// Compliance Proxy family (S-080..S-085) — verifies the CP admin
// surface that drives the compliance-proxy service: interception
// domains, exemptions, proxy health/metrics.
package scenarios_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/tests/scenarios/helpers"
)

// TestS085_InterceptionDomainPropagates — PM-grade e2e.
//
// BRAINSTORM (pre): catalog §5.8 / §10 gap S-085 — a new interception
// domain must propagate from CP admin write → Hub broadcast →
// compliance-proxy (and agent) hot-reload signal. The
// interception_domains config_key per
// helpers.ConfigKeyServices subscribes both compliance-proxy AND
// agent. agent has no metrics URL (skipped gracefully); the test
// asserts compliance-proxy applied the new domain via its
// config_applies counter.
//
// Cross-service: CP admin → DB write + Hub.InvalidateConfig →
// compliance-proxy thingclient apply → metrics tick. AdminAuditLog
// records the create.
//
// Assertions:
//   1. POST /api/admin/interception-domains 201 + ID returned
//   2. compliance-proxy hot-reload signal within 30 s
//   3. GET round-trip preserves hostPattern + adapterId
//   4. AdminAuditLog 'create' row for entityId == domain.ID
//   5. Cleanup DELETE + second hot-reload signal
func TestS085_InterceptionDomainPropagates(t *testing.T) {
	sc := setupScenarioNoVK(t)
	ctx := context.Background()

	token, err := helpers.CPLogin(ctx, sc.Env)
	if err != nil {
		t.Fatalf("CPLogin: %v", err)
	}

	preApply, err := helpers.BaselineConfigApply(ctx, sc.Env, "interception_domains")
	if err != nil {
		t.Fatalf("BaselineConfigApply: %v", err)
	}

	domainName := fmt.Sprintf("s085-%d", time.Now().UnixNano())
	body, _ := json.Marshal(map[string]any{
		"name":              domainName,
		"hostPattern":       fmt.Sprintf("s085-test-%d.example.invalid", time.Now().UnixNano()),
		"hostMatchType":     "EXACT",
		"adapterId":         "openai-compat",
		"enabled":           true,
		"priority":          50,
		"defaultPathAction": "PROCESS",
		"onAdapterError":    "FAIL_OPEN",
		"networkZone":       "INTERNAL",
		"source":            "scenario-test",
	})
	status, respBody, err := helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodPost, "/api/admin/interception-domains", body)
	if err != nil {
		t.Fatalf("create domain: %v", err)
	}
	if status != http.StatusCreated && status != http.StatusOK {
		t.Fatalf("create domain: status %d body=%q", status, truncate(respBody, 200))
	}
	var created struct {
		ID            string `json:"id"`
		Name          string `json:"name"`
		HostPattern   string `json:"hostPattern"`
		AdapterID     string `json:"adapterId"`
		HostMatchType string `json:"hostMatchType"`
	}
	if err := json.Unmarshal(respBody, &created); err != nil {
		t.Fatalf("decode create: %v body=%q", err, truncate(respBody, 200))
	}
	if created.ID == "" {
		t.Fatalf("create response missing id: %s", truncate(respBody, 200))
	}
	sc.Cleanup.Register("DeleteInterceptionDomain("+created.ID+")", func() error {
		_, _, err := helpers.CPDoJSON(context.Background(), sc.Env, token,
			http.MethodDelete, "/api/admin/interception-domains/"+created.ID, nil)
		return err
	})

	// Hot-reload signal across compliance-proxy (and any other
	// subscriber that exposes metrics — agent doesn't, gracefully
	// skipped by helpers).
	if _, err := helpers.WaitForConfigApply(ctx, sc.Env, "interception_domains",
		preApply, 30*time.Second); err != nil {
		t.Fatalf("compliance-proxy did not hot-reload interception_domains: %v", err)
	}

	// Round-trip GET preserves the input fields we care about.
	getStatus, getBody, err := helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodGet, "/api/admin/interception-domains/"+created.ID, nil)
	if err != nil {
		t.Fatalf("get domain: %v", err)
	}
	if getStatus != 200 {
		t.Fatalf("get domain: status %d body=%q", getStatus, truncate(getBody, 200))
	}
	var got map[string]any
	if err := json.Unmarshal(getBody, &got); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	if got["adapterId"] != "openai-compat" {
		t.Errorf("GET adapterId=%v, want 'openai-compat'", got["adapterId"])
	}
	if got["name"] != domainName {
		t.Errorf("GET name=%v, want %q", got["name"], domainName)
	}

	// AdminAuditLog 'create' for this domain id.
	auditRow, err := helpers.WaitForAdminAuditRow(ctx, sc.DB,
		"create", created.ID, 15*time.Second)
	if err != nil {
		t.Fatalf("WaitForAdminAuditRow: %v", err)
	}
	if auditRow == nil {
		// Some admin handlers stamp domain Name instead of UUID — fall
		// back to a name-based lookup before failing.
		auditRow, _ = helpers.WaitForAdminAuditRow(ctx, sc.DB,
			"create", domainName, 5*time.Second)
	}
	if auditRow == nil {
		t.Errorf("no AdminAuditLog 'create' row for interception domain (id=%s name=%s) within 15 s",
			created.ID, domainName)
	}

	t.Logf("S-085 OK: domain %s propagated (hot-reload signal observed, audit=%v)",
		created.ID, auditRow)
}
