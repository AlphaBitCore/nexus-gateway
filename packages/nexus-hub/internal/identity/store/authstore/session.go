package authstore

import (
	"context"
	"fmt"
)

// TouchSessionParams carries the session-time fields that a Thing may change
// on reconnect. Fields left at the zero value are not written (COALESCE(NULLIF)).
type TouchSessionParams struct {
	ID      string
	Name    string
	Version string
	Address string
}

// TouchThingSession updates last_seen_at, status=online, version, and address
// for an existing Thing. It never touches auth_type, conn_protocol, enrolled_by,
// physical_id, desired, desired_ver, reported, reported_ver, or metadata —
// those are owned by enrollment and the config-push loop, respectively.
//
// On the offline→online edge (status was anything other than 'online') it
// rewrites process_started_at to NOW() and resets reported_outcomes to {}.
// The Thing's OutcomeTracker is in-memory and reset on process restart, so
// stale entries in the DB would only mislead operators until the next
// successful apply rewrote them — clearing on the edge is the conservative
// choice and matches Thing-side semantics.
//
// Returns ErrNotFound if the Thing has not yet been enrolled.
func (s *Store) TouchThingSession(ctx context.Context, p TouchSessionParams) error {
	tag, err := s.db.Exec(ctx, `
		UPDATE thing SET
			version           = COALESCE(NULLIF($2, ''), version),
			address           = COALESCE(NULLIF($3, ''), address),
			name              = COALESCE(NULLIF($4, ''), NULLIF(name, ''), id),
			status            = 'online',
			last_seen_at      = NOW(),
			updated_at        = NOW(),
			process_started_at = CASE WHEN status <> 'online' THEN NOW() ELSE process_started_at END,
			reported_outcomes  = CASE WHEN status <> 'online' THEN '{}'::jsonb ELSE reported_outcomes END
		WHERE id = $1
	`, p.ID, p.Version, p.Address, p.Name)
	if err != nil {
		return fmt.Errorf("touch thing session: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
