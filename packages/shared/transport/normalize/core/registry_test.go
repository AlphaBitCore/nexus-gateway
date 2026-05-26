package core

import (
	"context"
	"errors"
	"testing"
)

type stubNormalizer struct {
	id      string
	payload NormalizedPayload
	err     error
}

func (s *stubNormalizer) ID() string { return s.id }
func (s *stubNormalizer) Normalize(_ context.Context, _ []byte, _ Meta) (NormalizedPayload, error) {
	return s.payload, s.err
}

func TestRegistry_Resolve_ExactMatch(t *testing.T) {
	r := NewRegistry()
	openai := &stubNormalizer{id: "openai-chat"}
	r.Register("openai:application/json:/v1/chat/completions", openai)
	r.Freeze()

	got := r.Resolve(Meta{
		AdapterType:  "openai",
		ContentType:  "application/json",
		EndpointPath: "/v1/chat/completions",
	})
	if got != openai {
		t.Fatalf("expected exact-match resolution to openai-chat, got %v", got)
	}
}

func TestRegistry_Resolve_FallbackChain(t *testing.T) {
	r := NewRegistry()
	provider := &stubNormalizer{id: "openai-default"}
	generic := &stubNormalizer{id: "generic-http"}
	r.Register("openai", provider)
	r.Register("*:*:*", generic)
	r.Freeze()

	t.Run("provider only", func(t *testing.T) {
		got := r.Resolve(Meta{AdapterType: "openai", ContentType: "application/json", EndpointPath: "/v1/x"})
		if got != provider {
			t.Fatalf("expected provider fallback, got %v", got)
		}
	})
	t.Run("generic fallback", func(t *testing.T) {
		got := r.Resolve(Meta{AdapterType: "unknown", ContentType: "text/plain", EndpointPath: "/anything"})
		if got != generic {
			t.Fatalf("expected generic fallback, got %v", got)
		}
	})
}

func TestRegistry_Normalize_NoMatchYieldsErrUnsupported(t *testing.T) {
	r := NewRegistry()
	r.Freeze()
	payload, err := r.Normalize(context.Background(), []byte("hi"), Meta{AdapterType: "novendor"})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported, got %v", err)
	}
	if payload.Kind != KindUnsupported {
		t.Fatalf("expected KindUnsupported, got %v", payload.Kind)
	}
	if payload.NormalizeVersion != SchemaVersion {
		t.Fatalf("expected NormalizeVersion to default to %q, got %q", SchemaVersion, payload.NormalizeVersion)
	}
}

func TestRegistry_Normalize_VersionDefaulted(t *testing.T) {
	r := NewRegistry()
	r.Register("foo", &stubNormalizer{
		id:      "foo",
		payload: NormalizedPayload{Kind: KindAIChat},
	})
	r.Freeze()
	payload, err := r.Normalize(context.Background(), nil, Meta{AdapterType: "foo"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if payload.NormalizeVersion != SchemaVersion {
		t.Fatalf("expected version defaulted to %q, got %q", SchemaVersion, payload.NormalizeVersion)
	}
}

func TestRegistry_RegisterPanicsOnFrozen(t *testing.T) {
	r := NewRegistry()
	r.Freeze()
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on Register after Freeze")
		}
	}()
	r.Register("foo", &stubNormalizer{})
}

func TestRegistry_RegisterPanicsOnDuplicate(t *testing.T) {
	r := NewRegistry()
	r.Register("foo", &stubNormalizer{})
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on duplicate Register")
		}
	}()
	r.Register("foo", &stubNormalizer{})
}

func TestKindIsAI(t *testing.T) {
	cases := map[Kind]bool{
		KindAIChat:      true,
		KindAIEmbedding: true,
		KindHTTPJSON:    false,
		KindHTTPBinary:  false,
		KindUnsupported: false,
	}
	for k, want := range cases {
		if got := k.IsAI(); got != want {
			t.Errorf("Kind(%q).IsAI() = %v, want %v", k, got, want)
		}
	}
}
