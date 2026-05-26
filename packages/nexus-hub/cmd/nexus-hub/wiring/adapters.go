package wiring

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	alerting "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine/rules"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine/senders"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/observability/opsmetrics"
	sharedops "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillupload"
)

// SenderRegAdapter wraps *senders.Registry and satisfies alerting.SenderRegistry.
// The alerting package declares its own Sender / SenderRegistry interfaces to
// break the import cycle that would otherwise form with the senders subpackage
// (which already imports alerting for Alert and Channel types). The two Sender
// interfaces share a method set but are distinct Go types.
type SenderRegAdapter struct{ R *senders.Registry }

func (a SenderRegAdapter) Get(channelType string) (alerting.Sender, error) {
	s, err := a.R.Get(channelType)
	if err != nil {
		return nil, err
	}
	return senderShim{s}, nil
}

// senderShim wraps a senders.Sender so it satisfies alerting.Sender.
type senderShim struct{ s senders.Sender }

func (w senderShim) Send(ctx context.Context, ch alerting.Channel, a alerting.Alert) (int, error) {
	return w.s.Send(ctx, ch, a)
}

// RulesRegAdapter wraps *rules.Registry and satisfies alerting.RuleRegistry.
// The alerting package cannot import rules (rules already imports alerting for
// Severity), so the adapter lives here where both packages are in scope.
type RulesRegAdapter struct{ R *rules.Registry }

func (a RulesRegAdapter) Lookup(id string) (alerting.RuleDefault, bool) {
	d, ok := a.R.Lookup(id)
	if !ok {
		return alerting.RuleDefault{}, false
	}
	return alerting.RuleDefault{
		ID:              d.ID,
		DisplayName:     d.DisplayName,
		DefaultSeverity: d.DefaultSeverity,
		RequiresAck:     d.RequiresAck,
		Enabled:         d.Enabled,
		CooldownSec:     d.CooldownSec,
		Params:          d.Params,
		ParamsSchema:    d.ParamsSchema,
	}, true
}

// hubMetaQuerier is the narrow pool interface hubMetadataAdapter uses.
// *pgxpool.Pool satisfies it; tests may inject a pgxmock via newHubMetadataAdapterWithQuerier.
type hubMetaQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// hubMetadataAdapter satisfies spillupload.MetadataStore against Hub's pgx
// pool. Lives here rather than internal/store to avoid a dependency cycle and
// to keep the spill-upload bootstrap flow isolated from the Cat B loader code.
type hubMetadataAdapter struct {
	pool hubMetaQuerier
}

func newHubMetadataAdapter(pool *pgxpool.Pool) *hubMetadataAdapter {
	return &hubMetadataAdapter{pool: pool}
}

// newHubMetadataAdapterWithQuerier is the test seam that accepts any hubMetaQuerier.
// Production code calls newHubMetadataAdapter with a *pgxpool.Pool.
func newHubMetadataAdapterWithQuerier(q hubMetaQuerier) *hubMetadataAdapter {
	return &hubMetadataAdapter{pool: q}
}

func (a *hubMetadataAdapter) GetSystemMetadata(ctx context.Context, key string) ([]byte, error) {
	if a == nil || a.pool == nil {
		return nil, errors.New("hub: nil dbPool")
	}
	row := a.pool.QueryRow(ctx, `SELECT value FROM system_metadata WHERE key = $1`, key)
	var raw []byte
	if err := row.Scan(&raw); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("hub: select system_metadata[%s]: %w", key, err)
	}
	return raw, nil
}

func (a *hubMetadataAdapter) SetSystemMetadata(ctx context.Context, key string, value any, updatedBy string) error {
	if a == nil || a.pool == nil {
		return errors.New("hub: nil dbPool")
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("hub: marshal system_metadata[%s]: %w", key, err)
	}
	// Upsert: create the row on first boot, replace on rotation.
	// updated_at flips automatically via the schema default.
	_, err = a.pool.Exec(ctx, `
		INSERT INTO system_metadata (key, value, updated_by, updated_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (key) DO UPDATE SET
		  value      = EXCLUDED.value,
		  updated_by = EXCLUDED.updated_by,
		  updated_at = NOW()
	`, key, raw, updatedBy)
	if err != nil {
		return fmt.Errorf("hub: upsert system_metadata[%s]: %w", key, err)
	}
	return nil
}

// Verify hubMetadataAdapter satisfies spillupload.MetadataStore.
var _ spillupload.MetadataStore = (*hubMetadataAdapter)(nil)

// HubDiagAdapter wraps DiagWriterImpl so it satisfies shareddiag.ThingClientPusher.
// Forwards PushDiagEvent calls to DiagWriterImpl.Enqueue so the Hub SlogSink
// can capture Hub's own ERROR+ slog records without a loopback thingclient.
type HubDiagAdapter struct {
	ThingID   string
	ThingType string
	Writer    *opsmetrics.DiagWriterImpl
}

func (h *HubDiagAdapter) PushDiagEvent(ctx context.Context, evt sharedops.DiagEvent) error {
	evt.ThingID = h.ThingID
	return h.Writer.Enqueue(ctx, h.ThingID, h.ThingType, evt)
}

