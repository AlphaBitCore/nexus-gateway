package providers

import (
	"context"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
	"github.com/labstack/echo/v4"
)

// rotationInProgress guards against concurrent key rotation runs.
var rotationInProgress atomic.Bool

// RotateCredentialKey triggers background re-encryption of credentials from old
// encryption keys to the current key.
// POST /api/admin/credentials/rotate-key
func (h *Handler) RotateCredentialKey(c echo.Context) error {
	if h.multiVault == nil {
		return c.JSON(http.StatusBadRequest, errJSON(
			"CREDENTIAL_KEY_MAP not configured", "configuration_error", "CONFIGURATION_ERROR"))
	}

	if !rotationInProgress.CompareAndSwap(false, true) {
		return c.JSON(http.StatusConflict, errJSON(
			"Rotation already in progress", "conflict_error", "ROTATION_IN_PROGRESS"))
	}

	targetKeyID := h.multiVault.CurrentKeyID()
	pending, err := h.creds.CountCredentialsNotOnKey(c.Request().Context(), targetKeyID)
	if err != nil {
		rotationInProgress.Store(false)
		h.logger.Error("rotate-key: count pending", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON(
			"Failed to count pending credentials", "server_error", "INTERNAL_ERROR"))
	}

	go h.rotateCredentialsWorker(targetKeyID)

	ae := audit.EntryFor(c, iam.ResourceCredential, iam.VerbRotate)
	ae.EntityID = targetKeyID
	ae.AfterState = map[string]any{"targetKeyId": targetKeyID, "pendingCount": pending}
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusOK, map[string]any{
		"status":       "rotating",
		"targetKeyId":  targetKeyID,
		"pendingCount": pending,
	})
}

// GetKeyRotationStatus returns current encryption-key rotation progress.
// GET /api/admin/credentials/key-rotation-status
func (h *Handler) GetKeyRotationStatus(c echo.Context) error {
	if h.multiVault == nil {
		return c.JSON(http.StatusOK, map[string]any{"status": "not_configured"})
	}

	targetKeyID := h.multiVault.CurrentKeyID()
	pending, _ := h.creds.CountCredentialsNotOnKey(c.Request().Context(), targetKeyID)

	status := "idle"
	if rotationInProgress.Load() {
		status = "rotating"
	}

	return c.JSON(http.StatusOK, map[string]any{
		"status":       status,
		"targetKeyId":  targetKeyID,
		"pendingCount": pending,
	})
}

// rotateCredentialsWorker re-encrypts all credentials from their current key to
// targetKeyID in batches, then publishes an invalidation event.
func (h *Handler) rotateCredentialsWorker(targetKeyID string) {
	defer rotationInProgress.Store(false)

	ctx := context.Background()
	const batchSize = 50
	rotated := 0

	for {
		creds, err := h.creds.ListCredentialsForRotation(ctx, targetKeyID, batchSize)
		if err != nil {
			h.logger.Error("rotation worker: list failed", "error", err)
			return
		}
		if len(creds) == 0 {
			break
		}

		for _, cred := range creds {
			// Decrypt with the key that was used to encrypt this credential.
			plaintext, err := h.multiVault.Decrypt(
				cred.EncryptionKeyID, cred.EncryptedKey, cred.EncryptionIV, cred.EncryptionTag)
			if err != nil {
				h.logger.Error("rotation: decrypt failed",
					"credId", cred.ID, "keyId", cred.EncryptionKeyID, "error", err)
				continue
			}

			// Re-encrypt with the current (target) key.
			result, keyID, err := h.multiVault.Encrypt(plaintext)
			if err != nil {
				h.logger.Error("rotation: encrypt failed", "credId", cred.ID, "error", err)
				continue
			}

			// Persist re-encrypted fields.
			if err := h.creds.UpdateCredentialEncryption(ctx, cred.ID,
				result.Ciphertext, result.IV, result.Tag, keyID); err != nil {
				h.logger.Error("rotation: update failed", "credId", cred.ID, "error", err)
				continue
			}
			rotated++
		}

		// Small pause between batches to avoid overwhelming the database.
		time.Sleep(100 * time.Millisecond)
	}

	h.logger.Info("credential key rotation complete",
		"rotated", rotated, "targetKeyId", targetKeyID)

	// Notify downstream services (AI-Gateway) to refresh credential caches.
	if h.hub != nil {
		h.hub.InvalidateConfig(ctx, "ai-gateway", "credentials")
	}
}
