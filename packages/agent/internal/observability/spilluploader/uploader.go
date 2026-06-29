// Package spilluploader implements the agent half of the pre-signed-URL
// spill flow. The agent calls Upload with the captured body bytes; the
// uploader negotiates a one-shot URL with Hub, PUTs the body to the URL
// the mint endpoint returned (S3 in prod, Hub-localfs in dev), and
// returns the resulting SpillRef the agent stamps onto its audit envelope.
//
// On any error the uploader returns ErrFallbackInline so the caller can
// degrade to inline-truncated capture rather than dropping the audit
// row. The intercept layer wraps Upload with that fallback.
package spilluploader

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/goccy/go-json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
)

// ErrFallbackInline is returned when the uploader could not complete
// the round-trip and the caller should inline-truncate the body
// instead. Wraps the underlying transport / Hub error so the caller
// can log it.
var ErrFallbackInline = errors.New("spilluploader: fallback to inline")

// HubClient is the narrow surface the uploader needs from
// agent/internal/hub.Client. The uploader does not depend on the
// concrete client so tests can inject a stub round-tripper without
// pulling in the full mTLS stack.
type HubClient interface {
	BaseURL() string
	HTTPClient() *http.Client
}

// mintRequest mirrors handler.SpillUploadMintRequest on the Hub side.
type mintRequest struct {
	EventID     string `json:"eventId"`
	Direction   string `json:"direction"`
	SizeBytes   int64  `json:"sizeBytes"`
	ContentType string `json:"contentType,omitempty"`
	SHA256      string `json:"sha256"`
}

// mintResponse mirrors handler.SpillUploadMintResponse.
type mintResponse struct {
	UploadURL string    `json:"uploadUrl"`
	Key       string    `json:"key"`
	Backend   string    `json:"backend"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// Uploader is the production implementation. Construct one per agent
// process; it is safe for concurrent use.
type Uploader struct {
	hub HubClient
}

// New returns an Uploader that talks to the supplied Hub client. The
// client must already be configured with mTLS — the mint endpoint
// requires a thing identity. nil hub means the uploader always
// returns ErrFallbackInline (useful for tests / off-by-default
// deployments).
func New(hub HubClient) *Uploader {
	return &Uploader{hub: hub}
}

// Upload performs the full mint → PUT round-trip and returns the
// SpillRef the agent must stamp onto its audit envelope. ContentType
// is forwarded into the SpillRef so the Control Plane reader picks the
// right rendering. On any error a wrapped ErrFallbackInline is
// returned and the caller falls back to inline-truncated capture.
func (u *Uploader) Upload(ctx context.Context, eventID, direction, contentType string, body []byte) (audit.SpillRef, error) {
	if u == nil || u.hub == nil {
		return audit.SpillRef{}, fmt.Errorf("%w: uploader not configured", ErrFallbackInline)
	}
	if eventID == "" || direction == "" {
		return audit.SpillRef{}, fmt.Errorf("%w: missing eventId/direction", ErrFallbackInline)
	}
	if len(body) == 0 {
		return audit.SpillRef{}, fmt.Errorf("%w: empty body", ErrFallbackInline)
	}
	sum := sha256.Sum256(body)
	hashHex := hex.EncodeToString(sum[:])

	mint, err := u.mint(ctx, mintRequest{
		EventID:     eventID,
		Direction:   direction,
		SizeBytes:   int64(len(body)),
		ContentType: contentType,
		SHA256:      hashHex,
	})
	if err != nil {
		return audit.SpillRef{}, fmt.Errorf("%w: mint: %w", ErrFallbackInline, err)
	}

	if err := u.put(ctx, mint.UploadURL, contentType, body); err != nil {
		return audit.SpillRef{}, fmt.Errorf("%w: put: %w", ErrFallbackInline, err)
	}

	return audit.SpillRef{
		Backend:     mint.Backend,
		Key:         mint.Key,
		Size:        int64(len(body)),
		SHA256:      hashHex,
		ContentType: contentType,
	}, nil
}

// mint POSTs to /api/internal/things/spill-uploads and returns the
// decoded URL + key the agent must PUT to.
func (u *Uploader) mint(ctx context.Context, req mintRequest) (mintResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return mintResponse{}, fmt.Errorf("encode mint request: %w", err)
	}
	url := strings.TrimRight(u.hub.BaseURL(), "/") + "/api/internal/things/spill-uploads"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return mintResponse{}, fmt.Errorf("build mint request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := u.hub.HTTPClient().Do(httpReq)
	if err != nil {
		return mintResponse{}, fmt.Errorf("send mint request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		// Bound the error body so a chatty Hub does not blow the agent's
		// log line size; 4 KiB is enough to carry a structured error
		// payload + a useful hint.
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
		return mintResponse{}, fmt.Errorf("mint endpoint returned %d: %s", resp.StatusCode, strings.TrimSpace(string(errBody)))
	}

	var out mintResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32*1024)).Decode(&out); err != nil {
		return mintResponse{}, fmt.Errorf("decode mint response: %w", err)
	}
	if out.UploadURL == "" || out.Key == "" {
		return mintResponse{}, fmt.Errorf("mint response missing uploadUrl/key")
	}
	return out, nil
}

// put streams the body to the upload URL with Content-Length pinned to
// len(body). The agent reuses Hub's HTTP client for the localfs PUT
// (so the request follows mTLS + tracing); for an S3 PUT the URL is a
// public presigned URL and S3 enforces the signed Content-Length on the
// PUT. SHA-256 is NOT bound into the presigned URL, so we do not send an
// x-amz-checksum-sha256 header here — integrity is anchored on the
// agent-computed sha256 recorded on the SpillRef (and the mint token),
// verified at read time, not by an S3-side write checksum. See
// spillstore/s3.PresignPut for the full integrity-model rationale.
func (u *Uploader) put(ctx context.Context, url, contentType string, body []byte) error {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build put request: %w", err)
	}
	httpReq.ContentLength = int64(len(body))
	if contentType != "" {
		httpReq.Header.Set("Content-Type", contentType)
	}
	resp, err := u.hub.HTTPClient().Do(httpReq)
	if err != nil {
		return fmt.Errorf("send put request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusOK:
		return nil
	case http.StatusConflict:
		// 409 means a previous attempt already consumed this token.
		// Surface as a non-retryable failure so the caller falls back
		// to inline rather than retrying with the same URL.
		return fmt.Errorf("upload URL already consumed (409)")
	default:
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
		return fmt.Errorf("upload endpoint returned %d: %s", resp.StatusCode, strings.TrimSpace(string(errBody)))
	}
}
