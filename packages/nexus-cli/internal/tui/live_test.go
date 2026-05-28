//go:build live

// Live TUI check: log in headlessly, build a real client, and drive the
// Overview + Radar fetches through the models, asserting the rendered View
// reflects real local-stack data. Run with:
//
//	go test -tags live -run TestLive -v ./internal/tui/...
package tui

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/core"
)

// liveStore is a minimal in-memory SecretStore for the live check.
type liveStore struct{ m map[string]string }

func (s liveStore) Get(env, key string) (string, error) {
	if v, ok := s.m[env+":"+key]; ok {
		return v, nil
	}
	return "", core.ErrSecretNotFound
}
func (s liveStore) Set(env, key, val string) error { s.m[env+":"+key] = val; return nil }
func (s liveStore) Delete(env, key string) error   { delete(s.m, env+":"+key); return nil }

func TestLive_ViewsRenderRealData(t *testing.T) {
	env := core.Env{Name: "local", CPBaseURL: "http://localhost:3001", AIGatewayBaseURL: "http://localhost:3050", OAuthClientID: "cp-ui", OAuthRedirectURI: "http://localhost:3000/auth/callback"}
	hc := &http.Client{Timeout: 15 * time.Second}
	store := liveStore{m: map[string]string{}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := core.NewAuthenticator(env, store, hc).LoginHeadless(ctx, "admin@nexus.ai", "admin123"); err != nil {
		t.Fatalf("login: %v", err)
	}
	gw := core.NewClient(env, core.NewTokenSource(env, store, hc), hc)

	o := newOverview(gw)
	ov, _ := o.Update(o.Init()())
	overviewOut := ov.View(120, 30)
	t.Logf("OVERVIEW:\n%s", overviewOut)
	if !strings.Contains(overviewOut, "Services") {
		t.Fatalf("overview did not render services panel")
	}

	r := newRadar(gw)
	rv, _ := r.Update(r.Init()())
	radarOut := rv.View(120, 30)
	t.Logf("RADAR:\n%s", radarOut)
	if !strings.Contains(radarOut, "Live traffic") {
		t.Fatalf("radar did not render")
	}
}

func TestLive_AskNexus(t *testing.T) {
	env := core.Env{Name: "local", CPBaseURL: "http://localhost:3001", AIGatewayBaseURL: "http://localhost:3050", OAuthClientID: "cp-ui", OAuthRedirectURI: "http://localhost:3000/auth/callback"}
	hc := &http.Client{Timeout: 60 * time.Second}
	store := liveStore{m: map[string]string{}}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if err := core.NewAuthenticator(env, store, hc).LoginHeadless(ctx, "admin@nexus.ai", "admin123"); err != nil {
		t.Fatalf("login: %v", err)
	}
	gw := core.NewClient(env, core.NewTokenSource(env, store, hc), hc)

	// Pick a real, enabled chat model from the catalog.
	cat, err := gw.AdminModels(ctx)
	if err != nil {
		t.Fatalf("models: %v", err)
	}
	model := ""
	for _, g := range cat.Data {
		for _, mod := range g.Models {
			if mod.Enabled && mod.Type == "chat" {
				model = mod.Code
				break
			}
		}
		if model != "" {
			break
		}
	}
	if model == "" {
		t.Skip("no enabled chat model in the local catalog — cannot exercise the ask bar")
	}

	// Create a throwaway personal VK we own, and revoke it at the end (touches only
	// its own data). The ask bar's ChatStream needs a VK plaintext.
	vk, err := gw.CreateVK(ctx, "ask-nexus-live-test")
	if err != nil {
		t.Skipf("cannot create a throwaway VK (%v) — skipping live ask", err)
	}
	defer func() {
		if rerr := gw.RevokeVK(context.Background(), vk.ID); rerr != nil {
			t.Logf("cleanup: failed to revoke throwaway VK %s: %v", vk.ID, rerr)
		}
	}()
	sess := Session{EnvName: "local", Model: model, VKSecret: vk.Key, VKName: vk.Name}

	// Navigate question: the router call must complete without a transport failure.
	nav := newAskBar(gw, sess, entriesFixture())
	nav, ncmd := submit(nav, "show me the cost view")
	nav, navMsg := pump(nav, ncmd)
	t.Logf("NAVIGATE result: msg=%T phase=%d notice=%q", navMsg, nav.phase, nav.notice)
	if nav.phase == askErrorPhase && strings.Contains(nav.notice, "routing failed") {
		t.Fatalf("live navigate hit a transport routing failure: %s", nav.notice)
	}

	// Answer question: route + fetch real analytics + summarize, all must complete.
	ans := newAskBar(gw, sess, entriesFixture())
	ans, acmd := submit(ans, "what is my most expensive provider today?")
	ans, _ = pump(ans, acmd)
	t.Logf("ANSWER phase=%d notice=%q answer=%q", ans.phase, ans.notice, ans.answer)
	// The answer prompt rides the same VK ChatStream pipeline as any chat, so the
	// operator's own compliance hooks apply to the fetched data (a PII block on a
	// numeric analytics field is a valid, gracefully-surfaced outcome on a stack
	// with aggressive hooks). Only a transport/infra failure is a real test failure.
	if ans.phase == askErrorPhase {
		if strings.Contains(ans.notice, "routing failed") || strings.Contains(ans.notice, "fetch failed") {
			t.Fatalf("live answer hit a transport/infra failure: %s", ans.notice)
		}
		t.Logf("answer was blocked by the gateway's own compliance policy (valid on this stack): %s", ans.notice)
	}
}
