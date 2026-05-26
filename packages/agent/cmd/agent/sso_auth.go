package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/lifecycle/bootstrap"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/identity/enrollment"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/sync/status"
)

// ssoAuthState glues the statusapi IPC commands (AUTHENTICATE,
// AUTHENTICATE CONFIRM, AUTHENTICATE CANCEL) to a single enrollment.Flow
// instance. AUTHENTICATE first returns a confirmation prompt when the
// device is already enrolled; the Swift UI then sends AUTHENTICATE CONFIRM
// (which runs the OAuth flow and blocks until it finishes) or
// AUTHENTICATE CANCEL (which aborts an in-progress run). The atomic guard
// prevents two concurrent flows when the UI is double-clicked.
type ssoAuthState struct {
	flow      *enrollment.Flow
	mgr       *enrollment.Manager
	bootstrap *bootstrap.Client
	// onSuccess is invoked once after a successful enrollment run. Used
	// by pending-enrollment mode to signal the runner that the daemon
	// should exit so launchd/systemd can respawn with the full stack.
	// Optional — leave nil in steady-state mode.
	onSuccess func()
	runMu     sync.Mutex
	running   bool
}

func (s *ssoAuthState) authenticate() (map[string]any, error) {
	// Gate on Hub-published device_auth_mode so an mtls-only fleet
	// returns "not configured" instead of pretending SSO is wired up.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	info, err := s.bootstrap.Get(ctx)
	if err == nil && info.DeviceAuthMode != "enterprise-login" {
		return nil, fmt.Errorf("enterprise login not configured")
	}

	// When the device is already enrolled, return a confirmation payload
	// instead of starting the flow immediately.
	if s.mgr.IsEnrolled() {
		return map[string]any{
			"confirmation_required": true,
			"device_id":             s.mgr.ThingID(),
			"message":               "Signing in with SSO will re-link this device to your identity. Continue?",
		}, nil
	}
	return s.run()
}

func (s *ssoAuthState) confirm() (map[string]any, error) {
	return s.run()
}

func (s *ssoAuthState) cancel() {
	s.flow.Cancel()
}

func (s *ssoAuthState) run() (map[string]any, error) {
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

	result, err := s.flow.Run(context.Background())
	if err != nil {
		return nil, err
	}
	if s.onSuccess != nil {
		s.onSuccess()
	}
	return map[string]any{
		"email":     result.Email,
		"device_id": result.ThingID,
	}, nil
}

// Compile-time check: ssoAuthState methods must satisfy statusapi function types.
var _ status.AuthenticateFn = status.AuthenticateFn((*ssoAuthState)(nil).authenticate)
var _ status.ConfirmAuthFn = status.ConfirmAuthFn((*ssoAuthState)(nil).confirm)
var _ status.CancelAuthFn = status.CancelAuthFn((*ssoAuthState)(nil).cancel)
