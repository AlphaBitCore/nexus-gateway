package core

import (
	"encoding/json"
	"fmt"
)

// HookConfigRow is the raw row shape from the HookConfig table. Used by
// every service that loads HookConfig out of Postgres (AI Gateway,
// Compliance Proxy, Agent's hub-pushed bridge). Each service runs its own
// SQL — only the row shape and the BuildHookConfig converter are shared.
//
// Endpoint is the top-level `endpoint` column, used for webhook.forward
// and other hooks that need a remote URL. BuildHookConfig folds it into
// the config map as "endpoint" so factories can read it uniformly.
type HookConfigRow struct {
	ID                string
	Name              string
	ImplementationID  string
	Stage             string
	Enabled           bool
	Priority          int
	TimeoutMs         int
	FailBehavior      string
	ConfigJSON        string // jsonb as text; empty string = NULL
	Endpoint          string // top-level endpoint column; "" = NULL
	ApplicableIngress []string
}

// BuildHookConfig converts a raw HookConfigRow to HookConfig.
// Malformed JSON is a hard error.
//
// The top-level Endpoint column is folded into the config map as the
// "endpoint" key when present and not already set by the config JSON.
// This lets webhook-style hooks read endpoint uniformly via
// cfg.Config["endpoint"] regardless of whether the UI wrote it to the
// top-level column (typical) or stuffed it into the config body (legacy
// seeds).
func BuildHookConfig(row HookConfigRow) (HookConfig, error) {
	cfg := map[string]any{}
	if len(row.ConfigJSON) > 0 {
		if err := json.Unmarshal([]byte(row.ConfigJSON), &cfg); err != nil {
			return HookConfig{}, fmt.Errorf("unmarshal config jsonb: %w", err)
		}
		if cfg == nil {
			cfg = map[string]any{}
		}
	}
	if row.Endpoint != "" {
		if _, exists := cfg["endpoint"]; !exists {
			cfg["endpoint"] = row.Endpoint
		}
	}
	return HookConfig{
		ID:                row.ID,
		ImplementationID:  row.ImplementationID,
		Name:              row.Name,
		Priority:          row.Priority,
		Enabled:           row.Enabled,
		Stage:             row.Stage,
		FailBehavior:      row.FailBehavior,
		TimeoutMs:         row.TimeoutMs,
		ApplicableIngress: row.ApplicableIngress,
		Config:            cfg,
	}, nil
}
