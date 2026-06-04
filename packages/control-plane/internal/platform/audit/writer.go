// Package audit provides admin audit log writing for the control-plane.
package audit

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
	"github.com/google/uuid"
)

// Entry represents a single admin audit log entry.
type Entry struct {
	ActorID        string
	ActorLabel     string
	ActorRole      string
	SourceIP       string
	Action         string
	EntityType     string
	EntityID       string
	BeforeState    any
	AfterState     any
	NexusRequestID string
	// Via is the request channel ("assistant" for AI-initiated web-assistant
	// writes, empty for direct human/UI actions). EntryFor populates it from the
	// unforgeable in-process initiator context value (WithInitiator), set only by the
	// assistant self-call transport; it flows to the Hub consumer and into the
	// tamper-evident audit hash chain so AI writes are distinguishable from human
	// ones (E90 I5).
	Via string
}

// FailureObserver is called by Writer.Log on every failure to publish an
// audit entry (marshal error or MQ enqueue error). The wiring layer
// (cmd/control-plane/main.go) supplies a closure that increments the
// admin.audit_log_failed_total{action} Prometheus counter — keeping the
// audit package decoupled from the metrics package so we can avoid a
// circular import (handler→audit, handler→metrics, metrics→opsmetrics;
// audit must not pull metrics in).
//
// `action` is the audit action (e.g. "thing_force_resync"). Implementations
// must be safe for concurrent use; Log holds no lock around the call.
type FailureObserver func(action string)

// Writer publishes admin audit log entries to MQ. Hash chain computation
// happens Hub-side (packages/nexus-hub/internal/observability/audit/chain.go) so the CP
// is now a pure formatter+publisher; concurrent admin actions across CP
// replicas no longer need a shared chain head here.
type Writer struct {
	producer mq.Producer
	queue    string
	logger   *slog.Logger
	onFail   FailureObserver
}

// NewWriter creates an audit writer that publishes to MQ.
// If producer is nil, Log calls are silently dropped (no-op mode).
func NewWriter(producer mq.Producer, queue string, logger *slog.Logger) *Writer {
	return &Writer{producer: producer, queue: queue, logger: logger}
}

// WithFailureObserver returns w with onFail set. The hook fires once per
// failed Log call (marshal or enqueue error), with the entry's action
// passed through so the metric carries enough cardinality-bounded
// dimension to drive per-action alerts. nil disables the hook.
func (w *Writer) WithFailureObserver(onFail FailureObserver) *Writer {
	w.onFail = onFail
	return w
}

// Log publishes an audit entry to MQ using a detached context so client
// disconnects cannot cancel the write. The returned error is ALSO surfaced
// via FailureObserver + a warn log — callers in admin handlers may safely
// ignore the return value for fire-and-forget paths but should NOT fail
// the user-visible request because of it (the upstream operation, e.g. a
// force-resync at Hub, has already committed).
//
// Returning an error gives tests a deterministic way to assert the
// failure path without relying on log or counter side effects, and lets
// future critical-audit paths choose to surface the failure to the
// operator.
func (w *Writer) Log(_ context.Context, e Entry) error {
	if w.producer == nil {
		return nil
	}

	msg := mq.AdminAuditMessage{
		ID:             uuid.New().String(),
		Timestamp:      time.Now().UTC(),
		ActorID:        e.ActorID,
		ActorLabel:     e.ActorLabel,
		ActorRole:      e.ActorRole,
		SourceIP:       e.SourceIP,
		Action:         e.Action,
		EntityType:     e.EntityType,
		EntityID:       e.EntityID,
		BeforeState:    e.BeforeState,
		AfterState:     e.AfterState,
		NexusRequestID: e.NexusRequestID,
		Via:            e.Via,
	}

	data, err := json.Marshal(msg)
	if err != nil {
		w.observeFailure(e, err, "marshal")
		return err
	}

	writeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := w.producer.Enqueue(writeCtx, w.queue, data); err != nil {
		w.observeFailure(e, err, "enqueue")
		return err
	}
	return nil
}

// LogObserved is the fire-and-forget variant of Log: failures are already
// surfaced through Writer.observeFailure (warn log + FailureObserver
// metric), so the typical admin-handler caller has nothing actionable to
// do with the returned error from Log. Use LogObserved for those call
// sites; reserve Log() for the rare path that needs to react to the
// failure (e.g. break-glass paths that must escalate to an operator).
func (w *Writer) LogObserved(ctx context.Context, e Entry) {
	_ = w.Log(ctx, e)
}

// observeFailure runs the warn log + observer hook on a publish failure.
// Stage names the failing step ("marshal" or "enqueue") so the warn log
// distinguishes "audit row never marshaled" from "MQ rejected the
// payload" — both are ops-visible gaps but have different remediation.
func (w *Writer) observeFailure(e Entry, err error, stage string) {
	if w.logger != nil {
		w.logger.Warn("audit log publish failed",
			"event", "admin_audit_log_publish_failed",
			"stage", stage,
			"action", e.Action,
			"entityType", e.EntityType,
			"entityId", e.EntityID,
			"error", err.Error(),
		)
	}
	if w.onFail != nil {
		w.onFail(e.Action)
	}
}
