package spillsweep

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Querier is the read seam this package needs from a pgx pool. Narrow on
// purpose: a sweep that could Exec is a sweep that could delete rows, and the
// only thing it is allowed to do to the database is ask a question.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// pgDBQuerier answers HasSpillRefs from traffic_event_payload.
type pgDBQuerier struct{ q Querier }

// NewDBQuerier returns the reference checker the sweep needs to tell an orphan
// from a live blob.
//
// One implementation rather than one per service. Leaving each service's wiring
// layer to supply its own asks three teams to write the same query, against a
// schema that is not the obvious one: there is no "TrafficEvent" table and no
// `spill_ref` column. The references live on traffic_event_payload, in TWO JSONB
// columns, with the object key under the `key` field of each. A query written
// against the obvious shape errors — or, worse, silently matches nothing and
// lets the sweep delete every referenced blob it is old enough to reach.
func NewDBQuerier(q Querier) DBQuerier { return pgDBQuerier{q: q} }

// hasSpillRefsSQL asks which of the candidate keys are still referenced.
//
// Both directions are checked, because either one alone leaves the other's
// blobs deletable while a live row still points at them. The keys are compared
// after extraction rather than by matching the whole JSONB value: the sweep
// knows object keys, and the stored value is an envelope carrying size, sha256
// and content type alongside the key.
const hasSpillRefsSQL = `
SELECT DISTINCT k
FROM (
    SELECT request_spill_ref ->> 'key' AS k
      FROM traffic_event_payload
     WHERE request_spill_ref IS NOT NULL
    UNION ALL
    SELECT response_spill_ref ->> 'key'
      FROM traffic_event_payload
     WHERE response_spill_ref IS NOT NULL
) refs
WHERE k = ANY($1)`

// HasSpillRefs returns the subset of keys still referenced by a live row.
//
// An error is returned rather than an empty map, and the distinction is the
// whole safety property: the sweep treats an error as "delete nothing", while
// an empty map means "none of these are referenced, delete them all". A DB
// hiccup answered as an empty map would delete every candidate blob.
func (p pgDBQuerier) HasSpillRefs(ctx context.Context, keys []string) (map[string]bool, error) {
	if len(keys) == 0 {
		return map[string]bool{}, nil
	}
	rows, err := p.q.Query(ctx, hasSpillRefsSQL, keys)
	if err != nil {
		return nil, fmt.Errorf("query spill references: %w", err)
	}
	defer rows.Close()

	referenced := make(map[string]bool, len(keys))
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, fmt.Errorf("scan spill reference: %w", err)
		}
		referenced[k] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate spill references: %w", err)
	}
	return referenced, nil
}
