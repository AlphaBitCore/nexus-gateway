//go:build live

// Live CLI integration check against a running local stack. Run with:
//
//	go test -tags live -run TestLive -v ./internal/cli/...
//
// Logs in headlessly into an in-memory store (no real keychain, no browser) and
// drives the real commands end to end.
package cli

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/local"
)

func TestLive_CLICommands(t *testing.T) {
	env := core.Env{
		Name:             "local",
		CPBaseURL:        "http://localhost:3001",
		AIGatewayBaseURL: "http://localhost:3050",
		OAuthClientID:    "cp-ui",
		OAuthRedirectURI: "http://localhost:3000/auth/callback",
	}
	hc := &http.Client{Timeout: 15 * time.Second}
	store := fakeStore{m: map[string]string{}}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := core.NewAuthenticator(env, store, hc).LoginHeadless(ctx, "admin@nexus.ai", "admin123"); err != nil {
		t.Fatalf("headless login: %v", err)
	}

	newApp := func() *App {
		return &App{
			Cfg:   &local.Config{DefaultEnv: "local", Envs: map[string]core.Env{"local": env}},
			Env:   env,
			Store: store,
			HTTP:  hc,
		}
	}

	for _, args := range [][]string{
		{"health", "-o", "json"},
		{"models", "ls"},
		{"traffic", "ls", "--limit", "2", "-o", "json"},
		{"cost", "--group", "provider"},
	} {
		out, err := runCLI(t, newApp(), args...)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		t.Logf("$ nexus %v\n%s", args, truncate(out, 400))
	}
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
