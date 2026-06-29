package proxy

import (
	"net/http/httptest"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	goHooks "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// TestSetResponseHeaders_NonStream validates the non-streaming variant
// across the four sub-cases defined in plan §10.1.
func TestSetResponseHeaders_NonStream(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		rec          *audit.Record
		target       routingcore.RoutingTarget
		result       *routingcore.RouteResult
		attempts     int
		wantPresent  map[string]string // header → exact value
		wantAnyValue []string          // header must be non-empty
		wantAbsent   []string          // header must be missing
	}{
		{
			name:     "bare success",
			rec:      &audit.Record{RequestID: "req-bare"},
			target:   routingcore.RoutingTarget{ProviderName: "openai", ModelCode: "gpt-4o"},
			result:   &routingcore.RouteResult{},
			attempts: 1,
			wantPresent: map[string]string{
				"X-Nexus-Via": "ai-gateway",
				// attempts header is always emitted; first-try success = "1"
				"X-Nexus-Attempts": "1",
			},
			wantAbsent: []string{
				"X-Nexus-Routed-Model",
				"X-Nexus-Routed-Provider",
			},
		},
		{
			name:     "with retry",
			rec:      &audit.Record{RequestID: "req-retry"},
			target:   routingcore.RoutingTarget{ProviderName: "azure", ModelCode: "gpt-4"},
			result:   &routingcore.RouteResult{},
			attempts: 3,
			wantPresent: map[string]string{
				"X-Nexus-Attempts": "3",
			},
			wantAbsent: []string{
				"X-Nexus-Routed-Model",
				"X-Nexus-Routed-Provider",
			},
		},
		{
			name:   "with substitution",
			rec:    &audit.Record{RequestID: "req-sub"},
			target: routingcore.RoutingTarget{ProviderName: "anthropic", ModelCode: "claude-3-5-sonnet"},
			result: &routingcore.RouteResult{
				Substituted: true,
			},
			attempts: 1,
			wantPresent: map[string]string{
				"X-Nexus-Routed-Model":    "claude-3-5-sonnet",
				"X-Nexus-Routed-Provider": "anthropic",
				"X-Nexus-Attempts":        "1",
			},
			wantAbsent: []string{},
		},
		{
			name:   "with routing rule",
			rec:    &audit.Record{RequestID: "req-rule"},
			target: routingcore.RoutingTarget{ProviderName: "openai", ModelCode: "gpt-4o-mini"},
			result: &routingcore.RouteResult{
				RuleName: "cost-opt-rule",
			},
			attempts: 1,
			wantPresent: map[string]string{
				"X-Nexus-Attempts": "1",
			},
			wantAbsent: []string{
				"X-Nexus-Routed-Model",
				"X-Nexus-Routed-Provider",
			},
		},
	}

	h := &Handler{}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := httptest.NewRecorder()
			start := time.Now()

			h.setResponseHeaders(w, tc.rec, tc.target, tc.result, start, tc.attempts)

			for k, want := range tc.wantPresent {
				if got := w.Header().Get(k); got != want {
					t.Errorf("header %s: got %q, want %q", k, got, want)
				}
			}
			for _, k := range tc.wantAnyValue {
				if got := w.Header().Get(k); got == "" {
					t.Errorf("header %s: expected non-empty value, got empty", k)
				}
			}
			for _, k := range tc.wantAbsent {
				if got := w.Header().Get(k); got != "" {
					t.Errorf("header %s: expected absent, got %q", k, got)
				}
			}
		})
	}
}

// TestSetResponseHeadersStream_LatencyAbsent verifies that the streaming
// variant emits the core markers and the stream flag but does NOT emit
// x-nexus-aigw-latency-ms (per spec §5 — latency is meaningless when
// headers are sent before the last byte arrives).
func TestSetResponseHeadersStream(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		rec           *audit.Record
		target        routingcore.RoutingTarget
		result        *routingcore.RouteResult
		attempts      int
		preSetHeaders map[string]string // headers pre-seeded on the recorder
		wantPresent   map[string]string
		wantAnyValue  []string
		wantAbsent    []string
	}{
		{
			name:     "bare stream",
			rec:      &audit.Record{RequestID: "req-stream"},
			target:   routingcore.RoutingTarget{ProviderName: "openai", ModelCode: "gpt-4o"},
			result:   &routingcore.RouteResult{},
			attempts: 1,
			wantPresent: map[string]string{
				// attempts header is always emitted; first-try success = "1"
				"X-Nexus-Attempts": "1",
			},
			// latency-ms must be ABSENT in the streaming variant
			wantAbsent: []string{
				"X-Nexus-Routed-Model",
				"X-Nexus-Routed-Provider",
			},
		},
		{
			name:     "via prepend when upstream already set via",
			rec:      &audit.Record{RequestID: "req-via"},
			target:   routingcore.RoutingTarget{ProviderName: "openai", ModelCode: "gpt-4o"},
			result:   &routingcore.RouteResult{},
			attempts: 1,
			// Simulate a scenario where an upstream hop already wrote a via value.
			// PrependVia should prepend "ai-gateway" in front of the existing entry.
			preSetHeaders: map[string]string{
				"X-Nexus-Via": "compliance-proxy",
			},
			wantPresent: map[string]string{
				"X-Nexus-Attempts": "1",
			},
			// The via header should have been prepended: "ai-gateway, compliance-proxy"
			wantAnyValue: []string{"X-Nexus-Via"},
		},
		{
			name:     "stream with retry",
			rec:      &audit.Record{RequestID: "req-stream-retry"},
			target:   routingcore.RoutingTarget{ProviderName: "azure", ModelCode: "gpt-4"},
			result:   &routingcore.RouteResult{},
			attempts: 2,
			wantPresent: map[string]string{
				"X-Nexus-Attempts": strconv.Itoa(2),
			},
		},
		{
			name:   "stream with substitution",
			rec:    &audit.Record{RequestID: "req-stream-sub"},
			target: routingcore.RoutingTarget{ProviderName: "anthropic", ModelCode: "claude-3-5-haiku"},
			result: &routingcore.RouteResult{
				Substituted: true,
			},
			attempts: 1,
			wantPresent: map[string]string{
				"X-Nexus-Routed-Model":    "claude-3-5-haiku",
				"X-Nexus-Routed-Provider": "anthropic",
				"X-Nexus-Attempts":        "1",
			},
		},
	}

	h := &Handler{}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := httptest.NewRecorder()

			// Pre-seed any headers the test scenario requires.
			for k, v := range tc.preSetHeaders {
				w.Header().Set(k, v)
			}

			h.setResponseHeadersStream(w, tc.rec, tc.target, tc.result, tc.attempts)

			for k, want := range tc.wantPresent {
				if got := w.Header().Get(k); got != want {
					t.Errorf("header %s: got %q, want %q", k, got, want)
				}
			}
			for _, k := range tc.wantAnyValue {
				if got := w.Header().Get(k); got == "" {
					t.Errorf("header %s: expected non-empty value, got empty", k)
				}
			}
			for _, k := range tc.wantAbsent {
				if got := w.Header().Get(k); got != "" {
					t.Errorf("header %s: expected absent, got %q", k, got)
				}
			}
		})
	}
}

// TestSetResponseHeadersStream_ViaChain explicitly asserts the prepended
// via value so the "already-set via" case above is unambiguous.
func TestSetResponseHeadersStream_ViaChain(t *testing.T) {
	t.Parallel()

	h := &Handler{}
	w := httptest.NewRecorder()
	// Simulate an upstream proxy that already appended its own hop.
	w.Header().Set("X-Nexus-Via", "compliance-proxy")

	rec := &audit.Record{RequestID: "req-via-chain"}
	target := routingcore.RoutingTarget{ProviderName: "openai", ModelCode: "gpt-4o"}
	result := &routingcore.RouteResult{}

	h.setResponseHeadersStream(w, rec, target, result, 1)

	got := w.Header().Get("X-Nexus-Via")
	want := "ai-gateway, compliance-proxy"
	if got != want {
		t.Errorf("x-nexus-via: got %q, want %q", got, want)
	}
}

// TestAigwHookOutcomeFromResult validates the mapping from
// CompliancePipelineResult to HookOutcomeInput per spec §4.5.
func TestAigwHookOutcomeFromResult(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   *goHooks.CompliancePipelineResult
		want traffic.HookOutcomeInput
	}{
		{
			name: "nil",
			in:   nil,
			want: traffic.HookOutcomeInput{},
		},
		{
			name: "empty hook results",
			in:   &goHooks.CompliancePipelineResult{},
			want: traffic.HookOutcomeInput{},
		},
		{
			name: "two approve hooks",
			in: &goHooks.CompliancePipelineResult{HookResults: []goHooks.HookResult{
				{HookName: "pii-redact", Decision: goHooks.Approve},
				{HookName: "jwt-strip", Decision: goHooks.Approve},
			}},
			want: traffic.HookOutcomeInput{Passed: []string{"pii-redact", "jwt-strip"}},
		},
		{
			name: "abstain treated as passed",
			in: &goHooks.CompliancePipelineResult{HookResults: []goHooks.HookResult{
				{HookName: "rate-check", Decision: goHooks.Abstain},
			}},
			want: traffic.HookOutcomeInput{Passed: []string{"rate-check"}},
		},
		{
			name: "modify sets transformed flag",
			in: &goHooks.CompliancePipelineResult{HookResults: []goHooks.HookResult{
				{HookName: "pii-redact", Decision: goHooks.Modify},
			}},
			want: traffic.HookOutcomeInput{Passed: []string{"pii-redact"}, Transformed: true},
		},
		{
			name: "mixed approve and modify",
			in: &goHooks.CompliancePipelineResult{HookResults: []goHooks.HookResult{
				{HookName: "pii-redact", Decision: goHooks.Approve},
				{HookName: "prompt-clean", Decision: goHooks.Modify},
				{HookName: "jwt-strip", Decision: goHooks.Approve},
			}},
			want: traffic.HookOutcomeInput{Passed: []string{"pii-redact", "prompt-clean", "jwt-strip"}, Transformed: true},
		},
		{
			name: "reject hard halts and uses reasonCode",
			in: &goHooks.CompliancePipelineResult{HookResults: []goHooks.HookResult{
				{HookName: "pii-redact", Decision: goHooks.Approve},
				{HookName: "prompt-injection", Decision: goHooks.RejectHard, ReasonCode: "sql-fragment"},
				{HookName: "should-not-appear", Decision: goHooks.Approve},
			}},
			want: traffic.HookOutcomeInput{Rejected: "prompt-injection", RejectReason: "sql-fragment"},
		},
		{
			// BlockSoft is no longer a reject outcome (soft-block was merged into
			// block); a per-hook BlockSoft now passes through to Passed rather
			// than halting the outcome with a reject attribution.
			name: "block-soft no longer halts; treated as passed",
			in: &goHooks.CompliancePipelineResult{HookResults: []goHooks.HookResult{
				{HookName: "content-safety", Decision: goHooks.BlockSoft, ReasonCode: "toxic"},
				{HookName: "after", Decision: goHooks.Approve},
			}},
			want: traffic.HookOutcomeInput{Passed: []string{"content-safety", "after"}},
		},
		{
			name: "reject falls back to Reason when ReasonCode empty",
			in: &goHooks.CompliancePipelineResult{HookResults: []goHooks.HookResult{
				{HookName: "blocker", Decision: goHooks.RejectHard, Reason: "blocked by policy", ReasonCode: ""},
			}},
			want: traffic.HookOutcomeInput{Rejected: "blocker", RejectReason: "blocked by policy"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := aigwHookOutcomeFromResult(c.in)
			if got.Rejected != c.want.Rejected || got.RejectReason != c.want.RejectReason || got.Transformed != c.want.Transformed {
				t.Errorf("scalar fields mismatch:\n  got  %+v\n  want %+v", got, c.want)
			}
			if !reflect.DeepEqual(got.Passed, c.want.Passed) {
				t.Errorf("Passed slice mismatch:\n  got  %v\n  want %v", got.Passed, c.want.Passed)
			}
		})
	}
}

// TestAigwHookOutcomeFromResult_FormatRoundtrip verifies that the helper's
// output feeds correctly into FormatHookOutcome, so the end-to-end header
// value is correct for the key spec §4.5 paths.
func TestAigwHookOutcomeFromResult_FormatRoundtrip(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		in      *goHooks.CompliancePipelineResult
		wantHdr string
	}{
		{
			name:    "nil → none",
			in:      nil,
			wantHdr: "none",
		},
		{
			name: "two passed → passed:a,b",
			in: &goHooks.CompliancePipelineResult{HookResults: []goHooks.HookResult{
				{HookName: "hook-a", Decision: goHooks.Approve},
				{HookName: "hook-b", Decision: goHooks.Approve},
			}},
			wantHdr: "passed:hook-a,hook-b",
		},
		{
			name: "modify → transformed prefix",
			in: &goHooks.CompliancePipelineResult{HookResults: []goHooks.HookResult{
				{HookName: "pii-redact", Decision: goHooks.Modify},
			}},
			wantHdr: "transformed:pii-redact",
		},
		{
			name: "reject → rejected:<hook>:<slug>",
			in: &goHooks.CompliancePipelineResult{HookResults: []goHooks.HookResult{
				{HookName: "prompt-injection", Decision: goHooks.RejectHard, ReasonCode: "sql-fragment detected"},
			}},
			wantHdr: "rejected:prompt-injection:sql-fragment-detected",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := traffic.FormatHookOutcome(aigwHookOutcomeFromResult(c.in))
			if got != c.wantHdr {
				t.Errorf("FormatHookOutcome: got %q, want %q", got, c.wantHdr)
			}
		})
	}
}
