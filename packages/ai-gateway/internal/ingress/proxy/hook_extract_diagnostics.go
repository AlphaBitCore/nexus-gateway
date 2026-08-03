package proxy

import (
	"log/slog"
	"sync"
	"sync/atomic"
)

// extractFailLogEvery throttles the extract-failure warning. A malformed provider
// response can repeat for every request to one model, and this is a per-request
// event: unthrottled it would drown the log exactly when an operator needs to read
// it. Every 50th occurrence carries the running total, so the rate is visible
// without the volume.
const extractFailLogEvery = 50

// Throttled PER (direction, adapter), not process-wide. A single global counter
// would let one high-rate failure — say a provider returning malformed SSE on
// every request to one model — absorb the whole 1-in-50 budget and permanently
// mask a low-rate failure on a different adapter. The masked one is exactly the
// case an operator needs to see, because a rare content-blind message is the
// hard one to notice any other way.
//
// The map is bounded by the adapter population (a fixed, small set), so it does
// not grow with traffic.
var extractFailLogCounts sync.Map // key: "direction|adapterID" → *atomic.Uint64

func extractFailCounter(key string) *atomic.Uint64 {
	if v, ok := extractFailLogCounts.Load(key); ok {
		return v.(*atomic.Uint64)
	}
	v, _ := extractFailLogCounts.LoadOrStore(key, new(atomic.Uint64))
	return v.(*atomic.Uint64)
}

// logExtractFailure reports that the hook pipeline ran CONTENT-BLIND for one
// request or response.
//
// This was a Debug line, and Debug is off in practice — measured, not assumed.
// The counter `nexus_traffic_extract_total{outcome=error}` would increment and
// nothing on the box could say which adapter, which path, or why. That is the
// shape of defect this program keeps finding: a signal that something went wrong,
// with the explanation behind a switch nobody has on.
//
// WARN rather than ERROR is deliberate. The audit row is still written and the
// request still completes; what is degraded is that the content hooks saw nothing
// to scan for THIS message. That is a compliance-relevant gap and belongs in the
// log an operator actually reads — but it is not a process-level failure, and
// grading it ERROR would train operators to ignore ERRORs.
//
// The consequence is stated in the message rather than left to be inferred: a
// reader who only sees "extract failed" has to know the pipeline architecture to
// understand that no scanning happened.
func logExtractFailure(logger *slog.Logger, direction, adapterID, path string, bodyBytes int, err error) {
	if logger == nil {
		return
	}
	n := extractFailCounter(direction + "|" + adapterID).Add(1)
	if n%extractFailLogEvery != 1 {
		return
	}
	logger.Warn("content extract failed; hooks saw NO content for this "+direction+" (throttled)",
		slog.String("direction", direction),
		slog.String("adapter", adapterID),
		slog.String("path", path),
		slog.Int("body_bytes", bodyBytes),
		slog.String("error", err.Error()),
		slog.Uint64("occurrences_for_this_adapter", n),
		slog.String("consequence", "content-scanning hooks were not given this message's text, so nothing was scanned for it; "+
			"the audit row is written and the request completes normally, which is why the counter is the only other signal"),
	)
}
