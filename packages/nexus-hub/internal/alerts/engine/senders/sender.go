// Package senders holds one file per channel transport. Register a Sender
// at init via the Registry; the dispatcher consults it by channel.Type.
package senders

import (
	"bytes"
	"context"
	"fmt"
	"github.com/goccy/go-json"
	"net/http"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine"
)

type Sender interface {
	Send(ctx context.Context, ch alerting.Channel, a alerting.Alert) (statusCode int, err error)
}

type Registry struct{ m map[string]Sender }

func NewRegistry() *Registry { return &Registry{m: map[string]Sender{}} }

func (r *Registry) Register(channelType string, s Sender) { r.m[channelType] = s }

func (r *Registry) Get(channelType string) (Sender, error) {
	s, ok := r.m[channelType]
	if !ok {
		return nil, fmt.Errorf("senders: no sender for %q", channelType)
	}
	return s, nil
}

// decodeConfig round-trips map → JSON → T. Keep it here so all senders share it.
func decodeConfig[T any](m map[string]any) (T, error) {
	var out T
	b, err := json.Marshal(m)
	if err != nil {
		return out, err
	}
	return out, json.Unmarshal(b, &out)
}

// errGenericDeliveryFailure is the single, byte-identical error every
// admin-configured-webhook delivery failure collapses to. The sender
// result (status code + error string) is persisted onto the AlertDispatch row and
// read back in the admin UI, so a distinct transport error (SSRF-guard reject vs.
// connection-refused vs. TLS handshake failure) or a distinct upstream status
// (401 vs. 403 vs. 500) would be a blind-SSRF / internal-endpoint fingerprinting
// oracle for a caller who can configure a channel but should not learn the shape
// of the deployment's internal network. Mirrors the SIEM HTTP-sink collapse:
// any two distinct failure causes MUST produce identical output, so the status
// code is dropped (returned 0) and the cause is hidden.
var errGenericDeliveryFailure = fmt.Errorf("alert delivery failed")

// collapseSendResult maps a raw delivery outcome to the generic
// success/failure envelope persisted on the dispatch row. A dial/transport
// error or a non-2xx upstream status both collapse to (0, errGenericDeliveryFailure)
// so the failure cause cannot be distinguished by an admin reading the dispatch
// inbox. Success returns (status, nil) — the 2xx code is not sensitive.
func collapseSendResult(status int, err error) (int, error) {
	if err != nil {
		return 0, errGenericDeliveryFailure
	}
	if status >= 300 {
		return 0, errGenericDeliveryFailure
	}
	return status, nil
}

// postJSON POSTs body as JSON and returns the collapsed (status, error) outcome.
// Caller provides optional headers (e.g. Authorization). Every failure cause is
// collapsed to a single generic error (see [collapseSendResult]); only a 2xx
// status is surfaced.
func postJSON(ctx context.Context, c *http.Client, url string, body []byte, headers http.Header) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return collapseSendResult(0, err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := c.Do(req)
	if err != nil {
		return collapseSendResult(0, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	return collapseSendResult(resp.StatusCode, nil)
}
