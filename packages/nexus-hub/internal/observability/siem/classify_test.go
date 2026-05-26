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
