package core

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

func TestClient_DLQ(t *testing.T) {
	c, done := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/admin/observability/dlq" {
			t.Errorf("path %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"rows":[{"id":"d1"},{"id":"d2"}]}`)
	}))
	defer done()
	dlq, err := c.DLQ(context.Background())
	if err != nil || len(dlq.Rows) != 2 {
		t.Fatalf("DLQ wrong: %+v err=%v", dlq, err)
	}
}

func TestClient_Nodes(t *testing.T) {
	c, done := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/admin/nodes" {
			t.Errorf("path %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"nodes":[{"id":"n1","name":"ai-gw-1","type":"ai-gateway","status":"online","version":"1.2.3","targetVersion":5,"appliedVersion":5,"last_seen_at":"2026-05-28T00:00:00Z"},{"id":"n2","name":"agent-9","type":"agent","status":"online","targetVersion":7,"appliedVersion":6}],"total":2}`)
	}))
	defer done()
	res, err := c.Nodes(context.Background())
	if err != nil || res.Total != 2 || res.Nodes[0].Name != "ai-gw-1" {
		t.Fatalf("Nodes wrong: %+v err=%v", res, err)
	}
	if res.Nodes[0].Drifted() {
		t.Fatal("n1 (target==applied) should not be drifted")
	}
	if !res.Nodes[1].Drifted() {
		t.Fatal("n2 (target!=applied) should be drifted")
	}
}

func TestClient_Alerts(t *testing.T) {
	c, done := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/admin/alerts" {
			t.Errorf("path %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"alerts":[{"id":"a1","targetLabel":"OpenAI","severity":"critical","state":"firing","message":"error spike","firedAt":"2026-05-28T00:00:00Z","duplicateCount":3,"resolvedAt":""},{"id":"a2","state":"resolved","resolvedAt":"2026-05-28T01:00:00Z"}],"total":2}`)
	}))
	defer done()
	res, err := c.Alerts(context.Background())
	if err != nil || len(res.Alerts) != 2 {
		t.Fatalf("Alerts wrong: %+v err=%v", res, err)
	}
	if !res.Alerts[0].Firing() {
		t.Fatal("a1 (resolvedAt empty, state firing) should be firing")
	}
	if res.Alerts[1].Firing() {
		t.Fatal("a2 (resolved) should not be firing")
	}
}

func TestClient_RoutingSimulate(t *testing.T) {
	c, done := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/admin/routing-rules/simulate" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		var body RoutingSimulateRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.ModelID != "gpt-4o-mini" || body.EndpointType != "chat" {
			t.Errorf("request not forwarded: %+v", body)
		}
		_, _ = io.WriteString(w, `{"substituted":true,"ruleName":"prefer-anthropic","targets":[{"providerName":"Anthropic","modelCode":"claude-sonnet-4-6","providerModelId":"claude-sonnet-4-6"}],"recoveryTargets":[],"warnings":["no stage-1 rule matched"]}`)
	}))
	defer done()
	res, err := c.RoutingSimulate(context.Background(), RoutingSimulateRequest{ModelID: "gpt-4o-mini", EndpointType: "chat"})
	if err != nil || !res.Substituted || res.RuleName != "prefer-anthropic" {
		t.Fatalf("RoutingSimulate wrong: %+v err=%v", res, err)
	}
	if len(res.Targets) != 1 || res.Targets[0].ProviderName != "Anthropic" {
		t.Fatalf("targets wrong: %+v", res.Targets)
	}
	if len(res.Warnings) != 1 {
		t.Fatalf("warnings wrong: %+v", res.Warnings)
	}
}

func TestClient_CreateVK(t *testing.T) {
	c, done := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/admin/virtual-keys" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["name"] != "nexus-cli" || body["vkType"] != "personal" {
			t.Errorf("create body wrong: %+v", body)
		}
		_, _ = io.WriteString(w, `{"id":"vk-new","name":"nexus-cli","keyPrefix":"nvk_abc","key":"nvk_plaintext_once"}`)
	}))
	defer done()
	vk, err := c.CreateVK(context.Background(), "nexus-cli")
	if err != nil || vk.ID != "vk-new" || vk.Key != "nvk_plaintext_once" {
		t.Fatalf("CreateVK wrong: %+v err=%v", vk, err)
	}
}

func TestClient_CreateVK_NoPlaintext(t *testing.T) {
	c, done := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"vk-new","name":"x"}`) // no "key"
	}))
	defer done()
	if _, err := c.CreateVK(context.Background(), "x"); err == nil {
		t.Fatal("CreateVK should error when the server returns no plaintext key")
	}
}

func TestClient_Wave1ErrorPaths(t *testing.T) {
	c, done := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"boom"}}`)
	}))
	defer done()
	if _, err := c.DLQ(context.Background()); err == nil {
		t.Error("DLQ should error on 500")
	}
	if _, err := c.Nodes(context.Background()); err == nil {
		t.Error("Nodes should error on 500")
	}
	if _, err := c.Alerts(context.Background()); err == nil {
		t.Error("Alerts should error on 500")
	}
	if _, err := c.RoutingSimulate(context.Background(), RoutingSimulateRequest{ModelID: "m"}); err == nil {
		t.Error("RoutingSimulate should error on 500")
	}
}

// TestTrafficEvent_BodyHookFields confirms the single-event decode picks up the
// body + hook-decision fields the drill-down renders.
func TestTrafficEvent_BodyHookFields(t *testing.T) {
	c, done := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"ev1","statusCode":403,"requestBody":{"model":"gpt-4o-mini"},"responseBody":{"error":"blocked"},"requestHookDecision":"block","requestHookReason":"pii detected","responseHookDecision":"allow"}`)
	}))
	defer done()
	ev, err := c.TrafficEvent(context.Background(), "ev1")
	if err != nil || ev.RequestHookDecision != "block" || ev.RequestHookReason != "pii detected" {
		t.Fatalf("hook fields wrong: %+v err=%v", ev, err)
	}
	if len(ev.RequestBody) == 0 || len(ev.ResponseBody) == 0 {
		t.Fatalf("bodies not decoded: req=%s resp=%s", ev.RequestBody, ev.ResponseBody)
	}
}
