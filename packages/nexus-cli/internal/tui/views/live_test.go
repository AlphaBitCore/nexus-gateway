//go:build live

package views

import (
	"context"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
	"net/http"
	"strings"
	"testing"
	"time"
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

func (s liveStore) Delete(env, key string) error { delete(s.m, env+":"+key); return nil }

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

	cp := newCockpit(gw)
	cpv, _ := cp.Update(cp.fetch()())
	cockpitOut := cpv.View(120, 30)
	t.Logf("COCKPIT:\n%s", cockpitOut)
	if !strings.Contains(cockpitOut, "Providers") {
		t.Fatalf("cockpit did not render the provider leaderboard")
	}

	r := newRadar(gw)
	rv, _ := r.Update(r.Init()())
	radarOut := rv.View(120, 30)
	t.Logf("RADAR:\n%s", radarOut)
	if !strings.Contains(radarOut, "Live traffic") {
		t.Fatalf("Radar did not render")
	}
}
