package core

import (
	"bytes"
	"github.com/goccy/go-json"
	"sort"
	"sync"
	"testing"
)

// refMarshalSortedKeys is the pre-refactor return-and-copy implementation, kept
// as the byte-identity ORACLE: the pooled single-buffer canonicalizeJSON must
// produce output byte-identical to this across the corpus, proving cache keys
// are unchanged (a single differing byte = a full cache miss on every entry).
func refMarshalSortedKeys(v any) ([]byte, error) {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var buf bytes.Buffer
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			kb, err := json.Marshal(k)
			if err != nil {
				return nil, err
			}
			buf.Write(kb)
			buf.WriteByte(':')
			vb, err := refMarshalSortedKeys(x[k])
			if err != nil {
				return nil, err
			}
			buf.Write(vb)
		}
		buf.WriteByte('}')
		return buf.Bytes(), nil
	case []any:
		var buf bytes.Buffer
		buf.WriteByte('[')
		for i, item := range x {
			if i > 0 {
				buf.WriteByte(',')
			}
			ib, err := refMarshalSortedKeys(item)
			if err != nil {
				return nil, err
			}
			buf.Write(ib)
		}
		buf.WriteByte(']')
		return buf.Bytes(), nil
	default:
		return json.Marshal(x)
	}
}

func refCanonicalize(body []byte) []byte {
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return body
	}
	out, err := refMarshalSortedKeys(v)
	if err != nil {
		return body
	}
	return out
}

var canonCorpus = [][]byte{
	[]byte(`{}`),
	[]byte(`[]`),
	[]byte(`{"b":1,"a":2}`),
	[]byte(`{"z":{"y":1,"x":2},"a":[3,2,1]}`),
	[]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"}],"temperature":0.7,"max_tokens":128}`),
	[]byte(`{"unicode":"héllo 世界 é","esc":"a\"b\\c\nd\te","amp":"<>&"}`),
	[]byte(`{"nums":[1,2.5,-3,1e10,0,1.0,123456789012345],"bool":true,"null":null}`),
	[]byte(`{"nested":{"deep":{"deeper":{"k":[{"a":1},{"b":2}]}}}}`),
	[]byte(`{"empty_obj":{},"empty_arr":[],"empty_str":""}`),
	[]byte(`not valid json`),                  // pass-through unchanged
	[]byte(`{"ZZZ":3,"aaa":4,"Aaa":5,"b":1}`), // case-sensitive key sort
	// Review-recommended hardening for the cache-key byte-identity gate:
	[]byte(`"hello"`),                  // top-level scalar string (default branch at root)
	[]byte(`123`),                      // top-level number
	[]byte(`true`),                     // top-level bool
	[]byte(`null`),                     // top-level null
	[]byte(`{"e":"😀","pair":"𝄞"}`),     // astral-plane / surrogate-pair runes
	[]byte(`{"a\"b":1,"":2,"x\ny":3}`), // keys requiring JSON escaping
	[]byte(`{"f":[1e308,-0,123456789012345678,1e-9]}`), // float64 reformatting edges
}

// TestCanonicalizeJSON_ByteIdentityVsReference is the F-lite gate: the pooled
// single-buffer canonicalizer must be byte-for-byte identical to the prior
// implementation, so no cache key changes.
func TestCanonicalizeJSON_ByteIdentityVsReference(t *testing.T) {
	for i, body := range canonCorpus {
		got := canonicalizeJSON(body)
		want := refCanonicalize(body)
		if !bytes.Equal(got, want) {
			t.Fatalf("corpus[%d] canonicalize mismatch:\n body=%s\n got =%s\n want=%s", i, body, got, want)
		}
		// Idempotence: canonicalizing the canonical form reproduces it (a
		// fixed-point guard — the cache key is stable across re-canonicalization).
		if again := canonicalizeJSON(got); !bytes.Equal(again, got) {
			t.Fatalf("corpus[%d] not idempotent:\n once=%s\n twice=%s", i, got, again)
		}
	}
}

// TestCanonicalizeJSON_ConcurrentPoolSafe exercises the shared scratch pool
// under concurrency: the copy-out must make each result independent (no
// cross-goroutine corruption of the cache key). Run under -race.
func TestCanonicalizeJSON_ConcurrentPoolSafe(t *testing.T) {
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 300 {
				body := canonCorpus[i%len(canonCorpus)]
				got := canonicalizeJSON(body)
				want := refCanonicalize(body)
				if !bytes.Equal(got, want) {
					t.Errorf("concurrent canonicalize mismatch for %s", body)
					return
				}
			}
		}()
	}
	wg.Wait()
}
