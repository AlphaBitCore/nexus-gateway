package wiring

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	alerting "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/diag/runtimeintrospect"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configkey"
)

// introspectDBPool is the narrow interface buildIntrospectReg needs from the
// pool. *pgxpool.Pool satisfies it; tests may inject a fake via pgxmock.
type introspectDBPool interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	PoolStats() pgxpoolStats
}

// introspectDBPoolAdapter wraps *pgxpool.Pool to satisfy introspectDBPool.
// Production code always passes the real pool via buildIntrospectReg.
type introspectDBPoolAdapter struct{ pool *pgxpool.Pool }

// pgxpool is already imported via db.go in the same package; no new import needed.

func (a introspectDBPoolAdapter) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return a.pool.Query(ctx, sql, args...)
}
func (a introspectDBPoolAdapter) PoolStats() pgxpoolStats {
	return pgxpoolStatSnapshot(a).PoolStats()
}

// buildIntrospectReg builds and populates the runtimeintrospect Registry with
// all Hub sources: config.flags, runtime.scheduler, runtime.db_pool,
// runtime.thing_registry, runtime.diag_mode_windows, runtime.consumer_manager,
// runtime.alerts.rules, runtime.alerts.channels, and the selfshadow config keys.
func buildIntrospectReg(ec EchoConfig) *runtimeintrospect.Registry {
	var dbPool introspectDBPool
	if ec.DBPool != nil {
		dbPool = introspectDBPoolAdapter{pool: ec.DBPool}
	}
	return buildIntrospectRegWithDB(ec, dbPool)
}

func buildIntrospectRegWithDB(ec EchoConfig, dbPool introspectDBPool) *runtimeintrospect.Registry {
	introspectReg := runtimeintrospect.New("nexus-hub", ec.Cfg.Hub.ID, ec.BuildVersion)
	introspectReg.Register(runtimeintrospect.SourceFunc{
		SourceName: "config.flags",
		Fn: func(_ context.Context) (any, error) {
			return map[string]any{
				"hub_id":            ec.Cfg.Hub.ID,
				"advertise_addr":    ec.Cfg.Hub.AdvertiseAddr,
				"scheduler_enabled": ec.Cfg.Scheduler.Enabled,
				"build_version":     ec.BuildVersion,
			}, nil
		},
	})

	if ec.Sched != nil {
		introspectReg.Register(runtimeintrospect.SourceFunc{
			SourceName: "runtime.scheduler",
			Fn: func(ctx context.Context) (any, error) {
				return ec.Sched.ListJobs(ctx)
			},
		})
	}

	if dbPool != nil {
		introspectReg.Register(runtimeintrospect.SourceFunc{
			SourceName: "runtime.db_pool",
			Fn: func(_ context.Context) (any, error) {
				s := dbPool.PoolStats()
				return map[string]any{
					"acquired_conns": s.AcquiredConns,
					"idle_conns":     s.IdleConns,
					"total_conns":    s.TotalConns,
					"max_conns":      s.MaxConns,
				}, nil
			},
		})
		introspectReg.Register(runtimeintrospect.SourceFunc{
			SourceName: "runtime.thing_registry",
			Fn: func(ctx context.Context) (any, error) {
				rows, err := dbPool.Query(ctx, `
					SELECT type, status, COUNT(*)
					FROM thing
					GROUP BY type, status
					ORDER BY type, status
				`)
				if err != nil {
					return nil, err
				}
				defer rows.Close()
				type row struct {
					Type   string `json:"type"`
					Status string `json:"status"`
					Count  int    `json:"count"`
				}
				out := make([]row, 0, 8)
				for rows.Next() {
					var r row
					if err := rows.Scan(&r.Type, &r.Status, &r.Count); err != nil {
						return nil, err
					}
					out = append(out, r)
				}
				return out, rows.Err()
			},
		})
		introspectReg.Register(runtimeintrospect.SourceFunc{
			SourceName: "runtime.diag_mode_windows",
			Fn: func(ctx context.Context) (any, error) {
				rows, err := dbPool.Query(ctx, `
					SELECT id, thing_id, started_at, ended_at, set_by, reason
					FROM thing_diag_mode_window
					WHERE ended_at > NOW()
					ORDER BY started_at DESC
				`)
				if err != nil {
					return nil, err
				}
				defer rows.Close()
				type window struct {
					ID        string  `json:"id"`
					ThingID   string  `json:"thing_id"`
					StartedAt string  `json:"started_at"`
					EndedAt   string  `json:"ended_at"`
					SetBy     *string `json:"set_by,omitempty"`
					Reason    *string `json:"reason,omitempty"`
				}
				out := make([]window, 0, 4)
				for rows.Next() {
					var w window
					var startedAt, endedAt time.Time
					if err := rows.Scan(&w.ID, &w.ThingID, &startedAt, &endedAt, &w.SetBy, &w.Reason); err != nil {
						return nil, err
					}
					w.StartedAt = startedAt.UTC().Format(time.RFC3339)
					w.EndedAt = endedAt.UTC().Format(time.RFC3339)
					out = append(out, w)
				}
				return out, rows.Err()
			},
		})
	}

	if ec.ConsumerMgr != nil {
		introspectReg.Register(runtimeintrospect.SourceFunc{
			SourceName: "runtime.consumer_manager",
			Fn: func(_ context.Context) (any, error) {
				return map[string]any{"running": true}, nil
			},
		})
	}

	if ec.AlertStore != nil {
		introspectReg.Register(runtimeintrospect.SourceFunc{
			SourceName: "runtime.alerts.rules",
			Fn: func(ctx context.Context) (any, error) {
				rules, _, err := ec.AlertStore.ListRules(ctx, alerting.ListRulesParams{Limit: 1000})
				if err != nil {
					return nil, err
				}
				type ruleSummary struct {
					ID              string `json:"id"`
					DisplayName     string `json:"displayName"`
					SourceType      string `json:"sourceType"`
					DefaultSeverity string `json:"defaultSeverity"`
					Enabled         bool   `json:"enabled"`
					RequiresAck     bool   `json:"requiresAck"`
					CooldownSec     int    `json:"cooldownSec"`
				}
				out := make([]ruleSummary, 0, len(rules))
				for _, r := range rules {
					out = append(out, ruleSummary{
						ID:              r.ID,
						DisplayName:     r.DisplayName,
						SourceType:      r.SourceType,
						DefaultSeverity: string(r.DefaultSeverity),
						Enabled:         r.Enabled,
						RequiresAck:     r.RequiresAck,
						CooldownSec:     r.CooldownSec,
					})
				}
				return out, nil
			},
		})
		introspectReg.Register(runtimeintrospect.SourceFunc{
			SourceName: "runtime.alerts.channels",
			Fn: func(ctx context.Context) (any, error) {
				channels, err := ec.AlertStore.ListChannels(ctx)
				if err != nil {
					return nil, err
				}
				type channelSummary struct {
					ID          string   `json:"id"`
					Name        string   `json:"name"`
					Type        string   `json:"type"`
					Enabled     bool     `json:"enabled"`
					Severities  []string `json:"severities"`
					SourceTypes []string `json:"sourceTypes"`
					ConfigKeys  []string `json:"configKeys"`
				}
				out := make([]channelSummary, 0, len(channels))
				for _, ch := range channels {
					keys := make([]string, 0, len(ch.Config))
					for k := range ch.Config {
						keys = append(keys, k)
					}
					// ch.Severities is []alerting.Severity (typed enum);
					// stringify for the introspect snapshot so external
					// scrapers see plain JSON strings.
					sevs := make([]string, len(ch.Severities))
					for i, s := range ch.Severities {
						sevs[i] = s.String()
					}
					out = append(out, channelSummary{
						ID:          ch.ID,
						Name:        ch.Name,
						Type:        ch.Type,
						Enabled:     ch.Enabled,
						Severities:  sevs,
						SourceTypes: ch.SourceTypes,
						ConfigKeys:  keys,
					})
				}
				return out, nil
			},
		})
	}

	// Surface the keys the selfshadow Manager consumes for the Hub's own thing
	// row so the Runtime State tab carries a card per consumed key.
	ec.SelfShadow.ConfigKeyRecorder.RegisterAll(introspectReg, []string{
		configkey.LogLevel,
		configkey.Observability,
	})

	return introspectReg
}
