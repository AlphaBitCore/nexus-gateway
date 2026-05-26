package core

import "testing"

func TestBuildHookConfig_ValidJSON(t *testing.T) {
	row := HookConfigRow{
		ID:                "h1",
		Name:              "keyword-filter",
		ImplementationID:  "keyword-filter",
		Stage:             "request",
		Enabled:           true,
		Priority:          10,
		TimeoutMs:         5000,
		FailBehavior:      "fail-open",
		ConfigJSON:        `{"keywords":["secret"]}`,
		ApplicableIngress: []string{"ALL"},
	}
	hc, err := BuildHookConfig(row)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hc.ID != "h1" {
		t.Fatalf("ID = %q, want h1", hc.ID)
	}
	if hc.Config["keywords"] == nil {
		t.Fatal("expected config.keywords to be set")
	}
}

func TestBuildHookConfig_NullJSON(t *testing.T) {
	row := HookConfigRow{
		ID:                "h2",
		Name:              "rate-limiter",
		ImplementationID:  "rate-limiter",
		Stage:             "request",
		Enabled:           true,
		Priority:          20,
		TimeoutMs:         1000,
		FailBehavior:      "fail-open",
		ConfigJSON:        "",
		ApplicableIngress: []string{"ALL"},
	}
	hc, err := BuildHookConfig(row)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hc.Config == nil {
		t.Fatal("Config should be empty map, not nil")
	}
	if len(hc.Config) != 0 {
		t.Fatalf("Config should be empty, got %d keys", len(hc.Config))
	}
}

func TestBuildHookConfig_MalformedJSON(t *testing.T) {
	row := HookConfigRow{
		ID:         "h3",
		ConfigJSON: "{invalid json",
	}
	_, err := BuildHookConfig(row)
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

// TestBuildHookConfig_EndpointFold verifies that the top-level endpoint
// column is visible to factories via cfg.Config["endpoint"], without
// overwriting an explicit value already present in the config JSON.
func TestBuildHookConfig_EndpointFold(t *testing.T) {
	t.Run("fold when config has no endpoint", func(t *testing.T) {
		hc, err := BuildHookConfig(HookConfigRow{
			ID:         "h-wh",
			ConfigJSON: `{"timeoutMs":1000}`,
			Endpoint:   "https://hooks.example.com/pipe",
		})
		if err != nil {
			t.Fatalf("BuildHookConfig: %v", err)
		}
		got, _ := hc.Config["endpoint"].(string)
		if got != "https://hooks.example.com/pipe" {
			t.Errorf("endpoint not folded; got %q", got)
		}
	})

	t.Run("config endpoint wins over column", func(t *testing.T) {
		hc, err := BuildHookConfig(HookConfigRow{
			ID:         "h-wh2",
			ConfigJSON: `{"endpoint":"https://explicit"}`,
			Endpoint:   "https://column",
		})
		if err != nil {
			t.Fatalf("BuildHookConfig: %v", err)
		}
		got, _ := hc.Config["endpoint"].(string)
		if got != "https://explicit" {
			t.Errorf("explicit config.endpoint clobbered; got %q", got)
		}
	})

	t.Run("empty column leaves config alone", func(t *testing.T) {
		hc, err := BuildHookConfig(HookConfigRow{
			ID:         "h-wh3",
			ConfigJSON: `{}`,
		})
		if err != nil {
			t.Fatalf("BuildHookConfig: %v", err)
		}
		if _, exists := hc.Config["endpoint"]; exists {
			t.Errorf("no column → no endpoint key expected")
		}
	})
}
