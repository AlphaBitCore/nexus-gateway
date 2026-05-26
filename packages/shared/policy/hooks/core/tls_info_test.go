package core

import "testing"

func TestTLSInfo_Struct_Fields(t *testing.T) {
	i := TLSInfo{SNI: "api.openai.com", ClientCertFingerprint: "sha256:abc"}
	if i.SNI != "api.openai.com" || i.ClientCertFingerprint != "sha256:abc" {
		t.Fatal("TLSInfo fields not roundtrip")
	}
}
