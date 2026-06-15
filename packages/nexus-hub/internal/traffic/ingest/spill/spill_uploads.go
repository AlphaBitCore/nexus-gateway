package spill

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/identity/handler/enroll"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillupload"
	nexushttperr "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/httperr"
)

// SpillUploadAPI implements the spill upload endpoints:
//
//   - POST /api/internal/things/spill-uploads
//     Mints a one-shot upload URL plus an HMAC token. The agent PUTs
//     the body to the URL, then submits the audit envelope carrying
//     the resulting SpillRef. The mint endpoint requires the agent's
//     mTLS thing identity (existing DeviceOrServiceAuth on the
//     `/api/internal/things` group).
//
//   - PUT /api/internal/spill/blob/:token
//     Localfs-only sink. Verifies the HMAC token, dedups via Redis
//     SETNX, validates Content-Length, streams the body into the
//     localfs SpillStore while computing a running SHA-256, and
//     rejects hash mismatches by deleting the partial file.
//
// Hub never decides "should we spill?" on the audit ingestion path —
// that decision is the agent's. This API is pure infrastructure: token
// minting + token-gated upload sink. Agents with direct S3 IAM
// credentials can skip the blob endpoint and PUT to S3 directly.
type SpillUploadAPI struct {
	// Spill is the local backend the blob endpoint writes to and the
	// mint endpoint generates keys against. Required.
	Spill spillstore.SpillStore

	// SpillBackend mirrors cfg.Spill.Backend so the mint endpoint can
	// decide between an S3 pre-signed URL ("s3") and the in-Hub
	// /spill/blob/:token URL ("localfs"). Anything else is rejected
	// with 503.
	SpillBackend string

	// PerObjectCap is the hard ceiling on a single upload size. Bodies
	// larger than this are rejected at mint with 413 — operators tune
	// this via spill.{localfs|s3}.perObjectCap YAML.
	PerObjectCap int64

	// Secrets serves Active() at mint time and Lookup() at verify time.
	// Required. Typed as the SecretSource interface (production passes a
	// *spillupload.SecretStore) so the sign/verify failure modes are testable.
	Secrets spillupload.SecretSource

	// Dedup gates blob-endpoint replay protection. Required when
	// SpillBackend == "localfs"; nil disables the dev sink with 503.
	Dedup spillupload.Dedup

	// HubURL is the externally-reachable base URL the mint endpoint
	// stamps onto localfs upload URLs (`<HubURL>/api/internal/spill/blob/<token>`).
	// When empty the mint endpoint falls back to the request's
	// scheme + Host header so dev (localhost:3060) and prod (LB
	// hostname) both work without explicit YAML configuration.
	HubURL string

	// Logger receives one log line per mint and per blob accept/reject.
	// Token bytes are NEVER logged; only structural fields (eventId,
	// direction, sizeBytes, result) appear in logs.
	Logger *slog.Logger

	// Now is the clock used for token-expiry verification. Nil defaults to
	// time.Now; tests inject a future clock to exercise the expired-token
	// rejection path.
	Now func() time.Time
}

// now returns the handler's clock (time.Now when unset).
func (h *SpillUploadAPI) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

// SpillUploadMintRequest mirrors the OpenAPI schema in
// docs/users/api/openapi/admin/e37-s2-agent-presigned-spill.yaml.
type SpillUploadMintRequest struct {
	EventID     string `json:"eventId"`
	Direction   string `json:"direction"`
	SizeBytes   int64  `json:"sizeBytes"`
	ContentType string `json:"contentType,omitempty"`
	SHA256      string `json:"sha256"`
}

// SpillUploadMintResponse is the JSON returned from POST /spill-uploads.
type SpillUploadMintResponse struct {
	UploadURL string    `json:"uploadUrl"`
	Key       string    `json:"key"`
	Backend   string    `json:"backend"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// MintSpillUpload handles POST /api/internal/things/spill-uploads.
func (h *SpillUploadAPI) MintSpillUpload(c echo.Context) error {
	if h.Spill == nil || h.Secrets == nil {
		return serviceUnavailable(c, "spill upload subsystem not configured")
	}

	var req SpillUploadMintRequest
	if err := c.Bind(&req); err != nil {
		return badRequest(c, "invalid request body")
	}
	if err := validateMintRequest(req); err != nil {
		return badRequest(c, err.Error())
	}
	if h.PerObjectCap > 0 && req.SizeBytes > h.PerObjectCap {
		return c.JSON(http.StatusRequestEntityTooLarge, nexushttperr.ErrJSON(fmt.Sprintf("sizeBytes %d exceeds perObjectCap %d", req.SizeBytes, h.PerObjectCap), "validation_error", "PAYLOAD_TOO_LARGE"))
	}

	presigner, ok := h.Spill.(spillstore.Presigner)
	if !ok {
		return serviceUnavailable(c, "spill backend does not expose a presigner")
	}
	now := time.Now().UTC()
	key := presigner.KeyFor(now, req.EventID, req.Direction)

	// Namespace the storage key by the authenticated node identity so
	// one node can never address (and overwrite) another node's spill object.
	// DeviceOrServiceAuth resolves the mTLS device token to a Thing; a device
	// caller (every agent) gets a <nodeId>/ prefix bound into the signed token.
	// Service-token callers (trusted internal services) carry no Thing and keep
	// the flat key — they spill via direct in-process Put anyway, and the rogue-
	// node attack persona only holds a device token.
	nodeID := ""
	if t := enroll.ThingFromContext(c); t != nil {
		nodeID = t.ID
	}
	if nodeID != "" {
		key = nodeID + "/" + key
	}

	claims := spillupload.Claims{
		EventID:   req.EventID,
		Direction: req.Direction,
		Key:       key,
		NodeID:    nodeID,
		SizeBytes: req.SizeBytes,
		SHA256:    strings.ToLower(req.SHA256),
		Backend:   h.SpillBackend,
		Mime:      req.ContentType,
	}
	token, signed, err := spillupload.Sign(h.Secrets, claims, spillupload.MaxTTL)
	if err != nil {
		if errors.Is(err, spillupload.ErrTokenInvalid) {
			return badRequest(c, err.Error())
		}
		h.logf(slog.LevelError, "spill mint: sign", "error", err, "eventId", req.EventID)
		return internalError(c, "sign upload token")
	}

	expiresAt := time.Unix(signed.ExpiresAt, 0).UTC()
	switch h.SpillBackend {
	case "s3":
		url, err := presigner.PresignPut(c.Request().Context(), key, req.SizeBytes, req.ContentType, spillupload.MaxTTL)
		if err != nil {
			h.logf(slog.LevelError, "spill mint: s3 presign", "error", err, "eventId", req.EventID)
			return internalError(c, "presign upload URL")
		}
		h.logf(slog.LevelInfo, "spill upload minted",
			"eventId", req.EventID, "direction", req.Direction,
			"sizeBytes", req.SizeBytes, "backend", "s3")
		return c.JSON(http.StatusOK, SpillUploadMintResponse{
			UploadURL: url,
			Key:       key,
			Backend:   "s3",
			ExpiresAt: expiresAt,
		})
	case "localfs":
		url := h.localfsBlobURL(c, token)
		h.logf(slog.LevelInfo, "spill upload minted",
			"eventId", req.EventID, "direction", req.Direction,
			"sizeBytes", req.SizeBytes, "backend", "localfs")
		return c.JSON(http.StatusOK, SpillUploadMintResponse{
			UploadURL: url,
			Key:       key,
			Backend:   "localfs",
			ExpiresAt: expiresAt,
		})
	default:
		return serviceUnavailable(c, fmt.Sprintf("unsupported spill backend %q", h.SpillBackend))
	}
}

// localfsBlobURL builds the in-Hub blob upload URL the agent will PUT
// to. Prefers the configured HubURL; falls back to scheme+Host from
// the inbound request so a dev workstation hitting localhost:3060 and
// a prod LB both work without explicit configuration.
func (h *SpillUploadAPI) localfsBlobURL(c echo.Context, token string) string {
	base := strings.TrimRight(h.HubURL, "/")
	if base == "" {
		scheme := c.Scheme()
		host := c.Request().Host
		base = scheme + "://" + host
	}
	return base + "/api/internal/spill/blob/" + token
}

// validateMintRequest checks the structural shape of an incoming mint
// request before we touch the secret store.
func validateMintRequest(r SpillUploadMintRequest) error {
	if r.EventID == "" {
		return errors.New("eventId is required")
	}
	if !looksLikeUUID(r.EventID) {
		return errors.New("eventId must be a UUID")
	}
	if r.Direction != spillupload.DirectionRequest && r.Direction != spillupload.DirectionResponse {
		return errors.New("direction must be \"request\" or \"response\"")
	}
	if r.SizeBytes <= 0 {
		return errors.New("sizeBytes must be > 0")
	}
	sha := strings.ToLower(strings.TrimSpace(r.SHA256))
	if len(sha) != 64 {
		return errors.New("sha256 must be 64 lowercase-hex characters")
	}
	for _, b := range []byte(sha) {
		if (b < '0' || b > '9') && (b < 'a' || b > 'f') {
			return errors.New("sha256 must be 64 lowercase-hex characters")
		}
	}
	return nil
}

// looksLikeUUID is a cheap shape check (8-4-4-4-12 hex with dashes).
// Hub does not require RFC 4122 version/variant bits — the audit
// pipeline already accepts any UUID-shaped string.
func looksLikeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	dashAt := map[int]bool{8: true, 13: true, 18: true, 23: true}
	for i, b := range []byte(s) {
		if dashAt[i] {
			if b != '-' {
				return false
			}
			continue
		}
		if (b < '0' || b > '9') && (b < 'a' || b > 'f') && (b < 'A' || b > 'F') {
			return false
		}
	}
	return true
}

// PutSpillBlob handles PUT /api/internal/spill/blob/:token. Registered
// only when SpillBackend == "localfs"; the prod (s3) deployment never
// routes through this endpoint.
//
// Authorisation is the HMAC token alone — the mTLS thing identity that
// minted the token has already been verified, and re-checking it here
// would prevent agents on networks that strip client certs at the LB
// from completing the upload.
func (h *SpillUploadAPI) PutSpillBlob(c echo.Context) error {
	if h.Spill == nil || h.Secrets == nil || h.Dedup == nil {
		return serviceUnavailable(c, "spill blob endpoint not configured")
	}

	token := c.Param("token")
	if token == "" {
		return badRequest(c, "missing token")
	}

	claims, err := spillupload.Verify(h.Secrets, token, h.now())
	switch {
	case errors.Is(err, spillupload.ErrTokenExpired):
		return c.JSON(http.StatusBadRequest, nexushttperr.ErrJSON("token expired", "auth_error", "TOKEN_EXPIRED"))
	case errors.Is(err, spillupload.ErrUnknownKID), errors.Is(err, spillupload.ErrTokenInvalid):
		return unauthorized(c, "invalid token")
	case err != nil:
		h.logf(slog.LevelError, "spill blob: verify", "error", err)
		return internalError(c, "verify token")
	}

	if claims.Backend != "" && claims.Backend != h.SpillBackend {
		return c.JSON(http.StatusBadRequest, nexushttperr.ErrJSON(fmt.Sprintf("token backend %q does not match Hub backend %q", claims.Backend, h.SpillBackend), "validation_error", "BACKEND_MISMATCH"))
	}

	if c.Request().ContentLength < 0 {
		return c.JSON(http.StatusLengthRequired, nexushttperr.ErrJSON("Content-Length header is required", "validation_error", "LENGTH_REQUIRED"))
	}
	if c.Request().ContentLength != claims.SizeBytes {
		return c.JSON(http.StatusRequestEntityTooLarge, nexushttperr.ErrJSON(fmt.Sprintf("Content-Length %d does not match token sizeBytes %d", c.Request().ContentLength, claims.SizeBytes), "validation_error", "CONTENT_LENGTH_MISMATCH"))
	}

	// Stream into spillstore through a sha256-tracking reader BEFORE the
	// dedup slot is acquired. Acquiring the one-shot SetNX slot
	// before the durable Put meant a Put failure (or sha mismatch) burned
	// the slot for the whole DedupTTL — a legitimate retry of a never-stored
	// body would then 409 forever. The slot must only be consumed once the
	// body is durably stored. The upload key is deterministic per
	// (event, direction), so two concurrent identical PUTs write the same
	// verified bytes to the same key (idempotent overwrite); the dedup gate
	// below still ensures exactly one of them is accepted.
	//
	// On hash mismatch we delete the partial via SpillStore.Delete; on any
	// other Put error we emit 500 and the operator inspects logs.
	hashing := newSHA256Tee(c.Request().Body)
	ref, putErr := h.Spill.Put(
		c.Request().Context(),
		hashing,
		claims.SizeBytes,
		spillstore.PutOptions{
			EventID:     claims.EventID,
			Direction:   claims.Direction,
			ContentType: claims.Mime,
			// Write to the exact node-namespaced key the mint signed
			// into this token, not a re-derived (shared, attacker-overwritable)
			// one. claims.Key is HMAC-bound, so a node cannot retarget the write.
			Key: claims.Key,
		},
	)
	if putErr != nil {
		h.logf(slog.LevelError, "spill blob: put", "error", putErr, "eventId", claims.EventID)
		// Best-effort delete in case Put got partway.
		_ = h.Spill.Delete(context.Background(), ref)
		return internalError(c, "store body")
	}

	got := hex.EncodeToString(hashing.sum())
	if got != claims.SHA256 {
		_ = h.Spill.Delete(context.Background(), ref)
		h.logf(slog.LevelWarn, "spill blob: sha256 mismatch",
			"eventId", claims.EventID, "direction", claims.Direction,
			"got", got, "want", claims.SHA256)
		return c.JSON(http.StatusBadRequest, nexushttperr.ErrJSON("uploaded body sha256 does not match token", "validation_error", "SHA256_MISMATCH"))
	}

	// One-shot consumption: the token is good for at most one ACCEPTED PUT.
	// Acquired only after the body is durably stored and verified, so a
	// failed upload never burns the slot. SETNX returns false on a second
	// (already-accepted) attempt — agents that retry after a successful
	// upload get 409 and must NOT re-upload.
	dedupKey := spillupload.DedupKey(token)
	acquired, err := h.Dedup.SetNX(c.Request().Context(), dedupKey, spillupload.DedupTTL)
	if err != nil {
		h.logf(slog.LevelError, "spill blob: dedup", "error", err, "eventId", claims.EventID)
		return serviceUnavailable(c, "dedup unavailable")
	}
	if !acquired {
		// Another concurrent PUT already won the slot for this token; the
		// body we just wrote is identical (same key, sha-verified) so the
		// store is consistent. Report the replay so the agent stops retrying.
		return c.JSON(http.StatusConflict, nexushttperr.ErrJSON("token already consumed", "conflict", "TOKEN_REPLAY"))
	}

	h.logf(slog.LevelInfo, "spill blob accepted",
		"eventId", claims.EventID, "direction", claims.Direction,
		"sizeBytes", claims.SizeBytes, "key", ref.Key)
	return c.NoContent(http.StatusNoContent)
}

// sha256Tee wraps an io.Reader and computes a running SHA-256 of every
// byte read. Used to verify the upload body hash without buffering the
// whole stream.
type sha256Tee struct {
	r io.Reader
	h hash256
}

// hash256 is an alias kept so the field type stays narrow on the struct
// — we only need Write + Sum out of the hash.Hash surface.
type hash256 interface {
	io.Writer
	Sum(b []byte) []byte
}

func newSHA256Tee(r io.Reader) *sha256Tee {
	return &sha256Tee{r: r, h: sha256.New()}
}

func (t *sha256Tee) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 {
		_, _ = t.h.Write(p[:n])
	}
	return n, err
}

func (t *sha256Tee) sum() []byte {
	return t.h.Sum(nil)
}

// logf is a nil-safe slog wrapper so a handler constructed without a
// logger (e.g. in tests) does not panic on the hot path.
func (h *SpillUploadAPI) logf(level slog.Level, msg string, kv ...any) {
	if h.Logger == nil {
		return
	}
	h.Logger.Log(context.Background(), level, msg, kv...)
}

// Compile-time guards: ensure the streaming reader path stays io.Reader.
var _ io.Reader = (*sha256Tee)(nil)
