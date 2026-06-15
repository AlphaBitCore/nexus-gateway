package providers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/ai/providers/credstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/crypto"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/hub"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/keyderive"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/credstate"
)

// RegisterCredentialRoutes registers credential CRUD routes.
func (h *Handler) RegisterCredentialRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	g.GET("/credentials", h.ListCredentials, iamMW(iam.ResourceCredential.Action(iam.VerbRead)))
	g.POST("/credentials", h.CreateCredential, iamMW(iam.ResourceCredential.Action(iam.VerbCreate)))
	g.GET("/credentials/rotation-status", h.CredentialRotationStatus, iamMW(iam.ResourceCredential.Action(iam.VerbRead)))
	g.POST("/credentials/rotate-key", h.RotateCredentialKey, iamMW(iam.ResourceCredential.Action(iam.VerbRotate)))
	g.GET("/credentials/key-rotation-status", h.GetKeyRotationStatus, iamMW(iam.ResourceCredential.Action(iam.VerbRead)))
	g.GET("/credentials/:id", h.GetCredential, iamMW(iam.ResourceCredential.Action(iam.VerbRead)))
	g.PUT("/credentials/:id", h.UpdateCredential, iamMW(iam.ResourceCredential.Action(iam.VerbUpdate)))
	g.DELETE("/credentials/:id", h.DeleteCredential, iamMW(iam.ResourceCredential.Action(iam.VerbDelete)))
	g.POST("/credentials/:id/circuit-reset", h.CircuitReset, iamMW(iam.ResourceCredential.Action(iam.VerbUpdate)))
	g.POST("/credentials/:id/probe", h.ProbeCredential, iamMW(iam.ResourceCredential.Action(iam.VerbProbe)))
	g.PUT("/credentials/:id/reliability-overrides", h.UpdateCredentialReliabilityOverrides, iamMW(iam.ResourceCredential.Action(iam.VerbUpdate)))
}

func (h *Handler) ListCredentials(c echo.Context) error {
	pg := parsePagination(c)
	params := credstore.CredentialListParams{
		Q:          c.QueryParam("q"),
		ProviderID: c.QueryParam("providerId"),
		Limit:      pg.Limit,
		Offset:     pg.Offset,
	}
	if v := c.QueryParam("enabled"); v == "true" {
		t := true
		params.Enabled = &t
	} else if v == "false" {
		f := false
		params.Enabled = &f
	}

	creds, total, err := h.creds.ListCredentials(c.Request().Context(), params)
	if err != nil {
		h.logger.Error("list credentials", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to list credentials", "server_error", "INTERNAL_ERROR"))
	}
	return c.JSON(http.StatusOK, map[string]any{"data": creds, "total": total})
}

func (h *Handler) GetCredential(c echo.Context) error {
	ctx := c.Request().Context()
	cred, err := h.creds.GetCredential(ctx, c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to get credential", "server_error", "INTERNAL_ERROR"))
	}
	if cred == nil {
		return c.JSON(http.StatusNotFound, errJSON("Credential not found", "not_found", "NOT_FOUND"))
	}
	return c.JSON(http.StatusOK, h.withCircuit(ctx, cred))
}

func (h *Handler) CreateCredential(c echo.Context) error {
	if h.multiVault == nil && h.vault == nil {
		return c.JSON(http.StatusServiceUnavailable, errJSON("Credential vault not available — encryption key not configured", "server_error", "VAULT_UNAVAILABLE"))
	}

	var body struct {
		Name            string     `json:"name"`
		ProviderID      string     `json:"providerId"`
		APIKey          string     `json:"apiKey"`
		Enabled         *bool      `json:"enabled"`
		RotationState   string     `json:"rotationState"`
		ExpiresAt       *time.Time `json:"expiresAt"`
		SelectionWeight *int       `json:"selectionWeight"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}
	if body.Name == "" || body.ProviderID == "" || body.APIKey == "" {
		return c.JSON(http.StatusBadRequest, errJSON("name, providerId, and apiKey are required", "validation_error", "VALIDATION_ERROR"))
	}

	// Generate the credential id app-side BEFORE sealing so the
	// ciphertext is AAD-bound to its own immutable row identity. A blob copied
	// from another credential's row then fails GCM auth on decrypt instead of
	// yielding the wrong upstream key.
	credID := uuid.New().String()
	aad := keyderive.ProviderCredentialAAD(credID, body.ProviderID)

	var enc *crypto.EncryptResult
	var keyID string
	var encErr error
	if h.multiVault != nil {
		enc, keyID, encErr = h.multiVault.Encrypt(body.APIKey, aad)
	} else {
		enc, encErr = h.vault.Encrypt(body.APIKey, aad)
		keyID = "v1"
	}
	if encErr != nil {
		h.logger.Error("encrypt credential", "error", encErr)
		return c.JSON(http.StatusInternalServerError, errJSON("Encryption failed", "server_error", "ENCRYPTION_ERROR"))
	}

	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	rotationState := "none"
	if body.RotationState != "" {
		rotationState = body.RotationState
	}

	weight := 100
	if body.SelectionWeight != nil && *body.SelectionWeight > 0 {
		weight = *body.SelectionWeight
	}
	cred, err := h.creds.CreateCredential(c.Request().Context(), credstore.CreateCredentialParams{
		ID:              credID,
		Name:            body.Name,
		ProviderID:      body.ProviderID,
		EncryptedKey:    enc.Ciphertext,
		EncryptionIV:    enc.IV,
		EncryptionTag:   enc.Tag,
		EncryptionKeyID: keyID,
		Enabled:         enabled,
		RotationState:   rotationState,
		ExpiresAt:       body.ExpiresAt,
		SelectionWeight: weight,
	})
	if err != nil {
		h.logger.Error("create credential", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to create credential", "server_error", "INTERNAL_ERROR"))
	}

	if h.hub != nil {
		if err := h.hub.InvalidateConfigE(c.Request().Context(), "ai-gateway", "credentials"); err != nil {
			h.logger.Error("create credential: hub invalidate failed", "id", cred.ID, "error", err)
			return hub.RespondPropagationFailure(c, err)
		}
	}

	ae := audit.EntryFor(c, iam.ResourceCredential, iam.VerbCreate)
	ae.EntityID = cred.ID
	ae.AfterState = map[string]any{"id": cred.ID, "name": cred.Name, "providerId": cred.ProviderID}
	if err := h.audit.LogCritical(c.Request().Context(), ae); err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Audit failure", "server_error", "AUDIT_FAILURE"))
	}

	return c.JSON(http.StatusCreated, cred)
}

func (h *Handler) UpdateCredential(c echo.Context) error {
	id := c.Param("id")
	existing, err := h.creds.GetCredential(c.Request().Context(), id)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to get credential", "server_error", "INTERNAL_ERROR"))
	}
	if existing == nil {
		return c.JSON(http.StatusNotFound, errJSON("Credential not found", "not_found", "NOT_FOUND"))
	}

	// Read raw body so we can distinguish absent / null / value for expiresAt.
	raw, err := io.ReadAll(c.Request().Body)
	if err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}
	var body struct {
		APIKey          string `json:"apiKey"`
		Name            string `json:"name"`
		Enabled         *bool  `json:"enabled"`
		RotationState   string `json:"rotationState"`
		SelectionWeight *int   `json:"selectionWeight"`
		Status          string `json:"status"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
		}
	}

	// 3-state expiresAt + retireAt: absent=no-op, null=clear, value=set.
	var expiresAtParam **time.Time
	var retireAtParam **time.Time
	{
		var rawFields map[string]json.RawMessage
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &rawFields); err == nil {
				if rv, ok := rawFields["expiresAt"]; ok {
					var t *time.Time
					if err := json.Unmarshal(rv, &t); err == nil {
						expiresAtParam = &t
					}
				}
				if rv, ok := rawFields["retireAt"]; ok {
					var t *time.Time
					if err := json.Unmarshal(rv, &t); err == nil {
						retireAtParam = &t
					}
				}
			}
		}
	}

	ctx := c.Request().Context()

	// Update encrypted key if provided
	apiKeyUpdated := false
	if body.APIKey != "" {
		if h.multiVault == nil && h.vault == nil {
			return c.JSON(http.StatusServiceUnavailable, errJSON("Credential vault not available", "server_error", "VAULT_UNAVAILABLE"))
		}
		// Re-seal under the same row-identity AAD (immutable id +
		// provider) as the create path, so the rotated ciphertext stays bound.
		aad := keyderive.ProviderCredentialAAD(id, existing.ProviderID)
		var enc *crypto.EncryptResult
		var keyID string
		var encErr error
		if h.multiVault != nil {
			enc, keyID, encErr = h.multiVault.Encrypt(body.APIKey, aad)
		} else {
			enc, encErr = h.vault.Encrypt(body.APIKey, aad)
			keyID = "v1"
		}
		if encErr != nil {
			return c.JSON(http.StatusInternalServerError, errJSON("Encryption failed", "server_error", "ENCRYPTION_ERROR"))
		}
		if err := h.creds.UpdateCredentialEncryption(ctx, id, enc.Ciphertext, enc.IV, enc.Tag, keyID); err != nil {
			return c.JSON(http.StatusInternalServerError, errJSON("Failed to update credential", "server_error", "INTERNAL_ERROR"))
		}
		apiKeyUpdated = true
	}

	// Update metadata
	var params credstore.UpdateCredentialParams
	hasMetaUpdate := false
	if body.Name != "" {
		name := body.Name
		params.Name = &name
		hasMetaUpdate = true
	}
	if body.Enabled != nil {
		params.Enabled = body.Enabled
		hasMetaUpdate = true
	}
	if body.RotationState != "" {
		validStates := map[string]bool{
			"none": true, "pending_rotation": true, "validating": true,
			"rotated": true, "completed": true, "failed": true,
		}
		if validStates[body.RotationState] {
			rs := body.RotationState
			params.RotationState = &rs
			hasMetaUpdate = true
			if body.RotationState == "completed" || body.RotationState == "rotated" {
				now := time.Now().UTC()
				params.LastRotatedAt = &now
			}
		}
	} else if apiKeyUpdated {
		// API key replaced without an explicit rotationState: treat as completed rotation.
		completed := "completed"
		now := time.Now().UTC()
		params.RotationState = &completed
		params.LastRotatedAt = &now
		hasMetaUpdate = true
	}
	if expiresAtParam != nil {
		params.UpdateExpiresAt = true
		params.ExpiresAt = *expiresAtParam
		hasMetaUpdate = true
	}
	if body.SelectionWeight != nil {
		params.SelectionWeight = body.SelectionWeight
		hasMetaUpdate = true
	}
	if body.Status != "" {
		validStatuses := map[string]bool{"active": true, "retiring": true, "retired": true}
		if validStatuses[body.Status] {
			s := body.Status
			params.Status = &s
			hasMetaUpdate = true
			// When admin manually retires a credential, set retireAt = now + 7 days.
			if body.Status == "retired" && retireAtParam == nil {
				t := time.Now().UTC().AddDate(0, 0, 7)
				tp := &t
				retireAtParam = &tp
			}
		}
	}
	if retireAtParam != nil {
		params.UpdateRetireAt = true
		params.RetireAt = *retireAtParam
		hasMetaUpdate = true
	}

	var updated *credstore.Credential
	if hasMetaUpdate {
		updated, err = h.creds.UpdateCredentialMetadata(ctx, id, params)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, errJSON("Failed to update credential", "server_error", "INTERNAL_ERROR"))
		}
	} else {
		updated, err = h.creds.GetCredential(ctx, id)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, errJSON("Failed to fetch credential", "server_error", "INTERNAL_ERROR"))
		}
	}

	if h.hub != nil {
		if err := h.hub.InvalidateConfigE(ctx, "ai-gateway", "credentials"); err != nil {
			h.logger.Error("update credential: hub invalidate failed", "id", id, "error", err)
			return hub.RespondPropagationFailure(c, err)
		}
	}

	ae := audit.EntryFor(c, iam.ResourceCredential, iam.VerbUpdate)
	ae.EntityID = id
	if err := h.audit.LogCritical(ctx, ae); err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Audit failure", "server_error", "AUDIT_FAILURE"))
	}

	return c.JSON(http.StatusOK, updated)
}

func (h *Handler) DeleteCredential(c echo.Context) error {
	id := c.Param("id")
	existing, err := h.creds.GetCredential(c.Request().Context(), id)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to get credential", "server_error", "INTERNAL_ERROR"))
	}
	if existing == nil {
		return c.JSON(http.StatusNotFound, errJSON("Credential not found", "not_found", "NOT_FOUND"))
	}

	if err := h.creds.DeleteCredential(c.Request().Context(), id); err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to delete credential", "server_error", "INTERNAL_ERROR"))
	}

	if h.hub != nil {
		if err := h.hub.InvalidateConfigE(c.Request().Context(), "ai-gateway", "credentials"); err != nil {
			h.logger.Error("delete credential: hub invalidate failed", "id", id, "error", err)
			return hub.RespondPropagationFailure(c, err)
		}
	}

	ae := audit.EntryFor(c, iam.ResourceCredential, iam.VerbDelete)
	ae.EntityID = id
	if err := h.audit.LogCritical(c.Request().Context(), ae); err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Audit failure", "server_error", "AUDIT_FAILURE"))
	}

	return c.JSON(http.StatusOK, map[string]any{"deleted": true, "id": id})
}

func (h *Handler) CredentialRotationStatus(c echo.Context) error {
	creds, _, err := h.creds.ListCredentials(c.Request().Context(), credstore.CredentialListParams{
		Limit: 1000, Offset: 0,
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to get rotation status", "server_error", "INTERNAL_ERROR"))
	}

	type rotationItem struct {
		ID                string `json:"id"`
		Name              string `json:"name"`
		ProviderID        string `json:"providerId"`
		RotationState     any    `json:"rotationState"`
		LastRotatedAt     any    `json:"lastRotatedAt"`
		DaysSinceRotation int    `json:"daysSinceRotation"`
		Overdue           bool   `json:"overdue"`
	}

	now := time.Now().UTC()
	data := make([]rotationItem, 0, len(creds))
	for _, c := range creds {
		lastRotated := c.CreatedAt
		if c.LastRotatedAt != nil {
			lastRotated = *c.LastRotatedAt
		}
		days := int(now.Sub(lastRotated).Hours() / 24)
		data = append(data, rotationItem{
			ID: c.ID, Name: c.Name, ProviderID: c.ProviderID,
			RotationState: c.RotationState, LastRotatedAt: lastRotated,
			DaysSinceRotation: days, Overdue: days > 90,
		})
	}
	return c.JSON(http.StatusOK, map[string]any{"data": data})
}

// CircuitReset force-closes a credential's circuit breaker, allowing it to
// re-enter the eligible pool. Admin action; used after a credential opened its
// circuit (repeated 401/403 auth failures, or a 429 rate-limit).
//
// A reset MUST reconcile BOTH stores or it does not take effect:
//   - Redis cred:circuit:{id} is the live state the AI Gateway reads.
//   - The Credential.circuit* DB columns are what the UI badge shows AND what
//     the Hub rehydrate-on-restart path re-arms Redis from.
//
// Deleting only Redis (the historical behavior) left the DB row "open": the UI
// stayed "Open" forever, and because the reset never marked the dirty set, the
// Hub flush job never reconciled it — so nothing ever closed the row. Worse, a
// later Hub restart re-armed the live circuit from the stale DB row. We now
// clear the DB columns directly so the reset is durable and instantly visible.
func (h *Handler) CircuitReset(c echo.Context) error {
	ctx := c.Request().Context()
	id := c.Param("id")
	cred, err := h.creds.GetCredential(ctx, id)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to get credential", "server_error", "INTERNAL_ERROR"))
	}
	if cred == nil {
		return c.JSON(http.StatusNotFound, errJSON("Credential not found", "not_found", "NOT_FOUND"))
	}
	// Clear the durable DB state first — it is the source of truth for the UI
	// and rehydrate. A failure here is fatal to the reset (return 500); a stale
	// Redis key alone self-heals (cooldown promote / reconcile), but a stale DB
	// row does not.
	if err := h.creds.ClearCircuit(ctx, id); err != nil {
		h.logger.Error("circuit-reset: db clear failed", "credentialID", id, "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to reset circuit", "server_error", "INTERNAL_ERROR"))
	}
	// Clear the live Redis hash so the gateway sees the credential as closed
	// immediately, and drop any pending dirty marker so a concurrent flush
	// cannot re-derive the just-cleared state. Best-effort: the DB is already
	// authoritative and the Hub reconcile will converge any residue.
	if h.redis != nil {
		if err := h.redis.Del(ctx, credstate.CircuitKey(id)).Err(); err != nil {
			h.logger.Warn("circuit-reset: redis del failed", "credentialID", id, "error", err)
		}
		if err := h.redis.SRem(ctx, credstate.CircuitDirtySet, id).Err(); err != nil {
			h.logger.Warn("circuit-reset: redis dirty-set srem failed", "credentialID", id, "error", err)
		}
	}
	ae := audit.EntryFor(c, iam.ResourceCredential, iam.VerbUpdate)
	ae.EntityID = id
	if err := h.audit.LogCritical(ctx, ae); err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Audit failure", "server_error", "AUDIT_FAILURE"))
	}
	return c.JSON(http.StatusOK, map[string]any{"reset": true, "id": id})
}

// credentialWithCircuit is the API response shape for a single credential.
// It merges the persistent state in the Credential row (already inlined via
// the embedded *credstore.Credential — see store/credential.go for the
// circuitState / circuitReason / health* fields) with the live Redis-backed
// view of the same circuit. The embedded fields are the durable last-flushed
// values; the LiveCircuit struct carries the up-to-the-moment Redis view
// admin operators want when diagnosing a flaky credential.
type credentialWithCircuit struct {
	*credstore.Credential
	LiveCircuit *liveCircuitView `json:"liveCircuit,omitempty"`
}

// liveCircuitView is the Redis HGETALL snapshot for cred:circuit:{id}.
// All fields except State may be omitted when Redis returns an empty hash
// (state defaults to "closed"). AuthFailsCurrent is the only field that is
// *not* persisted to the Credential table by design — it is intentionally
// ephemeral to keep DB writes proportional to state transitions, not
// per-attempt auth-fail counter increments.
type liveCircuitView struct {
	State            string `json:"state"`
	OpenReason       string `json:"openReason,omitempty"`
	OpenedAt         string `json:"openedAt,omitempty"`
	NextProbeAt      string `json:"nextProbeAt,omitempty"`
	AuthFailsCurrent int    `json:"authFailsCurrent"`
}

// withCircuit returns the credential merged with its live Redis-side circuit
// view. The persistent circuit fields are already on the embedded
// *credstore.Credential via the new credential schema columns.
func (h *Handler) withCircuit(ctx context.Context, cred *credstore.Credential) credentialWithCircuit {
	out := credentialWithCircuit{Credential: cred}
	if h.redis == nil {
		return out
	}
	fields, err := h.redis.HGetAll(ctx, "cred:circuit:"+cred.ID).Result()
	if err != nil || len(fields) == 0 {
		return out
	}
	live := &liveCircuitView{State: "closed"}
	if s := fields["state"]; s != "" {
		live.State = s
	}
	live.OpenReason = fields["open_reason"]
	live.OpenedAt = fields["opened_at"]
	live.NextProbeAt = fields["next_probe_at"]
	if raw, ok := fields["auth_fails"]; ok && raw != "" {
		// Parse defensively; an unparseable counter just yields 0 — we'd rather
		// show "0" than blow up the whole response.
		if n, perr := strconv.Atoi(raw); perr == nil {
			live.AuthFailsCurrent = n
		}
	}
	out.LiveCircuit = live
	return out
}
