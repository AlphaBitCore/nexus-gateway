package audit

import (
	"github.com/goccy/go-json"

	sharedaudit "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
)

// extractInlineBytes pulls the raw byte slice from a body envelope JSON
// (the wire form produced by spillstore.EmitBody → audit.Body) when the
// body kind is "inline". Returns nil for absent / spill-ref bodies — the
// caller treats a nil slice as "no inline bytes available".
func extractInlineBytes(envelope []byte) []byte {
	if len(envelope) == 0 {
		return nil
	}
	var body sharedaudit.Body
	if err := json.Unmarshal(envelope, &body); err != nil {
		return nil
	}
	if body.Kind != sharedaudit.BodyInline {
		return nil
	}
	return body.InlineBytes
}

// nilJSONIfEmpty returns nil for an empty byte slice; otherwise wraps the
// bytes as json.RawMessage. Mirrors traffic.go's nullableJSON.
func nilJSONIfEmpty(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return json.RawMessage(b)
}

// nilIfEmpty is the same helper traffic.go uses — empty string maps to SQL
// NULL, non-empty stays as the value.
func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
