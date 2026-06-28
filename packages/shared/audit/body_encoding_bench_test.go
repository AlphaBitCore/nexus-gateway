package audit_test

import (
	"encoding/base64"
	stdjson "encoding/json"
	"os"
	"testing"

	gojson "github.com/goccy/go-json"
)

// loadCorpus reads a real captured body dumped from the perf rig. Absent → skip,
// so the benchmark is a local decision tool, not a CI dependency.
func loadCorpus(b *testing.B, path string) []byte {
	b.Helper()
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		b.Skipf("corpus %s absent (%v) — run the perf rig dump first", path, err)
	}
	// psql -At appends a trailing newline; trim it so we bench the body only.
	if n := len(data); n > 0 && data[n-1] == '\n' {
		data = data[:n-1]
	}
	return data
}

// 子提交② wire-encoding decision: for an SSE response body (valid UTF-8, NOT valid
// JSON), compare the three ways it can ride inside the audit message —
//   base64 (today's path for non-JSON), escaped JSON string (the json.Valid-gated
//   hybrid's "text" form), and (for reference) the verbatim nested splice that only
//   valid-JSON bodies can use.
// We measure B/op + ns/op AND the resulting wire byte size (drives streaming-drain
// data volume). Run: go test ./audit/ -run x -bench BodyWireEncoding -benchmem.

func BenchmarkBodyWireEncoding_SSE_Base64(b *testing.B) {
	body := loadCorpus(b, "/tmp/nexus-local/corpus_sse.txt")
	b.ReportAllocs()
	var last int
	for range b.N {
		s := base64.StdEncoding.EncodeToString(body)
		last = len(s)
	}
	b.ReportMetric(float64(last), "wire_bytes")
}

func BenchmarkBodyWireEncoding_SSE_EscapedString_stdlib(b *testing.B) {
	body := loadCorpus(b, "/tmp/nexus-local/corpus_sse.txt")
	b.ReportAllocs()
	var last int
	for range b.N {
		out, _ := stdjson.Marshal(string(body))
		last = len(out)
	}
	b.ReportMetric(float64(last), "wire_bytes")
}

func BenchmarkBodyWireEncoding_SSE_EscapedString_goccy(b *testing.B) {
	body := loadCorpus(b, "/tmp/nexus-local/corpus_sse.txt")
	b.ReportAllocs()
	var last int
	for range b.N {
		out, _ := gojson.Marshal(string(body))
		last = len(out)
	}
	b.ReportMetric(float64(last), "wire_bytes")
}

// Reference: a valid-JSON request body rides as a verbatim nested splice (no
// escape, no base64) — this is what json.Valid GATES, and why deleting json.Valid
// would force this body onto the escaped-string path instead. Measure the splice
// (a copy) vs what escaping it would cost.
func BenchmarkBodyWireEncoding_JSONReq_VerbatimSplice(b *testing.B) {
	body := loadCorpus(b, "/tmp/nexus-local/corpus_json_req.txt")
	b.ReportAllocs()
	var last int
	for range b.N {
		out := make([]byte, len(body))
		copy(out, body) // verbatim splice = one copy, no escape/validate
		last = len(out)
	}
	b.ReportMetric(float64(last), "wire_bytes")
}

func BenchmarkBodyWireEncoding_JSONReq_IfEscaped(b *testing.B) {
	body := loadCorpus(b, "/tmp/nexus-local/corpus_json_req.txt")
	b.ReportAllocs()
	var last int
	for range b.N {
		out, _ := gojson.Marshal(string(body)) // what deleting json.Valid would force
		last = len(out)
	}
	b.ReportMetric(float64(last), "wire_bytes")
}
