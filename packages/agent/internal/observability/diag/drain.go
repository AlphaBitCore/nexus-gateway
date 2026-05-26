package diag

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

// drainPath is the Hub HTTP endpoint that accepts crash-buffered DiagEvents.
// See packages/nexus-hub/internal/handler/opsmetrics_diag.go.
const drainPath = "/api/internal/things/diag-events:batch"

// defaultBatchSize is the row count per HTTP POST. Tuned to stay well under
// the Hub-side defensive cap (500 — see maxDiagDrainBatchSize on the
// handler) while still draining a long-disconnected agent in only a few
// round-trips.
const defaultBatchSize = 100

// DrainConfig parameterizes DrainPending. The agent main wires it once
// after enrollment + before connecting the WebSocket.
type DrainConfig struct {
	// Buffer is the SQLCipher pending_diag_event source. Required.
	Buffer *LocalBuffer

	// HTTPClient is the agent's existing httpclient.New(...) result so the
	// drain shares the H2 pool, mTLS identity, and timeouts of every other
	// Hub-bound HTTP call. Required.
	HTTPClient *http.Client

	// HubURL is the Hub HTTP origin (e.g. "https://hub:3060"). Required.
	HubURL string

	// DeviceToken is the Authorization Bearer that the Hub's
	// DeviceOrServiceAuth middleware checks. Required.
	DeviceToken string

	// ThingID is sent in the X-Thing-Id header so the Hub-side handler can
	// fall back to it when the device-token auth context is unavailable.
	// Optional but recommended (matches the agent-audit upload contract).
	ThingID string

	// Log receives non-fatal warnings (e.g. corrupt rows). Optional.
	Log *slog.Logger

	// BatchSize caps each request size. Defaults to 100.
	BatchSize int
}

// drainEnvelope mirrors hub-side handler.DiagDrainEvent: a wire-format
// DiagEvent with a top-level id field. Inlined here so the agent doesn't
// import the Hub package.
type drainEnvelope struct {
	ID string `json:"id"`
	registry.DiagEvent
}

// drainBody is the JSON request body shape the Hub expects.
type drainBody struct {
	Events []drainEnvelope `json:"events"`
}

// drainAck is the Hub response shape.
type drainAck struct {
	AcceptedIds []string `json:"acceptedIds"`
}

// DrainPending uploads the local SQLCipher pending_diag_event rows to Hub
// and prunes the rows the Hub acks. Loops with partial acks: a Hub that
// only acks half a batch causes the next iteration to read the remaining
// rows and POST again. Loops stop when (a) the buffer is empty, or (b) the
// Hub accepts zero of a non-empty batch — both Buffer.IncrAttempts is
// called and an error is returned so the caller can log + move on.
//
// Errors are non-fatal at the call site (cmd/agent/main.go wraps the call
// in a Warn log and continues startup); the rows survive in the SQLCipher
// buffer and the next start retries.
func DrainPending(ctx context.Context, cfg DrainConfig) error {
	if cfg.Buffer == nil {
		return fmt.Errorf("DrainPending: nil buffer")
	}
	if cfg.HTTPClient == nil {
		return fmt.Errorf("DrainPending: nil http client")
	}
	if cfg.HubURL == "" {
		return fmt.Errorf("DrainPending: empty hub url")
	}
	if cfg.DeviceToken == "" {
		return fmt.Errorf("DrainPending: empty device token")
	}
	batchSize := cfg.BatchSize
	if batchSize <= 0 {
		batchSize = defaultBatchSize
	}
	logger := cfg.Log
	if logger == nil {
		logger = slog.Default()
	}

	for {
		rows, err := cfg.Buffer.List(batchSize)
		if err != nil {
			return fmt.Errorf("list pending: %w", err)
		}
		if len(rows) == 0 {
			return nil
		}

		envs := make([]drainEnvelope, len(rows))
		for i, r := range rows {
			envs[i] = drainEnvelope(r)
		}
		body, err := json.Marshal(drainBody{Events: envs})
		if err != nil {
			return fmt.Errorf("marshal drain body: %w", err)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.HubURL+drainPath, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("build drain request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+cfg.DeviceToken)
		req.Header.Set("Content-Type", "application/json")
		if cfg.ThingID != "" {
			req.Header.Set("X-Thing-Id", cfg.ThingID)
		}

		resp, err := cfg.HTTPClient.Do(req)
		if err != nil {
			// Bump attempts so the next start can show the row didn't
			// even reach Hub — distinct from "Hub rejected" diagnostics.
			ids := envIds(envs)
			if bumpErr := cfg.Buffer.IncrAttempts(ids); bumpErr != nil {
				logger.Warn("incr attempts after transport error", "error", bumpErr)
			}
			return fmt.Errorf("post diag drain: %w", err)
		}

		if resp.StatusCode != http.StatusOK {
			// Drain a bounded slice of the body for the error message,
			// then close. Don't try to parse — the Hub's error JSON shape
			// is informational only.
			peek, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
			_ = resp.Body.Close()
			ids := envIds(envs)
			if bumpErr := cfg.Buffer.IncrAttempts(ids); bumpErr != nil {
				logger.Warn("incr attempts after non-200", "error", bumpErr)
			}
			return fmt.Errorf("diag drain: status %d: %s", resp.StatusCode, string(peek))
		}

		var ack drainAck
		decErr := json.NewDecoder(resp.Body).Decode(&ack)
		_ = resp.Body.Close()
		if decErr != nil {
			return fmt.Errorf("decode drain ack: %w", decErr)
		}

		if len(ack.AcceptedIds) == 0 {
			ids := envIds(envs)
			if bumpErr := cfg.Buffer.IncrAttempts(ids); bumpErr != nil {
				logger.Warn("incr attempts after zero ack", "error", bumpErr)
			}
			return fmt.Errorf("diag drain: hub accepted 0/%d events", len(envs))
		}

		if err := cfg.Buffer.Delete(ack.AcceptedIds); err != nil {
			return fmt.Errorf("delete acked rows: %w", err)
		}

		logger.Info("diag drain progress",
			"posted", len(envs),
			"accepted", len(ack.AcceptedIds),
		)

		if len(ack.AcceptedIds) < len(envs) {
			// Partial ack — loop continues. Some envs remained buffered;
			// the next List call returns them ordered by occurred_at ASC
			// alongside any newly inserted rows (none in startup-drain;
			// possible if the agent is in flight when the spec evolves
			// to drain mid-session).
			continue
		}

		// Full ack — loop continues so we can drain a tail batch if the
		// buffer had more than batchSize rows.
	}
}

// envIds returns the wire-format ids for a slice of envelopes.
func envIds(envs []drainEnvelope) []string {
	out := make([]string, len(envs))
	for i, e := range envs {
		out[i] = e.ID
	}
	return out
}
