package providers

import (
	"context"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/ai/providers/credstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/keyderive"
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

// rotationResult is the outcome of a single rotation run.
type rotationResult struct {
	// rotated is the number of credentials successfully re-encrypted onto the
	// target key.
	rotated int
	// stuck is the number of distinct credentials that could not be rotated
	// (decrypt / encrypt / persist failure) and were skipped. The most common
	// cause is a row whose encryption_key_id is no longer present in
	// CREDENTIAL_KEY_MAP, which Decrypt rejects with "unknown encryption key
	// ID". A non-zero count means an operator must restore the missing key (or
	// delete the orphan rows) before that ciphertext can migrate.
	stuck int
}

// rotateCredentialsWorker is the goroutine entry point: it runs one rotation
// pass, logs the outcome, fans out a cache invalidation, and always releases
// the in-progress guard. The guard release is the liveness contract — every
// exit path (including the list-query error) must reach the deferred Store.
func (h *Handler) rotateCredentialsWorker(targetKeyID string) {
	defer rotationInProgress.Store(false)

	ctx := context.Background()
	res, err := h.rotateCredentials(ctx, targetKeyID)
	if err != nil {
		h.logger.Error("rotation worker: list failed", "error", err)
		return
	}

	h.logger.Info("credential key rotation complete",
		"rotated", res.rotated, "stuck", res.stuck, "targetKeyId", targetKeyID)

	// Notify downstream services (AI-Gateway) to refresh credential caches.
	if h.hub != nil {
		h.hub.InvalidateConfig(ctx, "ai-gateway", "credentials")
	}
}

// rotateCredentials re-encrypts every credential not yet on targetKeyID, in
// oldest-first batches, and returns the rotated/stuck counts.
//
// Termination is guaranteed: a credential that cannot be re-encrypted (decrypt,
// encrypt, or persist failure — e.g. an encryption_key_id no longer in
// CREDENTIAL_KEY_MAP) is recorded in the stuck set and excluded from every
// subsequent ListCredentialsForRotation query. A successful row leaves the
// candidate set because its encryption_key_id advances to targetKeyID. Either
// way each batch removes every row it touched from the candidate pool, so the
// pool strictly shrinks and the loop ends when a query returns no rows. This
// also lets healthy rows queued behind a run of stuck rows still rotate, rather
// than letting one stale-key row at the front block the whole migration.
func (h *Handler) rotateCredentials(ctx context.Context, targetKeyID string) (rotationResult, error) {
	const batchSize = 50
	var res rotationResult
	seen := make(map[string]struct{})
	excludeIDs := []string{}

	for {
		creds, err := h.creds.ListCredentialsForRotation(ctx, targetKeyID, excludeIDs, batchSize)
		if err != nil {
			return res, err
		}
		if len(creds) == 0 {
			break
		}

		for _, cred := range creds {
			if h.rotateOne(ctx, cred) {
				res.rotated++
				continue
			}
			// Stuck row: never re-query it, so the loop can terminate.
			if _, dup := seen[cred.ID]; !dup {
				seen[cred.ID] = struct{}{}
				excludeIDs = append(excludeIDs, cred.ID)
			}
		}

		// Small pause between batches to avoid overwhelming the database.
		time.Sleep(100 * time.Millisecond)
	}

	res.stuck = len(seen)
	return res, nil
}

// rotateOne re-encrypts a single credential onto the current (target) key and
// persists the new ciphertext. It returns true only when the row was fully
// migrated; any decrypt / encrypt / persist failure is logged and returns false
// so the caller records the row as stuck.
func (h *Handler) rotateOne(ctx context.Context, cred credstore.CredentialEncrypted) bool {
	// The ciphertext is AAD-bound to this row's identity; pass the
	// same AAD on decrypt and re-encrypt so a key rotation preserves the binding
	// (and a row whose blob was swapped in fails here instead of being rotated
	// forward as the wrong upstream key).
	aad := keyderive.ProviderCredentialAAD(cred.ID, cred.ProviderID)

	// Decrypt with the key that was used to encrypt this credential.
	plaintext, err := h.multiVault.Decrypt(
		cred.EncryptionKeyID, cred.EncryptedKey, cred.EncryptionIV, cred.EncryptionTag, aad)
	if err != nil {
		h.logger.Error("rotation: decrypt failed",
			"credId", cred.ID, "keyId", cred.EncryptionKeyID, "error", err)
		return false
	}

	// Re-encrypt with the current (target) key, same AAD.
	result, keyID, err := h.multiVault.Encrypt(plaintext, aad)
	if err != nil {
		h.logger.Error("rotation: encrypt failed", "credId", cred.ID, "error", err)
		return false
	}

	// Persist re-encrypted fields.
	if err := h.creds.UpdateCredentialEncryption(ctx, cred.ID,
		result.Ciphertext, result.IV, result.Tag, keyID); err != nil {
		h.logger.Error("rotation: update failed", "credId", cred.ID, "error", err)
		return false
	}
	return true
}
