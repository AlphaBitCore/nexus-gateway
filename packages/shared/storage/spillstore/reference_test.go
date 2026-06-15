package spillstore

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// stubFilter is a SweepFilter whose KeepReferenced behaviour the test controls.
type stubFilter struct {
	gotKeys    []string
	referenced map[string]bool
	err        error
}

func (s *stubFilter) KeepReferenced(_ context.Context, candidateKeys []string) (map[string]bool, error) {
	s.gotKeys = candidateKeys
	return s.referenced, s.err
}

// TestResolveReferenced_NilFilter returns an empty (non-nil) set without
// invoking any filter — the pure age-based sweep path.
func TestResolveReferenced_NilFilter(t *testing.T) {
	got, err := ResolveReferenced(context.Background(), nil, []string{"a", "b"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("returned map must never be nil")
	}
	if len(got) != 0 {
		t.Errorf("expected empty set, got %v", got)
	}
}

// TestResolveReferenced_NoCandidates short-circuits before calling the filter
// so an empty sweep makes no DB round-trip.
func TestResolveReferenced_NoCandidates(t *testing.T) {
	f := &stubFilter{referenced: map[string]bool{"x": true}}
	got, err := ResolveReferenced(context.Background(), f, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty set, got %v", got)
	}
	if f.gotKeys != nil {
		t.Errorf("filter must not be invoked with no candidates; got keys %v", f.gotKeys)
	}
}

// TestResolveReferenced_PassesCandidatesAndReturnsSet forwards the full
// candidate set to the filter and returns its referenced map verbatim.
func TestResolveReferenced_PassesCandidatesAndReturnsSet(t *testing.T) {
	want := map[string]bool{"a": true, "c": true}
	f := &stubFilter{referenced: want}
	candidates := []string{"a", "b", "c"}
	got, err := ResolveReferenced(context.Background(), f, candidates)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(f.gotKeys, candidates) {
		t.Errorf("filter received %v, want %v", f.gotKeys, candidates)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestResolveReferenced_FilterError propagates the error and returns nil so the
// caller aborts the sweep (fail-safe per F-0187 — never delete on a failed
// reference check).
func TestResolveReferenced_FilterError(t *testing.T) {
	sentinel := errors.New("db down")
	f := &stubFilter{err: sentinel}
	got, err := ResolveReferenced(context.Background(), f, []string{"a"})
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want %v", err, sentinel)
	}
	if got != nil {
		t.Errorf("on error the set must be nil, got %v", got)
	}
}

// TestResolveReferenced_NilMapFromFilter normalizes a nil map (filter found
// nothing referenced) into an empty, indexable set.
func TestResolveReferenced_NilMapFromFilter(t *testing.T) {
	f := &stubFilter{referenced: nil}
	got, err := ResolveReferenced(context.Background(), f, []string{"a"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("returned map must never be nil")
	}
	if got["a"] {
		t.Errorf("no key should be referenced, got %v", got)
	}
}
