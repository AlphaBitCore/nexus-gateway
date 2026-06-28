package catbagent

import (
	"context"
	"github.com/goccy/go-json"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

// payloadCaptureCols mirrors the column order in AgentPayloadCaptureLoader.Load.
var payloadCaptureCols = []string{"value", "updated_at"}

func TestAgentPayloadCaptureLoader_Load_PresentRow(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	updated := time.Date(2026, 4, 22, 3, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`FROM system_metadata WHERE key = \$1`).
		WithArgs(payloadCaptureConfigKey).
		WillReturnRows(pgxmock.NewRows(payloadCaptureCols).AddRow(
			[]byte(`{"storeRequestBody":true,"storeResponseBody":false,"maxInlineBodyBytes":8192,"maxRequestBytes":15728640,"maxResponseBytes":20971520}`),
			updated,
		))

	l := NewAgentPayloadCaptureLoader(mock, nil)
	state, ver, err := l.Load(context.Background(), "thing-x")
	if err != nil {
		t.Fatalf("Load err=%v", err)
	}
	if ver != updated.Unix() {
		t.Errorf("version = %d want %d", ver, updated.Unix())
	}
	raw, _ := json.Marshal(state)
	want := `{"storeRequestBody":true,"storeResponseBody":false,"maxInlineBodyBytes":8192,"maxRequestBytes":15728640,"maxResponseBytes":20971520}`
	if string(raw) != want {
		t.Errorf("state mismatch:\n got %s\nwant %s", raw, want)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAgentPayloadCaptureLoader_Load_MissingRow(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectQuery(`FROM system_metadata WHERE key = \$1`).
		WithArgs(payloadCaptureConfigKey).
		WillReturnRows(pgxmock.NewRows(payloadCaptureCols))

	l := NewAgentPayloadCaptureLoader(mock, nil)
	state, ver, err := l.Load(context.Background(), "")
	if err != nil {
		t.Fatalf("Load err=%v", err)
	}
	if ver != 0 {
		t.Errorf("missing row should report version=0, got %d", ver)
	}
	raw, _ := json.Marshal(state)
	want := `{"storeRequestBody":false,"storeResponseBody":false,"maxInlineBodyBytes":262144,"maxRequestBytes":10485760,"maxResponseBytes":10485760}`
	if string(raw) != want {
		t.Errorf("state mismatch:\n got %s\nwant %s", raw, want)
	}
}

func TestAgentPayloadCaptureLoader_Load_MalformedJSONFallsBackToDefaults(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	updated := time.Date(2026, 4, 22, 3, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`FROM system_metadata WHERE key = \$1`).
		WithArgs(payloadCaptureConfigKey).
		WillReturnRows(pgxmock.NewRows(payloadCaptureCols).AddRow(
			[]byte(`{not-json`),
			updated,
		))

	l := NewAgentPayloadCaptureLoader(mock, nil)
	state, ver, err := l.Load(context.Background(), "")
	if err != nil {
		t.Fatalf("malformed JSON should degrade to defaults, not error; got %v", err)
	}
	if ver != updated.Unix() {
		t.Errorf("version should still reflect updated_at on malformed; got %d want %d",
			ver, updated.Unix())
	}
	raw, _ := json.Marshal(state)
	want := `{"storeRequestBody":false,"storeResponseBody":false,"maxInlineBodyBytes":262144,"maxRequestBytes":10485760,"maxResponseBytes":10485760}`
	if string(raw) != want {
		t.Errorf("malformed row should yield defaults:\n got %s\nwant %s", raw, want)
	}
}

func TestAgentPayloadCaptureLoader_Load_ZeroCapsCoerce(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	updated := time.Date(2026, 4, 22, 3, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`FROM system_metadata WHERE key = \$1`).
		WithArgs(payloadCaptureConfigKey).
		WillReturnRows(pgxmock.NewRows(payloadCaptureCols).AddRow(
			[]byte(`{"storeRequestBody":true,"storeResponseBody":true,"maxInlineBodyBytes":0,"maxRequestBytes":0,"maxResponseBytes":0}`),
			updated,
		))

	l := NewAgentPayloadCaptureLoader(mock, nil)
	state, _, err := l.Load(context.Background(), "")
	if err != nil {
		t.Fatalf("Load err=%v", err)
	}
	raw, _ := json.Marshal(state)
	want := `{"storeRequestBody":true,"storeResponseBody":true,"maxInlineBodyBytes":262144,"maxRequestBytes":10485760,"maxResponseBytes":10485760}`
	if string(raw) != want {
		t.Errorf("zero byte caps should coerce to defaults:\n got %s\nwant %s", raw, want)
	}
}

// TestAgentPayloadCaptureLoader_Load_PartialRowFillsNetworkCapDefaults
// pins the behaviour for a row that omits the network caps. The loader
// must surface the missing maxRequestBytes / maxResponseBytes as the
// package defaults so an incomplete row never collapses the agent's
// read cap to zero (which would 413 every request).
func TestAgentPayloadCaptureLoader_Load_PartialRowFillsNetworkCapDefaults(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	updated := time.Date(2026, 4, 22, 3, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`FROM system_metadata WHERE key = \$1`).
		WithArgs(payloadCaptureConfigKey).
		WillReturnRows(pgxmock.NewRows(payloadCaptureCols).AddRow(
			[]byte(`{"storeRequestBody":true,"storeResponseBody":true,"maxInlineBodyBytes":131072}`),
			updated,
		))

	l := NewAgentPayloadCaptureLoader(mock, nil)
	state, _, err := l.Load(context.Background(), "")
	if err != nil {
		t.Fatalf("Load err=%v", err)
	}
	raw, _ := json.Marshal(state)
	want := `{"storeRequestBody":true,"storeResponseBody":true,"maxInlineBodyBytes":131072,"maxRequestBytes":10485760,"maxResponseBytes":10485760}`
	if string(raw) != want {
		t.Errorf("partial row should fill network caps with defaults:\n got %s\nwant %s", raw, want)
	}
}

func TestAgentPayloadCaptureLoader_Load_ThingIDIgnored(t *testing.T) {
	// The loader accepts thingID for interface parity with CatBLoader but
	// does not filter on it. Two different thing IDs must see the same SQL.
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	updated := time.Date(2026, 4, 22, 0, 0, 0, 0, time.UTC)
	for range 2 {
		mock.ExpectQuery(`FROM system_metadata WHERE key = \$1`).
			WithArgs(payloadCaptureConfigKey).
			WillReturnRows(pgxmock.NewRows(payloadCaptureCols).AddRow(
				[]byte(`{"storeRequestBody":false,"storeResponseBody":true,"maxInlineBodyBytes":4096,"maxRequestBytes":15728640,"maxResponseBytes":20971520}`),
				updated,
			))
	}

	l := NewAgentPayloadCaptureLoader(mock, nil)
	s1, _, err1 := l.Load(context.Background(), "thing-a")
	s2, _, err2 := l.Load(context.Background(), "thing-b")
	if err1 != nil || err2 != nil {
		t.Fatalf("Load errs: %v %v", err1, err2)
	}
	r1, _ := json.Marshal(s1)
	r2, _ := json.Marshal(s2)
	if string(r1) != string(r2) {
		t.Errorf("thingID must be ignored; got %s vs %s", r1, r2)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
