// Cross-cutting invariant — the hook pipeline always fires.
//
// Every successful /v1 request must run the compliance hook chain and record
// it: a non-empty request_hooks_pipeline plus a request_hook_decision. A
// request that reaches a provider WITHOUT the hook chain running is a
// compliance-bypass regression (the exact failure a silent refactor produces).
package scenarios_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	intg "github.com/AlphaBitCore/nexus-gateway/tests/integration-go/helpers"
	"github.com/AlphaBitCore/nexus-gateway/tests/scenarios/helpers"
)

// TestS154_HookPipelineAlwaysFires — cross-cutting hook invariant.
//
// Cross-service: AI Gw (hook engine runs the request-stage chain) -> MQ ->
// traffic_event (request_hook_decision + request_hooks_pipeline). For a clean
// prompt on each ingress the decision is set (APPROVE) and the pipeline lists
// the hooks that ran. The invariant: decision != '' AND pipeline length >= 1.
//
// Messages SKIPs when no Anthropic credential is seeded locally.
func TestS154_HookPipelineAlwaysFires(t *testing.T) {
	sc := setupScenarioNoVK(t)
	ctx := context.Background()

	token, err := helpers.CPLogin(ctx, sc.Env)
	if err != nil {
		t.Fatalf("CPLogin: %v", err)
	}
	vkName := fmt.Sprintf("s154-%d", time.Now().UnixNano())
	vk, err := helpers.CreateMyVK(ctx, sc.Env, token, vkName)
	if err != nil {
		t.Fatalf("CreateMyVK: %v", err)
	}
	sc.Cleanup.Register("DeleteMyVK("+vk.ID+")", func() error {
		return helpers.DeleteMyVK(context.Background(), sc.Env, token, vk.ID)
	})

	envForCall := *sc.Env
	envForCall.TestVK = vk.RawKey
	client := intg.LocalHTTPClient()
	nonce := time.Now().UnixNano()
	clean := fmt.Sprintf("Please reply with the single word OK. n=%d", nonce)

	type arm struct {
		name          string
		path          string
		body          []byte
		skipOnNoMatch bool
	}
	arms := []arm{
		{
			name: "chat", path: "/v1/chat/completions",
			body: mustMarshal(t, map[string]any{
				"model":       "moonshot-v1-8k",
				"messages":    []map[string]string{{"role": "user", "content": clean}},
				"max_tokens":  8,
				"temperature": 0,
			}),
		},
		{
			name: "responses", path: "/v1/responses",
			body: mustMarshal(t, map[string]any{
				"model": "moonshot-v1-8k",
				"input": clean,
				"store": false,
			}),
		},
		{
			name: "embeddings", path: "/v1/embeddings",
			body: mustMarshal(t, map[string]any{
				"model": "text-embedding-3-small",
				"input": clean,
			}),
		},
		{
			name: "messages", path: "/v1/messages", skipOnNoMatch: true,
			body: mustMarshal(t, map[string]any{
				"model":      "claude-haiku-4-5",
				"max_tokens": 8,
				"messages":   []map[string]string{{"role": "user", "content": clean}},
			}),
		},
	}

	for _, a := range arms {
		a := a
		t.Run(a.name, func(t *testing.T) {
			status, respBody, err := intg.AIGwPostJSON(&envForCall, client, a.path, a.body)
			if err != nil {
				t.Fatalf("%s: AIGwPostJSON: %v", a.name, err)
			}
			if status != 200 {
				rb := string(respBody)
				if a.skipOnNoMatch && (strings.Contains(rb, "ROUTING_NO_MATCH") || strings.Contains(rb, "no available provider")) {
					t.Skipf("%s: no provider seeded locally for this ingress (ROUTING_NO_MATCH); body=%q", a.name, truncate(respBody, 160))
				}
				t.Fatalf("%s: expected 200, got %d (%q)", a.name, status, truncate(respBody, 200))
			}

			// jsonb_array_length errors on a non-array, so guard with jsonb_typeof.
			const query = `
				SELECT COALESCE(request_hook_decision, ''),
				       CASE WHEN jsonb_typeof(request_hooks_pipeline) = 'array'
				            THEN jsonb_array_length(request_hooks_pipeline) ELSE 0 END
				FROM traffic_event
				WHERE source = 'ai-gateway'
				  AND identity->'vk'->>'id' = $1
				  AND path = $2
				  AND "timestamp" > NOW() - INTERVAL '300 seconds'
				ORDER BY created_at DESC
				LIMIT 1`
			const tries = 15
			const interval = 2 * time.Second
			var decision string
			var pipelineLen int
			found := false
			for i := 0; i < tries; i++ {
				if scanErr := sc.DB.QueryRow(ctx, query, vk.ID, a.path).Scan(&decision, &pipelineLen); scanErr == nil && decision != "" {
					found = true
					break
				}
				time.Sleep(interval)
			}
			if !found {
				t.Fatalf("%s: no traffic_event row with a hook decision (path=%s, vk=%s) within %v — "+
					"row never landed or hook decision is NULL on a successful call (pipeline bypassed)",
					a.name, a.path, vk.ID, time.Duration(tries)*interval)
			}
			if pipelineLen < 1 {
				t.Errorf("%s: request_hooks_pipeline empty on a successful call (decision=%q) — "+
					"the hook chain did not run for this ingress (compliance bypass)", a.name, decision)
			}
			t.Logf("S-154 %s OK: request_hook_decision=%q pipeline_hooks=%d", a.name, decision, pipelineLen)
		})
	}
}
