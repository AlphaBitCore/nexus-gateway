package codecs

import (
	stdjson "encoding/json"
	"strings"
	"testing"

	goccy "github.com/goccy/go-json"
	"github.com/tidwall/gjson"
)

// realisticBody mirrors a typical chat-completion request/response that flows
// through the normalize sniff on every request.
var realisticBody = []byte(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"` +
	strings.Repeat("the quick brown fox ", 200) +
	`"}],"temperature":0.7,"max_tokens":1024,"stream":false,"metadata":{"a":1,"b":[1,2,3],"c":{"d":true}}}`)

// BenchmarkValid_Goccy_vs_Stdlib documents WHY the sniff moved off goccy: goccy's
// json.Valid decodes into interface{} (allocates ~body-size), while stdlib's is a
// zero-alloc scanner. Run: go test -run x -bench Valid -benchmem ./codecs/
func BenchmarkValid_Goccy(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(realisticBody)))
	for range b.N {
		if !goccy.Valid(realisticBody) {
			b.Fatal("invalid")
		}
	}
}

func BenchmarkValid_Stdlib(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(realisticBody)))
	for range b.N {
		if !stdjson.Valid(realisticBody) {
			b.Fatal("invalid")
		}
	}
}

func BenchmarkValid_Gjson(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(realisticBody)))
	for range b.N {
		if !gjson.ValidBytes(realisticBody) {
			b.Fatal("invalid")
		}
	}
}

// TestValid_SniffSafetyInvariant guards the two properties that make swapping the
// JSON-document/NDJSON sniff off goccy.Valid (22KB/op, decodes into interface{})
// onto stdlib encoding/json.Valid (zero-alloc) safe:
//
//  1. stdlib and gjson agree on every case (the two zero-alloc options are
//     interchangeable — both implement strict RFC 8259).
//  2. SAFETY INVARIANT: stdlib.Valid(x) ⇒ goccy.Valid(x). The sniff feeds the
//     JSON path, whose decoder is goccy.Unmarshal. Because stdlib is STRICTER than
//     goccy (goccy leniently accepts a bare `nul`, a raw tab in a string, a
//     leading-zero number), anything stdlib calls valid goccy will also decode —
//     so the sniff never claims a body the decoder would reject (the documented
//     looksLikeJSONDocument invariant). The only effect of the swap is that a few
//     RFC-MALFORMED bodies goccy would leniently decode now route to the text path
//     instead — strictly safer (raw bytes are still preserved) and arguably more
//     correct. goccy's extra leniencies are logged, not failed.
func TestValid_SniffSafetyInvariant(t *testing.T) {
	cases := [][]byte{
		[]byte(`{}`),
		[]byte(`[]`),
		[]byte(`{"a":1}`),
		[]byte(`{"a":1,}`), // trailing comma -> invalid
		[]byte(`{"a":}`),   // missing value -> invalid
		[]byte(`[1,2,3]`),
		[]byte(`[1,2,3,]`), // trailing comma -> invalid
		[]byte(`"just a string"`),
		[]byte(`123`),
		[]byte(`1.5e10`),
		[]byte(`true`),
		[]byte(`null`),
		[]byte(`nul`),             // invalid literal
		[]byte(``),                // empty -> invalid
		[]byte(`   `),             // whitespace only -> invalid
		[]byte(`{"a":1} {"b":2}`), // two docs -> invalid as one value
		[]byte(`{"a":1}` + "\n"),  // trailing newline -> valid
		[]byte(`{"nested":{"deep":[{"x":[1,{"y":null}]}]}}`),
		[]byte(`{"unicode":"héllo 世界  "}`),
		[]byte(`{"dup":1,"dup":2}`),   // duplicate keys -> both accept (RFC)
		[]byte("{\"ctrl\":\"a\tb\"}"), // raw tab in string -> invalid JSON
		[]byte(`{"num":01}`),          // leading zero -> invalid
		[]byte(`{"num":.5}`),          // bare fraction -> invalid
		[]byte(`NaN`),                 // not JSON
		realisticBody,
	}
	for i, c := range cases {
		g := goccy.Valid(c)
		s := stdjson.Valid(c)
		gj := gjson.ValidBytes(c)
		// (1) the two zero-alloc validators must agree.
		if s != gj {
			t.Errorf("case %d %q: stdlib=%v but gjson=%v (zero-alloc options disagree)", i, c, s, gj)
		}
		// (2) safety invariant: stdlib-valid ⇒ goccy-valid (never over-claim).
		if s && !g {
			t.Errorf("case %d %q: stdlib says valid but goccy (the decoder) rejects — swap would over-claim", i, c)
		}
		if g != s {
			t.Logf("case %d %-28q goccy leniently accepts, stdlib rejects -> routes to text (intended)", i, c)
		}
	}
}
