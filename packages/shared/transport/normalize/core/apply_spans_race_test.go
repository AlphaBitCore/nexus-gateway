package core

import (
	"fmt"
	"sync"
	"testing"

	"github.com/goccy/go-json"
)

// TestApplySpans_ConcurrentToolLeafWriteBack is the regression guard for the
// deferred-write data race: ApplySpans is called concurrently per request
// (the pipeline join + ToolCallArgsFromPayload), and the tool-leaf / map-entry
// write-back closures used to live on package-global slices with no lock. Two
// goroutines appending to + truncating one shared slice would race, and one
// goroutine's flush could drop another's masking closure, leaving PII
// UNMASKED on the wire.
//
// Each goroutine masks a DISTINCT tool-call argument leaf (a unique email per
// payload) and asserts its own result is fully masked. With the package
// globals this both data-races (caught by `go test -race`) and intermittently
// leaks an unmasked leaf; with the per-call writeCtx accumulator every result
// is deterministically masked and no shared state is touched.
func TestApplySpans_ConcurrentToolLeafWriteBack(t *testing.T) {
	const n = 64
	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for g := range n {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			email := fmt.Sprintf("user%d@example.com", g)
			full := "contact " + email
			p := NormalizedPayload{
				Kind:             KindAIChat,
				NormalizeVersion: SchemaVersion,
				Messages: []Message{{
					Role: RoleAssistant,
					Content: []ContentBlock{
						{Type: ContentText, Text: "calling"},
						{Type: ContentToolUse, ToolUse: &ToolUse{
							CallID: fmt.Sprintf("call_%d", g),
							Name:   "search",
							Input:  map[string]any{"query": full},
						}},
					},
				}},
			}
			repl := fmt.Sprintf("[R%d]", g)
			span := TransformSpan{
				Source: SourceHook, SourceID: "email", Action: ActionRedact,
				ContentAddress: "messages.0.content.1.toolUse.input.0",
				Start:          len("contact "), End: len(full), Replacement: repl,
			}
			out, skipped := ApplySpans(p, []TransformSpan{span})
			if len(skipped) != 0 {
				errCh <- fmt.Errorf("g=%d: unexpected skip %+v", g, skipped)
				return
			}
			got := out.Messages[0].Content[1].ToolUse.Input["query"].(string)
			want := "contact " + repl
			if got != want {
				errCh <- fmt.Errorf("g=%d: leaf not fully masked: got %q want %q", g, got, want)
				return
			}
		}(g)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}

// TestToolCallArgsFromPayload_ConcurrentDistinctCalls exercises the same race
// through the higher-level join helper that the wire rewriter consumes:
// concurrent ToolCallArgsFromPayload calls (each itself invoking ApplySpans)
// must each return their own masked arguments with no cross-talk.
func TestToolCallArgsFromPayload_ConcurrentDistinctCalls(t *testing.T) {
	const n = 48
	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for g := range n {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			email := fmt.Sprintf("p%d@example.com", g)
			full := "to " + email
			p := NormalizedPayload{
				Kind:             KindAIChat,
				NormalizeVersion: SchemaVersion,
				Messages: []Message{{
					Role: RoleAssistant,
					Content: []ContentBlock{
						{Type: ContentToolUse, ToolUse: &ToolUse{
							CallID: fmt.Sprintf("c%d", g),
							Name:   "send",
							Input:  map[string]any{"addr": full},
						}},
					},
				}},
			}
			repl := fmt.Sprintf("[M%d]", g)
			span := TransformSpan{
				Source: SourceHook, SourceID: "email", Action: ActionRedact,
				ContentAddress: "messages.0.content.0.toolUse.input.0",
				Start:          len("to "), End: len(full), Replacement: repl,
			}
			args := ToolCallArgsFromPayload(p, []TransformSpan{span})
			if len(args) != 1 {
				errCh <- fmt.Errorf("g=%d: want 1 arg, got %d", g, len(args))
				return
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(args[0]), &m); err != nil {
				errCh <- fmt.Errorf("g=%d: invalid JSON: %w", g, err)
				return
			}
			if m["addr"].(string) != "to "+repl {
				errCh <- fmt.Errorf("g=%d: masked arg wrong: %v", g, m)
				return
			}
		}(g)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}
