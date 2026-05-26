package manager

import (
	"context"
	"fmt"
	"log/slog"
)

// HeartbeatRequest is the input for a Thing heartbeat.
//
// IPAddress is the request RealIP at the moment the heartbeat reached the
// Hub. It is NOT taken from the body — the Hub handler backfills it from
// c.RealIP() before invoking HandleHeartbeat, mirroring the ambient-audit
// pattern used by ConfigUpdate's ActorID/SourceIP. Used to keep
// DeviceAssignment.ip_address fresh so the IdentityEnricher job's
// ip_address-based user resolution stays current as users move networks.
type HeartbeatRequest struct {
	ID          string         `json:"id"`
	Status      string         `json:"status"`
	ReportedVer int64          `json:"reportedVer"`
	Metadata    map[string]any `json:"metadata"`
	IPAddress   string         `json:"-"`
}

// HeartbeatResponse is returned from a heartbeat.
type HeartbeatResponse struct {
	Ack        bool           `json:"ack"`
	DesiredVer int64          `json:"desiredVer"`
	Desired    map[string]any `json:"desired,omitempty"`
}

// HandleHeartbeat processes a Thing heartbeat: updates last_seen and returns
// desired config if the Thing's reported version is behind.
//
// After the core shadow update, it asynchronously recomputes trust_level for
// agent-type Things so the field stays current without blocking the heartbeat
// response. Non-agent Things (no thing_agent row) skip the trust computation
// silently.
func (m *Manager) HandleHeartbeat(ctx context.Context, req HeartbeatRequest) (*HeartbeatResponse, error) {
	result, err := m.store.RegistryStore().Heartbeat(ctx, req.ID, req.Status, req.Metadata, req.ReportedVer)
	if err != nil {
		return nil, fmt.Errorf("heartbeat %s: %w", req.ID, err)
	}

	// Recompute trust_level asynchronously so the DB round-trips (thing_agent
	// lookup + DeviceAssignment existence check + UPDATE) do not add latency to
	// the heartbeat response path. Fire-and-forget: failures are logged inside
	// updateTrustLevel.
	thingID := req.ID
	thingStatus := req.Status
	ipAddress := req.IPAddress
	go m.updateTrustLevel(context.Background(), thingID, thingStatus, "")

	// Refresh DeviceAssignment.ip_address from the heartbeat's egress IP
	// when it differs from the value last stamped. Without this the
	// IdentityEnricher job can never match users behind dynamic NATs — the
	// ip_address column stays frozen at enrollment time. Fire-and-forget;
	// failures are logged inside refreshDeviceAssignmentIP.
	if ipAddress != "" {
		go m.refreshDeviceAssignmentIP(context.Background(), thingID, ipAddress)
	}

	return &HeartbeatResponse{
		Ack:        true,
		DesiredVer: result.DesiredVer,
		Desired:    result.Desired,
	}, nil
}

// refreshDeviceAssignmentIP keeps DeviceAssignment.ip_address in sync
// with the agent's current egress IP so the IdentityEnricher job can
// resolve users on traffic_event rows from compliance-proxy / agent
// passthrough that only carry source_ip. Fire-and-forget from the
// heartbeat goroutine — Warn on failure, never block the heartbeat
// response. No-op for non-agent Things (no active assignment row).
func (m *Manager) refreshDeviceAssignmentIP(ctx context.Context, thingID, newIP string) {
	changed, err := m.store.EnrollStore().RefreshActiveDeviceAssignmentIP(ctx, thingID, newIP)
	if err != nil {
		m.logger.Warn("refresh device assignment ip failed",
			slog.String("thing_id", thingID),
			slog.String("new_ip", newIP),
			slog.Any("err", err))
		return
	}
	if changed {
		m.logger.Debug("device assignment ip refreshed",
			slog.String("thing_id", thingID),
			slog.String("new_ip", newIP))
	}
}
