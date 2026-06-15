package traffic

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/interception"
)

// adapter.go — Register frozen-error, All, Namespace.

// Verifies Register rejects after Freeze and that All returns every registered
// id post-freeze (registry is read-only after startup).
func TestAdapterRegistry_All_AndFreezeRejectsRegister(t *testing.T) {
	reg := NewAdapterRegistry("ns_test")
	if err := reg.Register("a", func() Adapter { return &stubAdapter{id: "a"} }); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	if err := reg.Register("b", func() Adapter { return &stubAdapter{id: "b"} }); err != nil {
		t.Fatalf("second Register: %v", err)
	}
	reg.Freeze()

	if err := reg.Register("c", func() Adapter { return &stubAdapter{id: "c"} }); err == nil {
		t.Fatalf("Register after Freeze must return an error")
	} else if !strings.Contains(err.Error(), "frozen") {
		t.Errorf("error should mention 'frozen', got %v", err)
	}

	ids := reg.All()
	sort.Strings(ids)
	if !reflect.DeepEqual(ids, []string{"a", "b"}) {
		t.Errorf("All()=%v, want [a b]", ids)
	}
}

// Verifies Namespace returns the constructor-time value.
func TestAdapterRegistry_Namespace(t *testing.T) {
	reg := NewAdapterRegistry("nexus_compliance_proxy")
	if got := reg.Namespace(); got != "nexus_compliance_proxy" {
		t.Errorf("Namespace=%q, want %q", got, "nexus_compliance_proxy")
	}
}

// types.go — FilterResult.String for every variant + unknown.

func TestFilterResult_String(t *testing.T) {
	cases := []struct {
		in   FilterResult
		want string
	}{
		{Process, "PROCESS"},
		{Passthrough, "PASSTHROUGH"},
		{Block, "BLOCK"},
		{FilterResult(99), "UNKNOWN"},
	}
	for _, c := range cases {
		if got := c.in.String(); got != c.want {
			t.Errorf("FilterResult(%d).String()=%q, want %q", int(c.in), got, c.want)
		}
	}
}

// matchers.go — HostMatchSpecificity, matchHost default + path Glob/default,
// matchRegex compile-failure + cache-eviction.

func TestHostMatchSpecificity_AllVariants(t *testing.T) {
	cases := []struct {
		mt   interception.HostMatchType
		want int
	}{
		{interception.HostMatchTypeExact, 4},
		{interception.HostMatchTypePrefix, 3},
		{interception.HostMatchTypeGlob, 2},
		{interception.HostMatchTypeRegex, 1},
		{interception.HostMatchType("UNKNOWN"), 0},
	}
	for _, c := range cases {
		if got := HostMatchSpecificity(c.mt); got != c.want {
			t.Errorf("HostMatchSpecificity(%q)=%d, want %d", c.mt, got, c.want)
		}
	}
}

func TestMatchHost_UnknownTypeIsFalse(t *testing.T) {
	// Default arm — unrecognised HostMatchType must NOT match (fail-closed).
	if matchHost("api.openai.com", "api.openai.com", interception.HostMatchType("UNKNOWN")) {
		t.Errorf("unknown HostMatchType must return false")
	}
}

func TestMatchPath_Glob_And_Default(t *testing.T) {
	// PathMatchTypeGlob — explicit arm not covered by the existing tests.
	if !matchPath("/v1/chat/completions", "/v1/*/completions", interception.PathMatchTypeGlob) {
		t.Errorf("expected glob match")
	}
	if matchPath("/v2/anything", "/v1/*", interception.PathMatchTypeGlob) {
		t.Errorf("expected glob no-match")
	}
	// Default arm — unknown PathMatchType returns false.
	if matchPath("/anything", "/anything", interception.PathMatchType("UNKNOWN")) {
		t.Errorf("unknown PathMatchType must return false")
	}
}

func TestMatchRegex_InvalidPatternReturnsFalse(t *testing.T) {
	// Invalid regex must not panic — must return false. Covers err-branch.
	if matchRegex("(invalid[", "anything") {
		t.Errorf("invalid regex must return false")
	}
	// Second call hits the same broken pattern path again (no caching of failures).
	if matchRegex("(invalid[", "anything") {
		t.Errorf("invalid regex must remain false on retry")
	}
}

func TestMatchRegex_CacheEvictionResetsAtLimit(t *testing.T) {
	// Force the cache up over maxRegexCache (512). The eviction arm wipes
	// the cache when len >= maxRegexCache so subsequent compilations re-fill it.
	// We can't read the private cache size; instead drive enough unique
	// patterns through and re-match the first one to verify it still works
	// after eviction.
	first := "^aaa_first$"
	if !matchRegex(first, "aaa_first") {
		t.Fatalf("warm-up match should pass")
	}
	for i := range maxRegexCache + 10 {
		// Different patterns each iteration so the cache grows then resets.
		pat := "^pat_" + strings.Repeat("x", i%50) + "_" + itoa(i) + "$"
		_ = matchRegex(pat, "anything")
	}
	// The first pattern's cached entry may have been evicted; re-matching
	// must still work because matchRegex recompiles on miss.
	if !matchRegex(first, "aaa_first") {
		t.Errorf("after cache eviction, recompile path must still match")
	}
}

func itoa(i int) string {
	// inline avoid strconv import for one helper
	if i == 0 {
		return "0"
	}
	var out []byte
	neg := i < 0
	if neg {
		i = -i
	}
	for i > 0 {
		out = append([]byte{byte('0' + i%10)}, out...)
		i /= 10
	}
	if neg {
		out = append([]byte{'-'}, out...)
	}
	return string(out)
}

// snapshot.go — matchTypeSpecificity unknown arm, action conversion defaults,
// Domains, HostPatterns, BuildDomainSnapshot adapter-Configure error + bad
// adapterConfig JSON, FindInstance slow-path matcher, sortPaths same-priority
// same-specificity createdAt tiebreak.

func TestMatchTypeSpecificity_AllVariants(t *testing.T) {
	cases := []struct {
		mt   interception.PathMatchType
		want int
	}{
		{interception.PathMatchTypeExact, 4},
		{interception.PathMatchTypePrefix, 3},
		{interception.PathMatchTypeGlob, 2},
		{interception.PathMatchTypeRegex, 1},
		{interception.PathMatchType("UNKNOWN"), 0},
	}
	for _, c := range cases {
		if got := matchTypeSpecificity(c.mt); got != c.want {
			t.Errorf("matchTypeSpecificity(%q)=%d, want %d", c.mt, got, c.want)
		}
	}
}

func TestPathActionConversion_AllActionsAndDefault(t *testing.T) {
	// pathActionToFilterResult: every defined action + unknown -> Passthrough.
	if pathActionToFilterResult(interception.PathActionProcess) != Process {
		t.Error("PathActionProcess should map to Process")
	}
	if pathActionToFilterResult(interception.PathActionPassthrough) != Passthrough {
		t.Error("PathActionPassthrough should map to Passthrough")
	}
	if pathActionToFilterResult(interception.PathActionBlock) != Block {
		t.Error("PathActionBlock should map to Block")
	}
	if pathActionToFilterResult(interception.PathAction("BOGUS")) != Passthrough {
		t.Error("unknown PathAction should fall back to Passthrough")
	}

	// defaultPathActionToFilterResult: every defined action + unknown -> Passthrough.
	if defaultPathActionToFilterResult(interception.DefaultPathActionProcess) != Process {
		t.Error("DefaultPathActionProcess should map to Process")
	}
	if defaultPathActionToFilterResult(interception.DefaultPathActionPassthrough) != Passthrough {
		t.Error("DefaultPathActionPassthrough should map to Passthrough")
	}
	if defaultPathActionToFilterResult(interception.DefaultPathActionBlock) != Block {
		t.Error("DefaultPathActionBlock should map to Block")
	}
	if defaultPathActionToFilterResult(interception.DefaultPathAction("BOGUS")) != Passthrough {
		t.Error("unknown DefaultPathAction should fall back to Passthrough")
	}
}

func TestDomainSnapshot_Domains_AndHostPatterns(t *testing.T) {
	reg := NewAdapterRegistry("test")
	_ = reg.Register("openai", func() Adapter { return &stubAdapter{id: "openai"} })
	_ = reg.Register("anth", func() Adapter { return &stubAdapter{id: "anth"} })

	now := time.Now()
	domains := []interception.InterceptionDomain{
		{
			Id: "d1", Name: "openai-public", HostPattern: "api.openai.com",
			HostMatchType: interception.HostMatchTypeExact, AdapterId: "openai",
			Enabled: true, Priority: 10, CreatedAt: now, UpdatedAt: now,
		},
		{
			Id: "d2", Name: "anthropic-public", HostPattern: "api.anthropic.com",
			HostMatchType: interception.HostMatchTypeExact, AdapterId: "anth",
			Enabled: true, Priority: 5, CreatedAt: now, UpdatedAt: now,
		},
	}
	snap := BuildDomainSnapshot(domains, nil, reg, testLogger())

	names := snap.Domains()
	if len(names) != 2 {
		t.Fatalf("Domains()=%v, want 2", names)
	}
	// Must contain "name(host)" formatting for both.
	combined := strings.Join(names, "|")
	if !strings.Contains(combined, "openai-public(api.openai.com)") {
		t.Errorf("Domains missing openai entry: %q", combined)
	}
	if !strings.Contains(combined, "anthropic-public(api.anthropic.com)") {
		t.Errorf("Domains missing anthropic entry: %q", combined)
	}

	hp := snap.HostPatterns()
	sort.Strings(hp)
	if !reflect.DeepEqual(hp, []string{"api.anthropic.com", "api.openai.com"}) {
		t.Errorf("HostPatterns=%v", hp)
	}
}

// HostPatterns skips entries whose HostPattern is empty even when enabled.
func TestDomainSnapshot_HostPatterns_SkipsEmptyPattern(t *testing.T) {
	// Build by hand to bypass BuildDomainSnapshot's natural filtering — we
	// want to exercise the inner skip-arm.
	snap := &DomainSnapshot{
		ByHost: map[string]*Instance{},
		Instances: []*Instance{
			{Domain: InterceptionDomainConfig{Enabled: true, HostPattern: "api.openai.com"}},
			{Domain: InterceptionDomainConfig{Enabled: true, HostPattern: ""}}, // skipped
			{Domain: InterceptionDomainConfig{Enabled: false, HostPattern: "api.disabled.com"}},
		},
	}
	got := snap.HostPatterns()
	if !reflect.DeepEqual(got, []string{"api.openai.com"}) {
		t.Errorf("HostPatterns=%v, want [api.openai.com] only", got)
	}
}

// failingConfigureAdapter returns an error from Configure to exercise the
// "Configure failed" warn-skip branch in BuildDomainSnapshot.
type failingConfigureAdapter struct{ stubAdapter }

func (failingConfigureAdapter) Configure(_ map[string]any) error {
	return errors.New("bad config")
}

func TestBuildDomainSnapshot_ConfigureError_Skipped(t *testing.T) {
	reg := NewAdapterRegistry("test")
	_ = reg.Register("broken", func() Adapter { return &failingConfigureAdapter{stubAdapter{id: "broken"}} })

	now := time.Now()
	domains := []interception.InterceptionDomain{
		{
			Id: "d1", Name: "broken-cfg", HostPattern: "api.test.com",
			HostMatchType: interception.HostMatchTypeExact, AdapterId: "broken",
			Enabled: true, CreatedAt: now, UpdatedAt: now,
		},
	}
	snap := BuildDomainSnapshot(domains, nil, reg, testLogger())
	if snap.Size() != 0 {
		t.Errorf("domain whose adapter.Configure failed must be skipped, got Size=%d", snap.Size())
	}
}

func TestBuildDomainSnapshot_BadAdapterConfigJSON_Skipped(t *testing.T) {
	reg := NewAdapterRegistry("test")
	_ = reg.Register("openai", func() Adapter { return &stubAdapter{id: "openai"} })

	now := time.Now()
	domains := []interception.InterceptionDomain{
		{
			Id: "d1", Name: "bad-json", HostPattern: "api.openai.com",
			HostMatchType: interception.HostMatchTypeExact, AdapterId: "openai",
			Enabled: true, CreatedAt: now, UpdatedAt: now,
			AdapterConfig: []byte("{not-valid-json"),
		},
	}
	snap := BuildDomainSnapshot(domains, nil, reg, testLogger())
	if snap.Size() != 0 {
		t.Errorf("domain with malformed adapterConfig must be skipped, got Size=%d", snap.Size())
	}
}

// BuildDomainSnapshot disabled-paths must be filtered out before being attached.
func TestBuildDomainSnapshot_DisabledPathsFiltered(t *testing.T) {
	reg := NewAdapterRegistry("test")
	_ = reg.Register("openai", func() Adapter { return &stubAdapter{id: "openai"} })

	now := time.Now()
	domains := []interception.InterceptionDomain{
		{
			Id: "d1", Name: "openai", HostPattern: "api.openai.com",
			HostMatchType: interception.HostMatchTypeExact, AdapterId: "openai",
			Enabled: true, CreatedAt: now, UpdatedAt: now,
		},
	}
	paths := []interception.InterceptionPath{
		{
			Id: "p1", DomainId: "d1", PathPattern: []string{"/v1/x"},
			MatchType: interception.PathMatchTypePrefix, Action: interception.PathActionPassthrough,
			Enabled: true, CreatedAt: now, UpdatedAt: now,
		},
		{
			Id: "p2", DomainId: "d1", PathPattern: []string{"/v1/y"},
			MatchType: interception.PathMatchTypePrefix, Action: interception.PathActionBlock,
			Enabled: false, CreatedAt: now, UpdatedAt: now, // disabled
		},
	}
	snap := BuildDomainSnapshot(domains, paths, reg, testLogger())
	if snap.Size() != 1 {
		t.Fatalf("expected 1 domain, got %d", snap.Size())
	}
	if len(snap.Instances[0].Paths) != 1 {
		t.Errorf("disabled path must be filtered out, got %d paths", len(snap.Instances[0].Paths))
	}
}

// FindInstance slow-path: GLOB host matching forces iteration past ByHost.
func TestDomainSnapshot_FindInstance_SlowPathGlob(t *testing.T) {
	reg := NewAdapterRegistry("test")
	_ = reg.Register("openai", func() Adapter { return &stubAdapter{id: "openai"} })

	now := time.Now()
	domains := []interception.InterceptionDomain{
		{
			Id: "d1", Name: "openai-glob", HostPattern: "*.openai.com",
			HostMatchType: interception.HostMatchTypeGlob, AdapterId: "openai",
			Enabled: true, CreatedAt: now, UpdatedAt: now,
		},
	}
	snap := BuildDomainSnapshot(domains, nil, reg, testLogger())

	// ByHost is empty for GLOB; FindInstance must fall through to slow path.
	if _, ok := snap.ByHost["api.openai.com"]; ok {
		t.Errorf("GLOB host must not populate ByHost fast-path map")
	}
	if inst := snap.FindInstance("api.openai.com"); inst == nil {
		t.Errorf("FindInstance must find GLOB-matching host via slow path")
	}
	if inst := snap.FindInstance("api.somewhere.else"); inst != nil {
		t.Errorf("FindInstance must return nil for non-matching host")
	}
}

// sortPaths same-priority same-specificity falls back to createdAt asc.
func TestSortPaths_SamePrioritySameSpecificity_CreatedAtTiebreak(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	paths := []InterceptionPathConfig{
		{ID: "later", MatchType: interception.PathMatchTypeExact, Priority: 0, CreatedAt: t0.Add(time.Hour)},
		{ID: "earlier", MatchType: interception.PathMatchTypeExact, Priority: 0, CreatedAt: t0},
	}
	sortPaths(paths)
	if paths[0].ID != "earlier" {
		t.Errorf("earlier createdAt must sort first, got order=%s,%s", paths[0].ID, paths[1].ID)
	}
}

// Disabled path inside ResolveAction is skipped (covers the !p.Enabled arm of
// the path loop).
func TestResolveAction_SkipsDisabledPathRule(t *testing.T) {
	reg := NewAdapterRegistry("test")
	_ = reg.Register("openai", func() Adapter { return &stubAdapter{id: "openai"} })

	now := time.Now()
	domains := []interception.InterceptionDomain{
		{
			Id: "d1", Name: "o", HostPattern: "api.openai.com",
			HostMatchType: interception.HostMatchTypeExact, AdapterId: "openai",
			Enabled: true, DefaultPathAction: interception.DefaultPathActionProcess,
			CreatedAt: now, UpdatedAt: now,
		},
	}
	snap := BuildDomainSnapshot(domains, nil, reg, testLogger())

	// Manually inject a disabled path on the live snapshot so the path-loop
	// `if !p.Enabled { continue }` arm executes. Then resolution must fall
	// through to DefaultPathAction (Process).
	snap.Instances[0].Paths = []InterceptionPathConfig{
		{
			ID: "p1", PathPattern: []string{"/v1/chat/completions"},
			MatchType: interception.PathMatchTypeExact, Action: interception.PathActionBlock,
			Priority: 0, Enabled: false, // disabled — must be skipped
		},
	}
	_, result, pathRule := snap.ResolveAction("api.openai.com", "/v1/chat/completions")
	if result != Process {
		t.Errorf("disabled path must be skipped; expected default Process, got %s", result)
	}
	if pathRule != nil {
		t.Errorf("pathRule must be nil when no enabled rule matches")
	}
}

// observability.go — RegisterMetrics + RecordUnmatched smoke.

func TestObservability_RegisterAndRecordUnmatched(t *testing.T) {
	// RegisterMetrics is sync.Once-guarded; first call wins. We call it with
	// a unique namespace; subsequent tests in this package may have already
	// registered, in which case our call is the documented no-op. Either way
	// RecordUnmatched MUST be safe (nil-check inside guards a missing counter).
	RegisterMetrics("nexus_traffic_test")
	// Second call is a no-op — must not panic / duplicate-register.
	RegisterMetrics("nexus_traffic_test_other")

	// Increment a couple of label combos — the function returns nothing; we
	// assert it does not panic and that unmatchedTotal is wired.
	RecordUnmatched("api.openai.com", "no_rule")
	RecordUnmatched("api.openai.com", "no_adapter")
	RecordUnmatched("api.anthropic.com", "parse_error")

	if unmatchedTotal == nil {
		t.Fatalf("unmatchedTotal must be initialised after RegisterMetrics")
	}
}

// RecordUnmatched without a prior RegisterMetrics must be a safe no-op.
// We test the nil-guard by temporarily zeroing the package counter, calling,
// and restoring. (Single-test isolation: this runs after the register test;
// we restore so subsequent test runs are deterministic.)
func TestRecordUnmatched_NilCounterIsNoop(t *testing.T) {
	saved := unmatchedTotal
	unmatchedTotal = nil
	defer func() { unmatchedTotal = saved }()
	// Must not panic.
	RecordUnmatched("any", "any")
}

// tracing.go — Unwrap, AddBreakdown, Breakdown, WithPhaseSink nil-sink,
// NewTracingTransport nil-base, RoundTrip transport error path.

func TestNewTracingTransport_NilBaseUsesDefault(t *testing.T) {
	tr := NewTracingTransport(nil)
	tt, ok := tr.(*tracingTransport)
	if !ok {
		t.Fatalf("expected *tracingTransport, got %T", tr)
	}
	if tt.base == nil {
		t.Errorf("nil base must be replaced by http.DefaultTransport, got nil")
	}
	// Unwrap returns the wrapped transport — covers Unwrap().
	if got := tt.Unwrap(); got != tt.base {
		t.Errorf("Unwrap should return the wrapped base")
	}
}

func TestWithPhaseSink_NilSinkReturnsParentUnchanged(t *testing.T) {
	parent := context.Background()
	got := WithPhaseSink(parent, nil)
	// Must literally return the parent (no wrap) — early-return arm.
	if got != parent {
		t.Errorf("nil sink must return parent context unchanged")
	}
	// Sanity: real sink installs.
	ps := NewPhaseSink()
	child := WithPhaseSink(parent, ps)
	if PhaseSinkFromContext(child) != ps {
		t.Errorf("real sink must be retrievable from child context")
	}
}

func TestPhaseSink_AddBreakdown_AndBreakdown(t *testing.T) {
	ps := NewPhaseSink()
	if got := ps.Breakdown(); got != nil {
		t.Errorf("empty sink Breakdown must be nil, got %v", got)
	}
	// Stamp two phases; the same key twice must accumulate.
	ps.AddBreakdown("codec_decode_ms", 7)
	ps.AddBreakdown("codec_decode_ms", 3)
	ps.AddBreakdown("usage_extract_ms", 2)
	got := ps.Breakdown()
	if got["codec_decode_ms"] != 10 {
		t.Errorf("codec_decode_ms accumulate: got %d, want 10", got["codec_decode_ms"])
	}
	if got["usage_extract_ms"] != 2 {
		t.Errorf("usage_extract_ms: got %d, want 2", got["usage_extract_ms"])
	}
	// Returned map is independent — mutating it must not affect the sink.
	got["codec_decode_ms"] = 9999
	again := ps.Breakdown()
	if again["codec_decode_ms"] != 10 {
		t.Errorf("Breakdown must return a defensive copy; got mutated value %d", again["codec_decode_ms"])
	}
}

func TestPhaseSink_AddBreakdown_NilEmptyZeroSkipped(t *testing.T) {
	// nil sink — must not panic.
	var nilPS *PhaseSink
	nilPS.AddBreakdown("x", 5)
	if got := nilPS.Breakdown(); got != nil {
		t.Errorf("nil sink Breakdown must be nil, got %v", got)
	}

	ps := NewPhaseSink()
	ps.AddBreakdown("", 5)   // empty name dropped
	ps.AddBreakdown("a", 0)  // zero dropped
	ps.AddBreakdown("b", -1) // negative dropped
	if got := ps.Breakdown(); got != nil {
		t.Errorf("invalid AddBreakdown calls must leave sink empty, got %v", got)
	}
}

// errorTransport returns the supplied error so we exercise the RoundTrip
// `if err != nil` branch (stamps totalMs from the failure path).
type errorTransport struct{ err error }

func (e *errorTransport) RoundTrip(_ *http.Request) (*http.Response, error) {
	// Sleep a tick so the millisecond clamp is observable but bounded.
	time.Sleep(2 * time.Millisecond)
	return nil, e.err
}

func TestTracingTransport_RoundTripError_StampsTotalMs(t *testing.T) {
	tr := NewTracingTransport(&errorTransport{err: errors.New("boom")})
	ps := NewPhaseSink()
	ctx := WithPhaseSink(context.Background(), ps)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://example.invalid/x", nil)

	_, err := tr.RoundTrip(req) //nolint:bodyclose // errorTransport returns (nil, err); no body exists
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected boom error, got %v", err)
	}
	// TotalMs stamped even though no body close; TTFB never observed.
	if ps.TotalMs() == nil {
		t.Errorf("TotalMs must be populated on transport-error path")
	}
	if ps.TtfbMs() != nil {
		t.Errorf("TtfbMs must be nil when no first response byte arrived, got %v", *ps.TtfbMs())
	}
}

// Streaming-style scenario: server flushes header before body, body close
// stamps TotalMs; multiple Close() invocations must fire onClose ONLY once.
func TestTracingTransport_CloseOnce_StampsTotalOnce(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		if fl != nil {
			fl.Flush()
		}
		time.Sleep(5 * time.Millisecond)
		_, _ = w.Write([]byte("payload"))
	}))
	defer srv.Close()

	tr := NewTracingTransport(http.DefaultTransport)
	ps := NewPhaseSink()
	ctx := WithPhaseSink(context.Background(), ps)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	// First Close stamps TotalMs.
	_ = resp.Body.Close()
	firstTotal := ps.TotalMs()
	if firstTotal == nil {
		t.Fatalf("TotalMs must be populated after first Close")
	}
	// Second Close must not advance / change (sync.Once guards onClose).
	_ = resp.Body.Close()
	secondTotal := ps.TotalMs()
	if secondTotal == nil || *secondTotal != *firstTotal {
		t.Errorf("Close called twice must not re-stamp TotalMs (sync.Once); first=%v second=%v",
			derefInt(firstTotal), derefInt(secondTotal))
	}
}

// Streaming-broker-pump scenario: the upstream body is fully drained
// (Read returns EOF) but the goroutine that will call Close() is delayed
// — modelling broker.pump's `defer session.Close()` firing later than
// the request handler's audit defer. The fix (stamp totalMs on every
// successful Read) means TotalMs is already populated by the time the
// handler reads it, even though Close hasn't fired. Without the fix
// the handler would observe TotalMs==nil (~99% of streaming MISS rows had
// upstream_total_ms NULL before the fix that stamps totalMs on every Read).
func TestTracingTransport_TotalMsAvailableBeforeClose(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		if fl != nil {
			fl.Flush()
		}
		time.Sleep(5 * time.Millisecond)
		_, _ = w.Write([]byte("frame-1\n"))
		if fl != nil {
			fl.Flush()
		}
		time.Sleep(5 * time.Millisecond)
		_, _ = w.Write([]byte("frame-2\n"))
	}))
	defer srv.Close()

	tr := NewTracingTransport(http.DefaultTransport)
	ps := NewPhaseSink()
	ctx := WithPhaseSink(context.Background(), ps)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := tr.RoundTrip(req) //nolint:bodyclose // intentional deferred-close in goroutine below mimics broker.pump
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}

	// Drain to EOF in this goroutine — Read-side stamps fire here.
	_, _ = io.Copy(io.Discard, resp.Body)

	// TotalMs MUST be populated even though Close() hasn't been called
	// yet. This is the streaming-pump race we are fixing.
	beforeClose := ps.TotalMs()
	if beforeClose == nil {
		t.Fatalf("TotalMs must be populated by Read-stamping before Close fires (broker pump race)")
	}
	if *beforeClose <= 0 {
		t.Errorf("TotalMs must be > 0, got %d", *beforeClose)
	}

	// Defer-close in a different goroutine to mimic broker.pump's
	// `defer session.Close()` reaching later than the handler defer.
	doneClose := make(chan struct{})
	go func() {
		time.Sleep(2 * time.Millisecond)
		_ = resp.Body.Close()
		close(doneClose)
	}()
	<-doneClose
}

func derefInt(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

// A fresh non-nil PhaseSink has neither TTFB nor TotalMs populated; both
// must return nil (the "v <= 0" guard on the live receiver).
func TestPhaseSink_FreshNonNil_TtfbAndTotalNil(t *testing.T) {
	ps := NewPhaseSink()
	if got := ps.TtfbMs(); got != nil {
		t.Errorf("fresh sink TtfbMs must be nil, got %v", *got)
	}
	if got := ps.TotalMs(); got != nil {
		t.Errorf("fresh sink TotalMs must be nil, got %v", *got)
	}
}

// sanitizeSlug returns "unknown" when the input collapses to empty (all
// runes stripped). Covers the all-stripped → "unknown" arm.
func TestFormatHookOutcome_RejectReasonAllStrippedIsUnknown(t *testing.T) {
	// "!!!" is all non-alnum; sanitizeSlug strips everything and returns "unknown".
	got := FormatHookOutcome(HookOutcomeInput{Rejected: "hook", RejectReason: "!!!"})
	if got != "rejected:hook:unknown" {
		t.Errorf("all-stripped reason should yield 'unknown', got %q", got)
	}
	// Empty reason — sanitizeSlug also yields "unknown".
	got = FormatHookOutcome(HookOutcomeInput{Rejected: "hook", RejectReason: ""})
	if got != "rejected:hook:unknown" {
		t.Errorf("empty reason should yield 'unknown', got %q", got)
	}
}

// MergeExposeHeaders skips empty segments (",,x-foo, , y-bar"). Covers the
// `if t == "" { continue }` arm inside the splitter.
func TestMergeExposeHeaders_SkipsEmptySegments(t *testing.T) {
	h := http.Header{}
	// Three empty fragments + one real entry; merger must keep just the real one
	// plus the new nexus marker.
	h.Set("Access-Control-Expose-Headers", ", ,x-real, ")
	MergeExposeHeaders(h, "x-nexus-via")
	v := h.Get("Access-Control-Expose-Headers")
	// Must contain x-real and x-nexus-via, not contain double-commas.
	if !strings.Contains(v, "x-real") || !strings.Contains(v, "x-nexus-via") {
		t.Errorf("expected x-real + x-nexus-via, got %q", v)
	}
	if strings.Contains(v, ",,") {
		t.Errorf("merger must not emit empty-string entries, got %q", v)
	}
}

// BuildDomainSnapshot sort: two same-priority domains must sort by createdAt asc.
// Exercises the `return a.CreatedAt.Before(b.CreatedAt)` arm at snapshot.go:156.
func TestBuildDomainSnapshot_SortSamePriorityByCreatedAt(t *testing.T) {
	reg := NewAdapterRegistry("test")
	_ = reg.Register("openai", func() Adapter { return &stubAdapter{id: "openai"} })

	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Provide d2 (later) first in the slice; sort must put d1 (earlier) first.
	domains := []interception.InterceptionDomain{
		{
			Id: "d2", Name: "later", HostPattern: "api.later.com",
			HostMatchType: interception.HostMatchTypeExact, AdapterId: "openai",
			Enabled: true, Priority: 5, CreatedAt: t0.Add(time.Hour), UpdatedAt: t0.Add(time.Hour),
		},
		{
			Id: "d1", Name: "earlier", HostPattern: "api.earlier.com",
			HostMatchType: interception.HostMatchTypeExact, AdapterId: "openai",
			Enabled: true, Priority: 5, CreatedAt: t0, UpdatedAt: t0,
		},
	}
	snap := BuildDomainSnapshot(domains, nil, reg, testLogger())
	if snap.Size() != 2 {
		t.Fatalf("expected 2 instances, got %d", snap.Size())
	}
	if snap.Instances[0].Domain.ID != "d1" {
		t.Errorf("same-priority domains must sort by createdAt asc; got order %s,%s",
			snap.Instances[0].Domain.ID, snap.Instances[1].Domain.ID)
	}
}

// Confirms WithPhaseSink + atomic stores are concurrency-safe under load.
// Establishes observable behavior of AddBreakdown accumulation under contention.
func TestPhaseSink_AddBreakdown_ConcurrentAccumulates(t *testing.T) {
	ps := NewPhaseSink()
	const goroutines = 16
	const perGoroutine = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			for range perGoroutine {
				ps.AddBreakdown("phase_x", 1)
			}
		}()
	}
	wg.Wait()
	got := ps.Breakdown()
	if got["phase_x"] != goroutines*perGoroutine {
		t.Errorf("phase_x: got %d, want %d", got["phase_x"], goroutines*perGoroutine)
	}
}
