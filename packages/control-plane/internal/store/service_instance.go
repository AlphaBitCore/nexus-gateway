// Package store — service_instance.go
// Provides ThingService CRUD for non-agent service instances (ai-gateway,
// compliance-proxy, nexus-hub, control-plane). Data is stored in the
// thing + thing_service tables.
package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// ThingService represents a non-agent service instance backed by thing + thing_service.
type ThingService struct {
	InstanceID      string          `json:"instanceId"`
	Service         string          `json:"service"`
	Version         string          `json:"version"`
	Address         *string         `json:"address"`
	MetricsURL      *string         `json:"metricsUrl,omitempty"`
	Role            string          `json:"role"` // "api" or "scheduler"
	Status          string          `json:"status"`
	Uptime          *int            `json:"uptime"`
	Checks          json.RawMessage `json:"checks"`
	RegisteredAt    time.Time       `json:"registeredAt"`
	LastHeartbeatAt time.Time       `json:"lastHeartbeatAt"`
}

// ServiceSummary holds per-service aggregate counts by status.
type ServiceSummary struct {
	Service   string `json:"service"`
	Total     int    `json:"total"`
	Healthy   int    `json:"healthy"`
	Degraded  int    `json:"degraded"`
	Unhealthy int    `json:"unhealthy"`
	Offline   int    `json:"offline"`
}

// ListThingServices returns all non-agent service instances using thing + thing_service JOIN.
func (db *DB) ListThingServices(ctx context.Context) ([]ThingService, error) {
	query := `
		SELECT t.id, t.type, COALESCE(t.version, ''), t.address, t.status,
		       t.enrolled_at, t.last_seen_at, t.reported,
		       COALESCE(ts.role, 'default'), ts.metrics_url
		FROM thing t
		LEFT JOIN thing_service ts ON ts.thing_id = t.id
		WHERE t.type != 'agent'
		ORDER BY t.type ASC, t.status ASC, t.last_seen_at DESC NULLS LAST`

	rows, err := db.pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list service instances: %w", err)
	}
	defer rows.Close()

	var result []ThingService
	for rows.Next() {
		var (
			si       ThingService
			lastSeen *time.Time
			reported json.RawMessage
		)
		err := rows.Scan(
			&si.InstanceID, &si.Service, &si.Version, &si.Address, &si.Status,
			&si.RegisteredAt, &lastSeen, &reported,
			&si.Role, &si.MetricsURL,
		)
		if err != nil {
			return nil, fmt.Errorf("scan service instance: %w", err)
		}
		if lastSeen != nil {
			si.LastHeartbeatAt = *lastSeen
		}
		if len(reported) > 0 {
			var rm struct {
				Uptime *int            `json:"uptime"`
				Checks json.RawMessage `json:"checks"`
				Status string          `json:"status"`
			}
			if err := json.Unmarshal(reported, &rm); err == nil {
				si.Uptime = rm.Uptime
				si.Checks = rm.Checks
				if rm.Status != "" {
					si.Status = rm.Status
				}
			}
		}
		if si.Role == "" {
			si.Role = "default"
		}
		result = append(result, si)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate service instances: %w", err)
	}
	return result, nil
}

// GetServiceSummaries returns per-service (type) aggregate counts grouped by status.
func (db *DB) GetServiceSummaries(ctx context.Context) ([]ServiceSummary, error) {
	summaries, err := db.GetThingTypeSummaries(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]ServiceSummary, len(summaries))
	for i, s := range summaries {
		result[i] = ServiceSummary{
			Service:  s.Type,
			Total:    s.Total,
			Healthy:  s.Online,
			Offline:  s.Offline,
			Degraded: s.Drift,
		}
	}
	return result, nil
}
