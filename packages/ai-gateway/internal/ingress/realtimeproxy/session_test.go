// Tests for SessionState accounting and close-reason mapping.
//
// Named failure modes: a done without created returns the documented 1 ms
// floor; the first close reason wins over the cascade it triggers; the
// session row's error code prefers the close reason's code over the last
// provider error; concurrent pump/finalize access is race-safe.
package realtimeproxy

import (
	"sync"
	"testing"
	"time"
)

func TestSessionState_ResponseClock(t *testing.T) {
	var s SessionState
	base := time.Unix(1000, 0)

	s.OnResponseCreated(base)
	if ms := s.OnResponseDone(base.Add(2300 * time.Millisecond)); ms != 2300 {
		t.Fatalf("latency = %d, want 2300", ms)
	}

	// done without created → documented 1 ms floor.
	if ms := s.OnResponseDone(base.Add(5 * time.Second)); ms != 1 {
		t.Fatalf("orphan done latency = %d, want floor 1", ms)
	}

	// sub-millisecond exchange floors to 1, never 0 (0 would coerce to a
	// recomputed wall clock downstream).
	s.OnResponseCreated(base)
	if ms := s.OnResponseDone(base); ms != 1 {
		t.Fatalf("zero-duration latency = %d, want floor 1", ms)
	}

	if n, _, _ := s.Snapshot(); n != 3 {
		t.Fatalf("responses = %d, want 3", n)
	}
}

func TestSessionState_CloseReasonFirstWins(t *testing.T) {
	var s SessionState
	s.SetCloseReason(ReasonQuotaExceeded)
	s.SetCloseReason(ReasonClientClosed) // the cascade must not overwrite
	_, reason, code := s.Snapshot()
	if reason != ReasonQuotaExceeded || code != "REALTIME_QUOTA_EXCEEDED" {
		t.Fatalf("reason = %v code = %q, want quota sever preserved", reason, code)
	}
}

func TestSessionState_ErrorCodePrecedence(t *testing.T) {
	t.Run("close_reason_code_wins", func(t *testing.T) {
		var s SessionState
		s.OnProviderError("rate_limit_exceeded")
		s.SetCloseReason(ReasonVKRevoked)
		if _, _, code := s.Snapshot(); code != "REALTIME_VK_REVOKED" {
			t.Fatalf("code = %q, want close-reason code", code)
		}
	})
	t.Run("provider_error_when_normal_close", func(t *testing.T) {
		var s SessionState
		s.OnProviderError("first")
		s.OnProviderError("terminal_error") // last provider error wins
		s.OnProviderError("")               // empty never overwrites
		s.SetCloseReason(ReasonNormal)
		if _, _, code := s.Snapshot(); code != "terminal_error" {
			t.Fatalf("code = %q, want last provider error", code)
		}
	})
}

func TestCloseReason_ErrorCode(t *testing.T) {
	cases := []struct {
		reason CloseReason
		errStr string
	}{
		// Abnormal terminations stamp a machine-readable code.
		{ReasonBinaryFrame, "REALTIME_BINARY_FRAME"},
		{ReasonFrameTooBig, "REALTIME_FRAME_TOO_BIG"},
		{ReasonVKRevoked, "REALTIME_VK_REVOKED"},
		{ReasonQuotaExceeded, "REALTIME_QUOTA_EXCEEDED"},
		{ReasonSessionGuard, "REALTIME_SESSION_GUARD"},
		{ReasonRelayError, "REALTIME_RELAY_ERROR"},
		// The benign ends of a voice session stamp NO code — otherwise every
		// realtime session row reads as an error to an error_code filter (a
		// clean client close is the ordinary way a call ends). The close
		// reason still rides the session-row metadata.
		{ReasonNormal, ""},
		{ReasonClientClosed, ""},
		{ReasonUpstreamClosed, ""},
	}
	for _, tc := range cases {
		if got := tc.reason.ErrorCode(); got != tc.errStr {
			t.Errorf("%v.ErrorCode() = %q, want %q", tc.reason, got, tc.errStr)
		}
	}
}

func TestSessionState_RaceSafe(t *testing.T) {
	var s SessionState
	var wg sync.WaitGroup
	base := time.Unix(0, 0)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				s.OnResponseCreated(base)
				s.OnResponseDone(base.Add(time.Millisecond))
				s.OnProviderError("e")
				s.Snapshot()
			}
		}()
	}
	wg.Wait()
	s.SetCloseReason(ReasonNormal)
	if n, _, _ := s.Snapshot(); n != 8*200 {
		t.Fatalf("responses = %d, want %d", n, 8*200)
	}
}
