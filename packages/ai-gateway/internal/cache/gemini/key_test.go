package geminicache

import "testing"

// TestContentHash_JSONCanonicalization pins the fix that made the Gemini
// prompt-cache key insensitive to JSON whitespace and field ordering.
// Without this normalization, /v1beta native ingress and
// /v1/responses cross-format Gemini hash the SAME logical
// systemInstruction to DIFFERENT Redis keys — the user observed the
// asymmetry as "/v1/responses caches hit, /v1/beta misses". Three
// permutations of the same JSON must collapse to one key.
func TestContentHash_JSONCanonicalization(t *testing.T) {
	const provider = "openai"
	const model = "gemini-2.5-flash"

	cases := []string{
		// Compact, parts-then-role (canonical bridge output)
		`{"parts":[{"text":"system body"}],"role":"system"}`,
		// Pretty, role-then-parts (typical SDK / smoke output)
		`{"role": "system", "parts": [{"text": "system body"}]}`,
		// Mixed whitespace
		`{ "role":"system","parts":[ {"text":"system body"} ] }`,
	}
	keys := make([]string, 0, len(cases))
	for _, c := range cases {
		k := contentHash(provider, model, c, "", "")
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		if keys[i] != keys[0] {
			t.Fatalf("permutation %d hashed differently:\n  base (%s) → %s\n  perm (%s) → %s",
				i, cases[0], keys[0], cases[i], keys[i])
		}
	}
}

// TestContentHash_DistinguishesDifferentContent guards against the
// canonicalization over-collapsing — content that differs in value
// must still hash differently.
func TestContentHash_DistinguishesDifferentContent(t *testing.T) {
	a := contentHash("openai", "gemini-2.5-flash", `{"role":"system","parts":[{"text":"A"}]}`, "", "")
	b := contentHash("openai", "gemini-2.5-flash", `{"role":"system","parts":[{"text":"B"}]}`, "", "")
	if a == b {
		t.Fatalf("different content hashed to same key: %s", a)
	}
}

// TestContentHash_MalformedJSONFallback ensures invalid JSON doesn't
// blow up — it falls back to the raw string so the hash is still
// stable for that input (no unexpected key collapse to a single value).
func TestContentHash_MalformedJSONFallback(t *testing.T) {
	a := contentHash("p", "m", "{not valid json", "", "")
	b := contentHash("p", "m", "{also not valid json", "", "")
	if a == b {
		t.Fatalf("two distinct malformed inputs collapsed to one key")
	}
}
