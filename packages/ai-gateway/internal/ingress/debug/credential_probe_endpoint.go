package debug

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// credentialCache is the DB-snapshot seam consumed by the probe handler.
// *cachelayer.Layer satisfies this interface at the wiring call site via
// Go structural typing — no changes to that package are required. The
// seam lets the handler be unit-tested without a live PostgreSQL connection.
type credentialCache interface {
	GetCredentialByID(ctx context.Context, id string) (*store.Credential, error)
	GetProvider(ctx context.Context, id string) (*store.Provider, error)
}

// credentialDecrypter is the decrypt seam consumed by the probe handler.
// *credmanager.Manager satisfies this interface at the wiring call site
// via Go structural typing.
type credentialDecrypter interface {
	GetDecrypted(ctx context.Context, credentialID string) (string, error)
}

// CredentialProbeResult is the JSON returned by POST
// /internal/v1/credentials/{id}/probe. Stable shape — the Control Plane
// admin API forwards this body to the browser verbatim under
// POST /api/admin/credentials/{id}/probe (see e41-s5 OpenAPI).
type CredentialProbeResult struct {
	OK             bool   `json:"ok"`
	LatencyMs      int    `json:"latencyMs"`
	Detail         string `json:"detail,omitempty"`
	Error          string `json:"error,omitempty"`
	ProviderName   string `json:"providerName,omitempty"`
	AdapterType    string `json:"adapterType,omitempty"`
	CredentialID   string `json:"credentialId"`
	CredentialName string `json:"credentialName,omitempty"`
	ProbedAt       string `json:"probedAt"`
}

// CredentialProbeHandler issues a minimal upstream "ping" using a specific
// credential. The operator clicks "Test credential" in the admin UI →
// Control Plane proxies to this endpoint → we resolve the credential
// (decrypt key, look up provider's base URL + adapter type) and call
// adapter.Probe against the real provider with a configurable timeout.
//
// Path: POST /internal/v1/credentials/{id}/probe
// Body: optional {"timeoutSeconds": int} (default 5, capped 30).
//
// No traffic_event is written by this handler — probes are diagnostic
// and operators want them visible separately from production calls.
//
// Decrypts via credmanager.Manager so both single-key and multi-key
// modes work — Manager.decrypt() dispatches to whichever decryptor was
// configured at boot.
func CredentialProbeHandler(cache credentialCache, reg *provcore.Registry, credMgr credentialDecrypter, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		credID := r.PathValue("id")
		if credID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"ok": false, "error": "credential id required",
			})
			return
		}

		var body struct {
			TimeoutSeconds int `json:"timeoutSeconds"`
		}
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		timeout := 5 * time.Second
		if body.TimeoutSeconds > 0 {
			t := time.Duration(body.TimeoutSeconds) * time.Second
			if t > 30*time.Second {
				t = 30 * time.Second
			}
			timeout = t
		}

		probeCtx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()

		cred, err := cache.GetCredentialByID(probeCtx, credID)
		if err != nil || cred == nil {
			writeJSON(w, http.StatusNotFound, map[string]any{
				"ok":           false,
				"error":        "credential not found",
				"credentialId": credID,
			})
			return
		}
		provider, err := cache.GetProvider(probeCtx, cred.ProviderID)
		if err != nil || provider == nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"ok":           false,
				"error":        "provider not found for credential",
				"credentialId": credID,
			})
			return
		}

		format := provcore.Format(strings.ToLower(strings.TrimSpace(provider.AdapterType)))
		if !format.Valid() {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"ok":           false,
				"error":        "invalid provider adapterType: " + provider.AdapterType,
				"credentialId": credID,
			})
			return
		}
		adapter, ok := reg.Get(format)
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"ok":           false,
				"error":        "no adapter registered for format: " + string(format),
				"credentialId": credID,
			})
			return
		}

		apiKey, err := credMgr.GetDecrypted(probeCtx, credID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"ok":           false,
				"error":        "decrypt credential: " + err.Error(),
				"credentialId": credID,
			})
			return
		}

		target := provcore.CallTarget{
			ProviderName: provider.Name,
			Format:       format,
			BaseURL:      provider.BaseURL,
			APIKey:       apiKey,
		}
		probedAt := time.Now().UTC()
		// Adapter.Probe returns a nil *ProbeResult when probeErr is set —
		// dereference only on the success branch so an SDK-level failure
		// (timeout, DNS, refused connection) still produces a valid JSON
		// response rather than panicking the handler.
		result, probeErr := adapter.Probe(probeCtx, target)
		latencyMs := int(time.Since(probedAt).Milliseconds())

		out := CredentialProbeResult{
			LatencyMs:      latencyMs,
			ProviderName:   provider.Name,
			AdapterType:    string(format),
			CredentialID:   cred.ID,
			CredentialName: cred.Name,
			ProbedAt:       probedAt.Format(time.RFC3339),
		}
		if probeErr != nil {
			out.OK = false
			out.Error = probeErr.Error()
		} else if result != nil {
			out.OK = result.OK
			out.Detail = result.Detail
			if result.LatencyMs > 0 {
				out.LatencyMs = int(result.LatencyMs)
			}
		}

		if logger != nil {
			logger.Info("credential probe",
				"credentialId", cred.ID,
				"credentialName", cred.Name,
				"provider", provider.Name,
				"ok", out.OK,
				"latencyMs", out.LatencyMs,
				"detail", out.Detail,
			)
		}
		writeJSON(w, http.StatusOK, out)
	}
}
