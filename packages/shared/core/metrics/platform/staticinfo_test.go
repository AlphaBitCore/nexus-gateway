package platform

import (
	"testing"
)

func TestCaptureStaticInfoIncludesIdentity(t *testing.T) {
	info := CaptureStaticInfo(BuildInfo{
		ServiceVersion: "v1.4.2",
		BuildSHA:       "abc123",
		BuildTime:      "2026-04-25T10:00:00Z",
		StartTime:      "2026-04-26T08:00:00Z",
	})
	if info.Hostname == "" {
		t.Error("hostname not captured")
	}
	if info.OS == "" {
		t.Error("os not captured")
	}
	if info.CPUCores <= 0 {
		t.Errorf("cpuCores = %d, want >0", info.CPUCores)
	}
	if info.ServiceVersion != "v1.4.2" {
		t.Errorf("serviceVersion mismatch: %q", info.ServiceVersion)
	}
}
