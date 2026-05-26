package manager

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
)

// computeTrustLevel derives the trust_level integer (0–3) for a device based
// on its certificate validity, online status, user assignment, and version.
//
// Rules:
//
//	0 — certificate expired or thing is revoked (not trustworthy).
//	1 — valid certificate + enrolled online, but no active user assignment.
//	2 — level 1 + active DeviceAssignment (user identified).
//	3 — level 2 + agent version >= minVersion (fully compliant). When minVersion
//	    is empty the version check is skipped and level 2 upgrades directly to 3.
func computeTrustLevel(thingStatus string, agent *store.ThingAgentRecord, hasActiveAssignment bool, minVersion string) int {
	// Level 0: device is administratively revoked.
	if thingStatus == "revoked" {
		return 0
	}
	// Level 0: agent TLS certificate has expired.
	if agent.CertExpiresAt != nil && agent.CertExpiresAt.Before(time.Now()) {
		return 0
	}

	// Level 1: enrolled + certificate valid, but no user linked.
	if !hasActiveAssignment {
		return 1
	}

	// Level 2 / 3: user is linked; check version compliance.
	if minVersion == "" {
		return 3 // no minimum configured → skip version check, grant level 3
	}
	if versionAtLeast(agent.Version, minVersion) {
		return 3
	}
	return 2
}

// versionAtLeast returns true when version >= min using a simple token-by-token
// comparison of the semver-like "vMAJOR.MINOR.PATCH" strings. Strips a leading
// "v" prefix before splitting. Non-numeric tokens fall back to string comparison.
// An empty or unparseable version is treated as less than any min.
func versionAtLeast(version, min string) bool {
	vParts := splitVersion(strings.TrimPrefix(version, "v"))
	mParts := splitVersion(strings.TrimPrefix(min, "v"))

	maxLen := len(vParts)
	if len(mParts) > maxLen {
		maxLen = len(mParts)
	}

	for i := range maxLen {
		v := 0
		m := 0
		if i < len(vParts) {
			_, _ = fmt.Sscanf(vParts[i], "%d", &v)
		}
		if i < len(mParts) {
			_, _ = fmt.Sscanf(mParts[i], "%d", &m)
		}
		if v < m {
			return false
		}
		if v > m {
			return true
		}
	}
	return true // equal
}

func splitVersion(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ".")
}

// ComputeAndStoreTrustLevel runs the same trust-level computation
// updateTrustLevel performs, persists the result, and returns the
// computed level to the caller. The enrollment handler uses this to
// include trust_level in the response so the agent can surface its
// own current trust level via the local status IPC without a
// round-trip to Hub.
//
// Returns 0 if the lookup or computation fails (treated as "not
// trustworthy" by downstream consumers). Errors are logged at Warn.
func (m *Manager) ComputeAndStoreTrustLevel(ctx context.Context, thingID, thingStatus, minVersion string) int {
	agent, err := m.store.EnrollStore().GetThingAgentForTrustLevel(ctx, thingID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return 0
		}
		m.logger.Warn("trust level: get thing agent failed",
			slog.String("thing_id", thingID), slog.Any("err", err))
		return 0
	}
	hasAssignment, err := m.store.EnrollStore().HasActiveDeviceAssignment(ctx, thingID)
	if err != nil {
		m.logger.Warn("trust level: check active assignment failed",
			slog.String("thing_id", thingID), slog.Any("err", err))
		return 0
	}
	level := computeTrustLevel(thingStatus, agent, hasAssignment, minVersion)
	if err := m.store.EnrollStore().UpdateThingAgentTrustLevel(ctx, thingID, level); err != nil {
		m.logger.Warn("trust level: update failed",
			slog.String("thing_id", thingID), slog.Int("level", level), slog.Any("err", err))
	}
	m.cacheShadow(ctx, thingID, map[string]any{"trust_level": level})
	return level
}

// updateTrustLevel computes and persists trust_level for an agent-type Thing
// immediately after a heartbeat is processed. It also merges the new
// trust_level into the reported shadow in Redis so the next config-sync
// round-trip carries the updated value.
//
// Failures are logged and swallowed: a trust_level write failure must not
// cause the heartbeat response to fail, since the agent may be in a degraded
// network state and losing the heartbeat ACK would worsen its situation.
func (m *Manager) updateTrustLevel(ctx context.Context, thingID, thingStatus, minVersion string) {
	agent, err := m.store.EnrollStore().GetThingAgentForTrustLevel(ctx, thingID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Not an agent-type Thing (no thing_agent row) — skip silently.
			return
		}
		m.logger.Warn("trust level: get thing agent failed",
			slog.String("thing_id", thingID), slog.Any("err", err))
		return
	}

	hasAssignment, err := m.store.EnrollStore().HasActiveDeviceAssignment(ctx, thingID)
	if err != nil {
		m.logger.Warn("trust level: check active assignment failed",
			slog.String("thing_id", thingID), slog.Any("err", err))
		return
	}

	level := computeTrustLevel(thingStatus, agent, hasAssignment, minVersion)

	if err := m.store.EnrollStore().UpdateThingAgentTrustLevel(ctx, thingID, level); err != nil {
		m.logger.Warn("trust level: update failed",
			slog.String("thing_id", thingID), slog.Int("level", level), slog.Any("err", err))
		return
	}

	// Merge trust_level into the Redis-cached reported shadow so consumers that
	// read the cache (e.g. compliance-proxy, config-sync diff) see the fresh value
	// without waiting for the next explicit shadow report from the agent.
	m.cacheShadow(ctx, thingID, map[string]any{"trust_level": level})

	m.logger.Debug("trust level updated",
		slog.String("thing_id", thingID),
		slog.Int("level", level),
		slog.Bool("has_assignment", hasAssignment))
}
