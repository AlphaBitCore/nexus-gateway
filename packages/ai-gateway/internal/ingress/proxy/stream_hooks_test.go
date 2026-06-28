package proxy

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
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
		wantHoldBack    bool
	}{
		{name: "no response rule: skip prehook + drop holdback", wantActive: false, wantHoldBack: false},
		{name: "request hook only: still no response gate", alsoRequestHook: true, wantActive: false, wantHoldBack: false},
		{name: "no hook cache: conservative default keeps prehook+holdback", noCache: true, wantActive: true, wantHoldBack: true},
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
			if s.holdBack != tt.wantHoldBack {
				t.Errorf("holdBack = %v, want %v", s.holdBack, tt.wantHoldBack)
			}
			if s.hookRunner == nil {
				t.Errorf("hookRunner must always be wired (refusal path depends on it)")
			}
		})
	}
}
