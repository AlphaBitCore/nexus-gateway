package core

import (
	"context"
	"errors"
	"github.com/goccy/go-json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func testClient(t *testing.T, h http.Handler) (*Client, func()) {
	t.Helper()
	srv := httptest.NewServer(h)
	c := NewClient(Env{Name: "local", CPBaseURL: srv.URL},
		fixedTokenSource{header: "Authorization", value: "Bearer T"}, srv.Client())
	return c, srv.Close
}

func TestClient_Sparkline(t *testing.T) {
	c, done := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/admin/analytics/sparkline" {
			t.Errorf("path %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"granularity":"hour","summary":{"requestCount":42},"series":[{"bucketStart":"2026-05-28T00:00:00Z","values":{"requestCount":42}}]}`)
	}))
	defer done()
	got, err := c.Sparkline(context.Background(), url.Values{"window": {"24h"}})
	if err != nil {
		t.Fatalf("Sparkline: %v", err)
	}
	if got.Summary["requestCount"] != 42 || len(got.Series) != 1 {
		t.Fatalf("decoded wrong: %+v", got)
	}
}

func TestClient_TrafficListAndEvent(t *testing.T) {
	c, done := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/admin/traffic":
			if r.URL.Query().Get("statusRange") != "5xx" {
				t.Errorf("filter not propagated: %s", r.URL.RawQuery)
			}
			_, _ = io.WriteString(w, `{"data":[{"id":"ev1","statusCode":500,"modelName":"gpt-4"}],"total":1,"limit":50,"offset":0}`)
		case "/api/admin/traffic/ev1":
			_, _ = io.WriteString(w, `{"id":"ev1","statusCode":500,"traceId":"trace-9","totalTokens":1234}`)
		case "/api/admin/traffic/ev1/normalized":
			_, _ = io.WriteString(w, `{"kind":"ai-chat"}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer done()

	list, err := c.TrafficList(context.Background(), TrafficFilter{StatusRange: "5xx", Limit: 50})
	if err != nil || list.Total != 1 || list.Data[0].ID != "ev1" || list.Data[0].ModelName != "gpt-4" {
		t.Fatalf("TrafficList wrong: %+v err=%v", list, err)
	}
	ev, err := c.TrafficEvent(context.Background(), "ev1")
	if err != nil || ev.TraceID != "trace-9" || ev.TotalTokens != 1234 {
		t.Fatalf("TrafficEvent wrong: %+v err=%v", ev, err)
	}
	norm, err := c.TrafficEventNormalized(context.Background(), "ev1")
	if err != nil || string(norm) != `{"kind":"ai-chat"}` {
		t.Fatalf("normalized wrong: %s err=%v", norm, err)
	}
}

func TestClient_InstancesAndVKs(t *testing.T) {
	c, done := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/admin/instances":
			_, _ = io.WriteString(w, `{"count":5,"services":{"ai-gateway":{"total":2}}}`)
		case "/api/admin/virtual-keys":
			_, _ = io.WriteString(w, `{"data":[{"id":"vk1","name":"research","keyPrefix":"nvk_abc","sourceApp":"cli","enabled":true,"ownerId":"u1"}]}`)
		}
	}))
	defer done()
	inst, err := c.Instances(context.Background())
	if err != nil || inst.Count != 5 || inst.Services["ai-gateway"].Total != 2 {
		t.Fatalf("Instances wrong: %+v err=%v", inst, err)
	}
	vks, err := c.VirtualKeys(context.Background())
	if err != nil || len(vks) != 1 || vks[0].Name != "research" || !vks[0].Enabled || vks[0].KeyPrefix != "nvk_abc" {
		t.Fatalf("VirtualKeys wrong: %+v err=%v", vks, err)
	}
}

func TestClient_SetKillSwitch_SendsBody(t *testing.T) {
	c, done := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method %s", r.Method)
		}
		var body map[string]bool
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["engaged"] != true {
			t.Errorf("body engaged=%v, want true", body["engaged"])
		}
		_, _ = io.WriteString(w, `{"engaged":true,"version":7,"thingsNotified":3,"thingsOnline":4}`)
	}))
	defer done()
	res, err := c.SetKillSwitch(context.Background(), true)
	if err != nil || !res.Engaged || res.Version != 7 || res.ThingsNotified != 3 {
		t.Fatalf("SetKillSwitch wrong: %+v err=%v", res, err)
	}
}

func TestClient_CredentialAttached(t *testing.T) {
	var sawHeader, sawValue string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-admin-key") != "" {
			sawHeader, sawValue = "x-admin-key", r.Header.Get("x-admin-key")
		} else {
			sawHeader, sawValue = "Authorization", r.Header.Get("Authorization")
		}
		_, _ = io.WriteString(w, `{"count":0,"services":{}}`)
	}))
	defer srv.Close()

	// admin-key surface
	c := NewClient(Env{Name: "local", CPBaseURL: srv.URL}, fixedTokenSource{header: "x-admin-key", value: "nxk_k"}, srv.Client())
	if _, err := c.Instances(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sawHeader != "x-admin-key" || sawValue != "nxk_k" {
		t.Fatalf("admin-key not attached: %s=%s", sawHeader, sawValue)
	}

	// bearer surface
	c2 := NewClient(Env{Name: "local", CPBaseURL: srv.URL}, fixedTokenSource{header: "Authorization", value: "Bearer J"}, srv.Client())
	if _, err := c2.Instances(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sawHeader != "Authorization" || sawValue != "Bearer J" {
		t.Fatalf("bearer not attached: %s=%s", sawHeader, sawValue)
	}
}

func TestClient_ErrorMapping(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   error
		action string
	}{
		{401, `{"error":{"message":"no token","type":"authentication_error","code":"AUTH_REQUIRED"}}`, ErrUnauthorized, ""},
		{403, `{"error":{"message":"denied","code":"FORBIDDEN","action":"admin:traffic-log.read"}}`, ErrForbidden, "admin:traffic-log.read"},
		{404, `{"error":{"message":"missing","code":"NOT_FOUND"}}`, ErrNotFound, ""},
		{500, `internal boom`, ErrTransport, ""}, // non-envelope body
	}
	for _, tc := range cases {
		c, done := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = io.WriteString(w, tc.body)
		}))
		_, err := c.Instances(context.Background())
		if !errors.Is(err, tc.want) {
			t.Errorf("status %d: errors.Is(%v, want) = false", tc.status, err)
		}
		var apiErr *APIError
		if errors.As(err, &apiErr) {
			if apiErr.Status != tc.status {
				t.Errorf("status %d: APIError.Status = %d", tc.status, apiErr.Status)
			}
			if apiErr.IAMAction != tc.action {
				t.Errorf("status %d: IAMAction = %q, want %q", tc.status, apiErr.IAMAction, tc.action)
			}
			if apiErr.Error() == "" {
				t.Errorf("status %d: empty Error() string", tc.status)
			}
		} else {
			t.Errorf("status %d: not an *APIError: %v", tc.status, err)
		}
		done()
	}
}

func TestClient_CredentialErrorShortCircuits(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
	defer srv.Close()
	c := NewClient(Env{Name: "local", CPBaseURL: srv.URL},
		fixedTokenSource{err: &APIError{kind: ErrUnauthorized, Message: "login required"}}, srv.Client())
	_, err := c.Instances(context.Background())
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("want ErrUnauthorized, got %v", err)
	}
	if hit {
		t.Fatal("server should not be called when credential fails")
	}
}

func TestClient_DecodeError(t *testing.T) {
	c, done := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `not json`)
	}))
	defer done()
	_, err := c.Instances(context.Background())
	if !errors.Is(err, ErrTransport) {
		t.Fatalf("want ErrTransport on bad JSON, got %v", err)
	}
}

func TestClient_TransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	c := NewClient(Env{Name: "local", CPBaseURL: srv.URL},
		fixedTokenSource{header: "Authorization", value: "Bearer T"}, srv.Client())
	srv.Close() // force a dial failure
	_, err := c.Instances(context.Background())
	if !errors.Is(err, ErrTransport) {
		t.Fatalf("want ErrTransport on dial failure, got %v", err)
	}
}

func TestTrafficFilter_Values(t *testing.T) {
	exclude := true
	f := TrafficFilter{
		StatusRange: "4xx", Provider: "openai", ModelUsed: "gpt-4", VirtualKeyID: "vk1",
		Source: "ai-gateway", StartTime: time.Date(2026, 5, 28, 0, 0, 0, 0, time.UTC),
		EndTime: time.Date(2026, 5, 28, 1, 0, 0, 0, time.UTC), Limit: 25, Offset: 50,
		ExcludeInternal: &exclude,
	}
	q := f.values()
	want := map[string]string{
		"statusRange": "4xx", "provider": "openai", "modelUsed": "gpt-4",
		"virtualKeyId": "vk1", "source": "ai-gateway", "limit": "25", "offset": "50",
		"excludeInternal": "true", "startTime": "2026-05-28T00:00:00Z", "endTime": "2026-05-28T01:00:00Z",
	}
	for k, v := range want {
		if q.Get(k) != v {
			t.Errorf("filter %s = %q, want %q", k, q.Get(k), v)
		}
	}
	// Zero-valued filter emits nothing.
	if len(TrafficFilter{}.values()) != 0 {
		t.Errorf("empty filter should produce no query params")
	}
}

func TestClient_Env(t *testing.T) {
	c := NewClient(Env{Name: "prod", IsProd: true}, fixedTokenSource{}, nil)
	if c.Env().Name != "prod" || !c.Env().IsProd {
		t.Fatal("Env() accessor wrong")
	}
}

func TestClient_AdminModels(t *testing.T) {
	c, done := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/admin/models" {
			t.Errorf("path %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"data":[{"provider":{"id":"p1","name":"OpenAI"},"models":[{"id":"m1","code":"gpt-4","name":"GPT-4","type":"chat","enabled":true,"maxContextTokens":128000,"inputPricePerMillion":2.5}]}]}`)
	}))
	defer done()
	cat, err := c.AdminModels(context.Background())
	if err != nil || len(cat.Data) != 1 || cat.Data[0].Provider.Name != "OpenAI" {
		t.Fatalf("AdminModels group wrong: %+v err=%v", cat, err)
	}
	m := cat.Data[0].Models[0]
	if m.Code != "gpt-4" || !m.Enabled || m.MaxContextTokens != 128000 || m.InputPricePerMillion != 2.5 {
		t.Fatalf("AdminModels model wrong: %+v", m)
	}
}

func TestClient_Cost(t *testing.T) {
	c, done := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/admin/analytics/cost" || r.URL.Query().Get("groupBy") != "provider" {
			t.Errorf("path/query: %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		_, _ = io.WriteString(w, `{"data":[{"group":"p1","groupLabel":"OpenAI","requestCount":111,"totalTokens":297738,"totalCostUsd":1.3373,"cacheHitCount":7}],"total":5}`)
	}))
	defer done()
	rep, err := c.Cost(context.Background(), url.Values{"groupBy": {"provider"}})
	if err != nil || rep.Total != 5 || len(rep.Data) != 1 {
		t.Fatalf("Cost wrong: %+v err=%v", rep, err)
	}
	if rep.Data[0].GroupLabel != "OpenAI" || rep.Data[0].TotalCostUSD != 1.3373 || rep.Data[0].CacheHitCount != 7 {
		t.Fatalf("Cost row wrong: %+v", rep.Data[0])
	}
}
