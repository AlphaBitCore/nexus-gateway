package senders

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine"
	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
)

// Slack delivers alerts to Slack via either an incoming webhook URL or the
// chat.postMessage Web API. The webhook path is preferred when configured
// because it does not require a bot token.
type Slack struct{ c *http.Client }

func NewSlack(c *http.Client) *Slack {
	if c == nil {
		c = nexushttp.New(nexushttp.Config{
			Caller:         "hub-alert-slack",
			Timeout:        10 * time.Second,
			PropagateReqID: true,
		})
	}
	return &Slack{c: c}
}

type slackConfig struct {
	BotToken   string `json:"botToken"`
	Channel    string `json:"channel"`
	WebhookURL string `json:"webhookUrl,omitempty"` // preferred when set
}

func (s *Slack) Send(ctx context.Context, ch alerting.Channel, a alerting.Alert) (int, error) {
	cfg, err := decodeConfig[slackConfig](ch.Config)
	if err != nil {
		return 0, err
	}

	text := fmt.Sprintf("*[%s] %s*\n%s",
		strings.ToUpper(string(a.Severity)), a.TargetLabel, a.Message)

	// Prefer incoming webhook URL; fall back to chat.postMessage if botToken set.
	if cfg.WebhookURL != "" {
		body, _ := json.Marshal(map[string]any{"text": text})
		return postJSON(ctx, s.c, cfg.WebhookURL, body, nil)
	}
	if cfg.BotToken == "" || cfg.Channel == "" {
		return 0, fmt.Errorf("slack: need either webhookUrl or (botToken+channel)")
	}
	body, _ := json.Marshal(map[string]any{
		"channel": cfg.Channel,
		"text":    text,
	})
	return postJSON(ctx, s.c, "https://slack.com/api/chat.postMessage", body, http.Header{
		"Authorization": []string{"Bearer " + cfg.BotToken},
	})
}
