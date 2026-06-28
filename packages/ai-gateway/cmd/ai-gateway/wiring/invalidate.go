// invalidate.go — Hub thingclient invalidate-payload helpers.
//
// Hub may push two payload shapes for cache invalidation events:
//
//  1. Full reload payload: arbitrary JSON the consumer treats as a
//     signal to call Reload (the JSON content is ignored).
//  2. Targeted invalidate-by-id payload: {"op":"invalidate","ids":[...]}
//     consumed by KeyCache to evict only the named entries.
//
// ParseInvalidateIDs returns the IDs from form (2). It returns nil for
// any payload that is not a well-formed invalidate envelope, including
// the empty State seen on snapshot reload signals — callers fall back
// to a full purge when the slice is empty.
package wiring

import "github.com/goccy/go-json"

type invalidatePayload struct {
	Op  string   `json:"op"`
	IDs []string `json:"ids"`
}

// ParseInvalidateIDs decodes an invalidate-by-id payload.
func ParseInvalidateIDs(state []byte) []string {
	if len(state) == 0 {
		return nil
	}
	var p invalidatePayload
	if err := json.Unmarshal(state, &p); err != nil {
		return nil
	}
	if p.Op != "invalidate" {
		return nil
	}
	return p.IDs
}
