package catbagent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

var interceptionDomainCols = []string{
	"id", "name", "host_pattern", "host_match_type", "adapter_id",
	"adapter_config", "enabled", "priority", "default_path_action",
	"on_adapter_error", "network_zone", "updated_at",
	// Per-host StreamingPolicy + payload-capture overrides.
	"streaming_mode", "streaming_chunk_bytes", "streaming_hook_timeout_ms",
	"streaming_max_buffer_bytes", "streaming_fail_behavior",
	"capture_request_body", "capture_response_body",
	"raw_body_spill_enabled",
}

var interceptionPathCols = []string{
	"id", "domain_id", "path_pattern", "match_type", "action",
	"priority", "enabled", "updated_at",
}

func TestAgentInterceptionDomainsLoader_Empty(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectQuery(`FROM interception_domain`).
		WillReturnRows(pgxmock.NewRows(interceptionDomainCols))

	l := NewAgentInterceptionDomainsLoader(mock, nil)
	state, ver, err := l.Load(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if ver != 0 {
		t.Errorf("version = %d want 0", ver)
	}
	raw, _ := json.Marshal(state)
	if string(raw) != `{"interceptionDomains":[]}` {
		t.Errorf("empty state = %s", raw)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAgentInterceptionDomainsLoader_SingleDomainNoPaths(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	updated := time.Date(2026, 4, 22, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`FROM interception_domain`).
		WillReturnRows(pgxmock.NewRows(interceptionDomainCols).AddRow(
			"dom-1", "openai", "api.openai.com", "EXACT", "openai-compat",
			[]byte(nil), true, 100, "PROCESS", "FAIL_OPEN", "PUBLIC", updated,
			nil, nil, nil, nil, nil, nil, nil, nil,
		))

	mock.ExpectQuery(`FROM interception_path`).
		WillReturnRows(pgxmock.NewRows(interceptionPathCols))

	l := NewAgentInterceptionDomainsLoader(mock, nil)
	state, ver, err := l.Load(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if ver != updated.Unix() {
		t.Errorf("version = %d want %d", ver, updated.Unix())
	}
	raw, _ := json.Marshal(state)
	want := `{"interceptionDomains":[{"id":"dom-1","name":"openai","hostPattern":"api.openai.com","hostMatchType":"EXACT","adapterId":"openai-compat","enabled":true,"priority":100,"defaultPathAction":"PROCESS","onAdapterError":"FAIL_OPEN","networkZone":"PUBLIC","paths":[]}]}`
	if string(raw) != want {
		t.Errorf("state mismatch:\n got %s\nwant %s", raw, want)
	}
}

func TestAgentInterceptionDomainsLoader_WithPaths(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	domUpdated := time.Date(2026, 4, 20, 0, 0, 0, 0, time.UTC)
	pathUpdated := time.Date(2026, 4, 22, 0, 0, 0, 0, time.UTC)

	mock.ExpectQuery(`FROM interception_domain`).
		WillReturnRows(pgxmock.NewRows(interceptionDomainCols).
			AddRow("dom-openai", "openai", "api.openai.com", "EXACT", "openai-compat",
				[]byte(`{"k":1}`), true, 100, "PROCESS", "FAIL_OPEN", "PUBLIC", domUpdated, nil, nil, nil, nil, nil, nil, nil, nil).
			AddRow("dom-anthropic", "anthropic", "api.anthropic.com", "EXACT", "openai-compat",
				[]byte(nil), true, 90, "PROCESS", "FAIL_OPEN", "PUBLIC", domUpdated, nil, nil, nil, nil, nil, nil, nil, nil))

	mock.ExpectQuery(`FROM interception_path`).
		WillReturnRows(pgxmock.NewRows(interceptionPathCols).
			AddRow("p-1", "dom-openai", []string{"/v1/chat/completions"}, "PREFIX", "PROCESS", 10, true, pathUpdated).
			AddRow("p-2", "dom-openai", []string{"/v1/embeddings"}, "PREFIX", "PROCESS", 5, true, domUpdated).
			// Path for a domain not in the set — must be skipped without error.
			AddRow("p-orphan", "dom-disabled", []string{"/v1"}, "PREFIX", "DENY", 1, true, domUpdated))

	l := NewAgentInterceptionDomainsLoader(mock, nil)
	state, ver, err := l.Load(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if ver != pathUpdated.Unix() {
		t.Errorf("version must track max(updated_at) across domain+path; got %d want %d", ver, pathUpdated.Unix())
	}

	type envelope struct {
		InterceptionDomains []struct {
			ID    string `json:"id"`
			Paths []struct {
				ID string `json:"id"`
			} `json:"paths"`
		} `json:"interceptionDomains"`
	}
	var env envelope
	raw, _ := json.Marshal(state)
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(env.InterceptionDomains) != 2 {
		t.Fatalf("expected 2 domains, got %d", len(env.InterceptionDomains))
	}
	// OpenAI domain should have both its paths; the orphan path must
	// be discarded. Anthropic should have zero paths.
	for _, d := range env.InterceptionDomains {
		switch d.ID {
		case "dom-openai":
			if len(d.Paths) != 2 {
				t.Errorf("dom-openai expected 2 paths, got %d", len(d.Paths))
			}
		case "dom-anthropic":
			if len(d.Paths) != 0 {
				t.Errorf("dom-anthropic expected 0 paths, got %d", len(d.Paths))
			}
		}
	}
}

func TestAgentInterceptionDomainsLoader_NullAdapterConfigOmitted(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectQuery(`FROM interception_domain`).
		WillReturnRows(pgxmock.NewRows(interceptionDomainCols).AddRow(
			"dom-1", "d", "host", "EXACT", "openai-compat",
			[]byte(nil), true, 1, "PROCESS", "FAIL_OPEN", "PUBLIC", time.Now().UTC(),
			nil, nil, nil, nil, nil, nil, nil, nil,
		))
	mock.ExpectQuery(`FROM interception_path`).
		WillReturnRows(pgxmock.NewRows(interceptionPathCols))

	l := NewAgentInterceptionDomainsLoader(mock, nil)
	state, _, err := l.Load(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(state)
	if containsJSONKey(raw, "adapterConfig") {
		t.Errorf("NULL adapter_config should be omitted; got %s", raw)
	}
}
