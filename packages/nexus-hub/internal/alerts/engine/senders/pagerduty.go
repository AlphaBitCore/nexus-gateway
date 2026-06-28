package senders

import (
	"context"
	"fmt"
	"github.com/goccy/go-json"
	"net/http"
	"strings"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine"
	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
)

// PagerDuty delivers alerts to the PagerDuty Events API v2 (the integration
// key based firehose used by Events API routing keys). A stable dedup_key of
// "ruleID|targetKey" collapses repeated raises for the same (rule, target).
type PagerDuty struct{ c *http.Client }

func NewPagerDuty(c *http.Client) *PagerDuty {
	if c == nil {
		c = nexushttp.New(nexushttp.Config{
			Caller:         "hub-alert-pagerduty",
			Timeout:        10 * time.Second,
			PropagateReqID: true,
		})
	}
	return &PagerDuty{c: c}
}

type pdConfig struct {
	RoutingKey string `json:"routingKey"`
}

const pdEventsAPIURL = "https://events.pagerduty.com/v2/enqueue"

func (p *PagerDuty) Send(ctx context.Context, ch alerting.Channel, a alerting.Alert) (int, error) {
	cfg, err := decodeConfig[pdConfig](ch.Config)
	if err != nil {
		return 0, err
	}
	if cfg.RoutingKey == "" {
		return 0, fmt.Errorf("pagerduty: routingKey required")
	}
	body, _ := json.Marshal(map[string]any{
		"routing_key":  cfg.RoutingKey,
		"event_action": "trigger",
		"dedup_key":    a.RuleID + "|" + a.TargetKey,
		"payload": map[string]any{
			"summary":        fmt.Sprintf("[%s] %s: %s", strings.ToUpper(string(a.Severity)), a.RuleID, a.Message),
			"severity":       string(a.Severity),
			"source":         a.TargetLabel,
			"custom_details": a.Details,
		},
	})
	return postJSON(ctx, p.c, pdEventsAPIURL, body, nil)
}
