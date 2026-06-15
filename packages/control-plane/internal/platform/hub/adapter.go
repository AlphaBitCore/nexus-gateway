// Package hubadapter translates Nexus Hub internal terminology ("thing",
// "shadow", "desired", "reported", "drift") into product-facing terminology
// ("node", "configSync", "targetConfig", "appliedConfig", "outOfSync") at the
// boundary between the Control Plane admin API and the Hub HTTP API.
//
// Callers receive raw JSON bytes from Hub and pass them through one of the
// Rename* functions before returning to admin clients. Fields not covered by
// the mapping pass through unchanged.
package hub

import (
	"encoding/json"
	"fmt"
)

// nodeFieldMap covers one-to-one key renames for a single Thing row. Metadata
// lifting (role, metricsUrl → top-level) is handled separately in renameNodeMap.
var nodeFieldMap = map[string]string{
	"address":          "listen_address",
	"authType":         "auth_type",
	"connProtocol":     "conn_protocol",
	"desired":          "targetConfig",
	"reported":         "appliedConfig",
	"desiredVer":       "targetVersion",
	"reportedVer":      "appliedVersion",
	"lastSeenAt":       "last_seen_at",
	"enrolledAt":       "created_at",
	"reportedOutcomes": "appliedOutcomes",
	"processStartedAt": "processStartedAt",
	// Hub now joins thing_service.metrics_url into the Thing response; the
	// admin Node Detail page reads `metrics_url` (snake_case) for the
	// clickable /metrics link. Lift-from-metadata still runs below as a
	// fallback for legacy rows that wrote the URL into metadata.metricsUrl.
	"metricsUrl": "metrics_url",
}

// RenameThingsList rewrites the top-level "things" wrapper into "nodes" and
// runs each entry through renameNodeMap. Pagination fields pass through.
func RenameThingsList(in []byte) ([]byte, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(in, &raw); err != nil {
		return nil, fmt.Errorf("hubadapter: unmarshal list: %w", err)
	}
	out := make(map[string]json.RawMessage, len(raw))
	for k, v := range raw {
		if k == "things" {
			renamed, err := renameArrayOfNodes(v)
			if err != nil {
				return nil, err
			}
			out["nodes"] = renamed
			continue
		}
		out[k] = v
	}
	return json.Marshal(out)
}

// RenameNode rewrites a single Thing object into the UI-facing Node shape.
func RenameNode(in []byte) ([]byte, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(in, &raw); err != nil {
		return nil, fmt.Errorf("hubadapter: unmarshal node: %w", err)
	}
	return json.Marshal(renameNodeMap(raw))
}

// configUpdateRenames maps Hub's UpdateConfigResponse field names onto the
// product-facing shape the UI's ConfigUpdateRequest response type expects.
var configUpdateRenames = map[string]string{
	"thingsNotified": "nodesNotified",
	"thingsOnline":   "nodesOnline",
}

// RenameConfigUpdateResponse renames the per-counter fields of Hub's
// /api/hub/config/update response so admin clients never see the internal
// "thing" terminology. `ok` and `version` pass through unchanged.
func RenameConfigUpdateResponse(in []byte) ([]byte, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(in, &raw); err != nil {
		return nil, fmt.Errorf("hubadapter: unmarshal config update: %w", err)
	}
	out := make(map[string]json.RawMessage, len(raw))
	for k, v := range raw {
		if newKey, ok := configUpdateRenames[k]; ok {
			out[newKey] = v
			continue
		}
		// Monotonic shadow revision broadcast to Things (distinct from template `version`).
		if k == "thingDesiredVer" {
			out["targetShadowVersion"] = v
			continue
		}
		out[k] = v
	}
	return json.Marshal(out)
}

// RenameDriftResponse renames the "drifted" wrapper and per-item fields into
// the UI's OutOfSync shape. Hub's DriftedThing now carries outOfSyncKeys
// (a sorted list of config keys whose desired value diverges from reported),
// which is passed through unchanged. When the field is absent (e.g. from an
// older Hub version) an empty array is synthesized so the UI can render the
// column unconditionally without a nil-check.
func RenameDriftResponse(in []byte) ([]byte, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(in, &raw); err != nil {
		return nil, fmt.Errorf("hubadapter: unmarshal drift: %w", err)
	}
	out := make(map[string]json.RawMessage, len(raw))
	for k, v := range raw {
		if k == "drifted" {
			renamed, err := renameDriftArray(v)
			if err != nil {
				return nil, err
			}
			out["outOfSync"] = renamed
			continue
		}
		out[k] = v
	}
	return json.Marshal(out)
}

func renameDriftArray(arr json.RawMessage) (json.RawMessage, error) {
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(arr, &items); err != nil {
		return nil, fmt.Errorf("hubadapter: unmarshal drift array: %w", err)
	}
	out := make([]map[string]json.RawMessage, len(items))
	for i, item := range items {
		renamed := make(map[string]json.RawMessage, len(item)+1)
		for k, v := range item {
			switch k {
			case "id":
				renamed["nodeId"] = v
			case "type":
				renamed["nodeType"] = v
			case "lastSeenAt":
				renamed["lastSeen"] = v
			default:
				// outOfSyncKeys and all other fields pass through unchanged.
				renamed[k] = v
			}
		}
		// Guarantee the field is always present so the UI can render the
		// column unconditionally. Hub normally provides this, but if an
		// older Hub version omits it, synthesize an empty array rather than
		// leaving the field absent (null/absent breaks the UI's .map() call).
		if _, ok := renamed["outOfSyncKeys"]; !ok {
			renamed["outOfSyncKeys"] = json.RawMessage("[]")
		}
		out[i] = renamed
	}
	return json.Marshal(out)
}

func renameArrayOfNodes(arr json.RawMessage) (json.RawMessage, error) {
	var nodes []map[string]json.RawMessage
	if err := json.Unmarshal(arr, &nodes); err != nil {
		return nil, fmt.Errorf("hubadapter: unmarshal array: %w", err)
	}
	out := make([]map[string]json.RawMessage, len(nodes))
	for i, n := range nodes {
		out[i] = renameNodeMap(n)
	}
	return json.Marshal(out)
}

// renameNodeMap applies nodeFieldMap and lifts two well-known metadata keys
// (role, metricsUrl) to top-level fields the UI expects. The raw metadata
// object is also passed through under the same key so the Node Detail
// Overview tab can render the Metadata section; per-key lifts remain so
// the rest of the UI (Service role badge, metrics URL link) keeps reading
// from top-level fields.
func renameNodeMap(in map[string]json.RawMessage) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(in)+2)
	for k, v := range in {
		if k == "metadata" {
			if role, url := liftMetadata(v); role != nil {
				out["role"] = role
				if url != nil {
					out["metrics_url"] = url
				}
			} else if url != nil {
				out["metrics_url"] = url
			}
			// Keep the full metadata blob so the Overview Metadata
			// panel can surface every key (hostname, os, source_ip,
			// custom labels, etc.) without the BFF needing a fresh
			// allow-list each time the producer adds a field.
			out["metadata"] = v
			continue
		}
		if newKey, ok := nodeFieldMap[k]; ok {
			out[newKey] = v
		} else {
			out[k] = v
		}
	}
	return out
}

func liftMetadata(raw json.RawMessage) (role, metricsURL json.RawMessage) {
	if len(raw) == 0 {
		return nil, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, nil
	}
	return m["role"], m["metricsUrl"]
}
