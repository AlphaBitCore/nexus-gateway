package thingclient

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"
)

// OnConfigChangedFunc is the callback signature for config changes.
// It receives the full desired config map and returns the reported config map
// (what was actually applied). If it returns an error, the shadow report
// is not sent and the error is logged.
type OnConfigChangedFunc func(desired map[string]ConfigState) (reported map[string]ConfigState, err error)

// OnConfigChanged registers the callback invoked when desired config changes.
// Although the convention is "call before Start()", we still take c.mu so a
// caller that legitimately swaps the callback at runtime (hot-reload tests,
// runtime introspection) is race-free under -race.
//
// The callback receives the full desired state map. It must:
//   - Apply Category A configs directly from ConfigState.State
//   - Reload Category B configs from DB when ConfigState.Version changes
//   - Return the reported state map reflecting what was actually applied
//   - Return an error only if the apply fundamentally failed (partial applies
//     should still return the partial reported state)
//
// The callback is called synchronously on the client's internal goroutine.
func (c *Client) OnConfigChanged(fn OnConfigChangedFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onConfigChanged = fn
}

// applyConfig checks versions, calls the OnConfigChanged callback, and sends
// a shadow report. Called from connectWS (initial), readPump (config_changed),
// and httpHeartbeat (version mismatch).
func (c *Client) applyConfig(desired map[string]ConfigState, desiredVer int64) {
	c.mu.RLock()
	cb := c.onConfigChanged
	c.mu.RUnlock()
	if cb == nil {
		c.logger.Warn("Config changed but no OnConfigChanged callback registered",
			slog.String("event", "config_no_callback"),
			slog.Int64("desired_ver", desiredVer),
		)
		return
	}

	currentReported := c.reportedVer.Load()
	if desiredVer <= currentReported {
		c.logger.Info("Config already applied, skipping",
			slog.String("event", "config_already_applied"),
			slog.Int64("desired_ver", desiredVer),
			slog.Int64("reported_ver", currentReported),
		)
		return
	}

	c.logger.Info("Applying config",
		slog.String("event", "config_applying"),
		slog.Int64("desired_ver", desiredVer),
		slog.Int64("reported_ver", currentReported),
		slog.Int("config_keys", len(desired)),
	)

	reported, err := cb(desired)
	if err != nil {
		c.logger.Error("Config apply callback failed",
			slog.String("event", "config_apply_failed"),
			slog.Int64("desired_ver", desiredVer),
			slog.String("error", err.Error()),
		)
		c.promMetrics.configApplies.WithLabelValues("failure").Inc()
		return
	}

	c.promMetrics.configApplies.WithLabelValues("success").Inc()

	if err := c.sendShadowReport(reported, desiredVer); err != nil {
		c.logger.Error("Shadow report after config apply failed",
			slog.String("event", "shadow_report_after_apply_failed"),
			slog.Int64("desired_ver", desiredVer),
			slog.String("error", err.Error()),
		)
		return
	}
	c.reportedVer.Store(desiredVer)

	c.logger.Info("Config applied successfully",
		slog.String("event", "config_applied"),
		slog.Int64("reported_ver", desiredVer),
	)
}

// applyConfigForce is applyConfig's twin for admin-triggered re-sync replays.
// It intentionally bypasses the `desiredVer <= reportedVer` short-circuit so
// that an operator can force a re-run of OnConfigChanged and a fresh
// shadow_report at the *same* version the Thing already reported — used to
// repair cases where the Hub's thing.Reported content drifted from what the
// Thing is actually running (for example, an initial deploy where the seeded
// reported state is null/version:0 even though the Thing already applied the
// desired template).
//
// The callback-error branch still leaves reportedVer untouched — a failed
// re-sync does not invent a success. If the callback is not registered we
// log and return without touching state, mirroring applyConfig.
func (c *Client) applyConfigForce(desired map[string]ConfigState, desiredVer int64) {
	c.mu.RLock()
	cb := c.onConfigChanged
	c.mu.RUnlock()
	if cb == nil {
		c.logger.Warn("Forced re-sync received but no OnConfigChanged callback registered",
			slog.String("event", "config_no_callback"),
			slog.Int64("desired_ver", desiredVer),
		)
		return
	}

	currentReported := c.reportedVer.Load()
	c.logger.Info("Applying config (forced re-sync)",
		slog.String("event", "config_applying"),
		slog.Bool("forced", true),
		slog.Int64("desired_ver", desiredVer),
		slog.Int64("reported_ver", currentReported),
		slog.Int("config_keys", len(desired)),
	)

	reported, err := cb(desired)
	if err != nil {
		c.logger.Error("Forced re-sync callback failed",
			slog.String("event", "config_apply_failed"),
			slog.Bool("forced", true),
			slog.Int64("desired_ver", desiredVer),
			slog.String("error", err.Error()),
		)
		c.promMetrics.configApplies.WithLabelValues("failure").Inc()
		return
	}

	c.promMetrics.configApplies.WithLabelValues("success").Inc()

	if err := c.sendShadowReport(reported, desiredVer); err != nil {
		c.logger.Error("Shadow report after forced re-sync failed",
			slog.String("event", "shadow_report_after_apply_failed"),
			slog.Bool("forced", true),
			slog.Int64("desired_ver", desiredVer),
			slog.String("error", err.Error()),
		)
		return
	}
	c.reportedVer.Store(desiredVer)

	c.logger.Info("Config applied (forced re-sync)",
		slog.String("event", "config_applied"),
		slog.Bool("forced", true),
		slog.Int64("reported_ver", desiredVer),
	)
}

// sendShadowReport sends the reported state to Hub via the currently active channel.
func (c *Client) sendShadowReport(reported map[string]ConfigState, ver int64) error {
	c.mu.RLock()
	mode := c.mode
	c.mu.RUnlock()

	switch mode {
	case ModeWSConnected:
		if err := c.sendShadowReportWS(reported, ver); err != nil {
			c.promMetrics.shadowReports.WithLabelValues("failure").Inc()
			return err
		}
	case ModeHTTPFallback:
		if err := c.sendShadowReportHTTP(reported, ver); err != nil {
			c.promMetrics.shadowReports.WithLabelValues("failure").Inc()
			return err
		}
	default:
		c.logger.Warn("Cannot send shadow report: not connected",
			slog.String("event", "shadow_report_no_connection"),
			slog.String("mode", mode.String()),
		)
		c.promMetrics.shadowReports.WithLabelValues("failure").Inc()
		return fmt.Errorf("thingclient: cannot send shadow report, mode=%s", mode.String())
	}
	c.promMetrics.shadowReports.WithLabelValues("success").Inc()
	return nil
}

// flattenReported converts the client-side ConfigState map into the flat
// raw-state wire format used by shadow_report messages. Per-key raw state
// matches the shape Hub stores for thing.desired, so sending the flat form
// keeps thing.reported[key] byte-comparable with thing.desired[key] in the
// Hub's per-key diff. Keys with nil State are skipped; per-key version
// lives on ReportedVer / KeyVersions and is deliberately dropped here.
func flattenReported(reported map[string]ConfigState) map[string]json.RawMessage {
	// Always return a non-nil map so JSON encodes `"reported":{}` instead of
	// `"reported":null`, which Hub rejects (POST /api/internal/things/shadow).
	out := make(map[string]json.RawMessage, len(reported))
	if len(reported) == 0 {
		return out
	}
	for k, cs := range reported {
		if len(cs.State) == 0 {
			continue
		}
		out[k] = cs.State
	}
	return out
}

// sendShadowReportWS sends a shadow_report message via WebSocket.
func (c *Client) sendShadowReportWS(reported map[string]ConfigState, ver int64) error {
	msg := thingMessage{
		Type:             "shadow_report",
		Reported:         flattenReported(reported),
		ReportedVer:      ver,
		ReportedOutcomes: c.outcomes.Snapshot(),
	}

	if err := c.sendMessage(msg); err != nil {
		return err
	}

	now := time.Now().UTC()
	c.lastReportedAt.Store(now.Format(time.RFC3339))
	c.lastReportedAtNanos.Store(now.UnixNano())
	c.logger.Info("Shadow report sent via WebSocket",
		slog.String("event", "shadow_reported"),
		slog.String("mode", "ws"),
		slog.Int64("reported_ver", ver),
		slog.Int("reported_keys", len(reported)),
	)
	return nil
}

// sendShadowReportHTTP sends a shadow report via HTTP POST.
func (c *Client) sendShadowReportHTTP(reported map[string]ConfigState, ver int64) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := c.httpShadowReport(ctx, reported, ver)
	if err != nil {
		c.logger.Error("Shadow report via HTTP failed",
			slog.String("event", "shadow_report_failed"),
			slog.String("mode", "http"),
			slog.Int64("reported_ver", ver),
			slog.String("error", err.Error()),
		)
		return err
	}

	now := time.Now().UTC()
	c.lastReportedAt.Store(now.Format(time.RFC3339))
	c.lastReportedAtNanos.Store(now.UnixNano())
	c.logger.Info("Shadow report sent via HTTP",
		slog.String("event", "shadow_reported"),
		slog.String("mode", "http"),
		slog.Int64("reported_ver", ver),
		slog.Int("reported_keys", len(reported)),
	)
	return nil
}

// DesiredVer returns the last known desired config version from Hub.
func (c *Client) DesiredVer() int64 {
	return c.desiredVer.Load()
}

// SnapshotDesired returns a copy of the last-applied desired-state map. The
// copy is safe for the caller to read concurrently with further shadow
// updates; the underlying ConfigState.State payloads are json.RawMessage
// values and must be treated as read-only (do not mutate the byte slice).
func (c *Client) SnapshotDesired() map[string]ConfigState {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]ConfigState, len(c.desiredCache))
	for k, v := range c.desiredCache {
		out[k] = v
	}
	return out
}

// ReportedVer returns the last reported config version sent to Hub.
func (c *Client) ReportedVer() int64 {
	return c.reportedVer.Load()
}

// InSync returns true if the reported version matches the desired version.
func (c *Client) InSync() bool {
	return c.reportedVer.Load() >= c.desiredVer.Load()
}

// KeyVersion returns the version last observed from Hub for the given
// config_key, or 0 when the key has not been seen. Uses sync.Map for
// lock-free reads on the runtime API hot path.
func (c *Client) KeyVersion(key string) int64 {
	v, ok := c.perKeyVersion.Load(key)
	if !ok {
		return 0
	}
	ver, ok := v.(int64)
	if !ok {
		return 0
	}
	return ver
}

// LastReportedAt returns the RFC3339 timestamp of the most recent
// successful shadow_report, or the empty string when no report has been
// sent since the process started.
func (c *Client) LastReportedAt() string {
	v := c.lastReportedAt.Load()
	if v == nil {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}

// LastReportedAtTime returns the wall-clock time of the most recent
// successful shadow_report, or the zero time when no report has been sent
// since the process started. Prefer this over LastReportedAt when the
// caller needs to compute an age (e.g. health probes, staleness metrics).
func (c *Client) LastReportedAtTime() time.Time {
	nanos := c.lastReportedAtNanos.Load()
	if nanos == 0 {
		return time.Time{}
	}
	return time.Unix(0, nanos).UTC()
}

// HeartbeatInterval returns the configured HTTP-fallback heartbeat cadence.
// Callers (health probes, staleness gauges) use this as the base unit for
// "how old is too old" thresholds.
func (c *Client) HeartbeatInterval() time.Duration {
	return c.cfg.HeartbeatInterval
}
