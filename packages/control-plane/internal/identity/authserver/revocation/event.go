package revocation

import (
	"encoding/json"
	"fmt"
	"time"
)

// Scope is the revocation granularity -- matches the CHECK constraint on
// revoked_token.scope.
type Scope string

const (
	ScopeJTI     Scope = "jti"
	ScopeUser    Scope = "user"
	ScopeDevice  Scope = "device"
	ScopeSession Scope = "session"
)

func (s Scope) valid() bool {
	switch s {
	case ScopeJTI, ScopeUser, ScopeDevice, ScopeSession:
		return true
	}
	return false
}

// Reason strings are free-form for observability but we define the canonical
// set used by the auth server. See spec section 8.3 "reason" field.
const (
	ReasonUserLogout     = "user_logout"
	ReasonAdminDisable   = "admin_disable"
	ReasonReplayDetected = "replay_detected"
	ReasonUnenroll       = "unenroll"
	ReasonRoleChange     = "role_change"
	ReasonIdPDisable     = "idp_disable"
)

// Event is the wire shape of a nexus.auth.revocation message. Only one of
// TargetJTI / TargetUserID / TargetDeviceID / TargetSessionID is set per
// event, determined by Scope.
type Event struct {
	EventID         string    `json:"event_id"`
	RevokedAt       time.Time `json:"revoked_at"`
	ExpiresAt       time.Time `json:"expires_at"`
	Scope           Scope     `json:"scope"`
	TargetJTI       string    `json:"target_jti,omitempty"`
	TargetUserID    string    `json:"target_user_id,omitempty"`
	TargetDeviceID  string    `json:"target_device_id,omitempty"`
	TargetSessionID string    `json:"target_session_id,omitempty"`
	Reason          string    `json:"reason"`
}

// UnmarshalJSON enforces the scope allow-list so forward-compat junk cannot
// slip through as a silent no-op.
func (e *Event) UnmarshalJSON(b []byte) error {
	type alias Event
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	if !a.Scope.valid() {
		return fmt.Errorf("revocation: unknown scope %q", a.Scope)
	}
	*e = Event(a)
	return nil
}
