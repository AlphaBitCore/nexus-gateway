package revocation_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/revocation"
)

func TestRevocationEvent_JSONRoundTrip(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	cases := []struct {
		name  string
		event revocation.Event
	}{
		{
			name: "jti",
			event: revocation.Event{
				EventID:   "evt_jti",
				RevokedAt: now,
				ExpiresAt: now.Add(time.Hour),
				Scope:     revocation.ScopeJTI,
				TargetJTI: "00000000-0000-0000-0000-000000000001",
				Reason:    revocation.ReasonReplayDetected,
			},
		},
		{
			name: "user",
			event: revocation.Event{
				EventID:      "evt_user",
				RevokedAt:    now,
				ExpiresAt:    now.Add(time.Hour),
				Scope:        revocation.ScopeUser,
				TargetUserID: "user_1",
				Reason:       revocation.ReasonAdminDisable,
			},
		},
		{
			name: "device",
			event: revocation.Event{
				EventID:        "evt_device",
				RevokedAt:      now,
				ExpiresAt:      now.Add(time.Hour),
				Scope:          revocation.ScopeDevice,
				TargetDeviceID: "dev_1",
				Reason:         revocation.ReasonUnenroll,
			},
		},
		{
			name: "session",
			event: revocation.Event{
				EventID:         "evt_session",
				RevokedAt:       now,
				ExpiresAt:       now.Add(time.Hour),
				Scope:           revocation.ScopeSession,
				TargetSessionID: "sess_1",
				Reason:          revocation.ReasonUserLogout,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.event)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var got revocation.Event
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got != tc.event {
				t.Fatalf("round-trip mismatch:\n want %+v\n got  %+v", tc.event, got)
			}
		})
	}
}

func TestRevocationEvent_RejectsUnknownScope(t *testing.T) {
	raw := []byte(`{"event_id":"e","revoked_at":"2026-01-01T00:00:00Z","expires_at":"2026-01-01T00:00:00Z","scope":"garbage","reason":"x"}`)
	var ev revocation.Event
	if err := json.Unmarshal(raw, &ev); err == nil {
		t.Fatalf("expected error for unknown scope, got none (ev=%+v)", ev)
	}
}
