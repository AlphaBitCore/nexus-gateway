// Cross-cutting invariant — the response cache is mandatory across every
// chat-like ingress. Catches the cross-ingress asymmetry class (ingress A
// caches, ingress B silently does not), which an upstream cost regression and
// the prod cache-NULL incident both depend on.
package scenarios_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	intg "github.com/AlphaBitCore/nexus-gateway/tests/integration-go/helpers"
	"github.com/AlphaBitCore/nexus-gateway/tests/scenarios/helpers"
)

// TestS151_CacheMandatoryAcrossIngress — cross-cutting cache invariant.
//
// For each chat-like /v1 ingress, two byte-identical requests within TTL must
// produce a cache HIT on the second: the AI Gw serves the same upstream
// response (identical response id) and stamps gateway_cache_status='hit' on the
// second traffic_event row. Asserted broadly (one bounded model per ingress) so
// a single test guards "ingress X stopped caching" for every ingress at once.
//
// Embeddings is excluded — it has no prompt-cache semantic. Messages SKIPs when
// no Anthropic credential is seeded locally (amber, not our bug).
//
// Cross-service: AI Gw (cache.Get/Set, Redis L1) -> MQ -> traffic_event
// (gateway_cache_status). Binding: feedback_cache_mandatory_all_ingress.
func TestS151_CacheMandatoryAcrossIngress(t *testing.T) {
	sc := setupScenarioNoVK(t)
	ctx := context.Background()

	token, err := helpers.CPLogin(ctx, sc.Env)
	if err != nil {
		t.Fatalf("CPLogin: %v", err)
	}
	vkName := fmt.Sprintf("s151-%d", time.Now().UnixNano())
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

	parseID := func(b []byte) string {
		var out struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(b, &out)
		return out.ID
	}

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
				"messages":    []map[string]string{{"role": "user", "content": fmt.Sprintf("Reply CACHE_OK. n=%d", nonce)}},
				"max_tokens":  8,
				"temperature": 0,
			}),
		},
		{
			name: "responses", path: "/v1/responses",
			body: mustMarshal(t, map[string]any{
				"model": "moonshot-v1-8k",
				"input": fmt.Sprintf("Say CACHE_OK. n=%d", nonce),
				"store": false,
			}),
		},
		{
			name: "messages", path: "/v1/messages", skipOnNoMatch: true,
			body: mustMarshal(t, map[string]any{
				"model":      "claude-haiku-4-5",
				"max_tokens": 8,
				"messages":   []map[string]string{{"role": "user", "content": fmt.Sprintf("Reply CACHE_OK. n=%d", nonce)}},
			}),
		},
	}

	for _, a := range arms {
		a := a
		t.Run(a.name, func(t *testing.T) {
			// Request 1 — cache MISS (writes the entry).
			s1, b1, err := intg.AIGwPostJSON(&envForCall, client, a.path, a.body)
			if err != nil {
				t.Fatalf("%s req1: %v", a.name, err)
			}
			if s1 != 200 {
				rb := string(b1)
				if a.skipOnNoMatch && (strings.Contains(rb, "ROUTING_NO_MATCH") || strings.Contains(rb, "no available provider")) {
					t.Skipf("%s: no provider seeded locally for this ingress (ROUTING_NO_MATCH); body=%q", a.name, truncate(b1, 160))
				}
				t.Fatalf("%s req1 expected 200, got %d (%q)", a.name, s1, truncate(b1, 200))
			}
			id1 := parseID(b1)

			time.Sleep(2 * time.Second) // let the cache write commit before req2

			// Request 2 — cache HIT expected.
			s2, b2, err := intg.AIGwPostJSON(&envForCall, client, a.path, a.body)
			if err != nil {
				t.Fatalf("%s req2: %v", a.name, err)
			}
			if s2 != 200 {
				t.Fatalf("%s req2 expected 200, got %d (%q)", a.name, s2, truncate(b2, 200))
			}
			id2 := parseID(b2)

			// Both requests log a traffic_event row; the repeat must produce a
			// gateway_cache_status='hit'. We COUNT hit rows for this fresh
			// per-test VK+path rather than reading "the latest row": the MQ
			// consumer can write req1's miss and req2's hit with an identical
			// created_at, so an ORDER BY created_at tie-break is non-deterministic.
			// (Response-id equality is also not a reliable signal — the responses
			// ingress mints a fresh local id even on a cache hit.)
			const q = `
				SELECT count(*), count(*) FILTER (WHERE gateway_cache_status = 'hit')
				FROM traffic_event
				WHERE source = 'ai-gateway'
				  AND identity->'vk'->>'id' = $1
				  AND path = $2
				  AND "timestamp" > NOW() - INTERVAL '300 seconds'`
			const tries = 20
			const interval = 2 * time.Second
			var total, hits int64
			for i := 0; i < tries; i++ {
				_ = sc.DB.QueryRow(ctx, q, vk.ID, a.path).Scan(&total, &hits)
				if total >= 2 && hits >= 1 {
					break
				}
				time.Sleep(interval)
			}
			if total < 2 {
				t.Fatalf("%s: only %d traffic_event row(s) for path=%s VK=%s within %v — expected 2 (req1 + repeat); "+
					"audit pipeline lagged", a.name, total, a.path, vk.ID, time.Duration(tries)*interval)
			}
			if hits < 1 {
				t.Errorf("%s: no gateway_cache_status='hit' across %d rows for path=%s — this ingress did not serve the "+
					"repeat request from cache (cross-ingress cache asymmetry)", a.name, total, a.path)
			}
			t.Logf("S-151 %s OK: rows=%d hits=%d (chat-style id equal=%v)", a.name, total, hits, id1 != "" && id1 == id2)
		})
	}
}
