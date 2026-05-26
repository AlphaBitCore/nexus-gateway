package thingclient

import (
	"sync"
	"time"
)

// ApplyError describes a single failed config-apply attempt. Carried on
// ApplyOutcome.ApplyError and serialized into shadow_report so Hub can
// surface it to operators on the Nodes page without round-tripping
// through logs or Prometheus.
type ApplyError struct {
	Message string    `json:"message"`
	At      time.Time `json:"at"`
}

// ApplyOutcome is the per-key outcome of the most recent OnConfigChanged
// dispatch. It is additive metadata alongside the existing flattened
// `reported` map — the byte-comparable state stays in `reported`, while
// AppliedAt / AppliedVersion / ApplyError add the "did it actually work?"
// channel missing from the raw reported bytes alone.
//
// Semantics:
//   - AppliedAt + AppliedVersion are the LAST KNOWN SUCCESSFUL apply.
//     On a fresh success, both advance. On failure, both are preserved
//     from the previous successful tick — operators see "still serving
//     v=41, latest attempt v=42 failed".
//   - ApplyError is the MOST RECENT failure. Cleared the moment a fresh
//     success lands. Operators see only an active error; resolved
//     errors disappear from the UI without further action.
type ApplyOutcome struct {
	AppliedAt      *time.Time  `json:"appliedAt,omitempty"`
	AppliedVersion *int64      `json:"appliedVersion,omitempty"`
	ApplyError     *ApplyError `json:"applyError,omitempty"`
}

// OutcomeTracker is the per-Client in-memory ledger of apply outcomes.
// Services call Record() from inside their OnConfigChanged dispatch
// switch; the Client pulls Snapshot() into every outgoing shadow_report
// so Hub receives the current ledger.
//
// State is *process-scoped*: it resets on process restart and the next
// successful apply repopulates it. Hub correlates this with the new
// thing.process_started_at column to detect "lost outcome ledger" cases
// (post-restart, no apply yet → AppliedAt nil → UI renders "pending").
//
// The tracker is goroutine-safe: a sync.Mutex serialises Record + Snapshot
// without blocking the hot config-apply path noticeably (Record is O(1),
// Snapshot is O(N keys) and called once per shadow_report).
type OutcomeTracker struct {
	mu    sync.Mutex
	state map[string]ApplyOutcome
}

// NewOutcomeTracker constructs an empty tracker. Always exported so
// tests / mock harnesses can build one without a full *Client.
func NewOutcomeTracker() *OutcomeTracker {
	return &OutcomeTracker{state: map[string]ApplyOutcome{}}
}

// Record updates the tracker with the outcome of a single config-key
// apply attempt.
//
//   - On success (err == nil): AppliedAt/AppliedVersion advance to the
//     incoming desiredVer/now; any previous ApplyError is cleared.
//   - On failure (err != nil): ApplyError is set with the message + time;
//     AppliedAt + AppliedVersion are preserved from the previous tick so
//     the UI can show "last good version" alongside "latest error".
//
// Empty key is a programmer error and silently dropped — services should
// never call Record("", ...).
func (t *OutcomeTracker) Record(key string, desiredVer int64, err error) {
	if t == nil || key == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	prev := t.state[key]
	now := time.Now().UTC()
	if err == nil {
		prev.AppliedAt = &now
		v := desiredVer
		prev.AppliedVersion = &v
		prev.ApplyError = nil
	} else {
		msg := err.Error()
		prev.ApplyError = &ApplyError{Message: msg, At: now}
		// AppliedAt / AppliedVersion deliberately preserved.
	}
	t.state[key] = prev
}

// Snapshot returns a stable copy of the current per-key outcomes. The
// returned map is safe to encode without further locking. An empty
// tracker returns an empty (non-nil) map so JSON encoders emit
// `"reportedOutcomes":{}` rather than null.
func (t *OutcomeTracker) Snapshot() map[string]ApplyOutcome {
	if t == nil {
		return map[string]ApplyOutcome{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]ApplyOutcome, len(t.state))
	for k, v := range t.state {
		out[k] = v
	}
	return out
}

// Outcomes returns the Client's apply-outcome tracker. Services call this
// once at wiring time and stash the result, then invoke tracker.Record
// from each dispatch case of their OnConfigChanged switch. The tracker
// is lazily created at NewClient time and lives for the Client's
// lifetime — services do not need to manage its lifecycle.
func (c *Client) Outcomes() *OutcomeTracker {
	return c.outcomes
}
