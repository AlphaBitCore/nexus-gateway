package freshness

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// Rule describes a single time-sensitive pattern that the Detector evaluates
// against incoming chat messages.
//
// Detection algorithm for a single rule (all conditions are AND-ed):
//  1. At least one keyword matches the text case-insensitively.
//  2. If RequireQuestionMark is true, the text contains "?" or "？".
//  3. If RequireEntity is true, the entity heuristic returns true (see
//     entityHeuristic in this file for the exact definition).
//
// Disabled rules are excluded from the compiled rule set at NewDetector time
// and have no effect until a Reload is called with Enabled=true.
type Rule struct {
	// ID is a stable, human-readable identifier (e.g. "stock-price"). It is
	// used as the rule_id Prometheus label. Must be non-empty for the rule to
	// be accepted.
	ID string `json:"id"`

	// Keywords is the list of phrases to match (case-insensitive substring
	// match). At least one keyword must match for the rule to proceed to the
	// co-occurrence checks. The list must be non-empty.
	Keywords []string `json:"keywords"`

	// RequireQuestionMark, when true, requires the message to contain a
	// literal "?" (ASCII) or "？" (U+FF1F, fullwidth) before the rule fires.
	// Use this to prevent discourse-particle false positives ("Use this now"
	// vs "What is the price now?").
	RequireQuestionMark bool `json:"requireQuestionMark"`

	// RequireEntity, when true, requires an entity heuristic match before the
	// rule fires. The heuristic detects: uppercase ticker-like tokens (≥2
	// uppercase ASCII letters), digits of two or more characters, currency
	// codes (USD, EUR, JPY, CNY, GBP, AUD, CAD, CHF, HKD, SGD, NZD, INR,
	// KRW, BTC, ETH), currency symbols ($ € £ ¥ ₩ ₹ ₿), and ZH currency
	// words (元, 美元, 欧元, 港元, 日元, 英镑). Named entities such as
	// "Bitcoin", "S&P 500", and well-known company names in sentence-initial
	// position are caught by the uppercase-letter and digit heuristics.
	RequireEntity bool `json:"requireEntity"`

	// Languages lists the BCP-47 language tags this rule applies to
	// ("en", "zh"). An empty slice means the rule applies to all languages.
	// S1 does not perform language detection — the field is stored for future
	// use by S2 (Hub shadow) and passed through to the Prometheus label.
	Languages []string `json:"languages"`

	// Enabled controls whether this rule participates in detection. Disabled
	// rules are excluded at compile time; Reload is required to re-enable.
	Enabled bool `json:"enabled"`
}

// compiledRule is the internal representation built from a Rule. Regex
// compilation is done once at NewDetector / Reload time to avoid repeated
// compilation on the hot path.
type compiledRule struct {
	rule     Rule
	patterns []*regexp.Regexp // one pattern per keyword, case-insensitive
}

// matches reports whether the compiled rule fires for the given text.
//
// All conditions are AND-ed:
//  1. At least one keyword regex matches.
//  2. If RequireQuestionMark: text contains "?" or "？".
//  3. If RequireEntity: entityHeuristic returns true.
func (cr *compiledRule) matches(text string) bool {
	// Step 1 — keyword match (any keyword is sufficient).
	keywordMatched := false
	for _, p := range cr.patterns {
		if p.MatchString(text) {
			keywordMatched = true
			break
		}
	}
	if !keywordMatched {
		return false
	}

	// Step 2 — question-mark co-occurrence.
	if cr.rule.RequireQuestionMark {
		if !strings.Contains(text, "?") && !strings.Contains(text, "？") {
			return false
		}
	}

	// Step 3 — entity co-occurrence.
	if cr.rule.RequireEntity {
		if !entityHeuristic(text) {
			return false
		}
	}

	return true
}

// compile builds a compiledRule from r. Returns an error if any keyword is
// empty or produces an invalid regex.
func compile(r Rule) (*compiledRule, error) {
	if len(r.Keywords) == 0 {
		return nil, fmt.Errorf("rule %q: keywords must not be empty", r.ID)
	}
	patterns := make([]*regexp.Regexp, 0, len(r.Keywords))
	for _, kw := range r.Keywords {
		if strings.TrimSpace(kw) == "" {
			return nil, fmt.Errorf("rule %q: keyword must not be blank", r.ID)
		}
		// (?i) = case-insensitive; regexp.QuoteMeta escapes any regex
		// metacharacters in the keyword literal. We intentionally use a
		// simple substring match rather than word-boundary anchors because
		// multi-word phrases (e.g. "exchange rate") must match across
		// language boundaries where word-boundary semantics differ.
		p, err := regexp.Compile("(?i)" + regexp.QuoteMeta(kw))
		if err != nil {
			return nil, fmt.Errorf("rule %q keyword %q: %w", r.ID, kw, err)
		}
		patterns = append(patterns, p)
	}
	return &compiledRule{rule: r, patterns: patterns}, nil
}

// compileAll compiles every enabled rule in rs. Returns (compiled list, first
// error). Disabled rules are silently skipped. An empty ID is an error because
// the ID is used as a Prometheus label.
func compileAll(rs []Rule) ([]*compiledRule, error) {
	out := make([]*compiledRule, 0, len(rs))
	for _, r := range rs {
		if !r.Enabled {
			continue
		}
		if strings.TrimSpace(r.ID) == "" {
			return nil, fmt.Errorf("rule with empty ID is not allowed")
		}
		cr, err := compile(r)
		if err != nil {
			return nil, err
		}
		out = append(out, cr)
	}
	return out, nil
}

// --- Entity heuristic ---

// currencySymbols contains currency symbol runes recognised by the heuristic.
var currencySymbols = map[rune]bool{
	'$': true, '€': true, '£': true, '¥': true, '₩': true, '₹': true, '₿': true,
}

// zhCurrencyWords are ZH currency phrases whose presence counts as an entity.
var zhCurrencyWords = []string{"元", "美元", "欧元", "港元", "日元", "英镑", "人民币"}

// entityHeuristic returns true when text contains at least one entity
// indicator. The indicators (in evaluation order, short-circuit on first hit):
//  1. A currency symbol ($, €, £, ¥, ₩, ₹, ₿).
//  2. A ZH currency word (元, 美元, 欧元, 港元, 日元, 英镑, 人民币).
//  3. A run of two or more ASCII decimal digits (price, year, percentage,
//     index value such as "500" in "S&P 500").
//  4. An uppercase word of two or more ASCII uppercase letters that is either
//     a known currency code (USD, CNY …) or a plausible ticker-like token
//     (AAPL, GOOG, TSLA, NVDA …). Single uppercase letters (like the pronoun
//     "I" at sentence start) are excluded.
//
// The heuristic is intentionally permissive — it is the co-occurrence
// condition, not the sole gating condition. A rule also requires a keyword
// match (and optionally a question mark), so false positives at this layer
// have limited impact.
func entityHeuristic(text string) bool {
	// Check currency symbols (rune-by-rune; most efficient path for short
	// messages that contain a $ or €).
	for _, r := range text {
		if currencySymbols[r] {
			return true
		}
	}

	// Check ZH currency words (substring match; ZH text is multi-byte UTF-8
	// so we work at the string level).
	for _, w := range zhCurrencyWords {
		if strings.Contains(text, w) {
			return true
		}
	}

	// Scan for digit runs and uppercase-letter runs in one pass.
	// We iterate over ASCII-range runes only; non-ASCII runes reset both
	// accumulators cleanly.
	digitRun := 0
	upperRun := 0

	for _, r := range text {
		switch {
		case r >= '0' && r <= '9':
			digitRun++
			if digitRun >= 2 {
				return true // two-or-more-digit number found
			}
			upperRun = 0
		case unicode.IsUpper(r) && r <= 'Z': // ASCII uppercase only
			upperRun++
			digitRun = 0
		default:
			// Check accumulated uppercase run. Any run of ≥2 consecutive ASCII
			// uppercase letters is treated as a ticker-like entity (AAPL, TSLA,
			// GDP, USD, BTC, EUR, CNY, …). Known currency codes and ticker codes
			// are both caught by this single condition.
			if upperRun >= 2 {
				return true
			}
			upperRun = 0
			digitRun = 0
		}
	}

	// Flush at end of string (handles text that ends with uppercase letters).
	if upperRun >= 2 {
		return true
	}

	return false
}
