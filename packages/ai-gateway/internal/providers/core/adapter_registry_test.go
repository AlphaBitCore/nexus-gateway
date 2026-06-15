package core

import (
	"context"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"testing"
)

// stubOpenAIAdapter implements [Adapter] with a fixed Format for
// registry-level assertions. The concrete Execute/Probe behaviour is
// irrelevant here.
type stubOpenAIAdapter struct{}

func (stubOpenAIAdapter) Format() Format { return FormatOpenAI }
func (stubOpenAIAdapter) SupportsShape(shape typology.WireShape) bool {
	return shape == typology.WireShapeOpenAIChat
}
func (stubOpenAIAdapter) Execute(context.Context, Request) (*Response, error) {
	return &Response{StatusCode: 200}, nil
}
func (stubOpenAIAdapter) Probe(context.Context, CallTarget) (*ProbeResult, error) {
	return &ProbeResult{OK: true}, nil
}
func (stubOpenAIAdapter) PrepareBody(req Request) ([]byte, []string, string, error) {
	return req.Body, nil, "", nil
}
func (stubOpenAIAdapter) ExecuteWithBody(context.Context, Request, []byte, []string, string) (*Response, error) {
	return &Response{StatusCode: 200}, nil
}

func TestRegistry_RegisterAndGet(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(stubOpenAIAdapter{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	got, ok := r.Get(FormatOpenAI)
	if !ok || got == nil {
		t.Fatalf("expected openai adapter")
	}
	if got.Format() != FormatOpenAI {
		t.Fatalf("unexpected format: %s", got.Format())
	}
}

func TestRegistry_DuplicateRejected(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(stubOpenAIAdapter{}); err != nil {
		t.Fatalf("first register: %v", err)
	}
	err := r.Register(stubOpenAIAdapter{})
	if err == nil {
		t.Fatalf("expected duplicate error")
	}
}

func TestRegistry_UnknownFormatReturnsFalse(t *testing.T) {
	r := NewRegistry()
	got, ok := r.Get(Format("does-not-exist"))
	if ok {
		t.Fatalf("unknown format must yield (nil,false), got adapter=%v", got)
	}
}

func TestRegistry_NilAdapterRejected(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(nil); err == nil {
		t.Fatalf("expected error on nil adapter")
	}
}

func TestRegistry_InvalidFormatRejected(t *testing.T) {
	r := NewRegistry()
	err := r.Register(invalidFormatAdapter{})
	if err == nil {
		t.Fatalf("expected error on invalid format")
	}
}

type invalidFormatAdapter struct{}

func (invalidFormatAdapter) Format() Format                              { return Format("bogus") }
func (invalidFormatAdapter) SupportsShape(shape typology.WireShape) bool { return false }
func (invalidFormatAdapter) Execute(context.Context, Request) (*Response, error) {
	return nil, nil
}
func (invalidFormatAdapter) Probe(context.Context, CallTarget) (*ProbeResult, error) {
	return nil, nil
}
func (invalidFormatAdapter) PrepareBody(req Request) ([]byte, []string, string, error) {
	return req.Body, nil, "", nil
}
func (invalidFormatAdapter) ExecuteWithBody(context.Context, Request, []byte, []string, string) (*Response, error) {
	return nil, nil
}

func TestRegistry_FreezeBlocksWrites(t *testing.T) {
	r := NewRegistry()
	r.Freeze()

	defer func() {
		if recover() == nil {
			t.Fatalf("expected panic on register after freeze")
		}
	}()
	_ = r.Register(stubOpenAIAdapter{})
}
