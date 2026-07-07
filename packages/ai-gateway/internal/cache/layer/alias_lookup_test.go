package cachelayer

import (
	"context"
	"testing"

	"github.com/pashagolub/pgxmock/v4"
)

// aliases is column index 18 in the loadModels SELECT (see makeModelRow).
const aliasesCol = 18

// TestGetModelByCodeOrAlias_ResolvesCodeAndAlias asserts the O(1) code-or-alias
// index resolves both a model's code and each of its aliases to the same model,
// while GetModelByCode stays strict (aliases miss).
func TestGetModelByCodeOrAlias_ResolvesCodeAndAlias(t *testing.T) {
	mock, l := newMockLayer(t, Config{})
	row := makeModelRow("m1", "opus", "p1", true)
	row[aliasesCol] = []string{"fast", "smart"}
	mock.ExpectQuery(`FROM "Model" m`).
		WillReturnRows(pgxmock.NewRows(modelCols).AddRow(row...))
	if _, err := l.loadModels(context.Background()); err != nil {
		t.Fatalf("loadModels: %v", err)
	}

	for _, key := range []string{"opus", "fast", "smart"} {
		m, err := l.GetModelByCodeOrAlias(context.Background(), key)
		if err != nil {
			t.Fatalf("key %q must resolve: %v", key, err)
		}
		if m.Code != "opus" || m.ID != "m1" {
			t.Errorf("key %q resolved to wrong model: %+v", key, m)
		}
	}
	// GetModelByCode must NOT resolve an alias — its narrow contract is unchanged.
	if _, err := l.GetModelByCode(context.Background(), "fast"); !IsNotFound(err) {
		t.Errorf("GetModelByCode must not resolve aliases; got %v", err)
	}
	if _, err := l.GetModelByCodeOrAlias(context.Background(), "unknown"); !IsNotFound(err) {
		t.Errorf("unknown key must be not-found; got %v", err)
	}
}

// TestGetModelByCodeOrAlias_CodeWinsOverAliasCollision pins the priority rule:
// when one model's alias collides with another model's real code, the code wins.
func TestGetModelByCodeOrAlias_CodeWinsOverAliasCollision(t *testing.T) {
	mock, l := newMockLayer(t, Config{})
	rowA := makeModelRow("mA", "shared", "p1", true) // real code "shared"
	rowB := makeModelRow("mB", "other", "p1", true)
	rowB[aliasesCol] = []string{"shared"} // alias collides with A's code
	mock.ExpectQuery(`FROM "Model" m`).
		WillReturnRows(pgxmock.NewRows(modelCols).AddRow(rowA...).AddRow(rowB...))
	if _, err := l.loadModels(context.Background()); err != nil {
		t.Fatalf("loadModels: %v", err)
	}
	m, err := l.GetModelByCodeOrAlias(context.Background(), "shared")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if m.ID != "mA" {
		t.Errorf("code must win over alias collision; got model %s", m.ID)
	}
}

// TestGetModelByCodeOrAlias_DisabledExcluded asserts a disabled model's alias
// is not routable (mirrors the enabled-only byCode filter).
func TestGetModelByCodeOrAlias_DisabledExcluded(t *testing.T) {
	mock, l := newMockLayer(t, Config{})
	row := makeModelRow("m1", "opus", "p1", false) // disabled
	row[aliasesCol] = []string{"fast"}
	mock.ExpectQuery(`FROM "Model" m`).
		WillReturnRows(pgxmock.NewRows(modelCols).AddRow(row...))
	if _, err := l.loadModels(context.Background()); err != nil {
		t.Fatalf("loadModels: %v", err)
	}
	if _, err := l.GetModelByCodeOrAlias(context.Background(), "fast"); !IsNotFound(err) {
		t.Errorf("disabled model's alias must not resolve; got %v", err)
	}
	if _, err := l.GetModelByCodeOrAlias(context.Background(), "opus"); !IsNotFound(err) {
		t.Errorf("disabled model's code must not resolve; got %v", err)
	}
}
