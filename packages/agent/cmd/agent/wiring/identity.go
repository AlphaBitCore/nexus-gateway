package wiring

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/lifecycle/bootstrap"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/identity/enrollment"
)

// SSOAuthState glues the statusapi IPC commands (AUTHENTICATE,
// AUTHENTICATE CONFIRM, AUTHENTICATE CANCEL) to a single enrollment.Flow
// instance. AUTHENTICATE first returns a confirmation prompt when the
// device is already enrolled; the Swift UI then sends AUTHENTICATE CONFIRM
// (which actually runs the OAuth flow and blocks until it finishes) or
// AUTHENTICATE CANCEL (which aborts an in-progress run). The atomic guard
// prevents two concurrent flows when the UI is double-clicked.
type SSOAuthState struct {
	Flow      *enrollment.Flow
	Mgr       *enrollment.Manager
	Bootstrap *bootstrap.Client
	// OnSuccess is invoked once after a successful enrollment run. Used
	// by pending-enrollment mode to signal the runner that the daemon
	// should exit so launchd/systemd can respawn with the full stack.
	// Optional — leave nil in steady-state mode where the IPC is used
	// for re-link only.
	OnSuccess func()
	runMu     sync.Mutex
	running   bool
}

// Authenticate implements status.AuthenticateFn.
// Gate on Hub-published device_auth_mode so an mtls-only fleet
// returns the same "not configured" payload it always did instead
// of pretending SSO is wired up.
func (s *SSOAuthState) Authenticate() (map[string]any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	info, err := s.Bootstrap.Get(ctx)
	if err == nil && info.DeviceAuthMode != "enterprise-login" {
		return nil, fmt.Errorf("enterprise login not configured")
	}

	// When the device is already enrolled, return a confirmation payload
	// instead of starting the flow immediately.
	if s.Mgr.IsEnrolled() {
		return map[string]any{
			"confirmation_required": true,
			"device_id":             s.Mgr.ThingID(),
			"message":               "Signing in with SSO will re-link this device to your identity. Continue?",
		}, nil
	}
	return s.run()
}

// Confirm implements status.ConfirmAuthFn.
func (s *SSOAuthState) Confirm() (map[string]any, error) {
	return s.run()
}

// Cancel implements status.CancelAuthFn.
func (s *SSOAuthState) Cancel() {
	s.Flow.Cancel()
}

func (s *SSOAuthState) run() (map[string]any, error) {
	s.runMu.Lock()
	if s.running {
		s.runMu.Unlock()
		return nil, fmt.Errorf("authentication already in progress")
	}
	s.running = true
	s.runMu.Unlock()
	defer func() {
		s.runMu.Lock()
		s.running = false
		s.runMu.Unlock()
	}()

	result, err := s.Flow.Run(context.Background())
	if err != nil {
		return nil, err
	}
	if s.OnSuccess != nil {
		s.OnSuccess()
	}
	return map[string]any{
		"email":     result.Email,
		"device_id": result.ThingID,
	}, nil
}
