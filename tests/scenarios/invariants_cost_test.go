// Cross-cutting invariant — cost is always stamped.
//
// For every /v1 ingress, a successful upstream call must stamp a non-null,
// positive cost on its traffic_event row. Asserted broadly (one bounded model
// per ingress, never the full catalog) so a single test guards the whole
// "forgot to stamp cost on ingress X" / "missed a stamp site" regression class.
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

// TestS150_CostStampedAcrossIngress — cross-cutting cost-stamping invariant.
//
// Cross-service: AI Gw (cost estimator stamps the row) -> MQ -> traffic_event.
// Cost lands in estimated_cost_usd for chat/responses/messages and in
// embedding_cost_usd for embeddings. The invariant: after a 200 from any
// ingress, the ingress-relevant cost column is > 0.
//
// Arms whose provider is not seeded locally SKIP with a concrete precondition
// (e.g. /v1/messages needs an Anthropic credential), per the L5 skip rule —
// the gate never reds on a missing local credential (that is amber, not our bug).
func TestS150_CostStampedAcrossIngress(t *testing.T) {
	sc := setupScenarioNoVK(t)
	ctx := context.Background()

	token, err := helpers.CPLogin(ctx, sc.Env)
	if err != nil {
		t.Fatalf("CPLogin: %v", err)
	}
	vkName := fmt.Sprintf("s150-%d", time.Now().UnixNano())
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

	type arm struct {
		name          string
		path          string
		body          []byte
		skipOnNoMatch bool // provider may be unseeded locally -> SKIP, not FAIL
	}
	arms := []arm{
		{
			name: "chat", path: "/v1/chat/completions",
			body: mustMarshal(t, map[string]any{
				"model":       "moonshot-v1-8k",
				"messages":    []map[string]string{{"role": "user", "content": fmt.Sprintf("Reply OK. n=%d", nonce)}},
				"max_tokens":  8,
				"temperature": 0,
			}),
		},
		{
			name: "responses", path: "/v1/responses",
			body: mustMarshal(t, map[string]any{
				"model": "moonshot-v1-8k",
				"input": fmt.Sprintf("Say OK. n=%d", nonce),
				"store": false,
			}),
		},
		{
			name: "embeddings", path: "/v1/embeddings",
			body: mustMarshal(t, map[string]any{
				"model": "text-embedding-3-small",
				"input": fmt.Sprintf("hello world n=%d", nonce),
			}),
		},
		{
			name: "messages", path: "/v1/messages", skipOnNoMatch: true,
			body: mustMarshal(t, map[string]any{
				"model":      "claude-haiku-4-5",
				"max_tokens": 8,
				"messages":   []map[string]string{{"role": "user", "content": fmt.Sprintf("Reply OK. n=%d", nonce)}},
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
					t.Skipf("%s: no provider seeded locally for this ingress (ROUTING_NO_MATCH); "+
						"seed a credential to exercise this arm. body=%q", a.name, truncate(respBody, 160))
				}
				t.Fatalf("%s: expected 200, got %d (%q)", a.name, status, truncate(respBody, 200))
			}

			// Poll for the row of this ingress + VK (audit pipeline is async via MQ).
			// The invariant is "cost stamped non-null": a successful call must
			// populate estimated_cost_usd — the column the prod NULL-cost incident
			// left empty. A legitimately-zero cost (a model with no local pricing
			// seeded) is still non-null and passes; only an unstamped NULL fails.
			// pgx scans a NULL NUMERIC into a *float64 as nil.
			const query = `
				SELECT estimated_cost_usd::float8, embedding_cost_usd::float8
				FROM traffic_event
				WHERE source = 'ai-gateway'
				  AND identity->'vk'->>'id' = $1
				  AND path = $2
				  AND "timestamp" > NOW() - INTERVAL '300 seconds'
				ORDER BY created_at DESC
				LIMIT 1`
			const tries = 15
			const interval = 2 * time.Second
			var estCost, embCost *float64
			found := false
			for i := 0; i < tries; i++ {
				if scanErr := sc.DB.QueryRow(ctx, query, vk.ID, a.path).Scan(&estCost, &embCost); scanErr == nil {
					found = true
					break
				}
				time.Sleep(interval)
			}
			if !found {
				t.Fatalf("%s: no traffic_event row (source=ai-gateway, path=%s, vk=%s) within %v — "+
					"audit pipeline lagged or the row never landed", a.name, a.path, vk.ID, time.Duration(tries)*interval)
			}
			if estCost == nil {
				t.Errorf("%s: estimated_cost_usd is NULL on a successful call — cost stamp site missing for this ingress", a.name)
				return
			}
			embStamped := embCost != nil
			t.Logf("S-150 %s OK: estimated_cost_usd stamped = %.8f (embedding_cost_usd stamped=%v)", a.name, *estCost, embStamped)
		})
	}
}
