package pipeline

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/redact"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// redetectStubHook emits a redact span and exposes RedetectText — the
// pii-detector shape, minus the patterns.
type redetectStubHook struct {
	core.AnyEndpointAnyModality
	marker  string
	noSpans bool
}

func (h *redetectStubHook) Execute(_ context.Context, _ *core.HookInput) (*core.HookResult, error) {
	if h.noSpans {
		return &core.HookResult{Decision: core.Approve}, nil
	}
	return &core.HookResult{
		Decision: core.Approve,
		TransformSpans: []normalize.TransformSpan{
			{Source: normalize.SourceHook, SourceID: "email", Action: normalize.ActionRedact, ContentAddress: "messages.0.content.0", Start: 0, End: 5, Replacement: "[R]"},
		},
		StorageAction: core.StorageRedact,
	}, nil
}

func (h *redetectStubHook) RedetectText(text string, ruleIDs []string) []redact.Match {
	for _, id := range ruleIDs {
		if id != "email" {
			continue
		}
		if i := strings.Index(text, h.marker); i >= 0 {
			return []redact.Match{{RuleID: id, Start: i, End: i + len(h.marker), Replacement: "[R]"}}
		}
	}
	return nil
}

func TestPipeline_StampsRedetectorWhenSpansProduced(t *testing.T) {
	hks := []boundHook{
		{hook: &redetectStubHook{marker: "bob@x.io"}, config: &core.HookConfig{ID: "h1", Name: "pii", Priority: 1, FailBehavior: "fail-open"}},
		// A span-less hook without redetect capability must not block the
		// capable one from being exported.
		{hook: &stubHook{decision: core.Approve}, config: &core.HookConfig{ID: "h2", Name: "plain", Priority: 2, FailBehavior: "fail-open"}},
	}
	p := NewPipeline(hks, 5*time.Second, 30*time.Second, false, testLogger())
	result := p.Execute(context.Background(), &core.HookInput{})

	if result.Redetect == nil {
		t.Fatal("pipeline with a redetect-capable span-producing hook must stamp Redetect")
	}
	got := result.Redetect("mail bob@x.io now", []string{"email"})
	if len(got) != 1 || got[0].RuleID != "email" || got[0].Start != 5 || got[0].End != 13 {
		t.Errorf("redetector must locate the hook's match, got %v", got)
	}
	if miss := result.Redetect("nothing here", []string{"email"}); miss != nil {
		t.Errorf("no match → nil, got %v", miss)
	}
}

func TestPipeline_NoRedetectorWithoutCapableHook(t *testing.T) {
	// Spans produced (e.g. webhook / AI-guard suggestions) but no bound
	// hook can re-scan: Redetect stays nil so the audit writer degrades
	// with the structured diagnosis instead of guessing.
	hks := []boundHook{
		{hook: &spanStubHook{}, config: &core.HookConfig{ID: "h1", Name: "webhook", Priority: 1, FailBehavior: "fail-open"}},
	}
	p := NewPipeline(hks, 5*time.Second, 30*time.Second, false, testLogger())
	result := p.Execute(context.Background(), &core.HookInput{})
	if len(result.TransformSpans) == 0 {
		t.Fatal("fixture must produce spans")
	}
	if result.Redetect != nil {
		t.Error("no redetect-capable hook bound — Redetect must stay nil")
	}
}

func TestPipeline_NoRedetectorWithoutSpans(t *testing.T) {
	// A redetect-capable hook is bound, but this run matched nothing and
	// produced no spans: there is nothing for the storage rewrite to
	// re-locate, so Redetect stays nil.
	hks := []boundHook{
		{hook: &redetectStubHook{marker: "bob@x.io", noSpans: true}, config: &core.HookConfig{ID: "h1", Name: "pii", Priority: 1, FailBehavior: "fail-open"}},
	}
	p := NewPipeline(hks, 5*time.Second, 30*time.Second, false, testLogger())
	result := p.Execute(context.Background(), &core.HookInput{})
	if len(result.TransformSpans) != 0 {
		t.Fatal("fixture must produce no spans")
	}
	if result.Redetect != nil {
		t.Error("span-less run must not stamp a redetector")
	}
}

// spanStubHook emits spans but cannot re-detect (remote-suggestion shape).
type spanStubHook struct {
	core.AnyEndpointAnyModality
}

func (h *spanStubHook) Execute(_ context.Context, _ *core.HookInput) (*core.HookResult, error) {
	return &core.HookResult{
		Decision: core.Approve,
		TransformSpans: []normalize.TransformSpan{
			{Source: normalize.SourceAIGuard, SourceID: "ai-1", Action: normalize.ActionRedact, ContentAddress: "messages.0.content.0", Start: 0, End: 3, Replacement: "[A]"},
		},
		StorageAction: core.StorageRedact,
	}, nil
}
