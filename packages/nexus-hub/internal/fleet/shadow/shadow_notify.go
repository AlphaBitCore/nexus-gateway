// shadow_notify.go — postgres LISTEN/NOTIFY plumbing for Hub's
// self-shadow consumer (packages/nexus-hub/internal/self/shadow).
//
// Every code path that writes thing.desired MUST call
// notifyConfigChanged inside the same pgx.Tx before commit so the
// listener only ever observes committed state. The payload is the
// thing_id; each Hub instance LISTENs and filters by its own hub.id.
//
// Channel name lives here so both the writer side (this file) and the
// reader side (selfshadow.Channel) reference a single constant.
package shadow

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ConfigChangedChannel is the postgres LISTEN channel name shared
// between Hub's shadow-write paths and the Hub selfshadow consumer.
const ConfigChangedChannel = "config_changed"

// notifyConfigChanged emits pg_notify(ConfigChangedChannel, thingID)
// inside the supplied transaction. Postgres only delivers the
// notification on commit, so rollback discards it automatically — no
// listener will fire for a write that didn't land.
//
// Errors are wrapped with the channel name so failures are easy to
// recognise in logs. Callers should bubble the error up to the caller's
// transaction so the commit can be aborted (a stale-listener mismatch
// is worse than a failed write).
func notifyConfigChanged(ctx context.Context, tx pgx.Tx, thingID string) error {
	if _, err := tx.Exec(ctx, "SELECT pg_notify($1, $2)", ConfigChangedChannel, thingID); err != nil {
		return fmt.Errorf("pg_notify %s for %s: %w", ConfigChangedChannel, thingID, err)
	}
	return nil
}
