//go:build live

package shell

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	capabilities "github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/capabilities/runtime"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
)

// liveStore is a minimal in-memory SecretStore for the live agent check.
type liveStore struct{ m map[string]string }

func (s liveStore) Get(env, key string) (string, error) {
	if v, ok := s.m[env+":"+key]; ok {
		return v, nil
	}
	return "", core.ErrSecretNotFound
}

func (s liveStore) Set(env, key, val string) error { s.m[env+":"+key] = val; return nil }

func (s liveStore) Delete(env, key string) error { delete(s.m, env+":"+key); return nil }

// TestLive_AgentCockpit drives the agent end-to-end over the real bridge — the
// exact production Canvas wiring — to prove the bridge↔agent↔model↔VK↔canvas
// pipeline works against the live stack. It asserts the turn completes without a
// transport/build error and that the bridge actually pumped agent activity
// (streamed text / a tool call / a canvas drive); it does NOT assert a specific
// navigate, which the model decides non-deterministically.
func TestLive_AgentCockpit(t *testing.T) {
	env := core.Env{Name: "local", CPBaseURL: "http://localhost:3001", AIGatewayBaseURL: "http://localhost:3050", OAuthClientID: "cp-ui", OAuthRedirectURI: "http://localhost:3000/auth/callback"}
	hc := &http.Client{Timeout: 90 * time.Second}
	store := liveStore{m: map[string]string{}}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	if err := core.NewAuthenticator(env, store, hc).LoginHeadless(ctx, "admin@nexus.ai", "admin123"); err != nil {
		t.Fatalf("login: %v", err)
	}
	gw := core.NewClient(env, core.NewTokenSource(env, store, hc), hc)

	cat, err := gw.AdminModels(ctx)
	if err != nil {
		t.Fatalf("models: %v", err)
	}
	model := ""
	for _, g := range cat.Data {
		for _, m := range g.Models {
			if m.Enabled && m.Type == "chat" {
				model = m.Code
				break
			}
		}
		if model != "" {
			break
		}
	}
	if model == "" {
		t.Skip("no enabled chat model in the local catalog — cannot drive the agent")
	}

	// The agent's model calls ride a Virtual Key; use the standing local test VK
	// (known synced + valid against the AI Gateway — a freshly-created VK has not
	// yet propagated to the gateway's validation cache).
	vkSecret := os.Getenv("NEXUS_TEST_VK")
	if vkSecret == "" {
		t.Skip("NEXUS_TEST_VK not set (source tests/.env.local) — cannot drive the VK-authed agent")
	}

	// Build the real agent wired to the TUI bridge as its Canvas + Confirm — the
	// same construction the conversation pane uses in production.
	b := newBridge(nil)
	onText := func(s string) { b.sendEv(agentTextMsg{delta: s}) }
	onTool := func(name string, input []byte) { b.sendEv(agentToolMsg{name: name, input: input}) }
	ag, err := capabilities.BuildAgent(ctx, capabilities.AgentDeps{
		Streamer: gw, Gateway: gw, Canvas: b, Confirm: b.confirm,
		VKSecret: vkSecret, Model: model, Env: "local",
		MemoryDir:   t.TempDir(),
		SessionDir:  t.TempDir(),
		OnText:      onText,
		OnToolStart: onTool,
	})
	if err != nil {
		t.Fatalf("BuildAgent: %v", err)
	}
	b.agent = ag

	cmd := b.startTurn("Summarize current traffic volume, then open the cost view.", "overview")
	if cmd == nil {
		t.Fatal("startTurn returned nil")
	}
	var sawActivity bool
	var done agentDoneMsg
	for i := 0; i < 400; i++ {
		switch m := cmd().(type) {
		case agentTextMsg, agentToolMsg, agentNavMsg, agentShowMsg, agentHighlightMsg:
			sawActivity = true
			cmd = b.drain()
		case agentConfirmMsg:
			cmd = b.reply(false) // a read prompt should not mitigate; decline defensively
		case agentDoneMsg:
			done = m
			i = 400
		default:
			cmd = b.drain()
		}
	}
	t.Logf("AGENT COCKPIT turn: final=%q err=%v sawActivity=%v", done.final, done.err, sawActivity)
	if done.err != nil {
		// The bridge→agent→model→gateway pipeline is exercised end-to-end; a VK that
		// the data plane rejects is an env precondition (a stale/disabled NEXUS_TEST_VK
		// — it lists models but fails vkauth on /v1/chat/completions), not a wiring
		// defect, so skip rather than fail. Any other error is a real defect.
		if errors.Is(done.err, core.ErrUnauthorized) || strings.Contains(done.err.Error(), "virtual key invalid") {
			t.Skipf("NEXUS_TEST_VK is not valid for data-plane traffic on this stack (%v) — bridge/agent wiring exercised up to the VK-auth boundary", done.err)
		}
		t.Fatalf("the agent turn errored over the live bridge: %v", done.err)
	}
	if !sawActivity && done.final == "" {
		t.Fatal("the bridge pumped no agent activity and no final answer — the wiring is dead")
	}
}
