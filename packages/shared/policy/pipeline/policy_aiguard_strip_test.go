package pipeline

import "testing"

func TestStripAIGuardForAgentIngress(t *testing.T) {
	cases := []struct {
		name    string
		ingress string
		cfgIn   map[string]any
		wantHas bool
	}{
		{
			"strips_on_agent",
			"AGENT",
			map[string]any{"ai_guard": map[string]any{"enabled": true}, "other": 1},
			false,
		},
		{
			"keeps_on_ai_gateway",
			"AI_GATEWAY",
			map[string]any{"ai_guard": map[string]any{"enabled": true}},
			true,
		},
		{
			"keeps_on_compliance_proxy",
			"COMPLIANCE_PROXY",
			map[string]any{"ai_guard": map[string]any{"enabled": true}},
			true,
		},
		{
			"no_op_when_absent",
			"AGENT",
			map[string]any{"foo": "bar"},
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Capture original to detect input mutation.
			_, inputHadKey := tc.cfgIn["ai_guard"]

			out := stripAIGuardForAgent(tc.cfgIn, tc.ingress)
			_, has := out["ai_guard"]
			if has != tc.wantHas {
				t.Errorf("has ai_guard: got %v want %v; out=%+v", has, tc.wantHas, out)
			}
			_, stillInInput := tc.cfgIn["ai_guard"]
			if inputHadKey && !stillInInput {
				t.Errorf("input was mutated: original %+v", tc.cfgIn)
			}
		})
	}
}
