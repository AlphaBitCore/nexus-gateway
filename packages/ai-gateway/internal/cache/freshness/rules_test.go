package freshness

import (
	"strings"
	"testing"
)

// matchText drives compiledRule.matches the way IsTimeSensitive does in
// production: the candidate text is lowercased once and passed alongside the
// original. Tests assert the substring matcher yields the same verdicts the
// previous (?i)-regex matcher did (differential gate).
func matchText(cr *compiledRule, text string) bool {
	return cr.matches(text, strings.ToLower(text))
}

// --- compile / compileAll ---

func TestCompile_ErrorOnEmptyKeywords(t *testing.T) {
	r := Rule{ID: "test", Keywords: []string{}, Enabled: true}
	_, err := compile(r)
	if err == nil {
		t.Fatal("expected error for empty keywords, got nil")
	}
}

func TestCompile_ErrorOnBlankKeyword(t *testing.T) {
	r := Rule{ID: "test", Keywords: []string{"  "}, Enabled: true}
	_, err := compile(r)
	if err == nil {
		t.Fatal("expected error for blank keyword, got nil")
	}
}

func TestCompile_ValidRule(t *testing.T) {
	r := Rule{
		ID:       "test",
		Keywords: []string{"stock price"},
		Enabled:  true,
	}
	cr, err := compile(r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cr == nil {
		t.Fatal("expected non-nil compiledRule")
	}
}

func TestCompileAll_SkipsDisabledRules(t *testing.T) {
	rules := []Rule{
		{ID: "enabled-rule", Keywords: []string{"foo"}, Enabled: true},
		{ID: "disabled-rule", Keywords: []string{"bar"}, Enabled: false},
	}
	compiled, err := compileAll(rules)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(compiled) != 1 {
		t.Fatalf("expected 1 compiled rule, got %d", len(compiled))
	}
	if compiled[0].rule.ID != "enabled-rule" {
		t.Fatalf("expected 'enabled-rule', got %q", compiled[0].rule.ID)
	}
}

func TestCompileAll_ErrorOnEmptyID(t *testing.T) {
	rules := []Rule{
		{ID: "", Keywords: []string{"foo"}, Enabled: true},
	}
	_, err := compileAll(rules)
	if err == nil {
		t.Fatal("expected error for empty rule ID, got nil")
	}
}

func TestCompileAll_ErrorOnInvalidRule(t *testing.T) {
	rules := []Rule{
		{ID: "bad", Keywords: []string{}, Enabled: true},
	}
	_, err := compileAll(rules)
	if err == nil {
		t.Fatal("expected error for empty keywords, got nil")
	}
}

func TestCompileAll_EmptyList(t *testing.T) {
	compiled, err := compileAll(nil)
	if err != nil {
		t.Fatalf("unexpected error on nil input: %v", err)
	}
	if len(compiled) != 0 {
		t.Fatalf("expected 0 compiled rules, got %d", len(compiled))
	}
}

// --- matches ---

func TestCompiledRule_CaseInsensitiveKeyword(t *testing.T) {
	cr, err := compile(Rule{
		ID:       "r1",
		Keywords: []string{"Stock Price"},
		Enabled:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		text string
		want bool
	}{
		{"What is the stock price of AAPL?", true},
		{"What is the STOCK PRICE of AAPL?", true},
		{"STOCK PRICE of AAPL?", true},
		{"No relevant content", false},
	}
	for _, tc := range cases {
		if got := matchText(cr, tc.text); got != tc.want {
			t.Errorf("matches(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
}

func TestCompiledRule_RequireQuestionMark(t *testing.T) {
	cr, err := compile(Rule{
		ID:                  "r2",
		Keywords:            []string{"weather"},
		RequireQuestionMark: true,
		Enabled:             true,
	})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		text string
		want bool
	}{
		{"What's the weather?", true},           // ASCII question mark
		{"天气怎么样？", false},                       // has full-width ? but no keyword "weather"
		{"The weather is nice today.", false},   // no question mark
		{"Tell me about weather.", false},       // no question mark
		{"What about the weather today?", true}, // has question mark
	}
	for _, tc := range cases {
		if got := matchText(cr, tc.text); got != tc.want {
			t.Errorf("matches(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
}

func TestCompiledRule_FullWidthQuestionMark(t *testing.T) {
	cr, err := compile(Rule{
		ID:                  "r-fw",
		Keywords:            []string{"天气"},
		RequireQuestionMark: true,
		Enabled:             true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Full-width question mark should satisfy the requirement.
	if !matchText(cr, "今天的天气怎么样？") {
		t.Error("expected match for full-width question mark")
	}
	// No question mark should not match.
	if matchText(cr, "今天的天气很好。") {
		t.Error("expected no match without question mark")
	}
}

func TestCompiledRule_RequireEntity(t *testing.T) {
	cr, err := compile(Rule{
		ID:            "r3",
		Keywords:      []string{"stock price"},
		RequireEntity: true,
		Enabled:       true,
	})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		text string
		want bool
		desc string
	}{
		{"What is the stock price of AAPL?", true, "uppercase ticker AAPL"},
		{"What is the stock price of Apple?", false, "no entity — 'Apple' has only one capital letter per run after word boundary"},
		{"What is the stock price for $100?", true, "currency symbol $"},
		{"stock price as a metaphor without entity.", false, "no entity"},
		{"Current stock price: USD 50.00?", true, "currency code USD"},
	}
	for _, tc := range cases {
		if got := matchText(cr, tc.text); got != tc.want {
			t.Errorf("[%s] matches(%q) = %v, want %v", tc.desc, tc.text, got, tc.want)
		}
	}
}

// --- entityHeuristic ---

func TestEntityHeuristic(t *testing.T) {
	cases := []struct {
		text string
		want bool
		desc string
	}{
		{"What is the price of AAPL?", true, "uppercase ticker"},
		{"BTC price now?", true, "currency code BTC"},
		{"price is $50?", true, "currency symbol $"},
		{"price is €100?", true, "currency symbol €"},
		{"¥500 today?", true, "currency symbol ¥"},
		{"price is 50 dollars?", true, "two-digit number"},
		{"价格是多少元？", true, "ZH currency word 元"},
		{"USD rate today?", true, "currency code USD"},
		{"CNY exchange?", true, "currency code CNY"},
		{"Use this now.", false, "no entity — single uppercase 'U' starts word"},
		{"It's great.", false, "no entity at all"},
		{"S&P 500 today?", true, "S is single uppercase, but 500 is a 3-digit number"},
		{"price is 5 dollars?", false, "single digit, not ≥2"},
		{"price is 55 dollars?", true, "two-digit number 55"},
		{"美元兑人民币？", true, "ZH currency word 美元"},
		{"欧元汇率？", true, "ZH currency word 欧元"},
		{"英镑对美元？", true, "ZH currency word 英镑"},
	}
	for _, tc := range cases {
		if got := entityHeuristic(tc.text); got != tc.want {
			t.Errorf("[%s] entityHeuristic(%q) = %v, want %v", tc.desc, tc.text, got, tc.want)
		}
	}
}

func TestEntityHeuristic_UppercaseRun(t *testing.T) {
	// Two or more consecutive ASCII uppercase letters = ticker-like entity.
	if !entityHeuristic("TSLA is moving") {
		t.Error("expected entity for TSLA")
	}
	if !entityHeuristic("The GDP report") {
		t.Error("expected entity for GDP")
	}
	// Single uppercase (e.g. sentence-initial "I") should NOT count.
	if entityHeuristic("I agree.") {
		t.Error("single uppercase 'I' should not count as entity")
	}
}

func TestEntityHeuristic_DigitRun(t *testing.T) {
	// A run of ≥2 digits triggers the entity heuristic.
	if !entityHeuristic("price 42") {
		t.Error("expected entity for 2-digit number")
	}
	if !entityHeuristic("price 1000") {
		t.Error("expected entity for 4-digit number")
	}
	if entityHeuristic("x1y") {
		t.Error("single digit should NOT count")
	}
}

func TestEntityHeuristic_UppercaseRunAtEndOfString(t *testing.T) {
	// The flush branch at end-of-string triggers when text ends with ≥2
	// uppercase letters and no non-uppercase rune follows to flush the run.
	if !entityHeuristic("what is AAPL") {
		t.Error("expected entity for uppercase run at end of string")
	}
	if !entityHeuristic("check BTC") {
		t.Error("expected entity for BTC at end of string")
	}
	// Single uppercase at end: must NOT count.
	if entityHeuristic("check A") {
		t.Error("single uppercase letter at end should not count")
	}
}
