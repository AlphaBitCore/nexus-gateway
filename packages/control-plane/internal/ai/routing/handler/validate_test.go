package routing

import (
	"encoding/json"
	"strings"
	"testing"
)

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
	for _, st := range []string{"single", "fallback", "loadbalance", "conditional", "ab_split", "policy", "smart"} {
		t.Run("accept_"+st, func(t *testing.T) {
			if msg, ok := validateStrategyType(st); !ok {
				t.Errorf("strategyType %q should be accepted; got msg=%q", st, msg)
			}
		})
	}

	for _, st := range []string{"", "Smart", "round-robin", "weighted", "random", "best", "unknown"} {
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
