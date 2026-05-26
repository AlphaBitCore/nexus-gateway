package compliance

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/sync/shadow"
	hooks "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestApplyDomainsShadowState_HappyPath(t *testing.T) {
	p := NewAgentPipeline(silentLogger())
	if sz := p.Snapshot().Size(); sz != 0 {
		t.Fatalf("precondition: expected empty snapshot, got size=%d", sz)
	}

	payload := map[string]any{
		"interceptionDomains": []shadow.InterceptionDomainDTO{
			{
				ID:                "dom-openai",
				Name:              "openai",
				HostPattern:       "api.openai.com",
				HostMatchType:     "EXACT",
				AdapterID:         "openai-compat",
				Enabled:           true,
				Priority:          100,
				DefaultPathAction: "PROCESS",
				OnAdapterError:    "FAIL_OPEN",
				NetworkZone:       "PUBLIC",
			},
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if err := p.ApplyDomainsShadowState(context.Background(), raw); err != nil {
		t.Fatalf("ApplyDomainsShadowState err=%v", err)
	}
	if sz := p.Snapshot().Size(); sz != 1 {
		t.Fatalf("expected snapshot size=1 after apply, got %d", sz)
	}
}

func TestApplyDomainsShadowState_EmptyIsNoOp(t *testing.T) {
	// Seed a snapshot then verify empty-payload cases leave it untouched.
	p := NewAgentPipeline(silentLogger())
	seed := map[string]any{
		"interceptionDomains": []shadow.InterceptionDomainDTO{
			{
				ID:                "dom-seed",
				Name:              "seed",
				HostPattern:       "example.com",
				HostMatchType:     "EXACT",
				AdapterID:         "openai-compat",
				Enabled:           true,
				Priority:          100,
				DefaultPathAction: "PROCESS",
				OnAdapterError:    "FAIL_OPEN",
				NetworkZone:       "PUBLIC",
			},
		},
	}
	seedRaw, _ := json.Marshal(seed)
	if err := p.ApplyDomainsShadowState(context.Background(), seedRaw); err != nil {
		t.Fatalf("seed apply err=%v", err)
	}
	beforePtr := p.Snapshot()
	if beforePtr.Size() != 1 {
		t.Fatalf("seed precondition: size=%d want 1", beforePtr.Size())
	}

	cases := []struct {
		name string
		raw  string
	}{
		{"empty bytes", ""},
		{"null", "null"},
		{"empty object", "{}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := p.ApplyDomainsShadowState(context.Background(), json.RawMessage(tc.raw)); err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			after := p.Snapshot()
			if after != beforePtr {
				t.Fatal("snapshot pointer changed for no-op payload")
			}
			if after.Size() != 1 {
				t.Fatalf("expected size=1 (seed preserved); got %d", after.Size())
			}
		})
	}
}

func TestApplyDomainsShadowState_MalformedErrors(t *testing.T) {
	p := NewAgentPipeline(silentLogger())
	before := p.Snapshot()
	err := p.ApplyDomainsShadowState(context.Background(), json.RawMessage(`{"interceptionDomains":[`))
	if err == nil {
		t.Fatal("expected error for malformed json")
	}
	if p.Snapshot() != before {
		t.Fatal("snapshot must not change when parse fails")
	}
}

func TestApplyHooksShadowState_HappyPath(t *testing.T) {
	p := NewAgentPipeline(silentLogger())
	if p.Resolver().HasHooks("request") {
		t.Fatal("fresh pipeline should have no request hooks")
	}

	payload := map[string]any{
		"hookConfigs": []hooks.HookConfig{
			{
				ID:               "hook-pii",
				ImplementationID: "pii-detector",
				Name:             "pii",
				Priority:         10,
				Enabled:          true,
				Stage:            "request",
				FailBehavior:     "fail-open",
			},
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if err := p.ApplyHooksShadowState(context.Background(), raw); err != nil {
		t.Fatalf("ApplyHooksShadowState err=%v", err)
	}
	if !p.Resolver().HasHooks("request") {
		t.Fatal("resolver should expose the request-stage hook after Apply")
	}
}

func TestApplyHooksShadowState_EmptyIsNoOp(t *testing.T) {
	// Seed a real config so we can observe whether subsequent no-op
	// payloads reset the resolver state.
	p := NewAgentPipeline(silentLogger())
	seed := map[string]any{
		"hookConfigs": []hooks.HookConfig{
			{ID: "seed", ImplementationID: "pii-detector", Name: "seed",
				Stage: "request", Enabled: true, FailBehavior: "fail-open"},
		},
	}
	seedRaw, _ := json.Marshal(seed)
	if err := p.ApplyHooksShadowState(context.Background(), seedRaw); err != nil {
		t.Fatalf("seed err: %v", err)
	}
	if !p.Resolver().HasHooks("request") {
		t.Fatal("seed: expected request hooks present")
	}

	cases := []struct {
		name string
		raw  string
	}{
		{"empty bytes", ""},
		{"null", "null"},
		{"empty object", "{}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := p.ApplyHooksShadowState(context.Background(), json.RawMessage(tc.raw)); err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if !p.Resolver().HasHooks("request") {
				t.Fatal("no-op payload should not blank seeded hooks")
			}
		})
	}
}

func TestApplyHooksShadowState_EmptyListIsAuthoritative(t *testing.T) {
	// Seed a non-empty resolver first so we can verify the cleared state
	// after an authoritative empty-list payload.
	p := NewAgentPipeline(silentLogger())
	seed := map[string]any{
		"hookConfigs": []hooks.HookConfig{
			{ID: "seed", ImplementationID: "pii-detector", Name: "seed",
				Stage: "request", Enabled: true, FailBehavior: "fail-open"},
		},
	}
	seedRaw, _ := json.Marshal(seed)
	if err := p.ApplyHooksShadowState(context.Background(), seedRaw); err != nil {
		t.Fatalf("seed err: %v", err)
	}
	if !p.Resolver().HasHooks("request") {
		t.Fatal("seed: expected request hooks present")
	}

	// Explicit empty list — authoritative zero, must clear the
	// resolver, NOT be treated as no-op.
	if err := p.ApplyHooksShadowState(context.Background(), json.RawMessage(`{"hookConfigs":[]}`)); err != nil {
		t.Fatalf("empty-list apply err: %v", err)
	}
	if p.Resolver().HasHooks("request") {
		t.Fatal("explicit empty list should clear the resolver hooks (authoritative zero)")
	}
}

func TestApplyHooksShadowState_MalformedErrors(t *testing.T) {
	p := NewAgentPipeline(silentLogger())
	before := p.Resolver()
	err := p.ApplyHooksShadowState(context.Background(), json.RawMessage(`{"hookConfigs":[`))
	if err == nil {
		t.Fatal("expected error for malformed json")
	}
	if p.Resolver() != before {
		t.Fatal("resolver must not change when parse fails")
	}
}

func TestApplyDomainsShadowState_EmptyDomainListReplacesSnapshot(t *testing.T) {
	// An explicit empty list in a well-formed payload is authoritative:
	// it replaces the snapshot with one of size 0. Only the "{}" /
	// "null" / "" shapes are treated as no-op.
	p := NewAgentPipeline(silentLogger())
	seed := map[string]any{
		"interceptionDomains": []shadow.InterceptionDomainDTO{
			{
				ID:                "dom-seed",
				Name:              "seed",
				HostPattern:       "example.com",
				HostMatchType:     "EXACT",
				AdapterID:         "openai-compat",
				Enabled:           true,
				Priority:          100,
				DefaultPathAction: "PROCESS",
				OnAdapterError:    "FAIL_OPEN",
				NetworkZone:       "PUBLIC",
			},
		},
	}
	seedRaw, _ := json.Marshal(seed)
	if err := p.ApplyDomainsShadowState(context.Background(), seedRaw); err != nil {
		t.Fatalf("seed err: %v", err)
	}
	if err := p.ApplyDomainsShadowState(context.Background(), json.RawMessage(`{"interceptionDomains":[]}`)); err != nil {
		t.Fatalf("empty-list apply err: %v", err)
	}
	if sz := p.Snapshot().Size(); sz != 0 {
		t.Fatalf("explicit empty list should zero the snapshot; got size=%d", sz)
	}
}
