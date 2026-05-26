package hub

import (
	"encoding/json"
	"fmt"
)

// RenameConfigCatalogResponse rewrites the Hub response for
// GET /api/hub/config/catalog into the admin shape: per-entry `thingType`
// becomes product-facing `nodeType`, matching the rest of the admin
// surface. `configKeys` is passed through unchanged and the top-level
// `entries` envelope is preserved.
//
// Lives in its own file so it sits next to the existing node/drift/shadow/
// history aliases without touching the in-flight adapter.go refactor.
func RenameConfigCatalogResponse(in []byte) ([]byte, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(in, &raw); err != nil {
		return nil, fmt.Errorf("hubadapter: unmarshal catalog: %w", err)
	}
	entriesRaw, ok := raw["entries"]
	if !ok || len(entriesRaw) == 0 {
		return in, nil
	}
	renamed, err := renameCatalogEntries(entriesRaw)
	if err != nil {
		return nil, err
	}
	raw["entries"] = renamed
	return json.Marshal(raw)
}

func renameCatalogEntries(arr json.RawMessage) (json.RawMessage, error) {
	var entries []map[string]json.RawMessage
	if err := json.Unmarshal(arr, &entries); err != nil {
		return nil, fmt.Errorf("hubadapter: unmarshal catalog entries: %w", err)
	}
	out := make([]map[string]json.RawMessage, len(entries))
	for i, e := range entries {
		renamed := make(map[string]json.RawMessage, len(e))
		for k, v := range e {
			if k == "thingType" {
				renamed["nodeType"] = v
				continue
			}
			renamed[k] = v
		}
		out[i] = renamed
	}
	return json.Marshal(out)
}
