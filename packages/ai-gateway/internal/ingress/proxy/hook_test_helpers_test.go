package proxy

import (
	"context"
	"testing"

	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
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
