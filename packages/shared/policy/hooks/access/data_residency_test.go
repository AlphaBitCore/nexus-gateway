package access

import (
	"context"
	"testing"
)

func newTestResidencyHook(t *testing.T) Hook {
	t.Helper()
	h, err := NewDataResidency(&HookConfig{
		ID:               "dr-1",
		ImplementationID: "data-residency",
		Name:             "Data Residency",
		Config: map[string]any{
			"policies": []any{
				map[string]any{"classification": "CONFIDENTIAL", "allowedRegions": []any{"eu-west-1", "eu-central-1"}},
				map[string]any{"classification": "RESTRICTED", "allowedRegions": []any{"eu-west-1"}},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestDataResidencyAllowsPublic(t *testing.T) {
	h := newTestResidencyHook(t)

	resp, err := h.Execute(context.Background(), &HookInput{
		UpstreamTags:   []string{"severity:public"},
		ProviderRegion: "us-east-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Decision != Approve {
		t.Errorf("public data should be approved, got %s", resp.Decision)
	}
}

func TestDataResidencyAllowsEmptyClassification(t *testing.T) {
	h := newTestResidencyHook(t)

	resp, err := h.Execute(context.Background(), &HookInput{
		ProviderRegion: "us-east-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Decision != Approve {
		t.Errorf("empty classification should be approved, got %s", resp.Decision)
	}
}

func TestDataResidencyAllowsCorrectRegion(t *testing.T) {
	h := newTestResidencyHook(t)

	resp, err := h.Execute(context.Background(), &HookInput{
		UpstreamTags:   []string{"severity:confidential"},
		ProviderRegion: "eu-west-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Decision != Approve {
		t.Errorf("CONFIDENTIAL to allowed region should be approved, got %s", resp.Decision)
	}
}

func TestDataResidencyBlocksWrongRegion(t *testing.T) {
	h := newTestResidencyHook(t)

	resp, err := h.Execute(context.Background(), &HookInput{
		UpstreamTags:   []string{"severity:confidential"},
		ProviderRegion: "us-east-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Decision != RejectHard {
		t.Errorf("CONFIDENTIAL to wrong region should be rejected, got %s", resp.Decision)
	}
	if resp.ReasonCode != "DATA_RESIDENCY_VIOLATION" {
		t.Errorf("reason code = %q, want DATA_RESIDENCY_VIOLATION", resp.ReasonCode)
	}
}

func TestDataResidencyBlocksUnknownRegion(t *testing.T) {
	h := newTestResidencyHook(t)

	resp, err := h.Execute(context.Background(), &HookInput{
		UpstreamTags:   []string{"severity:restricted"},
		ProviderRegion: "",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Decision != RejectHard {
		t.Errorf("RESTRICTED to unknown region should be rejected, got %s", resp.Decision)
	}
	if resp.ReasonCode != "DATA_RESIDENCY_UNKNOWN_REGION" {
		t.Errorf("reason code = %q, want DATA_RESIDENCY_UNKNOWN_REGION", resp.ReasonCode)
	}
}

func TestDataResidencyNoPolicyForClassification(t *testing.T) {
	h := newTestResidencyHook(t)

	resp, err := h.Execute(context.Background(), &HookInput{
		UpstreamTags:   []string{"severity:internal"},
		ProviderRegion: "us-east-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Decision != Approve {
		t.Errorf("classification without policy should be approved, got %s", resp.Decision)
	}
}

func TestDataResidencyCaseInsensitiveRegion(t *testing.T) {
	h := newTestResidencyHook(t)

	resp, err := h.Execute(context.Background(), &HookInput{
		UpstreamTags:   []string{"severity:confidential"},
		ProviderRegion: "EU-WEST-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Decision != Approve {
		t.Errorf("region matching should be case-insensitive, got %s", resp.Decision)
	}
}

func TestDataResidencyEmptyPolicies(t *testing.T) {
	h, err := NewDataResidency(&HookConfig{
		ID:               "dr-empty",
		ImplementationID: "data-residency",
		Name:             "Data Residency",
		Config:           map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}

	resp, err := h.Execute(context.Background(), &HookInput{
		UpstreamTags:   []string{"severity:confidential"},
		ProviderRegion: "us-east-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Decision != Approve {
		t.Errorf("no policies configured should approve, got %s", resp.Decision)
	}
}

func TestDataResidencyConfigValidation(t *testing.T) {
	tests := []struct {
		name    string
		config  map[string]any
		wantErr bool
	}{
		{
			name:    "policies not an array",
			config:  map[string]any{"policies": "bad"},
			wantErr: true,
		},
		{
			name:    "policy not an object",
			config:  map[string]any{"policies": []any{"bad"}},
			wantErr: true,
		},
		{
			name:    "missing classification",
			config:  map[string]any{"policies": []any{map[string]any{"allowedRegions": []any{"us-east-1"}}}},
			wantErr: true,
		},
		{
			name:    "missing allowedRegions",
			config:  map[string]any{"policies": []any{map[string]any{"classification": "PII"}}},
			wantErr: true,
		},
		{
			name:    "allowedRegions not array",
			config:  map[string]any{"policies": []any{map[string]any{"classification": "PII", "allowedRegions": "bad"}}},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewDataResidency(&HookConfig{
				ID:     "dr-test",
				Config: tt.config,
			})
			if (err != nil) != tt.wantErr {
				t.Errorf("NewDataResidency() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
