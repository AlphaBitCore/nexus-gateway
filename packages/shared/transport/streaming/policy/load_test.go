package policy_test

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming/policy"
)

func TestOverrideFromColumns_NilWhenAllNull(t *testing.T) {
	if policy.OverrideFromColumns(nil, nil, nil, nil, nil, nil, nil) != nil {
		t.Fatal("all-NULL should return nil Override (inherit global)")
	}
}

func TestOverrideFromColumns_DropsInvalidEnum(t *testing.T) {
	bad := "not-a-mode"
	zero := 0
	o := policy.OverrideFromColumns(&bad, nil, nil, nil, &bad, nil, nil)
	if o != nil {
		t.Fatalf("invalid enum overrides should be dropped, got %+v", o)
	}
	// chunk_bytes=0 is technically valid (treated as "use global")
	o = policy.OverrideFromColumns(nil, &zero, nil, nil, nil, nil, nil)
	if o == nil || o.ChunkBytes == nil || *o.ChunkBytes != 0 {
		t.Fatalf("ChunkBytes=0 should round-trip; got %+v", o)
	}
}

func TestOverrideFromColumns_AllValid(t *testing.T) {
	mode := string(policy.ModeChunkedAsync)
	cb := 16384
	to := 5000
	mb := 32 << 20
	fb := string(policy.FailClose)
	tr := true
	o := policy.OverrideFromColumns(&mode, &cb, &to, &mb, &fb, &tr, &tr)
	if o == nil || *o.Mode != policy.ModeChunkedAsync || *o.ChunkBytes != cb ||
		*o.HookTimeoutMs != to || *o.MaxBufferBytes != mb ||
		*o.FailBehavior != policy.FailClose ||
		!*o.CaptureRequestBody || !*o.CaptureResponseBody {
		t.Fatalf("override unmarshal wrong: %+v", o)
	}
}

// A shadow blob written before the raw-body-spill switch was retired still
// carries the key. Decoding must IGNORE it, not fail: the field was inert, and a
// node that refused its own stored config over a retired key would turn a
// cleanup into an outage.
func TestDecodeGlobalPolicy_RetiredRawSpillKeyIsIgnored(t *testing.T) {
	blob := []byte(`{
		"default_mode": "buffer_full_block",
		"chunk_bytes": 8192,
		"hook_timeout_ms": 1500,
		"max_buffer_bytes": 65536,
		"fail_behavior": "fail_close",
		"capture_request_body": true,
		"capture_response_body": true,
		"raw_body_spill_enabled": true
	}`)
	got, err := policy.DecodeGlobalPolicy(blob)
	if err != nil {
		t.Fatalf("a blob carrying the retired key must still decode: %v", err)
	}
	if got.Mode != policy.ModeBufferFullBlock {
		t.Errorf("Mode = %q; the rest of the blob must survive the retired key", got.Mode)
	}
	if !got.CaptureRequestBody || !got.CaptureResponseBody {
		t.Errorf("capture flags lost: %+v", got)
	}
}
