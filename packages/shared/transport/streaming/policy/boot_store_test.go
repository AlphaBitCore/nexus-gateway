package policy

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// TestBootStore_NilLoader_KeepsDefault covers the agent path: no DB
// loader, so BootStore returns a Store seeded with DefaultPolicy().
func TestBootStore_NilLoader_KeepsDefault(t *testing.T) {
	s := BootStore(context.Background(), nil, nil)
	if s == nil {
		t.Fatal("BootStore returned nil Store")
	}
	got := s.Get()
	want := DefaultPolicy()
	if got.Mode != want.Mode {
		t.Errorf("nil loader path: Mode = %q, want %q", got.Mode, want.Mode)
	}
}

// TestBootStore_LoaderError_KeepsDefault covers the cp/ai-gateway DB-
// read transient failure path: load returns an error, BootStore warn-
// logs and keeps the DefaultPolicy(). The Store is still usable.
func TestBootStore_LoaderError_KeepsDefault(t *testing.T) {
	boomErr := errors.New("simulated DB connect refused")
	loader := func(context.Context) (json.RawMessage, error) {
		return nil, boomErr
	}
	s := BootStore(context.Background(), loader, nil)
	if s == nil {
		t.Fatal("BootStore returned nil Store on loader error")
	}
	got := s.Get()
	if got.Mode != DefaultPolicy().Mode {
		t.Errorf("loader-error path should keep DefaultPolicy; got Mode=%q", got.Mode)
	}
}

// TestBootStore_EmptyRaw_KeepsDefault covers the "admin hasn't set the
// policy yet" path: loader returns empty (e.g. system_metadata row
// missing). BootStore keeps DefaultPolicy() so the next Hub shadow
// push can install the admin value via ApplyShadowState.
func TestBootStore_EmptyRaw_KeepsDefault(t *testing.T) {
	loader := func(context.Context) (json.RawMessage, error) {
		return nil, nil // empty raw + no error
	}
	s := BootStore(context.Background(), loader, nil)
	if s == nil {
		t.Fatal("BootStore returned nil Store on empty raw")
	}
	if got := s.Get(); got.Mode != DefaultPolicy().Mode {
		t.Errorf("empty raw should keep DefaultPolicy; got Mode=%q", got.Mode)
	}
}

// TestBootStore_DecodeError_KeepsDefault covers the corrupted-row
// path: loader returns non-empty raw but it doesn't unmarshal. Same
// outcome as the loader-error path — warn + keep default.
func TestBootStore_DecodeError_KeepsDefault(t *testing.T) {
	loader := func(context.Context) (json.RawMessage, error) {
		return json.RawMessage("{not valid json"), nil
	}
	s := BootStore(context.Background(), loader, nil)
	if s == nil {
		t.Fatal("BootStore returned nil Store on decode error")
	}
	if got := s.Get(); got.Mode != DefaultPolicy().Mode {
		t.Errorf("decode-error path should keep DefaultPolicy; got Mode=%q", got.Mode)
	}
}

// TestBootStore_ValidPolicy_Installs covers the happy path: loader
// returns a valid admin Policy blob, BootStore decodes + Set()s it
// onto the Store. The Get()ed Mode reflects the admin value.
func TestBootStore_ValidPolicy_Installs(t *testing.T) {
	loader := func(context.Context) (json.RawMessage, error) {
		return json.RawMessage(`{"default_mode":"buffer_full_block","fail_behavior":"fail_close","chunk_bytes":2048}`), nil
	}
	s := BootStore(context.Background(), loader, nil)
	if s == nil {
		t.Fatal("BootStore returned nil Store on valid policy")
	}
	got := s.Get()
	if got.Mode != ModeBufferFullBlock {
		t.Errorf("expected Mode=%q (admin-configured), got %q", ModeBufferFullBlock, got.Mode)
	}
	if got.FailBehavior != FailClose {
		t.Errorf("expected FailBehavior=%q, got %q", FailClose, got.FailBehavior)
	}
	if got.ChunkBytes != 2048 {
		t.Errorf("expected ChunkBytes=2048, got %d", got.ChunkBytes)
	}
}

// TestBootStore_ContextPlumbsToLoader pins that ctx is forwarded to
// the loader closure unchanged — important so request-scoped
// deadlines or trace IDs propagate to the DB read.
func TestBootStore_ContextPlumbsToLoader(t *testing.T) {
	type ctxKey struct{}
	ctx := context.WithValue(context.Background(), ctxKey{}, "sentinel")
	var seenValue any
	loader := func(loaderCtx context.Context) (json.RawMessage, error) {
		seenValue = loaderCtx.Value(ctxKey{})
		return nil, nil
	}
	_ = BootStore(ctx, loader, nil)
	if seenValue != "sentinel" {
		t.Errorf("loader did not receive caller ctx; got value = %v", seenValue)
	}
}
