package thingstore

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/configreconcile"
)

// QueryThingDesired returns the per-thing desired-state JSON for one config
// key, scoped to one thing_type. Used by the configreconcile job to compare
// CP source-of-truth against gateway-side desired state. Only online things
// are inspected — offline things will pick up the new desired on reconnect.
//
// The returned DesiredJSON is the JSON value at thing.desired -> configKey
// (i.e. NOT the whole desired blob — the helper applies the JSON path
// extraction in the SQL query for cheap-ness).
func (store *Store) QueryThingDesired(ctx context.Context, thingType, configKey string) ([]configreconcile.ThingDesiredRow, error) {
	rows, err := store.pool.Query(ctx, `
		SELECT id, type, COALESCE(desired -> $2, '{}'::jsonb)
		FROM thing
		WHERE ($1 = '' OR type = $1)
		  AND status = 'online'
	`, thingType, configKey)
	if err != nil {
		return nil, fmt.Errorf("query thing desired: %w", err)
	}
	defer rows.Close()

	var out []configreconcile.ThingDesiredRow
	for rows.Next() {
		var r configreconcile.ThingDesiredRow
		var raw []byte
		if err := rows.Scan(&r.ThingID, &r.ThingType, &raw); err != nil {
			return nil, fmt.Errorf("scan thing desired row: %w", err)
		}
		r.DesiredJSON = json.RawMessage(raw)
		out = append(out, r)
	}
	return out, rows.Err()
}
