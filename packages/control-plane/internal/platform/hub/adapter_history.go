package hub

import (
	"encoding/json"
	"fmt"
)

// historyEventFieldMap renames Hub-internal config_change_event fields into
// the product-facing names the admin UI expects. Keys not listed here pass
// through unchanged (id, configKey, action, actorId, actorName, newState,
// newVersion, sourceIp, emergencyOverride).
var historyEventFieldMap = map[string]string{
	"timestamp": "createdAt",
	"thingType": "nodeType",
}

// RenameConfigHistoryResponse rewrites the Hub response for
// GET /api/hub/config/history into the admin shape. The top-level
// `events` / `total` / `page` / `pageSize` envelope is preserved; only
// per-event field names are aliased so the admin UI sees `createdAt` /
// `nodeType` instead of `timestamp` / `thingType`.
//
// Fields the map does not cover stay as-is, so future Hub schema additions
// show up verbatim on the admin API until we decide on an alias.
func RenameConfigHistoryResponse(in []byte) ([]byte, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(in, &raw); err != nil {
		return nil, fmt.Errorf("hubadapter: unmarshal history: %w", err)
	}
	eventsRaw, ok := raw["events"]
	if !ok || len(eventsRaw) == 0 {
		return in, nil
	}
	renamed, err := renameHistoryEventArray(eventsRaw)
	if err != nil {
		return nil, err
	}
	raw["events"] = renamed
	return json.Marshal(raw)
}

func renameHistoryEventArray(arr json.RawMessage) (json.RawMessage, error) {
	var events []map[string]json.RawMessage
	if err := json.Unmarshal(arr, &events); err != nil {
		return nil, fmt.Errorf("hubadapter: unmarshal history events: %w", err)
	}
	out := make([]map[string]json.RawMessage, len(events))
	for i, ev := range events {
		renamed := make(map[string]json.RawMessage, len(ev))
		for k, v := range ev {
			if alias, ok := historyEventFieldMap[k]; ok {
				renamed[alias] = v
			} else {
				renamed[k] = v
			}
		}
		out[i] = renamed
	}
	return json.Marshal(out)
}
