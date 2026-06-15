// Package dsarstore owns the DSAR (Data Subject Access Request)
// persistence — extracted from internal/store/dsar.go per R8-B18.
package dsarstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/users/usercascade"
)

// PgxPool is the minimum pgx pool surface dsarstore methods need.
// *pgxpool.Pool satisfies it in production; pgxmock satisfies it in
// tests. Mirrors store.PgxPool.
type PgxPool interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Store is the DSAR persistence handle.
type Store struct {
	pool PgxPool
}

// New constructs a Store from a PgxPool.
func New(pool PgxPool) *Store {
	return &Store{pool: pool}
}

// DSARRequest represents a row from the dsar_request table.
type DSARRequest struct {
	ID          string          `json:"id"`
	SubjectID   string          `json:"subjectId"`
	Contact     *string         `json:"contact"`
	Type        string          `json:"type"`   // ACCESS | ERASURE
	Status      string          `json:"status"` // PENDING | IN_PROGRESS | COMPLETED | REJECTED
	Notes       *string         `json:"notes"`
	CompletedAt *time.Time      `json:"completedAt"`
	Outcome     json.RawMessage `json:"outcome"`
	CreatedAt   time.Time       `json:"createdAt"`
	CreatedBy   string          `json:"createdBy"`
	UpdatedAt   time.Time       `json:"updatedAt"`
	UpdatedBy   *string         `json:"updatedBy"`
}

const dsarColumns = `id, subject_id, contact, type, status, notes, completed_at,
	outcome, "createdAt", created_by, "updatedAt", updated_by`

func scanDSAR(row pgx.Row) (*DSARRequest, error) {
	var d DSARRequest
	err := row.Scan(
		&d.ID, &d.SubjectID, &d.Contact, &d.Type, &d.Status, &d.Notes,
		&d.CompletedAt, &d.Outcome, &d.CreatedAt, &d.CreatedBy, &d.UpdatedAt, &d.UpdatedBy,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// ListDSARRequests returns paginated DSAR requests optionally filtered by status.
// Returns the matching rows and the total unfiltered count.
func (store *Store) ListDSARRequests(ctx context.Context, status string, limit, offset int) ([]DSARRequest, int, error) {
	if limit <= 0 {
		limit = 20
	}
	where := "WHERE 1=1"
	args := []any{}
	n := 1
	if status != "" {
		where += fmt.Sprintf(` AND status = $%d`, n)
		args = append(args, status)
		n++
	}

	var total int
	if err := store.pool.QueryRow(ctx,
		fmt.Sprintf(`SELECT COUNT(*) FROM dsar_request %s`, where), args...,
	).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count dsar: %w", err)
	}

	q := fmt.Sprintf(`SELECT %s FROM dsar_request %s ORDER BY "createdAt" DESC LIMIT $%d OFFSET $%d`,
		dsarColumns, where, n, n+1)
	args = append(args, limit, offset)

	rows, err := store.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list dsar: %w", err)
	}
	defer rows.Close()

	requests := []DSARRequest{}
	for rows.Next() {
		var d DSARRequest
		if err := rows.Scan(
			&d.ID, &d.SubjectID, &d.Contact, &d.Type, &d.Status, &d.Notes,
			&d.CompletedAt, &d.Outcome, &d.CreatedAt, &d.CreatedBy, &d.UpdatedAt, &d.UpdatedBy,
		); err != nil {
			return nil, 0, err
		}
		requests = append(requests, d)
	}
	return requests, total, rows.Err()
}

// GetDSARRequest returns a DSAR request by ID.
func (store *Store) GetDSARRequest(ctx context.Context, id string) (*DSARRequest, error) {
	q := fmt.Sprintf(`SELECT %s FROM dsar_request WHERE id = $1`, dsarColumns)
	return scanDSAR(store.pool.QueryRow(ctx, q, id))
}

// CreateDSARRequestParams holds fields for creating a DSAR request.
type CreateDSARRequestParams struct {
	SubjectID string
	Contact   *string
	Type      string // ACCESS | ERASURE
	Notes     *string
	CreatedBy string
}

// CreateDSARRequest inserts a new DSAR request.
func (store *Store) CreateDSARRequest(ctx context.Context, p CreateDSARRequestParams) (*DSARRequest, error) {
	q := fmt.Sprintf(`
		INSERT INTO dsar_request (id, subject_id, contact, type, status, notes, created_by, "createdAt", "updatedAt")
		VALUES (gen_random_uuid(), $1, $2, $3, 'PENDING', $4, $5, NOW(), NOW())
		RETURNING %s
	`, dsarColumns)
	return scanDSAR(store.pool.QueryRow(ctx, q, p.SubjectID, p.Contact, p.Type, p.Notes, p.CreatedBy))
}

// UpdateDSARParams holds optional fields for updating a DSAR request.
type UpdateDSARParams struct {
	Status      *string
	Notes       *string
	CompletedAt *time.Time
	Outcome     json.RawMessage // nil = no change
	UpdatedBy   *string
}

// UpdateDSARRequest updates a DSAR request using COALESCE.
func (store *Store) UpdateDSARRequest(ctx context.Context, id string, p UpdateDSARParams) (*DSARRequest, error) {
	q := fmt.Sprintf(`UPDATE dsar_request SET
		status = COALESCE($2, status),
		notes = COALESCE($3, notes),
		completed_at = COALESCE($4, completed_at),
		outcome = COALESCE($5, outcome),
		updated_by = COALESCE($6, updated_by),
		"updatedAt" = NOW()
	WHERE id = $1 RETURNING %s`, dsarColumns)
	return scanDSAR(store.pool.QueryRow(ctx, q, id, p.Status, p.Notes, p.CompletedAt, p.Outcome, p.UpdatedBy))
}

// DSARAssistantExport holds the subject's assistant data ("Chat with
// Nexus") for an Art.15 access export. Bodies are included because they are
// the subject's own personal data; session transcripts and file contents
// that spilled to object storage are referenced by spillRef and delivered
// out-of-band (the DB row carries only the reference).
type DSARAssistantExport struct {
	Sessions []map[string]any `json:"sessions"`
	Memory   []map[string]any `json:"memory"`
	Files    []map[string]any `json:"files"`
}

// DSARAccessExport holds data for ACCESS fulfillment — actual rows capped per
// source. It covers the same personal-data surfaces ERASURE touches so an
// Art.15 access request and an Art.17 erasure request see a symmetric view of
// the subject's footprint: the user record, IAM group memberships, AI-gateway
// + agent traffic, the inline prompt/response bodies, device-assignment
// history, and the assistant data.
type DSARAccessExport struct {
	User      map[string]any      `json:"user"`      // the NexusUser record; nil when the subject row is already gone
	IAMGroups []map[string]any    `json:"iamGroups"` // admin_user group memberships
	VKRows    []map[string]any    `json:"vk"`
	AgentRows []map[string]any    `json:"agent"`
	Devices   []map[string]any    `json:"devices"`  // devices assigned to this user (for context)
	Payloads  []map[string]any    `json:"payloads"` // inline request/response bodies (capped)
	Assistant DSARAssistantExport `json:"assistant"`
}

// SubjectExists reports whether subjectID resolves to a NexusUser row. DSAR
// fulfillment uses it to refuse a mistyped/free-string subjectId before it
// force-marks an empty ACCESS/ERASURE as COMPLETED (which would tell a
// compliance officer "the data is gone" when nothing was ever matched).
func (store *Store) SubjectExists(ctx context.Context, subjectID string) (bool, error) {
	var exists bool
	if err := store.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM "NexusUser" WHERE id = $1)`, subjectID,
	).Scan(&exists); err != nil {
		return false, fmt.Errorf("dsar subject exists: %w", err)
	}
	return exists, nil
}

// FulfillDSARAccess queries every personal-data surface for a NexusUser and
// returns it as a single export. subjectID is the NexusUser.id. It collects:
//   - the NexusUser record itself + IAM group memberships
//   - AI Gateway traffic via entity_id (= NexusUser.id)
//   - Agent traffic via DeviceAssignment joined on thing_id (= agent Thing ID),
//     respecting assignment time windows
//   - the inline request/response bodies on the subject's traffic (capped)
//   - device-assignment history + assistant sessions/memory/files
func (store *Store) FulfillDSARAccess(ctx context.Context, subjectID string) (*DSARAccessExport, error) {
	const maxRows = 10000
	const maxPayloadRows = 1000
	result := &DSARAccessExport{}

	// 0a. The NexusUser record — the subject's profile/identity data.
	if err := store.fillAccessUser(ctx, subjectID, result); err != nil {
		return nil, err
	}

	// 0b. IAM group memberships (principalType 'admin_user').
	if err := store.fillAccessIAMGroups(ctx, subjectID, result); err != nil {
		return nil, err
	}

	// 1. AI Gateway traffic: entity_id matches the NexusUser ID
	vkRows, err := store.pool.Query(ctx, `
		SELECT id, timestamp, COALESCE(routed_provider_name, provider_name, ''), method, path, status_code,
			COALESCE(routed_model_name, model_name), estimated_cost_usd, prompt_tokens, completion_tokens
		FROM traffic_event
		WHERE source = 'ai-gateway' AND entity_id = $1
		ORDER BY timestamp DESC LIMIT $2
	`, subjectID, maxRows)
	if err != nil {
		return nil, fmt.Errorf("dsar access vk query: %w", err)
	}
	for vkRows.Next() {
		var id, provider string
		var method, path, model *string
		var ts any
		var statusCode, pt, ct *int
		var cost *float64
		if err := vkRows.Scan(&id, &ts, &provider, &method, &path, &statusCode, &model, &cost, &pt, &ct); err == nil {
			result.VKRows = append(result.VKRows, map[string]any{
				"id": id, "timestamp": ts, "provider": provider,
				"method": method, "path": path, "statusCode": statusCode,
				"modelUsed": model, "estimatedCostUsd": cost,
				"promptTokens": pt, "completionTokens": ct,
			})
		}
	}
	vkRows.Close()

	// 2. Agent traffic: find devices assigned to this user, then query traffic
	//    within each assignment window (assignedAt <= timestamp < releasedAt).
	agentRows, err := store.pool.Query(ctx, `
		SELECT t.id, t.timestamp, t.thing_id,
			COALESCE(t.source_process,''), COALESCE(t.target_host,''),
			t.action, t.request_hook_decision, t.latency_ms
		FROM traffic_event t
		JOIN "DeviceAssignment" da ON da."deviceId" = t.thing_id
		WHERE da."userId" = $1
		  AND t.source = 'agent'
		  AND t.timestamp >= da."assignedAt"
		  AND (da."releasedAt" IS NULL OR t.timestamp < da."releasedAt")
		ORDER BY t.timestamp DESC LIMIT $2
	`, subjectID, maxRows)
	if err != nil {
		return nil, fmt.Errorf("dsar access agent query: %w", err)
	}
	for agentRows.Next() {
		var id, srcProc, destHost string
		var deviceID *string
		var ts any
		var action, hookDec *string
		var latency *int
		if err := agentRows.Scan(&id, &ts, &deviceID, &srcProc, &destHost, &action, &hookDec, &latency); err == nil {
			result.AgentRows = append(result.AgentRows, map[string]any{
				"id": id, "timestamp": ts, "deviceId": deviceID,
				"sourceProcess": srcProc, "destHost": destHost,
				"action": action, "hookDecision": hookDec, "latencyMs": latency,
			})
		}
	}
	agentRows.Close()

	// 3. Device assignment history for context
	devRows, err := store.pool.Query(ctx, `
		SELECT da."deviceId", COALESCE(t.hostname, ''), da."assignedAt", da."releasedAt"
		FROM "DeviceAssignment" da
		JOIN thing t ON t.id = da."deviceId"
		WHERE da."userId" = $1
		ORDER BY da."assignedAt" DESC
	`, subjectID)
	if err != nil {
		return nil, fmt.Errorf("dsar device assignments: %w", err)
	}
	for devRows.Next() {
		var deviceID, hostname string
		var assignedAt any
		var releasedAt *time.Time
		if err := devRows.Scan(&deviceID, &hostname, &assignedAt, &releasedAt); err == nil {
			result.Devices = append(result.Devices, map[string]any{
				"deviceId": deviceID, "hostname": hostname,
				"assignedAt": assignedAt, "releasedAt": releasedAt,
			})
		}
	}
	devRows.Close()

	// 4. Inline request/response bodies on the subject's traffic (both legs),
	//    capped. These are the prompt/response contents — the subject's own
	//    personal data under Art.15.
	if err := store.fillAccessPayloads(ctx, subjectID, maxPayloadRows, result); err != nil {
		return nil, err
	}

	// 5. Assistant data (sessions, memory, files) keyed on userId = subjectID.
	if err := store.fillAccessAssistant(ctx, subjectID, result); err != nil {
		return nil, err
	}

	return result, nil
}

// fillAccessUser loads the NexusUser record into result.User. A missing row
// leaves result.User nil (the caller already gates on SubjectExists, but the
// row can vanish between the gate and this read).
func (store *Store) fillAccessUser(ctx context.Context, subjectID string, result *DSARAccessExport) error {
	var (
		id, orgID, displayName, status, source         string
		email, osUsername, osDomain, preferredTimezone *string
		lastLoginAt                                    *time.Time
		createdAt, updatedAt                           time.Time
	)
	err := store.pool.QueryRow(ctx, `
		SELECT id, "organizationId", "displayName", email, status, source,
			"osUsername", "osDomain", "preferredTimezone", "lastLoginAt",
			"createdAt", "updatedAt"
		FROM "NexusUser" WHERE id = $1
	`, subjectID).Scan(&id, &orgID, &displayName, &email, &status, &source,
		&osUsername, &osDomain, &preferredTimezone, &lastLoginAt, &createdAt, &updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("dsar access user query: %w", err)
	}
	result.User = map[string]any{
		"id": id, "organizationId": orgID, "displayName": displayName,
		"email": email, "status": status, "source": source,
		"osUsername": osUsername, "osDomain": osDomain,
		"preferredTimezone": preferredTimezone, "lastLoginAt": lastLoginAt,
		"createdAt": createdAt, "updatedAt": updatedAt,
	}
	return nil
}

// fillAccessIAMGroups loads the subject's IAM group memberships.
func (store *Store) fillAccessIAMGroups(ctx context.Context, subjectID string, result *DSARAccessExport) error {
	rows, err := store.pool.Query(ctx, `
		SELECT g.id, g.name, m."createdAt"
		FROM "IamGroupMembership" m
		JOIN "IamGroup" g ON g.id = m."groupId"
		WHERE m."principalType" = 'admin_user' AND m."principalId" = $1
		ORDER BY g.name ASC
	`, subjectID)
	if err != nil {
		return fmt.Errorf("dsar access iam-groups query: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var groupID, groupName string
		var createdAt any
		if err := rows.Scan(&groupID, &groupName, &createdAt); err == nil {
			result.IAMGroups = append(result.IAMGroups, map[string]any{
				"groupId": groupID, "groupName": groupName, "joinedAt": createdAt,
			})
		}
	}
	return rows.Err()
}

// fillAccessPayloads loads the subject's inline request/response bodies across
// both traffic legs (same scoping predicate as erasure), capped at limit.
func (store *Store) fillAccessPayloads(ctx context.Context, subjectID string, limit int, result *DSARAccessExport) error {
	rows, err := store.pool.Query(ctx, `
		SELECT t.id, t.source, p.inline_request_body, p.inline_response_body
		FROM traffic_event_payload p
		JOIN traffic_event t ON t.id = p.traffic_event_id
		WHERE (
			(t.source = 'ai-gateway' AND t.entity_id = $1)
			OR (t.source = 'agent' AND EXISTS (
				SELECT 1 FROM "DeviceAssignment" da
				WHERE da."userId" = $1 AND da."deviceId" = t.thing_id
					AND t.timestamp >= da."assignedAt"
					AND (da."releasedAt" IS NULL OR t.timestamp < da."releasedAt")
			))
		)
		ORDER BY t.timestamp DESC LIMIT $2
	`, subjectID, limit)
	if err != nil {
		return fmt.Errorf("dsar access payload query: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, source string
		var reqBody, respBody json.RawMessage
		if err := rows.Scan(&id, &source, &reqBody, &respBody); err == nil {
			result.Payloads = append(result.Payloads, map[string]any{
				"trafficEventId": id, "source": source,
				"requestBody": reqBody, "responseBody": respBody,
			})
		}
	}
	return rows.Err()
}

// fillAccessAssistant loads the subject's assistant sessions, memory, and files.
func (store *Store) fillAccessAssistant(ctx context.Context, subjectID string, result *DSARAccessExport) error {
	sessRows, err := store.pool.Query(ctx, `
		SELECT id, title, "msgCount", "createdAt", "updatedAt"
		FROM "AssistantSession" WHERE "userId" = $1 ORDER BY "updatedAt" DESC
	`, subjectID)
	if err != nil {
		return fmt.Errorf("dsar access assistant sessions query: %w", err)
	}
	for sessRows.Next() {
		var id, title string
		var msgCount int
		var createdAt, updatedAt any
		if err := sessRows.Scan(&id, &title, &msgCount, &createdAt, &updatedAt); err == nil {
			result.Assistant.Sessions = append(result.Assistant.Sessions, map[string]any{
				"id": id, "title": title, "msgCount": msgCount,
				"createdAt": createdAt, "updatedAt": updatedAt,
			})
		}
	}
	sessRows.Close()
	if err := sessRows.Err(); err != nil {
		return fmt.Errorf("dsar access assistant sessions scan: %w", err)
	}

	memRows, err := store.pool.Query(ctx, `
		SELECT name, type, body, "updatedAt"
		FROM "AssistantMemory" WHERE "userId" = $1 ORDER BY name ASC
	`, subjectID)
	if err != nil {
		return fmt.Errorf("dsar access assistant memory query: %w", err)
	}
	for memRows.Next() {
		var name, memType, body string
		var updatedAt any
		if err := memRows.Scan(&name, &memType, &body, &updatedAt); err == nil {
			result.Assistant.Memory = append(result.Assistant.Memory, map[string]any{
				"name": name, "type": memType, "body": body, "updatedAt": updatedAt,
			})
		}
	}
	memRows.Close()
	if err := memRows.Err(); err != nil {
		return fmt.Errorf("dsar access assistant memory scan: %w", err)
	}

	fileRows, err := store.pool.Query(ctx, `
		SELECT id, "sessionId", name, size, "contentType", "createdAt"
		FROM "AssistantFile" WHERE "userId" = $1 ORDER BY "createdAt" DESC
	`, subjectID)
	if err != nil {
		return fmt.Errorf("dsar access assistant files query: %w", err)
	}
	for fileRows.Next() {
		var id, sessionID, name, contentType string
		var size int
		var createdAt any
		if err := fileRows.Scan(&id, &sessionID, &name, &size, &contentType, &createdAt); err == nil {
			result.Assistant.Files = append(result.Assistant.Files, map[string]any{
				"id": id, "sessionId": sessionID, "name": name, "size": size,
				"contentType": contentType, "createdAt": createdAt,
			})
		}
	}
	fileRows.Close()
	return fileRows.Err()
}

// DSARErasureResult holds counts for ERASURE fulfillment.
type DSARErasureResult struct {
	VKAnonymised    int `json:"vkAnonymised"`
	AgentAnonymised int `json:"agentAnonymised"`
	TotalAnonymised int `json:"totalAnonymised"`
	// PayloadsScrubbed is the number of traffic_event_payload rows whose inline
	// request/response bodies and spill references were cleared.
	PayloadsScrubbed int `json:"payloadsScrubbed"`
	// NormalizedScrubbed is the number of traffic_event_normalized rows whose
	// normalized request/response copies, error reasons, and redaction spans were
	// cleared (the canonical copy of the captured bodies — without this the
	// prompt/response text survives the erasure).
	NormalizedScrubbed int `json:"normalizedScrubbed"`
	// SpillRefsOrphaned counts payload rows that still referenced spilled
	// (S3/localfs) bodies at scrub time. The DB references are cleared here; the
	// physical spill objects are deleted out-of-band (no spill
	// delete API is wired into this store yet).
	SpillRefsOrphaned int `json:"spillRefsOrphaned"`
	// AssistantErased is the number of assistant rows (memory + session + file)
	// deleted for the subject.
	AssistantErased int `json:"assistantErased"`
	// AccessOutcomesScrubbed is the number of the subject's prior ACCESS
	// dsar_request rows whose persisted `outcome` export (which itself holds a
	// full copy of the subject's PII) was nulled. Without this, the correct
	// GDPR sequence — ACCESS then ERASURE for the same subject — would leave
	// MORE un-erased PII than erasure alone (the access export survives in
	// dsar_request.outcome).
	AccessOutcomesScrubbed int `json:"accessOutcomesScrubbed"`

	// ── Account-record deletion stage ────────────────────────────────
	// Erasure DEFAULTS to full account deletion: Art.17 covers "all personal
	// data", which includes the account record itself (NexusUser
	// displayName/email/osUsername), the SSO link rows (UserFederatedIdentity
	// externalEmail/rawClaims), the subject's refresh tokens, and the keys the
	// subject owns. Only the tamper-evident admin-audit hash chain is retained,
	// under a documented accountability/legal-obligation exception.

	// VKOwnedDeleted is the number of VirtualKey rows owned by the subject that
	// were deleted. The ownerId FK is ON DELETE SET NULL, so deleting the account
	// alone would only orphan-null these rows — they are removed explicitly here.
	VKOwnedDeleted int `json:"vkOwnedDeleted"`
	// AdminApiKeysDeleted is the number of AdminApiKey rows owned by the subject
	// that were deleted. ownerUserId is ON DELETE SET NULL (same orphan concern).
	AdminApiKeysDeleted int `json:"adminApiKeysDeleted"`
	// FederatedIdentitiesDeleted is the number of UserFederatedIdentity rows
	// (external IdP subject, externalEmail, rawClaims) deleted for the subject.
	// The FK is ON DELETE CASCADE; they are deleted explicitly so the count is
	// reported (and the order is FK-correct regardless of cascade configuration).
	FederatedIdentitiesDeleted int `json:"federatedIdentitiesDeleted"`
	// RefreshTokensDeleted is the number of the subject's refresh-token rows
	// deleted (FK ON DELETE CASCADE; deleted explicitly for the same reasons).
	RefreshTokensDeleted int `json:"refreshTokensDeleted"`
	// ScimTokensDeleted is the number of SCIM provisioning tokens the subject
	// CREATED that were deleted. ScimToken.createdBy is ON DELETE RESTRICT, so
	// these rows would otherwise BLOCK the NexusUser delete — removing them is a
	// hard requirement for an atomic full-account erasure of an admin subject.
	ScimTokensDeleted int `json:"scimTokensDeleted"`
	// IamGroupMembershipsDeleted / IamPolicyAttachmentsDeleted are the subject's
	// admin_user IAM grants removed so erasure leaves no orphaned authz rows.
	IamGroupMembershipsDeleted  int `json:"iamGroupMembershipsDeleted"`
	IamPolicyAttachmentsDeleted int `json:"iamPolicyAttachmentsDeleted"`
	// AccountDeleted reports whether the subject's own NexusUser row was deleted.
	// False only if the row vanished between the existence gate and this
	// transaction (a benign TOCTOU race — the account is gone either way).
	AccountDeleted bool `json:"accountDeleted"`
}

// FulfillDSARErasure performs a GDPR Art.17 erasure of all of a NexusUser's
// personal data. subjectID is the NexusUser.id. In a single transaction it:
//   - scrubs the inline request/response bodies and spill references on the
//     subject's traffic_event_payload rows (AI-Gateway + agent legs);
//   - nulls the identifying columns (entity_id, entity_name, identity snapshot,
//     source_ip, source_process) on the subject's traffic_event rows;
//   - deletes the subject's assistant data (memory, sessions, files);
//   - nulls the persisted ACCESS export (dsar_request.outcome) for the subject,
//     which itself holds a full PII copy from any prior access request;
//   - DELETES THE ACCOUNT RECORD ITSELF (default behaviour): the
//     subject's NexusUser row, their SSO link rows (UserFederatedIdentity), their
//     refresh tokens, the SCIM tokens they created, and the VirtualKey /
//     AdminApiKey rows they own. Art.17 covers "all personal data", which on a
//     compliance gateway includes the account profile and SSO claims, not just
//     the traffic footprint.
//
// Audit-trail retention exception: the tamper-evident admin-audit hash chain
// (AdminAuditLog) is DELIBERATELY NOT touched. Those rows carry no subject PII
// beyond an opaque actor id, and breaking the chain would destroy the
// tamper-evidence the gateway relies on as a compliance control. Retaining them
// is the documented accountability / legal-obligation basis exception to the
// otherwise-complete erasure (see data-retention-purge-architecture.md §3).
//
// Payloads are scrubbed BEFORE the identifying columns are nulled, because the
// agent/VK selection predicates depend on those columns; the account-deletion
// stage runs LAST (after the traffic anonymisation, which keys on entity_id =
// NexusUser.id) so it cannot strand any earlier stage. The whole sequence is one
// transaction: any mid-stage failure rolls back the entire erasure.
//
// NOTE: the physical spill objects behind any cleared spill references are NOT
// deleted here (no spill delete API is wired into this pool-only store); their
// count is reported as SpillRefsOrphaned and their physical deletion is tracked
// as a follow-up. The DB no longer exposes any path to them.
func (store *Store) FulfillDSARErasure(ctx context.Context, subjectID string) (*DSARErasureResult, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin erasure tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	result := &DSARErasureResult{}

	// 0. Count spill references about to be orphaned (for operator visibility +
	//    the physical-deletion follow-up), before they are cleared.
	if err := tx.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM traffic_event_payload p
		JOIN traffic_event t ON t.id = p.traffic_event_id
		WHERE (p.request_spill_ref IS NOT NULL OR p.response_spill_ref IS NOT NULL)
		  AND (
		    (t.source = 'ai-gateway' AND t.entity_id = $1)
		    OR (t.source = 'agent' AND EXISTS (
		      SELECT 1 FROM "DeviceAssignment" da
		      WHERE da."userId" = $1 AND da."deviceId" = t.thing_id
		        AND t.timestamp >= da."assignedAt"
		        AND (da."releasedAt" IS NULL OR t.timestamp < da."releasedAt")
		    ))
		  )
	`, subjectID).Scan(&result.SpillRefsOrphaned); err != nil {
		return nil, fmt.Errorf("count orphaned spill refs: %w", err)
	}

	// 1. Scrub the subject's payload bodies + spill references (both legs). This
	//    MUST run before the identifying columns are nulled.
	tagP, err := tx.Exec(ctx, `
		UPDATE traffic_event_payload p
		SET inline_request_body = NULL,
		    inline_response_body = NULL,
		    request_spill_ref = NULL,
		    response_spill_ref = NULL
		FROM traffic_event t
		WHERE t.id = p.traffic_event_id
		  AND (
		    (t.source = 'ai-gateway' AND t.entity_id = $1)
		    OR (t.source = 'agent' AND EXISTS (
		      SELECT 1 FROM "DeviceAssignment" da
		      WHERE da."userId" = $1 AND da."deviceId" = t.thing_id
		        AND t.timestamp >= da."assignedAt"
		        AND (da."releasedAt" IS NULL OR t.timestamp < da."releasedAt")
		    ))
		  )
	`, subjectID)
	if err != nil {
		return nil, fmt.Errorf("scrub payload bodies: %w", err)
	}
	result.PayloadsScrubbed = int(tagP.RowsAffected())

	// 1b. Scrub the NORMALIZED copy of the bodies (traffic_event_normalized). This
	//     1:1 sidecar holds the canonical normalized request/response text; without
	//     scrubbing it the subject's prompts/responses survive the erasure. Same
	//     scoping + same before-nulling ordering as the payload scrub.
	tagN, err := tx.Exec(ctx, `
		UPDATE traffic_event_normalized n
		SET request_normalized = NULL,
		    response_normalized = NULL,
		    request_error_reason = NULL,
		    response_error_reason = NULL
		FROM traffic_event t
		WHERE t.id = n.traffic_event_id
		  AND (
		    (t.source = 'ai-gateway' AND t.entity_id = $1)
		    OR (t.source = 'agent' AND EXISTS (
		      SELECT 1 FROM "DeviceAssignment" da
		      WHERE da."userId" = $1 AND da."deviceId" = t.thing_id
		        AND t.timestamp >= da."assignedAt"
		        AND (da."releasedAt" IS NULL OR t.timestamp < da."releasedAt")
		    ))
		  )
	`, subjectID)
	if err != nil {
		return nil, fmt.Errorf("scrub normalized bodies: %w", err)
	}
	result.NormalizedScrubbed = int(tagN.RowsAffected())

	// 2. Anonymise VK traffic identifying columns (name + identity snapshot too).
	tag1, err := tx.Exec(ctx, `
		UPDATE traffic_event
		SET entity_id = NULL, entity_name = NULL, identity = NULL, source_ip = NULL
		WHERE source = 'ai-gateway' AND entity_id = $1
	`, subjectID)
	if err != nil {
		return nil, fmt.Errorf("anonymise vk traffic: %w", err)
	}
	result.VKAnonymised = int(tag1.RowsAffected())

	// 3. Anonymise agent traffic within assignment windows.
	tag2, err := tx.Exec(ctx, `
		UPDATE traffic_event t
		SET source_ip = NULL, source_process = NULL, entity_name = NULL, identity = NULL
		FROM "DeviceAssignment" da
		WHERE da."userId" = $1
		  AND t.thing_id = da."deviceId"
		  AND t.source = 'agent'
		  AND t.timestamp >= da."assignedAt"
		  AND (da."releasedAt" IS NULL OR t.timestamp < da."releasedAt")
	`, subjectID)
	if err != nil {
		return nil, fmt.Errorf("anonymise agent traffic: %w", err)
	}
	result.AgentAnonymised = int(tag2.RowsAffected())

	// 4. Erase the subject's assistant data (transcripts, memory, files).
	var assistantErased int64
	for _, q := range []struct{ name, sql string }{
		{"assistant memory", `DELETE FROM "AssistantMemory" WHERE "userId" = $1`},
		{"assistant sessions", `DELETE FROM "AssistantSession" WHERE "userId" = $1`},
		{"assistant files", `DELETE FROM "AssistantFile" WHERE "userId" = $1`},
		{"assistant pending confirms", `DELETE FROM "AssistantPendingConfirm" WHERE "userId" = $1`},
		// The chat audit-chain rows carry no transcript content (digests +
		// counts only), but they are keyed to the subject — erasure removes
		// them with the sessions they attest.
		{"assistant chat events", `DELETE FROM "AssistantChatEvent" WHERE "userId" = $1`},
	} {
		tag, derr := tx.Exec(ctx, q.sql, subjectID)
		if derr != nil {
			return nil, fmt.Errorf("erase %s: %w", q.name, derr)
		}
		assistantErased += tag.RowsAffected()
	}
	result.AssistantErased = int(assistantErased)

	// 5. Scrub the persisted ACCESS export bodies for this subject. A prior
	//    ACCESS DSAR stored the full export (name, identity, prompt/response
	//    bodies) in dsar_request.outcome; erasure must null it so the correct
	//    ACCESS-then-ERASURE sequence does not leave residual PII.
	tagD, err := tx.Exec(ctx, `
		UPDATE dsar_request
		SET outcome = NULL
		WHERE subject_id = $1 AND type = 'ACCESS' AND outcome IS NOT NULL
	`, subjectID)
	if err != nil {
		return nil, fmt.Errorf("scrub prior access outcomes: %w", err)
	}
	result.AccessOutcomesScrubbed = int(tagD.RowsAffected())

	// 6. Delete the subject's owned keys, identity/credential rows, and the
	//    account record itself, in FK-correct order. The ordering is
	//    owned by usercascade — the SINGLE source shared with the admin hard
	//    delete (userstore.DeleteNexusUser) so the FK rules (ScimToken
	//    RESTRICT cleared first; SET-NULL owners deleted outright; NexusUser
	//    last) live in exactly one place. AdminAuditLog (the tamper-evident hash
	//    chain) is intentionally NOT touched — see the function doc's audit-trail
	//    retention exception.
	counts, err := usercascade.DeleteUserAccount(ctx, tx, subjectID)
	if err != nil {
		return nil, err
	}
	result.VKOwnedDeleted = counts.VKOwnedDeleted
	result.AdminApiKeysDeleted = counts.AdminApiKeysDeleted
	result.FederatedIdentitiesDeleted = counts.FederatedIdentitiesDeleted
	result.RefreshTokensDeleted = counts.RefreshTokensDeleted
	result.ScimTokensDeleted = counts.ScimTokensDeleted
	result.IamGroupMembershipsDeleted = counts.IamGroupMembershipsDeleted
	result.IamPolicyAttachmentsDeleted = counts.IamPolicyAttachmentsDeleted
	result.AccountDeleted = counts.AccountDeleted

	result.TotalAnonymised = result.VKAnonymised + result.AgentAnonymised

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit erasure: %w", err)
	}
	return result, nil
}

// DSARStatusCounts holds counts by DSAR status.
type DSARStatusCounts struct {
	Pending    int `json:"pending"`
	InProgress int `json:"inProgress"`
	Completed  int `json:"completed"`
	Rejected   int `json:"rejected"`
}

// GetDSARStatusCounts returns counts of DSAR requests by status using a single query.
func (store *Store) GetDSARStatusCounts(ctx context.Context) (*DSARStatusCounts, error) {
	var s DSARStatusCounts
	err := store.pool.QueryRow(ctx, `
		SELECT
			COUNT(*) FILTER (WHERE status = 'PENDING'),
			COUNT(*) FILTER (WHERE status = 'IN_PROGRESS'),
			COUNT(*) FILTER (WHERE status = 'COMPLETED'),
			COUNT(*) FILTER (WHERE status = 'REJECTED')
		FROM dsar_request
	`).Scan(&s.Pending, &s.InProgress, &s.Completed, &s.Rejected)
	if err != nil {
		return nil, fmt.Errorf("dsar status counts: %w", err)
	}
	return &s, nil
}

// GetDSARCompletedInPeriod returns count of DSAR requests completed in the given period.
func (store *Store) GetDSARCompletedInPeriod(ctx context.Context, start, end time.Time) (int, error) {
	var count int
	err := store.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM dsar_request
		WHERE status = 'COMPLETED' AND completed_at >= $1 AND completed_at <= $2
	`, start, end).Scan(&count)
	return count, err
}
