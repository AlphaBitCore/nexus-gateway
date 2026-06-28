package wiring

import (
	"os"
	"testing"
)

// TestMain pins the JSON audit wire as this package's test baseline. The
// production default wire is BINARY (NEXUS_AUDIT_WIRE), but the wiring tests here
// assert on a published audit payload by JSON-unmarshaling it (e.g. the
// WriterBackedTrafficSink trace-id / cost stamping check); decoding the binary TLV
// wire lives in the Hub consumer package, not here. An explicit env override set by
// the caller (e.g. a CI wire matrix) is respected.
func TestMain(m *testing.M) {
	if _, set := os.LookupEnv("NEXUS_AUDIT_WIRE"); !set {
		os.Setenv("NEXUS_AUDIT_WIRE", "json")
	}
	os.Exit(m.Run())
}
