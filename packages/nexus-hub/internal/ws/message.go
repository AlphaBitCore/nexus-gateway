// Package ws implements the Hub-side WebSocket server for real-time
// Thing communication: config push and shadow reports.
package ws

import "encoding/json"

// --- Hub → Thing messages ---

// ConnectedMessage is sent immediately after a Thing connects.
type ConnectedMessage struct {
	Type       string         `json:"type"`
	HubID      string         `json:"hubId"`
	Desired    map[string]any `json:"desired,omitempty"`
	DesiredVer int64          `json:"desiredVer"`
}

// ConfigChangedMessage is sent when config changes for a Thing's type.
//
// Force is set only when the upstream event is an admin-triggered re-sync
// (see thingmgr.Manager.RePushConfigKey). Things use this flag to bypass the
// version-equality short-circuit inside applyConfig so a replay at the same
// DesiredVer still runs OnConfigChanged and emits a shadow_report.
type ConfigChangedMessage struct {
	Type       string         `json:"type"`
	ConfigKey  string         `json:"configKey,omitempty"`
	State      any            `json:"state,omitempty"`
	DesiredVer int64          `json:"desiredVer"`
	Desired    map[string]any `json:"desired,omitempty"`
	Force      bool           `json:"force,omitempty"`
}

// --- Thing → Hub messages ---

// IncomingMessage is the envelope for all Thing → Hub WebSocket messages.
type IncomingMessage struct {
	Type string          `json:"type"`
	Raw  json.RawMessage `json:"-"`
}

// ShadowReportPayload is the payload of a shadow_report message.
//
// ReportedOutcomes carries the per-config-key apply-outcome ledger from
// the Thing's in-memory OutcomeTracker. Values are intentionally typed
// as RawMessage here — the Hub-side thingmgr.HandleShadowReport decodes
// them into ReportedKeyOutcome so the ws package stays orthogonal to the
// store wire shape.
type ShadowReportPayload struct {
	Reported         map[string]any             `json:"reported"`
	ReportedVer      int64                      `json:"reportedVer"`
	ReportedOutcomes map[string]json.RawMessage `json:"reportedOutcomes,omitempty"`
}

// ParseIncoming parses the message type from a raw WebSocket message.
func ParseIncoming(data []byte) (*IncomingMessage, error) {
	var msg IncomingMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, err
	}
	msg.Raw = data
	return &msg, nil
}
