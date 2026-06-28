package catbagent

import (
	"context"
	"errors"
	"github.com/goccy/go-json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
)

// TestStreamingCompliance_Load_NoRowReturnsEmptyZeroVer covers the
// pgx.ErrNoRows branch — a fresh DB without the
// streaming_compliance.config row returns an empty wire object with
// version=0 so the agent applies DefaultPolicy. Critical: a regression
// here would silently bake "passthrough mode, no capture" into every
// agent boot even after admins configured otherwise.
func TestStreamingCompliance_Load_NoRowReturnsEmptyZeroVer(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectQuery(`FROM system_metadata`).
		WithArgs(streamingComplianceConfigKey).
		WillReturnError(pgx.ErrNoRows)

	l := NewAgentStreamingComplianceLoader(mock, nil)
	state, ver, err := l.Load(context.Background(), "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if ver != 0 {
		t.Errorf("no-row must report version=0; got %d", ver)
	}
	raw, _ := json.Marshal(state)
	if string(raw) != `{}` {
		t.Errorf("no-row state: got %s, want {}", raw)
	}
}

// TestStreamingCompliance_Load_ValidConfig covers the happy path:
// admin-written JSON in system_metadata threads through to the wire
// shape unchanged so the agent's DecodeGlobalPolicy sees the right
// override fields.
func TestStreamingCompliance_Load_ValidConfig(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	updated := time.Date(2026, 5, 16, 10, 0, 0, 0, time.UTC)
	payload := []byte(`{"default_mode":"chunked_async","chunk_bytes":4096,"hook_timeout_ms":2000,"capture_request_body":true}`)
	mock.ExpectQuery(`FROM system_metadata`).
		WithArgs(streamingComplianceConfigKey).
		WillReturnRows(pgxmock.NewRows([]string{"value", "updated_at"}).AddRow(payload, updated))

	l := NewAgentStreamingComplianceLoader(mock, nil)
	state, ver, err := l.Load(context.Background(), "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if ver != updated.Unix() {
		t.Errorf("version: got %d, want %d", ver, updated.Unix())
	}
	raw, _ := json.Marshal(state)
	var got agentStreamingComplianceWire
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.DefaultMode != "chunked_async" || got.ChunkBytes != 4096 || got.HookTimeoutMs != 2000 {
		t.Errorf("wire: %+v", got)
	}
	if got.CaptureRequestBody == nil || !*got.CaptureRequestBody {
		t.Errorf("CaptureRequestBody: %+v", got.CaptureRequestBody)
	}
}

// TestStreamingCompliance_Load_EmptyValueReturnsEmptyWireWithVer
// covers the `len(raw) == 0` branch — a row exists but value is
// empty bytes. Must return empty wire (agent applies defaults) but
// retain the updated_at version so the apply-stamp survives.
func TestStreamingCompliance_Load_EmptyValueReturnsEmptyWireWithVer(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	updated := time.Date(2026, 5, 16, 10, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`FROM system_metadata`).
		WithArgs(streamingComplianceConfigKey).
		WillReturnRows(pgxmock.NewRows([]string{"value", "updated_at"}).AddRow([]byte(nil), updated))

	l := NewAgentStreamingComplianceLoader(mock, nil)
	state, ver, err := l.Load(context.Background(), "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if ver != updated.Unix() {
		t.Errorf("empty value should still surface version: got %d", ver)
	}
	if state.(agentStreamingComplianceWire).DefaultMode != "" {
		t.Errorf("empty value should yield empty wire; got %+v", state)
	}
}

// TestStreamingCompliance_Load_MalformedJSONDegradesToEmpty covers
// the json.Unmarshal error branch — a corrupted row must NOT crash
// or surface the parse err to the agent; it logs (when logger set)
// and falls back to empty wire so DefaultPolicy kicks in.
func TestStreamingCompliance_Load_MalformedJSONDegradesToEmpty(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	updated := time.Date(2026, 5, 16, 10, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`FROM system_metadata`).
		WithArgs(streamingComplianceConfigKey).
		WillReturnRows(pgxmock.NewRows([]string{"value", "updated_at"}).AddRow([]byte("not json"), updated))

	l := NewAgentStreamingComplianceLoader(mock, nil)
	state, ver, err := l.Load(context.Background(), "")
	if err != nil {
		t.Fatalf("malformed JSON must NOT propagate; got: %v", err)
	}
	if ver != updated.Unix() {
		t.Errorf("malformed-JSON path must still surface version; got %d", ver)
	}
	if state.(agentStreamingComplianceWire).DefaultMode != "" {
		t.Errorf("malformed JSON should yield empty wire; got %+v", state)
	}
}

// TestStreamingCompliance_Load_GenericErrorWraps covers any other
// query error — must surface "catb: query system_metadata[…]:" so
// the Hub log distinguishes this loader from siblings.
func TestStreamingCompliance_Load_GenericErrorWraps(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	want := errors.New("connection refused")
	mock.ExpectQuery(`FROM system_metadata`).
		WithArgs(streamingComplianceConfigKey).
		WillReturnError(want)

	l := NewAgentStreamingComplianceLoader(mock, nil)
	_, _, err := l.Load(context.Background(), "")
	if err == nil {
		t.Fatal("expected wrapped error")
	}
	if !errors.Is(err, want) {
		t.Errorf("error must wrap via %%w; got: %v", err)
	}
}
