// Package senders holds one file per channel transport. Register a Sender
// at init via the Registry; the dispatcher consults it by channel.Type.
package senders

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

// postJSON POSTs body as JSON and returns (status, error). Caller provides
// optional headers (e.g. Authorization). Non-2xx is reported as error with
// the statusCode for the dispatcher to log.
func postJSON(ctx context.Context, c *http.Client, url string, body []byte, headers http.Header) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("post %s: status %d", url, resp.StatusCode)
	}
	return resp.StatusCode, nil
}
