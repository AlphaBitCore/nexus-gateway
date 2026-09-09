// Package proxy — every channel the client receives is a channel the scanner sees.
//
// Named failure modes:
//   - a delivered channel is absent from the prescan input, so a value that
//     appears only there never triggers the confirm and is delivered raw
//   - ContentBytes disagrees with AppendRedactableText, so the engine admits or
//     evicts units from the tail window by a number that does not match what it
//     scanned
package proxy

import (
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// deliveredChannels enumerates what a canonical chunk can carry TO THE CLIENT,
// with a distinctive value in each. A channel added to provcore.Chunk and
// encoded onto the wire but forgotten here — and therefore forgotten in
// AppendRedactableText — is exactly the defect this file exists to catch; it
// has happened twice, for `refusal` and for `reasoning`.
func deliveredChannels() (provcore.Chunk, map[string]string) {
	want := map[string]string{
		"content":        "CONTENT-4111111111111111",
		"refusal":        "REFUSAL-4222222222222222",
		"reasoning":      "REASONING-4333333333333333",
		"tool arguments": `{"ssn":"REASONS-444-44-4444"}`,
		"tool name":      "TOOLNAME-lookup_customer",
		"tool id":        "TOOLID-call_abc123",
	}
	return provcore.Chunk{
		Delta:          want["content"],
		RefusalDelta:   want["refusal"],
		ReasoningDelta: want["reasoning"],
		ToolCallDeltas: []provcore.ToolCallDelta{{
			Arguments: want["tool arguments"],
			Name:      want["tool name"],
			ID:        want["tool id"],
		}},
	}, want
}

// TestEveryDeliveredChannelReachesThePrescan is the gate.
//
// The prescan decides whether the expensive confirm runs at all, so a channel
// missing from this text is invisible to BOTH passes: the rule never fires, and
// the value is delivered. That is not a weaker scan, it is no scan.
func TestEveryDeliveredChannelReachesThePrescan(t *testing.T) {
	sub := &modelACanonicalSubstrate{}
	chunk, want := deliveredChannels()

	got := string(sub.AppendRedactableText(nil, chunk))
	for channel, marker := range want {
		if !strings.Contains(got, marker) {
			t.Errorf("the %s channel is absent from the scanned text — a rule matching only "+
				"there would never fire, and the value reaches the client:\n  scanned = %q",
				channel, got)
		}
	}
}

// TestContentBytesMatchesWhatIsScanned holds the engine's symmetry requirement:
// ContentBytes MUST measure the same content AppendRedactableText emits. The
// engine admits and evicts units from the tail window by that number, so an
// under-report evicts scanned content early and an over-report evicts the tail
// before the bytes completing a pattern arrive.
//
// Separators are the one allowed difference — ContentBytes counts content
// without them — so the check is that the two move together, not that they are
// equal.
func TestContentBytesMatchesWhatIsScanned(t *testing.T) {
	sub := &modelACanonicalSubstrate{}
	chunk, _ := deliveredChannels()

	scanned := len(sub.AppendRedactableText(nil, chunk))
	counted := sub.ContentBytes(chunk)

	// One newline per non-empty tool-call field.
	separators := 3
	if counted != scanned-separators {
		t.Errorf("ContentBytes = %d but AppendRedactableText emitted %d bytes (%d of them "+
			"separators). The window is budgeted by the first number and filled by the "+
			"second; when they disagree the engine holds the wrong amount.",
			counted, scanned, separators)
	}

	// And the halves must move together: adding content to any one channel has
	// to move both numbers by the same amount.
	bigger := chunk
	bigger.ReasoningDelta += "MORE"
	if d1, d2 := sub.ContentBytes(bigger)-counted,
		len(sub.AppendRedactableText(nil, bigger))-scanned; d1 != d2 {
		t.Errorf("adding 4 bytes of reasoning moved ContentBytes by %d and the scanned text "+
			"by %d — the two are measuring different things", d1, d2)
	}
}
