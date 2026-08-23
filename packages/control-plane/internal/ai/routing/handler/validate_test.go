package routing

import (
	"bytes"
	"os"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	cfgpolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/policy"
	"github.com/goccy/go-json"
	"gopkg.in/yaml.v3"
)

// latencyConfigWithTargets builds a latency strategy config JSON carrying n
// {providerId,modelId} entries under the generic "targets" key.
func latencyConfigWithTargets(n int) string {
	var b strings.Builder
	b.WriteString(`{"type":"latency","targets":[`)
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"providerId":"p","modelId":"m"}`)
	}
	b.WriteString(`]}`)
	return b.String()
}

// TestValidateMatchConditions: the admin write path rejects the legacy
// field name "organizations" in favor of "projects".
func TestValidateMatchConditions(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantOK  bool
		wantMsg string
	}{
		{
			name:   "empty body ok",
			raw:    ``,
			wantOK: true,
		},
		{
			name:   "projects ok",
			raw:    `{"projects":["p-1"],"models":["m-1"]}`,
			wantOK: true,
		},
		{
			name:   "no filter block ok",
			raw:    `{}`,
			wantOK: true,
		},
		{
			name:    "legacy organizations rejected",
			raw:     `{"organizations":["p-1"]}`,
			wantOK:  false,
			wantMsg: "matchConditions.organizations has been renamed to matchConditions.projects",
		},
		{
			name:    "legacy organizations + projects both rejected",
			raw:     `{"organizations":["p-1"],"projects":["p-2"]}`,
			wantOK:  false,
			wantMsg: "matchConditions.organizations has been renamed to matchConditions.projects",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var raw json.RawMessage
			if tt.raw != "" {
				raw = json.RawMessage(tt.raw)
			}
			msg, ok := validateMatchConditions(raw)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v (msg=%q)", ok, tt.wantOK, msg)
			}
			if !tt.wantOK && msg != tt.wantMsg {
				t.Errorf("msg = %q, want %q", msg, tt.wantMsg)
			}
		})
	}
}

// TestValidateStrategyType: the admin write path accepts only the closed set
// of strategy types the AI Gateway resolver can dispatch and rejects any
// free-string value with an operator-facing message that lists the allowed
// set (F-0272b).
func TestValidateStrategyType(t *testing.T) {
	for _, st := range []string{"single", "fallback", "loadbalance", "conditional", "ab_split", "smart", "latency"} {
		t.Run("accept_"+st, func(t *testing.T) {
			if msg, ok := validateStrategyType(st); !ok {
				t.Errorf("strategyType %q should be accepted; got msg=%q", st, msg)
			}
		})
	}

	// `policy` sits with the free strings on purpose. It was accepted for as
	// long as it had no implementation: a rule carrying it was persisted,
	// broadcast fleet-wide, and then yielded the primary slot on every gateway
	// — listed and enabled in the admin UI, and never firing. Accepting a value
	// the resolver cannot dispatch is the defect, not the vocabulary.
	for _, st := range []string{"policy", "", "Smart", "round-robin", "weighted", "random", "best", "unknown"} {
		t.Run("reject_"+st, func(t *testing.T) {
			msg, ok := validateStrategyType(st)
			if ok {
				t.Fatalf("strategyType %q should be rejected", st)
			}
			if !strings.Contains(msg, "not a recognized routing strategy") {
				t.Errorf("msg = %q; want it to explain the rejection", msg)
			}
			// The message must enumerate the accepted set so an operator
			// can self-correct without reading source.
			if !strings.Contains(msg, "single") || !strings.Contains(msg, "smart") {
				t.Errorf("msg = %q; want it to list the allowed strategies", msg)
			}
		})
	}
}

// TestValidateStrategyConfig: a malformed config payload is rejected before it
// can be persisted and broadcast fleet-wide; a well-shaped strategy object is
// accepted; absent/null config defers to the required-field check (F-0272b).
func TestValidateStrategyConfig(t *testing.T) {
	tests := []struct {
		name          string
		raw           string
		wantOK        bool
		wantMsgSubstr string
	}{
		{
			name:   "empty defers to required-field check",
			raw:    ``,
			wantOK: true,
		},
		{
			name:   "null defers to required-field check",
			raw:    `null`,
			wantOK: true,
		},
		{
			name:   "valid single node",
			raw:    `{"type":"single","providerId":"p-1","modelId":"m-1"}`,
			wantOK: true,
		},
		{
			name:   "valid loadbalance node",
			raw:    `{"type":"loadbalance","algorithm":"weighted","weightedTargets":[{"weight":1,"node":{"type":"single","providerId":"p","modelId":"m"}}]}`,
			wantOK: true,
		},
		{
			name:   "valid smart node",
			raw:    `{"type":"smart","routerProviderId":"p","routerModelId":"m","maxTokens":256,"timeoutMs":3000}`,
			wantOK: true,
		},
		{
			name:   "valid latency node (targets under generic key)",
			raw:    `{"type":"latency","targets":[{"providerId":"p","modelId":"m"}]}`,
			wantOK: true,
		},
		{
			name:          "latency node over the target cap is rejected",
			raw:           latencyConfigWithTargets(maxLatencyTargets + 1),
			wantOK:        false,
			wantMsgSubstr: "at most",
		},
		{
			name:   "latency node exactly at the target cap is accepted",
			raw:    latencyConfigWithTargets(maxLatencyTargets),
			wantOK: true,
		},
		{
			name:   "object without type field is accepted (type checked elsewhere)",
			raw:    `{"providerId":"p-1"}`,
			wantOK: true,
		},
		{
			name:          "JSON array is not a strategy object",
			raw:           `[{"type":"single"}]`,
			wantOK:        false,
			wantMsgSubstr: "not a valid strategy object",
		},
		{
			name:          "JSON string is not a strategy object",
			raw:           `"single"`,
			wantOK:        false,
			wantMsgSubstr: "not a valid strategy object",
		},
		{
			name:          "truncated JSON rejected",
			raw:           `{"type":"single",`,
			wantOK:        false,
			wantMsgSubstr: "not a valid strategy object",
		},
		{
			name:          "wrong type for typed field rejected",
			raw:           `{"type":"single","providerId":123}`,
			wantOK:        false,
			wantMsgSubstr: "not a valid strategy object",
		},
		{
			name:          "wrong type for maxTokens rejected",
			raw:           `{"type":"smart","maxTokens":"lots"}`,
			wantOK:        false,
			wantMsgSubstr: "not a valid strategy object",
		},
		{
			name:          "unknown node type rejected",
			raw:           `{"type":"frobnicate"}`,
			wantOK:        false,
			wantMsgSubstr: "not a recognized strategy node type",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var raw json.RawMessage
			if tt.raw != "" {
				raw = json.RawMessage(tt.raw)
			}
			msg, ok := validateStrategyConfig(raw)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v (msg=%q)", ok, tt.wantOK, msg)
			}
			if !tt.wantOK && !strings.Contains(msg, tt.wantMsgSubstr) {
				t.Errorf("msg = %q; want substring %q", msg, tt.wantMsgSubstr)
			}
		})
	}
}

// TestValidateSmartRuleMatchConditions: the admin API rejects
// smart-strategy RoutingRules whose matchConditions would let the smart
// strategy fire on non-"auto" traffic. The guard prevents an operator
// from creating a rule that makes smart routing fire on explicit-model
// requests, which bypasses the rule's intent.
func TestValidateSmartRuleMatchConditions(t *testing.T) {
	tests := []struct {
		name          string
		strategyType  string
		raw           string
		wantOK        bool
		wantMsgSubstr string
	}{
		{
			name:         "non-smart strategy is not checked (single)",
			strategyType: "single",
			raw:          `{}`,
			wantOK:       true,
		},
		{
			name:         "non-smart strategy with non-auto literals is not checked (conditional)",
			strategyType: "conditional",
			raw:          `{"requestedModelLiterals":["claude-opus-4-7"]}`,
			wantOK:       true,
		},
		{
			name:          "smart with empty matchConditions rejected",
			strategyType:  "smart",
			raw:           `{}`,
			wantOK:        false,
			wantMsgSubstr: `must include "requestedModelLiterals": ["auto"]`,
		},
		{
			name:          "smart with nil matchConditions rejected",
			strategyType:  "smart",
			raw:           ``,
			wantOK:        false,
			wantMsgSubstr: `must include "requestedModelLiterals": ["auto"]`,
		},
		{
			name:          "smart with null matchConditions rejected",
			strategyType:  "smart",
			raw:           `null`,
			wantOK:        false,
			wantMsgSubstr: `must include "requestedModelLiterals": ["auto"]`,
		},
		{
			name:          "smart with projects but no literals rejected",
			strategyType:  "smart",
			raw:           `{"projects":["p-1"]}`,
			wantOK:        false,
			wantMsgSubstr: `must include "requestedModelLiterals": ["auto"]`,
		},
		{
			name:          "smart with empty literals array rejected",
			strategyType:  "smart",
			raw:           `{"requestedModelLiterals":[]}`,
			wantOK:        false,
			wantMsgSubstr: `must include "requestedModelLiterals": ["auto"]`,
		},
		{
			name:         "smart with auto-only literals accepted",
			strategyType: "smart",
			raw:          `{"requestedModelLiterals":["auto"]}`,
			wantOK:       true,
		},
		{
			name:         "smart with auto-only literals plus other conditions accepted",
			strategyType: "smart",
			raw:          `{"requestedModelLiterals":["auto"],"projects":["p-1"]}`,
			wantOK:       true,
		},
		{
			name:          "smart with non-auto literal rejected (mentions offending literal)",
			strategyType:  "smart",
			raw:           `{"requestedModelLiterals":["claude-opus-4-7"]}`,
			wantOK:        false,
			wantMsgSubstr: `"claude-opus-4-7" is not safe for strategyType=smart`,
		},
		{
			name:          "smart with mixed auto + non-auto literals rejected",
			strategyType:  "smart",
			raw:           `{"requestedModelLiterals":["auto","smart"]}`,
			wantOK:        false,
			wantMsgSubstr: `"smart" is not safe for strategyType=smart`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var raw json.RawMessage
			if tt.raw != "" {
				raw = json.RawMessage(tt.raw)
			}
			msg, ok := validateSmartRuleMatchConditions(tt.strategyType, raw)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v (msg=%q)", ok, tt.wantOK, msg)
			}
			if !tt.wantOK && !strings.Contains(msg, tt.wantMsgSubstr) {
				t.Errorf("msg = %q, want substring %q", msg, tt.wantMsgSubstr)
			}
			if !tt.wantOK && !strings.Contains(msg, "r-routing-rule-matchconditions-audit.md") {
				t.Errorf("msg should reference the runbook; got %q", msg)
			}
		})
	}
}

// TestStrategyTypes_ArePublishedExactlyAsAccepted.
//
// Four lists say which strategies exist: this validator, the gateway's
// registry, the OpenAPI enum an integrator reads, and the UI dropdown. They
// drift in both directions and each direction has its own failure. A value in
// the spec that the validator refuses is an integrator writing a rule against
// the published contract and getting a 400. A value the validator accepts that
// the spec omits is a working feature nobody can find — which is what happened
// to `latency`: dispatchable since it was written, absent from both OpenAPI
// copies.
//
// Checked per ENUM BLOCK, not per file. Each spec declares the set three times
// — list response, create request, update request — and a union over the file
// passes while one of the three is missing a value, which is the drift most
// likely to happen: someone adds a strategy and updates the request schema
// they were looking at.
//
// The gateway registry is not read here (different module); the agreement
// tests in the strategies package hold that side.
func TestStrategyTypes_ArePublishedExactlyAsAccepted(t *testing.T) {
	specs := []string{
		"../../../../../../docs/users/api/openapi/control-plane/routing-rules.yaml",
		"../../../../../nexus-agent-core/capabilities/resource/openapi/control-plane/routing-rules.yaml",
	}
	known := map[string]bool{"single": true, "fallback": true, "loadbalance": true,
		"conditional": true, "ab_split": true, "latency": true, "smart": true, "policy": true}

	for _, spec := range specs {
		raw, err := os.ReadFile(spec)
		if err != nil {
			t.Fatalf("read %s: %v — the spec is the published contract; a test that cannot "+
				"find it silently stops comparing", spec, err)
		}
		blocks := strategyEnumBlocks(string(raw), known)
		// Three: the list response, the create request, the update request. A
		// different count means the spec's shape moved and this test is now
		// reading something other than what it was written to read.
		if len(blocks) != 3 {
			t.Fatalf("%s: found %d strategy enum block(s), want 3 — the spec's shape moved and "+
				"this test no longer knows what it is comparing", spec, len(blocks))
		}
		for n, found := range blocks {
			for name := range found {
				if _, ok := validStrategyTypes[name]; !ok {
					t.Errorf("%s enum #%d publishes %q, which the admin API refuses: an "+
						"integrator writing that rule against the spec gets a 400", spec, n+1, name)
				}
			}
			for name := range validStrategyTypes {
				if !found[name] {
					t.Errorf("%s enum #%d omits %q, which the admin API accepts and the gateway "+
						"dispatches: a working strategy nobody reading the contract can find",
						spec, n+1, name)
				}
			}
		}
	}
}

// strategyEnumBlocks returns one set per `enum:` block whose members are all
// strategy names, in file order. A block mixing strategy names with anything
// else is a different enum (retry classes, statuses) and is skipped.
func strategyEnumBlocks(src string, known map[string]bool) []map[string]bool {
	var out []map[string]bool
	lines := strings.Split(src, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) != "enum:" {
			continue
		}
		members := map[string]bool{}
		allKnown := true
		for _, item := range lines[i+1:] {
			v := strings.TrimSpace(item)
			if !strings.HasPrefix(v, "- ") {
				break
			}
			name := strings.TrimSpace(v[2:])
			if !known[name] {
				allKnown = false
				break
			}
			members[name] = true
		}
		if allKnown && len(members) > 0 {
			out = append(out, members)
		}
	}
	return out
}

// routingRuleSpecs are the two published copies of the admin API contract. They
// are read as one: an integrator reaching either has the same expectations.
var routingRuleSpecs = []string{
	"../../../../../../docs/users/api/openapi/control-plane/routing-rules.yaml",
	"../../../../../nexus-agent-core/capabilities/resource/openapi/control-plane/routing-rules.yaml",
}

// retryPolicyPropertyBlocks returns the published property names under every
// `retryPolicy` schema in a parsed spec, keyed by the order encountered.
//
// It walks the whole document rather than reading known paths. A spec that
// grows a fifth retryPolicy schema — a simulate response, a new endpoint —
// gets checked the moment it appears, which is the case a path list misses.
func retryPolicyPropertyBlocks(node any) []map[string]bool {
	var out []map[string]bool
	switch n := node.(type) {
	case map[string]any:
		// Keys are walked in sorted order so the block sequence a failure
		// message names is the same on every run.
		keys := make([]string, 0, len(n))
		for k := range n {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if k == "retryPolicy" {
				if schema, ok := n[k].(map[string]any); ok {
					if props, ok := schema["properties"].(map[string]any); ok {
						have := map[string]bool{}
						for name := range props {
							have[name] = true
						}
						out = append(out, have)
					}
				}
			}
			out = append(out, retryPolicyPropertyBlocks(n[k])...)
		}
	case []any:
		for _, item := range n {
			out = append(out, retryPolicyPropertyBlocks(item)...)
		}
	}
	return out
}

// TestRetryPolicy_EveryAcceptedFieldIsPublished holds the admin API's retry
// knobs to the contract that describes them.
//
// The handler binds retryPolicy as a raw JSON document and unmarshals it into
// cfgpolicy.RetryPolicy, so EVERY json-tagged field on that struct is accepted,
// persisted, and pushed to every gateway. A field the struct honours but the
// spec omits is a knob only someone reading Go source can find — which is how
// maxUpstreamCalls came to be published in one copy of the spec and not the
// other, and how the three backoff fields went unpublished entirely.
//
// Reflection rather than a written-down list: adding a field to the struct
// breaks this test until the spec catches up, which is the point.
func TestRetryPolicy_EveryAcceptedFieldIsPublished(t *testing.T) {
	rt := reflect.TypeOf(cfgpolicy.RetryPolicy{})
	accepted := make([]string, 0, rt.NumField())
	for i := range rt.NumField() {
		tag := rt.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			t.Fatalf("RetryPolicy.%s has no json tag; the admin API's accepted "+
				"shape is defined by those tags and this test can no longer see it",
				rt.Field(i).Name)
		}
		accepted = append(accepted, strings.Split(tag, ",")[0])
	}

	for _, spec := range routingRuleSpecs {
		raw, err := os.ReadFile(spec)
		if err != nil {
			t.Fatalf("read %s: %v — the spec is the published contract; a test that "+
				"cannot find it silently stops comparing", spec, err)
		}
		var doc any
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("parse %s: %v", spec, err)
		}
		blocks := retryPolicyPropertyBlocks(doc)
		// Four: the create request, the update request, the list response, and
		// the persisted-row schema. A different count means the spec's shape
		// moved and this test is now reading something other than what it was
		// written to read.
		if len(blocks) != 4 {
			t.Fatalf("%s: found %d retryPolicy property block(s), want 4 — the spec's "+
				"shape moved and this test no longer knows what it is comparing",
				spec, len(blocks))
		}
		for n, published := range blocks {
			for _, field := range accepted {
				if !published[field] {
					t.Errorf("%s retryPolicy block #%d omits %q: the admin API accepts, "+
						"persists and broadcasts that field, so an integrator reading "+
						"the spec cannot discover a knob that changes how their rule "+
						"spends and waits", spec, n+1, field)
				}
			}
			for name := range published {
				if !slices.Contains(accepted, name) {
					t.Errorf("%s retryPolicy block #%d publishes %q, which RetryPolicy "+
						"does not carry: an integrator who sets it gets silence, not a 400",
						spec, n+1, name)
				}
			}
		}
	}
}

// TestRoutingRuleSpecs_TwoCopiesAreOne holds the two published copies byte-for-byte.
//
// They describe one API. Nothing regenerates one from the other, so a change
// applied to the copy under docs/ and not to the one shipped inside the agent
// resource bundle leaves two contracts disagreeing, with no signal — which is
// the state maxUpstreamCalls sat in.
func TestRoutingRuleSpecs_TwoCopiesAreOne(t *testing.T) {
	first, err := os.ReadFile(routingRuleSpecs[0])
	if err != nil {
		t.Fatalf("read %s: %v", routingRuleSpecs[0], err)
	}
	for _, spec := range routingRuleSpecs[1:] {
		other, err := os.ReadFile(spec)
		if err != nil {
			t.Fatalf("read %s: %v", spec, err)
		}
		if !bytes.Equal(first, other) {
			t.Errorf("%s differs from %s — the two are copies of one contract; "+
				"whichever was edited alone now describes an API the other does not",
				spec, routingRuleSpecs[0])
		}
	}
}

// TestRetryPolicy_TheBackoffFieldsAreBounded.
//
// These three reach the API but not the admin UI, so the only person who meets
// them is an integrator posting the published contract — and nothing downstream
// would catch what they sent. computeBackoff clamps the doubling at whatever
// BackoffMax the rule carries, which makes the rule's own value the ceiling
// rather than something checked against one.
//
// Each case is a value the API accepted, with the behaviour it bought.
func TestRetryPolicy_TheBackoffFieldsAreBounded(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		accept bool
		bought string
	}{
		{
			name: "backoffInitial written as a bare number is nanoseconds",
			body: `{"backoffInitial":250}`,
			bought: "250ns is not a pause; a rule written to slow its retries down speeds " +
				"them up against an upstream that asked to be left alone",
		},
		{
			name:   "backoffMax written as a bare number is nanoseconds",
			body:   `{"backoffMax":5}`,
			bought: "5ns clamps every wait to nothing, whatever backoffInitial says",
		},
		{
			name:   "a negative backoff disables the pause entirely",
			body:   `{"backoffInitial":-1000000000}`,
			bought: "a negative duration passes the merge and fires the timer immediately",
		},
		{
			name:   "a backoff longer than a minute stalls the walk",
			body:   `{"backoffMax":3600000000000}`,
			bought: "an hour-long pause outlives every client waiting on the request",
		},
		{
			name:   "jitter above 1 swings wider than the wait it jitters",
			body:   `{"backoffJitter":5}`,
			bought: "the swing exceeds the base, so a pause is as likely to be nothing as to be the configured value",
		},
		{
			name:   "a negative jitter reads as no jitter at all",
			body:   `{"backoffJitter":-0.5}`,
			bought: "the guard is `> 0`, so the value is accepted and silently ignored",
		},
		{name: "the shipped defaults are accepted", accept: true,
			body: `{"backoffInitial":250000000,"backoffMax":5000000000,"backoffJitter":0.2}`},
		{name: "an omitted backoff still inherits", accept: true, body: `{"maxAttemptsPerTarget":2}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg, ok := validateRetryPolicyJSON(json.RawMessage(tc.body))
			if tc.accept {
				if !ok {
					t.Fatalf("%s was refused with %q; it is a legal policy", tc.body, msg)
				}
				return
			}
			if ok {
				t.Fatalf("%s was accepted, persisted and broadcast — %s", tc.body, tc.bought)
			}
			if !strings.Contains(msg, "retryPolicy.backoff") {
				t.Errorf("the refusal is %q, which does not name the field the caller must fix", msg)
			}
		})
	}
}
