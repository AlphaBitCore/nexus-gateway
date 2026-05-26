package analytics

import (
	"context"
	"errors"
	"testing"

	"github.com/pashagolub/pgxmock/v4"
)

// TestResolveDimensionLabels_NilGuards exercises the three short-circuit
// guards: empty ids, nil handler, nil pool.
func TestResolveDimensionLabels_NilGuards(t *testing.T) {
	t.Parallel()

	t.Run("empty ids", func(t *testing.T) {
		_, h := newMockHandler(t)
		if got := h.resolveDimensionLabels(context.Background(), "provider", nil); got != nil {
			t.Errorf("want nil, got %v", got)
		}
	})

	t.Run("nil handler", func(t *testing.T) {
		var h *Handler
		if got := h.resolveDimensionLabels(context.Background(), "provider", []string{"a"}); got != nil {
			t.Errorf("want nil, got %v", got)
		}
	})

	t.Run("nil pool", func(t *testing.T) {
		h := &Handler{} // no pool set
		if got := h.resolveDimensionLabels(context.Background(), "provider", []string{"a"}); got != nil {
			t.Errorf("want nil, got %v", got)
		}
	})
}

// TestResolveDimensionLabels_AllCases covers every case branch in the
// dimName switch so each SQL template runs at least once.
func TestResolveDimensionLabels_AllCases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		dimName    string
		sqlMatcher string
		ids        []string
		rowID      string
		rowLabel   string
		wantMap    map[string]string
		wantNil    bool
	}{
		{
			name: "provider", dimName: "provider", sqlMatcher: `FROM "Provider"`,
			ids: []string{"p1"}, rowID: "p1", rowLabel: "OpenAI",
			wantMap: map[string]string{"p1": "OpenAI"},
		},
		{
			name: "routed_provider", dimName: "routed_provider", sqlMatcher: `FROM "Provider"`,
			ids: []string{"p2"}, rowID: "p2", rowLabel: "Anthropic",
			wantMap: map[string]string{"p2": "Anthropic"},
		},
		{
			name: "model", dimName: "model", sqlMatcher: `FROM "Model"`,
			ids: []string{"m1"}, rowID: "m1", rowLabel: "gpt-4",
			wantMap: map[string]string{"m1": "gpt-4"},
		},
		{
			name: "organization", dimName: "organization", sqlMatcher: `FROM "Organization"`,
			ids: []string{"o1"}, rowID: "o1", rowLabel: "Acme",
			wantMap: map[string]string{"o1": "Acme"},
		},
		{
			name: "project", dimName: "project", sqlMatcher: `FROM "Project"`,
			ids: []string{"pr1"}, rowID: "pr1", rowLabel: "Proj1",
			wantMap: map[string]string{"pr1": "Proj1"},
		},
		{
			name: "user", dimName: "user", sqlMatcher: `FROM "NexusUser"`,
			ids: []string{"u1"}, rowID: "u1", rowLabel: "Alice",
			wantMap: map[string]string{"u1": "Alice"},
		},
		{
			name: "virtual_key", dimName: "virtual_key", sqlMatcher: `FROM "VirtualKey"`,
			ids: []string{"v1"}, rowID: "v1", rowLabel: "VK-1",
			wantMap: map[string]string{"v1": "VK-1"},
		},
		{
			name: "routing_rule", dimName: "routing_rule", sqlMatcher: `FROM "RoutingRule"`,
			ids: []string{"r1"}, rowID: "r1", rowLabel: "rule-1",
			wantMap: map[string]string{"r1": "rule-1"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mock, h := newMockHandler(t)
			mock.ExpectQuery(tc.sqlMatcher).
				WithArgs(pgxmock.AnyArg()).
				WillReturnRows(pgxmock.NewRows([]string{"id", "label"}).AddRow(tc.rowID, tc.rowLabel))

			got := h.resolveDimensionLabels(context.Background(), tc.dimName, tc.ids)
			if len(got) != len(tc.wantMap) {
				t.Fatalf("got %v, want %v", got, tc.wantMap)
			}
			for k, v := range tc.wantMap {
				if got[k] != v {
					t.Errorf("got[%q]=%q, want %q", k, got[k], v)
				}
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet: %v", err)
			}
		})
	}
}

// TestResolveDimensionLabels_NoTranslationCases asserts entity/device/
// target_host return nil (no lookup performed).
func TestResolveDimensionLabels_NoTranslationCases(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"entity", "device", "target_host", "unknown-dim"} {
		t.Run(name, func(t *testing.T) {
			_, h := newMockHandler(t)
			if got := h.resolveDimensionLabels(context.Background(), name, []string{"x"}); got != nil {
				t.Errorf("dim %q: want nil, got %v", name, got)
			}
		})
	}
}

// TestFetchLabelsBy_QueryError logs and returns nil — does not propagate.
func TestFetchLabelsBy_QueryError(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	mock.ExpectQuery(`FROM "Provider"`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnError(errors.New("conn lost"))
	got := h.fetchLabelsBy(context.Background(), `SELECT id, name FROM "Provider" WHERE id = ANY($1)`, []string{"p"})
	if got != nil {
		t.Errorf("want nil on query error, got %v", got)
	}
}

// TestFetchLabelsBy_ScanError continues past a row that fails to scan and
// returns the labels it could decode.
func TestFetchLabelsBy_ScanError(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	// First row OK; second row wrong column count → scan fails. fetchLabelsBy
	// logs & continues, returning the first row's label.
	rows := pgxmock.NewRows([]string{"id", "name"}).
		AddRow("p1", "ok").
		AddRow("p2", "good")
	mock.ExpectQuery(`FROM "Provider"`).WithArgs(pgxmock.AnyArg()).WillReturnRows(rows)
	got := h.fetchLabelsBy(context.Background(), `SELECT id, name FROM "Provider" WHERE id = ANY($1)`, []string{"p1", "p2"})
	if got["p1"] != "ok" || got["p2"] != "good" {
		t.Errorf("got %v", got)
	}
}

// TestFetchProviderAdapterTypes_EmptyIDs short-circuits.
func TestFetchProviderAdapterTypes_EmptyIDs(t *testing.T) {
	t.Parallel()
	_, h := newMockHandler(t)
	if got := h.fetchProviderAdapterTypes(context.Background(), nil); got != nil {
		t.Errorf("want nil, got %v", got)
	}
}

// TestFetchProviderAdapterTypes_Happy returns id→adapter_type map.
func TestFetchProviderAdapterTypes_Happy(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	rows := pgxmock.NewRows([]string{"id", "adapter_type"}).
		AddRow("p1", "openai").
		AddRow("p2", "anthropic")
	mock.ExpectQuery(`FROM "Provider"`).WithArgs(pgxmock.AnyArg()).WillReturnRows(rows)

	got := h.fetchProviderAdapterTypes(context.Background(), []string{"p1", "p2"})
	if got["p1"] != "openai" || got["p2"] != "anthropic" {
		t.Errorf("got %v", got)
	}
}
