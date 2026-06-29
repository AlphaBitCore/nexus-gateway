package proxy

import (
	"os"
	"testing"
)

// TestMain pins the JSON audit wire as this package's test baseline.
//
// The production default wire is BINARY (see the audit Writer / NEXUS_AUDIT_WIRE),
// but many proxy tests assert on the emitted audit envelope by decoding it as JSON
// (the binary TLV record has no decoder in this package — that lives in the Hub
// consumer package, where the binwire round-trip/frame tests verify it end to end).
// So the proxy tests run on the JSON wire; an explicit env override set by the
// caller (e.g. a CI wire matrix) is respected.
func TestMain(m *testing.M) {
	if _, set := os.LookupEnv("NEXUS_AUDIT_WIRE"); !set {
		os.Setenv("NEXUS_AUDIT_WIRE", "json")
	}
	os.Exit(m.Run())
}
