package catbagent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// AgentUserContextLoader aggregates the user-and-organization identity
// context for the Dashboard's Identity surface. The agent itself does
// not gate behaviour on these fields (compliance hooks key on
// devicePosture / sso_email already), but the user-facing display needs
// to show "you are X, in org Y, which rolls up under Z, ..." so the
// person operating the device understands what their CP admin sees.
//
// Wire shape (this is base data unconditionally downloaded):
//
//	{
//	  "user": {id, displayName, email, status, organizationId},
//	  "currentOrgId": "...",
//	  "organizations": [
//	    {id, name, code, parentId, path, description, timezone}, ...
//	  ]
//	}
//
// organizations carries the user's org PLUS every ancestor up to the
// root, sorted root → leaf so the UI can render a breadcrumb without
// re-sorting. Sibling / descendant orgs are NOT included — the agent
// shouldn't know about peer organisations.
type AgentUserContextLoader struct {
	db     pgxQuerier
	logger *slog.Logger
}

// NewAgentUserContextLoader constructs a loader bound to the given pool.
func NewAgentUserContextLoader(db pgxQuerier, logger *slog.Logger) *AgentUserContextLoader {
	return &AgentUserContextLoader{db: db, logger: logger}
}

type agentUserContextWire struct {
	User          *agentUserInfoRow `json:"user,omitempty"`
	CurrentOrgID  string            `json:"currentOrgId,omitempty"`
	Organizations []agentOrgNodeRow `json:"organizations"`
}

type agentUserInfoRow struct {
	ID             string `json:"id"`
	DisplayName    string `json:"displayName"`
	Email          string `json:"email,omitempty"`
	Status         string `json:"status,omitempty"`
	Source         string `json:"source,omitempty"`
	OrganizationID string `json:"organizationId"`
}

type agentOrgNodeRow struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Code        string `json:"code"`
	ParentID    string `json:"parentId,omitempty"`
	Path        string `json:"path"`
	Description string `json:"description,omitempty"`
	Timezone    string `json:"timezone,omitempty"`
}

// Load uses the thingID (= device id from the agent's perspective) to
// find the currently-assigned user via device_assignment.releasedAt
// IS NULL. Falls back to the org hierarchy of the most-recent past
// assignment if no current row exists — gives the Dashboard something
// to show during the window between user logout and the next sign-in.
func (l *AgentUserContextLoader) Load(ctx context.Context, thingID string) (any, int64, error) {
	out := agentUserContextWire{Organizations: []agentOrgNodeRow{}}

	if thingID == "" {
		return out, 0, nil
	}

	var (
		user      agentUserInfoRow
		updatedAt time.Time
		userFound bool
	)
	err := l.db.QueryRow(ctx, `
		SELECT u.id, u."displayName", COALESCE(u.email, ''),
		       u.status, u.source, u."organizationId", u."updatedAt"
		FROM "DeviceAssignment" da
		JOIN "NexusUser" u ON u.id = da."userId"
		WHERE da."deviceId" = $1 AND da."releasedAt" IS NULL
		ORDER BY da."assignedAt" DESC
		LIMIT 1
	`, thingID).Scan(
		&user.ID, &user.DisplayName, &user.Email, &user.Status, &user.Source,
		&user.OrganizationID, &updatedAt,
	)
	switch err {
	case nil:
		userFound = true
	default:
		// No active assignment — try the most recent past assignment.
		fallbackErr := l.db.QueryRow(ctx, `
			SELECT u.id, u."displayName", COALESCE(u.email, ''),
			       u.status, u.source, u."organizationId", u."updatedAt"
			FROM "DeviceAssignment" da
			JOIN "NexusUser" u ON u.id = da."userId"
			WHERE da."deviceId" = $1
			ORDER BY COALESCE(da."releasedAt", da."assignedAt") DESC
			LIMIT 1
		`, thingID).Scan(
			&user.ID, &user.DisplayName, &user.Email, &user.Status, &user.Source,
			&user.OrganizationID, &updatedAt,
		)
		if fallbackErr == nil {
			userFound = true
		}
	}

	if userFound {
		out.User = &user
		out.CurrentOrgID = user.OrganizationID
	}

	// Resolve the org tree. Without a current user we still try to
	// emit the "default" org so the Dashboard has something to show
	// (most local-dev installs land here pre-enrollment).
	orgID := user.OrganizationID
	if orgID == "" {
		orgID = "default"
	}
	orgs, orgVer, err := l.loadOrgAncestors(ctx, orgID)
	if err != nil {
		return nil, 0, err
	}
	out.Organizations = orgs
	if orgVer.After(updatedAt) {
		updatedAt = orgVer
	}

	// timestampVersion guards a zero time.Time → 0; a fully empty result
	// set (no active assignment, no resolvable org row) otherwise yields a
	// large negative epoch-predecessor version that the configloader would
	// treat as stale. Matches every sibling Cat B loader in this package.
	return out, timestampVersion(updatedAt), nil
}

// loadOrgAncestors walks the org tree from leafID up to the root via
// the materialized `path` column (faster than recursive parentId joins).
// Returns the chain root → leaf so the Dashboard can render a breadcrumb.
func (l *AgentUserContextLoader) loadOrgAncestors(ctx context.Context, leafID string) ([]agentOrgNodeRow, time.Time, error) {
	out := []agentOrgNodeRow{}
	var maxUpdated time.Time

	var leafPath string
	err := l.db.QueryRow(ctx, `SELECT path FROM "Organization" WHERE id = $1`, leafID).Scan(&leafPath)
	if err != nil {
		// Org row missing (deleted or never existed). Surface an empty
		// list — the caller knows the user's organizationId field, and
		// the UI will fall back to "no org metadata available".
		return out, maxUpdated, nil
	}

	// leafPath looks like "/root-id/child-id/leaf-id/". Split into the
	// segment ids, then SELECT every row in one shot.
	trimmed := strings.Trim(leafPath, "/")
	if trimmed == "" {
		return out, maxUpdated, nil
	}
	ids := strings.Split(trimmed, "/")

	rows, err := l.db.Query(ctx, `
		SELECT id, name, code, COALESCE("parentId", ''), path,
		       COALESCE(description, ''), timezone, "updatedAt"
		FROM "Organization"
		WHERE id = ANY($1)
		ORDER BY path
	`, ids)
	if err != nil {
		return nil, maxUpdated, fmt.Errorf("catb: query org ancestors: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			o         agentOrgNodeRow
			updatedAt time.Time
		)
		if err := rows.Scan(&o.ID, &o.Name, &o.Code, &o.ParentID, &o.Path,
			&o.Description, &o.Timezone, &updatedAt); err != nil {
			return nil, maxUpdated, fmt.Errorf("catb: scan org: %w", err)
		}
		out = append(out, o)
		if updatedAt.After(maxUpdated) {
			maxUpdated = updatedAt
		}
	}
	if err := rows.Err(); err != nil {
		return nil, maxUpdated, fmt.Errorf("catb: iterate org: %w", err)
	}
	return out, maxUpdated, nil
}
