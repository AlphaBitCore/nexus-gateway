// Audit family (S-100..S-103) — verifies the traffic_event lifecycle
// + spillstore overflow path + admin audit log coverage.
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

// TestS101_PayloadCaptureSpillstore — PM-grade e2e.
//
// BRAINSTORM (pre): the payload-capture subsystem stores request /
// response bodies into traffic_event_payload. Bodies under
// maxInlineBodyBytes go inline (jsonb column); bodies over the cap
// either spill to S3 (request_spill_ref non-null) or get truncated
// (request_truncated=true) when spillstore is unavailable. Plan §4
// S-101 wants the "spillstore overflow" path — but in dev, spillstore
// may not be wired to a real S3 bucket, so we accept BOTH outcomes as
// long as the row records the oversize signal (size_bytes > cap AND
// (spilled OR truncated)).
//
// Cross-service: CP admin PUT /settings/payload-capture → Hub
// `payload_capture` shadow broadcast → ai-gateway + compliance-proxy
// hot-reload → chat path captures body → MQ → Hub TrafficEventWriter
// → traffic_event_payload row.
//
// We use a small maxInlineBodyBytes (4 KiB) so the test prompt only
// needs ~8 KiB to trigger overflow — much faster than a 256 KiB
// payload + cheaper on upstream tokens. We restore the original
// config in cleanup so parallel sessions don't see a flipped switch.
func TestS101_PayloadCaptureSpillstore(t *testing.T) {
	sc := setupScenarioNoVK(t)
	ctx := context.Background()

	token, err := helpers.CPLogin(ctx, sc.Env)
	if err != nil {
		t.Fatalf("CPLogin: %v", err)
	}

	// 1) Snapshot original config so we can restore it on cleanup.
	original, err := helpers.GetPayloadCaptureConfig(ctx, sc.Env, token)
	if err != nil {
		t.Fatalf("GetPayloadCaptureConfig: %v", err)
	}
	sc.Cleanup.Register("restore payload-capture", func() error {
		// Best-effort restore — pass back the original keys.
		restore := map[string]any{}
		for _, k := range []string{"storeRequestBody", "storeResponseBody",
			"maxInlineBodyBytes", "maxRequestBytes", "maxResponseBytes"} {
			if v, ok := original[k]; ok {
				restore[k] = v
			}
		}
		return helpers.UpdatePayloadCaptureConfig(context.Background(), sc.Env, token, restore)
	})

	// 2) Enable capture with a small inline limit so an 8 KiB prompt
	// overflows. maxRequestBytes generous so the entire body is
	// captured (subject only to inline-vs-spill split, not truncation).
	preApply, err := helpers.BaselineConfigApply(ctx, sc.Env, "payload_capture")
	if err != nil {
		t.Fatalf("BaselineConfigApply payload_capture: %v", err)
	}
	if err := helpers.UpdatePayloadCaptureConfig(ctx, sc.Env, token, map[string]any{
		"storeRequestBody":   true,
		"storeResponseBody":  false,
		"maxInlineBodyBytes": 4096,
		"maxRequestBytes":    1024 * 1024,
	}); err != nil {
		t.Fatalf("UpdatePayloadCaptureConfig: %v", err)
	}
	if _, err := helpers.WaitForConfigApply(ctx, sc.Env, "payload_capture",
		preApply, 30*time.Second); err != nil {
		t.Fatalf("ai-gw/compliance-proxy did not hot-reload payload_capture: %v", err)
	}

	// 3) Send a chat with an oversize prompt.
	vkName := fmt.Sprintf("s101-%d", time.Now().UnixNano())
	vk, err := helpers.CreateMyVK(ctx, sc.Env, token, vkName)
	if err != nil {
		t.Fatalf("CreateMyVK: %v", err)
	}
	sc.Cleanup.Register("DeleteMyVK("+vk.ID+")", func() error {
		return helpers.DeleteMyVK(context.Background(), sc.Env, token, vk.ID)
	})

	// 8 KiB of repeating-but-unique tokens so the prompt is unique
	// (cache-bust) AND large enough to overflow the 4 KiB inline cap.
	bigPrompt := "Summarise in one word: " + strings.Repeat(
		fmt.Sprintf("token-%d-", time.Now().UnixNano()), 256) // ~8 KiB
	body := mustMarshal(t, map[string]any{
		"model": "moonshot-v1-32k",
		"messages": []map[string]string{
			{"role": "user", "content": bigPrompt},
		},
		"max_tokens":  4,
		"temperature": 0,
	})
	envForCall := *sc.Env
	envForCall.TestVK = vk.RawKey
	client := intg.LocalHTTPClient()
	status, respBody, err := intg.AIGwPostJSON(&envForCall, client, "/v1/chat/completions", body)
	if err != nil {
		t.Fatalf("AIGwPostJSON: %v", err)
	}
	if status != 200 {
		t.Fatalf("expected HTTP 200, got %d (body=%q)", status, truncate(respBody, 200))
	}

	// 4) traffic_event row first — that's the join key for payload.
	predicate := fmt.Sprintf(`source = 'ai-gateway'
		 AND path = '/v1/chat/completions'
		 AND identity->'vk'->>'id' = '%s'`, vk.ID)
	row, err := intg.WaitForRecentAuditEvent(
		context.Background(), sc.DB, predicate, nil, 45*time.Second,
	)
	if err != nil {
		t.Fatalf("traffic_event poll: %v", err)
	}
	if row == nil {
		t.Fatalf("no traffic_event row for VK %s within 45 s", vk.ID)
	}

	// 5) Poll traffic_event_payload row for the same id.
	deadline := time.Now().Add(45 * time.Second)
	var sizeBytes int64
	var inlineNotNull, spillNotNull, truncated bool
	for time.Now().Before(deadline) {
		err := sc.DB.QueryRow(ctx, `
			SELECT
				COALESCE(request_size_bytes, 0),
				inline_request_body IS NOT NULL,
				request_spill_ref IS NOT NULL,
				request_truncated
			FROM traffic_event_payload
			WHERE traffic_event_id = $1
		`, row.ID).Scan(&sizeBytes, &inlineNotNull, &spillNotNull, &truncated)
		if err == nil {
			break
		}
		time.Sleep(1 * time.Second)
	}
	if sizeBytes == 0 {
		t.Fatalf("no traffic_event_payload row for traffic_event %s within 45 s", row.ID)
	}

	// 6) Assertions:
	//   - size_bytes > inline cap (otherwise no overflow trigger)
	//   - either spilled OR truncated (NOT stored inline)
	if sizeBytes <= 4096 {
		t.Errorf("request_size_bytes=%d ≤ 4096 — prompt didn't overflow inline cap", sizeBytes)
	}
	if inlineNotNull && !spillNotNull && !truncated {
		t.Errorf("body stored inline (%d bytes) despite size > 4096 cap — overflow path didn't engage", sizeBytes)
	}
	if !spillNotNull && !truncated {
		t.Errorf("neither spill_ref nor truncated set — payload-capture overflow signal missing")
	}

	overflowKind := "spilled"
	if !spillNotNull && truncated {
		overflowKind = "truncated"
	}
	t.Logf("S-101 OK: payload %s (size=%d bytes, inline=%v, spill=%v, truncated=%v)",
		overflowKind, sizeBytes, inlineNotNull, spillNotNull, truncated)
}
