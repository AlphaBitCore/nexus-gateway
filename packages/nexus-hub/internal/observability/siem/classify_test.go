package siem

import (
	"testing"
)

func TestClassifyTrafficEvent(t *testing.T) {
	tests := []struct {
		name string
		evt  Event
		want string
	}{
		{"allowed request", Event{"hookDecision": "allow"}, "traffic.allowed"},
		{"missing hookDecision is passthrough", Event{}, "traffic.passthrough"},
		{"unknown decision", Event{"hookDecision": "quarantine"}, "traffic.unknown"},
		{"blocked rate_limited", Event{"hookDecision": "block", "hookReasonCode": "rate_limited"}, "traffic.rate_limited"},
		{"blocked budget_exceeded", Event{"hookDecision": "block", "hookReasonCode": "budget_exceeded"}, "traffic.budget_exceeded"},
		{"blocked other reason", Event{"hookDecision": "block", "hookReasonCode": "content_policy"}, "traffic.request_blocked"},
		{"blocked missing reason", Event{"hookDecision": "block"}, "traffic.request_blocked"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyTrafficEvent(tc.evt); got != tc.want {
				t.Errorf("ClassifyTrafficEvent(%v) = %q; want %q", tc.evt, got, tc.want)
			}
		})
	}
}

func TestClassifyAdminEvent(t *testing.T) {
	tests := []struct {
		name string
		evt  Event
		want string
	}{
		{"login success", Event{"action": "login", "entityType": "session"}, "session.login"},
		{"login failure", Event{"action": "login_failed", "entityType": "session"}, "session.login_failed"},
		{"logout", Event{"action": "logout", "entityType": "session"}, "session.logout"},
		{"api key created", Event{"action": "create", "entityType": "apiKey"}, "apiKey.create"},
		{"api key deleted", Event{"action": "delete", "entityType": "apiKey"}, "apiKey.delete"},
		{"iam policy update", Event{"action": "update", "entityType": "iamPolicy"}, "iamPolicy.update"},
		{"iam policy create", Event{"action": "create", "entityType": "iamPolicy"}, "iamPolicy.create"},
		{"iam group create", Event{"action": "create", "entityType": "iamGroup"}, "iamGroup.create"},
		{"iam group member delete", Event{"action": "delete", "entityType": "iamGroupMember"}, "iamGroupMember.delete"},
		{"credential export", Event{"action": "export", "entityType": "credential"}, "credential.export"},
		{"credential update", Event{"action": "update", "entityType": "credential"}, "credential.update"},
		{"credential create", Event{"action": "create", "entityType": "credential"}, "credential.create"},
		{"settings update", Event{"action": "update", "entityType": "settings"}, "settings.update"},
		{"siem config update", Event{"action": "update", "entityType": "siemConfig"}, "siemConfig.update"},
		{"sso config update", Event{"action": "update", "entityType": "ssoConfig"}, "ssoConfig.update"},
		{"routing rule create", Event{"action": "create", "entityType": "routingRule"}, "routingRule.create"},
		{"hook config delete", Event{"action": "delete", "entityType": "hookConfig"}, "hookConfig.delete"},
		{"provider create", Event{"action": "create", "entityType": "provider"}, "provider.create"},
		{"device enrolled", Event{"action": "create", "entityType": "agentDevice"}, "agentDevice.create"},
		{"user created", Event{"action": "create", "entityType": "nexusUser"}, "nexusUser.create"},
		{"rollback action", Event{"action": "rollback", "entityType": "anything"}, "anything.rollback"},
		{"empty action returns empty", Event{"action": "", "entityType": "session"}, ""},
		{"empty entityType returns empty", Event{"action": "login", "entityType": ""}, ""},
		{"empty event returns empty", Event{}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyAdminEvent(tc.evt); got != tc.want {
				t.Errorf("ClassifyAdminEvent(%v) = %q; want %q", tc.evt, got, tc.want)
			}
		})
	}
}

// TestClassifyAdminEvent_LoginAndOverrideAreCataloged is the F-0192 regression.
// Before the fix, a login-failure row (Action="admin.login.failed", no
// EntityType) classified to "" and an override row (EntityType="thing",
// Action="thing_override_set") classified to the non-cataloged
// "thing.thing_override_set" — both dropped by any SIEM whitelist and absent
// from the picker. They must now map to their canonical cataloged identities.
func TestClassifyAdminEvent_LoginAndOverrideAreCataloged(t *testing.T) {
	cases := []struct {
		name string
		evt  Event
		want string
	}{
		{
			// Exactly the shape control-plane/identity/authserver/login/password.go emits.
			name: "login failure (no entityType) maps to auth.login_failure",
			evt:  Event{"action": "admin.login.failed", "actorLabel": "attacker@example.com"},
			want: "auth.login_failure",
		},
		{
			name: "login success maps to auth.login_success",
			evt:  Event{"action": "admin.login.succeeded", "entityType": "user", "entityId": "u-1"},
			want: "auth.login_success",
		},
		{
			// Exactly the shape nexus-hub fleet override.go / break_glass.go emit.
			name: "node override maps to node.write-override",
			evt:  Event{"action": "thing_override_set", "entityType": "thing", "entityId": "thing-1"},
			want: "node.write-override",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyAdminEvent(tc.evt); got != tc.want {
				t.Errorf("ClassifyAdminEvent(%v) = %q; want %q", tc.evt, got, tc.want)
			}
		})
	}
}

// TestClassifyAdminEvent_CatalogedEventsSurviveWhitelist proves the end-to-end
// F-0192 outcome: once classified, login + override events match an
// explicit SIEM whitelist instead of being silently dropped. The whitelist uses
// the same canonical strings the admin picker now offers.
func TestClassifyAdminEvent_CatalogedEventsSurviveWhitelist(t *testing.T) {
	raw := []Event{
		{"action": "admin.login.failed", "actorLabel": "x"},
		{"action": "thing_override_set", "entityType": "thing", "entityId": "t-1"},
		{"action": "create", "entityType": "provider"}, // ordinary, not in whitelist
	}
	for i := range raw {
		raw[i]["eventType"] = ClassifyAdminEvent(raw[i])
	}
	whitelist := []string{"auth.login_failure", "node.write-override"}
	got := FilterByEventTypes(raw, whitelist)
	if len(got) != 2 {
		t.Fatalf("whitelisted events = %d, want 2 (login + override must survive)", len(got))
	}
	seen := map[string]bool{}
	for _, e := range got {
		seen[e["eventType"].(string)] = true
	}
	if !seen["auth.login_failure"] || !seen["node.write-override"] {
		t.Errorf("whitelist dropped a cataloged event: got %v", seen)
	}
}

func TestFilterByEventTypes(t *testing.T) {
	events := []Event{
		{"eventType": "session.login"},
		{"eventType": "traffic.rate_limited"},
		{"eventType": "iamPolicy.update"},
		{"eventType": "session.login_failed"},
	}

	t.Run("nil filter passes all", func(t *testing.T) {
		if got := FilterByEventTypes(events, nil); len(got) != 4 {
			t.Errorf("got %d events; want 4", len(got))
		}
	})

	t.Run("empty filter passes all", func(t *testing.T) {
		if got := FilterByEventTypes(events, []string{}); len(got) != 4 {
			t.Errorf("got %d events; want 4", len(got))
		}
	})

	t.Run("specific filter", func(t *testing.T) {
		got := FilterByEventTypes(events, []string{"session.login", "session.login_failed"})
		if len(got) != 2 {
			t.Fatalf("got %d events; want 2", len(got))
		}
	})

	t.Run("no matches", func(t *testing.T) {
		got := FilterByEventTypes(events, []string{"credential.export"})
		if len(got) != 0 {
			t.Errorf("got %d events; want 0", len(got))
		}
	})
}
