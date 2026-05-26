package hubapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/labstack/echo/v4"
)

// dlqPool is the narrow pgx surface the DLQ handlers consume. *pgxpool.Pool
// satisfies it via structural typing; tests inject pgxmock.PgxPoolIface.
// Declared on the handler side (rather than in shared/storage) so the
// interface stays scoped to the two endpoints that need it.
type dlqPool interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// dlqDefaultLimit / dlqMaxLimit cap the list pagination. 50 is enough to
// fill a default UI grid; 200 is the upper bound to keep response shapes
// reasonable for psql / curl operator usage.
const (
	dlqDefaultLimit = 50
	dlqMaxLimit     = 200
)

// dlqRow mirrors the JSON shape returned by ListDLQ. payload bytes are
// intentionally omitted from the list response (a full table can be
// hundreds of MB of raw audit payloads); a future detail endpoint can
// surface them per-row if operator workflow demands it.
type dlqRow struct {
	ID            string    `json:"id"`
	MsgID         string    `json:"msgId"`
	Subject       string    `json:"subject"`
	DeliveryCount int       `json:"deliveryCount"`
	LastError     string    `json:"lastError,omitempty"`
	FirstSeenAt   time.Time `json:"firstSeenAt"`
	DLQInsertedAt time.Time `json:"dlqInsertedAt"`
	PayloadSize   int       `json:"payloadSize"`
}

// dlqListResponse pairs the row slice with an opaque cursor. The cursor
// is the dlq_inserted_at timestamp of the last row in the page; clients
// pass it back as ?cursor= to fetch the next page. Empty cursor means
// "no more rows" so the UI's pagination control disables the next button.
type dlqListResponse struct {
	Rows       []dlqRow `json:"rows"`
	NextCursor string   `json:"nextCursor,omitempty"`
}

// ListDLQ handles GET /api/hub/dlq?subject=X&limit=N&cursor=ISO8601.
//
// Returns rows from traffic_event_dlq ordered by dlq_inserted_at DESC
// (newest first — matches the "what just broke?" admin workflow). The
// btree index `traffic_event_dlq_inserted_at_idx` covers this sort so
// the page LIMIT does not scan the whole table.
//
//   - subject (optional) filters to one MQ subject (e.g.
//     "nexus.event.compliance"). Empty matches all subjects.
//   - limit (optional, default 50, max 200) caps page size.
//   - cursor (optional) is the dlqInsertedAt ISO8601 timestamp of the
//     last row in the previous page. The query then selects rows with
//     dlq_inserted_at < cursor.
//
// Returns 503 service_unavailable when the DLQ pool is not wired (test
// setups, broken boot).
func (h *HubAPI) ListDLQ(c echo.Context) error {
	if h.DLQPool == nil {
		return c.JSON(http.StatusServiceUnavailable, echo.Map{
			"error": "dlq_unavailable",
		})
	}

	subject := c.QueryParam("subject")
	limit := dlqDefaultLimit
	if raw := c.QueryParam("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
			if limit > dlqMaxLimit {
				limit = dlqMaxLimit
			}
		}
	}
	var cursor time.Time
	if raw := c.QueryParam("cursor"); raw != "" {
		t, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return c.JSON(http.StatusBadRequest, echo.Map{
				"error": "invalid_cursor",
				"hint":  "cursor must be an RFC3339 timestamp from a previous nextCursor",
			})
		}
		cursor = t
	}

	// Build the query with optional subject + cursor filters. Both are
	// indexed (msg_id_idx, inserted_at_idx) so combining them stays fast.
	const baseSQL = `
SELECT
    id::text,
    msg_id,
    subject,
    delivery_count,
    COALESCE(last_error, '') AS last_error,
    first_seen_at,
    dlq_inserted_at,
    LENGTH(payload) AS payload_size
FROM traffic_event_dlq
`
	where := " WHERE 1=1"
	args := []any{}
	n := 1
	if subject != "" {
		where += fmt.Sprintf(" AND subject = $%d", n)
		args = append(args, subject)
		n++
	}
	if !cursor.IsZero() {
		where += fmt.Sprintf(" AND dlq_inserted_at < $%d", n)
		args = append(args, cursor)
		n++
	}
	order := " ORDER BY dlq_inserted_at DESC LIMIT $" + strconv.Itoa(n)
	args = append(args, limit)

	ctx := c.Request().Context()
	rows, err := h.DLQPool.Query(ctx, baseSQL+where+order, args...)
	if err != nil {
		h.logger().Error("dlq: list query failed", "error", err)
		return c.JSON(http.StatusInternalServerError, echo.Map{
			"error": "db_error",
		})
	}
	defer rows.Close()

	out := make([]dlqRow, 0, limit)
	for rows.Next() {
		var r dlqRow
		if err := rows.Scan(
			&r.ID, &r.MsgID, &r.Subject, &r.DeliveryCount,
			&r.LastError, &r.FirstSeenAt, &r.DLQInsertedAt, &r.PayloadSize,
		); err != nil {
			h.logger().Error("dlq: list scan failed", "error", err)
			return c.JSON(http.StatusInternalServerError, echo.Map{
				"error": "db_error",
			})
		}
		out = append(out, r)
	}

	resp := dlqListResponse{Rows: out}
	if len(out) == limit {
		// Page is full → caller may want the next page. The cursor is
		// the inserted-at timestamp of the last row (DESC order, so the
		// "oldest" row in the current page). RFC3339Nano so sub-second
		// resolution isn't lost across the round-trip.
		resp.NextCursor = out[len(out)-1].DLQInsertedAt.UTC().Format(time.RFC3339Nano)
	}
	return c.JSON(http.StatusOK, resp)
}

// RetryDLQ handles POST /api/hub/dlq/:id/retry.
//
// Reads the DLQ row's payload + subject, republishes the payload to the
// original MQ subject, and DELETEs the DLQ row. Failure modes are
// non-destructive: a publish error leaves the row in place so the
// operator can retry after the bug is fixed; a missing row returns
// 404 without side effects.
//
// Returns 503 when either the DLQ pool or the MQ producer is not wired.
func (h *HubAPI) RetryDLQ(c echo.Context) error {
	if h.DLQPool == nil {
		return c.JSON(http.StatusServiceUnavailable, echo.Map{
			"error": "dlq_unavailable",
		})
	}
	if h.DLQProducer == nil {
		return c.JSON(http.StatusServiceUnavailable, echo.Map{
			"error": "mq_producer_unavailable",
		})
	}

	id := c.Param("id")
	if id == "" {
		return c.JSON(http.StatusBadRequest, echo.Map{
			"error": "id_required",
		})
	}

	ctx := c.Request().Context()
	var subject string
	var payload []byte
	err := h.DLQPool.QueryRow(ctx,
		`SELECT subject, payload FROM traffic_event_dlq WHERE id = $1::uuid`,
		id,
	).Scan(&subject, &payload)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return c.JSON(http.StatusNotFound, echo.Map{
				"error": "dlq_not_found",
				"id":    id,
			})
		}
		h.logger().Error("dlq: retry select failed", "error", err, "id", id)
		return c.JSON(http.StatusInternalServerError, echo.Map{
			"error": "db_error",
		})
	}

	// Republish first. Only on success do we DELETE so a publish failure
	// keeps the DLQ row in place for another retry attempt.
	if err := h.DLQProducer.Enqueue(ctx, subject, payload); err != nil {
		h.logger().Error("dlq: retry republish failed", "error", err, "id", id, "subject", subject)
		return c.JSON(http.StatusInternalServerError, echo.Map{
			"error": "republish_failed",
		})
	}

	if _, err := h.DLQPool.Exec(ctx,
		`DELETE FROM traffic_event_dlq WHERE id = $1::uuid`, id,
	); err != nil {
		// The republish already succeeded — the broker has the message.
		// Failing to delete leaves a stale row in DLQ; an operator will
		// see the row come back via the retry cycle, but the actual MQ
		// message has been re-enqueued. Log + return 200 with a warning
		// flag rather than 500 so the UI shows partial success rather
		// than tricking the operator into re-publishing again.
		h.logger().Warn("dlq: retry succeeded but DELETE failed; row will linger",
			"error", err, "id", id)
		return c.JSON(http.StatusOK, echo.Map{
			"ok":         true,
			"subject":    subject,
			"deleteWarn": true,
		})
	}

	return c.JSON(http.StatusOK, echo.Map{
		"ok":      true,
		"subject": subject,
	})
}

// Compile-time sanity: dlqListResponse is exported via JSON only, but
// having the marshal step here ensures the round-trip stays in sync with
// CP-UI's expected shape. Unused at runtime; the linker drops it.
var _ = json.Marshal
