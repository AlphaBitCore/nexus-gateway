package catbagent

import (
	"context"
	"github.com/goccy/go-json"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/rulepack"
)

// fakeRulePackLister satisfies rulepack.InstallLister for testing the
// enrichment path on the agent hook-config loader. Returns the
// configured EffectiveRuleSets keyed by hook ID.
type fakeRulePackLister struct {
	sets map[string][]rulepack.EffectiveRuleSet
	err  error
}

func (f *fakeRulePackLister) LoadEffectiveSetsForHook(_ context.Context, hookID string) ([]rulepack.EffectiveRuleSet, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.sets[hookID], nil
}

// hookConfigCols mirrors the column order in agentHookConfigSelect.
var hookConfigCols = []string{
	"id", "name", "type", "implementationId", "stage", "category",
	"endpoint", "script", "config", "priority", "timeoutMs",
	"failBehavior", "enabled", "applicableIngress", "updatedAt",
}

func TestAgentHookConfigLoader_Load_Empty(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	mock.ExpectQuery(`FROM "HookConfig"`).
		WillReturnRows(pgxmock.NewRows(hookConfigCols))

	l := NewAgentHookConfigLoader(mock, nil, nil)
	state, ver, err := l.Load(context.Background(), "thing-x")
	if err != nil {
		t.Fatalf("Load err=%v", err)
	}
	if ver != 0 {
		t.Errorf("empty result set should report version=0, got %d", ver)
	}
	raw, _ := json.Marshal(state)
	if string(raw) != `{"hookConfigs":[]}` {
		t.Errorf("empty state marshalled to %s, want {\"hookConfigs\":[]}", raw)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAgentHookConfigLoader_Load_SingleRow(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	updated := time.Date(2026, 4, 22, 3, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`FROM "HookConfig"`).
		WillReturnRows(pgxmock.NewRows(hookConfigCols).AddRow(
			"hook-1",            // id
			"pii",               // name
			"builtin",           // type
			"pii-detector",      // implementationId
			"request",           // stage
			(*string)(nil),      // category
			(*string)(nil),      // endpoint
			(*string)(nil),      // script
			[]byte(`{"k":"v"}`), // config
			10,                  // priority
			5000,                // timeoutMs
			"fail-open",         // failBehavior
			true,                // enabled
			[]string{"ALL"},     // applicableIngress
			updated,
		))

	l := NewAgentHookConfigLoader(mock, nil, nil)
	state, ver, err := l.Load(context.Background(), "thing-x")
	if err != nil {
		t.Fatalf("Load err=%v", err)
	}
	if ver != updated.Unix() {
		t.Errorf("version = %d want %d", ver, updated.Unix())
	}
	raw, _ := json.Marshal(state)
	want := `{"hookConfigs":[{"id":"hook-1","implementationId":"pii-detector","name":"pii","priority":10,"enabled":true,"stage":"request","failBehavior":"fail-open","timeoutMs":5000,"applicableIngress":["ALL"],"config":{"k":"v"}}]}`
	if string(raw) != want {
		t.Errorf("state mismatch:\n got %s\nwant %s", raw, want)
	}
}

func TestAgentHookConfigLoader_Load_MultiRow_VersionIsMax(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	older := time.Date(2026, 4, 20, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 4, 22, 0, 0, 0, 0, time.UTC)

	mock.ExpectQuery(`FROM "HookConfig"`).
		WillReturnRows(pgxmock.NewRows(hookConfigCols).
			AddRow(
				"hook-a", "a", "builtin", "pii-detector", "request",
				(*string)(nil), (*string)(nil), (*string)(nil), []byte(nil),
				1, 5000, "fail-open", true, []string{"ALL"}, older,
			).
			AddRow(
				"hook-b", "b", "builtin", "keyword-filter", "request",
				(*string)(nil), (*string)(nil), (*string)(nil), []byte(nil),
				2, 5000, "fail-open", true, []string{"ALL"}, newer,
			))

	l := NewAgentHookConfigLoader(mock, nil, nil)
	state, ver, err := l.Load(context.Background(), "")
	if err != nil {
		t.Fatalf("Load err=%v", err)
	}
	if ver != newer.Unix() {
		t.Errorf("version should be max(updatedAt); got %d want %d", ver, newer.Unix())
	}
	type envelope struct {
		HookConfigs []struct {
			ID string `json:"id"`
		} `json:"hookConfigs"`
	}
	var env envelope
	raw, _ := json.Marshal(state)
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(env.HookConfigs) != 2 {
		t.Fatalf("expected 2 hooks, got %d", len(env.HookConfigs))
	}
}

func TestAgentHookConfigLoader_Load_NullConfigOmitted(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectQuery(`FROM "HookConfig"`).
		WillReturnRows(pgxmock.NewRows(hookConfigCols).AddRow(
			"hook-1", "n", "builtin", "noop", "request",
			(*string)(nil), (*string)(nil), (*string)(nil),
			[]byte(nil), 0, 5000, "fail-open", true,
			[]string{"ALL"}, time.Now().UTC(),
		))

	l := NewAgentHookConfigLoader(mock, nil, nil)
	state, _, err := l.Load(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(state)
	// A NULL config column must produce omitempty — no "config" key in
	// the marshalled row. Agent treats absent config as zero-value
	// map[string]any.
	if containsJSONKey(raw, "config") {
		t.Errorf("NULL config should be omitted; got %s", raw)
	}
}

func TestAgentHookConfigLoader_Load_NullApplicableIngressFallsBackToALL(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectQuery(`FROM "HookConfig"`).
		WillReturnRows(pgxmock.NewRows(hookConfigCols).AddRow(
			"hook-1", "n", "builtin", "noop", "request",
			(*string)(nil), (*string)(nil), (*string)(nil),
			[]byte(nil), 0, 5000, "fail-open", true,
			[]string(nil), time.Now().UTC(),
		))

	l := NewAgentHookConfigLoader(mock, nil, nil)
	state, _, err := l.Load(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(state)
	if !bytesContains(raw, []byte(`"applicableIngress":["ALL"]`)) {
		t.Errorf("NULL applicableIngress should default to [\"ALL\"]; got %s", raw)
	}
}

// containsJSONKey is a tiny helper that checks for a top-level / nested
// "<key>" substring; sufficient for asserting presence of field tags
// without building a dedicated parse path.
func containsJSONKey(raw []byte, key string) bool {
	return bytesContains(raw, []byte(`"`+key+`":`))
}

// TestAgentHookConfigLoader_Load_EnrichesRulePackConsumer asserts the
// core fix: when a rulePackStore is wired, hooks whose ImplementationID
// is in rulepack.RulePackConsumer (rulepack-engine, content-safety,
// keyword-filter, pii-detector) ship to the agent with their bound
// installs injected into Config["_rulePackInstalls"]. Without this
// the agent's rulepack-engine evaluates with no rules and admin-
// configured rule packs have zero effect on decisions.
func TestAgentHookConfigLoader_Load_EnrichesRulePackConsumer(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectQuery(`FROM "HookConfig"`).
		WillReturnRows(pgxmock.NewRows(hookConfigCols).AddRow(
			"cs-hook",                // id
			"content-safety",         // name
			"builtin",                // type
			"content-safety",         // implementationId — in RulePackConsumer
			"request",                // stage
			(*string)(nil),           // category
			(*string)(nil),           // endpoint
			(*string)(nil),           // script
			[]byte(`{"keep":"yes"}`), // config (legacy field that must survive)
			10,                       // priority
			5000,                     // timeoutMs
			"fail-open",              // failBehavior
			true,                     // enabled
			[]string{"ALL"},          // applicableIngress
			time.Now().UTC(),
		).AddRow(
			"ip-hook", "ip-access",
			"builtin", "ip-access-filter", // NOT in RulePackConsumer
			"request",
			(*string)(nil), (*string)(nil), (*string)(nil),
			[]byte(`{}`),
			20, 5000, "fail-open", true, []string{"ALL"},
			time.Now().UTC(),
		))

	store := &fakeRulePackLister{sets: map[string][]rulepack.EffectiveRuleSet{
		"cs-hook": {{
			Install: rulepack.Install{ID: "inst-1", PackName: "safety", Enabled: true},
			Pack: rulepack.Pack{
				Name:    "safety",
				Version: "1.0.0",
				Rules: []rulepack.Rule{{
					RuleID: "r-1", Category: "safety", Severity: "hard", Pattern: `\bsecret\b`,
				}},
			},
		}},
	}}

	l := NewAgentHookConfigLoader(mock, store, nil)
	state, _, err := l.Load(context.Background(), "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	raw, _ := json.Marshal(state)

	// Content-safety hook (in RulePackConsumer) MUST carry the install payload.
	if !bytesContains(raw, []byte(`"_rulePackInstalls"`)) {
		t.Fatalf("rulepack-consumer hook missing _rulePackInstalls; got %s", raw)
	}
	// The legacy `keep` field MUST survive the enrich round-trip.
	if !bytesContains(raw, []byte(`"keep":"yes"`)) {
		t.Errorf("legacy Config field lost during enrich; got %s", raw)
	}
	// IP-access hook (NOT in RulePackConsumer) MUST NOT be enriched —
	// the assertion is necessarily indirect via JSON inspection because
	// the loader returns an opaque `any`. We check there is exactly ONE
	// occurrence of `_rulePackInstalls` (the cs-hook's), not two.
	count := 0
	for i := 0; i+len(`"_rulePackInstalls"`) <= len(raw); i++ {
		if bytesContains(raw[i:i+len(`"_rulePackInstalls"`)], []byte(`"_rulePackInstalls"`)) {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly one _rulePackInstalls occurrence, got %d in %s", count, raw)
	}
}

// TestAgentHookConfigLoader_Load_NoEnrichWithoutStore confirms backward
// compat: passing rulePackStore=nil short-circuits the enrichment so
// existing test fixtures (and dev wiring without a Postgres pool)
// behave identically to pre-fix.
func TestAgentHookConfigLoader_Load_NoEnrichWithoutStore(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectQuery(`FROM "HookConfig"`).
		WillReturnRows(pgxmock.NewRows(hookConfigCols).AddRow(
			"cs-hook", "cs", "builtin", "content-safety", "request",
			(*string)(nil), (*string)(nil), (*string)(nil),
			[]byte(`{"k":"v"}`),
			10, 5000, "fail-open", true, []string{"ALL"},
			time.Now().UTC(),
		))

	l := NewAgentHookConfigLoader(mock, nil, nil) // nil store
	state, _, err := l.Load(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(state)
	if bytesContains(raw, []byte(`"_rulePackInstalls"`)) {
		t.Errorf("nil store should skip enrichment; got %s", raw)
	}
}

func bytesContains(haystack, needle []byte) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
