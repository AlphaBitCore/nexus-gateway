package traffic

import (
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestPrependVia_Empty(t *testing.T) {
	h := http.Header{}
	PrependVia(h, "ai-gateway")
	if got := h.Get("X-Nexus-Via"); got != "ai-gateway" {
		t.Errorf("got %q, want %q", got, "ai-gateway")
	}
}

func TestPrependVia_Existing(t *testing.T) {
	h := http.Header{}
	h.Set("X-Nexus-Via", "ai-gateway")
	PrependVia(h, "compliance-proxy")
	if got := h.Get("X-Nexus-Via"); got != "compliance-proxy, ai-gateway" {
		t.Errorf("got %q, want %q", got, "compliance-proxy, ai-gateway")
	}
}

func TestPrependVia_TwoHopChain(t *testing.T) {
	// Production maximum is 2 hops (front {agent|compliance-proxy} + ai-gateway).
	// Per nexus-response-markers.md §4, the 3-hop agent → compliance-proxy →
	// ai-gateway chain is not a production traffic path.
	h := http.Header{}
	PrependVia(h, "ai-gateway")
	PrependVia(h, "agent")
	if got := h.Get("X-Nexus-Via"); got != "agent, ai-gateway" {
		t.Errorf("got %q, want %q", got, "agent, ai-gateway")
	}
}

func TestPrependVia_DedupesSelfIfPresent(t *testing.T) {
	h := http.Header{}
	h.Set("X-Nexus-Via", "ai-gateway")
	PrependVia(h, "ai-gateway") // double-call should not duplicate
	if got := h.Get("X-Nexus-Via"); got != "ai-gateway" {
		t.Errorf("got %q, want %q", got, "ai-gateway")
	}
}

func TestPrependChain_FirstHopSets(t *testing.T) {
	// Header absent on entry → set verbatim, no comma prefix.
	h := http.Header{}
	PrependChain(h, "X-Nexus-Hook", "passed:rate-check")
	if got := h.Get("X-Nexus-Hook"); got != "passed:rate-check" {
		t.Errorf("got %q, want %q", got, "passed:rate-check")
	}
}

func TestPrependChain_SecondHopPrepends(t *testing.T) {
	// Header present with non-empty value → prepend with ", " separator.
	h := http.Header{}
	h.Set("X-Nexus-Hook", "passed:rate-check") // inner hop already stamped
	PrependChain(h, "X-Nexus-Hook", "transformed:redact")
	if got := h.Get("X-Nexus-Hook"); got != "transformed:redact, passed:rate-check" {
		t.Errorf("got %q, want %q", got, "transformed:redact, passed:rate-check")
	}
}

func TestPrependChain_PreservesEmptyInnerPosition(t *testing.T) {
	// Header present but empty (inner hop reserved its position without a
	// value, e.g. AI Gateway has no Mode concept) → outer hop prepends and
	// the empty inner position becomes a trailing "" preserved via the
	// "value, " trailing-empty form. Strict 1:1 with X-Nexus-Via.
	h := http.Header{}
	h.Set("X-Nexus-Mode", "") // AIGW reserved this position with empty value
	PrependChain(h, "X-Nexus-Mode", "mitm")
	if got := h.Get("X-Nexus-Mode"); got != "mitm, " {
		t.Errorf("got %q, want %q", got, "mitm, ")
	}
}

func TestExposeHeaders_HasAllMarkers(t *testing.T) {
	// Entry list must match the catalogue in nexus-response-markers.md.
	want := []string{
		"X-Nexus-Via",
		"X-Nexus-Request-Id",
		"X-Nexus-Cache",
		"X-Nexus-Routed-Model",
		"X-Nexus-Routed-Provider",
		"X-Nexus-Attempts",
		"X-Nexus-Coerced",
		"X-Nexus-Quota-Used",
		"X-Nexus-Quota-Limit",
		"X-Nexus-Quota-Downgrade",
		"X-Nexus-Quota-Original-Model",
		"X-Nexus-Quota-Warning",
		"X-Nexus-Hook",
		"X-Nexus-Mode",
		"X-Nexus-Domain-Rule",
		"Server-Timing",
		"X-Nexus-Attestation",
	}
	got := append([]string{}, ExposeHeaders...)
	sort.Strings(got)
	wantSorted := append([]string{}, want...)
	sort.Strings(wantSorted)
	if !reflect.DeepEqual(got, wantSorted) {
		t.Errorf("ExposeHeaders mismatch.\n got:  %v\n want: %v", got, wantSorted)
	}
}

func TestSetExposeHeaders_OverridesAny(t *testing.T) {
	h := http.Header{}
	h.Set("Access-Control-Expose-Headers", "x-other")
	SetExposeHeaders(h)
	if v := h.Get("Access-Control-Expose-Headers"); !strings.Contains(v, "X-Nexus-Via") {
		t.Errorf("expected X-Nexus-Via in expose; got %q", v)
	}
	if strings.Contains(h.Get("Access-Control-Expose-Headers"), "x-other") {
		t.Errorf("SetExposeHeaders should overwrite, not merge; got %q", h.Get("Access-Control-Expose-Headers"))
	}
}

func TestMergeExposeHeaders_PreservesUpstream(t *testing.T) {
	h := http.Header{}
	h.Set("Access-Control-Expose-Headers", "X-Custom-Foo, x-other")
	MergeExposeHeaders(h, "X-Nexus-Via")
	v := h.Get("Access-Control-Expose-Headers")
	if !strings.Contains(v, "X-Custom-Foo") {
		t.Errorf("upstream header dropped; got %q", v)
	}
	if !strings.Contains(strings.ToLower(v), "x-nexus-via") {
		t.Errorf("nexus header missing; got %q", v)
	}
}

func TestMergeExposeHeaders_DedupesCaseInsensitive(t *testing.T) {
	h := http.Header{}
	h.Set("Access-Control-Expose-Headers", "X-Nexus-Via")
	MergeExposeHeaders(h, "x-nexus-via") // already present case-insensitively
	v := h.Get("Access-Control-Expose-Headers")
	count := strings.Count(strings.ToLower(v), "x-nexus-via")
	if count != 1 {
		t.Errorf("expected 1 x-nexus-via, got %d in %q", count, v)
	}
}

func TestFormatHookOutcome(t *testing.T) {
	cases := []struct {
		name string
		in   HookOutcomeInput
		want string
	}{
		{
			name: "no hooks",
			in:   HookOutcomeInput{},
			want: "none",
		},
		{
			name: "one passed",
			in:   HookOutcomeInput{Passed: []string{"pii-redact"}},
			want: "passed:pii-redact",
		},
		{
			name: "two passed",
			in:   HookOutcomeInput{Passed: []string{"pii-redact", "jwt-strip"}},
			want: "passed:pii-redact,jwt-strip",
		},
		{
			name: "transformed list (any modify present means transformed prefix)",
			in:   HookOutcomeInput{Passed: []string{"pii-redact"}, Transformed: true},
			want: "transformed:pii-redact",
		},
		{
			name: "rejected with reason",
			in:   HookOutcomeInput{Rejected: "prompt-injection", RejectReason: "sql-fragment"},
			want: "rejected:prompt-injection:sql-fragment",
		},
		{
			name: "rejected sanitizes unsafe reason",
			in:   HookOutcomeInput{Rejected: "pii-detector", RejectReason: "Contains SSN! \r\n"},
			want: "rejected:pii-detector:contains-ssn",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := FormatHookOutcome(c.in)
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}
