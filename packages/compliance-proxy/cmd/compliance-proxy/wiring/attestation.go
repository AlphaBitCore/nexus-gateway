package wiring

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/cmd/compliance-proxy/config"
	proxyserver "github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/proxy/server"
	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/tlsbump"
)

// InitAttestationVerifier constructs the attestation verifier the CONNECT
// path consults. Returns nil when the per-cluster feature flag is off —
// the proxy then skips the header peek entirely.
//
// The key-cache loader calls Hub's GET /api/internal/things/:id/attestation-pubkey
// with the same internal-service-token CP uses for every other internal Hub call.
// 404 maps to tlsbump.ErrUnknownAgent so the cache's negative-TTL branch
// dampens scan-the-key-space attacks. Other HTTP/transport errors map to a
// generic loader error that the verifier translates into the unknown_agent
// outcome (fail-open).
func InitAttestationVerifier(cfg *config.Config, logger *slog.Logger) *proxyserver.AttestationVerifier {
	if cfg == nil || !cfg.Compliance.AttestationEnabled {
		return nil
	}
	hubURL := strings.TrimSuffix(cfg.Registry.NexusHubURL, "/")
	if hubURL == "" {
		logger.Warn("attestation enabled but registry.nexusHubUrl is empty — disabling verifier")
		return nil
	}
	if cfg.Auth.InternalServiceToken == "" {
		logger.Warn("attestation enabled but auth.internalServiceToken is empty — disabling verifier")
		return nil
	}

	client := nexushttp.New(nexushttp.Config{
		Timeout:        5 * time.Second,
		Caller:         "cp-attestation-key-loader",
		PropagateReqID: false,
	})
	loader := func(ctx context.Context, agentID string) (ed25519.PublicKey, error) {
		return fetchAttestationPubKey(ctx, client, hubURL, agentID, cfg.Auth.InternalServiceToken)
	}
	keyCache := tlsbump.NewAttestationKeyCache(loader, logger)
	replay := tlsbump.NewAttestationReplayCache()
	return proxyserver.NewAttestationVerifier(keyCache, replay, true, logger)
}

// fetchAttestationPubKey is the Hub HTTP loader plugged into the
// AttestationKeyCache. Factored out for unit testability — callers
// inject a custom client + base URL against an httptest.Server.
func fetchAttestationPubKey(ctx context.Context, client *http.Client, hubURL, agentID, token string) (ed25519.PublicKey, error) {
	url := hubURL + "/api/internal/things/" + agentID + "/attestation-pubkey"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("attestation pubkey: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("attestation pubkey: hub fetch: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	switch resp.StatusCode {
	case http.StatusOK:
		// fall through
	case http.StatusNotFound:
		return nil, tlsbump.ErrUnknownAgent
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("attestation pubkey: hub returned %d: %s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload struct {
		AgentID   string `json:"agentId"`
		PublicKey string `json:"publicKey"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("attestation pubkey: decode: %w", err)
	}
	if payload.PublicKey == "" {
		return nil, tlsbump.ErrUnknownAgent
	}
	raw, err := base64.StdEncoding.DecodeString(payload.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("attestation pubkey: base64 decode: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("attestation pubkey: wrong size %d", len(raw))
	}
	return ed25519.PublicKey(raw), nil
}

// keep the errors import alive for the errors.As call in fetchAttestationPubKey.
var _ = errors.New
