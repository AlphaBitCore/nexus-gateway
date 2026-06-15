package audit

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	sharedndjson "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit/ndjson"
)

// NDJSONWriter writes audit events to local NDJSON spool files as a fallback
// when the primary database/MQ writer is unavailable or the queue overflows.
//
// It is a thin, AuditEvent-typed adapter over the shared spill writer
// (packages/shared/audit/ndjson): this file owns only the compliance-proxy
// wire shape (eventToMap) and metrics, while file rotation, the per-instance
// total-size quota, and per-instance directory isolation live in the shared
// package and are identical to the ai-gateway audit spill path.
type NDJSONWriter struct {
	w      *sharedndjson.Writer
	logger *slog.Logger
}

// NewNDJSONWriter creates an NDJSON writer with the given spool directory and
// limits. maxFileSizeMB caps a single spool file before rotation;
// maxTotalSizeMB caps the instance's total on-disk spool.
func NewNDJSONWriter(dir, instanceID string, maxFileSizeMB, maxTotalSizeMB int, logger *slog.Logger) (*NDJSONWriter, error) {
	w, err := sharedndjson.New(dir, instanceID, maxFileSizeMB, maxTotalSizeMB, func(n int) {
		if NDJSONWrites != nil {
			NDJSONWrites.With().Inc()
		}
		if NDJSONBytes != nil {
			NDJSONBytes.With().Add(float64(n))
		}
	})
	if err != nil {
		return nil, err
	}
	return &NDJSONWriter{w: w, logger: logger}, nil
}

// Write marshals an audit event to one JSON line and appends it to the spool.
// File rotation and the total-size quota are enforced by the shared writer; a
// quota-exceeded or I/O error is returned to the caller (never dropped here).
func (n *NDJSONWriter) Write(event AuditEvent) error {
	data, err := json.Marshal(eventToMap(event))
	if err != nil {
		n.logger.Error("audit/ndjson: marshal event failed", "id", event.ID, "error", err)
		return fmt.Errorf("audit/ndjson: marshal event: %w", err)
	}
	return n.w.Write(data)
}

// Close closes the underlying spool file handle.
func (n *NDJSONWriter) Close() error {
	return n.w.Close()
}

// eventToMap converts an AuditEvent to a map for JSON marshalling,
// preserving all fields including nil-able ones.
func eventToMap(e AuditEvent) map[string]any {
	m := map[string]any{
		"id":                  e.ID,
		"transactionId":       e.TransactionID,
		"connectionId":        e.ConnectionID,
		"trafficSource":       e.TrafficSource,
		"ingressType":         e.IngressType,
		"bumpStatus":          e.BumpStatus,
		"sourceIp":            e.SourceIP,
		"targetHost":          e.TargetHost,
		"method":              e.Method,
		"path":                e.Path,
		"requestHookDecision": e.RequestHookDecision,
		"latencyMs":           e.LatencyMs,
		"timestamp":           e.Timestamp.Format(time.RFC3339Nano),
	}

	if e.StatusCode != nil {
		m["statusCode"] = *e.StatusCode
	}
	if e.RequestHookReason != nil {
		m["requestHookReason"] = *e.RequestHookReason
	}
	if e.RequestHookReasonCode != nil {
		m["requestHookReasonCode"] = *e.RequestHookReasonCode
	}
	if e.RequestHooksPipeline != nil {
		m["requestHooksPipeline"] = json.RawMessage(e.RequestHooksPipeline)
	}
	if e.ResponseHookDecision != nil {
		m["responseHookDecision"] = *e.ResponseHookDecision
	}
	if e.ResponseHookReason != nil {
		m["responseHookReason"] = *e.ResponseHookReason
	}
	if e.ResponseHookReasonCode != nil {
		m["responseHookReasonCode"] = *e.ResponseHookReasonCode
	}
	if e.ResponseHooksPipeline != nil {
		m["responseHooksPipeline"] = json.RawMessage(e.ResponseHooksPipeline)
	}
	if len(e.ComplianceTags) > 0 {
		m["complianceTags"] = e.ComplianceTags
	}
	if e.SubjectID != nil {
		m["subjectId"] = *e.SubjectID
	}
	if e.DSARDeleteRequested != nil {
		m["dsarDeleteRequested"] = *e.DSARDeleteRequested
	}
	if e.UserAgent != nil {
		m["userAgent"] = *e.UserAgent
	}

	return m
}
