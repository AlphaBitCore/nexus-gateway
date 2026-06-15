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

// --- F-0282a: matchers are sorted by Priority DESC regardless of input order

// mkPrefixPri builds an enabled PREFIX-match domain with an explicit priority,
// so two domains can both match the same host and priority decides the winner.
func mkPrefixPri(name, prefix string, priority int) domain.InterceptionDomain {
	return domain.InterceptionDomain{
		ID:                "id-" + name,
		Name:              name,
		HostPattern:       prefix,
		HostMatchType:     domain.HostMatchPrefix,
		NetworkZone:       domain.ZonePublic,
		DefaultPathAction: domain.PathActionProcess,
		OnAdapterError:    domain.AdapterErrorFailOpen,
		Enabled:           true,
		Priority:          priority,
	}
}

func TestEngine_Swap_SortsMatchersByPriorityDesc(t *testing.T) {
	// Two overlapping prefix matchers both match "api.example.com". The
	// higher-priority one must win regardless of the order Swap received them.
	// Pre-fix, MatchHost returned whichever appeared first in the input slice.
	low := mkPrefixPri("low", "api.", 1)
	high := mkPrefixPri("high", "api.exa", 100)

	for _, tc := range []struct {
		name  string
		input []domain.InterceptionDomain
	}{
		{"low-first", []domain.InterceptionDomain{low, high}},
		{"high-first", []domain.InterceptionDomain{high, low}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := domain.NewEngine()
			if err := e.Swap(tc.input); err != nil {
				t.Fatalf("swap: %v", err)
			}
			got := e.MatchHost("api.example.com")
			if got == nil {
				t.Fatal("expected a match")
				return
			}
			if got.Name != "high" {
				t.Errorf("highest-priority domain must win regardless of input order; got %q", got.Name)
			}
		})
	}
}

func TestEngine_Swap_EqualPriorityStableOrder(t *testing.T) {
	// Equal-priority domains keep their input (DB query) order so resolution
	// is deterministic across reloads.
	first := mkPrefixPri("first", "api.", 5)
	second := mkPrefixPri("second", "api.", 5)
	e := domain.NewEngine()
	if err := e.Swap([]domain.InterceptionDomain{first, second}); err != nil {
		t.Fatalf("swap: %v", err)
	}
	got := e.MatchHost("api.example.com")
	if got == nil || got.Name != "first" {
		t.Errorf("equal-priority should keep input order (first wins); got %+v", got)
	}
}

// TestEngine_ShouldFailClosed is the safety-critical contract for the
// on_adapter_error runtime consumer: a flow is refused ONLY when a domain
// matches AND that domain is explicitly FAIL_CLOSED. Every other shape
// (unmatched host, FAIL_OPEN domain, blank/unset behavior, nil engine)
// preserves the historical fail-open passthrough.
func TestEngine_ShouldFailClosed(t *testing.T) {
	failClosed := mkExact("closed.example.com")
	failClosed.OnAdapterError = domain.AdapterErrorFailClosed

	failOpen := mkExact("open.example.com") // mkExact sets FAIL_OPEN

	unset := mkExact("unset.example.com")
	unset.OnAdapterError = "" // blank must NOT fail closed

	e := domain.NewEngine()
	if err := e.Swap([]domain.InterceptionDomain{failClosed, failOpen, unset}); err != nil {
		t.Fatalf("swap: %v", err)
	}

	cases := []struct {
		name string
		host string
		want bool
	}{
		{"matched FAIL_CLOSED domain refuses", "closed.example.com", true},
		{"matched FAIL_CLOSED with port still refuses", "closed.example.com:443", true},
		{"matched FAIL_OPEN domain passes", "open.example.com", false},
		{"matched but unset on_adapter_error passes (never default closed)", "unset.example.com", false},
		{"unmatched host passes (system/DNS flows keep working)", "dns.unmatched.example", false},
		{"empty host passes", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := e.ShouldFailClosed(tc.host); got != tc.want {
				t.Fatalf("ShouldFailClosed(%q) = %v, want %v", tc.host, got, tc.want)
			}
		})
	}
}

// TestEngine_ShouldFailClosed_NilEngine guards the nil-safe path: callers
// (cfg.DomainEngine / deps.DomainEngine may be nil) must observe fail-open.
func TestEngine_ShouldFailClosed_NilEngine(t *testing.T) {
	var e *domain.Engine
	if e.ShouldFailClosed("anything.example.com") {
		t.Fatal("nil engine must fail open (return false)")
	}
	// A constructed-but-empty engine matches no host → fail open.
	if domain.NewEngine().ShouldFailClosed("anything.example.com") {
		t.Fatal("empty engine must fail open (return false)")
	}
}
