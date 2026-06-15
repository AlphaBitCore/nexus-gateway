package core

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

func TestClient_ProviderAnalyticsMethods(t *testing.T) {
	c, done := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/admin/analytics/by-provider":
			_, _ = io.WriteString(w, `{"data":[{"provider":"p1","providerLabel":"OpenAI","requestCount":121,"avgLatencyMs":14634.2,"totalTokens":297906,"totalEstimatedCostUsd":1.337}]}`)
		case "/api/admin/compliance/overview":
			_, _ = io.WriteString(w, `{"kpis":{"totalRequests":586,"totalBlocked":7,"overallBlockRate":0.0119,"tlsCoveragePercent":0,"hookErrorRate":0}}`)
		case "/api/admin/jobs":
			_, _ = io.WriteString(w, `{"jobs":[{"id":"j1","name":"Cert Alerts","interval":3600000000000,"enabled":true,"lastRun":"2026-05-28T11:30:20Z"}]}`)
		case "/api/admin/config-sync/out-of-sync":
			_, _ = io.WriteString(w, `{"outOfSync":[{"id":"n1"}],"total":1}`)
		case "/api/admin/analytics/provider/p1":
			_, _ = io.WriteString(w, `{"summary":{"totalRequests":121,"errorCount":3,"errorRate":0.024,"cacheHitRate":0.1,"avgLatencyMs":14634.2,"avgUpstreamTtfbMs":1200,"totalEstimatedCostUsd":1.337}}`)
		case "/api/admin/providers":
			_, _ = io.WriteString(w, `{"data":[{"id":"6b6d307f","name":"openai","displayName":"OpenAI"}],"total":1}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer done()

	bp, err := c.ByProvider(context.Background(), nil)
	if err != nil || len(bp.Data) != 1 || bp.Data[0].ProviderLabel != "OpenAI" || bp.Data[0].RequestCount != 121 {
		t.Fatalf("ByProvider wrong: %+v err=%v", bp, err)
	}
	co, err := c.ComplianceOverview(context.Background(), nil)
	if err != nil || co.KPIs.TotalRequests != 586 || co.KPIs.TotalBlocked != 7 {
		t.Fatalf("ComplianceOverview wrong: %+v err=%v", co, err)
	}
	jobs, err := c.Jobs(context.Background())
	if err != nil || len(jobs.Jobs) != 1 || !jobs.Jobs[0].Enabled || jobs.Jobs[0].Name != "Cert Alerts" {
		t.Fatalf("Jobs wrong: %+v err=%v", jobs, err)
	}
	cs, err := c.ConfigSyncOutOfSync(context.Background())
	if err != nil || cs.Total != 1 || len(cs.OutOfSync) != 1 {
		t.Fatalf("ConfigSync wrong: %+v err=%v", cs, err)
	}
	pd, err := c.ProviderDetail(context.Background(), "p1", nil)
	if err != nil || pd.Summary.ErrorCount != 3 || pd.Summary.TotalRequests != 121 {
		t.Fatalf("ProviderDetail wrong: %+v err=%v", pd, err)
	}
	pr, err := c.Providers(context.Background())
	if err != nil || len(pr.Data) != 1 || pr.Data[0].Name != "openai" || pr.Data[0].DisplayName != "OpenAI" || pr.Data[0].ID != "6b6d307f" {
		t.Fatalf("Providers wrong: %+v err=%v", pr, err)
	}
}

func TestClient_MitigationWrites(t *testing.T) {
	var method, path string
	var body map[string]any
	c, done := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		body = nil
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer done()

	if err := c.SetProviderEnabled(context.Background(), "p1", false); err != nil {
		t.Fatalf("SetProviderEnabled: %v", err)
	}
	if method != "PUT" || path != "/api/admin/providers/p1" || body["enabled"] != false {
		t.Fatalf("provider write wrong: %s %s body=%v", method, path, body)
	}
	if err := c.CacheFlush(context.Background()); err != nil {
		t.Fatalf("CacheFlush: %v", err)
	}
	if method != "POST" || path != "/api/admin/cache/flush" {
		t.Fatalf("cache flush wrong: %s %s", method, path)
	}
}

func TestClient_MitigationWriteErrors(t *testing.T) {
	c, done := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"boom"}}`)
	}))
	defer done()
	if err := c.SetProviderEnabled(context.Background(), "p1", true); err == nil {
		t.Error("SetProviderEnabled should error on 500")
	}
	if err := c.CacheFlush(context.Background()); err == nil {
		t.Error("CacheFlush should error on 500")
	}
}

func TestClient_ProviderAnalyticsErrorPaths(t *testing.T) {
	c, done := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"boom"}}`)
	}))
	defer done()
	if _, err := c.ByProvider(context.Background(), nil); err == nil {
		t.Error("ByProvider should error on 500")
	}
	if _, err := c.ComplianceOverview(context.Background(), nil); err == nil {
		t.Error("ComplianceOverview should error on 500")
	}
	if _, err := c.Jobs(context.Background()); err == nil {
		t.Error("Jobs should error on 500")
	}
	if _, err := c.ConfigSyncOutOfSync(context.Background()); err == nil {
		t.Error("ConfigSyncOutOfSync should error on 500")
	}
	if _, err := c.ProviderDetail(context.Background(), "p1", nil); err == nil {
		t.Error("ProviderDetail should error on 500")
	}
	if _, err := c.Providers(context.Background()); err == nil {
		t.Error("Providers should error on 500")
	}
}
