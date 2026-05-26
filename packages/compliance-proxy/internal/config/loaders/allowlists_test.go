package loaders

import (
	"strings"
	"testing"
)

// The DB-bound LoadDomainAllowlist path is exercised end-to-end against
// a real Postgres; here we test the pure-logic normaliser + ensurePort
// helpers that govern how InterceptionDomain rows map into allowlist
// entries.

func TestEnsurePort_AppendsDefaultIfMissing(t *testing.T) {
	if got := ensurePort("api.openai.com"); got != "api.openai.com:443" {
		t.Errorf("missing port: got %q, want %q", got, "api.openai.com:443")
	}
}

func TestEnsurePort_PreservesExistingPort(t *testing.T) {
	cases := []string{"api.openai.com:443", "api.openai.com:8443", "host:80", "[::1]:443"}
	for _, in := range cases {
		if got := ensurePort(in); got != in {
			t.Errorf("explicit port stripped: in=%q got=%q", in, got)
		}
	}
}

func TestNormalizeToAllowlistEntry_ExactPattern(t *testing.T) {
	// EXACT match type passes through with port appended.
	if got := normalizeToAllowlistEntry("api.openai.com", "EXACT"); got != "api.openai.com:443" {
		t.Errorf("EXACT: %q", got)
	}
	// Pattern with explicit port honoured.
	if got := normalizeToAllowlistEntry("api.openai.com:8443", "EXACT"); got != "api.openai.com:8443" {
		t.Errorf("EXACT explicit port: %q", got)
	}
}

func TestNormalizeToAllowlistEntry_GlobPattern(t *testing.T) {
	// GLOB *.openai.com → *.openai.com:443 (passes through to the
	// allowlist's own glob matching).
	if got := normalizeToAllowlistEntry("*.openai.com", "GLOB"); got != "*.openai.com:443" {
		t.Errorf("GLOB: %q", got)
	}
}

func TestNormalizeToAllowlistEntry_PrefixPattern(t *testing.T) {
	// PREFIX (uncommon for domains) — best-effort exact entry.
	if got := normalizeToAllowlistEntry("api.openai", "PREFIX"); got != "api.openai:443" {
		t.Errorf("PREFIX: %q", got)
	}
}

func TestNormalizeToAllowlistEntry_RegexRejected(t *testing.T) {
	// REGEX patterns can't be cleanly mapped to allowlist entries.
	// Returning "" causes LoadDomainAllowlist to skip the row — the
	// regex must be evaluated by the full traffic matching layer
	// instead, not the simple allowlist check.
	if got := normalizeToAllowlistEntry(`^api\.openai\.com$`, "REGEX"); got != "" {
		t.Errorf("REGEX should be rejected: %q", got)
	}
}

func TestNormalizeToAllowlistEntry_EmptyHostReturnsEmpty(t *testing.T) {
	if got := normalizeToAllowlistEntry("", "EXACT"); got != "" {
		t.Errorf("empty host should yield empty: %q", got)
	}
	if got := normalizeToAllowlistEntry("   ", "GLOB"); got != "" {
		t.Errorf("whitespace-only host should yield empty: %q", got)
	}
}

func TestNormalizeToAllowlistEntry_TrimsWhitespace(t *testing.T) {
	// Operator-entered values may have stray whitespace; the loader
	// must normalise so the access layer's lookup is deterministic.
	if got := normalizeToAllowlistEntry("  api.openai.com  ", "EXACT"); got != "api.openai.com:443" {
		t.Errorf("trim: %q", got)
	}
}

func TestNormalizeToAllowlistEntry_CaseInsensitiveMatchType(t *testing.T) {
	// MatchType is stored as Postgres ENUM uppercase; the loader's
	// strings.ToUpper guards against any caller passing lowercase
	// (e.g. derived from a JSON column).
	for _, mt := range []string{"exact", "Exact", "EXACT", "ExAcT"} {
		if got := normalizeToAllowlistEntry("api.x.com", mt); got != "api.x.com:443" {
			t.Errorf("matchType %q: got %q", mt, got)
		}
	}
}

func TestNormalizeToAllowlistEntry_UnknownTypeBestEffort(t *testing.T) {
	// Unknown match types fall through to the default branch (best-effort
	// exact entry with port). Drift in the ENUM at the DB layer thus
	// degrades gracefully rather than dropping the row.
	if got := normalizeToAllowlistEntry("api.x.com", "FUTURE_TYPE"); got != "api.x.com:443" {
		t.Errorf("unknown type: %q", got)
	}
}

// buildAllowlistEntries tests cover the deduplication + skip-on-empty
// pipeline that runs after the SQL caller scans rows. The DB query path
// itself is exercised end-to-end against a real Postgres in
// scenarios/proxy smoke tests; here we drive every branch of the
// in-memory loop without a live DB.

func TestBuildAllowlistEntries_EmptyInputYieldsNil(t *testing.T) {
	// Calling with no rows must NOT allocate a non-nil empty slice — the
	// caller relies on `len(entries) == 0` to skip its allowlist refresh
	// branch, so the contract is "nil = no rows".
	if got := buildAllowlistEntries(nil); got != nil {
		t.Errorf("nil input must yield nil result; got %#v", got)
	}
	if got := buildAllowlistEntries([]AllowlistRow{}); got != nil {
		t.Errorf("empty slice input must yield nil result; got %#v", got)
	}
}

func TestBuildAllowlistEntries_PreservesInputOrder(t *testing.T) {
	// The SQL caller sorts by priority DESC, created_at ASC. The pure
	// helper must preserve that order so the data-plane allowlist
	// honors priority. Any reshuffle here would silently demote a
	// high-priority domain in the matcher.
	rows := []AllowlistRow{
		{HostPattern: "api.openai.com", MatchType: "EXACT"},
		{HostPattern: "api.anthropic.com", MatchType: "EXACT"},
		{HostPattern: "*.cohere.ai", MatchType: "GLOB"},
	}
	got := buildAllowlistEntries(rows)
	want := []string{"api.openai.com:443", "api.anthropic.com:443", "*.cohere.ai:443"}
	if len(got) != len(want) {
		t.Fatalf("entry count: got %d, want %d (entries=%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("position %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestBuildAllowlistEntries_RegexRowsSkipped(t *testing.T) {
	// REGEX rows are unrepresentable as simple allowlist entries —
	// normalizeToAllowlistEntry returns "" — and the loop's `entry ==
	// ""` guard must drop them. Skipping is the binding contract:
	// admitting them as-is would put a regex literal into the matcher's
	// exact-match table and silently fail to allow the intended traffic.
	rows := []AllowlistRow{
		{HostPattern: "api.openai.com", MatchType: "EXACT"},
		{HostPattern: `^api\.openai\.com$`, MatchType: "REGEX"},
		{HostPattern: "api.anthropic.com", MatchType: "EXACT"},
	}
	got := buildAllowlistEntries(rows)
	if len(got) != 2 {
		t.Fatalf("REGEX rows must be dropped; got %v", got)
	}
	for _, g := range got {
		if strings.Contains(g, "^") || strings.Contains(g, "\\") {
			t.Errorf("REGEX literal leaked into allowlist: %q", g)
		}
	}
}

func TestBuildAllowlistEntries_EmptyHostPatternSkipped(t *testing.T) {
	// A row with an empty / whitespace host_pattern is dropped — the
	// normaliser returns "" and the loop skips it. This guards against
	// admin-UI bugs that submit a blank row.
	rows := []AllowlistRow{
		{HostPattern: "", MatchType: "EXACT"},
		{HostPattern: "  ", MatchType: "GLOB"},
		{HostPattern: "api.openai.com", MatchType: "EXACT"},
	}
	got := buildAllowlistEntries(rows)
	if len(got) != 1 || got[0] != "api.openai.com:443" {
		t.Errorf("blank rows must be dropped; got %v", got)
	}
}

func TestBuildAllowlistEntries_DuplicatesCollapsed(t *testing.T) {
	// Two domain rows that normalise to the same entry must produce a
	// single allowlist entry. The DomainAllowlist matcher uses a map
	// keyed on the entry string, so duplicates are silently merged
	// downstream — but emitting duplicates would inflate the slice and
	// (in earlier versions) confuse callers that count entries for
	// observability.
	rows := []AllowlistRow{
		{HostPattern: "api.openai.com", MatchType: "EXACT"},
		{HostPattern: "api.openai.com", MatchType: "EXACT"},
		{HostPattern: "api.openai.com:443", MatchType: "EXACT"}, // same after ensurePort
	}
	got := buildAllowlistEntries(rows)
	if len(got) != 1 {
		t.Errorf("duplicates must collapse to one entry; got %v", got)
	}
	if got[0] != "api.openai.com:443" {
		t.Errorf("dedup keeper wrong: %q", got[0])
	}
}

func TestBuildAllowlistEntries_MixedTypesAllNormalised(t *testing.T) {
	// EXACT + GLOB + PREFIX all coexist; REGEX drops out. Use distinct
	// hosts so dedup does not collapse them and we can assert each
	// type's output shape.
	rows := []AllowlistRow{
		{HostPattern: "api.openai.com", MatchType: "EXACT"},
		{HostPattern: "*.anthropic.com", MatchType: "GLOB"},
		{HostPattern: "api.cohere", MatchType: "PREFIX"},
		{HostPattern: `^skipme$`, MatchType: "REGEX"},
	}
	got := buildAllowlistEntries(rows)
	wantSet := map[string]bool{
		"api.openai.com:443":  true,
		"*.anthropic.com:443": true,
		"api.cohere:443":      true,
	}
	if len(got) != 3 {
		t.Fatalf("entry count wrong: got %d (%v), want 3", len(got), got)
	}
	for _, g := range got {
		if !wantSet[g] {
			t.Errorf("unexpected entry %q", g)
		}
	}
}
