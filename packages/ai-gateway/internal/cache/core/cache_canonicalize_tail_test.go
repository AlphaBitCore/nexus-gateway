package core

// Investigation harness (latency-tail study, not a coverage test):
// quantifies how much transient allocation + CPU the per-request cache-key
// canonicalization (canonicalizeJSON, reached on every cache-enabled request
// with no x-nexus-aigw-no-cache header via BuildScopedKey) costs as the
// request body grows. The hypothesis under test: this cost scales with body
// size, so a long-context (~12.5k-token) workload pays a large per-request
// allocation here even when hooks are OFF and the lookup always MISSes —
// which feeds GC pressure and an intermittent p95 tail.
//
// Run:
//   go test ./packages/ai-gateway/internal/cache/core/ -run TestCanonicalizeTail -v
//   go test ./packages/ai-gateway/internal/cache/core/ -bench BenchmarkCanonicalizeTail -benchmem -run x

import (
	"github.com/goccy/go-json"
	"runtime"
	"strings"
	"testing"
)

// chatBodySingleString builds an OpenAI chat-completions body whose user
// message is one large string of approxChars characters — the "padded
// document" shape (one big content blob per request).
func chatBodySingleString(approxChars int) []byte {
	content := strings.Repeat("The quick brown fox jumps over the lazy dog. ", approxChars/45+1)
	content = content[:approxChars]
	b, _ := json.Marshal(map[string]any{
		"model":    "gpt-4o-mini",
		"stream":   false,
		"messages": []any{map[string]any{"role": "user", "content": content}},
	})
	return b
}

// chatBodyStructured builds a body of the same on-wire size but spread across
// many small messages/values — the worst case for json.Unmarshal-into-any,
// because every scalar is boxed into an interface and re-marshaled.
func chatBodyStructured(approxChars int) []byte {
	const perMsg = 240
	n := approxChars / perMsg
	if n < 1 {
		n = 1
	}
	msgs := make([]any, 0, n)
	for i := range n {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, map[string]any{
			"role":    role,
			"content": strings.Repeat("lorem ipsum dolor sit amet ", perMsg/27+1)[:perMsg],
		})
	}
	b, _ := json.Marshal(map[string]any{
		"model":    "gpt-4o-mini",
		"stream":   false,
		"messages": msgs,
	})
	return b
}

// TestCanonicalizeTail_ScalesWithBodySize asserts the mechanism: canonicalize
// of a ~12.5k-token body allocates dramatically more than a small body, and
// the structured shape is worse than the single-string shape. It also proves
// canonicalizeJSON fully parses (key-sorting is observable), confirming it is
// a full unmarshal-into-any, not a cheap byte scan.
func TestCanonicalizeTail_ScalesWithBodySize(t *testing.T) {
	// Proof it fully parses: reordered keys must canonicalize identically.
	a := canonicalizeJSON([]byte(`{"b":1,"a":2,"messages":[{"role":"user","content":"hi"}]}`))
	bb := canonicalizeJSON([]byte(`{"a":2,"messages":[{"content":"hi","role":"user"}],"b":1}`))
	if string(a) != string(bb) {
		t.Fatalf("canonicalizeJSON did not sort keys identically:\n a=%s\n b=%s", a, bb)
	}

	small := chatBodySingleString(200)     // ~50 tokens
	bigStr := chatBodySingleString(50000)  // ~12.5k tokens, one blob
	bigStruct := chatBodyStructured(50000) // ~12.5k tokens, many values

	allocSmall := allocBytesPerOp(func() { _ = canonicalizeJSON(small) })
	allocBigStr := allocBytesPerOp(func() { _ = canonicalizeJSON(bigStr) })
	allocBigStruct := allocBytesPerOp(func() { _ = canonicalizeJSON(bigStruct) })

	t.Logf("body sizes:  small=%dB  bigSingleString=%dB  bigStructured=%dB", len(small), len(bigStr), len(bigStruct))
	t.Logf("canonicalize alloc/op:  small=%dB  bigSingleString=%dB  bigStructured=%dB",
		allocSmall, allocBigStr, allocBigStruct)
	t.Logf("structured/small alloc ratio = %.1fx", float64(allocBigStruct)/float64(allocSmall))

	if allocBigStr <= allocSmall {
		t.Errorf("expected big single-string body to allocate more than small; small=%d big=%d", allocSmall, allocBigStr)
	}
	if allocBigStruct <= allocBigStr {
		t.Errorf("expected structured big body to allocate more than single-string big body; single=%d struct=%d", allocBigStr, allocBigStruct)
	}
}

// allocBytesPerOp measures average bytes allocated per call to fn over a fixed
// iteration count using the runtime allocator counters (TotalAlloc is
// monotonic cumulative bytes, unaffected by GC between snapshots).
func allocBytesPerOp(fn func()) uint64 {
	const iters = 200
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for range iters {
		fn()
	}
	runtime.ReadMemStats(&after)
	return (after.TotalAlloc - before.TotalAlloc) / iters
}

func BenchmarkCanonicalizeTail_SingleString_Small(b *testing.B) {
	body := chatBodySingleString(200)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = canonicalizeJSON(body)
	}
}

func BenchmarkCanonicalizeTail_SingleString_12kTokens(b *testing.B) {
	body := chatBodySingleString(50000)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = canonicalizeJSON(body)
	}
}

func BenchmarkCanonicalizeTail_Structured_12kTokens(b *testing.B) {
	body := chatBodyStructured(50000)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = canonicalizeJSON(body)
	}
}
