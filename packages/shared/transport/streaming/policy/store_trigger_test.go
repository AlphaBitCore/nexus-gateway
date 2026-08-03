package policy_test

import (
	"context"
	"encoding/json"
	"testing"

	policy "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming/policy"
)

// TestApplyShadowState_EmptyTriggerNeverResetsState is the regression guard for
// R-7, which disabled stream inspection on the compliance proxy in production
// shape.
//
// streaming_compliance is a Type-B key in configkey.go — "invalidation trigger —
// state stays null/{}" — so the Hub pushes JSON null on EVERY push. The receiver
// handed that null to ApplyShadowState, which decoded it into DefaultPolicy().
// Measured on the proxy: boot installed the admin's chunked_async at
// 10:30:50.045 and the first trigger overwrote it with passthrough 70 ms later.
// passthrough neither accumulates nor can reject, so every SSE stream was then
// relayed uninspected while the admin's setting said otherwise.
//
// json.RawMessage("null") is FOUR BYTES, so a len()==0 guard does not catch what
// the Hub actually sends — which is why this test enumerates the encodings
// rather than testing one.
func TestApplyShadowState_EmptyTriggerNeverResetsState(t *testing.T) {
	configured := policy.Policy{
		Mode:          policy.ModeChunkedAsync,
		FailBehavior:  policy.FailClose,
		ChunkBytes:    4096,
		HookTimeoutMs: 1234,
	}

	for _, raw := range []string{"null", "", "  ", "{}", " null "} {
		t.Run("payload="+raw, func(t *testing.T) {
			s := policy.NewStore(configured)
			if err := s.ApplyShadowState(context.Background(), json.RawMessage(raw)); err != nil {
				t.Fatalf("an empty trigger must not error: %v", err)
			}
			got := s.Get()
			if got.Mode != configured.Mode {
				t.Errorf("mode = %q after an empty trigger, want %q kept — a trigger carries no state, "+
					"and installing defaults from it silently reverts the admin's configuration "+
					"(here: stream inspection off)", got.Mode, configured.Mode)
			}
			if got.FailBehavior != configured.FailBehavior || got.ChunkBytes != configured.ChunkBytes {
				t.Errorf("the rest of the policy was reset too: %+v want %+v", got, configured)
			}
		})
	}
}

// TestApplyShadowState_RealPayloadStillApplies is the other half. A guard that
// swallowed every payload would keep this test green while making the shadow
// push a no-op, which is the same defect pointing the other way.
func TestApplyShadowState_RealPayloadStillApplies(t *testing.T) {
	s := policy.NewStore(policy.Policy{Mode: policy.ModePassThrough})
	raw := json.RawMessage(`{"default_mode":"buffer_full_block","chunk_bytes":2048}`)
	if err := s.ApplyShadowState(context.Background(), raw); err != nil {
		t.Fatalf("ApplyShadowState: %v", err)
	}
	got := s.Get()
	if got.Mode != policy.ModeBufferFullBlock {
		t.Errorf("mode = %q, want buffer_full_block — a payload that carries state must still be applied", got.Mode)
	}
	if got.ChunkBytes != 2048 {
		t.Errorf("chunkBytes = %d, want 2048", got.ChunkBytes)
	}
}
