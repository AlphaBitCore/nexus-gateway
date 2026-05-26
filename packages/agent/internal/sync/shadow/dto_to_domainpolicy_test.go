package shadow

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/domain"
)

func TestToDomainPolicy_Empty(t *testing.T) {
	var s *ConfigSnapshot
	if got := s.ToDomainPolicy(); got != nil {
		t.Errorf("nil snapshot must produce nil slice, got %v", got)
	}
	empty := &ConfigSnapshot{}
	if got := empty.ToDomainPolicy(); len(got) != 0 {
		t.Errorf("empty snapshot must produce empty slice, got %d", len(got))
	}
}

func TestToDomainPolicy_BasicFieldsRoundTrip(t *testing.T) {
	s := &ConfigSnapshot{
		InterceptionDomains: []InterceptionDomainDTO{
			{
				ID:                "dom-1",
				Name:              "openai",
				HostPattern:       "api.openai.com",
				HostMatchType:     "EXACT",
				AdapterID:         "openai-compat",
				NetworkZone:       "PUBLIC",
				DefaultPathAction: "PROCESS",
				OnAdapterError:    "FAIL_OPEN",
				Enabled:           true,
				Priority:          100,
				Paths: []InterceptionPathDTO{
					{ID: "p-1", PathPattern: []string{"/v1/chat/completions"}, MatchType: "PREFIX", Action: "PROCESS", Enabled: true},
				},
			},
		},
	}
	got := s.ToDomainPolicy()
	if len(got) != 1 {
		t.Fatalf("want 1 domain, got %d", len(got))
	}
	d := got[0]
	if d.ID != "dom-1" || d.Name != "openai" || d.HostPattern != "api.openai.com" {
		t.Errorf("basic fields: %+v", d)
	}
	if d.HostMatchType != domain.HostMatchExact {
		t.Errorf("HostMatchType: got %q", d.HostMatchType)
	}
	if d.DefaultPathAction != domain.PathActionProcess {
		t.Errorf("DefaultPathAction: got %q", d.DefaultPathAction)
	}
	if len(d.Paths) != 1 || d.Paths[0].ID != "p-1" {
		t.Errorf("Paths not converted: %+v", d.Paths)
	}
	// All override pointers must be nil for a DTO with no overrides set.
	if d.StreamingMode != nil || d.CaptureRequestBody != nil || d.RawBodySpillEnabled != nil {
		t.Errorf("absent overrides must remain nil, got %+v / %v / %v",
			d.StreamingMode, d.CaptureRequestBody, d.RawBodySpillEnabled)
	}
}

func TestToDomainPolicy_PerHostOverridesPreserved(t *testing.T) {
	mode := "buffer_full_block"
	chunkBytes := 16384
	hookTimeoutMs := 8000
	maxBufferBytes := 4 * 1024 * 1024
	failBehavior := "fail_close"
	captureReq := true
	captureResp := false
	spill := true

	s := &ConfigSnapshot{
		InterceptionDomains: []InterceptionDomainDTO{
			{
				ID:                      "dom-2",
				Name:                    "anthropic",
				HostPattern:             "api.anthropic.com",
				HostMatchType:           "EXACT",
				Enabled:                 true,
				StreamingMode:           &mode,
				StreamingChunkBytes:     &chunkBytes,
				StreamingHookTimeoutMs:  &hookTimeoutMs,
				StreamingMaxBufferBytes: &maxBufferBytes,
				StreamingFailBehavior:   &failBehavior,
				CaptureRequestBody:      &captureReq,
				CaptureResponseBody:     &captureResp,
				RawBodySpillEnabled:     &spill,
			},
		},
	}
	got := s.ToDomainPolicy()
	d := got[0]
	if d.StreamingMode == nil || *d.StreamingMode != mode {
		t.Errorf("StreamingMode: got %v, want %q", d.StreamingMode, mode)
	}
	if d.StreamingChunkBytes == nil || *d.StreamingChunkBytes != chunkBytes {
		t.Errorf("StreamingChunkBytes: got %v, want %d", d.StreamingChunkBytes, chunkBytes)
	}
	if d.StreamingHookTimeoutMs == nil || *d.StreamingHookTimeoutMs != hookTimeoutMs {
		t.Errorf("StreamingHookTimeoutMs: got %v, want %d", d.StreamingHookTimeoutMs, hookTimeoutMs)
	}
	if d.StreamingMaxBufferBytes == nil || *d.StreamingMaxBufferBytes != maxBufferBytes {
		t.Errorf("StreamingMaxBufferBytes: got %v, want %d", d.StreamingMaxBufferBytes, maxBufferBytes)
	}
	if d.StreamingFailBehavior == nil || *d.StreamingFailBehavior != failBehavior {
		t.Errorf("StreamingFailBehavior: got %v, want %q", d.StreamingFailBehavior, failBehavior)
	}
	if d.CaptureRequestBody == nil || *d.CaptureRequestBody != true {
		t.Errorf("CaptureRequestBody: got %v, want true", d.CaptureRequestBody)
	}
	if d.CaptureResponseBody == nil || *d.CaptureResponseBody != false {
		t.Errorf("CaptureResponseBody: got %v, want false", d.CaptureResponseBody)
	}
	if d.RawBodySpillEnabled == nil || *d.RawBodySpillEnabled != true {
		t.Errorf("RawBodySpillEnabled: got %v, want true", d.RawBodySpillEnabled)
	}
}

func TestToDomainPolicy_DisabledPathsSkipped(t *testing.T) {
	s := &ConfigSnapshot{
		InterceptionDomains: []InterceptionDomainDTO{
			{
				ID:          "dom-3",
				HostPattern: "x.example.com",
				Enabled:     true,
				Paths: []InterceptionPathDTO{
					{ID: "p-on", PathPattern: []string{"/a"}, MatchType: "PREFIX", Action: "PROCESS", Enabled: true},
					{ID: "p-off", PathPattern: []string{"/b"}, MatchType: "PREFIX", Action: "BLOCK", Enabled: false},
				},
			},
		},
	}
	got := s.ToDomainPolicy()
	if len(got) != 1 {
		t.Fatalf("want 1 domain, got %d", len(got))
	}
	if len(got[0].Paths) != 1 {
		t.Fatalf("disabled path must be skipped; got %d paths: %+v", len(got[0].Paths), got[0].Paths)
	}
	if got[0].Paths[0].ID != "p-on" {
		t.Errorf("kept the wrong path: %s", got[0].Paths[0].ID)
	}
}
