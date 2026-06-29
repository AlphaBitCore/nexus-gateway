package senders

import (
	"bytes"
	"context"
	"fmt"
	"github.com/goccy/go-json"
	"net/http"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine"
	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
)

type Webhook struct{ c *http.Client }

func NewWebhook(c *http.Client) *Webhook {
	if c == nil {
		c = nexushttp.New(nexushttp.Config{
			Caller:         "hub-alert-webhook",
			Timeout:        10 * time.Second,
			PropagateReqID: true,
			// The URL is admin-supplied and external by nature, so this
			// is an SSRF primitive. Block every non-public address (loopback /
			// RFC-1918 / link-local / metadata) at dial time; the guard runs on
			// the resolved IP so it also defeats DNS-rebinding.
			DialControl: nexushttp.AdminEgressDialControl(nexushttp.AdminEgressExternalOnly),
		})
	}
	return &Webhook{c: c}
}

type webhookConfig struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
}

func (w *Webhook) Send(ctx context.Context, ch alerting.Channel, a alerting.Alert) (int, error) {
	cfg, err := decodeConfig[webhookConfig](ch.Config)
	if err != nil {
		return 0, err
	}
	if cfg.URL == "" {
		return 0, fmt.Errorf("webhook: url required")
	}
	body, err := json.Marshal(a)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.URL, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range cfg.Headers {
		req.Header.Set(k, v)
	}
	resp, err := w.c.Do(req)
	if err != nil {
		return collapseSendResult(0, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	return collapseSendResult(resp.StatusCode, nil)
}
