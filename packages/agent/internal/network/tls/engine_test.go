package tls

import (
	"testing"
	"time"
)

func TestNewEngine_GeneratesCA(t *testing.T) {
	e, err := NewEngine(nil, nil, 10, time.Hour)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if e.CACert() == nil {
		t.Fatal("CA cert should not be nil")
	}
	if !e.CACert().IsCA {
		t.Error("CA cert should have IsCA=true")
	}
	// The fleet-trusted MITM root must be path-length zero so a
	// stolen key cannot mint a working intermediate CA.
	ca := e.CACert()
	if !ca.MaxPathLenZero || ca.MaxPathLen != 0 {
		t.Errorf("device CA must be MaxPathLenZero (got MaxPathLenZero=%v MaxPathLen=%d)", ca.MaxPathLenZero, ca.MaxPathLen)
	}
	pem := e.CACertPEM()
	if len(pem) == 0 {
		t.Error("CA PEM should not be empty")
	}
}
