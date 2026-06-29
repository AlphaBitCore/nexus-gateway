package configstore

import (
	"context"
	"errors"
	"github.com/goccy/go-json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestFinalizeAIGuardLoad_ErrNoRowsReturnsDefaults covers the
// fresh-DB branch: a missing singleton row must produce the
// conservative defaults so the gateway boots without panicking on a
// freshly-migrated database where the seed step was skipped.
func TestFinalizeAIGuardLoad_ErrNoRowsReturnsDefaults(t *testing.T) {
	got, err := finalizeAIGuardLoad(&AIGuardConfig{}, nil, pgx.ErrNoRows)
	if err != nil {
		t.Fatalf("ErrNoRows must not propagate; got: %v", err)
	}
	want := defaultAIGuardConfig()
	if got.ID != want.ID || got.BackendMode != want.BackendMode ||
		got.TimeoutMs != want.TimeoutMs || got.CacheTTLSeconds != want.CacheTTLSeconds {
		t.Errorf("defaults: got %+v, want %+v", got, want)
	}
}

// TestFinalizeAIGuardLoad_GenericErrorWraps covers any other DB
// error path — timeout, connection refused, planner error — must
// surface a wrapped "configstore: load ai_guard_config:" prefix so
// admin logs can attribute the failure without re-deriving the
// package path.
func TestFinalizeAIGuardLoad_GenericErrorWraps(t *testing.T) {
	want := errors.New("simulated DB outage")
	got, err := finalizeAIGuardLoad(&AIGuardConfig{}, nil, want)
	if err == nil {
		t.Fatal("generic err must propagate")
	}
	if !errors.Is(err, want) {
		t.Errorf("error must wrap original via %%w; got: %v", err)
	}
	if !strings.Contains(err.Error(), "configstore: load ai_guard_config") {
		t.Errorf("missing package-attribution prefix: %q", err.Error())
	}
	if got != nil {
		t.Errorf("result must be nil on err; got: %+v", got)
	}
}

// TestFinalizeAIGuardLoad_HeadersParsed covers the success branch
// with a populated headers blob — the JSON decode must thread back
// into cfg.CustomHeaders.
func TestFinalizeAIGuardLoad_HeadersParsed(t *testing.T) {
	cfg := &AIGuardConfig{ID: "singleton", BackendMode: "external_url"}
	headers := []byte(`{"X-Tenant":"nexus","X-Build":"prod-20260516"}`)
	got, err := finalizeAIGuardLoad(cfg, headers, nil)
	if err != nil {
		t.Fatalf("happy path: %v", err)
	}
	if got.CustomHeaders["X-Tenant"] != "nexus" {
		t.Errorf("X-Tenant: %+v", got.CustomHeaders)
	}
	if got.CustomHeaders["X-Build"] != "prod-20260516" {
		t.Errorf("X-Build: %+v", got.CustomHeaders)
	}
}

// TestFinalizeAIGuardLoad_EmptyHeadersLeavesNil covers the
// `len(headersJSON) > 0` skip branch — an empty blob must NOT
// produce an empty map (callers distinguish nil from len-0 for
// "no headers configured" vs "explicitly empty headers").
func TestFinalizeAIGuardLoad_EmptyHeadersLeavesNil(t *testing.T) {
	cfg := &AIGuardConfig{ID: "singleton"}
	got, err := finalizeAIGuardLoad(cfg, nil, nil)
	if err != nil {
		t.Fatalf("empty headers: %v", err)
	}
	if got.CustomHeaders != nil {
		t.Errorf("CustomHeaders should stay nil for empty blob; got %+v", got.CustomHeaders)
	}
}

// TestFinalizeAIGuardLoad_MalformedHeadersWraps covers the
// json.Unmarshal error branch — operators who hand-edit the row
// must see "configstore: parse custom_headers:" not a bare json
// error so the fix surface is obvious.
func TestFinalizeAIGuardLoad_MalformedHeadersWraps(t *testing.T) {
	cfg := &AIGuardConfig{ID: "singleton"}
	got, err := finalizeAIGuardLoad(cfg, []byte("not json"), nil)
	if err == nil {
		t.Fatal("malformed headers must surface a decode error")
	}
	if !strings.Contains(err.Error(), "configstore: parse custom_headers") {
		t.Errorf("missing parse-prefix: %q", err.Error())
	}
	if got != nil {
		t.Errorf("result must be nil on parse err; got: %+v", got)
	}
}

// TestMarshalAIGuardHeaders_NilMapEncodesAsNilSlice pins the
// nil-versus-empty distinction. A nil headers map MUST encode to a
// nil byte slice (NOT the JSON literal `null` and NOT `[]byte{}`)
// so the jsonb column receives SQL NULL — that lets admins
// distinguish "operator omitted headers" from "explicitly empty
// headers" in DB audits.
func TestMarshalAIGuardHeaders_NilMapEncodesAsNilSlice(t *testing.T) {
	b, err := marshalAIGuardHeaders(nil)
	if err != nil {
		t.Fatalf("nil headers: %v", err)
	}
	if b != nil {
		t.Errorf("nil headers must encode to nil slice; got %v", b)
	}
}

// TestMarshalAIGuardHeaders_HappyPath covers the round-trip with a
// real header map.
func TestMarshalAIGuardHeaders_HappyPath(t *testing.T) {
	b, err := marshalAIGuardHeaders(map[string]any{"X-Tenant": "nexus"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(b) == 0 {
		t.Fatal("expected non-empty bytes")
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("round-trip: %v", err)
	}
	if got["X-Tenant"] != "nexus" {
		t.Errorf("round-trip: %+v", got)
	}
}

// TestMarshalAIGuardHeaders_UnmarshalableWraps covers the
// json.Marshal error branch — a payload containing an
// unmarshalable type (chan, func, complex) must surface the
// wrapped "configstore: marshal custom_headers:" prefix so the
// admin UI refuses the save before the SQL fires (saving a row
// with a partial / malformed jsonb would corrupt later reads).
func TestMarshalAIGuardHeaders_UnmarshalableWraps(t *testing.T) {
	hostile := map[string]any{"bad": make(chan int)}
	_, err := marshalAIGuardHeaders(hostile)
	if err == nil {
		t.Fatal("unmarshalable map must surface error")
	}
	if !strings.Contains(err.Error(), "configstore: marshal custom_headers") {
		t.Errorf("missing marshal-prefix: %q", err.Error())
	}
}

// TestSave_MarshalErrorReturnsBeforeExec covers the
// `if err != nil { return err }` branch right after
// marshalAIGuardHeaders inside Save() — when the headers map is
// unmarshalable, Save must return the wrapped marshal error
// BEFORE touching s.pool.Exec, otherwise we'd nil-deref the pool
// in callers that already validated their inputs upstream.
//
// We construct an AIGuardStore with a nil pool to assert this:
// reaching s.pool.Exec would nil-deref; reaching the marshal-err
// return does not.
func TestSave_MarshalErrorReturnsBeforeExec(t *testing.T) {
	store := &AIGuardStore{pool: nil} // would nil-deref if Save reaches Exec
	hostile := map[string]any{"bad": make(chan int)}
	err := store.Save(context.Background(), &AIGuardConfig{
		ID:            "singleton",
		BackendMode:   "configured_provider",
		CustomHeaders: hostile,
	})
	if err == nil {
		t.Fatal("expected marshal error")
	}
	if !strings.Contains(err.Error(), "marshal custom_headers") {
		t.Errorf("expected marshal-prefix; got: %v", err)
	}
}

// TestSave_AutoFillsIDOnEmpty pins the
// `if cfg.ID == "" { cfg.ID = "singleton" }` branch in Save. We
// drive it via marshalAIGuardHeaders failure too so the test never
// reaches the nil pool's Exec call; the side-effect on cfg.ID is
// what we assert.
func TestSave_AutoFillsIDOnEmpty(t *testing.T) {
	store := &AIGuardStore{pool: nil}
	cfg := &AIGuardConfig{
		ID:            "", // intentionally empty — Save must auto-fill
		BackendMode:   "configured_provider",
		CustomHeaders: map[string]any{"bad": make(chan int)},
	}
	_ = store.Save(context.Background(), cfg) // err is expected (marshal)
	if cfg.ID != "singleton" {
		t.Errorf("Save must auto-fill empty ID to 'singleton'; got %q", cfg.ID)
	}
}

// TestDefaultAIGuardConfig_PinsSchemaDefaults guards the contract
// between the Go-side fallback and the Prisma schema defaults. If
// the migration schema's defaults drift, the fallback row a fresh
// DB returns would no longer match what a seeded row returns —
// causing subtle behaviour differences right after a clean install.
func TestDefaultAIGuardConfig_PinsSchemaDefaults(t *testing.T) {
	cfg := defaultAIGuardConfig()
	if cfg.ID != "singleton" {
		t.Errorf("ID: got %q, want singleton", cfg.ID)
	}
	if cfg.BackendMode != "configured_provider" {
		t.Errorf("BackendMode: got %q, want configured_provider", cfg.BackendMode)
	}
	if cfg.TimeoutMs != 5000 {
		t.Errorf("TimeoutMs: got %d, want 5000", cfg.TimeoutMs)
	}
	if cfg.CacheTTLSeconds != 600 {
		t.Errorf("CacheTTLSeconds: got %d, want 600", cfg.CacheTTLSeconds)
	}
}
