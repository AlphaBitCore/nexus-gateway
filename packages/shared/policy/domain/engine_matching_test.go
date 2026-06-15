package domain_test

// engine_extra_test.go — covers the branches engine_test.go leaves on
// the table so the package reaches the ≥95% binding from
// [[unit_test_coverage_95]]. Tests assert observable behavior (matched
// domain identity, selected PathAction, allowlist contents, snapshot
// independence from the engine's live slice) rather than padding lines.

import (
	"reflect"
	"sort"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/domain"
)

// MatchHost on a freshly-constructed engine (no Swap yet) returns nil
// instead of panicking — covers the snapshot-empty branch.
func TestEngine_MatchHost_FreshEngine_NoSnapshotDomains(t *testing.T) {
	e := domain.NewEngine()
	if got := e.MatchHost("api.openai.com"); got != nil {
		t.Fatalf("fresh engine should not match anything, got %+v", got)
	}
}

// Empty / whitespace host string → nil (no domain). Covers the empty-host
// early-return in MatchHost.
func TestEngine_MatchHost_EmptyHost(t *testing.T) {
	e := domain.NewEngine()
	if err := e.Swap([]domain.InterceptionDomain{mkExact("api.openai.com")}); err != nil {
		t.Fatalf("swap: %v", err)
	}
	for _, h := range []string{"", "   ", ":", ":443"} {
		if got := e.MatchHost(h); got != nil {
			t.Errorf("host %q expected nil, got %+v", h, got)
		}
	}
}

// Host with port that has empty leading host should be treated as empty
// after the strip — guards against silent panic.
func TestEngine_MatchHost_WhitespaceTrim(t *testing.T) {
	e := domain.NewEngine()
	if err := e.Swap([]domain.InterceptionDomain{mkExact("api.openai.com")}); err != nil {
		t.Fatalf("swap: %v", err)
	}
	if got := e.MatchHost("  api.openai.com  "); got == nil {
		t.Fatalf("expected match after whitespace trim")
	}
}

func TestEngine_MatchHost_PrefixMatch(t *testing.T) {
	e := domain.NewEngine()
	d := mkExact("api.openai.com")
	d.HostPattern = "api."
	d.HostMatchType = domain.HostMatchPrefix
	if err := e.Swap([]domain.InterceptionDomain{d}); err != nil {
		t.Fatalf("swap: %v", err)
	}
	if got := e.MatchHost("api.openai.com"); got == nil {
		t.Fatalf("PREFIX 'api.' should match api.openai.com")
	}
	if got := e.MatchHost("api.anthropic.com"); got == nil {
		t.Fatalf("PREFIX 'api.' should match api.anthropic.com")
	}
	if got := e.MatchHost("static.example.com"); got != nil {
		t.Fatalf("PREFIX 'api.' must not match static.example.com, got %+v", got)
	}
}

func TestEngine_MatchHost_RegexMatch(t *testing.T) {
	e := domain.NewEngine()
	d := mkExact("openai")
	d.HostPattern = `^api\.openai\.(com|net)$`
	d.HostMatchType = domain.HostMatchRegex
	if err := e.Swap([]domain.InterceptionDomain{d}); err != nil {
		t.Fatalf("swap: %v", err)
	}
	if got := e.MatchHost("api.openai.com"); got == nil {
		t.Fatalf("regex should match api.openai.com")
	}
	if got := e.MatchHost("api.openai.net"); got == nil {
		t.Fatalf("regex should match api.openai.net")
	}
	if got := e.MatchHost("api.openai.org"); got != nil {
		t.Fatalf("regex should NOT match api.openai.org")
	}
}

// GLOB without a leading "*." falls back to literal exact-match
// semantics. Covers the trailing `return host == pat` branch of the
// GLOB case.
func TestEngine_MatchHost_GlobNonLeadingWildcard_FallsBackToLiteral(t *testing.T) {
	e := domain.NewEngine()
	d := mkExact("api.openai.com")
	d.HostPattern = "api.openai.com"
	d.HostMatchType = domain.HostMatchGlob
	if err := e.Swap([]domain.InterceptionDomain{d}); err != nil {
		t.Fatalf("swap: %v", err)
	}
	if got := e.MatchHost("api.openai.com"); got == nil {
		t.Fatalf("GLOB literal should match exact host")
	}
	if got := e.MatchHost("v1.api.openai.com"); got != nil {
		t.Fatalf("GLOB literal must not match deeper subdomain")
	}
}

// An unknown HostMatchType falls back to exact comparison (default
// branch). The DB enum is closed, but we should never panic on stale
// values pushed from a future schema.
func TestEngine_MatchHost_UnknownMatchType_FallsBackToExact(t *testing.T) {
	e := domain.NewEngine()
	d := mkExact("api.openai.com")
	d.HostMatchType = domain.HostMatchType("UNKNOWN_FROM_FUTURE_SCHEMA")
	if err := e.Swap([]domain.InterceptionDomain{d}); err != nil {
		t.Fatalf("swap: %v", err)
	}
	if got := e.MatchHost("api.openai.com"); got == nil {
		t.Fatalf("unknown match type should fall back to exact match")
	}
	if got := e.MatchHost("other.example.com"); got != nil {
		t.Fatalf("unknown match type must still reject non-equal hosts")
	}
}

func TestEngine_PathAction_PrefixGlobStripping(t *testing.T) {
	e := domain.NewEngine()
	d := mkExact("api.openai.com",
		domain.InterceptionPath{
			ID:          "p-glob-slash",
			PathPattern: []string{"/v1/*"},
			MatchType:   domain.PathMatchPrefix,
			Action:      domain.PathActionPassthrough,
		},
		domain.InterceptionPath{
			ID:          "p-glob-noslash",
			PathPattern: []string{"/admin*"},
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

	// "/v1/*" → trimmed to "/v1/" — must match /v1/anything.
	if got := e.PathAction(matched, "/v1/embeddings"); got != domain.PathActionPassthrough {
		t.Errorf("/v1/* expected PASSTHROUGH, got %v", got)
	}
	// "/admin*" → trimmed to "/admin" — must match /admin AND /administrators.
	if got := e.PathAction(matched, "/admin"); got != domain.PathActionBlock {
		t.Errorf("/admin* expected BLOCK for /admin, got %v", got)
	}
	if got := e.PathAction(matched, "/administrators"); got != domain.PathActionBlock {
		t.Errorf("/admin* expected BLOCK for /administrators, got %v", got)
	}
	// No-match path → DefaultPathAction (PROCESS).
	if got := e.PathAction(matched, "/v2/other"); got != domain.PathActionProcess {
		t.Errorf("unmatched path expected PROCESS, got %v", got)
	}
}

func TestEngine_PathAction_ExactMatch(t *testing.T) {
	e := domain.NewEngine()
	d := mkExact("api.openai.com",
		domain.InterceptionPath{
			ID:          "p-exact",
			PathPattern: []string{"/v1/chat/completions"},
			MatchType:   domain.PathMatchExact,
			Action:      domain.PathActionBlock,
		},
	)
	if err := e.Swap([]domain.InterceptionDomain{d}); err != nil {
		t.Fatalf("swap: %v", err)
	}
	matched := e.MatchHost("api.openai.com")
	if got := e.PathAction(matched, "/v1/chat/completions"); got != domain.PathActionBlock {
		t.Errorf("exact match expected BLOCK, got %v", got)
	}
	// Off-by-one suffix must NOT match.
	if got := e.PathAction(matched, "/v1/chat/completions/extra"); got != domain.PathActionProcess {
		t.Errorf("exact match must not match suffixed path, got %v", got)
	}
}

func TestEngine_PathAction_RegexMatch(t *testing.T) {
	e := domain.NewEngine()
	d := mkExact("api.openai.com",
		domain.InterceptionPath{
			ID:          "p-regex",
			PathPattern: []string{`^/v\d+/embeddings$`},
			MatchType:   domain.PathMatchRegex,
			Action:      domain.PathActionPassthrough,
		},
	)
	if err := e.Swap([]domain.InterceptionDomain{d}); err != nil {
		t.Fatalf("swap: %v", err)
	}
	matched := e.MatchHost("api.openai.com")
	if got := e.PathAction(matched, "/v1/embeddings"); got != domain.PathActionPassthrough {
		t.Errorf("regex expected PASSTHROUGH on /v1/embeddings, got %v", got)
	}
	if got := e.PathAction(matched, "/v17/embeddings"); got != domain.PathActionPassthrough {
		t.Errorf("regex expected PASSTHROUGH on /v17/embeddings, got %v", got)
	}
	if got := e.PathAction(matched, "/v1/embeddings/x"); got != domain.PathActionProcess {
		t.Errorf("regex should not match suffix, got %v", got)
	}
}

// Invalid regex in a path pattern is treated as no-match at request
// time (lazy compile returns false). Engine.Swap doesn't pre-validate
// path regex, so production stays up even with a broken pattern.
func TestEngine_PathAction_RegexInvalid_FailsToProcess(t *testing.T) {
	e := domain.NewEngine()
	d := mkExact("api.openai.com",
		domain.InterceptionPath{
			ID:          "p-bad-regex",
			PathPattern: []string{`[invalid`},
			MatchType:   domain.PathMatchRegex,
			Action:      domain.PathActionBlock,
		},
	)
	if err := e.Swap([]domain.InterceptionDomain{d}); err != nil {
		t.Fatalf("swap: %v", err)
	}
	matched := e.MatchHost("api.openai.com")
	if got := e.PathAction(matched, "/anything"); got != domain.PathActionProcess {
		t.Errorf("bad regex should not match — should fall to PROCESS, got %v", got)
	}
}

// Unknown PathMatchType falls back to exact equality.
func TestEngine_PathAction_UnknownMatchType_FallsBackToExact(t *testing.T) {
	e := domain.NewEngine()
	d := mkExact("api.openai.com",
		domain.InterceptionPath{
			ID:          "p-unknown",
			PathPattern: []string{"/v1/chat"},
			MatchType:   domain.PathMatchType("FUTURE_TYPE"),
			Action:      domain.PathActionBlock,
		},
	)
	if err := e.Swap([]domain.InterceptionDomain{d}); err != nil {
		t.Fatalf("swap: %v", err)
	}
	matched := e.MatchHost("api.openai.com")
	if got := e.PathAction(matched, "/v1/chat"); got != domain.PathActionBlock {
		t.Errorf("unknown match type should fall back to exact (match) → BLOCK, got %v", got)
	}
	if got := e.PathAction(matched, "/v1/chat/x"); got != domain.PathActionProcess {
		t.Errorf("unknown match type should fall back to exact (no match) → PROCESS, got %v", got)
	}
}

// A matched domain whose DefaultPathAction is empty string falls back
// to PROCESS — covers the `domain.DefaultPathAction == ""` branch.
func TestEngine_PathAction_EmptyDefaultFallsBackToProcess(t *testing.T) {
	e := domain.NewEngine()
	d := mkExact("api.openai.com")
	d.DefaultPathAction = ""
	if err := e.Swap([]domain.InterceptionDomain{d}); err != nil {
		t.Fatalf("swap: %v", err)
	}
	matched := e.MatchHost("api.openai.com")
	if matched == nil {
		t.Fatal("host should match")
	}
	if got := e.PathAction(matched, "/anything"); got != domain.PathActionProcess {
		t.Errorf("empty default should yield PROCESS, got %v", got)
	}
}

// A matched domain with a non-empty DefaultPathAction returns it on no
// path match — covers the `return domain.DefaultPathAction` branch.
func TestEngine_PathAction_NonEmptyDefaultReturned(t *testing.T) {
	e := domain.NewEngine()
	d := mkExact("api.openai.com")
	d.DefaultPathAction = domain.PathActionBlock
	if err := e.Swap([]domain.InterceptionDomain{d}); err != nil {
		t.Fatalf("swap: %v", err)
	}
	matched := e.MatchHost("api.openai.com")
	if got := e.PathAction(matched, "/no/match"); got != domain.PathActionBlock {
		t.Errorf("non-empty default should yield BLOCK, got %v", got)
	}
}

// First-call Swap with a bad regex must return an error AND leave the
// engine matching nothing (previous snapshot was empty). Pairs with the
// pre-existing bad-regex test which exercises the "preserve previous
// snapshot" branch — this one covers the cold-start failure.
func TestEngine_Swap_FirstCallBadRegex_ReturnsError(t *testing.T) {
	e := domain.NewEngine()
	bad := mkExact("[invalid")
	bad.HostMatchType = domain.HostMatchRegex
	bad.HostPattern = "[invalid"
	if err := e.Swap([]domain.InterceptionDomain{bad}); err == nil {
		t.Fatalf("expected swap to error on bad regex on cold engine")
	}
	if got := e.MatchHost("api.openai.com"); got != nil {
		t.Fatalf("engine should not match anything after rejected swap")
	}
}

// Swap with empty slice clears the engine and is not an error.
func TestEngine_Swap_EmptySlice(t *testing.T) {
	e := domain.NewEngine()
	if err := e.Swap([]domain.InterceptionDomain{mkExact("api.openai.com")}); err != nil {
		t.Fatalf("seed swap: %v", err)
	}
	if err := e.Swap(nil); err != nil {
		t.Fatalf("nil swap should be ok, got %v", err)
	}
	if got := e.MatchHost("api.openai.com"); got != nil {
		t.Fatalf("nil swap should clear matchers")
	}
	if got := e.AllowlistEntries(); len(got) != 0 {
		t.Fatalf("nil swap should clear allowlist, got %v", got)
	}
}

func TestEngine_AllowlistEntries_DedupAndPortAndRegexSkip(t *testing.T) {
	e := domain.NewEngine()

	d1 := mkExact("api.openai.com") // → api.openai.com:443
	d2 := mkExact("api.openai.com") // duplicate after normalize → dedup
	d2.ID = "id-dup"
	d3 := mkExact("internal:8443") // pre-ported → preserved verbatim, lowercased
	d3.ID = "id-port"
	d3.HostPattern = "internal:8443"
	d4 := mkExact("regex-only") // regex hosts are skipped from the allowlist
	d4.ID = "id-regex"
	d4.HostPattern = `^api\.openai\.com$`
	d4.HostMatchType = domain.HostMatchRegex
	d5 := mkExact("empty-host") // empty host_pattern is skipped
	d5.ID = "id-empty"
	d5.HostPattern = "   " // trimmed → empty

	if err := e.Swap([]domain.InterceptionDomain{d1, d2, d3, d4, d5}); err != nil {
		t.Fatalf("swap: %v", err)
	}
	got := e.AllowlistEntries()
	sort.Strings(got)
	want := []string{"api.openai.com:443", "internal:8443"}
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AllowlistEntries dedup/port/regex-skip mismatch:\n got: %v\nwant: %v", got, want)
	}
}

// AllowlistEntries on a cold engine returns an empty slice (no nil-deref).
func TestEngine_AllowlistEntries_FreshEngine(t *testing.T) {
	e := domain.NewEngine()
	if got := e.AllowlistEntries(); len(got) != 0 {
		t.Errorf("fresh engine should return empty allowlist, got %v", got)
	}
}

// Snapshot returns a copy that survives a subsequent Swap, so callers
// (eg the runtime introspection endpoint) can iterate it without
// holding a reader lock. Covers the 0% Snapshot function.
func TestEngine_Snapshot_DefensiveCopySurvivesSwap(t *testing.T) {
	e := domain.NewEngine()
	d1 := mkExact("api.openai.com")
	d1.ID = "before"
	if err := e.Swap([]domain.InterceptionDomain{d1}); err != nil {
		t.Fatalf("swap-1: %v", err)
	}
	snap := e.Snapshot()
	if len(snap) != 1 || snap[0].ID != "before" {
		t.Fatalf("snapshot pre-swap mismatch: %+v", snap)
	}

	// Mutating the returned slice header must not affect the engine.
	snap[0].ID = "mutated-by-caller"
	live := e.MatchHost("api.openai.com")
	if live == nil || live.ID != "before" {
		t.Fatalf("engine state must be independent of snapshot mutations, got %+v", live)
	}

	// A subsequent swap replaces the engine but the previously-returned
	// snapshot keeps its old contents (defensive copy at call time).
	d2 := mkExact("api.anthropic.com")
	d2.ID = "after"
	if err := e.Swap([]domain.InterceptionDomain{d2}); err != nil {
		t.Fatalf("swap-2: %v", err)
	}
	if len(snap) != 1 || snap[0].ID == "after" {
		t.Fatalf("prior snapshot must be independent of subsequent Swap, got %+v", snap)
	}
}

// Snapshot on a fresh engine returns an empty (non-nil-safe) slice.
func TestEngine_Snapshot_FreshEngine_Empty(t *testing.T) {
	e := domain.NewEngine()
	got := e.Snapshot()
	if len(got) != 0 {
		t.Errorf("fresh engine snapshot should be empty, got %v", got)
	}
}
