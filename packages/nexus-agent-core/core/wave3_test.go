package core

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// TestVirtualKeyStatus asserts the status helpers: a null column reads "active"
// (legacy rows), an explicit status is surfaced verbatim, and Revocable() is
// true only for active keys (the revoke endpoint 404s otherwise).
func TestVirtualKeyStatus(t *testing.T) {
	revoked := "revoked"
	cases := []struct {
		name      string
		vk        VirtualKey
		status    string
		revocable bool
	}{
		{"null defaults active", VirtualKey{}, "active", true},
		{"explicit revoked", VirtualKey{VKStatus: &revoked}, "revoked", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.vk.Status(); got != tc.status {
				t.Fatalf("Status() = %q, want %q", got, tc.status)
			}
			if got := tc.vk.Revocable(); got != tc.revocable {
				t.Fatalf("Revocable() = %v, want %v", got, tc.revocable)
			}
		})
	}
}

// TestClient_VKWrites covers revoke (no body) and regenerate (returns the new
// plaintext secret once) against their exact endpoints.
func TestClient_VKWrites(t *testing.T) {
	var method, path string
	c, done := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		switch {
		case r.URL.Path == "/api/admin/virtual-keys/vk1/revoke":
			_, _ = io.WriteString(w, `{"message":"Virtual key revoked"}`)
		case r.URL.Path == "/api/admin/virtual-keys/vk1/regenerate":
			_, _ = io.WriteString(w, `{"id":"vk1","keyPrefix":"nvk_abcd","key":"nvk_brand_new_secret","message":"saved once"}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer done()

	if err := c.RevokeVK(context.Background(), "vk1"); err != nil {
		t.Fatalf("RevokeVK: %v", err)
	}
	if method != "POST" || path != "/api/admin/virtual-keys/vk1/revoke" {
		t.Fatalf("revoke wrong: %s %s", method, path)
	}

	got, err := c.RegenerateVK(context.Background(), "vk1")
	if err != nil {
		t.Fatalf("RegenerateVK: %v", err)
	}
	if method != "POST" || path != "/api/admin/virtual-keys/vk1/regenerate" {
		t.Fatalf("regenerate wrong: %s %s", method, path)
	}
	if got.Key != "nvk_brand_new_secret" || got.KeyPrefix != "nvk_abcd" {
		t.Fatalf("regenerate decoded wrong: %+v", got)
	}
}

// TestClient_RegenerateVKNoSecret guards the "server returned no plaintext"
// failure: a regenerate that omits the key is an error, not a silent empty key
// the operator might think is real.
func TestClient_RegenerateVKNoSecret(t *testing.T) {
	c, done := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"vk1","keyPrefix":"nvk_abcd"}`)
	}))
	defer done()
	if _, err := c.RegenerateVK(context.Background(), "vk1"); err == nil {
		t.Fatal("RegenerateVK should error when no plaintext key is returned")
	}
}

// TestClient_RoutingRules covers the list shape and the enable/disable toggle
// (PUT with the partial {enabled} body).
func TestClient_RoutingRules(t *testing.T) {
	var method, path string
	var body map[string]any
	c, done := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		switch r.URL.Path {
		case "/api/admin/routing-rules":
			_, _ = io.WriteString(w, `{"data":[{"id":"r1","name":"Cheap default","strategyType":"smart","priority":10,"pipelineStage":1,"enabled":true}],"total":1}`)
		case "/api/admin/routing-rules/r1":
			body = nil
			_ = json.NewDecoder(r.Body).Decode(&body)
			_, _ = io.WriteString(w, `{"id":"r1","enabled":false}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer done()

	rules, err := c.RoutingRules(context.Background())
	if err != nil || len(rules) != 1 {
		t.Fatalf("RoutingRules: %+v err=%v", rules, err)
	}
	if rules[0].Name != "Cheap default" || rules[0].StrategyType != "smart" || rules[0].Priority != 10 || !rules[0].Enabled {
		t.Fatalf("routing rule decoded wrong: %+v", rules[0])
	}

	if err := c.SetRoutingRuleEnabled(context.Background(), "r1", false); err != nil {
		t.Fatalf("SetRoutingRuleEnabled: %v", err)
	}
	if method != "PUT" || path != "/api/admin/routing-rules/r1" || body["enabled"] != false {
		t.Fatalf("routing toggle wrong: %s %s body=%v", method, path, body)
	}
}

// TestClient_Wave3WriteErrors asserts every Wave 3 write surfaces a non-2xx as
// an error rather than swallowing it.
func TestClient_Wave3WriteErrors(t *testing.T) {
	c, done := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"boom"}}`)
	}))
	defer done()
	if err := c.RevokeVK(context.Background(), "vk1"); err == nil {
		t.Error("RevokeVK should error on 500")
	}
	if _, err := c.RegenerateVK(context.Background(), "vk1"); err == nil {
		t.Error("RegenerateVK should error on 500")
	}
	if _, err := c.RoutingRules(context.Background()); err == nil {
		t.Error("RoutingRules should error on 500")
	}
	if err := c.SetRoutingRuleEnabled(context.Background(), "r1", true); err == nil {
		t.Error("SetRoutingRuleEnabled should error on 500")
	}
}
