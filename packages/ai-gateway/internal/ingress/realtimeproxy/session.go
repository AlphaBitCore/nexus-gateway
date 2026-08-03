package realtimeproxy

import (
	"sync"
	"time"
)

// CloseReason names why a session ended. It drives the session row's
// error-code stamp (see ErrorCode). The WS close CODE sent to the peer is
// chosen by the relay itself — a policy/guard sever picks a fixed status,
// while a peer-initiated close propagates that peer's own status code — so
// the code is not derivable from the reason alone and lives at the teardown
// call sites, not here.
type CloseReason int

const (
	// ReasonNormal — clean close from either side.
	ReasonNormal CloseReason = iota
	// ReasonBinaryFrame — a binary frame arrived; the protocol is
	// text-frames-only.
	ReasonBinaryFrame
	// ReasonFrameTooBig — a frame exceeded the per-frame read limit.
	ReasonFrameTooBig
	// ReasonVKRevoked — the 60 s recheck got a definitive negative.
	ReasonVKRevoked
	// ReasonQuotaExceeded — the per-response post-settle evaluation found a
	// reject-mode level over its limit.
	ReasonQuotaExceeded
	// ReasonSessionGuard — the built-in 65-minute guard fired.
	ReasonSessionGuard
	// ReasonUpstreamClosed — the provider side closed or failed first.
	ReasonUpstreamClosed
	// ReasonClientClosed — the client side closed or failed first.
	ReasonClientClosed
	// ReasonRelayError — an internal relay failure (write timeout, panic
	// recovery) tore the session down.
	ReasonRelayError
)

// ErrorCode is the audit error-code stamp for the session row. It is EMPTY
// for the benign ways a voice session ends — a clean client or upstream
// close, or a normal teardown — because every realtime session row would
// otherwise read as an error to an operator filtering `traffic_event` by
// error_code (the ordinary end of a voice call is a client close). The
// close reason is always carried in the session row's metadata regardless;
// only genuinely abnormal terminations stamp a code here. A benign upstream
// close returns "" so the last provider error (if the provider sent one
// before closing) surfaces through SessionState instead.
func (r CloseReason) ErrorCode() string {
	switch r {
	case ReasonBinaryFrame:
		return "REALTIME_BINARY_FRAME"
	case ReasonFrameTooBig:
		return "REALTIME_FRAME_TOO_BIG"
	case ReasonVKRevoked:
		return "REALTIME_VK_REVOKED"
	case ReasonQuotaExceeded:
		return "REALTIME_QUOTA_EXCEEDED"
	case ReasonSessionGuard:
		return "REALTIME_SESSION_GUARD"
	case ReasonRelayError:
		return "REALTIME_RELAY_ERROR"
	}
	// ReasonNormal / ReasonClientClosed / ReasonUpstreamClosed → benign.
	return ""
}

// SessionState is the per-session accounting shared between the two pumps
// and the finalize path. All methods are safe for concurrent use.
type SessionState struct {
	mu              sync.Mutex
	responses       int
	lastErrorCode   string // provider error code/type, trimmed — never message text
	closeReason     CloseReason
	closeSet        bool
	responseStarted time.Time // zero = no response in flight
}

// OnResponseCreated starts the per-response wall clock. The protocol runs
// one response at a time; a second created before done simply restarts the
// clock (the later latency wins — closest to the real exchange).
func (s *SessionState) OnResponseCreated(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.responseStarted = now
}

// OnResponseDone counts the response and returns the created→done wall
// clock in milliseconds, floored to 1. A done without a created (should
// not happen on a healthy stream) returns 1 — the documented floor.
func (s *SessionState) OnResponseDone(now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.responses++
	if s.responseStarted.IsZero() {
		return 1
	}
	ms := int(now.Sub(s.responseStarted).Milliseconds())
	s.responseStarted = time.Time{}
	if ms < 1 {
		ms = 1
	}
	return ms
}

// OnProviderError records the provider error's trimmed code — last one
// wins (the terminal error is the one worth surfacing on the session row).
func (s *SessionState) OnProviderError(code string) {
	if code == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastErrorCode = code
}

// SetCloseReason records why the session ended. First writer wins: the
// originating cause must not be overwritten by the cascade it triggers
// (e.g. a policy sever also surfaces as a peer close a moment later).
func (s *SessionState) SetCloseReason(r CloseReason) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closeSet {
		return
	}
	s.closeReason, s.closeSet = r, true
}

// Snapshot returns the accounting for the session row: response count,
// close reason (ReasonNormal when never set), and the audit error code —
// the close reason's code, else the last provider error code.
func (s *SessionState) Snapshot() (responses int, reason CloseReason, errorCode string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	code := s.closeReason.ErrorCode()
	if code == "" {
		code = s.lastErrorCode
	}
	return s.responses, s.closeReason, code
}
