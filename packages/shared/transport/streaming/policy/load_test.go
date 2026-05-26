package policy_test

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming/policy"
)

func TestOverrideFromColumns_NilWhenAllNull(t *testing.T) {
	if policy.OverrideFromColumns(nil, nil, nil, nil, nil, nil, nil, nil) != nil {
		t.Fatal("all-NULL should return nil Override (inherit global)")
	}
}

func TestOverrideFromColumns_DropsInvalidEnum(t *testing.T) {
	bad := "not-a-mode"
	zero := 0
	o := policy.OverrideFromColumns(&bad, nil, nil, nil, &bad, nil, nil, nil)
	if o != nil {
		t.Fatalf("invalid enum overrides should be dropped, got %+v", o)
	}
	// chunk_bytes=0 is technically valid (treated as "use global")
	o = policy.OverrideFromColumns(nil, &zero, nil, nil, nil, nil, nil, nil)
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
	o := policy.OverrideFromColumns(&mode, &cb, &to, &mb, &fb, &tr, &tr, &tr)
	if o == nil || *o.Mode != policy.ModeChunkedAsync || *o.ChunkBytes != cb ||
		*o.HookTimeoutMs != to || *o.MaxBufferBytes != mb ||
		*o.FailBehavior != policy.FailClose ||
		!*o.CaptureRequestBody || !*o.CaptureResponseBody || !*o.RawSpillEnabled {
		t.Fatalf("override unmarshal wrong: %+v", o)
	}
}
