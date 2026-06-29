// Package queue provides the SQLite-backed audit event Queue with batch drain,
// config snapshots, local audit, lifecycle events, and rollup tables.
package queue

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"github.com/goccy/go-json"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	_ "github.com/mutecomm/go-sqlcipher/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/audit/event"
	sharedaudit "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/enums"
)

// BumpStatus values used in audit events to describe TLS bump outcomes.
// These re-export shared configtypes constants for consistency across services.
const (
	BumpStatusSuccess     = string(enums.BumpStatusSuccess)
	BumpStatusFailed      = string(enums.BumpStatusFailedPassthrough)
	BumpStatusAutoExempt  = string(enums.BumpStatusExemptPinned)
	BumpStatusAdminExempt = string(enums.BumpStatusExemptConfigured)
)

// Queue is an SQLite-backed audit event queue. Individual SQL statements are
// goroutine-safe via *sql.DB's connection pool. The mutex serializes only
// multi-statement transactions (MarkSynced, RecordLocal) to prevent
// interleaving within a transaction.
type Queue struct {
	db *sql.DB
	mu sync.Mutex
}

// The following package-level variables exist solely as test seams so unit
// tests can exercise the post-export mid-swap error arms in
// migrateToEncrypted (install-rename failure with restore, attach/export/detach
// failure cleanup). Production never reassigns them. Mirrors the established
// pattern in packages/agent/internal/identity/secretstore/fallback.go (osFile +
// createTempFn + renameFn) and packages/agent/internal/identity/enrollment/enroll.go.
var (
	renameFn = os.Rename
	removeFn = os.Remove
)

// NewQueue opens (or creates) the SQLite audit database.
// If encryptionKey is non-nil (32 bytes), SQLCipher encryption is enabled.
// An existing unencrypted database is automatically migrated to encrypted.
func NewQueue(dbPath string, encryptionKey []byte) (*Queue, error) {
	// SQLCipher requires the key to be applied on every connection the
	// driver opens. Go's database/sql pool can open multiple connections
	// on demand, so running `PRAGMA key` once on the *sql.DB is unreliable.
	// The go-sqlcipher driver supports `_pragma_key=x'<hex>'` in the DSN —
	// it then issues the PRAGMA on each new connection automatically.
	buildDSN := func() string {
		if dbPath == ":memory:" {
			return "file::memory:?cache=shared"
		}
		dsn := dbPath + "?_journal_mode=WAL&_busy_timeout=5000"
		if len(encryptionKey) > 0 {
			dsn += fmt.Sprintf("&_pragma_key=x'%s'", hex.EncodeToString(encryptionKey))
		}
		return dsn
	}

	dsn := buildDSN()
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open audit db: %w", err)
	}

	// Verify the key works by reading sqlite_master. If this fails, the
	// file may be an unencrypted legacy database — migrate it.
	if len(encryptionKey) > 0 {
		if err := testDBAccess(db); err != nil && dbPath != ":memory:" {
			_ = db.Close()
			slog.Info("migrating unencrypted audit database to encrypted", "path", dbPath)
			if err := migrateToEncrypted(dbPath, encryptionKey); err != nil {
				return nil, fmt.Errorf("migrate audit db to encrypted: %w", err)
			}
			// Reopen with the same keyed DSN.
			db, err = sql.Open("sqlite3", dsn)
			if err != nil {
				return nil, fmt.Errorf("reopen encrypted audit db: %w", err)
			}
			if err := testDBAccess(db); err != nil {
				_ = db.Close()
				return nil, fmt.Errorf("read encrypted audit db after migration: %w", err)
			}
		}
	}

	// Pre-GA + greenfield (CLAUDE.md "no data-migration code for dev-phase
	// records") — the agent's local SQLite queue is rebuilt from scratch on
	// every install; old "tolerant ALTER TABLE ADD COLUMN" chains were a
	// migration tax we no longer pay. The full schema lives in one CREATE
	// TABLE; new columns ship by editing this literal, not by appending a
	// new ALTER line. Existing dev DBs that predate a column will pick it
	// up via fresh install — agents are local-only state and we recreate
	// the SQLite file when needed.
	if _, err := db.ExecContext(context.Background(), `
		CREATE TABLE IF NOT EXISTS audit_events (
			id TEXT PRIMARY KEY,
			timestamp TEXT NOT NULL,
			source_process TEXT NOT NULL,
			source_user TEXT,
			dest_host TEXT NOT NULL,
			dest_ip TEXT NOT NULL,
			dest_port INTEGER NOT NULL,
			-- HTTP method + path captured by tlsbump's bumped path.
			-- Empty for non-bumped flows (passthrough opaque relay, SSH,
			-- non-TLS ports).
			method TEXT,
			path TEXT,
			-- HTTP response status code from the upstream, captured by
			-- tlsbump's bumped response. 0 / NULL for passthrough flows
			-- (no TLS decrypt → no HTTP layer visible) and for inspect
			-- flows where the upstream RTT didn't complete.
			status_code INTEGER,
			action TEXT NOT NULL,
			policy_rule_id TEXT,
			bump_status TEXT,
			bytes_in INTEGER,
			bytes_out INTEGER,
			duration_ms INTEGER,
			hook_decision TEXT,
			hook_reason TEXT,
			hook_reason_code TEXT,
			compliance_tags TEXT,
			-- #70: cross-service correlation id. Populated from
			-- audit.AuditEvent.TraceID (X-Nexus-Request-Id header or
			-- fallback txID per forward_handler). Without this column
			-- agent.audit_events trace_id stayed empty and the wire
			-- envelope shipped traceId='' → cp-ui Detail showed empty.
			trace_id TEXT,
			provider_name TEXT,
			model_name TEXT,
			api_key_class TEXT,
			api_key_fingerprint TEXT,
			-- Domain-matched adapter id (interception_domain.adapter_id). The
			-- detail drawer's view-time normalize recompute keys on this as the
			-- authoritative adapter; NULL/empty falls back to path + content
			-- sniff. Uploaded to Hub → traffic_event.ingress_format.
			ingress_format TEXT,
			prompt_tokens INTEGER,
			completion_tokens INTEGER,
			usage_extraction_status TEXT,
			payload_request BLOB,
			payload_response BLOB,
			-- Out-of-band spill refs (JSON-encoded audit.SpillRef) for bodies
			-- that exceed the inline cap and were uploaded to S3 via the Hub
			-- presign flow (or kept on local disk when offline). NULL when the
			-- body travelled inline in payload_request/payload_response.
			request_spill_ref TEXT,
			response_spill_ref TEXT,
			-- Pre-normalized payload JSON, computed by
			-- forward_handler's runtimeNormalize and forwarded through
			-- the audit emit chain. NULL when no AI adapter matched
			-- (non-LLM traffic, non-bumped flow).
			normalized_request TEXT,
			normalized_response TEXT,
			-- Redaction spans relocated to the storage-redacted normalized
			-- payloads above (JSON array of TransformSpan). NULL for
			-- unredacted rows.
			request_redaction_spans TEXT,
			response_redaction_spans TEXT,
			-- Latency phase breakdown. All nullable.
			upstream_ttfb_ms INTEGER,
			upstream_total_ms INTEGER,
			request_hooks_ms INTEGER,
			response_hooks_ms INTEGER,
			latency_breakdown TEXT,
			-- hooks_pipeline JSON is retained so the desktop UI Traffic
			-- Detail drawer can render per-hook latency without a second
			-- Hub round-trip.
			hooks_pipeline TEXT,
			synced INTEGER DEFAULT 0,
			created_at TEXT DEFAULT (datetime('now'))
		);
		CREATE INDEX IF NOT EXISTS idx_audit_synced_created ON audit_events(synced, created_at);

		-- Local-only audit log (full metadata, not uploaded).
		CREATE TABLE IF NOT EXISTS audit_local (
			id TEXT PRIMARY KEY,
			timestamp TEXT NOT NULL,
			dest_host TEXT NOT NULL,
			dest_ip TEXT,
			dest_port INTEGER,
			action TEXT NOT NULL,
			hook_decision TEXT,
			hook_reason TEXT,
			source_process TEXT,
			source_user TEXT,
			bytes_in INTEGER,
			bytes_out INTEGER,
			created_at TEXT DEFAULT (datetime('now'))
		);
		CREATE INDEX IF NOT EXISTS idx_audit_local_created ON audit_local(created_at);

		-- Per-Thing rollup, agent-local mirror.
		-- Powers the agent native UI stats view without server round-trip; agent
		-- is the only Thing in scope so no thing_id column. value stays REAL
		-- (vs DECIMAL on Postgres) — SQLite has no fixed-point type and the
		-- aggregated sums tolerate float drift at single-agent volume.
		-- metadata is JSON-as-TEXT (Histogram / TimestampMeta payloads).
		CREATE TABLE IF NOT EXISTS thing_metric_rollup_local_5m (
			bucket_start TEXT NOT NULL,
			metric_name TEXT NOT NULL,
			dimension_key TEXT NOT NULL DEFAULT '',
			sub_dimension TEXT NOT NULL DEFAULT '',
			value REAL NOT NULL DEFAULT 0,
			metadata TEXT,
			updated_at TEXT NOT NULL DEFAULT (datetime('now')),
			PRIMARY KEY (bucket_start, metric_name, dimension_key, sub_dimension)
		);
		CREATE INDEX IF NOT EXISTS idx_thing_rollup_local_5m_bucket ON thing_metric_rollup_local_5m(bucket_start);
		CREATE INDEX IF NOT EXISTS idx_thing_rollup_local_5m_metric ON thing_metric_rollup_local_5m(metric_name, bucket_start);

		CREATE TABLE IF NOT EXISTS thing_metric_rollup_local_1h (
			bucket_start TEXT NOT NULL,
			metric_name TEXT NOT NULL,
			dimension_key TEXT NOT NULL DEFAULT '',
			sub_dimension TEXT NOT NULL DEFAULT '',
			value REAL NOT NULL DEFAULT 0,
			metadata TEXT,
			updated_at TEXT NOT NULL DEFAULT (datetime('now')),
			PRIMARY KEY (bucket_start, metric_name, dimension_key, sub_dimension)
		);
		CREATE INDEX IF NOT EXISTS idx_thing_rollup_local_1h_bucket ON thing_metric_rollup_local_1h(bucket_start);
		CREATE INDEX IF NOT EXISTS idx_thing_rollup_local_1h_metric ON thing_metric_rollup_local_1h(metric_name, bucket_start);

		CREATE TABLE IF NOT EXISTS thing_metric_rollup_local_1d (
			bucket_start TEXT NOT NULL,
			metric_name TEXT NOT NULL,
			dimension_key TEXT NOT NULL DEFAULT '',
			sub_dimension TEXT NOT NULL DEFAULT '',
			value REAL NOT NULL DEFAULT 0,
			metadata TEXT,
			updated_at TEXT NOT NULL DEFAULT (datetime('now')),
			PRIMARY KEY (bucket_start, metric_name, dimension_key, sub_dimension)
		);
		CREATE INDEX IF NOT EXISTS idx_thing_rollup_local_1d_bucket ON thing_metric_rollup_local_1d(bucket_start);
		CREATE INDEX IF NOT EXISTS idx_thing_rollup_local_1d_metric ON thing_metric_rollup_local_1d(metric_name, bucket_start);

		CREATE TABLE IF NOT EXISTS thing_metric_rollup_local_1mo (
			bucket_start TEXT NOT NULL,
			metric_name TEXT NOT NULL,
			dimension_key TEXT NOT NULL DEFAULT '',
			sub_dimension TEXT NOT NULL DEFAULT '',
			value REAL NOT NULL DEFAULT 0,
			metadata TEXT,
			updated_at TEXT NOT NULL DEFAULT (datetime('now')),
			PRIMARY KEY (bucket_start, metric_name, dimension_key, sub_dimension)
		);
		CREATE INDEX IF NOT EXISTS idx_thing_rollup_local_1mo_bucket ON thing_metric_rollup_local_1mo(bucket_start);
		CREATE INDEX IF NOT EXISTS idx_thing_rollup_local_1mo_metric ON thing_metric_rollup_local_1mo(metric_name, bucket_start);

		CREATE TABLE IF NOT EXISTS rollup_watermark_local (
			job_name TEXT PRIMARY KEY,
			watermark TEXT NOT NULL,
			updated_at TEXT NOT NULL DEFAULT (datetime('now'))
		);

		-- lifecycle_event: user-visible local mirror of agent lifecycle
		-- DiagEvents (startup / shutdown / paused / resumed / sso_login /
		-- sso_logout). The Hub copy is the authoritative store; this
		-- table exists ONLY so the agent's own Dashboard "Activity"
		-- page can render a usable timeline without round-tripping
		-- through Hub. Writes are decoupled from Hub upload: the
		-- lifecycle Emitter calls Insert here AFTER pushing the same
		-- event via thingclient WS, so a Hub outage doesn't lose the
		-- user-visible record. occurred_at carries the original emit
		-- time (vs received_at on the Hub side) so the timeline
		-- matches what the user actually did. attrs is JSON-as-TEXT
		-- to keep the schema flat. No TTL today (lifecycle volume is
		-- ~10 rows / day / device; a year fits in <10k rows). Add
		-- prune when volume materially grows.
		CREATE TABLE IF NOT EXISTS lifecycle_event (
			id TEXT PRIMARY KEY,
			occurred_at TEXT NOT NULL,
			action TEXT NOT NULL,
			message TEXT,
			level TEXT NOT NULL DEFAULT 'info',
			attrs TEXT,
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		);
		CREATE INDEX IF NOT EXISTS idx_lifecycle_event_occurred_at ON lifecycle_event(occurred_at DESC);
	`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create audit tables: %w", err)
	}

	// Additive migration for installs that predate certain columns.
	// CREATE TABLE IF NOT EXISTS above does NOT add columns to an existing
	// table, so we must ALTER explicitly. SQLite returns "duplicate column
	// name" if the column already exists; treat as ok. Existing rows get
	// NULL for the new columns; new rows populate them.
	for _, alter := range []string{
		`ALTER TABLE audit_events ADD COLUMN method TEXT`,
		`ALTER TABLE audit_events ADD COLUMN path TEXT`,
		// Classification inputs: domain_rule_id is the matched
		// interception_domain.id; path_action is the resolved
		// PROCESS|PASSTHROUGH|BLOCK action. Together they let classify()
		// distinguish Untracked / Inspect / Processed / Blocked at query
		// time so the agent UI labels match the admin's domain config.
		`ALTER TABLE audit_events ADD COLUMN domain_rule_id TEXT`,
		`ALTER TABLE audit_events ADD COLUMN path_action TEXT`,
		// status_code was missing from older DBs; new CREATE TABLE already
		// includes it. ALTER is idempotent — duplicate column errors are
		// swallowed below.
		`ALTER TABLE audit_events ADD COLUMN status_code INTEGER`,
		// Normalized payload JSON produced by runtimeNormalize.
		`ALTER TABLE audit_events ADD COLUMN normalized_request TEXT`,
		`ALTER TABLE audit_events ADD COLUMN normalized_response TEXT`,
		// Cross-service correlation id threaded from agent → ai-gateway.
		`ALTER TABLE audit_events ADD COLUMN trace_id TEXT`,
		// Out-of-band spill refs (JSON audit.SpillRef) for oversize bodies.
		`ALTER TABLE audit_events ADD COLUMN request_spill_ref TEXT`,
		`ALTER TABLE audit_events ADD COLUMN response_spill_ref TEXT`,
		// Redaction spans for the storage-redacted normalized payloads.
		`ALTER TABLE audit_events ADD COLUMN request_redaction_spans TEXT`,
		`ALTER TABLE audit_events ADD COLUMN response_redaction_spans TEXT`,
	} {
		if _, err := db.ExecContext(context.Background(), alter); err != nil {
			msg := err.Error()
			if !strings.Contains(msg, "duplicate column") {
				slog.Warn("audit schema migration failed (non-fatal)",
					"alter", alter, "error", err)
			}
		}
	}

	return &Queue{db: db}, nil
}

// testDBAccess verifies that the database is readable (correct key or unencrypted).
func testDBAccess(db *sql.DB) error {
	_, err := db.ExecContext(context.Background(), "SELECT count(*) FROM sqlite_master")
	return err
}

// migrateToEncrypted converts an unencrypted SQLite database to SQLCipher.
func migrateToEncrypted(dbPath string, encryptionKey []byte) error {
	// Open the unencrypted database (no PRAGMA key)
	plainDB, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return fmt.Errorf("open plain db: %w", err)
	}
	defer plainDB.Close() //nolint:errcheck

	// Verify it's actually accessible without a key
	if err := testDBAccess(plainDB); err != nil {
		return fmt.Errorf("plain db not accessible: %w", err)
	}

	encPath := dbPath + ".encrypted"
	hexKey := hex.EncodeToString(encryptionKey)
	attachSQL := fmt.Sprintf("ATTACH DATABASE '%s' AS encrypted KEY \"x'%s'\"", encPath, hexKey)
	if _, err := plainDB.ExecContext(context.Background(), attachSQL); err != nil {
		return fmt.Errorf("attach encrypted db: %w", err)
	}

	// Export schema + data
	if _, err := plainDB.ExecContext(context.Background(), "SELECT sqlcipher_export('encrypted')"); err != nil {
		_ = removeFn(encPath)
		return fmt.Errorf("sqlcipher_export: %w", err)
	}

	if _, err := plainDB.ExecContext(context.Background(), "DETACH DATABASE encrypted"); err != nil {
		_ = removeFn(encPath)
		return fmt.Errorf("detach: %w", err)
	}
	_ = plainDB.Close()

	// Swap files: old → .backup, encrypted → original
	backupPath := dbPath + ".unencrypted.backup"
	if err := renameFn(dbPath, backupPath); err != nil {
		_ = removeFn(encPath)
		return fmt.Errorf("backup plain db: %w", err)
	}
	if err := renameFn(encPath, dbPath); err != nil {
		// Restore backup on failure
		_ = renameFn(backupPath, dbPath)
		return fmt.Errorf("install encrypted db: %w", err)
	}

	// Critical: remove the WAL/SHM journals from the plain DB. They were
	// generated against the now-renamed-aside plain DB; if left next to
	// the freshly installed encrypted DB, sqlite/sqlcipher will try to
	// replay them on top of the encrypted file and fail with
	// "file is not a database" on the next open.
	_ = removeFn(dbPath + "-wal")
	_ = removeFn(dbPath + "-shm")

	slog.Info("audit database migration complete", "backup", backupPath)
	return nil
}

// ComputeTodayStats reads audit_events rows from the local midnight
// (system tz) and returns a (Inspected, Passthrough, Denied, AvgUsMs,
// AvgUpstreamMs) tuple. Called by the agent status collector on every
// status poll; lightweight against an indexed timestamp scan.
//
// Counts are bucketed by `action`: inspect → Inspected, passthrough →
// Passthrough, deny → Denied. Phase averages are computed from rows
// with non-NULL upstream_total_ms; "our overhead" uses
// max(0, duration_ms - upstream_total_ms).
//
// Returns zero-value ints + nil pointers when the table has no rows
// since midnight (fresh install / first run).
func (q *Queue) ComputeTodayStats() (inspected, passthrough, denied int, avgUsMs, avgUpstreamMs *int) {
	if q == nil || q.db == nil {
		return 0, 0, 0, nil, nil
	}
	row := q.db.QueryRowContext(context.Background(), `
		SELECT
			COUNT(*) FILTER (WHERE action = 'inspect')      AS inspected,
			COUNT(*) FILTER (WHERE action = 'passthrough')  AS passthrough,
			COUNT(*) FILTER (WHERE action = 'deny')         AS denied,
			AVG(CASE WHEN upstream_total_ms IS NOT NULL AND duration_ms IS NOT NULL
			         THEN MAX(0, duration_ms - upstream_total_ms) END) AS avg_us,
			AVG(CASE WHEN upstream_total_ms IS NOT NULL THEN upstream_total_ms END) AS avg_upstream
		FROM audit_events
		WHERE timestamp >= date('now','start of day')`)
	var avgUs, avgUp sql.NullFloat64
	if err := row.Scan(&inspected, &passthrough, &denied, &avgUs, &avgUp); err != nil {
		return 0, 0, 0, nil, nil
	}
	if avgUs.Valid {
		v := int(avgUs.Float64)
		avgUsMs = &v
	}
	if avgUp.Valid {
		v := int(avgUp.Float64)
		avgUpstreamMs = &v
	}
	return inspected, passthrough, denied, avgUsMs, avgUpstreamMs
}

// Record inserts an audit event (synced=false).
func (q *Queue) Record(e event.Event) error {
	_, err := q.db.ExecContext(context.Background(), `
		INSERT OR IGNORE INTO audit_events
		(id, timestamp, source_process, source_user, dest_host, dest_ip, dest_port,
		 method, path, status_code,
		 action, policy_rule_id, bump_status, bytes_in, bytes_out, duration_ms,
		 hook_decision, hook_reason, hook_reason_code, compliance_tags,
		 provider_name, model_name, api_key_class, api_key_fingerprint,
		 ingress_format,
		 prompt_tokens, completion_tokens, usage_extraction_status,
		 payload_request, payload_response,
		 request_spill_ref, response_spill_ref,
		 normalized_request, normalized_response,
		 request_redaction_spans, response_redaction_spans,
		 upstream_ttfb_ms, upstream_total_ms, request_hooks_ms, response_hooks_ms,
		 latency_breakdown, hooks_pipeline,
		 domain_rule_id, path_action,
		 trace_id,
		 synced)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0)`,
		e.ID, e.Timestamp.UTC().Format(time.RFC3339Nano),
		e.SourceProcess, e.OSUser, e.TargetHost, e.DestIP, e.DestPort,
		nullableString(e.Method), nullableString(e.Path), nullableStatusCode(e.StatusCode),
		e.Action, e.PolicyRuleID, e.BumpStatus, e.BytesIn, e.BytesOut, e.LatencyMs,
		e.HookDecision, e.HookReason, e.HookReasonCode, encodeTags(e.ComplianceTags),
		e.ProviderName, e.ModelName, e.ApiKeyClass, e.ApiKeyFingerprint,
		nullableString(e.IngressFormat),
		nullableInt(e.PromptTokens), nullableInt(e.CompletionTokens), e.UsageExtractionStatus,
		nullableBytes(e.PayloadRequest), nullableBytes(e.PayloadResponse),
		nullableSpillRefJSON(e.RequestSpillRef), nullableSpillRefJSON(e.ResponseSpillRef),
		nullableJSONString(e.NormalizedRequest), nullableJSONString(e.NormalizedResponse),
		nullableJSONString(e.RequestRedactionSpans), nullableJSONString(e.ResponseRedactionSpans),
		nullableInt(e.UpstreamTtfbMs), nullableInt(e.UpstreamTotalMs),
		nullableInt(e.RequestHooksMs), nullableInt(e.ResponseHooksMs),
		encodeBreakdown(e.LatencyBreakdown), nullableJSONString(e.HooksPipeline),
		nullableString(e.DomainRuleID), nullableString(e.PathAction),
		// Cross-service correlation id.
		nullableString(e.TraceID),
	)
	return err
}

// RecordBatch inserts a batch of audit events in a single SQLite
// transaction. Used by the async QueueWriter flush loop so N events
// share one fsync instead of N — the hot path enqueues events into a
// channel and a background goroutine batches them off the inspect
// goroutine's critical path.
//
// Empty input returns nil (no-op). All-or-nothing semantics: a single
// row failure rolls back the whole batch; the caller logs + retries.
// INSERT OR IGNORE makes duplicate IDs a silent no-op (same as Record),
// so a partial retry after a crash is idempotent.
func (q *Queue) RecordBatch(events []event.Event) error {
	if len(events) == 0 {
		return nil
	}
	// Single per-row Exec path inside one tx — no separate Prepare
	// branch (saves a coverage seam). sqlite's WAL-mode planner caches
	// prepared statements internally so the per-row cost is the same.
	ctx := context.Background()
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin batch tx: %w", err)
	}
	const insertSQL = `
		INSERT OR IGNORE INTO audit_events
		(id, timestamp, source_process, source_user, dest_host, dest_ip, dest_port,
		 method, path, status_code,
		 action, policy_rule_id, bump_status, bytes_in, bytes_out, duration_ms,
		 hook_decision, hook_reason, hook_reason_code, compliance_tags,
		 provider_name, model_name, api_key_class, api_key_fingerprint,
		 ingress_format,
		 prompt_tokens, completion_tokens, usage_extraction_status,
		 payload_request, payload_response,
		 request_spill_ref, response_spill_ref,
		 normalized_request, normalized_response,
		 request_redaction_spans, response_redaction_spans,
		 upstream_ttfb_ms, upstream_total_ms, request_hooks_ms, response_hooks_ms,
		 latency_breakdown, hooks_pipeline,
		 domain_rule_id, path_action,
		 trace_id,
		 synced)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0)`
	for _, e := range events {
		if _, err := tx.ExecContext(ctx, insertSQL,
			e.ID, e.Timestamp.UTC().Format(time.RFC3339Nano),
			e.SourceProcess, e.OSUser, e.TargetHost, e.DestIP, e.DestPort,
			nullableString(e.Method), nullableString(e.Path), nullableStatusCode(e.StatusCode),
			e.Action, e.PolicyRuleID, e.BumpStatus, e.BytesIn, e.BytesOut, e.LatencyMs,
			e.HookDecision, e.HookReason, e.HookReasonCode, encodeTags(e.ComplianceTags),
			e.ProviderName, e.ModelName, e.ApiKeyClass, e.ApiKeyFingerprint,
			nullableString(e.IngressFormat),
			nullableInt(e.PromptTokens), nullableInt(e.CompletionTokens), e.UsageExtractionStatus,
			nullableBytes(e.PayloadRequest), nullableBytes(e.PayloadResponse),
			nullableSpillRefJSON(e.RequestSpillRef), nullableSpillRefJSON(e.ResponseSpillRef),
			nullableJSONString(e.NormalizedRequest), nullableJSONString(e.NormalizedResponse),
			nullableJSONString(e.RequestRedactionSpans), nullableJSONString(e.ResponseRedactionSpans),
			nullableInt(e.UpstreamTtfbMs), nullableInt(e.UpstreamTotalMs),
			nullableInt(e.RequestHooksMs), nullableInt(e.ResponseHooksMs),
			encodeBreakdown(e.LatencyBreakdown), nullableJSONString(e.HooksPipeline),
			nullableString(e.DomainRuleID), nullableString(e.PathAction),
			nullableString(e.TraceID),
		); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("batch insert event %s: %w", e.ID, err)
		}
	}
	return tx.Commit()
}

// nullableString returns nil for empty strings so SQLite stores SQL NULL
// rather than an empty string for the column. Used for method+path so the
// distinction "we didn't capture this" (NULL) vs "we captured an empty
// value" (empty string) is preserved on the wire to Hub.
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullableSpillRefJSON returns the JSON-encoded SpillRef for SQLite storage,
// or nil (SQL NULL) when the body travelled inline (ref == nil). Stored as
// TEXT in request_spill_ref / response_spill_ref and re-decoded on drain.
func nullableSpillRefJSON(ref *sharedaudit.SpillRef) any {
	if ref == nil {
		return nil
	}
	b, err := json.Marshal(ref)
	if err != nil {
		return nil
	}
	return string(b)
}

// decodeSpillRef parses a request_spill_ref / response_spill_ref TEXT column
// back into a *SpillRef. Returns nil for NULL / empty / malformed values so a
// corrupt column degrades to "no spill" rather than failing the drain.
func decodeSpillRef(col sql.NullString) *sharedaudit.SpillRef {
	if !col.Valid || col.String == "" {
		return nil
	}
	var ref sharedaudit.SpillRef
	if err := json.Unmarshal([]byte(col.String), &ref); err != nil {
		return nil
	}
	return &ref
}

// encodeBreakdown marshals a phase-breakdown map[string]int into a JSON
// text value for SQLite storage. Returns nil for empty maps so the column
// stores NULL.
func encodeBreakdown(m map[string]int) any {
	if len(m) == 0 {
		return nil
	}
	data, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return string(data)
}

// nullableJSONString returns nil for empty/nil RawMessage so SQLite stores
// NULL. Non-empty bytes are passed through as a string (SQLite TEXT column).
func nullableJSONString(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return string(raw)
}

// encodeTags marshals a compliance tag set into a JSON text value for
// SQLite storage. Returns nil for empty sets so the column stores NULL
// (matching nullableInt / nullableBytes conventions).
func encodeTags(tags []string) any {
	if len(tags) == 0 {
		return nil
	}
	data, err := json.Marshal(tags)
	if err != nil {
		return nil
	}
	return string(data)
}

// decodeTags parses the JSON text value produced by encodeTags. Returns nil
// for NULL or malformed payloads — callers treat nil as "no tags".
func decodeTags(raw sql.NullString) []string {
	if !raw.Valid || raw.String == "" {
		return nil
	}
	var tags []string
	if err := json.Unmarshal([]byte(raw.String), &tags); err != nil {
		return nil
	}
	return tags
}

// nullableBytes returns nil for an empty slice so SQLite stores NULL
// (matching the nullableInt convention). A non-empty slice is passed
// through unchanged; SQLite stores it in the payload_request /
// payload_response BLOB columns.
func nullableBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

// nullableInt converts a *int to a driver-friendly value: nil for NULL
// (so SQLite stores NULL and downstream SELECTs can distinguish "missing"
// from "zero") or the underlying int otherwise.
func nullableInt(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

// nullableStatusCode 把 0 当成 "未捕获" (SQLite NULL) — 真实 HTTP
// 状态码永远 > 0,所以 0 等价于 "没拿到 / 不适用",存 NULL 让
// downstream (UI / Hub 查询) 能区分 "没数据" 和 "确实是 0"。
func nullableStatusCode(sc int) any {
	if sc <= 0 {
		return nil
	}
	return sc
}

// DrainBatch returns up to `limit` unsynced events ordered by creation time.
func (q *Queue) DrainBatch(limit int) ([]event.Event, error) {
	rows, err := q.db.QueryContext(context.Background(), `
		SELECT id, timestamp, source_process, source_user, dest_host, dest_ip, dest_port,
		       method, path, status_code,
		       action, policy_rule_id, bump_status, bytes_in, bytes_out, duration_ms,
		       hook_decision, hook_reason, hook_reason_code, compliance_tags,
		       provider_name, model_name, api_key_class, api_key_fingerprint,
		       ingress_format,
		       prompt_tokens, completion_tokens, usage_extraction_status,
		       payload_request, payload_response,
		       request_spill_ref, response_spill_ref,
		       normalized_request, normalized_response,
		       request_redaction_spans, response_redaction_spans,
		       domain_rule_id, path_action,
		       trace_id
		FROM audit_events
		WHERE synced = 0
		ORDER BY created_at ASC
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck

	events := make([]event.Event, 0, limit)
	for rows.Next() {
		var e event.Event
		var ts string
		var method, path sql.NullString
		var statusCode sql.NullInt64
		var complianceTags sql.NullString
		var providerName, modelName, apiKeyClass, apiKeyFingerprint, usageStatus sql.NullString
		var promptTokens, completionTokens sql.NullInt64
		var payloadRequest, payloadResponse []byte
		var requestSpillRef, responseSpillRef sql.NullString
		var normalizedRequest, normalizedResponse sql.NullString
		var requestRedactionSpans, responseRedactionSpans sql.NullString
		var domainRuleID, pathAction, traceID, ingressFormat sql.NullString
		err := rows.Scan(&e.ID, &ts, &e.SourceProcess, &e.OSUser,
			&e.TargetHost, &e.DestIP, &e.DestPort,
			&method, &path, &statusCode,
			&e.Action,
			&e.PolicyRuleID, &e.BumpStatus, &e.BytesIn, &e.BytesOut, &e.LatencyMs,
			&e.HookDecision, &e.HookReason, &e.HookReasonCode, &complianceTags,
			&providerName, &modelName, &apiKeyClass, &apiKeyFingerprint,
			&ingressFormat,
			&promptTokens, &completionTokens, &usageStatus,
			&payloadRequest, &payloadResponse,
			&requestSpillRef, &responseSpillRef,
			&normalizedRequest, &normalizedResponse,
			&requestRedactionSpans, &responseRedactionSpans,
			&domainRuleID, &pathAction, &traceID)
		if err != nil {
			return nil, err
		}
		e.RequestSpillRef = decodeSpillRef(requestSpillRef)
		e.ResponseSpillRef = decodeSpillRef(responseSpillRef)
		if ingressFormat.Valid {
			e.IngressFormat = ingressFormat.String
		}
		if normalizedRequest.Valid && normalizedRequest.String != "" {
			e.NormalizedRequest = json.RawMessage(normalizedRequest.String)
		}
		if normalizedResponse.Valid && normalizedResponse.String != "" {
			e.NormalizedResponse = json.RawMessage(normalizedResponse.String)
		}
		if requestRedactionSpans.Valid && requestRedactionSpans.String != "" {
			e.RequestRedactionSpans = json.RawMessage(requestRedactionSpans.String)
		}
		if responseRedactionSpans.Valid && responseRedactionSpans.String != "" {
			e.ResponseRedactionSpans = json.RawMessage(responseRedactionSpans.String)
		}
		if traceID.Valid {
			e.TraceID = traceID.String
		}
		if method.Valid {
			e.Method = method.String
		}
		if path.Valid {
			e.Path = path.String
		}
		if statusCode.Valid {
			e.StatusCode = int(statusCode.Int64)
		}
		if domainRuleID.Valid {
			e.DomainRuleID = domainRuleID.String
		}
		if pathAction.Valid {
			e.PathAction = pathAction.String
		}
		e.ComplianceTags = decodeTags(complianceTags)
		if t, err := time.Parse(time.RFC3339Nano, ts); err != nil {
			slog.Warn("malformed audit timestamp", "id", e.ID, "raw", ts, "error", err)
		} else {
			e.Timestamp = t
		}
		e.ProviderName = providerName.String
		e.ModelName = modelName.String
		e.ApiKeyClass = apiKeyClass.String
		e.ApiKeyFingerprint = apiKeyFingerprint.String
		e.UsageExtractionStatus = usageStatus.String
		if promptTokens.Valid {
			v := int(promptTokens.Int64)
			e.PromptTokens = &v
		}
		if completionTokens.Valid {
			v := int(completionTokens.Int64)
			e.CompletionTokens = &v
		}
		e.PayloadRequest = payloadRequest
		e.PayloadResponse = payloadResponse
		events = append(events, e)
	}
	return events, rows.Err()
}

// MarkSynced marks the given event IDs as synced.
func (q *Queue) MarkSynced(ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()

	tx, err := q.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(context.Background(), "UPDATE audit_events SET synced = 1 WHERE id = ?")
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer stmt.Close() //nolint:errcheck
	for _, id := range ids {
		if _, err := stmt.ExecContext(context.Background(), id); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("mark synced %s: %w", id, err)
		}
	}
	return tx.Commit()
}

// UnsyncedCount returns the number of unsynced events.
func (q *Queue) UnsyncedCount() int {
	var count int
	if err := q.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM audit_events WHERE synced = 0").Scan(&count); err != nil {
		slog.Warn("UnsyncedCount query failed", "error", err)
		return 0
	}
	return count
}

// PruneAuditLocal deletes audit_local rows older than the given duration.
// audit_local is the local-only mirror that never uploads to Hub —
// without an explicit prune, every connection the agent ever
// intercepted accumulates forever in this table. Default retention
// is the same as audit_events (AuditRetentionDays) so the user-
// visible audit log and the system audit log age out together.
func (q *Queue) PruneAuditLocal(olderThan time.Duration) (int64, error) {
	if q == nil || q.db == nil {
		return 0, fmt.Errorf("PruneAuditLocal: nil queue")
	}
	// Wrap both sides in datetime() so sqlite normalizes the mixed
	// formats stored on this column: the schema DEFAULT uses
	// `datetime('now')` (space-separated "YYYY-MM-DD HH:MM:SS") while
	// some test/legacy paths backdate with RFC3339Nano (T-separated).
	// Raw lexicographic compare against either fixed format wrongly
	// includes or excludes the other.
	threshold := time.Now().Add(-olderThan).UTC().Format("2006-01-02 15:04:05")
	result, err := q.db.ExecContext(context.Background(),
		"DELETE FROM audit_local WHERE datetime(created_at) <= datetime(?)", threshold)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// PruneLifecycle deletes lifecycle_event rows older than the given
// duration. The Activity tab in the Dashboard renders this table; a
// year's worth of lifecycle events (~10/day/device) fits in well
// under 10k rows so retention can run conservatively long (30 days
// default) without filling the SQLCipher file.
func (q *Queue) PruneLifecycle(olderThan time.Duration) (int64, error) {
	if q == nil || q.db == nil {
		return 0, fmt.Errorf("PruneLifecycle: nil queue")
	}
	threshold := time.Now().Add(-olderThan).UTC().Format(time.RFC3339Nano)
	result, err := q.db.ExecContext(context.Background(),
		"DELETE FROM lifecycle_event WHERE occurred_at < ?", threshold)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// PruneSynced deletes synced events older than the given duration.
// Same datetime() normalization as PruneAuditLocal — see comment there
// for the lexicographic-comparison bug that raw < comparison would
// trigger against the `datetime('now')` schema DEFAULT.
func (q *Queue) PruneSynced(olderThan time.Duration) (int64, error) {
	threshold := time.Now().Add(-olderThan).UTC().Format("2006-01-02 15:04:05")
	result, err := q.db.ExecContext(context.Background(),
		"DELETE FROM audit_events WHERE synced = 1 AND datetime(created_at) <= datetime(?)", threshold)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// QueryEvents searches events with optional text filter and action filter.
// Returns matching events (paginated, time-descending) and total count.
//
// Lifecycle events (action prefix "agent.", e.g. agent.shutdown /
// agent.startup) are EXCLUDED by default — the GUI's Activity tab is
// scoped to "connections inspected, allowed, or denied" and surfacing
// agent.shutdown there confused users into thinking the daemon had
// intercepted its own process. Lifecycle rows still land in the local
// audit DB (forensic value + Hub upload) and are still returned when
// the caller asks for them explicitly via action="agent.shutdown" or
// any other exact "agent.*" string. See [[agent-shutdown-event-category]].
// QueryEventsFilter consolidates the QueryEvents parameter list so
// callers don't pass 6 positional args (search, action, aiOnly, since,
// offset, limit) at the call site. Zero values disable each respective
// filter (Search="" / Action="" / AIOnly=false / Since=time.Time{}).
//
// Why this struct: the UI has an AI-Only toggle + time-window selector
// on top of the original search/action pagination. Without the
// struct, every test caller and the IPC plumbing carry six positional
// args, half of which are usually zero — easy to swap by accident.
type QueryEventsFilter struct {
	Search string    // free-text LIKE %s% against source_process / dest_host / dest_ip / method / path
	Action string    // exact match on `action` column (deny|inspect|passthrough|agent.*)
	AIOnly bool      // when true, restrict to rows the agent treated as AI (domain_rule_id IS NOT NULL OR action='inspect')
	Since  time.Time // when non-zero, restrict to rows with created_at >= Since
	Offset int
	Limit  int
}

// QueryEvents searches events with optional text / action / AI / time
// filters. Returns matching events (paginated, time-descending) and
// total count. See QueryEventsFilter for filter semantics. The split
// between this method and `queryEventsImpl` is purely to keep the
// positional API for existing callers without losing the new
// filter capability — drop the wrapper once every caller migrates.
func (q *Queue) QueryEvents(search, action string, offset, limit int) ([]event.Event, int, error) {
	return q.QueryEventsFiltered(QueryEventsFilter{
		Search: search,
		Action: action,
		Offset: offset,
		Limit:  limit,
	})
}

// QueryEventsFiltered is the full-fat variant exposing the AI-Only and
// Since filters. Existing callers can keep using the
// positional QueryEvents above; the UI's Traffic page calls this one.
func (q *Queue) QueryEventsFiltered(f QueryEventsFilter) ([]event.Event, int, error) {
	where := "1=1"
	args := []any{}

	if f.Search != "" {
		// Extend search to method + path so users can filter by HTTP
		// method ("POST") or path ("/v1/messages") in addition to
		// host/IP/process.
		where += " AND (source_process LIKE ? OR dest_host LIKE ? OR dest_ip LIKE ? OR method LIKE ? OR path LIKE ?)"
		pat := "%" + f.Search + "%"
		args = append(args, pat, pat, pat, pat, pat)
	}
	if f.Action != "" {
		where += " AND action = ?"
		args = append(args, f.Action)
	} else {
		// Default view hides lifecycle rows; explicit action filter
		// (handled in the if-branch above) bypasses this so a future
		// Diagnostics page can pull them on demand.
		where += " AND action NOT LIKE 'agent.%'"
	}
	if f.AIOnly {
		// AI rows are those the daemon classified as worth inspecting:
		// either a domain rule matched (`domain_rule_id` populated) OR
		// the action is "inspect" (legacy rows pre-domain_rule_id stamp).
		// Without this clause the UI's "AI Only" filter had to over-fetch
		// pageSize*4 then drop in JS — broken pagination + wrong total.
		where += " AND (domain_rule_id != '' OR action = 'inspect')"
	}
	if !f.Since.IsZero() {
		// `created_at` is the SQLite-side write timestamp, not the
		// event's wire timestamp — what the user sees as "row recency"
		// in the UI. Compare against the same column we ORDER BY so
		// the time window precisely matches the displayed ordering.
		where += " AND created_at >= ?"
		args = append(args, f.Since.UTC().Format("2006-01-02 15:04:05"))
	}
	offset := f.Offset
	limit := f.Limit

	var total int
	countArgs := make([]any, len(args))
	copy(countArgs, args)
	if err := q.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM audit_events WHERE "+where, countArgs...).Scan(&total); err != nil {
		return nil, 0, err
	}

	// The list query is metadata-only: request/response bodies and normalized
	// payloads are NOT projected here (they can be large and the table never
	// renders them). The agent UI's detail drawer fetches those on demand via
	// EventByID. Spill refs are likewise omitted from the list.
	query := "SELECT id, timestamp, source_process, source_user, dest_host, dest_ip, dest_port, method, path, status_code, action, policy_rule_id, bump_status, bytes_in, bytes_out, duration_ms, hook_decision, hook_reason, hook_reason_code, compliance_tags, provider_name, model_name, api_key_class, api_key_fingerprint, prompt_tokens, completion_tokens, usage_extraction_status, upstream_ttfb_ms, upstream_total_ms, request_hooks_ms, response_hooks_ms, latency_breakdown, hooks_pipeline, domain_rule_id, path_action, trace_id FROM audit_events WHERE " + where + " ORDER BY created_at DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)
	rows, err := q.db.QueryContext(context.Background(), query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close() //nolint:errcheck

	var events []event.Event
	for rows.Next() {
		var e event.Event
		var ts string
		var method, path sql.NullString
		var statusCode sql.NullInt64
		var complianceTags sql.NullString
		var providerName, modelName, apiKeyClass, apiKeyFingerprint, usageStatus sql.NullString
		var promptTokens, completionTokens sql.NullInt64
		var upstreamTtfb, upstreamTotal, requestHooks, responseHooks sql.NullInt64
		var latencyBreakdown, hooksPipeline sql.NullString
		var domainRuleID, pathAction sql.NullString
		// Cross-service correlation id.
		var traceID sql.NullString
		// Body + normalized columns are intentionally NOT scanned here — the
		// list is metadata-only; the detail drawer fetches them via EventByID.
		if err := rows.Scan(&e.ID, &ts, &e.SourceProcess, &e.OSUser,
			&e.TargetHost, &e.DestIP, &e.DestPort,
			&method, &path, &statusCode,
			&e.Action,
			&e.PolicyRuleID, &e.BumpStatus, &e.BytesIn, &e.BytesOut, &e.LatencyMs,
			&e.HookDecision, &e.HookReason, &e.HookReasonCode, &complianceTags,
			&providerName, &modelName, &apiKeyClass, &apiKeyFingerprint,
			&promptTokens, &completionTokens, &usageStatus,
			&upstreamTtfb, &upstreamTotal, &requestHooks, &responseHooks,
			&latencyBreakdown, &hooksPipeline,
			&domainRuleID, &pathAction, &traceID); err != nil {
			return nil, 0, err
		}
		if traceID.Valid {
			e.TraceID = traceID.String
		}
		if method.Valid {
			e.Method = method.String
		}
		if path.Valid {
			e.Path = path.String
		}
		if statusCode.Valid {
			e.StatusCode = int(statusCode.Int64)
		}
		if domainRuleID.Valid {
			e.DomainRuleID = domainRuleID.String
		}
		if pathAction.Valid {
			e.PathAction = pathAction.String
		}
		e.ComplianceTags = decodeTags(complianceTags)
		if t, err := time.Parse(time.RFC3339Nano, ts); err != nil {
			slog.Warn("malformed audit timestamp", "id", e.ID, "raw", ts, "error", err)
		} else {
			e.Timestamp = t
		}
		e.ProviderName = providerName.String
		e.ModelName = modelName.String
		e.ApiKeyClass = apiKeyClass.String
		e.ApiKeyFingerprint = apiKeyFingerprint.String
		e.UsageExtractionStatus = usageStatus.String
		if promptTokens.Valid {
			v := int(promptTokens.Int64)
			e.PromptTokens = &v
		}
		if completionTokens.Valid {
			v := int(completionTokens.Int64)
			e.CompletionTokens = &v
		}
		if upstreamTtfb.Valid {
			v := int(upstreamTtfb.Int64)
			e.UpstreamTtfbMs = &v
		}
		if upstreamTotal.Valid {
			v := int(upstreamTotal.Int64)
			e.UpstreamTotalMs = &v
		}
		if requestHooks.Valid {
			v := int(requestHooks.Int64)
			e.RequestHooksMs = &v
		}
		if responseHooks.Valid {
			v := int(responseHooks.Int64)
			e.ResponseHooksMs = &v
		}
		if latencyBreakdown.Valid && latencyBreakdown.String != "" {
			var m map[string]int
			if err := json.Unmarshal([]byte(latencyBreakdown.String), &m); err == nil {
				e.LatencyBreakdown = m
			}
		}
		if hooksPipeline.Valid && hooksPipeline.String != "" {
			e.HooksPipeline = json.RawMessage(hooksPipeline.String)
		}
		events = append(events, e)
	}
	return events, total, rows.Err()
}

// EventByID returns the full detail row for a single event: list metadata plus
// the inline body, normalized payloads, and spill refs. It is the data source
// for the agent UI's detail drawer, which fetches the heavy fields on demand
// rather than dragging them through every list page (QueryEventsFiltered).
//
// Spill refs are returned as-is; reading an oversize body back from the local
// spill store is the caller's job (it owns the spill reader). Returns
// (nil, nil) when no row matches id.
func (q *Queue) EventByID(id string) (*event.Event, error) {
	if q == nil || q.db == nil {
		return nil, fmt.Errorf("EventByID: nil queue")
	}
	row := q.db.QueryRowContext(context.Background(), `
		SELECT id, timestamp, source_process, source_user, dest_host, dest_ip, dest_port,
		       method, path, status_code,
		       action, policy_rule_id, bump_status, bytes_in, bytes_out, duration_ms,
		       hook_decision, hook_reason, hook_reason_code, compliance_tags,
		       provider_name, model_name, api_key_class, api_key_fingerprint,
		       ingress_format,
		       prompt_tokens, completion_tokens, usage_extraction_status,
		       payload_request, payload_response,
		       request_spill_ref, response_spill_ref,
		       normalized_request, normalized_response,
		       request_redaction_spans, response_redaction_spans,
		       upstream_ttfb_ms, upstream_total_ms, request_hooks_ms, response_hooks_ms,
		       latency_breakdown, hooks_pipeline,
		       domain_rule_id, path_action, trace_id
		FROM audit_events WHERE id = ?`, id)

	var e event.Event
	var ts string
	var method, path sql.NullString
	var statusCode sql.NullInt64
	var complianceTags sql.NullString
	var providerName, modelName, apiKeyClass, apiKeyFingerprint, usageStatus sql.NullString
	var promptTokens, completionTokens sql.NullInt64
	var payloadRequest, payloadResponse []byte
	var requestSpillRef, responseSpillRef sql.NullString
	var normalizedRequest, normalizedResponse sql.NullString
	var requestRedactionSpans, responseRedactionSpans sql.NullString
	var upstreamTtfb, upstreamTotal, requestHooks, responseHooks sql.NullInt64
	var latencyBreakdown, hooksPipeline sql.NullString
	var domainRuleID, pathAction, traceID, ingressFormat sql.NullString
	err := row.Scan(&e.ID, &ts, &e.SourceProcess, &e.OSUser,
		&e.TargetHost, &e.DestIP, &e.DestPort,
		&method, &path, &statusCode,
		&e.Action,
		&e.PolicyRuleID, &e.BumpStatus, &e.BytesIn, &e.BytesOut, &e.LatencyMs,
		&e.HookDecision, &e.HookReason, &e.HookReasonCode, &complianceTags,
		&providerName, &modelName, &apiKeyClass, &apiKeyFingerprint,
		&ingressFormat,
		&promptTokens, &completionTokens, &usageStatus,
		&payloadRequest, &payloadResponse,
		&requestSpillRef, &responseSpillRef,
		&normalizedRequest, &normalizedResponse,
		&requestRedactionSpans, &responseRedactionSpans,
		&upstreamTtfb, &upstreamTotal, &requestHooks, &responseHooks,
		&latencyBreakdown, &hooksPipeline,
		&domainRuleID, &pathAction, &traceID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	e.PayloadRequest = payloadRequest
	e.PayloadResponse = payloadResponse
	e.RequestSpillRef = decodeSpillRef(requestSpillRef)
	e.ResponseSpillRef = decodeSpillRef(responseSpillRef)
	if ingressFormat.Valid {
		e.IngressFormat = ingressFormat.String
	}
	if normalizedRequest.Valid && normalizedRequest.String != "" {
		e.NormalizedRequest = json.RawMessage(normalizedRequest.String)
	}
	if normalizedResponse.Valid && normalizedResponse.String != "" {
		e.NormalizedResponse = json.RawMessage(normalizedResponse.String)
	}
	if requestRedactionSpans.Valid && requestRedactionSpans.String != "" {
		e.RequestRedactionSpans = json.RawMessage(requestRedactionSpans.String)
	}
	if responseRedactionSpans.Valid && responseRedactionSpans.String != "" {
		e.ResponseRedactionSpans = json.RawMessage(responseRedactionSpans.String)
	}
	if traceID.Valid {
		e.TraceID = traceID.String
	}
	if method.Valid {
		e.Method = method.String
	}
	if path.Valid {
		e.Path = path.String
	}
	if statusCode.Valid {
		e.StatusCode = int(statusCode.Int64)
	}
	if domainRuleID.Valid {
		e.DomainRuleID = domainRuleID.String
	}
	if pathAction.Valid {
		e.PathAction = pathAction.String
	}
	e.ComplianceTags = decodeTags(complianceTags)
	if t, perr := time.Parse(time.RFC3339Nano, ts); perr == nil {
		e.Timestamp = t
	}
	e.ProviderName = providerName.String
	e.ModelName = modelName.String
	e.ApiKeyClass = apiKeyClass.String
	e.ApiKeyFingerprint = apiKeyFingerprint.String
	e.UsageExtractionStatus = usageStatus.String
	if promptTokens.Valid {
		v := int(promptTokens.Int64)
		e.PromptTokens = &v
	}
	if completionTokens.Valid {
		v := int(completionTokens.Int64)
		e.CompletionTokens = &v
	}
	if upstreamTtfb.Valid {
		v := int(upstreamTtfb.Int64)
		e.UpstreamTtfbMs = &v
	}
	if upstreamTotal.Valid {
		v := int(upstreamTotal.Int64)
		e.UpstreamTotalMs = &v
	}
	if requestHooks.Valid {
		v := int(requestHooks.Int64)
		e.RequestHooksMs = &v
	}
	if responseHooks.Valid {
		v := int(responseHooks.Int64)
		e.ResponseHooksMs = &v
	}
	if latencyBreakdown.Valid && latencyBreakdown.String != "" {
		var m map[string]int
		if json.Unmarshal([]byte(latencyBreakdown.String), &m) == nil {
			e.LatencyBreakdown = m
		}
	}
	if hooksPipeline.Valid && hooksPipeline.String != "" {
		e.HooksPipeline = json.RawMessage(hooksPipeline.String)
	}
	return &e, nil
}

// LifecycleEvent is the on-the-wire shape returned by QueryLifecycle.
// It mirrors the lifecycle_event row columns plus a JSON-decoded Attrs
// map so the IPC layer can hand the GUI a ready-to-render object
// without re-parsing on the receiving end.
type LifecycleEvent struct {
	ID         string         `json:"id"`
	OccurredAt time.Time      `json:"occurredAt"`
	Action     string         `json:"action"`
	Message    string         `json:"message"`
	Level      string         `json:"level"`
	Attrs      map[string]any `json:"attrs,omitempty"`
}

// RecordLifecycle persists a lifecycle event row to the local
// SQLCipher mirror. Called by the lifecycle.Emitter after a successful
// (or attempted) PushDiagEvent so the agent's Dashboard "Activity"
// page can render the event without round-tripping through Hub.
//
// id must be globally unique (caller supplies a UUID). attrs is a Go
// map that is JSON-encoded into the TEXT column; pass nil for events
// with no extra attributes. Insert is best-effort — a write failure
// is logged by the caller but never breaks the lifecycle flow.
func (q *Queue) RecordLifecycle(id string, occurredAt time.Time, action, message, level string, attrs map[string]any) error {
	if q == nil || q.db == nil {
		return fmt.Errorf("RecordLifecycle: nil queue")
	}
	var attrsJSON sql.NullString
	if len(attrs) > 0 {
		b, err := json.Marshal(attrs)
		if err != nil {
			return fmt.Errorf("marshal lifecycle attrs: %w", err)
		}
		attrsJSON = sql.NullString{String: string(b), Valid: true}
	}
	_, err := q.db.ExecContext(context.Background(), `
		INSERT OR IGNORE INTO lifecycle_event (id, occurred_at, action, message, level, attrs)
		VALUES (?, ?, ?, ?, ?, ?)
	`, id, occurredAt.UTC().Format(time.RFC3339Nano), action, message, level, attrsJSON)
	if err != nil {
		return fmt.Errorf("insert lifecycle_event: %w", err)
	}
	return nil
}

// QueryLifecycle returns up to limit lifecycle event rows ordered
// time-descending, with offset for pagination. Consumed by the
// agent's Dashboard "Activity" page via the QUERY_LIFECYCLE_EVENTS
// IPC. Returns total row count for paging.
func (q *Queue) QueryLifecycle(offset, limit int) ([]LifecycleEvent, int, error) {
	if q == nil || q.db == nil {
		return nil, 0, fmt.Errorf("QueryLifecycle: nil queue")
	}
	if limit <= 0 {
		limit = 50
	}
	var total int
	if err := q.db.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM lifecycle_event").Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count lifecycle_event: %w", err)
	}
	rows, err := q.db.QueryContext(context.Background(), `
		SELECT id, occurred_at, action, message, level, attrs
		FROM lifecycle_event
		ORDER BY occurred_at DESC
		LIMIT ? OFFSET ?
	`, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("query lifecycle_event: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	out := make([]LifecycleEvent, 0, limit)
	for rows.Next() {
		var ev LifecycleEvent
		var occurredAt string
		var attrs sql.NullString
		if err := rows.Scan(&ev.ID, &occurredAt, &ev.Action, &ev.Message, &ev.Level, &attrs); err != nil {
			return nil, 0, fmt.Errorf("scan lifecycle_event: %w", err)
		}
		t, parseErr := time.Parse(time.RFC3339Nano, occurredAt)
		if parseErr != nil {
			// Fall back to non-nano RFC3339 for rows written before
			// the format was pinned; both parse cleanly.
			t, _ = time.Parse(time.RFC3339, occurredAt)
		}
		ev.OccurredAt = t
		if attrs.Valid && attrs.String != "" {
			_ = json.Unmarshal([]byte(attrs.String), &ev.Attrs)
		}
		out = append(out, ev)
	}
	return out, total, rows.Err()
}

// Close closes the database connection.
func (q *Queue) Close() error {
	return q.db.Close()
}

// DB exposes the underlying *sql.DB so adjacent subsystems (e.g. the diag
// pending_diag_event buffer) can run their own migrations and CRUD against
// the same SQLCipher handle without duplicating the encryption-key wiring
// or doubling the file-handle count.
func (q *Queue) DB() *sql.DB {
	return q.db
}

// DrainLoop runs the drain cycle. Blocks until ctx is cancelled.
func (q *Queue) DrainLoop(ctx context.Context, interval time.Duration, batchSize int, uploadFn func([]event.Event) error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			q.drainOnce(batchSize, uploadFn)
			return
		case <-ticker.C:
			q.drainOnce(batchSize, uploadFn)
		}
	}
}

// maxDrainBatchesPerWake bounds how many back-to-back full batches one
// wake-up will upload, so clearing a deep backlog cannot monopolise the
// goroutine (or the Hub) indefinitely — the next tick picks up the rest.
const maxDrainBatchesPerWake = 50

// drainOnce uploads pending audit events. It keeps draining back-to-back
// while each batch comes back FULL (a full batch means more is waiting),
// so a backlog built up between ticks clears at the upload rate instead
// of one batch per tick. A partial batch, an empty queue, an upload
// failure, or the per-wake cap ends the cycle.
func (q *Queue) drainOnce(batchSize int, uploadFn func([]event.Event) error) {
	for range maxDrainBatchesPerWake {
		events, err := q.DrainBatch(batchSize)
		if err != nil {
			slog.Error("drain batch failed", "error", err)
			return
		}
		if len(events) == 0 {
			return
		}

		if err := uploadFn(events); err != nil {
			slog.Warn("audit upload failed, will retry", "error", err, "count", len(events))
			return
		}

		ids := make([]string, len(events))
		for j, e := range events {
			ids[j] = e.ID
		}
		if err := q.MarkSynced(ids); err != nil {
			slog.Error("mark synced failed", "error", err)
			return
		}

		// A short batch means the queue is drained for now — stop and
		// wait for the next tick rather than spin on empty reads.
		if len(events) < batchSize {
			return
		}
	}
}

// RecordLocal writes an event to the local-only audit table (not uploaded).
func (q *Queue) RecordLocal(id, timestamp, destHost, destIP string, destPort int, action, hookDecision, hookReason, sourceProcess, sourceUser string, bytesIn, bytesOut int) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	_, err := q.db.ExecContext(context.Background(),
		`INSERT OR IGNORE INTO audit_local (id, timestamp, dest_host, dest_ip, dest_port, action, hook_decision, hook_reason, source_process, source_user, bytes_in, bytes_out)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, timestamp, destHost, destIP, destPort, action, hookDecision, hookReason, sourceProcess, sourceUser, bytesIn, bytesOut,
	)
	return err
}
