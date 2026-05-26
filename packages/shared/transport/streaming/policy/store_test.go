package policy

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
)

func TestStore_NilGetReturnsDefault(t *testing.T) {
	var s *Store
	got := s.Get()
	if got.Mode != ModePassThrough {
		t.Errorf("nil-Store Get should return DefaultPolicy, got Mode=%q", got.Mode)
	}
}

func TestStore_NilSetIsNoOp(t *testing.T) {
	// Defensive: Set on a nil receiver must not panic. Without this guard,
	// init-order bugs (Set called before NewStore) would crash the data
	// plane.
	var s *Store
	s.Set(DefaultPolicy()) // must not panic
}

func TestStore_GetUninitializedCurrent(t *testing.T) {
	// A Store constructed without NewStore (zero-value struct) has nil
	// current pointer. Get must fall back to DefaultPolicy rather than
	// dereferencing nil.
	var s Store
	got := s.Get()
	if got.Mode != ModePassThrough {
		t.Errorf("zero-value Store should return DefaultPolicy, got Mode=%q", got.Mode)
	}
}

func TestStore_GetReturnsInitial(t *testing.T) {
	want := DefaultPolicy()
	want.HookTimeoutMs = 4242
	s := NewStore(want)
	got := s.Get()
	if got.HookTimeoutMs != 4242 {
		t.Errorf("HookTimeoutMs: want 4242, got %d", got.HookTimeoutMs)
	}
}

func TestStore_SetReplacesAtomically(t *testing.T) {
	s := NewStore(DefaultPolicy())
	new := DefaultPolicy()
	new.Mode = ModeBufferFullBlock
	new.MaxBufferBytes = 1024
	s.Set(new)
	got := s.Get()
	if got.Mode != ModeBufferFullBlock {
		t.Errorf("Mode: want BufferFullBlock, got %q", got.Mode)
	}
	if got.MaxBufferBytes != 1024 {
		t.Errorf("MaxBufferBytes: want 1024, got %d", got.MaxBufferBytes)
	}
}

func TestDecodeGlobalPolicy_AllFieldsOverridden(t *testing.T) {
	// Pin: each rawConfig field has its own override branch in
	// DecodeGlobalPolicy. Without per-field coverage, a refactor that
	// drops one would silently inherit the default for that knob.
	captureReq := true
	captureResp := false
	rawSpill := true
	raw := []byte(`{
		"default_mode": "chunked_async",
		"chunk_bytes": 8192,
		"hook_timeout_ms": 250,
		"max_buffer_bytes": 1048576,
		"fail_behavior": "fail_close",
		"capture_request_body": true,
		"capture_response_body": false,
		"raw_body_spill_enabled": true
	}`)
	got, err := DecodeGlobalPolicy(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Mode != ModeChunkedAsync {
		t.Errorf("Mode: %q", got.Mode)
	}
	if got.ChunkBytes != 8192 {
		t.Errorf("ChunkBytes: %d", got.ChunkBytes)
	}
	if got.HookTimeoutMs != 250 {
		t.Errorf("HookTimeoutMs: %d", got.HookTimeoutMs)
	}
	if got.MaxBufferBytes != 1048576 {
		t.Errorf("MaxBufferBytes: %d", got.MaxBufferBytes)
	}
	if got.FailBehavior != FailClose {
		t.Errorf("FailBehavior: %q", got.FailBehavior)
	}
	if got.CaptureRequestBody != captureReq {
		t.Errorf("CaptureRequestBody: %v", got.CaptureRequestBody)
	}
	if got.CaptureResponseBody != captureResp {
		t.Errorf("CaptureResponseBody: %v", got.CaptureResponseBody)
	}
	if got.RawSpillEnabled != rawSpill {
		t.Errorf("RawSpillEnabled: %v", got.RawSpillEnabled)
	}
}

func TestDecodeGlobalPolicy_InvalidProducesDefault(t *testing.T) {
	// IsValid() check at the end of DecodeGlobalPolicy: a row that
	// passes JSON parse but fails policy invariants (e.g. negative
	// chunk_bytes coerces to 0 implicitly, but an explicit bad mode
	// makes IsValid false) must return DefaultPolicy + error.
	raw := []byte(`{"default_mode": "not-a-mode"}`)
	got, err := DecodeGlobalPolicy(raw)
	if err == nil {
		t.Fatal("invalid mode should yield error")
	}
	if got != DefaultPolicy() {
		t.Errorf("invalid mode should fall back to default: got %+v", got)
	}
}

func TestStore_ApplyShadowState_ValidPayload(t *testing.T) {
	s := NewStore(DefaultPolicy())
	raw := json.RawMessage(`{"default_mode":"chunked_async","hook_timeout_ms":5000,"fail_behavior":"fail_close"}`)
	if err := s.ApplyShadowState(context.Background(), raw); err != nil {
		t.Fatalf("ApplyShadowState: %v", err)
	}
	got := s.Get()
	if got.Mode != ModeChunkedAsync {
		t.Errorf("Mode: want chunked_async, got %q", got.Mode)
	}
	if got.HookTimeoutMs != 5000 {
		t.Errorf("HookTimeoutMs: want 5000, got %d", got.HookTimeoutMs)
	}
	if got.FailBehavior != FailClose {
		t.Errorf("FailBehavior: want fail_close, got %q", got.FailBehavior)
	}
}

func TestStore_ApplyShadowState_EmptyResetsToDefault(t *testing.T) {
	s := NewStore(DefaultPolicy())
	// Push a real config first.
	if err := s.ApplyShadowState(context.Background(), json.RawMessage(`{"default_mode":"buffer_full_block"}`)); err != nil {
		t.Fatalf("ApplyShadowState (initial): %v", err)
	}
	if s.Get().Mode != ModeBufferFullBlock {
		t.Fatalf("initial set didn't take")
	}
	// Empty payload should reset to DefaultPolicy.
	if err := s.ApplyShadowState(context.Background(), nil); err != nil {
		t.Fatalf("ApplyShadowState (empty): %v", err)
	}
	if s.Get().Mode != ModePassThrough {
		t.Errorf("empty payload should restore default (passthrough), got %q", s.Get().Mode)
	}
}

func TestStore_ApplyShadowState_InvalidJSONReturnsError(t *testing.T) {
	s := NewStore(DefaultPolicy())
	prev := s.Get()
	err := s.ApplyShadowState(context.Background(), json.RawMessage(`not-json`))
	if err == nil {
		t.Fatal("invalid JSON should error")
	}
	// Store unchanged on parse error.
	if s.Get().Mode != prev.Mode {
		t.Errorf("Store should not mutate on parse error, got %+v", s.Get())
	}
}

func TestStore_ConcurrentReadWriteRaceFree(t *testing.T) {
	s := NewStore(DefaultPolicy())
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.Get()
		}()
	}
	for i := range 10 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := DefaultPolicy()
			p.HookTimeoutMs = 1000 + i
			s.Set(p)
		}(i)
	}
	wg.Wait()
	// Just need it to not race; final value is whichever Set happened last.
}
