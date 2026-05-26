package cachelayer

import (
	"context"
	"errors"
	"testing"

	"github.com/pashagolub/pgxmock/v4"
)

func TestLoadProviders_QueryError(t *testing.T) {
	mock, l := newMockLayer(t, Config{})
	want := errors.New("query-boom")
	mock.ExpectQuery(`FROM "Provider"`).WillReturnError(want)
	_, err := l.loadProviders(context.Background())
	if !errors.Is(err, want) {
		t.Fatalf("must wrap; got %v", err)
	}
}

func TestLoadProviders_ScanError(t *testing.T) {
	mock, l := newMockLayer(t, Config{})
	// Provide 1 column when 9 are scanned → scan failure.
	mock.ExpectQuery(`FROM "Provider"`).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("p1"))
	_, err := l.loadProviders(context.Background())
	if err == nil {
		t.Fatal("expected scan error")
	}
}

func TestLoadModels_QueryError(t *testing.T) {
	mock, l := newMockLayer(t, Config{})
	want := errors.New("model-boom")
	mock.ExpectQuery(`FROM "Model" m`).WillReturnError(want)
	_, err := l.loadModels(context.Background())
	if !errors.Is(err, want) {
		t.Fatalf("must wrap; got %v", err)
	}
}

func TestLoadModels_ScanError(t *testing.T) {
	mock, l := newMockLayer(t, Config{})
	mock.ExpectQuery(`FROM "Model" m`).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("m1"))
	_, err := l.loadModels(context.Background())
	if err == nil {
		t.Fatal("expected scan error")
	}
}

func TestLoadCredentials_QueryError(t *testing.T) {
	mock, l := newMockLayer(t, Config{})
	want := errors.New("cred-boom")
	mock.ExpectQuery(`FROM "Credential"`).WillReturnError(want)
	_, err := l.loadCredentials(context.Background())
	if !errors.Is(err, want) {
		t.Fatalf("must wrap; got %v", err)
	}
}

func TestLoadCredentials_ScanError(t *testing.T) {
	mock, l := newMockLayer(t, Config{})
	mock.ExpectQuery(`FROM "Credential"`).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("c1"))
	_, err := l.loadCredentials(context.Background())
	if err == nil {
		t.Fatal("expected scan error")
	}
}

func TestLoadModels_NilPricesAndNullableSizes(t *testing.T) {
	mock, l := newMockLayer(t, Config{})
	// nil prices, NULL maxCtx/maxOut, "" code (excluded from byCode).
	row := makeModelRow("m-nil", "", "p1", true)
	row[11] = (*string)(nil)
	row[12] = (*string)(nil)
	row[13] = (*string)(nil)
	row[14] = (*string)(nil)
	mock.ExpectQuery(`FROM "Model" m`).
		WillReturnRows(pgxmock.NewRows(modelCols).AddRow(row...))
	out, err := l.loadModels(context.Background())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	got, ok := out["m-nil"]
	if !ok {
		t.Fatal("byID must include the row even with empty code")
	}
	if got.InputPricePM != nil || got.OutputPricePM != nil {
		t.Errorf("nil prices must remain nil; got %+v / %+v", got.InputPricePM, got.OutputPricePM)
	}
	// byCode index must NOT include the empty-code row.
	if _, err := l.GetModelByCode(context.Background(), ""); !IsNotFound(err) {
		t.Errorf("empty code must miss byCode; got %v", err)
	}
}

// TestLoadCredentials_PendingRotationOrderedAfterActive verifies the
// ORDER BY clause's pending-last invariant: when two rows exist for
// the same provider, an active row is preferred over a pending one
// (so byProvider[p] = active).
func TestLoadCredentials_PendingRotationOrderedAfterActive(t *testing.T) {
	mock, l := newMockLayer(t, Config{})
	// In real SQL the ORDER BY guarantees order; in pgxmock the mock
	// returns rows in insertion order, so simulate the expected order:
	// active first, pending second.
	active := makeCredRow("cA", "p1", true, "active")
	pending := makeCredRow("cP", "p1", true, "active")
	pending[8] = "pending_rotation"
	mock.ExpectQuery(`FROM "Credential"`).
		WillReturnRows(pgxmock.NewRows(credentialCols).
			AddRow(active...).
			AddRow(pending...))
	if _, err := l.loadCredentials(context.Background()); err != nil {
		t.Fatalf("err: %v", err)
	}
	got, err := l.GetCredentialForProvider(context.Background(), "p1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != "cA" {
		t.Errorf("active should win when listed first; got %q", got.ID)
	}
}

func TestLoadVirtualKey_DelegatesToStore(t *testing.T) {
	mock, l := newMockLayer(t, Config{})
	mock.ExpectQuery(`FROM "VirtualKey"`).
		WithArgs("h1").
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk1", "h1")...))
	got, err := l.loadVirtualKey(context.Background(), "h1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got.ID != "vk1" {
		t.Errorf("id = %q, want vk1", got.ID)
	}
}
