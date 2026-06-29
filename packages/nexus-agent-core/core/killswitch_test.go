package core

import (
	"context"
	"errors"
	"github.com/goccy/go-json"
	"io"
	"net/http"
	"testing"
	"time"
)

func TestClient_KillSwitchStatus(t *testing.T) {
	t.Run("newest event drives engaged + metadata", func(t *testing.T) {
		c, done := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/admin/config-sync/history" {
				t.Fatalf("unexpected path %s", r.URL.Path)
			}
			if got := r.URL.Query().Get("configKey"); got != "killswitch" {
				t.Fatalf("configKey = %q, want killswitch", got)
			}
			_, _ = io.WriteString(w, `{"events":[{"newState":{"engaged":true},"newVersion":12,"createdAt":"2026-05-28T10:00:00Z","actorName":"admin"}],"total":3}`)
		}))
		defer done()
		st, err := c.KillSwitchStatus(context.Background())
		if err != nil || !st.Known || !st.Engaged || st.Version != 12 || st.By != "admin" {
			t.Fatalf("KillSwitchStatus wrong: %+v err=%v", st, err)
		}
	})
	t.Run("no events → not known (never toggled)", func(t *testing.T) {
		c, done := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"events":[],"total":0}`)
		}))
		defer done()
		st, err := c.KillSwitchStatus(context.Background())
		if err != nil || st.Known || st.Engaged {
			t.Fatalf("empty history should be unknown+off, got %+v err=%v", st, err)
		}
	})
}

func TestClient_PassthroughSnapshot(t *testing.T) {
	c, done := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/admin/passthrough/snapshot" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{
			"global":{"enabled":false,"bypassHooks":false,"bypassCache":false,"bypassNormalize":false},
			"adapters":{"anthropic":{"enabled":true,"bypassHooks":true}},
			"providers":{"prov-1":{"enabled":true,"bypassCache":true},"prov-2":{"enabled":false,"bypassHooks":true}},
			"providerNames":{"prov-1":"OpenAI","prov-2":"Anthropic"}
		}`)
	}))
	defer done()
	snap, err := c.PassthroughSnapshot(context.Background())
	if err != nil {
		t.Fatalf("PassthroughSnapshot err=%v", err)
	}
	if snap.Global.active() {
		t.Fatal("global tier should be inactive")
	}
	if snap.ProviderNames["prov-1"] != "OpenAI" {
		t.Fatalf("provider name not decoded: %+v", snap.ProviderNames)
	}
	adapters, providers := snap.ActiveOverrides()
	// anthropic adapter active (enabled+bypassHooks); prov-1 active (enabled+bypassCache);
	// prov-2 inactive (enabled=false).
	if adapters != 1 || providers != 1 {
		t.Fatalf("ActiveOverrides = (%d adapters, %d providers), want (1,1)", adapters, providers)
	}
}

func TestClient_KillSwitchPassthrough_Errors(t *testing.T) {
	c, done := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"boom"}}`)
	}))
	defer done()
	if _, err := c.KillSwitchStatus(context.Background()); err == nil {
		t.Fatal("KillSwitchStatus should surface a 500")
	}
	if _, err := c.PassthroughSnapshot(context.Background()); err == nil {
		t.Fatal("PassthroughSnapshot should surface a 500")
	}
	if err := c.SetPassthroughGlobal(context.Background(), PassthroughGlobalRequest{}); err == nil {
		t.Fatal("SetPassthroughGlobal should surface a 500")
	}
}

// serverValidatePassthrough mirrors the Control Plane's passthrough write
// validator (governance/passthrough/handler validate): an engage MUST carry at
// least one bypass flag, a future expiresAt ≤ 8h out, a reason ≥ 20 chars, and
// must not bypass normalize without cache. Keeping a copy here lets the client
// test assert it never sends a request the server would 400 — the gap that let a
// missing-expiresAt engage ship past an echo-only stub.
func serverValidatePassthrough(p PassthroughGlobalRequest) error {
	if !p.Enabled {
		return nil
	}
	if !p.BypassHooks && !p.BypassCache && !p.BypassNormalize {
		return errors.New("no bypass flag")
	}
	if p.BypassNormalize && !p.BypassCache {
		return errors.New("normalize requires cache")
	}
	if p.ExpiresAt == nil {
		return errors.New("expiresAt required")
	}
	if p.ExpiresAt.After(time.Now().Add(8 * time.Hour)) {
		return errors.New("expiresAt exceeds 8h")
	}
	if p.ExpiresAt.Before(time.Now()) {
		return errors.New("expiresAt in the past")
	}
	if len(p.Reason) < 20 {
		return errors.New("reason too short")
	}
	return nil
}

func TestClient_SetPassthroughGlobal_EngageSatisfiesServerValidator(t *testing.T) {
	cases := []struct {
		name string
		req  PassthroughGlobalRequest
	}{
		{"bare engage gets defaults", PassthroughGlobalRequest{Enabled: true, BypassHooks: true}},
		{"normalize forces cache", PassthroughGlobalRequest{Enabled: true, BypassNormalize: true}},
		{"caller reason preserved", PassthroughGlobalRequest{Enabled: true, BypassHooks: true, Reason: "provider X melting down during incident"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got PassthroughGlobalRequest
			c, done := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPut || r.URL.Path != "/api/admin/passthrough/global" {
					t.Fatalf("wrong request: %s %s", r.Method, r.URL.Path)
				}
				_ = json.NewDecoder(r.Body).Decode(&got)
				w.WriteHeader(http.StatusOK)
			}))
			defer done()
			if err := c.SetPassthroughGlobal(context.Background(), tc.req); err != nil {
				t.Fatalf("SetPassthroughGlobal err=%v", err)
			}
			if err := serverValidatePassthrough(got); err != nil {
				t.Fatalf("client sent a server-invalid engage (%v): %+v", err, got)
			}
		})
	}
}

func TestClient_SetPassthroughGlobal_DisengageIsBare(t *testing.T) {
	var got PassthroughGlobalRequest
	c, done := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusOK)
	}))
	defer done()
	if err := c.SetPassthroughGlobal(context.Background(), PassthroughGlobalRequest{Enabled: false}); err != nil {
		t.Fatalf("disengage err=%v", err)
	}
	// Disengage must not get an expiry/reason defaulted onto it.
	if got.Enabled || got.ExpiresAt != nil || got.Reason != "" {
		t.Fatalf("disengage should send a bare flag, got %+v", got)
	}
}
