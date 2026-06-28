package enrollstore

import (
	"context"
	"fmt"
	"github.com/goccy/go-json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/traffic/chain"
)

// DeviceAssignmentAuditEntry is the shape every device-assignment audit
// emit carries. Mirrors the structurally identical adminAuditEntry from
// fleet/manager/override.go — duplicated rather than imported because
// the override package is in a different layer and exposing the helper
// there would create a cycle. The two helpers MUST stay in sync; the
// canonical write SQL + chain hashing live in
// writeDeviceAssignmentAuditTx below.
type DeviceAssignmentAuditEntry struct {
	ActorID     string
	ActorLabel  string
	Action      string // typically "device-assignment.update"
	EntityID    string // device thing_id
	BeforeState any    // previous (deviceId, userId, source, assignedAt) — nil for first-bind
	AfterState  any    // new (deviceId, userId, source, loginMethod, ipAddress, boundAt)
}

// WriteDeviceAssignmentAudit publishes one device-assignment audit row
// using the canonical Hub-side chain-aware writer. Opens a short-lived
// transaction so the chain.NextHash advisory lock + the row INSERT land
// atomically; the broader UpsertDeviceAssignment intentionally stays
// outside that transaction so a (rare) audit write failure cannot abort
// the device-binding mutation the operator just requested.
//
// Errors are returned to the caller for logging; the audit failure is
// non-fatal at the binding-flow layer. Callers must handle the error by
// logging a warn — silently dropping it would leave an unrecorded
// device-to-user binding in the AdminAuditLog.
func (s *Store) WriteDeviceAssignmentAudit(ctx context.Context, e DeviceAssignmentAuditEntry) error {
	if e.ActorID == "" {
		return fmt.Errorf("WriteDeviceAssignmentAudit: ActorID required")
	}
	if e.Action == "" {
		return fmt.Errorf("WriteDeviceAssignmentAudit: Action required")
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin audit tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if err := writeDeviceAssignmentAuditTx(ctx, tx, e); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit audit tx: %w", err)
	}
	return nil
}

// writeDeviceAssignmentAuditTx is the in-transaction worker that the
// public WriteDeviceAssignmentAudit wraps. Split out so a future caller
// running its own transaction (e.g. a multi-step bulk-assignment job)
// can reuse the same chain-aware insert without opening a fresh tx.
//
// The implementation mirrors fleet/manager/override.go's
// insertAdminAuditLog one-for-one — same chain.NewHashPayload + NextHash
// + INSERT SQL shape. Differences are intentional caller-supplied
// fields only.
func writeDeviceAssignmentAuditTx(ctx context.Context, tx pgx.Tx, e DeviceAssignmentAuditEntry) error {
	var beforeJSON, afterJSON json.RawMessage
	if e.BeforeState != nil {
		b, err := json.Marshal(e.BeforeState)
		if err != nil {
			return fmt.Errorf("marshal BeforeState: %w", err)
		}
		beforeJSON = b
	}
	if e.AfterState != nil {
		a, err := json.Marshal(e.AfterState)
		if err != nil {
			return fmt.Errorf("marshal AfterState: %w", err)
		}
		afterJSON = a
	}

	const entityType = "device-assignment"
	now := time.Now().UTC()
	payload, err := chain.NewHashPayload(e.Action, e.ActorID, entityType, e.EntityID)
	if err != nil {
		return fmt.Errorf("build hash payload: %w", err)
	}
	payload.TimestampMs = now.UnixMilli()
	payload.BeforeState = beforeJSON
	payload.AfterState = afterJSON

	prevHash, integrityHash, hashInput, err := chain.NextHash(ctx, tx, payload)
	if err != nil {
		return fmt.Errorf("compute chain hash: %w", err)
	}

	var prevArg any
	if prevHash != "" {
		prevArg = prevHash
	}
	var beforeArg, afterArg any
	if len(beforeJSON) > 0 {
		beforeArg = []byte(beforeJSON)
	}
	if len(afterJSON) > 0 {
		afterArg = []byte(afterJSON)
	}

	id := uuid.New().String()
	if _, err := tx.Exec(ctx, `
		INSERT INTO "AdminAuditLog" (
			id, timestamp,
			"actorId", "actorLabel", "actorRole",
			action, "entityType", "entityId",
			"beforeState", "afterState",
			"previousHash", "integrityHash", "hashInput"
		) VALUES (
			$1, to_timestamp($2 / 1000.0),
			$3, $4, $5,
			$6, $7, $8,
			$9, $10,
			$11, $12, $13
		)
	`,
		id, payload.TimestampMs,
		e.ActorID, nilStringIfEmpty(e.ActorLabel), nil,
		e.Action, entityType, nilStringIfEmpty(e.EntityID),
		beforeArg, afterArg,
		prevArg, integrityHash, hashInput,
	); err != nil {
		return fmt.Errorf("insert AdminAuditLog (device-assignment): %w", err)
	}
	return nil
}

// nilStringIfEmpty maps "" → nil so the column lands as SQL NULL rather
// than empty-string. AdminAuditLog.entityId and actorLabel are nullable;
// the SIEM bridge classifier treats NULL distinctly from "".
func nilStringIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
