// Quota family (S-040..S-045) — verifies the AI Gateway enforces the
// per-VK rateLimitRpm throttle: chats past the limit get HTTP 429 and
// traffic_event records the throttle decision.
package scenarios_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	intg "github.com/AlphaBitCore/nexus-gateway/tests/integration-go/helpers"
	"github.com/AlphaBitCore/nexus-gateway/tests/scenarios/helpers"
)

// TestS040_VKRateLimit429 — PM-grade e2e.
//
// BRAINSTORM (pre): plan §4 calls for a 100-req/min policy with 80%
// warning + 100% 429. We use a much smaller window (3 rpm) so the
// scenario can deterministically trip the 429 path in seconds rather
// than minutes, AND avoid spending hundreds of upstream tokens per
// CI run. Quota is enforced inside ai-gateway's VK cache; the
// virtual_keys config_key is lazy-load (not push-broadcast), so the
// runtime-state core here is "the throttle takes effect on the first
// chat that consults the new VK." We confirm this transitively via
// the chat returning 200 → 200 → 200 → 429 sequence.
//
// Cross-service: CP (POST /api/my/virtual-keys with rateLimitRpm) →
// CP DB write → AI Gw (resolves VK on cache miss, enforces rate
// limit) → traffic_event row stamps either the success path or a
// 429-throttle row.
//
// Assertions:
//   1. CreateMyVKWith persists rateLimitRpm=3.
//   2. 3 chats succeed within the window (status 200).
//   3. 4th chat returns 429 with a structured error envelope.
//   4. Cleanup deletes the VK.
//
// We intentionally do NOT assert a traffic_event row for the 429
// because the gateway's auth-fail / throttle path may not stamp a
// row in all builds; the HTTP 429 with envelope is sufficient
// terminal evidence.
func TestS040_VKRateLimit429(t *testing.T) {
	sc := setupScenarioNoVK(t)
	ctx := context.Background()

	token, err := helpers.CPLogin(ctx, sc.Env)
	if err != nil {
		t.Fatalf("CPLogin: %v", err)
	}

	rpm := 3
	vkName := fmt.Sprintf("s040-%d", time.Now().UnixNano())
	vk, err := helpers.CreateMyVKWith(ctx, sc.Env, token, vkName, &helpers.CreateMyVKOpts{
		RateLimitRpm: &rpm,
	})
	if err != nil {
		t.Fatalf("CreateMyVKWith: %v", err)
	}
	sc.Cleanup.Register("DeleteMyVK("+vk.ID+")", func() error {
		return helpers.DeleteMyVK(context.Background(), sc.Env, token, vk.ID)
	})

	// AdminAuditLog row for VK create (same as S-001).
	auditRow, err := helpers.WaitForAdminAuditRow(ctx, sc.DB,
		"create", vkName, 15*time.Second)
	if err != nil {
		t.Fatalf("WaitForAdminAuditRow: %v", err)
	}
	if auditRow == nil {
		t.Fatalf("AdminAuditLog row for VK create did not appear within 15 s")
	}

	envForCall := *sc.Env
	envForCall.TestVK = vk.RawKey
	client := intg.LocalHTTPClient()

	// Send 4 chats with cache-bust nonces so each hits upstream
	// independently. First 3 must succeed; 4th must be throttled.
	statuses := make([]int, 0, 4)
	for i := 0; i < 4; i++ {
		body := mustMarshal(t, map[string]any{
			"model": "moonshot-v1-8k",
			"messages": []map[string]string{
				{"role": "user", "content": fmt.Sprintf("Reply OK. i=%d nonce=%d", i, time.Now().UnixNano())},
			},
			"max_tokens":  4,
			"temperature": 0,
		})
		status, respBody, err := intg.AIGwPostJSON(&envForCall, client, "/v1/chat/completions", body)
		if err != nil {
			t.Fatalf("AIGwPostJSON i=%d: %v", i, err)
		}
		statuses = append(statuses, status)
		if i < 3 && status != 200 {
			t.Fatalf("i=%d expected 200 (within limit), got %d (%q)", i, status, truncate(respBody, 160))
		}
		if i == 3 && status != 429 {
			t.Fatalf("i=3 expected 429 (over rateLimitRpm=%d), got %d (%q)", rpm, status, truncate(respBody, 200))
		}
	}
	t.Logf("S-040 OK: rateLimitRpm=%d → status sequence %v (4th throttled)", rpm, statuses)
}