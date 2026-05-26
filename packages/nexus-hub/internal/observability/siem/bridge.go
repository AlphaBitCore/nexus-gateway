// Package siem contains the Hub's SIEM bridge: a polling forwarder that
// reads new traffic_event and AdminAuditLog rows, classifies them, and
// pushes them to an external SIEM via a pluggable Sink. Configuration is
// sourced from the siem.config row in system_metadata which is managed by
// the Control Plane admin API.
package siem

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PgxPool is the minimum pgx pool surface the SIEM bridge needs. The
// concrete *pgxpool.Pool satisfies it in production; pgxmock's
// PgxPoolIface satisfies it in unit tests so the bridge's Reload /
// Poll / queryEvents / queryAdminEvents / loadCheckpoint / saveCheckpoint
// paths can be driven without a live PostgreSQL.
//
// Mirrors the PgxPool convention from packages/control-plane/internal/store
// and packages/nexus-hub/internal/store.
type PgxPool interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// BridgeConfig holds runtime configuration for the SIEM bridge.
type BridgeConfig struct {
	Enabled      bool          `json:"enabled"`
	PollInterval time.Duration `json:"pollInterval"`
	BatchSize    int           `json:"batchSize"`
	EventTypes   []string      `json:"eventTypes"`
	TrafficMode  string        `json:"trafficMode"`
}

// Bridge polls the unified traffic_event table and the AdminAuditLog table for
// new rows since independent checkpoints and forwards them to an external SIEM
// via a pluggable Sink. Checkpoints are persisted in system_metadata so the
// bridge survives restarts without re-sending events.
//
// sink + cfg are atomic so the bridge re-reads siem.config from system_metadata
// at the head of every Poll() cycle. The Control Plane admin UI writes the
// config row; the bridge picks the change up within one poll interval
// (default 30s) — no shadow / restart plumbing needed.
type Bridge struct {
	pool   PgxPool
	logger *slog.Logger
	mu     sync.Mutex

	// activeSink is nil when SIEM is disabled (Enabled=false in
	// siem.config or row absent entirely). Poll() short-circuits on nil
	// so the scheduler tick costs nothing beyond the config probe.
	activeSink atomic.Pointer[Sink]
	activeCfg  atomic.Pointer[BridgeConfig]
}

// NewBridge creates a SIEM bridge. The bridge does not start polling
// automatically — the caller is responsible for invoking Poll on a
// schedule (typically via the Hub scheduler).
//
// When sink is nil or cfg.Enabled is false the bridge is constructed in
// "disabled" mode — Poll() refreshes config on every tick and lazily builds
// the sink on first enable, so the scheduler can register an always-on job
// at startup that lights up the moment an admin saves siem.config without
// requiring a restart.
func NewBridge(pool *pgxpool.Pool, sink Sink, cfg BridgeConfig, logger *slog.Logger) *Bridge {
	return newBridgeWithPool(pool, sink, cfg, logger)
}

// newBridgeWithPool is the internal constructor that accepts the PgxPool
// interface so tests can inject a pgxmock pool via the test-only
// NewBridgeWithPool wrapper. Production callers go through NewBridge with
// a real *pgxpool.Pool (which satisfies PgxPool).
func newBridgeWithPool(pool PgxPool, sink Sink, cfg BridgeConfig, logger *slog.Logger) *Bridge {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 30 * time.Second
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 200
	}
	b := &Bridge{
		pool:   pool,
		logger: logger,
	}
	if sink != nil {
		s := sink
		b.activeSink.Store(&s)
	}
	c := cfg
	b.activeCfg.Store(&c)
	return b
}

// PollInterval returns the configured poll interval for the scheduler.
// Reads the live snapshot so a poll-cadence change in siem.config takes
// effect after the next Reload (typically one tick later).
func (b *Bridge) PollInterval() time.Duration {
	if c := b.activeCfg.Load(); c != nil {
		return c.PollInterval
	}
	return 30 * time.Second
}

// ActiveSinkName returns the name of the currently active sink for
// startup logging. Empty string when the bridge is in the disabled
// state (no SIEM config / Enabled=false / unreachable row).
func (b *Bridge) ActiveSinkName() string {
	if p := b.activeSink.Load(); p != nil {
		return (*p).Name()
	}
	return ""
}

// Reload re-reads the siem.config row from system_metadata and swaps
// the active sink + cfg atomically. Called at the head of every Poll
// so admin UI saves take effect within one poll cycle (≤ 30s). Safe to
// call concurrently with Poll thanks to the atomic pointers; the mu
// inside Poll serialises actual delivery batches.
//
// Returns an error only on unmarshal failures (caller logs); a missing
// row, parse failure of a sub-field, or Enabled=false all collapse the
// active sink to nil so Poll becomes a no-op.
func (b *Bridge) Reload(ctx context.Context) error {
	var raw json.RawMessage
	err := b.pool.QueryRow(ctx,
		`SELECT value FROM system_metadata WHERE key = $1`, "siem.config",
	).Scan(&raw)
	if err != nil {
		// Row missing → SIEM intentionally off. Clear the active sink so
		// Poll skips. Don't surface as an error to the scheduler.
		if errors.Is(err, pgx.ErrNoRows) {
			b.activeSink.Store(nil)
			return nil
		}
		return fmt.Errorf("read siem.config: %w", err)
	}

	var cfgRow struct {
		Enabled    bool              `json:"enabled"`
		URL        string            `json:"url"`
		Headers    map[string]string `json:"headers"`
		Format     string            `json:"format"`
		Interval   int               `json:"pollIntervalSeconds"`
		Batch      int               `json:"batchSize"`
		EventTypes []string          `json:"eventTypes"`
	}
	if err := json.Unmarshal(raw, &cfgRow); err != nil {
		return fmt.Errorf("parse siem.config: %w", err)
	}

	if !cfgRow.Enabled || cfgRow.URL == "" {
		b.activeSink.Store(nil)
		return nil
	}

	// Build a new sink from the current config. Cheap (no network).
	formatter := NewFormatter(cfgRow.Format)
	sink, err := NewHTTPSink(cfgRow.URL, cfgRow.Headers, formatter)
	if err != nil {
		return fmt.Errorf("build sink: %w", err)
	}

	newCfg := BridgeConfig{
		Enabled:      true,
		PollInterval: time.Duration(cfgRow.Interval) * time.Second,
		BatchSize:    cfgRow.Batch,
		EventTypes:   cfgRow.EventTypes,
	}
	if newCfg.PollInterval <= 0 {
		newCfg.PollInterval = 30 * time.Second
	}
	if newCfg.BatchSize <= 0 {
		newCfg.BatchSize = 200
	}

	var s Sink = sink
	b.activeSink.Store(&s)
	b.activeCfg.Store(&newCfg)
	return nil
}

const checkpointKey = "siem.bridge.checkpoint"
const adminCheckpointKey = "siem.bridge.admin_checkpoint"

// Poll loads independent checkpoints for traffic events and admin audit events,
// queries new rows for each, classifies and merges them, optionally filters by
// configured event types, sends the batch to the Sink, and updates the
// checkpoints. Thread-safe: only one Poll executes at a time.
//
// Reload runs first on every tick so admin UI changes propagate within one
// poll interval. If SIEM is disabled (no row / Enabled=false / empty URL),
// Reload nils out activeSink and Poll returns immediately.
func (b *Bridge) Poll(ctx context.Context) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if err := b.Reload(ctx); err != nil {
		b.logger.Warn("siem bridge: reload config failed, using previous sink",
			"error", err)
	}
	sinkPtr := b.activeSink.Load()
	cfg := b.activeCfg.Load()
	if sinkPtr == nil || cfg == nil {
		return // SIEM disabled — no-op cycle.
	}
	activeSink := *sinkPtr

	trafficCP, err := b.loadCheckpoint(ctx, checkpointKey)
	if err != nil {
		b.logger.Error("siem bridge: load traffic checkpoint", "error", err)
		return
	}
	trafficEvents, trafficLastTS, err := b.queryEvents(ctx, trafficCP, cfg.BatchSize, cfg.TrafficMode)
	if err != nil {
		b.logger.Error("siem bridge: query traffic events", "error", err)
		return
	}
	for i := range trafficEvents {
		trafficEvents[i]["eventType"] = ClassifyTrafficEvent(trafficEvents[i])
	}

	adminCP, err := b.loadCheckpoint(ctx, adminCheckpointKey)
	if err != nil {
		b.logger.Error("siem bridge: load admin checkpoint", "error", err)
		return
	}
	adminEvents, adminLastTS, err := b.queryAdminEvents(ctx, adminCP, cfg.BatchSize)
	if err != nil {
		b.logger.Error("siem bridge: query admin events", "error", err)
		return
	}
	for i := range adminEvents {
		adminEvents[i]["eventType"] = ClassifyAdminEvent(adminEvents[i])
	}

	all := make([]Event, 0, len(trafficEvents)+len(adminEvents))
	all = append(all, trafficEvents...)
	all = append(all, adminEvents...)
	all = FilterByEventTypes(all, cfg.EventTypes)
	if len(all) == 0 {
		return
	}

	if err := activeSink.Send(ctx, all); err != nil {
		b.logger.Error("siem bridge: send failed",
			"sink", activeSink.Name(), "count", len(all), "error", err)
		return
	}

	if len(trafficEvents) > 0 {
		if err := b.saveCheckpoint(ctx, checkpointKey, trafficLastTS); err != nil {
			b.logger.Error("siem bridge: save traffic checkpoint", "error", err)
		}
	}
	if len(adminEvents) > 0 {
		if err := b.saveCheckpoint(ctx, adminCheckpointKey, adminLastTS); err != nil {
			b.logger.Error("siem bridge: save admin checkpoint", "error", err)
		}
	}

	b.logger.Info("siem bridge: forwarded events",
		"sink", activeSink.Name(),
		"count", len(all),
		"traffic", len(trafficEvents),
		"admin", len(adminEvents))
}

// loadCheckpoint reads the last-forwarded timestamp for key from system_metadata.
// Returns 24 hours ago if no checkpoint exists for that key.
//
// Accepts either of two on-disk shapes for backward compatibility with
// older runs that wrote a `{"lastForwardedAt": "..."}` object:
//
//   - bare RFC3339Nano JSON string (current format written by saveCheckpoint)
//   - object with a "lastForwardedAt" field (legacy, surfaced only when an
//     existing deploy already had the row before this code shipped)
//
// Stale unparseable rows fall back to the 24h-ago default rather than
// crashing the bridge — the next saveCheckpoint will normalise the row.
func (b *Bridge) loadCheckpoint(ctx context.Context, key string) (time.Time, error) {
	var raw json.RawMessage
	err := b.pool.QueryRow(ctx,
		`SELECT value FROM system_metadata WHERE key = $1`, key,
	).Scan(&raw)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return time.Now().UTC().Add(-24 * time.Hour), nil
		}
		return time.Time{}, fmt.Errorf("load checkpoint %s: %w", key, err)
	}

	// Try the canonical bare-string form first.
	var ts string
	if err := json.Unmarshal(raw, &ts); err == nil {
		if t, perr := time.Parse(time.RFC3339Nano, ts); perr == nil {
			return t, nil
		}
	}
	// Fall back to the legacy `{"lastForwardedAt": "..."}` object form.
	var legacy struct {
		LastForwardedAt string `json:"lastForwardedAt"`
	}
	if err := json.Unmarshal(raw, &legacy); err == nil && legacy.LastForwardedAt != "" {
		if t, perr := time.Parse(time.RFC3339Nano, legacy.LastForwardedAt); perr == nil {
			b.logger.Info("siem bridge: migrated legacy checkpoint format",
				slog.String("key", key))
			return t, nil
		}
	}
	// Unparseable — log and reset to the 24h-ago default. The next
	// successful flush will overwrite the row in the canonical format.
	b.logger.Warn("siem bridge: checkpoint row unparseable, resetting to 24h default",
		slog.String("key", key),
		slog.String("raw", string(raw)))
	return time.Now().UTC().Add(-24 * time.Hour), nil
}

// saveCheckpoint upserts the checkpoint for key into system_metadata.
func (b *Bridge) saveCheckpoint(ctx context.Context, key string, ts time.Time) error {
	value, err := json.Marshal(ts.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("marshal checkpoint %s: %w", key, err)
	}
	_, err = b.pool.Exec(ctx, `
		INSERT INTO system_metadata (key, value, updated_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (key) DO UPDATE SET value = $2, updated_at = NOW()
	`, key, value)
	if err != nil {
		return fmt.Errorf("save checkpoint %s: %w", key, err)
	}
	return nil
}

// queryEvents fetches up to batchSize rows from traffic_event with
// timestamp > since, ordered by timestamp ASC. When trafficMode is "all",
// every traffic event is returned; otherwise only security-relevant rows
// (blocked, rate-limited, budget-exceeded) are included.
//
// batchSize + trafficMode parameters come from the live cfg snapshot in
// Poll() so the bridge picks up the latest siem.config values without
// the per-query path reading from a struct field.
func (b *Bridge) queryEvents(ctx context.Context, since time.Time, batchSize int, trafficMode string) ([]Event, time.Time, error) {
	// The traffic_event table uses split request_hook_* + response_hook_*
	// columns (one per pipeline stage). The bridge selects both pairs and
	// exposes them as requestHook* / responseHook* in the outgoing event so
	// SIEM dashboards can distinguish pipeline stages. For "security mode"
	// filtering, EITHER stage's block / rate-limited / budget-exceeded signal
	// makes the row interesting.
	var query string
	if trafficMode == "all" {
		query = `
			SELECT id, source, timestamp,
			       source_ip, target_host, method, path, status_code, latency_ms,
			       entity_id, entity_type, org_id,
			       request_hook_decision, request_hook_reason, request_hook_reason_code,
			       response_hook_decision, response_hook_reason, response_hook_reason_code,
			       compliance_tags,
			       details
			FROM traffic_event
			WHERE timestamp > $1
			ORDER BY timestamp ASC
			LIMIT $2
		`
	} else {
		query = `
			SELECT id, source, timestamp,
			       source_ip, target_host, method, path, status_code, latency_ms,
			       entity_id, entity_type, org_id,
			       request_hook_decision, request_hook_reason, request_hook_reason_code,
			       response_hook_decision, response_hook_reason, response_hook_reason_code,
			       compliance_tags,
			       details
			FROM traffic_event
			WHERE timestamp > $1
			  AND (request_hook_decision = 'block'
			       OR response_hook_decision = 'block'
			       OR request_hook_reason_code IN ('rate_limited', 'budget_exceeded')
			       OR response_hook_reason_code IN ('rate_limited', 'budget_exceeded'))
			ORDER BY timestamp ASC
			LIMIT $2
		`
	}
	rows, err := b.pool.Query(ctx, query, since, batchSize)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("query traffic_event: %w", err)
	}
	defer rows.Close()

	var events []Event
	var lastTS time.Time

	for rows.Next() {
		var (
			id, source                                           string
			ts                                                   time.Time
			sourceIP, targetHost, method, path                   *string
			statusCode, latencyMs                                *int
			entityID, entityType, orgID                          *string
			reqHookDecision, reqHookReason, reqHookReasonCode    *string
			respHookDecision, respHookReason, respHookReasonCode *string
			complianceTags                                       []string
			details                                              *json.RawMessage
		)

		if err := rows.Scan(
			&id, &source, &ts,
			&sourceIP, &targetHost, &method, &path, &statusCode, &latencyMs,
			&entityID, &entityType, &orgID,
			&reqHookDecision, &reqHookReason, &reqHookReasonCode,
			&respHookDecision, &respHookReason, &respHookReasonCode,
			&complianceTags,
			&details,
		); err != nil {
			return nil, time.Time{}, fmt.Errorf("scan traffic_event: %w", err)
		}

		evt := Event{
			"id":        id,
			"source":    source,
			"timestamp": ts.UTC().Format(time.RFC3339Nano),
		}

		setIfNotNil(evt, "sourceIp", sourceIP)
		setIfNotNil(evt, "targetHost", targetHost)
		setIfNotNil(evt, "method", method)
		setIfNotNil(evt, "path", path)
		setIntIfNotNil(evt, "statusCode", statusCode)
		setIntIfNotNil(evt, "latencyMs", latencyMs)
		setIfNotNil(evt, "entityId", entityID)
		setIfNotNil(evt, "entityType", entityType)
		setIfNotNil(evt, "orgId", orgID)
		setIfNotNil(evt, "requestHookDecision", reqHookDecision)
		setIfNotNil(evt, "requestHookReason", reqHookReason)
		setIfNotNil(evt, "requestHookReasonCode", reqHookReasonCode)
		setIfNotNil(evt, "responseHookDecision", respHookDecision)
		setIfNotNil(evt, "responseHookReason", respHookReason)
		setIfNotNil(evt, "responseHookReasonCode", respHookReasonCode)
		// Back-compat alias: keep flat hookDecision / hookReasonCode in the
		// outgoing event so existing SIEM dashboards keep matching. Prefer
		// response (final) over request when both are present.
		flatDecision := respHookDecision
		if flatDecision == nil {
			flatDecision = reqHookDecision
		}
		flatReason := respHookReason
		if flatReason == nil {
			flatReason = reqHookReason
		}
		flatCode := respHookReasonCode
		if flatCode == nil {
			flatCode = reqHookReasonCode
		}
		setIfNotNil(evt, "hookDecision", flatDecision)
		setIfNotNil(evt, "hookReason", flatReason)
		setIfNotNil(evt, "hookReasonCode", flatCode)
		if len(complianceTags) > 0 {
			evt["complianceTags"] = complianceTags
		}
		if details != nil {
			var parsed any
			if json.Unmarshal(*details, &parsed) == nil {
				evt["details"] = parsed
			}
		}

		events = append(events, evt)
		lastTS = ts
	}

	if err := rows.Err(); err != nil {
		return nil, time.Time{}, fmt.Errorf("rows iteration: %w", err)
	}

	return events, lastTS, nil
}

// queryAdminEvents fetches up to batchSize rows from the AdminAuditLog table
// with timestamp > since, ordered by timestamp ASC.
//
// batchSize comes from the live cfg snapshot in Poll() so a shadow-driven
// change to siem.config.batchSize takes effect on the next tick without
// rebuilding the bridge.
func (b *Bridge) queryAdminEvents(ctx context.Context, since time.Time, batchSize int) ([]Event, time.Time, error) {
	rows, err := b.pool.Query(ctx, `
		SELECT id, timestamp,
		       "actorId", "actorLabel", "actorRole",
		       "sourceIp", action, "entityType", "entityId",
		       "beforeState", "afterState"
		FROM "AdminAuditLog"
		WHERE timestamp > $1
		ORDER BY timestamp ASC
		LIMIT $2
	`, since, batchSize)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("query AdminAuditLog: %w", err)
	}
	defer rows.Close()

	var events []Event
	var lastTS time.Time

	for rows.Next() {
		var (
			id          string
			ts          time.Time
			actorID     *string
			actorLabel  *string
			actorRole   *string
			sourceIP    *string
			action      string
			entityType  string
			entityID    *string
			beforeState *json.RawMessage
			afterState  *json.RawMessage
		)

		if err := rows.Scan(
			&id, &ts,
			&actorID, &actorLabel, &actorRole,
			&sourceIP, &action, &entityType, &entityID,
			&beforeState, &afterState,
		); err != nil {
			return nil, time.Time{}, fmt.Errorf("scan AdminAuditLog: %w", err)
		}

		evt := Event{
			"id":         id,
			"source":     "admin",
			"timestamp":  ts.UTC().Format(time.RFC3339Nano),
			"action":     action,
			"entityType": entityType,
		}

		setIfNotNil(evt, "actorId", actorID)
		setIfNotNil(evt, "actorLabel", actorLabel)
		setIfNotNil(evt, "actorRole", actorRole)
		setIfNotNil(evt, "sourceIp", sourceIP)
		setIfNotNil(evt, "entityId", entityID)

		if beforeState != nil {
			var parsed any
			if json.Unmarshal(*beforeState, &parsed) == nil {
				evt["beforeState"] = parsed
			}
		}
		if afterState != nil {
			var parsed any
			if json.Unmarshal(*afterState, &parsed) == nil {
				evt["afterState"] = parsed
			}
		}

		events = append(events, evt)
		lastTS = ts
	}

	if err := rows.Err(); err != nil {
		return nil, time.Time{}, fmt.Errorf("rows iteration (AdminAuditLog): %w", err)
	}

	return events, lastTS, nil
}

func setIfNotNil(evt Event, key string, val *string) {
	if val != nil {
		evt[key] = *val
	}
}

func setIntIfNotNil(evt Event, key string, val *int) {
	if val != nil {
		evt[key] = *val
	}
}
