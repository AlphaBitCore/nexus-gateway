package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	cache "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/requestcontext"
	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// hookCacheWith builds a started HookConfigCache seeded with the given configs
// (pure push mode, ttl=0) so HasHooks(stage) reflects them deterministically.
func hookCacheWith(t *testing.T, cfgs ...hookcore.HookConfig) *pipeline.HookConfigCache {
	t.Helper()
	c := pipeline.NewHookConfigCache(
		func(context.Context) ([]hookcore.HookConfig, error) { return cfgs, nil },
		hookcore.NewHookRegistry(),
		0,
		nil,
	)
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("hook cache start: %v", err)
	}
	return c
}

func enabledHook(stage string) hookcore.HookConfig {
	return hookcore.HookConfig{ID: "h-" + stage, Stage: stage, Enabled: true}
}

// rctxComputed returns a RequestContext whose canonical is already materialized
// (computed=true) when computed is true, else an empty context whose canonical
// was never pulled (computed=false). This is the request-direction gate input.
func rctxComputed(computed bool) *requestcontext.RequestContext {
	b := requestcontext.NewBuilder().WithRawBody([]byte(`{"model":"m"}`))
	if computed {
		b = b.WithNormalized(&normcore.NormalizedPayload{Protocol: "openai", Kind: normcore.KindAIChat})
	}
	return b.Build()
}

// TestStampNormalizeGate locks the per-direction lazy-audit-normalize gate. The
// normalized projection is never persisted for its own sake (the control plane
// recomputes it at view time from the stored raw body), so a direction defers
// UNLESS a genuine write-time consumer needs the projection:
//   - request deferred ⟺ flag on AND the canonical was NOT materialized (no smart
//     match / cache reuse).
//   - response deferred ⟺ flag on AND cache off.
//
// Compliance hooks do NOT force write-time normalize: redaction applies to the
// payload (the stored raw body is rewritten), so the view-time recompute is
// PII-safe without a stored projection.
func TestStampNormalizeGate(t *testing.T) {
	type want struct{ skipReq, skipResp bool }
	tests := []struct {
		name          string
		flagOn        bool
		canonComputed bool
		reqHook       bool
		respHook      bool
		cacheEnabled  bool
		want          want
	}{
		{
			name:   "flag off: never defer (legacy byte-identical)",
			flagOn: false,
			want:   want{false, false},
		},
		{
			name:   "lazy + nothing materialized: defer both",
			flagOn: true,
			want:   want{true, true},
		},
		{
			name:          "lazy + canonical computed (smart/cache pulled it): keep request, defer response",
			flagOn:        true,
			canonComputed: true,
			want:          want{false, true},
		},
		{
			name:    "lazy + request hook only: hook does NOT force normalize, defer both",
			flagOn:  true,
			reqHook: true,
			want:    want{true, true},
		},
		{
			name:     "lazy + response hook only: hook does NOT force normalize, defer both",
			flagOn:   true,
			respHook: true,
			want:     want{true, true},
		},
		{
			name:     "lazy + both hooks, no canonical, cache off: hooks do NOT force, defer both",
			flagOn:   true,
			reqHook:  true,
			respHook: true,
			want:     want{true, true},
		},
		{
			name:         "lazy + cache enabled: defer request (no canonical), keep response",
			flagOn:       true,
			cacheEnabled: true,
			want:         want{true, false},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deps := &Deps{}
			var cfgs []hookcore.HookConfig
			if tt.reqHook {
				cfgs = append(cfgs, enabledHook("request"))
			}
			if tt.respHook {
				cfgs = append(cfgs, enabledHook("response"))
			}
			deps.HookConfigCache = hookCacheWith(t, cfgs...)
			if tt.cacheEnabled {
				c := &cache.Cache{}
				c.SetConfig(cache.ConfigSnapshot{Enabled: true})
				deps.Cache = c
			}
			h := &Handler{deps: deps, lazyAuditNormalize: tt.flagOn}
			s := &proxyState{
				h:        h,
				r:        httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil),
				rec:      &audit.Record{},
				rctxFull: rctxComputed(tt.canonComputed),
			}
			s.stampNormalizeGate()
			if s.rec.SkipRequestNormalize != tt.want.skipReq {
				t.Errorf("SkipRequestNormalize = %v, want %v", s.rec.SkipRequestNormalize, tt.want.skipReq)
			}
			if s.rec.SkipResponseNormalize != tt.want.skipResp {
				t.Errorf("SkipResponseNormalize = %v, want %v", s.rec.SkipResponseNormalize, tt.want.skipResp)
			}
		})
	}
}
