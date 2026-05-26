// Package selfreg manages the Hub's own entry in the thing table.
// Unlike other Things that register via WebSocket or HTTP, Hub performs
// a direct DB write since it cannot call its own endpoints.
package selfreg

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
)

// ThingType is the canonical `thing.type` value Hub uses for its own row.
// Exported so the metrics-sample tick in cmd/nexus-hub/main.go writes the
// same label into metric_ops_raw.thing_type, instead of disagreeing with
// the registry.
const ThingType = "nexus-hub"

// heartbeatInterval is the default cadence for self-heartbeat. Declared as a
// var so tests can lower it; production is unaffected.
var heartbeatInterval = 15 * time.Second

// thingStore is the subset of *store.Store that selfreg depends on.
// Declared as an interface so tests can supply a fake without standing up
// Postgres; *store.Store satisfies it structurally.
type thingStore interface {
	UpsertThingEnrollment(ctx context.Context, p store.UpsertThingParams) error
	UpdateLastSeen(ctx context.Context, id string) error
	UpdateThingStatus(ctx context.Context, id, status string) error
}

// Config holds self-registration configuration.
type Config struct {
	InstanceID       string
	Address          string
	MetricsURL       string
	Version          string
	SchedulerEnabled bool
	// PhysicalID is the optional operator-supplied stable id. Written
	// into thing.physical_id so ops have a stable handle independent of
	// the auto-derived InstanceID (typically hostname+port). Empty is
	// fine — services keep physical_id NULL by default.
	PhysicalID string
}

// SelfRegistrar manages the Hub's own entry in the thing table.
type SelfRegistrar struct {
	cfg    Config
	store  thingStore
	logger *slog.Logger

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New creates a SelfRegistrar.
func New(cfg Config, st *store.Store, logger *slog.Logger) *SelfRegistrar {
	r := &SelfRegistrar{
		cfg:    cfg,
		logger: logger.With("component", "selfreg"),
	}
	if st != nil {
		r.store = st.RegistryStore()
	}
	return r
}

// Register writes the Hub's entry to the thing table and starts the heartbeat loop.
func (s *SelfRegistrar) Register(ctx context.Context) error {
	if err := s.doUpsert(ctx); err != nil {
		return fmt.Errorf("self-registration failed: %w", err)
	}

	s.logger.Info("hub self-registered",
		"id", s.cfg.InstanceID,
		"role", s.role(),
		"address", s.cfg.Address,
	)

	hbCtx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel

	s.wg.Add(1)
	go s.heartbeatLoop(hbCtx)

	return nil
}

// doUpsert writes the Hub's row to the registry. Shared by Register and the
// heartbeat self-heal path so both go through the same params and SQL.
func (s *SelfRegistrar) doUpsert(ctx context.Context) error {
	hostname, _ := os.Hostname()
	metadata := map[string]any{
		"hostname":         hostname,
		"schedulerEnabled": s.cfg.SchedulerEnabled,
		"pid":              os.Getpid(),
		"role":             s.role(),
		"metricsUrl":       s.cfg.MetricsURL,
	}

	return s.store.UpsertThingEnrollment(ctx, store.UpsertThingParams{
		ID:           s.cfg.InstanceID,
		Type:         ThingType,
		Name:         hostname,
		Version:      s.cfg.Version,
		Address:      s.cfg.Address,
		EnrolledBy:   "system",
		AuthType:     "bearer",
		ConnProtocol: "http",
		Status:       "online",
		Metadata:     metadata,
		PhysicalID:   s.cfg.PhysicalID,
	})
}

func (s *SelfRegistrar) heartbeatLoop(ctx context.Context) {
	defer s.wg.Done()
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			err := s.store.UpdateLastSeen(ctx, s.cfg.InstanceID)
			if err == nil {
				continue
			}
			// Self-heal: if the thing row was pruned mid-run (e.g. by a dev
			// `prisma migrate reset` or manual SQL), recreate it. Without
			// this, metric_ops_raw FK keeps failing every 15s and Hub
			// metrics never land. See docs/developers/specs/e31/e31-s6-hub-selfreg-self-heal.md.
			if errors.Is(err, store.ErrNotFound) {
				if upsertErr := s.doUpsert(ctx); upsertErr == nil {
					s.logger.Info("hub re-registered after thing row was missing",
						"id", s.cfg.InstanceID,
						"role", s.role(),
					)
					continue
				} else {
					s.logger.Warn("hub re-registration failed",
						"id", s.cfg.InstanceID,
						"error", upsertErr,
					)
					continue
				}
			}
			s.logger.Warn("self-heartbeat failed", "id", s.cfg.InstanceID, "error", err)
		}
	}
}

// Deregister marks the Hub as offline and stops the heartbeat. Safe to call multiple times.
func (s *SelfRegistrar) Deregister(ctx context.Context) error {
	if s.cancel != nil {
		s.cancel()
		s.wg.Wait()
		s.cancel = nil
	}

	err := s.store.UpdateThingStatus(ctx, s.cfg.InstanceID, "offline")
	if err != nil {
		return fmt.Errorf("self-deregistration failed: %w", err)
	}

	s.logger.Info("hub deregistered", "id", s.cfg.InstanceID)
	return nil
}

func (s *SelfRegistrar) role() string {
	if s.cfg.SchedulerEnabled {
		return "scheduler"
	}
	return "default"
}
