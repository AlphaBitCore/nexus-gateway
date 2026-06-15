package core

import (
	"bytes"
	"compress/gzip"
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

// sniffStub is a stub Normalizer that also implements Sniffer. It counts
// LooksLike / Normalize invocations so tests can pin how often the
// registry's walks actually consult it, and records the raw bytes the
// last Normalize call received.
type sniffStub struct {
	id         string
	payload    NormalizedPayload
	err        error
	looks      bool
	looksCalls int
	normCalls  int
	lastRaw    []byte
}

func (s *sniffStub) ID() string { return s.id }
func (s *sniffStub) Normalize(_ context.Context, raw []byte, _ Meta) (NormalizedPayload, error) {
	s.normCalls++
	s.lastRaw = raw
	return s.payload, s.err
}
func (s *sniffStub) LooksLike(_ []byte, _ Meta) bool {
	s.looksCalls++
	return s.looks
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

func TestRegistry_Resolve_NoMatchReturnsNil(t *testing.T) {
	r := NewRegistry()
	r.Register("openai", &stubNormalizer{id: "openai-chat"})
	r.Freeze()
	if got := r.Resolve(Meta{AdapterType: "unknown-host", ContentType: "text/plain"}); got != nil {
		t.Fatalf("expected nil for unmatched meta, got %v", got)
	}
}

func TestRegistry_Normalize_DecompressesGzipBeforeTiers(t *testing.T) {
	// Producers sometimes capture bodies before transport decompression;
	// the registry must hand normalizers the DECOMPRESSED bytes.
	plain := []byte(`{"hello":"world"}`)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(plain); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}

	n := &sniffStub{id: "echo", payload: NormalizedPayload{Kind: KindHTTPJSON}}
	r := NewRegistry()
	r.Register("foo", n)
	r.Freeze()

	payload, err := r.Normalize(context.Background(), buf.Bytes(), Meta{AdapterType: "foo"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if payload.Kind != KindHTTPJSON {
		t.Fatalf("expected claimed payload, got kind %q", payload.Kind)
	}
	if !bytes.Equal(n.lastRaw, plain) {
		t.Fatalf("normalizer saw %q, want decompressed %q", n.lastRaw, plain)
	}
}

func TestRegistry_RegisterSniffer_PanicsOnFrozen(t *testing.T) {
	r := NewRegistry()
	r.Freeze()
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on RegisterSniffer after Freeze")
		}
	}()
	r.RegisterSniffer(&sniffStub{id: "late"})
}

func TestRegistry_RegisterSniffer_PanicsOnNonSniffer(t *testing.T) {
	r := NewRegistry()
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic when normalizer lacks LooksLike")
		}
	}()
	r.RegisterSniffer(&stubNormalizer{id: "no-sniff"})
}

func TestRegistry_Sniff_ClaimsKeyMissedTraffic(t *testing.T) {
	// Capture-side meta carries a host name in AdapterType, so no key
	// resolves; the sniffer must still land the body on its codec.
	n := &sniffStub{
		id:      "anthropic-messages",
		payload: NormalizedPayload{Kind: KindAIChat, Protocol: "anthropic-messages", Confidence: 1.0},
		looks:   true,
	}
	r := NewRegistry()
	r.Register("anthropic", n)
	r.RegisterSniffer(n)
	r.Freeze()

	payload, err := r.Normalize(context.Background(), []byte("event: message_start\n"), Meta{AdapterType: "api.anthropic.com"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if payload.Kind != KindAIChat || payload.Protocol != "anthropic-messages" {
		t.Fatalf("sniff claim produced kind=%q protocol=%q", payload.Kind, payload.Protocol)
	}
	if n.normCalls != 1 {
		t.Fatalf("Normalize called %d times, want exactly 1 (sniff walk only)", n.normCalls)
	}
}

func TestRegistry_Sniff_NotOfferedWhenLooksLikeFalse(t *testing.T) {
	n := &sniffStub{id: "shy", payload: NormalizedPayload{Kind: KindAIChat}, looks: false}
	generic := &stubNormalizer{id: "generic-http", payload: NormalizedPayload{Kind: KindHTTPText}}
	r := NewRegistry()
	r.RegisterSniffer(n)
	r.Register("*:*:*", generic)
	r.Freeze()

	payload, err := r.Normalize(context.Background(), []byte("plain"), Meta{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if payload.Kind != KindHTTPText {
		t.Fatalf("expected Tier-3 catch-all, got %q", payload.Kind)
	}
	if n.looksCalls != 1 || n.normCalls != 0 {
		t.Fatalf("looksCalls=%d normCalls=%d; LooksLike=false must not trigger Normalize", n.looksCalls, n.normCalls)
	}
}

func TestRegistry_Sniff_LyingSnifferFallsThroughToTier2(t *testing.T) {
	// A sniffer whose LooksLike matched but whose Normalize rejects the
	// body must not block the walk — Tier 2 still gets its chance.
	liar := &sniffStub{id: "liar", err: ErrUnsupported, looks: true}
	tier2 := &stubNormalizer{id: "pattern-extract", payload: NormalizedPayload{Kind: KindAIChat, Protocol: "pattern-extract"}}
	r := NewRegistry()
	r.RegisterSniffer(liar)
	r.RegisterTier2(tier2)
	r.Freeze()

	payload, err := r.Normalize(context.Background(), []byte(`{"x":1}`), Meta{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if payload.Protocol != "pattern-extract" {
		t.Fatalf("expected Tier-2 claim after lying sniffer, got protocol %q", payload.Protocol)
	}
	if liar.normCalls != 1 {
		t.Fatalf("lying sniffer Normalize called %d times, want 1", liar.normCalls)
	}
}

func TestRegistry_Sniff_SkipsNormalizerAlreadyTriedByKey(t *testing.T) {
	// When the keyed Tier-1 walk already ran a normalizer (and it
	// declined), the sniff walk must not run the same instance again.
	n := &sniffStub{id: "dup", err: ErrUnsupported, looks: true}
	r := NewRegistry()
	r.Register("foo", n)
	r.RegisterSniffer(n)
	r.Freeze()

	_, err := r.Normalize(context.Background(), []byte("body"), Meta{AdapterType: "foo"})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported, got %v", err)
	}
	if n.normCalls != 1 {
		t.Fatalf("Normalize called %d times, want 1 (keyed walk only)", n.normCalls)
	}
	if n.looksCalls != 0 {
		t.Fatalf("LooksLike called %d times, want 0 (already tried by key)", n.looksCalls)
	}
}

func TestRegistry_Sniff_DedupeRegistration(t *testing.T) {
	// Registering the same instance twice must not double-walk it.
	n := &sniffStub{id: "twice", err: ErrUnsupported, looks: true}
	r := NewRegistry()
	r.RegisterSniffer(n)
	r.RegisterSniffer(n)
	r.Freeze()

	_, err := r.Normalize(context.Background(), []byte("body"), Meta{})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported, got %v", err)
	}
	if n.normCalls != 1 {
		t.Fatalf("Normalize called %d times, want 1 after dedupe", n.normCalls)
	}
}

func TestRegistry_Sniff_BelowThresholdKeepsBestPartial(t *testing.T) {
	// A sniff claim below the confidence threshold soft-falls-through;
	// with no later tier claiming, its payload returns as bestPartial.
	n := &sniffStub{
		id:      "partial",
		payload: NormalizedPayload{Kind: KindAIChat, Protocol: "partial-codec", Confidence: 0.5},
		looks:   true,
	}
	r := NewRegistry()
	r.RegisterSniffer(n)
	r.Freeze()

	payload, err := r.Normalize(context.Background(), []byte("body"), Meta{})
	if err != nil {
		t.Fatalf("expected bestPartial with nil error, got %v", err)
	}
	if payload.Protocol != "partial-codec" || payload.Confidence != 0.5 {
		t.Fatalf("expected bestPartial from sniff walk, got protocol=%q confidence=%v", payload.Protocol, payload.Confidence)
	}
}

func TestRegistry_Sniff_HardErrorDemotedToFallThrough(t *testing.T) {
	// A LooksLike byte probe is weak evidence: a hard parse error from a
	// sniff-matched normalizer (foreign protocol carrying the probed
	// marker, truncated body) must NOT abort the walk — later tiers still
	// get the bytes and the row ends in a structural projection.
	hardErr := errors.New("truncated frame")
	n := &sniffStub{
		id:      "broken",
		payload: NormalizedPayload{Kind: KindAIChat, Protocol: "broken-codec"},
		err:     hardErr,
		looks:   true,
	}
	generic := &stubNormalizer{id: "generic-http", payload: NormalizedPayload{Kind: KindHTTPText, Protocol: "generic-http"}}
	r := NewRegistry()
	r.RegisterSniffer(n)
	r.Register("*:*:*", generic)
	r.Freeze()

	payload, err := r.Normalize(context.Background(), []byte("body"), Meta{})
	if err != nil {
		t.Fatalf("sniff hard error must not propagate; got %v", err)
	}
	if payload.Kind != KindHTTPText || payload.Protocol != "generic-http" {
		t.Fatalf("expected Tier-3 claim after demoted sniff error, got kind=%q protocol=%q", payload.Kind, payload.Protocol)
	}
	if n.normCalls != 1 {
		t.Fatalf("sniffer Normalize called %d times, want 1", n.normCalls)
	}
}

func TestRegistry_Sniff_HardErrorPayloadWithConfidenceKeptAsBestPartial(t *testing.T) {
	// When the errored sniff payload carries explicit confidence and no
	// later tier claims, that partial decode is still the best available
	// audit row — it must come back with a nil error.
	n := &sniffStub{
		id:      "half-decoded",
		payload: NormalizedPayload{Kind: KindAIChat, Protocol: "half-codec", Confidence: 0.4},
		err:     errors.New("frame 7 cut off"),
		looks:   true,
	}
	r := NewRegistry()
	r.RegisterSniffer(n)
	r.Freeze()

	payload, err := r.Normalize(context.Background(), []byte("body"), Meta{})
	if err != nil {
		t.Fatalf("expected bestPartial with nil error, got %v", err)
	}
	if payload.Protocol != "half-codec" || payload.Confidence != 0.4 {
		t.Fatalf("expected errored sniff payload retained as bestPartial, got protocol=%q confidence=%v", payload.Protocol, payload.Confidence)
	}
}

func TestRegistry_Sniff_HardErrorZeroConfidenceNotRetained(t *testing.T) {
	// An errored sniff payload WITHOUT explicit confidence must not be
	// retained: the zero→1.0 promotion applies only to successful
	// decodes, so with nothing else registered the walk admits
	// ErrUnsupported instead of returning a broken payload.
	n := &sniffStub{
		id:      "broken-zero",
		payload: NormalizedPayload{Kind: KindAIChat, Protocol: "broken-codec"},
		err:     errors.New("corrupt"),
		looks:   true,
	}
	r := NewRegistry()
	r.RegisterSniffer(n)
	r.Freeze()

	payload, err := r.Normalize(context.Background(), []byte("body"), Meta{})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported, got %v", err)
	}
	if payload.Kind != KindUnsupported {
		t.Fatalf("expected KindUnsupported, got %q", payload.Kind)
	}
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
