// Package alertclient is the data-plane's outbound path to Hub alerting.
// Fire(ctx, env) attempts a best-effort HTTP POST; on failure the envelope
// is spooled to disk. ReplayPending drains the spool on reconnect and on
// a ticker.
package alertclient

import (
	"log/slog"
	"time"
)

// AlertEnvelope is the payload sent to Hub's /api/v1/alerts/raise endpoint.
type AlertEnvelope struct {
	RuleID      string         `json:"ruleId"`
	TargetKey   string         `json:"targetKey"`
	TargetLabel string         `json:"targetLabel"`
	Severity    string         `json:"severity"` // lowercase: critical|high|medium|low|info
	Message     string         `json:"message"`
	Details     map[string]any `json:"details,omitempty"`
	FiredAt     time.Time      `json:"firedAt"`
}

// ResolveRequest is the payload sent to Hub's /api/v1/alerts/resolve endpoint.
type ResolveRequest struct {
	RuleID    string `json:"ruleId"`
	TargetKey string `json:"targetKey"`
	Reason    string `json:"reason"`
}

// Config holds construction parameters for Client.
type Config struct {
	HubBaseURL    string
	AuthHeader    string        // e.g. "Bearer <thing-token>" or mTLS — caller sets
	SpoolDir      string        // e.g. /var/lib/compliance-proxy/alertspool
	SpoolMaxBytes int64         // default 50MB
	HTTPTimeout   time.Duration // default 5s
	ReplayEvery   time.Duration // default 30s
	Logger        *slog.Logger
}
