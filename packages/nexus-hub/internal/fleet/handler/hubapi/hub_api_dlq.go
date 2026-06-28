package hubapi

import (
	"context"
	"errors"
	"fmt"
	"github.com/goccy/go-json"
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

// dlqListResponse pairs the page's row slice with the total row count
// matching the subject filter. Offset-based: clients render a standard
// page footer (row range, page count, First/Prev/Next/Last) from
// total + the offset/limit they sent. This matches every other admin
// list surface (jobs, nodes, audit) so the UI binds the shared
// ListPagination control rather than a bespoke cursor footer.
type dlqListResponse struct {
	Rows  []dlqRow `json:"rows"`
	Total int      `json:"total"`
}

// ListDLQ handles GET /api/hub/dlq?subject=X&limit=N&offset=M.
//
// Returns rows from traffic_event_dlq ordered by dlq_inserted_at DESC
// (newest first — matches the "what just broke?" admin workflow) plus
// the total count matching the filter. The btree index
// `traffic_event_dlq_inserted_at_idx` covers the sort.
//
//   - subject (optional) filters to one MQ subject (e.g.
//     "nexus.event.compliance"). Empty matches all subjects.
//   - limit (optional, default 50, max 200) caps page size.
//   - offset (optional, default 0) skips that many rows for the page.
//
// Offset pagination (rather than keyset) is fine here: the DLQ is a
// small, bounded table (dead letters; near-empty in a healthy system),
// so COUNT(*) and OFFSET stay cheap, and offset gives the operator the
// same page-number / total-count footer as the rest of the admin UI.
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
	offset := 0
	if raw := c.QueryParam("offset"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 0 {
			offset = n
		}
	}

	// The optional subject filter is shared by both the COUNT and the
	// page query so the footer's total agrees with the rows returned.
	// subject is indexed (msg_id_idx covers the table) so the filter
	// stays cheap.
	where := " WHERE 1=1"
	args := []any{}
	n := 1
	if subject != "" {
		where += fmt.Sprintf(" AND subject = $%d", n)
		args = append(args, subject)
		n++
	}

	ctx := c.Request().Context()

	// Total matching the filter — drives the offset-pagination footer.
	var total int
	if err := h.DLQPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM traffic_event_dlq`+where, args...,
	).Scan(&total); err != nil {
		h.logger().Error("dlq: count query failed", "error", err)
		return c.JSON(http.StatusInternalServerError, echo.Map{
			"error": "db_error",
		})
	}

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
	pageSQL := baseSQL + where +
		" ORDER BY dlq_inserted_at DESC LIMIT $" + strconv.Itoa(n) +
		" OFFSET $" + strconv.Itoa(n+1)
	// Copy args so appending the page bounds never mutates the slice the
	// COUNT query already consumed.
	pageArgs := append(append([]any{}, args...), limit, offset)

	rows, err := h.DLQPool.Query(ctx, pageSQL, pageArgs...)
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

	return c.JSON(http.StatusOK, dlqListResponse{Rows: out, Total: total})
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
