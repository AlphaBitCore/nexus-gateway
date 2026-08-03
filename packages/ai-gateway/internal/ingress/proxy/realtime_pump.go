package proxy

// realtime_pump.go — the relay core: one goroutine per direction, sequential
// Read → Write, ZERO internal queueing. Backpressure is the transport's own
// (a slow reader stalls the opposite writer at the TCP layer), which makes
// the relay lossless and fixes the per-session memory budget at one
// in-flight frame per direction. Frames are forwarded VERBATIM — no
// re-marshal, no normalization; the relay never mutates bytes.

import (
	"context"
	"errors"
	"time"

	"github.com/coder/websocket"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/ingress/realtimeproxy"
)

// pump relays src → dst until the session ends. tap=true on the
// provider→client direction only: the three metering event types are parsed
// AFTER the bytes have been forwarded (parse-and-forward, never
// store-and-forward — metering can fail open; the relay must not). ALL
// client→provider frames are forwarded opaque; nothing is parsed on that
// path.
func (s *realtimeSession) pump(src, dst *websocket.Conn, tap bool) {
	defer s.wg.Done()
	// An unrecovered panic in a relay goroutine would crash the whole
	// gateway; a recovered one tears down this session only and stamps the
	// session row's error code.
	defer func() {
		if r := recover(); r != nil {
			if s.h.deps.Logger != nil {
				s.h.deps.Logger.Error("realtime pump panic", "panic", r)
			}
			s.teardown(realtimeproxy.ReasonRelayError, websocket.StatusInternalError)
		}
	}()

	srcIsUpstream := tap
	for {
		typ, data, err := src.Read(s.ctx)
		if err != nil {
			s.handleReadFailure(err, srcIsUpstream)
			return
		}
		// The protocol has no binary frames; one from either side is a
		// protocol violation that terminates the session.
		if typ != websocket.MessageText {
			s.teardown(realtimeproxy.ReasonBinaryFrame, websocket.StatusUnsupportedData)
			return
		}

		wctx, cancel := context.WithTimeout(s.ctx, realtimeWriteTimeout)
		werr := dst.Write(wctx, websocket.MessageText, data)
		cancel()
		if werr != nil {
			s.handleWriteFailure(werr, srcIsUpstream)
			return
		}

		if tap {
			s.tapServerEvent(data)
			if s.ctx.Err() != nil {
				return // a tap-driven sever tore the session down
			}
		}
	}
}

// handleReadFailure classifies a pump read error and tears the session down
// accordingly:
//   - read-limit overflow → the library already closed the offending side
//     with 1009; propagate 1009 to the other side.
//   - peer close frame → propagate the peer's own status code + reason
//     category (client vs upstream) to the other side.
//   - anything else (network drop, canceled session) → close normally; an
//     upstream loss additionally surfaces a terminal error event so the
//     client SDK sees a machine-readable reason (there is no mid-session
//     re-dial — failover is connection-establishment only).
func (s *realtimeSession) handleReadFailure(err error, srcIsUpstream bool) {
	if s.ctx.Err() != nil {
		return // teardown already in progress; this is the cascade
	}
	if errors.Is(err, websocket.ErrMessageTooBig) {
		s.teardown(realtimeproxy.ReasonFrameTooBig, websocket.StatusMessageTooBig)
		return
	}
	reason := realtimeproxy.ReasonClientClosed
	if srcIsUpstream {
		reason = realtimeproxy.ReasonUpstreamClosed
	}
	code := websocket.CloseStatus(err)
	if code == -1 {
		// No close frame (network drop / EOF): nothing to propagate; close
		// the other side normally.
		code = websocket.StatusNormalClosure
	}
	if srcIsUpstream {
		s.sendClientErrorEvent("REALTIME_UPSTREAM_CLOSED", "upstream realtime connection closed")
	}
	s.teardown(reason, code)
}

// handleWriteFailure mirrors handleReadFailure for the destination side of
// the relay: a failed/timed-out write to one peer ends the session. A write
// timeout (context deadline) is a relay failure — the peer is too slow or
// wedged; a peer-close error is ordinary close propagation.
func (s *realtimeSession) handleWriteFailure(err error, srcIsUpstream bool) {
	if s.ctx.Err() != nil {
		return
	}
	if errors.Is(err, context.DeadlineExceeded) {
		s.teardown(realtimeproxy.ReasonRelayError, websocket.StatusInternalError)
		return
	}
	// The write failed because the DESTINATION conn is gone; attribute the
	// close to that side (the opposite of src).
	reason := realtimeproxy.ReasonUpstreamClosed
	if srcIsUpstream {
		reason = realtimeproxy.ReasonClientClosed
	}
	code := websocket.CloseStatus(err)
	if code == -1 {
		code = websocket.StatusNormalClosure
	}
	s.teardown(reason, code)
}

// tapServerEvent runs the provider→client tap on an already-forwarded frame.
// response.created starts the per-response wall clock; response.done drives
// the metering row + quota settlement (severing when a reject-mode cap was
// crossed); error records the provider's machine code (never its free-text
// message) for the session row.
func (s *realtimeSession) tapServerEvent(frame []byte) {
	switch realtimeproxy.ClassifyServerEvent(frame) {
	case realtimeproxy.TapResponseCreated:
		s.state.OnResponseCreated(time.Now())
	case realtimeproxy.TapResponseDone:
		if s.meterResponseDone(frame, time.Now()) {
			// The crossing response was already forwarded and settled; the
			// sever bounds the overshoot to ≤ one response beyond the limit.
			s.sendClientErrorEvent("REALTIME_QUOTA_EXCEEDED", "cost quota exceeded")
			s.teardown(realtimeproxy.ReasonQuotaExceeded, websocket.StatusPolicyViolation)
		}
	case realtimeproxy.TapError:
		s.state.OnProviderError(realtimeproxy.ExtractErrorCode(frame))
	}
}
