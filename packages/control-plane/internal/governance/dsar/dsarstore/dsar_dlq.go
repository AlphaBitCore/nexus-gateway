// Dead-letter erasure for DSAR.
//
// Its own file because it is the one erasure stage that cannot be a SQL
// predicate: traffic_event_dlq stores the event as an opaque payload with no
// entity column, so ownership has to be decided in Go after decoding. Keeping
// it beside the transaction that calls it would put a decoder and a device
// -window matcher inside a file that is otherwise all SQL.
package dsarstore

import (
	"context"
	"fmt"
	"time"

	"github.com/goccy/go-json"

	"github.com/jackc/pgx/v5"
)

// dlqEnvelope is the subset of the dead-lettered traffic event this store has
// to read to decide ownership.
//
// Declared here rather than imported from the Hub consumer that writes it: the
// two are separate services and the wire between them is the contract, not a
// shared Go type. Only these four fields are read, and every one of them is
// already load-bearing for the live-table predicates above.
type dlqEnvelope struct {
	Source    string    `json:"source"`
	EntityID  *string   `json:"entityId"`
	ThingID   *string   `json:"thingId"`
	Timestamp time.Time `json:"timestamp"`
}

// deviceWindow is one interval during which a device belonged to the subject.
type deviceWindow struct {
	deviceID   string
	assignedAt time.Time
	releasedAt *time.Time
}

// eraseSubjectDLQ removes the subject's rows from traffic_event_dlq.
//
// Ownership is decided in Go rather than in SQL because the DLQ stores the
// event as an opaque payload — there is no entity column to write a predicate
// against, which is exactly why every other stage in this transaction misses
// it. The agent leg's assignment windows are read once and matched here for
// the same reason.
//
// The scan is unbounded on purpose. The DLQ is a failure surface: in steady
// state it is empty, and a queue large enough for this to matter is itself an
// incident. Sampling it would mean an erasure that silently leaves rows behind,
// which is the failure this stage exists to remove.
func eraseSubjectDLQ(ctx context.Context, tx pgx.Tx, subjectID string) (int, error) {
	windows, err := subjectDeviceWindows(ctx, tx, subjectID)
	if err != nil {
		return 0, err
	}

	rows, err := tx.Query(ctx, `SELECT id, payload FROM traffic_event_dlq`)
	if err != nil {
		return 0, fmt.Errorf("scan dlq for erasure: %w", err)
	}
	var doomed []string
	for rows.Next() {
		var id string
		var payload []byte
		if err := rows.Scan(&id, &payload); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan dlq row: %w", err)
		}
		var env dlqEnvelope
		if err := json.Unmarshal(payload, &env); err != nil {
			// A payload that did not decode cannot be shown to belong to
			// anyone. This is NOT a formality: goccy/go-json fills every field
			// it read before returning the error, so a truncated event arrives
			// with source and entityId already populated and will compare
			// EQUAL to the subject. Acting on that is deleting a row on the
			// strength of a read that failed. Left in place; the retention
			// sweep still ages it out.
			continue
		}
		if dlqRowBelongsTo(env, subjectID, windows) {
			doomed = append(doomed, id)
		}
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate dlq rows: %w", err)
	}
	rows.Close()
	if len(doomed) == 0 {
		return 0, nil
	}

	tag, err := tx.Exec(ctx, `DELETE FROM traffic_event_dlq WHERE id = ANY($1)`, doomed)
	if err != nil {
		return 0, fmt.Errorf("delete subject dlq rows: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// dlqRowBelongsTo mirrors the two live-table predicates: the gateway leg keys
// on entity_id, the agent leg on a device the subject held AT THE TIME of the
// event. A device reassigned later must not drag the next holder's traffic
// into this subject's erasure, which is the same reason the agent SQL above
// carries its assignment-window bounds.
func dlqRowBelongsTo(env dlqEnvelope, subjectID string, windows []deviceWindow) bool {
	switch env.Source {
	case "ai-gateway":
		return env.EntityID != nil && *env.EntityID == subjectID
	case "agent":
		if env.ThingID == nil {
			return false
		}
		for _, w := range windows {
			if w.deviceID != *env.ThingID || env.Timestamp.Before(w.assignedAt) {
				continue
			}
			if w.releasedAt == nil || env.Timestamp.Before(*w.releasedAt) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// subjectDeviceWindows reads every interval in which a device belonged to the
// subject.
func subjectDeviceWindows(ctx context.Context, tx pgx.Tx, subjectID string) ([]deviceWindow, error) {
	rows, err := tx.Query(ctx, `
		SELECT da."deviceId", da."assignedAt", da."releasedAt"
		FROM "DeviceAssignment" da
		WHERE da."userId" = $1
	`, subjectID)
	if err != nil {
		return nil, fmt.Errorf("read device assignments: %w", err)
	}
	defer rows.Close()
	var out []deviceWindow
	for rows.Next() {
		var w deviceWindow
		if err := rows.Scan(&w.deviceID, &w.assignedAt, &w.releasedAt); err != nil {
			return nil, fmt.Errorf("scan device assignment: %w", err)
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate device assignments: %w", err)
	}
	return out, nil
}
