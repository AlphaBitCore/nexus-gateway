package proxy

import (
	"context"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/semantic"
)

// liveReader is a real implementation, used to prove the normalization does
// not touch a dependency that is actually present.
type liveReader struct{}

func (liveReader) Read(context.Context, semantic.ReadRequest) (semantic.ReadResult, error) {
	return semantic.ReadResult{Outcome: "miss"}, nil
}

type liveWriter struct{}

func (liveWriter) Write(context.Context, semantic.WriteRequest) (semantic.WriteResult, error) {
	return semantic.WriteResult{}, nil
}

// The L2 seams are the ones this was found on: InitSemantic leaves Reader and
// Writer nil whenever Redis is reached through Sentinel or Cluster, the wiring
// assigns those nil pointers into interface fields, and `h.deps.SemanticReader
// == nil` in tryL2Lookup then reads false for a reader that cannot be called.
//
// Asserting on the guard expression rather than on a downstream panic is
// deliberate: the panic needs an enabled fleet policy to reach, so a test
// written against it would go green for the wrong reason the moment the policy
// lookup short-circuits.
func TestNewHandler_ATypedNilDependencyReadsAsAbsent(t *testing.T) {
	deps := &Deps{
		SemanticReader: (*semantic.Reader)(nil),
		SemanticWriter: (*semantic.Writer)(nil),
	}
	h := NewHandler(deps)

	if h.deps.SemanticReader != nil {
		t.Error("SemanticReader holds a nil *semantic.Reader but does not read as nil — tryL2Lookup's guard is defeated and the reader is called")
	}
	if h.deps.SemanticWriter != nil {
		t.Error("SemanticWriter holds a nil *semantic.Writer but does not read as nil — the write-back guard is defeated")
	}
}

func TestNewHandler_APresentDependencyIsLeftAlone(t *testing.T) {
	want := liveReader{}
	h := NewHandler(&Deps{SemanticReader: want})

	got, ok := h.deps.SemanticReader.(liveReader)
	if !ok || got != want {
		t.Fatalf("a present dependency was altered: got %#v", h.deps.SemanticReader)
	}
}

// unboxNilDeps walks every exported interface field by reflection rather than
// naming any of them, so the two cases above establish the mechanism and the
// loop covers the rest of Deps — including fields added after this was written,
// which is the part a per-field selector could never promise.
func TestUnboxNilDeps_ReportsWhatItChangedAndNothingElse(t *testing.T) {
	d := &Deps{
		SemanticReader: (*semantic.Reader)(nil),
		SemanticWriter: liveWriter{},
	}
	fixed := unboxNilDeps(d)
	if len(fixed) != 1 || fixed[0] != "SemanticReader" {
		t.Fatalf("reported %v, want exactly [SemanticReader]", fixed)
	}
	if unboxed := unboxNilDeps(&Deps{}); unboxed != nil {
		t.Errorf("a zero Deps has nothing to change, reported %v", unboxed)
	}
	if unboxNilDeps(nil) != nil {
		t.Error("a nil Deps must not panic")
	}
}
