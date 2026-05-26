package loaders

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/identity"
)

// activeExemptionGrantSelect returns the exemption grants that are currently
// eligible to bypass compliance hooks: not inactive, effective_from already
// reached, and expires_at still in the future. The predicate matches the
// admin / agent loaders (see catb_loader.go and catbagent/exemptions.go) so
// every consumer sees the same set of approved hosts.
//
// Column order MUST stay in sync with the rows.Scan call below.
const activeExemptionGrantSelect = `
	SELECT id, source_ip, target_host, reason, approved_by, inactive,
	       effective_from, expires_at
	FROM compliance_exemption_grant
	WHERE NOT inactive
	  AND effective_from <= $1
	  AND expires_at > $1
	ORDER BY expires_at ASC
`

// LoadActiveExemptions reads the compliance_exemption_grant table directly
// and projects active rows to the canonical identity.ActiveExemption wire
// shape. This is the Cat B server-side counterpart to the old Cat A path
// where Control Plane projected the snapshot and pushed it via WebSocket;
// compliance-proxy now owns its own DB read so the Hub WS push collapses
// to a stateless invalidate-only signal.
//
// A nil *sql.DB short-circuits to an empty slice + nil err so the receiver
// remains safe during boot / test contexts where DB wiring is optional —
// matching the LoadPayloadCaptureConfig nil-DB contract.
func LoadActiveExemptions(ctx context.Context, db *sql.DB) ([]identity.ActiveExemption, error) {
	if db == nil {
		return []identity.ActiveExemption{}, nil
	}
	now := time.Now().UTC()
	rows, err := db.QueryContext(ctx, activeExemptionGrantSelect, now)
	if err != nil {
		return nil, fmt.Errorf("exemptions: query compliance_exemption_grant: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	out := make([]identity.ActiveExemption, 0)
	for rows.Next() {
		var (
			id, sourceIP, targetHost, reason, approvedBy string
			inactive                                     bool
			effectiveFrom, expiresAt                     time.Time
		)
		if err := rows.Scan(&id, &sourceIP, &targetHost, &reason, &approvedBy,
			&inactive, &effectiveFrom, &expiresAt); err != nil {
			return nil, fmt.Errorf("exemptions: scan compliance_exemption_grant: %w", err)
		}
		out = append(out, identity.ActiveExemption{
			ID:            id,
			SourceIP:      sourceIP,
			TargetHost:    targetHost,
			ExpiresAt:     expiresAt.UTC().Format(time.RFC3339),
			EffectiveFrom: effectiveFrom.UTC().Format(time.RFC3339),
			Reason:        reason,
			ApprovedBy:    approvedBy,
			Disabled:      inactive,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("exemptions: iterate compliance_exemption_grant: %w", err)
	}
	return out, nil
}
