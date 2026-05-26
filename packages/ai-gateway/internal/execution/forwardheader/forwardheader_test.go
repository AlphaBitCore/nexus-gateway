package forwardheader

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// allFormats matches providers.AllFormats() lower-cased; duplicated
// here to keep this package leaf-ish (no providers import).
func allFormats() []string {
	return []string{
		"openai", "deepseek", "glm", "azure-openai", "anthropic",
		"gemini", "minimax", "bedrock", "vertex", "cohere",
		"huggingface", "replicate", "mistral", "xai", "groq",
		"perplexity", "together", "fireworks", "moonshot",
	}
}

// TestResolve_DefaultsMatchHistoricalHardcoded — AC-FH-S1-01.
// Loading the embedded defaults produces the canonical per-format
// forward-header request sets for each provider format.
func TestResolve_DefaultsMatchHistoricalHardcoded(t *testing.T) {
	r, err := Resolve(DefaultConfig(), allFormats())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	cases := map[string][]string{
		"openai":    {"accept", "user-agent", "content-type", "openai-beta", "openai-organization", "openai-project"},
		"anthropic": {"accept", "user-agent", "content-type", "anthropic-beta", "anthropic-version"},
		"gemini":    {"accept", "user-agent", "content-type", "x-goog-user-project"},
		"vertex":    {"accept", "user-agent", "content-type", "x-goog-user-project"},
		"groq":      {"accept", "user-agent", "content-type"}, // OpenAI-compat sibling: no openai-beta
		"deepseek":  {"accept", "user-agent", "content-type"},
	}
	for f, want := range cases {
		got := r.Request(f)
		if len(got) != len(want) {
			t.Errorf("%s: got %d headers, want %d (%v)", f, len(got), len(want), got)
		}
		for _, h := range want {
			if _, ok := got[h]; !ok {
				t.Errorf("%s: missing %q", f, h)
			}
		}
	}

	// Per-adapter-type isolation: Groq must NOT inherit openai-beta.
	groq := r.Request("groq")
	if _, ok := groq["openai-beta"]; ok {
		t.Errorf("groq leaked openai-beta from openai")
	}
}

// TestResolve_HardDenylistFailFast — AC-FH-S1-03. Each denylist
// entry, regardless of which list it sits in, aborts Resolve.
func TestResolve_HardDenylistFailFast(t *testing.T) {
	for _, name := range []string{
		"authorization", "cookie", "set-cookie", "x-api-key",
		"x-goog-api-key", "api-key", "proxy-authorization", "x-real-ip",
		"www-authenticate", "strict-transport-security",
		"content-security-policy", "x-frame-options", "server",
		"via", "x-served-by", "cf-ray", "content-length",
		"transfer-encoding", "connection", "accept-encoding",
		// prefix matchers
		"x-amz-content-sha256", "x-forwarded-for", "x-nexus-aigw-via",
		"access-control-allow-origin",
	} {
		cfg := Config{Request: Direction{Base: []string{name}}}
		if _, err := Resolve(cfg, allFormats()); err == nil {
			t.Errorf("denylist entry %q in request.base did not error", name)
		}
		cfg = Config{Response: Direction{BaseStatic: []string{name}}}
		if _, err := Resolve(cfg, allFormats()); err == nil {
			t.Errorf("denylist entry %q in response.base.static did not error", name)
		}
	}
}

// TestResolve_RejectsUnknownAdapterType — AC-FH-S1-04.
func TestResolve_RejectsUnknownAdapterType(t *testing.T) {
	cfg := Config{Request: Direction{
		PerAdapterType: map[string]Entry{
			"open-ai": {Headers: []string{"x-foo"}}, // typo
		},
	}}
	_, err := Resolve(cfg, allFormats())
	if err == nil {
		t.Fatal("expected error on unknown adapter_type, got nil")
	}
	if !strings.Contains(err.Error(), "open-ai") {
		t.Errorf("error did not name offending key: %v", err)
	}
}

// TestResolve_RejectsStaticPerRequestOverlap — AC-FH-S2-05. A header
// listed in both static and perRequest for the same adapter type is
// ambiguous; Resolve must error.
func TestResolve_RejectsStaticPerRequestOverlap(t *testing.T) {
	cfg := Config{Response: Direction{
		PerAdapterType: map[string]Entry{
			"openai": {
				Static:     []string{"x-request-id"},
				PerRequest: []string{"x-request-id"},
			},
		},
	}}
	_, err := Resolve(cfg, allFormats())
	if err == nil {
		t.Fatal("expected overlap error, got nil")
	}
}

// TestResolve_PerAdapterTypeIsolation — AC-FH-S1-02 / -05. A header
// added to one adapter type does not leak to siblings.
func TestResolve_PerAdapterTypeIsolation(t *testing.T) {
	cfg := Config{Request: Direction{
		Base: []string{"accept"},
		PerAdapterType: map[string]Entry{
			"openai": {Headers: []string{"openai-x"}},
		},
	}}
	r, err := Resolve(cfg, allFormats())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if _, ok := r.Request("openai")["openai-x"]; !ok {
		t.Error("openai missing its own openai-x")
	}
	for _, sibling := range []string{"groq", "deepseek", "anthropic"} {
		if _, ok := r.Request(sibling)["openai-x"]; ok {
			t.Errorf("%s leaked openai-x", sibling)
		}
	}
}

// TestResolve_HashStableAndDistinguishing confirms Hash() is
// deterministic for identical configs and changes when any list
// changes — a required property for cache-key derivation.
func TestResolve_HashStableAndDistinguishing(t *testing.T) {
	cfgA := DefaultConfig()
	rA1, _ := Resolve(cfgA, allFormats())
	rA2, _ := Resolve(cfgA, allFormats())
	if rA1.Hash() != rA2.Hash() {
		t.Errorf("Hash not deterministic: %s vs %s", rA1.Hash(), rA2.Hash())
	}

	cfgB := DefaultConfig()
	cfgB.Request.Base = append([]string{"x-extra"}, cfgB.Request.Base...)
	if rB, _ := Resolve(cfgB, allFormats()); rB.Hash() == rA1.Hash() {
		t.Error("Hash unchanged after adding x-extra to base")
	}
}

func TestResolved_NilReceiverReturnsEmpty(t *testing.T) {
	// Defensive nil-receiver paths in Request/Response/Hash protect
	// callers that haven't wired an explicit allowlist (pre-Resolve
	// dispatch, test fixtures). Without these, dispatch would
	// nil-deref under construction races.
	var r *Resolved
	if got := r.Request("openai"); len(got) != 0 {
		t.Errorf("nil Request: got %d entries, want 0", len(got))
	}
	resp := r.Response("openai")
	if len(resp.Static) != 0 || len(resp.PerRequest) != 0 {
		t.Errorf("nil Response: got %+v, want empty sets", resp)
	}
	if got := r.Hash(); got != "" {
		t.Errorf("nil Hash: got %q", got)
	}
}

func TestResolved_UnknownFormatReturnsEmpty(t *testing.T) {
	// Format slugs that weren't registered via Resolve still return
	// safe-empty maps so caller iteration is always non-nil. This
	// matters for adapters added after a resolver snapshot was
	// computed.
	r := Default()
	if got := r.Request("nonexistent-format"); got == nil {
		t.Error("unknown format Request should yield empty (not nil) map")
	}
	if got := r.Response("nonexistent-format"); got.Static == nil {
		t.Error("unknown format Response.Static should be non-nil empty map")
	}
}

func TestUnmarshalYAML_AbsentBase(t *testing.T) {
	// A direction block with no `base:` key (just perAdapterType) must
	// decode cleanly — the Kind=0 branch.
	src := `perAdapterType:
  openai:
    headers: [x-openai-extra]
`
	var d Direction
	if err := yaml.Unmarshal([]byte(src), &d); err != nil {
		t.Fatalf("absent base: %v", err)
	}
	if len(d.PerAdapterType) != 1 {
		t.Errorf("perAdapterType not parsed: %+v", d.PerAdapterType)
	}
	if len(d.Base) != 0 || len(d.BaseStatic) != 0 || len(d.BasePerRequest) != 0 {
		t.Errorf("absent base should yield empty: %+v", d)
	}
}

func TestUnmarshalYAML_MalformedBaseRejected(t *testing.T) {
	// Scalar base (string instead of sequence or mapping) is a config
	// error — must reject explicitly so operators see the line and fix.
	src := `base: "not-a-sequence-or-mapping"
`
	var d Direction
	err := yaml.Unmarshal([]byte(src), &d)
	if err == nil {
		t.Fatal("scalar base should error")
	}
}

func TestUnmarshalYAML_MappingBaseSplitsStaticPerRequest(t *testing.T) {
	// Response-side YAML uses base as a mapping with static/perRequest
	// sub-keys. Pin this branch separately from the sequence branch.
	src := `base:
  static: [x-foo]
  perRequest: [x-bar]
`
	var d Direction
	if err := yaml.Unmarshal([]byte(src), &d); err != nil {
		t.Fatalf("mapping base: %v", err)
	}
	if len(d.BaseStatic) != 1 || d.BaseStatic[0] != "x-foo" {
		t.Errorf("BaseStatic: %+v", d.BaseStatic)
	}
	if len(d.BasePerRequest) != 1 || d.BasePerRequest[0] != "x-bar" {
		t.Errorf("BasePerRequest: %+v", d.BasePerRequest)
	}
}

func TestSetActiveAndActive_AtomicSwap(t *testing.T) {
	// SetActive replaces the global snapshot atomically; readers via
	// Active() must observe the new value immediately. Without atomic
	// swap, the data plane could see a nil allowlist briefly and forward
	// unsafe headers.
	prev := Active()
	t.Cleanup(func() { SetActive(prev) })

	SetActive(nil)
	if got := Active(); got != nil {
		t.Errorf("after SetActive(nil): Active() = %v", got)
	}

	r := Default()
	SetActive(r)
	if got := Active(); got != r {
		t.Errorf("after SetActive(r): Active() returned wrong instance")
	}
}

func TestDefault_StableAcrossCalls(t *testing.T) {
	// Default() must return the same *Resolved across calls
	// (sync.Once memoization). Without this, every adapter lookup
	// would re-parse the embedded YAML.
	r1 := Default()
	r2 := Default()
	if r1 != r2 {
		t.Errorf("Default() returned different pointers: %p vs %p", r1, r2)
	}
	if r1 == nil {
		t.Fatal("Default() returned nil")
	}
}

func TestUnmarshalYAML_MappingBaseMalformedSplitRejected(t *testing.T) {
	// base is a mapping (so we enter the mapping branch) but the inner
	// sub-keys have a wrong shape — `static: not-a-sequence` cannot decode
	// into []string. Pinning this separately from MalformedBaseRejected
	// (scalar base) so the mapping-branch decode error path is covered.
	src := `base:
  static: "not-a-sequence"
`
	var d Direction
	if err := yaml.Unmarshal([]byte(src), &d); err == nil {
		t.Fatal("mapping base with non-sequence static should error")
	}
}

func TestUnmarshalYAML_OuterRawDecodeError(t *testing.T) {
	// A top-level mapping where `perAdapterType` is the wrong shape
	// (scalar instead of map) trips the outer `value.Decode(&raw)` so the
	// first error branch in UnmarshalYAML is exercised. Without this
	// failure mode the error message would not surface the field name.
	src := `perAdapterType: "not-a-map"
`
	var d Direction
	if err := yaml.Unmarshal([]byte(src), &d); err == nil {
		t.Fatal("perAdapterType as scalar should error")
	}
}

// TestResolve_DenylistInBasePerRequest — covers the validateDirection
// loop over response.BasePerRequest. Without this, that loop's denylist
// trigger stays uncovered because every other test pins the denylist on
// BaseStatic / Base only.
func TestResolve_DenylistInBasePerRequest(t *testing.T) {
	cfg := Config{
		Response: Direction{
			BasePerRequest: []string{"authorization"},
		},
	}
	if _, err := Resolve(cfg, allFormats()); err == nil {
		t.Fatal("authorization in response.base.perRequest must be rejected")
	} else if !strings.Contains(err.Error(), "authorization") {
		t.Errorf("error did not name the denied header: %v", err)
	}
}

// TestResolve_DenylistInPerAdapterEntryFields covers the three inner
// loops in validateDirection: PerAdapterType[k].Headers / Static /
// PerRequest. Each branch holds the same denylist check; without a
// test per field, two of the three stay uncovered.
func TestResolve_DenylistInPerAdapterEntryFields(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{
			"request perAdapterType headers",
			Config{Request: Direction{PerAdapterType: map[string]Entry{
				"openai": {Headers: []string{"cookie"}},
			}}},
		},
		{
			"response perAdapterType static",
			Config{Response: Direction{PerAdapterType: map[string]Entry{
				"openai": {Static: []string{"set-cookie"}},
			}}},
		},
		{
			"response perAdapterType perRequest",
			Config{Response: Direction{PerAdapterType: map[string]Entry{
				"openai": {PerRequest: []string{"x-api-key"}},
			}}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Resolve(tc.cfg, allFormats()); err == nil {
				t.Fatalf("denylist entry in %s should be rejected", tc.name)
			}
		})
	}
}

// TestCheckAgainstDenylist_EmptyName covers the
//
//	if lower == "" { return error }
//
// guard in checkAgainstDenylist. The validator must reject a config
// that lists an all-whitespace header — operators sometimes paste
// trailing newlines into YAML lists. Without this, the empty-name
// branch in the validator stays uncovered.
func TestCheckAgainstDenylist_EmptyName(t *testing.T) {
	cfg := Config{Request: Direction{Base: []string{"   "}}}
	if _, err := Resolve(cfg, allFormats()); err == nil {
		t.Fatal("whitespace-only header name must be rejected")
	} else if !strings.Contains(err.Error(), "empty header name") {
		t.Errorf("error did not flag empty name: %v", err)
	}
}

// TestResolve_BaseStaticPerRequestOverlap exercises the response-base
// overlap check (separate from the per-adapter overlap covered by
// TestResolve_RejectsStaticPerRequestOverlap).
func TestResolve_BaseStaticPerRequestOverlap(t *testing.T) {
	cfg := Config{
		Response: Direction{
			BaseStatic:     []string{"x-foo"},
			BasePerRequest: []string{"X-Foo"}, // case-insensitive overlap
		},
	}
	if _, err := Resolve(cfg, allFormats()); err == nil {
		t.Fatal("response base static/perRequest overlap must be rejected")
	} else if !strings.Contains(err.Error(), "response.base") {
		t.Errorf("error did not name response.base: %v", err)
	}
}

// TestLowerSet_SkipsEmptyEntries covers the `if s == "" { continue }`
// branch in lowerSet — callers (Resolve) pre-validate and reject empty
// header names in user-supplied lists, but lowerSet's defensive
// empty-skip still matters for direct callers and stays correct under
// trailing-whitespace edge cases. Drive it directly to exercise the
// skip branch.
func TestLowerSet_SkipsEmptyEntries(t *testing.T) {
	out := lowerSet([]string{"X-Real", "", "  ", "x-keep"})
	if _, ok := out[""]; ok {
		t.Error("lowerSet must drop empty entries")
	}
	if _, ok := out["x-real"]; !ok {
		t.Error("lowerSet must keep lower-cased real entries")
	}
	if _, ok := out["x-keep"]; !ok {
		t.Error("lowerSet must keep multiple real entries")
	}
	if len(out) != 2 {
		t.Errorf("lowerSet: got %d entries, want 2 (X-Real, x-keep)", len(out))
	}
}

// TestBucketDroppedHeader keeps cardinality bounded.
func TestBucketDroppedHeader(t *testing.T) {
	cases := map[string]string{
		"authorization":               "authorization",
		"cookie":                      "cookie",
		"x-amz-content-sha256":        "x-amz-*",
		"x-forwarded-for":             "x-forwarded-*",
		"x-nexus-aigw-via":            "x-nexus-*",
		"access-control-allow-origin": "access-control-*",
		"random-unknown-header":       "other",
		"openai-organization":         "other", // allow-list members are not denied; bucket as other
	}
	for in, want := range cases {
		if got := BucketDroppedHeader(in); got != want {
			t.Errorf("BucketDroppedHeader(%q) = %q, want %q", in, got, want)
		}
	}
}
