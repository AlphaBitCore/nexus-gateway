package traffic

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/interception"
)

// stubAdapter is a minimal adapter for testing.
type stubAdapter struct{ id string }

func (a *stubAdapter) ID() string                     { return a.id }
func (a *stubAdapter) Configure(map[string]any) error { return nil }
func (a *stubAdapter) ExtractRequest(_ context.Context, _ []byte, _ string) (NormalizedContent, error) {
	return NormalizedContent{}, nil
}
func (a *stubAdapter) ExtractResponse(_ context.Context, _ []byte, _ string) (NormalizedContent, error) {
	return NormalizedContent{}, nil
}
func (a *stubAdapter) ExtractStreamChunk(_ context.Context, _ []byte, _ string) (NormalizedContent, error) {
	return NormalizedContent{}, nil
}
func (a *stubAdapter) DetectRequestMeta(_ *http.Request, _ []byte) RequestMeta { return RequestMeta{} }
func (a *stubAdapter) DetectResponseUsage(_ *http.Response, _ []byte) UsageMeta {
	return UsageMeta{Status: UsageStatusNonLLM}
}
func (a *stubAdapter) RewriteRequestBody(_ context.Context, _ []byte, _ string, _ NormalizedContent) ([]byte, int, error) {
	return nil, 0, ErrRewriteUnsupported
}
func (a *stubAdapter) RewriteResponseBody(_ context.Context, _ []byte, _ string, _ NormalizedContent) ([]byte, int, error) {
	return nil, 0, ErrRewriteUnsupported
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func TestBuildDomainSnapshot_Basic(t *testing.T) {
	reg := NewAdapterRegistry("test")
	_ = reg.Register("openai-compat", func() Adapter { return &stubAdapter{id: "openai-compat"} })
	reg.Freeze()

	now := time.Now()
	domains := []interception.InterceptionDomain{
		{
			Id:                "d1",
			Name:              "openai-public",
			HostPattern:       "api.openai.com",
			HostMatchType:     interception.HostMatchTypeExact,
			AdapterId:         "openai-compat",
			Enabled:           true,
			Priority:          100,
			DefaultPathAction: interception.DefaultPathActionProcess,
			OnAdapterError:    interception.FailureActionFailOpen,
			NetworkZone:       interception.NetworkZonePublic,
			Source:            "builtin",
			CreatedAt:         now,
			UpdatedAt:         now,
		},
	}
	paths := []interception.InterceptionPath{
		{
			Id:          "p1",
			DomainId:    "d1",
			PathPattern: []string{"/v1/files", "/v1/models"},
			MatchType:   interception.PathMatchTypePrefix,
			Action:      interception.PathActionPassthrough,
			Priority:    0,
			Enabled:     true,
			CreatedAt:   now,
			UpdatedAt:   now,
		},
	}

	snap := BuildDomainSnapshot(domains, paths, reg, testLogger())

	if snap.Size() != 1 {
		t.Fatalf("expected 1 domain, got %d", snap.Size())
	}
	if _, ok := snap.ByHost["api.openai.com"]; !ok {
		t.Fatal("expected exact host in ByHost map")
	}
	if len(snap.Instances[0].Paths) != 1 {
		t.Fatalf("expected 1 path rule, got %d", len(snap.Instances[0].Paths))
	}
}

func TestDomainSnapshot_ResolveAction(t *testing.T) {
	reg := NewAdapterRegistry("test")
	_ = reg.Register("openai-compat", func() Adapter { return &stubAdapter{id: "openai-compat"} })

	now := time.Now()
	domains := []interception.InterceptionDomain{
		{
			Id:                "d1",
			Name:              "openai",
			HostPattern:       "api.openai.com",
			HostMatchType:     interception.HostMatchTypeExact,
			AdapterId:         "openai-compat",
			Enabled:           true,
			Priority:          0,
			DefaultPathAction: interception.DefaultPathActionProcess,
			OnAdapterError:    interception.FailureActionFailOpen,
			NetworkZone:       interception.NetworkZonePublic,
			CreatedAt:         now,
			UpdatedAt:         now,
		},
	}
	paths := []interception.InterceptionPath{
		{
			Id:          "p1",
			DomainId:    "d1",
			PathPattern: []string{"/v1/files"},
			MatchType:   interception.PathMatchTypePrefix,
			Action:      interception.PathActionPassthrough,
			Priority:    0,
			Enabled:     true,
			CreatedAt:   now,
			UpdatedAt:   now,
		},
	}

	snap := BuildDomainSnapshot(domains, paths, reg, testLogger())

	// Chat completions should use default PROCESS.
	_, result, pathRule := snap.ResolveAction("api.openai.com", "/v1/chat/completions")
	if result != Process {
		t.Errorf("expected Process for chat/completions, got %s", result)
	}
	if pathRule != nil {
		t.Error("expected nil pathRule for default action")
	}

	// Files should match the PASSTHROUGH rule.
	_, result, pathRule = snap.ResolveAction("api.openai.com", "/v1/files/upload")
	if result != Passthrough {
		t.Errorf("expected Passthrough for files, got %s", result)
	}
	if pathRule == nil {
		t.Error("expected non-nil pathRule for files match")
	}

	// Unknown domain should return Passthrough with no instance.
	inst, result, _ := snap.ResolveAction("api.unknown.com", "/anything")
	if inst != nil {
		t.Error("expected nil instance for unknown domain")
	}
	if result != Passthrough {
		t.Errorf("expected Passthrough for unknown domain, got %s", result)
	}
}

func TestDomainSnapshot_DisabledDomain(t *testing.T) {
	reg := NewAdapterRegistry("test")
	_ = reg.Register("openai-compat", func() Adapter { return &stubAdapter{id: "openai-compat"} })

	now := time.Now()
	domains := []interception.InterceptionDomain{
		{
			Id:            "d1",
			Name:          "disabled",
			HostPattern:   "api.openai.com",
			HostMatchType: interception.HostMatchTypeExact,
			AdapterId:     "openai-compat",
			Enabled:       false, // disabled
			CreatedAt:     now,
			UpdatedAt:     now,
		},
	}

	snap := BuildDomainSnapshot(domains, nil, reg, testLogger())
	if snap.Size() != 0 {
		t.Fatalf("expected 0 domains (disabled), got %d", snap.Size())
	}
}

func TestDomainSnapshot_UnknownAdapter(t *testing.T) {
	reg := NewAdapterRegistry("test")
	// No adapters registered.

	now := time.Now()
	domains := []interception.InterceptionDomain{
		{
			Id:            "d1",
			Name:          "test",
			HostPattern:   "api.test.com",
			HostMatchType: interception.HostMatchTypeExact,
			AdapterId:     "nonexistent",
			Enabled:       true,
			CreatedAt:     now,
			UpdatedAt:     now,
		},
	}

	snap := BuildDomainSnapshot(domains, nil, reg, testLogger())
	if snap.Size() != 0 {
		t.Fatalf("expected 0 domains (unknown adapter), got %d", snap.Size())
	}
}

func TestEmpty(t *testing.T) {
	snap := Empty()
	if snap.Size() != 0 {
		t.Fatalf("expected 0 domains, got %d", snap.Size())
	}
	inst := snap.FindInstance("anything")
	if inst != nil {
		t.Error("expected nil instance from empty snapshot")
	}
}
