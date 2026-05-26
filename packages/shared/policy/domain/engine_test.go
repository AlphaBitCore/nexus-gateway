package domain_test

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/domain"
)

func mkExact(host string, paths ...domain.InterceptionPath) domain.InterceptionDomain {
	return domain.InterceptionDomain{
		ID:                "id-" + host,
		Name:              host,
		HostPattern:       host,
		HostMatchType:     domain.HostMatchExact,
		NetworkZone:       domain.ZonePublic,
		DefaultPathAction: domain.PathActionProcess,
		OnAdapterError:    domain.AdapterErrorFailOpen,
		Enabled:           true,
		Priority:          0,
		Paths:             paths,
	}
}

func TestEngine_MatchHost_Exact(t *testing.T) {
	e := domain.NewEngine()
	if err := e.Swap([]domain.InterceptionDomain{
		mkExact("api.openai.com"),
		mkExact("api.anthropic.com"),
	}); err != nil {
		t.Fatalf("swap: %v", err)
	}

	if d := e.MatchHost("api.openai.com"); d == nil || d.HostPattern != "api.openai.com" {
		t.Fatalf("expected match for api.openai.com, got %+v", d)
	}
	if d := e.MatchHost("api.openai.com:443"); d == nil {
		t.Fatalf("expected match when port is included")
	}
	if d := e.MatchHost("API.OPENAI.COM"); d == nil {
		t.Fatalf("expected match to be case-insensitive")
	}
	if d := e.MatchHost("evil.example.com"); d != nil {
		t.Fatalf("expected nil for non-listed host, got %+v", d)
	}
}

func TestEngine_MatchHost_GlobWildcard(t *testing.T) {
	e := domain.NewEngine()
	d := mkExact("anthropic.com")
	d.HostPattern = "*.anthropic.com"
	d.HostMatchType = domain.HostMatchGlob
	if err := e.Swap([]domain.InterceptionDomain{d}); err != nil {
		t.Fatalf("swap: %v", err)
	}

	if got := e.MatchHost("api.anthropic.com"); got == nil {
		t.Fatalf("*.anthropic.com should match api.anthropic.com")
	}
	if got := e.MatchHost("v1.api.anthropic.com"); got == nil {
		t.Fatalf("*.anthropic.com should match deeper subdomains")
	}
	if got := e.MatchHost("anthropic.com"); got == nil {
		t.Fatalf("*.anthropic.com should also match the bare domain")
	}
	if got := e.MatchHost("notanthropic.com"); got != nil {
		t.Fatalf("*.anthropic.com should not match notanthropic.com")
	}
}

func TestEngine_PriorityOrder(t *testing.T) {
	e := domain.NewEngine()
	low := mkExact("api.openai.com")
	low.Name = "low"
	low.Priority = 0

	high := mkExact("api.openai.com")
	high.ID = "id-high"
	high.Name = "high"
	high.Priority = 100

	if err := e.Swap([]domain.InterceptionDomain{high, low}); err != nil {
		t.Fatalf("swap: %v", err)
	}
	if got := e.MatchHost("api.openai.com"); got == nil || got.Name != "high" {
		t.Fatalf("expected high-priority domain to win, got %+v", got)
	}
}

func TestEngine_PathAction_DefaultAndPath(t *testing.T) {
	e := domain.NewEngine()
	d := mkExact("api.openai.com",
		domain.InterceptionPath{
			ID:          "p1",
			PathPattern: []string{"/v1/embeddings"},
			MatchType:   domain.PathMatchPrefix,
			Action:      domain.PathActionPassthrough,
		},
		domain.InterceptionPath{
			ID:          "p2",
			PathPattern: []string{"/internal/admin"},
			MatchType:   domain.PathMatchPrefix,
			Action:      domain.PathActionBlock,
		},
	)
	if err := e.Swap([]domain.InterceptionDomain{d}); err != nil {
		t.Fatalf("swap: %v", err)
	}

	matched := e.MatchHost("api.openai.com")
	if matched == nil {
		t.Fatal("host should match")
	}

	// Path matching the first interception_path: PASSTHROUGH
	if got := e.PathAction(matched, "/v1/embeddings/x"); got != domain.PathActionPassthrough {
		t.Errorf("expected PASSTHROUGH for /v1/embeddings/*, got %v", got)
	}
	// Path matching the second: DENY
	if got := e.PathAction(matched, "/internal/admin/secret"); got != domain.PathActionBlock {
		t.Errorf("expected DENY for /internal/admin/*, got %v", got)
	}
	// No path match → default (PROCESS)
	if got := e.PathAction(matched, "/v1/chat/completions"); got != domain.PathActionProcess {
		t.Errorf("expected PROCESS for unmatched path, got %v", got)
	}
}

func TestEngine_AllowlistEntries(t *testing.T) {
	e := domain.NewEngine()
	d := mkExact("api.openai.com")
	d.HostPattern = "api.openai.com"
	if err := e.Swap([]domain.InterceptionDomain{d}); err != nil {
		t.Fatalf("swap: %v", err)
	}
	got := e.AllowlistEntries()
	if len(got) != 1 || got[0] != "api.openai.com:443" {
		t.Errorf("expected [api.openai.com:443], got %v", got)
	}
}

func TestEngine_BadRegex_Rejects(t *testing.T) {
	e := domain.NewEngine()
	bad := mkExact("[invalid")
	bad.HostMatchType = domain.HostMatchRegex
	bad.HostPattern = "[invalid"
	good := mkExact("api.openai.com")

	// First load a valid snapshot.
	if err := e.Swap([]domain.InterceptionDomain{good}); err != nil {
		t.Fatalf("initial swap: %v", err)
	}

	// Try to swap in a bad regex; expect rejection AND previous snapshot preserved.
	if err := e.Swap([]domain.InterceptionDomain{bad, good}); err == nil {
		t.Fatalf("expected swap with invalid regex to error")
	}
	if d := e.MatchHost("api.openai.com"); d == nil {
		t.Errorf("previous snapshot should be preserved after rejected swap")
	}
}

func TestEngine_PathAction_NilDomain(t *testing.T) {
	e := domain.NewEngine()
	if got := e.PathAction(nil, "/anything"); got != domain.PathActionProcess {
		t.Errorf("nil domain should default to PROCESS, got %v", got)
	}
}
