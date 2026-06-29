package thingclient

import (
	"context"
	"fmt"
	"github.com/goccy/go-json"
	"log/slog"
	"net/http"
	"time"
)

// breakGlassShadowRequest is the HTTP fallback body for a break-glass report.
// The shape mirrors the WebSocket thingMessage envelope fields populated for
// shadow_report_break_glass: standard shadow identity + the override audit
// context Hub needs to write the emergency_override row and queue the
// reconciliation job.
type breakGlassShadowRequest struct {
	ID string `json:"id"`
	// Reported is a map of configKey → raw state (no {state, version}
	// wrapper). See thingMessage.Reported for the rationale.
	Reported     map[string]json.RawMessage `json:"reported"`
	ReportedVer  int64                      `json:"reportedVer"`
	Reason       string                     `json:"reason,omitempty"`
	SourceIP     string                     `json:"sourceIp,omitempty"`
	ActorTokenID string                     `json:"actorTokenId,omitempty"`
	KeyVersions  map[string]int64           `json:"keyVersions,omitempty"`
}

// SendBreakGlassShadowReport pushes a break-glass shadow_report_break_glass
// envelope to Hub. The envelope carries the new state of the single
// config_key touched by the PUT handler plus the audit context (reason,
// source IP, actor token id, and the per-key version map needed by Hub to
// write the emergency_override row).
//
// Delivery is best-effort:
//   - WebSocket mode: queues via sendMessage (5s timeout, drop-on-stall)
//   - HTTP fallback mode: POST /api/internal/things/shadow/break-glass
//   - Disconnected: returns an error so the caller can spool the request to
//     the pending-buffer on disk and retry on reconnect
//
// Hub is authoritative — the local apply already happened before this call;
// a delivery failure does NOT roll back the local apply.
func (c *Client) SendBreakGlassShadowReport(
	ctx context.Context,
	key string,
	state json.RawMessage,
	keyVer int64,
	reason, sourceIP, actorTokenID string,
) error {
	if key == "" {
		return fmt.Errorf("thingclient: break-glass key is required")
	}
	// Send the raw state as-is on the wire. The old {state, version}
	// wrapper caused Hub-side thing.reported to diverge from thing.desired
	// and also leaked the wrapper into break-glass-driven config_template
	// upserts. KeyVersions below still carries the per-key version.
	reported := map[string]json.RawMessage{key: state}
	versions := map[string]int64{key: keyVer}

	c.mu.RLock()
	mode := c.mode
	c.mu.RUnlock()

	switch mode {
	case ModeWSConnected:
		if err := c.sendMessage(thingMessage{
			Type:         "shadow_report_break_glass",
			Reported:     reported,
			ReportedVer:  keyVer,
			Reason:       reason,
			SourceIP:     sourceIP,
			ActorTokenID: actorTokenID,
			KeyVersions:  versions,
		}); err != nil {
			return fmt.Errorf("break-glass shadow report: %w", err)
		}
		now := time.Now().UTC()
		c.lastReportedAt.Store(now.Format(time.RFC3339))
		c.lastReportedAtNanos.Store(now.UnixNano())
		c.logger.Info("break-glass shadow report sent via WebSocket",
			slog.String("event", "break_glass_reported"),
			slog.String("mode", "ws"),
			slog.String("config_key", key),
			slog.Int64("key_version", keyVer),
		)
		return nil

	case ModeHTTPFallback:
		req := breakGlassShadowRequest{
			ID:           c.cfg.ThingID,
			Reported:     reported,
			ReportedVer:  keyVer,
			Reason:       reason,
			SourceIP:     sourceIP,
			ActorTokenID: actorTokenID,
			KeyVersions:  versions,
		}
		hc := c.getHTTPClient()
		body, status, err := hc.do(ctx, http.MethodPost, "/api/internal/things/shadow/break-glass", req)
		if err != nil {
			return fmt.Errorf("break-glass shadow report: %w", err)
		}
		if status != http.StatusOK {
			return fmt.Errorf("break-glass shadow report: HTTP %d: %s", status, string(body))
		}
		now := time.Now().UTC()
		c.lastReportedAt.Store(now.Format(time.RFC3339))
		c.lastReportedAtNanos.Store(now.UnixNano())
		c.logger.Info("break-glass shadow report sent via HTTP",
			slog.String("event", "break_glass_reported"),
			slog.String("mode", "http"),
			slog.String("config_key", key),
			slog.Int64("key_version", keyVer),
		)
		return nil

	default:
		return fmt.Errorf("thingclient: not connected (mode=%s)", mode.String())
	}
}
