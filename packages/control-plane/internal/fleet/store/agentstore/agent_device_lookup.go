package agentstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ThingNodeInfo holds minimal node info attached by the mTLS middleware.
type ThingNodeInfo struct {
	ID         string
	Hostname   string
	Status     string
	CertSerial string
}

// LookupThingNodeByCertSerial returns the node matching the given certificate serial
// (current or previous), or nil if not found. Queries thing + thing_agent.
func (store *Store) LookupThingNodeByCertSerial(ctx context.Context, serial string) (*ThingNodeInfo, error) {
	row := store.pool.QueryRow(ctx, `
		SELECT t.id, COALESCE(t.hostname, ''), t.status, ta.cert_serial
		FROM thing t
		JOIN thing_agent ta ON ta.thing_id = t.id
		WHERE ta.cert_serial = $1 OR ta.previous_cert_serial = $1
		LIMIT 1
	`, serial)

	var info ThingNodeInfo
	err := row.Scan(&info.ID, &info.Hostname, &info.Status, &info.CertSerial)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lookup node by cert serial: %w", err)
	}
	return &info, nil
}
