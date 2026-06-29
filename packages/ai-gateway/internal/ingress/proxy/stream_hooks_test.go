package proxy

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	streampolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming/policy"
)

// TestStreamHooksStage_ResponseHooksActiveGate locks the stream-entry probe that
// drives BOTH the HoldBack drop and the PreHook-normalize skip. When the probe
// proves no executable response-stage rule exists, responseHooksActive=false so
// the live pipeline omits the per-checkpoint Registry normalize (and its
// raw-accumulating TeeReader) — the load-bearing SSE hot-path saving. When the
// probe CANNOT prove absence (no HookConfigCache wired) the gate stays
// conservatively true so a real hook is never starved of its normalized input.
//
// The "executable response rule present → active=true" branch is the
// pre-existing install path (unchanged by this gate) and is exercised by the
// hook execution + streaming smoke suites, which need a registered plugin
// implementation to make BuildPipeline return a non-nil pipeline.
func TestStreamHooksStage_ResponseHooksActiveGate(t *testing.T) {
	tests := []struct {
		name            string
		noCache         bool // deps.HookConfigCache unwired → cannot probe
		alsoRequestHook bool // a request hook must NOT make the response gate active
		wantActive      bool
	}{
		{name: "no response rule: skip prehook", wantActive: false},
		{name: "request hook only: still no response gate", alsoRequestHook: true, wantActive: false},
		{name: "no hook cache: conservative default keeps prehook", noCache: true, wantActive: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deps := &Deps{}
			if !tt.noCache {
				var cfgs []hookcore.HookConfig
				if tt.alsoRequestHook {
					cfgs = append(cfgs, enabledHook("request"))
				}
				deps.HookConfigCache = hookCacheWith(t, cfgs...)
			}
			s := &streamState{
				h:      &Handler{deps: deps},
				r:      httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil),
				logger: slog.Default(),
			}

			if ok := (streamHooksStage{s: s}).run(); !ok {
				t.Fatalf("streamHooksStage.run() returned false")
			}
			if s.responseHooksActive != tt.wantActive {
				t.Errorf("responseHooksActive = %v, want %v", s.responseHooksActive, tt.wantActive)
			}
			if s.hookRunner == nil {
				t.Errorf("hookRunner must always be wired (refusal path depends on it)")
			}
		})
	}
}

// TestStreamHooksStage_ProbeError_ForcesBuffer pins the fail-closed routing
// backstop. When a FAIL-CLOSED response hook is UNBUILDABLE (the only condition
// that errors BuildPipeline), the stream-entry probe errors. Because the live path
// is audit-only (B1) and can no longer enforce in-stream, the stage must force the
// request to BUFFER (responseEnforcingBlock=true) so redactCanonicalBuffer re-runs
// the build, hits the same error, and fails closed — never leaving a fail-closed
// hook to silently fail OPEN on a live/passthrough stream.
func TestStreamHooksStage_ProbeError_ForcesBuffer(t *testing.T) {
	deps := &Deps{HookConfigCache: hookCacheWith(t, hookcore.HookConfig{
		ID:               "h-resp-fc",
		Stage:            "response",
		Enabled:          true,
		FailBehavior:     "fail-closed",
		ImplementationID: "nonexistent-impl-forces-build-error",
	})}
	s := &streamState{
		h:      &Handler{deps: deps},
		r:      httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil),
		logger: slog.Default(),
	}

	if ok := (streamHooksStage{s: s}).run(); !ok {
		t.Fatalf("streamHooksStage.run() returned false")
	}
	if !s.responseEnforcingBlock {
		t.Error("an unbuildable fail-closed response hook must force buffer (responseEnforcingBlock=true) — otherwise it silently fails OPEN on the audit-only live path")
	}
}

// TestStreamShapeStage_EnforcingRoutingOverridesAdminMode pins that an enforcing
// response scope overrides the admin streaming-mode knob so it never lands on the
// audit-only live/passthrough path. Block always buffers; redact arms Model A under
// chunked_async but FALLS BACK to buffer under passthrough (raw forwarding can't
// honor a redact scope — the regression GATE-B1 caught); a non-enforcing scope
// keeps the admin mode.
func TestStreamShapeStage_EnforcingRoutingOverridesAdminMode(t *testing.T) {
	tests := []struct {
		name       string
		mode       streampolicy.Mode
		redact     bool
		block      bool
		wantMode   streampolicy.Mode
		wantModelA bool
	}{
		{name: "redact + passthrough → buffer (no raw PII leak)", mode: streampolicy.ModePassThrough, redact: true, wantMode: streampolicy.ModeBufferFullBlock},
		{name: "redact + chunked_async → Model A", mode: streampolicy.ModeChunkedAsync, redact: true, wantMode: streampolicy.ModeChunkedAsync, wantModelA: true},
		{name: "redact + buffer_full_block → buffer (already redacts)", mode: streampolicy.ModeBufferFullBlock, redact: true, wantMode: streampolicy.ModeBufferFullBlock},
		{name: "block + passthrough → buffer", mode: streampolicy.ModePassThrough, block: true, wantMode: streampolicy.ModeBufferFullBlock},
		{name: "non-enforcing + passthrough → passthrough (audit-only)", mode: streampolicy.ModePassThrough, wantMode: streampolicy.ModePassThrough},
		{name: "non-enforcing + chunked_async → chunked_async (audit-only live)", mode: streampolicy.ModeChunkedAsync, wantMode: streampolicy.ModeChunkedAsync},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &Handler{deps: &Deps{StreamingPolicy: streampolicy.NewStore(streampolicy.Policy{Mode: tt.mode})}}
			s := &streamState{
				h:                       h,
				r:                       httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil),
				responseEnforcingRedact: tt.redact,
				responseEnforcingBlock:  tt.block,
			}
			if ok := (streamShapeStage{s: s}).run(); !ok {
				t.Fatalf("streamShapeStage.run() returned false")
			}
			if s.streamMode != tt.wantMode {
				t.Errorf("streamMode = %q, want %q", s.streamMode, tt.wantMode)
			}
			if s.modelAArmed != tt.wantModelA {
				t.Errorf("modelAArmed = %v, want %v", s.modelAArmed, tt.wantModelA)
			}
		})
	}
}
