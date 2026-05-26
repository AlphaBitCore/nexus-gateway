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
