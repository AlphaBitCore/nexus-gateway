package freshness

import (
	"github.com/goccy/go-json"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// testLogger returns a discard slog.Logger for use in tests.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// loadSeedRulesJSON returns the rule list from the canonical seed fixture
// (tools/db-migrate/seed/fixtures/semantic_cache_config.json, the single row's
// time_sensitive_overrides field). That fixture is the
// single source of truth — seed.ts UPSERTs it into the DB; this test loads
// the same file so the conformance suite below validates the rules that
// actually ship.
func loadSeedRulesJSON(t *testing.T) []Rule {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed — cannot resolve seed rules JSON path")
	}
	// detector_test.go is 5 dirs below repo root:
	// packages/ai-gateway/internal/cache/freshness/detector_test.go
	// → ../../../../.. is the repo root.
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "..", "..")
	path := filepath.Join(repoRoot, "tools", "db-migrate", "seed", "fixtures", "semantic_cache_config.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read semantic_cache_config fixture %s: %v", path, err)
	}
	// The canonical time-sensitive rules live in the single semantic_cache_config
	// row's time_sensitive_overrides JSON ({"rules":[...]}).
	var fixture []struct {
		TimeSensitiveOverrides struct {
			Rules []Rule `json:"rules"`
		} `json:"time_sensitive_overrides"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("unmarshal semantic_cache_config fixture: %v", err)
	}
	if len(fixture) == 0 {
		t.Fatalf("semantic_cache_config fixture %s is empty", path)
	}
	return fixture[0].TimeSensitiveOverrides.Rules
}

// testDetector creates a Detector backed by the canonical seed rules with an
// isolated Prometheus registry.
func testDetector(t *testing.T) *Detector {
	t.Helper()
	reg := prometheus.NewRegistry()
	d, err := NewDetector(loadSeedRulesJSON(t), testLogger(), "nexus_aigw_test", reg)
	if err != nil {
		t.Fatalf("NewDetector: %v", err)
	}
	return d
}

func msgs(text string) []ChatMessage {
	return []ChatMessage{{Role: "user", Content: text}}
}

// --- NewDetector ---

func TestNewDetector_NilLogger(t *testing.T) {
	reg := prometheus.NewRegistry()
	_, err := NewDetector(loadSeedRulesJSON(t), nil, "ns", reg)
	if err == nil {
		t.Fatal("expected error for nil logger")
	}
}

func TestNewDetector_EmptyNamespace(t *testing.T) {
	reg := prometheus.NewRegistry()
	_, err := NewDetector(loadSeedRulesJSON(t), testLogger(), "", reg)
	if err == nil {
		t.Fatal("expected error for empty namespace")
	}
}

func TestNewDetector_InvalidRule(t *testing.T) {
	rules := []Rule{
		{ID: "bad", Keywords: []string{}, Enabled: true},
	}
	reg := prometheus.NewRegistry()
	_, err := NewDetector(rules, testLogger(), "nexus", reg)
	if err == nil {
		t.Fatal("expected error for rule with empty keywords")
	}
}

func TestNewDetector_NilRegisterer_UsesDefault(t *testing.T) {
	// NewDetector with nil reg should not panic (uses prometheus.DefaultRegisterer).
	// Use a unique namespace to avoid duplicate registration in the test binary.
	_, err := NewDetector([]Rule{}, testLogger(), "nexus_fresh_nilreg_test", nil)
	if err != nil {
		t.Fatalf("unexpected error with nil registerer: %v", err)
	}
}

func TestNewDetector_EmptyRuleSet(t *testing.T) {
	reg := prometheus.NewRegistry()
	d, err := NewDetector([]Rule{}, testLogger(), "nexus", reg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	matched, ruleID := d.IsTimeSensitive(msgs("What is the stock price of AAPL?"))
	if matched || ruleID != "" {
		t.Errorf("empty rule set must never match; got matched=%v ruleID=%q", matched, ruleID)
	}
}

// --- IsTimeSensitive seed-rule table tests ---

// seedRuleCase is a single positive or negative test case for a seed rule.
type seedRuleCase struct {
	name    string
	text    string
	wantHit bool
	wantID  string // non-empty only on positive cases
}

var seedRuleCases = []seedRuleCase{
	// --- time-current ---
	{
		name:    "time-current positive: 'today' with question mark",
		text:    "What happened today?",
		wantHit: true,
		wantID:  "time-current",
	},
	{
		name: "time-current positive: 'now' with question mark",
		// Use a standalone "now" with a question mark but no other rule keyword
		// so time-current is the only matching rule.
		text:    "What is going on now?",
		wantHit: true,
		wantID:  "time-current",
	},
	{
		name:    "time-current positive: 'latest' with question mark",
		text:    "What is the latest?",
		wantHit: true,
		wantID:  "time-current",
	},
	{
		// Product change: seed rules no longer require a question mark.
		// "Today" / "now" without "?" now fires time-current. Acceptable false
		// positive — cost is one wasted upstream call; benefit is fresh answers
		// for "现在几点" / "what's the weather today" style conversational prompts.
		name:    "time-current positive: 'today' without question mark (new default)",
		text:    "Today we will discuss architecture.",
		wantHit: true,
		wantID:  "time-current",
	},

	// --- stock-price ---
	// Prompts must NOT contain time-current keywords (now, today, latest,
	// current, this week, this month) to avoid time-current firing first.
	{
		name:    "stock-price positive: EN with ticker AAPL",
		text:    "What is the stock price of AAPL?",
		wantHit: true,
		wantID:  "stock-price",
	},
	{
		name: "stock-price positive: EN with market cap and ticker TSLA",
		// "right now" removed so time-current does not fire first.
		text:    "What is the market cap of TSLA?",
		wantHit: true,
		wantID:  "stock-price",
	},
	{
		// Product change (RequireEntity=false on all rules): Chinese place
		// names (中国, 北京) are not entities under the heuristic — gating on
		// entities killed legitimate prompts. Keyword strength is the signal;
		// the rare "stock price as a metaphor" false positive costs one upstream call.
		name:    "stock-price positive: bare keyword (new default, ex-negative)",
		text:    "Explain stock price as a concept.",
		wantHit: true,
		wantID:  "stock-price",
	},
	{
		name:    "stock-price positive: keyword and question mark (new default, ex-negative)",
		text:    "Can you explain what stock price means?",
		wantHit: true,
		wantID:  "stock-price",
	},

	// --- exchange-rate ---
	{
		name:    "exchange-rate positive: USD and CNY entity",
		text:    "What is the exchange rate between USD and CNY?",
		wantHit: true,
		wantID:  "exchange-rate",
	},
	{
		name: "exchange-rate positive: EUR entity — no time-current keyword",
		// "today" removed so time-current does not fire first; EUR provides entity.
		text:    "What is the EUR to USD exchange rate?",
		wantHit: true,
		wantID:  "exchange-rate",
	},
	{
		// Product change: bare "exchange rates" without entity now fires.
		// Accepted false positive.
		name:    "exchange-rate positive: bare keyword (new default, ex-negative)",
		text:    "How do exchange rates work?",
		wantHit: true,
		wantID:  "exchange-rate",
	},

	// --- weather ---
	{
		name: "weather positive: basic question without time-current keyword",
		// "today" removed; RequireEntity=false for weather so no entity needed.
		text:    "What's the weather in Beijing?",
		wantHit: true,
		wantID:  "weather",
	},
	{
		name: "weather positive: temperature question without time-current keyword",
		// "right now" removed.
		text:    "What is the temperature outside?",
		wantHit: true,
		wantID:  "weather",
	},
	{
		// Product change: keyword "weather" without "?" now matches.
		// Keyword presence is the gate; extra upstream call is the accepted cost.
		name:    "weather positive: keyword without question mark (new default)",
		text:    "The weather was great yesterday.",
		wantHit: true,
		wantID:  "weather",
	},

	// --- news ---
	{
		name:    "news positive: 'breaking' question",
		text:    "Any breaking news from Washington?",
		wantHit: true,
		wantID:  "news",
	},
	{
		name:    "news positive: 'headline' question",
		text:    "What are the headlines for tomorrow?",
		wantHit: true,
		wantID:  "news",
	},
	{
		name:    "news positive: keyword without question mark (new default)",
		text:    "Let me summarize the headlines.",
		wantHit: true,
		wantID:  "news",
	},

	// --- score ---
	{
		name: "score positive: basic question without 'current'",
		// "current" is in the time-current keyword list, so use a prompt
		// that does not contain it to ensure score fires, not time-current.
		text:    "What's the score of the game?",
		wantHit: true,
		wantID:  "score",
	},
	{
		name:    "score positive: keyword without question mark (new default)",
		text:    "The final score was 3-1.",
		wantHit: true,
		wantID:  "score",
	},

	// --- ZH seed rules ---
	{
		name: "ZH stock-price positive: 股价 with entity, no time-current keyword in scope",
		// 股价 keyword (stock-price rule) fires; AAPL is the entity.
		// 现在 is a time-current keyword too — time-current fires first!
		// Use a prompt without 现在/今天/最新/当前 to isolate the stock-price rule.
		text:    "AAPL的股价是多少？",
		wantHit: true,
		wantID:  "stock-price",
	},
	{
		// ZH discourse "现在" without question mark matches time-current.
		// The keyword carries the signal; the rare discourse-particle false
		// positive is the accepted cost vs the common-case win of
		// "现在几点" / "现在天气" returning fresh data.
		name:    "ZH time-current positive: 现在 without question mark (new default)",
		text:    "现在我们来讨论一下这个问题。",
		wantHit: true,
		wantID:  "time-current",
	},
	{
		name:    "ZH time-current positive: 今天 with question mark",
		text:    "今天的情况怎么样？",
		wantHit: true,
		wantID:  "time-current",
	},
	{
		name: "ZH weather positive: 天气 with question mark — time-current fires first",
		// 今天 triggers time-current before weather can match; the result must
		// still be wantHit=true (a rule fired). We allow any matching ruleID.
		// This is correct behaviour per SDD: "first matching rule wins".
		text:    "今天的天气怎么样？",
		wantHit: true,
		wantID:  "", // any rule
	},
	{
		name:    "ZH weather positive: 天气 without time-current keywords",
		text:    "北京的天气怎么样？",
		wantHit: true,
		wantID:  "weather",
	},
	{
		name: "ZH exchange-rate positive: 汇率 with CNY entity — time-current fires first",
		// 现在 is a time-current keyword.  The result must still be a hit.
		text:    "现在的汇率是多少？CNY对USD？",
		wantHit: true,
		wantID:  "", // any rule
	},
	{
		name:    "ZH exchange-rate positive: 汇率 with CNY, no time-current keyword",
		text:    "美元兑CNY的汇率是多少？",
		wantHit: true,
		wantID:  "exchange-rate",
	},
	{
		name: "ZH news positive: 新闻 with question mark — time-current fires first",
		// 今天 is a time-current keyword; the result is still a hit.
		text:    "今天有什么新闻？",
		wantHit: true,
		wantID:  "", // any rule
	},
	{
		name:    "ZH news positive: 新闻 without time-current keyword",
		text:    "有什么突发新闻？",
		wantHit: true,
		wantID:  "news",
	},

	// --- accepted-false-positives (post RequireQuestionMark=false change) ---
	// These would have been "discourse particle" false-positive negatives under
	// the old RequireQuestionMark=true policy. Now they fire intentionally:
	// the cost (one extra upstream call) is dwarfed by the win on conversational
	// prompts like "现在几点" that previously slipped through.
	{
		name:    "discourse 'now' (intentional false positive)",
		text:    "Use this now to understand the flow.",
		wantHit: true,
		wantID:  "time-current",
	},
	{
		name:    "discourse 'today' (intentional false positive)",
		text:    "Today we will cover dependency injection.",
		wantHit: true,
		wantID:  "time-current",
	},
	{
		name:    "discourse 'latest' (intentional false positive)",
		text:    "The latest version was just released.",
		wantHit: true,
		wantID:  "time-current",
	},

	// --- RequireEntity regression cases ---
	// "当前中国有多少人口?" returned no_match previously because (a) the
	// population rule had RequireEntity=true and (b) the entity heuristic
	// does not recognise Chinese place names. Both gates are now off; both
	// versions of the prompt fire.
	{
		name:    "regression: 当前中国有多少人口? — time-current fires first",
		text:    "当前中国有多少人口?",
		wantHit: true,
		wantID:  "time-current",
	},
	{
		name:    "regression: 中国有多少人口 — population fires when 当前 absent",
		text:    "中国有多少人口",
		wantHit: true,
		wantID:  "population",
	},
}

func TestIsTimeSensitive_SeedRules(t *testing.T) {
	d := testDetector(t)
	for _, tc := range seedRuleCases {

		t.Run(tc.name, func(t *testing.T) {
			matched, ruleID := d.IsTimeSensitive(msgs(tc.text))
			if matched != tc.wantHit {
				t.Errorf("IsTimeSensitive(%q) matched=%v want=%v (ruleID=%q)", tc.text, matched, tc.wantHit, ruleID)
			}
			// wantID=="" means "any matching rule is acceptable" (used when
			// multiple rules overlap on the same prompt and the first-match
			// ordering is well-defined but not the focus of the test).
			if tc.wantHit && tc.wantID != "" && ruleID != tc.wantID {
				t.Errorf("IsTimeSensitive(%q) ruleID=%q want=%q", tc.text, ruleID, tc.wantID)
			}
			if !tc.wantHit && ruleID != "" {
				t.Errorf("IsTimeSensitive(%q) expected empty ruleID on no-match, got %q", tc.text, ruleID)
			}
		})
	}
}

// --- lastUserText ---

func TestLastUserText_ReturnsLastUser(t *testing.T) {
	messages := []ChatMessage{
		{Role: "system", Content: "You are helpful."},
		{Role: "user", Content: "First question?"},
		{Role: "assistant", Content: "First answer."},
		{Role: "user", Content: "Second question?"},
	}
	text := lastUserText(messages)
	if text != "Second question?" {
		t.Errorf("expected 'Second question?', got %q", text)
	}
}

func TestLastUserText_NoUserMessage(t *testing.T) {
	messages := []ChatMessage{
		{Role: "system", Content: "Setup."},
		{Role: "assistant", Content: "Response."},
	}
	text := lastUserText(messages)
	if text != "" {
		t.Errorf("expected empty string, got %q", text)
	}
}

func TestLastUserText_EmptyMessages(t *testing.T) {
	if text := lastUserText(nil); text != "" {
		t.Errorf("expected empty string for nil messages, got %q", text)
	}
}

func TestLastUserText_RoleCaseInsensitive(t *testing.T) {
	messages := []ChatMessage{
		{Role: "USER", Content: "upper case role"},
	}
	text := lastUserText(messages)
	if text != "upper case role" {
		t.Errorf("expected 'upper case role', got %q", text)
	}
}

func TestIsTimeSensitive_NoUserMessage(t *testing.T) {
	d := testDetector(t)
	messages := []ChatMessage{
		{Role: "system", Content: "You are a helpful assistant."},
	}
	matched, ruleID := d.IsTimeSensitive(messages)
	if matched || ruleID != "" {
		t.Errorf("expected no match for system-only conversation; got matched=%v ruleID=%q", matched, ruleID)
	}
}

func TestIsTimeSensitive_EmptyMessages(t *testing.T) {
	d := testDetector(t)
	matched, ruleID := d.IsTimeSensitive(nil)
	if matched || ruleID != "" {
		t.Errorf("expected no match for nil messages; got matched=%v ruleID=%q", matched, ruleID)
	}
}

// --- Reload ---

func TestReload_SwapsRuleSet(t *testing.T) {
	reg := prometheus.NewRegistry()
	initialRules := []Rule{
		{ID: "r1", Keywords: []string{"initial keyword"}, RequireQuestionMark: true, Enabled: true},
	}
	d, err := NewDetector(initialRules, testLogger(), "nexus", reg)
	if err != nil {
		t.Fatalf("NewDetector: %v", err)
	}

	// Old rule should fire.
	if matched, _ := d.IsTimeSensitive(msgs("What is the initial keyword?")); !matched {
		t.Error("expected match for initial rule")
	}

	// Reload with a completely different rule.
	newRules := []Rule{
		{ID: "r2", Keywords: []string{"replacement keyword"}, RequireQuestionMark: true, Enabled: true},
	}
	if err := d.Reload(newRules); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	// Old rule must no longer fire.
	if matched, _ := d.IsTimeSensitive(msgs("What is the initial keyword?")); matched {
		t.Error("expected no match for old rule after Reload")
	}

	// New rule must fire.
	if matched, _ := d.IsTimeSensitive(msgs("What is the replacement keyword?")); !matched {
		t.Error("expected match for new rule after Reload")
	}
}

func TestReload_InvalidRuleKeepsExisting(t *testing.T) {
	reg := prometheus.NewRegistry()
	initialRules := []Rule{
		{ID: "r1", Keywords: []string{"weather"}, RequireQuestionMark: true, Enabled: true},
	}
	d, err := NewDetector(initialRules, testLogger(), "nexus", reg)
	if err != nil {
		t.Fatalf("NewDetector: %v", err)
	}

	// Attempt reload with invalid rule.
	badRules := []Rule{
		{ID: "bad", Keywords: []string{}, Enabled: true},
	}
	if err := d.Reload(badRules); err == nil {
		t.Fatal("expected error for invalid reload rule")
	}

	// Existing rule must still work.
	if matched, _ := d.IsTimeSensitive(msgs("What's the weather?")); !matched {
		t.Error("expected existing rule to still fire after failed reload")
	}
}

// --- Reload concurrency safety ---

func TestReload_ConcurrencySafety(t *testing.T) {
	reg := prometheus.NewRegistry()
	initialRules := loadSeedRulesJSON(t)
	d, err := NewDetector(initialRules, testLogger(), "nexus_conc", reg)
	if err != nil {
		t.Fatalf("NewDetector: %v", err)
	}
	// Cached rules used inside Reload goroutines — loadSeedRulesJSON cannot
	// be called concurrently because *testing.T methods are single-goroutine.
	reloadRules := loadSeedRulesJSON(t)

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				d.IsTimeSensitive(msgs("What is the weather today?"))
			}
		}()
	}
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				_ = d.Reload(reloadRules)
			}
		}()
	}
	wg.Wait()
}

// --- Prometheus counter ---

func TestIsTimeSensitive_IncrementsCounter(t *testing.T) {
	reg := prometheus.NewRegistry()
	rules := []Rule{
		{
			ID:                  "stock-price",
			Keywords:            []string{"stock price"},
			RequireQuestionMark: true,
			RequireEntity:       true,
			Languages:           []string{"en"},
			Enabled:             true,
		},
	}
	d, err := NewDetector(rules, testLogger(), "nexus_test", reg)
	if err != nil {
		t.Fatalf("NewDetector: %v", err)
	}

	// No match → counter must remain zero.
	d.IsTimeSensitive(msgs("Explain stock price as a concept."))

	// Match → counter must increment.
	matched, ruleID := d.IsTimeSensitive(msgs("What is the stock price of AAPL?"))
	if !matched {
		t.Fatal("expected match")
	}
	if ruleID != "stock-price" {
		t.Fatalf("unexpected ruleID %q", ruleID)
	}

	// Gather metrics and verify the counter incremented exactly once.
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	var found bool
	for _, mf := range mfs {
		if mf.GetName() == "nexus_test_cache_freshness_skips_total" {
			for _, m := range mf.GetMetric() {
				// Find the metric with rule_id=stock-price.
				for _, lp := range m.GetLabel() {
					if lp.GetName() == "rule_id" && lp.GetValue() == "stock-price" {
						if m.GetCounter().GetValue() != 1 {
							t.Errorf("expected counter=1, got %v", m.GetCounter().GetValue())
						}
						found = true
					}
				}
			}
		}
	}
	if !found {
		t.Error("metric nexus_test_cache_freshness_skips_total{rule_id=stock-price} not found after match")
	}
}

// --- freshnessMetrics nil-safety ---

func TestFreshnessMetrics_NilSafeRecordSkip(t *testing.T) {
	// A nil *freshnessMetrics must not panic on recordSkip.
	var m *freshnessMetrics
	m.recordSkip("stock-price", "en") // must not panic
}

// --- ruleLanguageLabel ---

func TestRuleLanguageLabel(t *testing.T) {
	cases := []struct {
		langs []string
		want  string
	}{
		{nil, "any"},
		{[]string{}, "any"},
		{[]string{"en"}, "en"},
		{[]string{"zh"}, "zh"},
		{[]string{"en", "zh"}, "en"},
	}
	for _, tc := range cases {
		if got := ruleLanguageLabel(tc.langs); got != tc.want {
			t.Errorf("ruleLanguageLabel(%v) = %q, want %q", tc.langs, got, tc.want)
		}
	}
}

// --- Seed JSON sanity ---

// These assertions guard the canonical seed JSON file: it must parse, contain
// the rules the conformance suite assumes are present, and compile cleanly.

func TestSeedJSON_AtLeast10RulesAndUnique(t *testing.T) {
	rules := loadSeedRulesJSON(t)
	if len(rules) < 10 {
		t.Errorf("seed JSON has %d rules, want ≥10", len(rules))
	}
	seen := map[string]bool{}
	for _, r := range rules {
		if r.ID == "" {
			t.Errorf("rule with empty ID: %+v", r)
		}
		if seen[r.ID] {
			t.Errorf("duplicate rule ID %q in seed JSON", r.ID)
		}
		seen[r.ID] = true
		if !r.Enabled {
			t.Errorf("rule %q ships disabled — seed defaults must be enabled", r.ID)
		}
		if len(r.Languages) == 0 {
			t.Errorf("rule %q has no languages — set at least one tag", r.ID)
		}
		// Both gates default to false. Keyword strength carries the signal;
		// the gates were Chinese-prompt-hostile.
		if r.RequireQuestionMark {
			t.Errorf("rule %q has RequireQuestionMark=true — must default to false", r.ID)
		}
		if r.RequireEntity {
			t.Errorf("rule %q has RequireEntity=true — must default to false (Chinese place names are not entities under the heuristic)", r.ID)
		}
	}
}

func TestSeedJSON_CompilesWithoutError(t *testing.T) {
	reg := prometheus.NewRegistry()
	_, err := NewDetector(loadSeedRulesJSON(t), testLogger(), "nexus_seed_compile", reg)
	if err != nil {
		t.Fatalf("seed JSON failed to compile: %v", err)
	}
}
