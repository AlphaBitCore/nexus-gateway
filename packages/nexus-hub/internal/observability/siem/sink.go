// Package siem implements the SIEM bridge that polls the unified audit_event
// table and forwards batches to an external SIEM endpoint (Splunk HEC,
// Datadog, Elastic, generic webhook). This complements the compliance-proxy's
// real-time forwarder by covering all audit sources (VK, admin, agent,
// device-lifecycle) that only land in PostgreSQL.
package siem

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
)

// Event is a generic audit event payload forwarded to SIEM. Using
// map[string]any keeps the package decoupled from any specific audit
// struct — the bridge queries columns directly into this map.
type Event = map[string]any

// Sink is the pluggable transport for forwarded audit events.
type Sink interface {
	// Name returns a short identifier used in logs (e.g. "http:splunk-hec").
	Name() string
	// Send delivers a batch of events. The caller retries on transient errors.
	Send(ctx context.Context, events []Event) error
}

// HTTPSink POSTs each batch as a JSON array to a configured webhook URL.
// Designed to be SIEM-vendor-agnostic: Splunk HEC, Datadog logs, Elastic,
// or any generic webhook. Operators configure the URL and auth headers
// (e.g. "Authorization: Splunk <token>") via SystemMetadata.
type HTTPSink struct {
	url       string
	headers   map[string]string
	formatter Formatter
	client    *http.Client
}

// NewHTTPSink creates a sink that POSTs batches to the given URL using the
// provided formatter. If formatter is nil, JSONFormatter is used as the default.
// Headers are set on every request (typically used for auth tokens).
func NewHTTPSink(url string, headers map[string]string, formatter Formatter) (*HTTPSink, error) {
	if url == "" {
		return nil, fmt.Errorf("siem: HTTP sink URL is required")
	}
	if formatter == nil {
		formatter = &JSONFormatter{}
	}
	return &HTTPSink{
		url:       url,
		headers:   headers,
		formatter: formatter,
		client: nexushttp.New(nexushttp.Config{
			Caller:         "hub-siem",
			Timeout:        30 * time.Second,
			PropagateReqID: true,
		}),
	}, nil
}

// Name returns a short identifier for logging.
func (h *HTTPSink) Name() string { return "http:" + h.url }

// Send POSTs the events using the configured formatter. Returns an error on
// non-2xx responses or network failures.
func (h *HTTPSink) Send(ctx context.Context, events []Event) error {
	body, err := h.formatter.FormatBatch(events)
	if err != nil {
		return fmt.Errorf("siem/http: format: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("siem/http: new request: %w", err)
	}
	req.Header.Set("Content-Type", h.formatter.ContentType())
	for k, v := range h.headers {
		req.Header.Set(k, v)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return fmt.Errorf("siem/http: post: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("siem/http: %s returned status %d", h.url, resp.StatusCode)
	}
	return nil
}
