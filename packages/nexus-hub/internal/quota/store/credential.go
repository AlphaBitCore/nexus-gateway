package quotastore

import (
	"context"
	"fmt"
	"time"
)

// CredentialExpiryInfo holds the minimum fields needed to emit a
// credential-expiring alert.
type CredentialExpiryInfo struct {
	ID         string
	Name       string
	ProviderID string
	ExpiresAt  time.Time
}

// ListExpiringCredentials returns enabled credentials whose expiresAt is
// within the next withinDays days and whose rotationState is still 'none'
// (not already in rotation).
func ListExpiringCredentials(ctx context.Context, pool PgxPool, withinDays int) ([]CredentialExpiryInfo, error) {
	rows, err := pool.Query(ctx, `
		SELECT id, name, "providerId", "expiresAt"
		FROM "Credential"
		WHERE "expiresAt" IS NOT NULL
		  AND "expiresAt" > NOW()
		  AND "expiresAt" <= NOW() + ($1 * INTERVAL '1 day')
		  AND enabled = true
		  AND COALESCE("rotationState", 'none') = 'none'
		ORDER BY "expiresAt" ASC
	`, withinDays)
	if err != nil {
		return nil, fmt.Errorf("list expiring credentials: %w", err)
	}
	defer rows.Close()

	var creds []CredentialExpiryInfo
	for rows.Next() {
		var c CredentialExpiryInfo
		if err := rows.Scan(&c.ID, &c.Name, &c.ProviderID, &c.ExpiresAt); err != nil {
			return nil, fmt.Errorf("scan expiring credential: %w", err)
		}
		creds = append(creds, c)
	}
	return creds, rows.Err()
}

// ListOverdueCredentials returns enabled credentials whose expiresAt has
// already passed (regardless of rotationState).
func ListOverdueCredentials(ctx context.Context, pool PgxPool) ([]CredentialExpiryInfo, error) {
	rows, err := pool.Query(ctx, `
		SELECT id, name, "providerId", "expiresAt"
		FROM "Credential"
		WHERE "expiresAt" IS NOT NULL
		  AND "expiresAt" <= NOW()
		  AND enabled = true
		ORDER BY "expiresAt" ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("list overdue credentials: %w", err)
	}
	defer rows.Close()

	var creds []CredentialExpiryInfo
	for rows.Next() {
		var c CredentialExpiryInfo
		if err := rows.Scan(&c.ID, &c.Name, &c.ProviderID, &c.ExpiresAt); err != nil {
			return nil, fmt.Errorf("scan overdue credential: %w", err)
		}
		creds = append(creds, c)
	}
	return creds, rows.Err()
}

// MarkCredentialsPendingRotation sets rotationState = 'pending_rotation' for
// the given credential IDs where rotationState is still 'none'. Returns the
// count of rows updated.
func MarkCredentialsPendingRotation(ctx context.Context, pool PgxPool, ids []string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	// Build a parameterised IN clause.
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	placeholders := ""
	for i := range ids {
		if i > 0 {
			placeholders += ", "
		}
		placeholders += fmt.Sprintf("$%d", i+1)
	}
	tag, err := pool.Exec(ctx, fmt.Sprintf(`
		UPDATE "Credential"
		SET "rotationState" = 'pending_rotation', "updatedAt" = NOW()
		WHERE id IN (%s)
		  AND COALESCE("rotationState", 'none') = 'none'
	`, placeholders), args...)
	if err != nil {
		return 0, fmt.Errorf("mark credentials pending rotation: %w", err)
	}
	return tag.RowsAffected(), nil
}
