package audit

import (
	"os"
	"testing"
)

// TestMain pins the JSON audit wire as this package's default test baseline.
//
// The production default wire is BINARY (NewWriter / NEXUS_AUDIT_WIRE), but decoding
// a binary TLV record back into a TrafficEventMessage lives in the Hub consumer
// package, not here — that is where the binwire round-trip / frame / all-fields /
// pooled tests verify the binary wire end to end. The tests in this package that
// decode a published payload to assert its carried fields therefore run on the JSON
// wire. Tests that exercise the binary publish/framing path itself opt back in with
// t.Setenv("NEXUS_AUDIT_WIRE", "binary") and assert by the magic+length-prefixed
// framing (countFrameRecords), not by decoding. An explicit env override set by the
// caller (e.g. a CI wire matrix) is respected.
func TestMain(m *testing.M) {
	if _, set := os.LookupEnv("NEXUS_AUDIT_WIRE"); !set {
		os.Setenv("NEXUS_AUDIT_WIRE", "json")
	}
	os.Exit(m.Run())
}
