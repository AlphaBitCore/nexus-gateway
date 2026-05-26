package normalize

import (
	"context"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/codecs"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/prometheus/client_golang/prometheus"
)

// Package-level aliases so white-box tests in package normalize can reference
// sub-package types without fully qualifying every occurrence.
type (
	NormalizedPayload = core.NormalizedPayload
	Message           = core.Message
	ContentBlock      = core.ContentBlock
	TransformSpan     = core.TransformSpan
	Meta              = core.Meta
)

const (
	KindAIChat   = core.KindAIChat
	KindHTTPJSON = core.KindHTTPJSON
	RoleUser     = core.RoleUser
	ContentText  = core.ContentText
	SourceHook   = core.SourceHook
	ActionRedact = core.ActionRedact
)

func NewRegistry() *core.Registry                                   { return core.NewRegistry() }
func RegisterDefaultAIBuiltins(reg *core.Registry)                  { codecs.RegisterDefaultAIBuiltins(reg) }
func BuildAuditFn(reg *core.Registry, m *core.Metrics) core.AuditFn { return core.BuildAuditFn(reg, m) }
func MustRegisterPrometheus(r prometheus.Registerer, ns string) *core.Metrics {
	return core.MustRegisterPrometheus(r, ns)
}
func ApplySpans(p core.NormalizedPayload, spans []core.TransformSpan) (core.NormalizedPayload, []core.TransformSpan) {
	return core.ApplySpans(p, spans)
}

// stubNormalizer is a minimal Normalizer for use in package-level tests.
type stubNormalizer struct {
	id      string
	payload core.NormalizedPayload
	err     error
}

func (s *stubNormalizer) ID() string { return s.id }
func (s *stubNormalizer) Normalize(_ context.Context, _ []byte, _ core.Meta) (core.NormalizedPayload, error) {
	return s.payload, s.err
}

// TestBridgeMustRegisterPrometheus_NilReturnsNil exercises the bridge's
// MustRegisterPrometheus wrapper (distinct from core.MustRegisterPrometheus).
func TestBridgeMustRegisterPrometheus_NilReturnsNil(t *testing.T) {
	if got := MustRegisterPrometheus(nil, "bridge_test_nil"); got != nil {
		t.Fatalf("nil reg → expected nil, got %+v", got)
	}
}

func TestBridgeMustRegisterPrometheus_RegistersWhenNonNil(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := MustRegisterPrometheus(reg, "bridge_test_nonil_unique")
	if m == nil {
		t.Fatal("expected non-nil metrics when reg non-nil")
	}
}

// TestBridgeApplySpans exercises the bridge's ApplySpans wrapper.
func TestBridgeApplySpans_SimpleRedact(t *testing.T) {
	p := NormalizedPayload{
		Kind:     KindAIChat,
		Messages: []Message{{Role: RoleUser, Content: []ContentBlock{{Type: ContentText, Text: "hello world"}}}},
	}
	spans := []TransformSpan{{
		Source:         SourceHook,
		Action:         ActionRedact,
		ContentAddress: "messages.0.content.0",
		Start:          0, End: 5, Replacement: "[REDACTED]",
	}}
	got, skipped := ApplySpans(p, spans)
	if len(skipped) != 0 {
		t.Fatalf("unexpected skipped: %+v", skipped)
	}
	if got.Messages[0].Content[0].Text != "[REDACTED] world" {
		t.Fatalf("expected '[REDACTED] world', got %q", got.Messages[0].Content[0].Text)
	}
}
