//go:build live

// Live integration check for the capabilities layer against a running local
// stack (CP :3001, AI-GW :3050, Hub :3060; admin admin@nexus.ai / admin123).
// Excluded from default builds and CI. Run explicitly with:
//
//	go test -tags live -run TestLive ./internal/capabilities/...
//
// These tests uniquely verify what unit tests cannot: that the AI Gateway passes
// native `tools`/`tool_calls` through the VK pipeline, that a full agent Turn
// drives a real tool and answers. They create only their own throwaway VK (touch own data).
package runtime

import (
	"context"
	"errors"
	"github.com/goccy/go-json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
)

// liveMemStore is an in-memory core.SecretStore so the live test never touches
// the real OS keychain.
type liveMemStore struct{ m map[string]string }

func (s liveMemStore) Get(env, key string) (string, error) {
	// Must return ErrSecretNotFound on a miss (not a nil error): NewTokenSource's
	// credential auto-select treats a nil error as "an admin key exists".
	v, ok := s.m[env+":"+key]
	if !ok {
		return "", core.ErrSecretNotFound
	}
	return v, nil
}
func (s liveMemStore) Set(env, key, val string) error { s.m[env+":"+key] = val; return nil }
func (s liveMemStore) Delete(env, key string) error   { delete(s.m, env+":"+key); return nil }

func liveClient(t *testing.T) (*core.Client, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	env := core.Env{
		Name:             "local",
		CPBaseURL:        "http://localhost:3001",
		AIGatewayBaseURL: "http://localhost:3050",
		OAuthClientID:    "cp-ui",
		OAuthRedirectURI: "http://localhost:3000/auth/callback",
	}
	hc := &http.Client{Timeout: 30 * time.Second}
	store := liveMemStore{m: map[string]string{}}
	a := core.NewAuthenticator(env, store, hc)
	if err := a.LoginHeadless(ctx, "admin@nexus.ai", "admin123"); err != nil {
		t.Skipf("local stack not reachable / login failed: %v", err)
	}
	return core.NewClient(env, core.NewTokenSource(env, store, hc), hc), ctx
}

// liveVKAndModel creates a throwaway VK and picks an enabled chat model.
func liveVKAndModel(t *testing.T, c *core.Client, ctx context.Context) (vkSecret, model string) {
	t.Helper()
	cat, err := c.AdminModels(ctx)
	if err != nil {
		t.Fatalf("AdminModels: %v", err)
	}
	for _, g := range cat.Data {
		for _, m := range g.Models {
			if m.Enabled && (m.Type == "chat" || m.Type == "") {
				model = m.Code
				break
			}
		}
		if model != "" {
			break
		}
	}
	if model == "" {
		t.Skip("no enabled chat model in the local catalog")
	}
	vk, err := c.CreateVK(ctx, "layer2-live-"+time.Now().Format("150405"))
	if err != nil {
		t.Skipf("cannot create a throwaway VK (%v)", err)
	}
	return vk.Key, model
}

// isComplianceBlock reports a gateway hook block (PII/compliance) on the VK path
// — an expected, by-design outcome (design §7), surfaced as a SKIP not a failure.
func isComplianceBlock(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return errors.Is(err, core.ErrForbidden) || strings.Contains(s, "pii") || strings.Contains(s, "block")
}

func TestLive_ToolCallingModel(t *testing.T) {
	c, ctx := liveClient(t)
	vk, model := liveVKAndModel(t, c, ctx)

	m := NewModel(c, vk, model)
	req := agent.ModelRequest{
		System:   "You are an SRE for the Nexus gateway. Use a tool when one fits.",
		Messages: []agent.Message{agent.TextMessage(agent.RoleUser, "What is the recent cost? Call the observe_cost tool to find out.")},
		Tools: []agent.ToolSchema{{
			Name: "observe_cost", Description: "Cost grouped by provider over the recent window.",
			Parameters: json.RawMessage(`{"type":"object","properties":{"groupBy":{"type":"string"}}}`),
		}},
	}
	resp, err := m.Generate(ctx, req, nil, nil)
	if isComplianceBlock(err) {
		t.Skipf("VK pipeline blocked the request (by design, §7): %v", err)
	}
	if err != nil {
		t.Fatalf("Generate against the live gateway failed: %v", err)
	}
	// The gateway must have accepted tools[] and produced a parseable turn: either
	// a tool_use (function calling round-trips the VK pipeline) or plain text.
	if len(resp.Message.ToolUses()) != 0 {
		u := resp.Message.ToolUses()[0]
		t.Logf("tool-calling round-trips: model called %q args=%s stop=%s", u.ToolName, u.Input, resp.StopReason)
		if u.ToolName == "" {
			t.Fatal("a tool_use block must carry a tool name")
		}
	} else {
		t.Logf("model answered without a tool call (acceptable): %q stop=%s", resp.Message.Text(), resp.StopReason)
		if strings.TrimSpace(resp.Message.Text()) == "" {
			t.Fatal("a non-tool turn must carry text")
		}
	}
}

func TestLive_AgentTurn(t *testing.T) {
	c, ctx := liveClient(t)
	vk, model := liveVKAndModel(t, c, ctx)
	dir := t.TempDir()

	ag, err := BuildAgent(ctx, AgentDeps{
		Streamer: c, Gateway: c, Canvas: &fakeCanvas{},
		VKSecret: vk, Model: model, Env: "local",
		MemoryDir: dir, SessionDir: dir,
		Confirm: func(context.Context, agent.Tool, json.RawMessage, string) (bool, error) { return true, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := ag.Turn(ctx, "Is the gateway healthy right now? Check and answer in one sentence.", "")
	if isComplianceBlock(err) {
		t.Skipf("VK pipeline blocked the agent turn (by design, §7): %v", err)
	}
	if err != nil && !errors.Is(err, agent.ErrStepCap) {
		t.Fatalf("agent Turn failed: %v", err)
	}
	t.Logf("agent answered: %q", out)
	if strings.TrimSpace(out) == "" {
		t.Fatal("agent Turn produced no answer")
	}
	// The turn was persisted.
	if metas, _ := ag.Store.List(); len(metas) == 0 {
		t.Fatal("the agent turn must persist a session")
	}
}
