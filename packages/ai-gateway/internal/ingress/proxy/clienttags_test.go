package proxy

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestExtractClientTags pins the parse contract. The tags are stored and never
// interpreted, so the only behaviour worth pinning is what survives parsing:
// the caps that stop one caller inflating every traffic_event row, and the
// per-pair (not per-request) failure mode.
func TestExtractClientTags(t *testing.T) {
	h := func(v string) http.Header {
		hdr := http.Header{}
		if v != "" {
			hdr.Set("X-Nexus-Client-Tags", v)
		}
		return hdr
	}

	tests := []struct {
		name   string
		header string
		want   map[string]string
	}{
		{
			name:   "single pair",
			header: "billing_check=CHECKED",
			want:   map[string]string{"billing_check": "CHECKED"},
		},
		{
			name:   "multiple pairs",
			header: "tenant_id=42,billing_check=CHECKED",
			want:   map[string]string{"tenant_id": "42", "billing_check": "CHECKED"},
		},
		{
			name:   "surrounding whitespace trimmed on both sides",
			header: "  tenant_id = 42 , billing_check = CHECKED  ",
			want:   map[string]string{"tenant_id": "42", "billing_check": "CHECKED"},
		},
		{
			name:   "header absent",
			header: "",
			want:   nil,
		},
		{
			name:   "header present but blank",
			header: "   ",
			want:   nil,
		},
		{
			name:   "pair with no separator is dropped, siblings survive",
			header: "tenant_id=42,garbage,billing_check=CHECKED",
			want:   map[string]string{"tenant_id": "42", "billing_check": "CHECKED"},
		},
		{
			name:   "empty value is kept — the caller asserted the key",
			header: "billing_check=",
			want:   map[string]string{"billing_check": ""},
		},
		{
			name:   "empty key is dropped",
			header: "=CHECKED,tenant_id=42",
			want:   map[string]string{"tenant_id": "42"},
		},
		{
			name:   "uppercase key is dropped",
			header: "Billing_Check=CHECKED,tenant_id=42",
			want:   map[string]string{"tenant_id": "42"},
		},
		{
			name:   "key with punctuation is dropped",
			header: "billing.check=CHECKED,tenant_id=42",
			want:   map[string]string{"tenant_id": "42"},
		},
		{
			name:   "duplicate key: last wins",
			header: "billing_check=CHECKED,billing_check=FAILOPEN",
			want:   map[string]string{"billing_check": "FAILOPEN"},
		},
		{
			name:   "value containing an equals sign keeps it",
			header: "note=a=b",
			want:   map[string]string{"note": "a=b"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractClientTags(h(tc.header))
			if len(got) != len(tc.want) {
				t.Fatalf("extractClientTags = %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("tag %q = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

// TestExtractClientTagsPairCap pins the pair ceiling: the 9th pair and beyond
// are dropped, so a caller cannot widen every row without bound.
func TestExtractClientTagsPairCap(t *testing.T) {
	parts := []string{}
	for i := range clientTagsMaxPairs + 4 {
		parts = append(parts, "k"+string(rune('a'+i))+"=v")
	}
	hdr := http.Header{}
	hdr.Set("X-Nexus-Client-Tags", strings.Join(parts, ","))

	got := extractClientTags(hdr)
	if len(got) != clientTagsMaxPairs {
		t.Fatalf("len = %d, want %d", len(got), clientTagsMaxPairs)
	}
	if _, ok := got["ka"]; !ok {
		t.Error("first pair should survive the cap")
	}
}

// TestExtractClientTagsDuplicateKeyAtCap pins that a duplicate key at the cap
// does not consume a slot — the map stays at clientTagsMaxPairs, and the
// repeated key holds the later value.
func TestExtractClientTagsDuplicateKeyAtCap(t *testing.T) {
	parts := []string{}
	// Fill to exactly clientTagsMaxPairs with distinct keys.
	for i := range clientTagsMaxPairs {
		parts = append(parts, "k"+string(rune('a'+i))+"=v"+string(rune('0'+i)))
	}
	// Add a duplicate of the first key with a new value.
	parts = append(parts, "ka=newvalue")

	hdr := http.Header{}
	hdr.Set("X-Nexus-Client-Tags", strings.Join(parts, ","))

	got := extractClientTags(hdr)
	if len(got) != clientTagsMaxPairs {
		t.Fatalf("len = %d, want %d (duplicate should not consume a slot)", len(got), clientTagsMaxPairs)
	}
	if got["ka"] != "newvalue" {
		t.Errorf("duplicate key = %q, want %q", got["ka"], "newvalue")
	}
}

// TestExtractClientTagsValueCap pins the per-value byte cap and that the cut
// never leaves a torn multi-byte rune, matching the end-user tag's contract.
func TestExtractClientTagsValueCap(t *testing.T) {
	long := strings.Repeat("好", 300) // 900 bytes (3 bytes per rune)
	hdr := http.Header{}
	hdr.Set("X-Nexus-Client-Tags", "note="+long)

	got := extractClientTags(hdr)["note"]
	if len(got) > endUserMaxBytes {
		t.Errorf("value len = %d, want <= %d", len(got), endUserMaxBytes)
	}
	if !utf8.ValidString(got) {
		t.Error("capped value is not valid UTF-8")
	}
}

// TestExtractClientTagsInvalidOnlyIsNil pins that a header whose every pair is
// rejected produces nil, not an empty map — so buildDetails omits the key and
// existing rows stay byte-identical.
func TestExtractClientTagsInvalidOnlyIsNil(t *testing.T) {
	hdr := http.Header{}
	hdr.Set("X-Nexus-Client-Tags", "GARBAGE,Also.Bad=1")

	if got := extractClientTags(hdr); got != nil {
		t.Errorf("extractClientTags = %v, want nil", got)
	}
}

// TestExtractClientTagsHeaderByteCap pins the pre-split bound: content past
// clientTagsMaxHeaderBytes must never reach a parsed tag, because Split
// allocates over the WHOLE header before any per-pair cap applies — an
// unbounded header (Go's 1 MiB default MaxHeaderBytes is the only ceiling
// without this cap) burns CPU/allocation on the request path for a caller
// still inside their rate limit. A wall-clock assertion here would be flaky,
// so this pins the only thing that matters: which tags come back. The
// padding alone (2x the cap) already dwarfs clientTagsMaxHeaderBytes, and a
// valid pair placed after it must be cut away along with it.
func TestExtractClientTagsHeaderByteCap(t *testing.T) {
	padding := strings.Repeat(",", clientTagsMaxHeaderBytes*2)
	hdr := http.Header{}
	hdr.Set("X-Nexus-Client-Tags", "billing_check=CHECKED"+padding+",tenant_id=42")

	got := extractClientTags(hdr)
	if got["billing_check"] != "CHECKED" {
		t.Errorf("billing_check = %q, want %q (pair before the cut must survive)", got["billing_check"], "CHECKED")
	}
	if _, ok := got["tenant_id"]; ok {
		t.Error("tenant_id should not appear — it lies beyond clientTagsMaxHeaderBytes and must never be parsed")
	}
}

// TestExtractClientTagsHeaderByteCapLegalMax pins the boundary from the
// LEGAL side: exactly clientTagsMaxPairs pairs at the documented maximum key
// (32 bytes) and value (endUserMaxBytes) size must survive whole. An earlier
// version of clientTagsMaxHeaderBytes counted the pair bytes but not the
// (clientTagsMaxPairs-1) commas joining them, so this exact fully-legal shape
// came back with its last value truncated by 7 bytes — the cap was clipping
// input it was never meant to touch. All 8 keys must be present and every
// value must be the full endUserMaxBytes long.
func TestExtractClientTagsHeaderByteCapLegalMax(t *testing.T) {
	value := strings.Repeat("v", endUserMaxBytes)
	keys := make([]string, clientTagsMaxPairs)
	parts := make([]string, clientTagsMaxPairs)
	for i := range clientTagsMaxPairs {
		keys[i] = fmt.Sprintf("k%031d", i) // 32-byte lower_snake_case key
		parts[i] = keys[i] + "=" + value
	}
	hdr := http.Header{}
	hdr.Set("X-Nexus-Client-Tags", strings.Join(parts, ","))

	got := extractClientTags(hdr)
	if len(got) != clientTagsMaxPairs {
		t.Fatalf("len = %d, want %d — the fully legal maximum shape must not lose a key", len(got), clientTagsMaxPairs)
	}
	for _, key := range keys {
		if len(got[key]) != endUserMaxBytes {
			t.Errorf("value for %q has len %d, want %d (must not be truncated)", key, len(got[key]), endUserMaxBytes)
		}
	}
}
