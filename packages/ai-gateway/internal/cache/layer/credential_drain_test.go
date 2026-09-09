package cachelayer

import (
	"context"
	"errors"
	"testing"

	"github.com/pashagolub/pgxmock/v4"
)

// The cache layer answers "which credential may serve this provider?" twice —
// once as a single-credential index (GetCredentialForProvider) and once as a
// list (ListCredentialsForProvider) — and the two must agree. A list that
// excludes SelectionWeight 0 where the index does not leaves the drained
// credential serving traffic: the target resolver falls back from the list to
// the index when the list comes back empty, which is exactly what draining
// every credential to weight 0 produces, while the console shows weight 0.
//
// This is the layer PRODUCTION runs — wiring builds the credential manager over
// cachelayer, never over the store directly — so a store-level fix alone leaves
// the defect live. These arms hold this pair.

// credRowWithWeight builds a credential row with an explicit selectionWeight.
func credRowWithWeight(id, providerID string, weight int) []any {
	row := makeCredRow(id, providerID, true, "active")
	row[9] = weight // selectionWeight — see credentialCols
	return row
}

func TestGetCredentialForProvider_SkipsDrainedCredential(t *testing.T) {
	mock, l := newMockLayer(t, Config{})
	mock.ExpectQuery(`FROM "Credential"`).
		WillReturnRows(pgxmock.NewRows(credentialCols).
			AddRow(credRowWithWeight("c-drained", "p1", 0)...))
	if _, err := l.loadCredentials(context.Background()); err != nil {
		t.Fatalf("loadCredentials: %v", err)
	}

	got, err := l.GetCredentialForProvider(context.Background(), "p1")
	if err == nil {
		t.Fatalf("a credential drained to weight 0 was resolved (%q) — the drain does nothing", got.ID)
	}
	if !errors.Is(err, errNotFound) {
		t.Errorf("err = %v, want the not-found sentinel", err)
	}
}

// The drained row must not shadow a usable one either: it is skipped, not
// treated as "the first for this provider".
func TestGetCredentialForProvider_DrainedDoesNotShadowUsable(t *testing.T) {
	mock, l := newMockLayer(t, Config{})
	// Newest-first order, as the ORDER BY produces: the drained row is first.
	mock.ExpectQuery(`FROM "Credential"`).
		WillReturnRows(pgxmock.NewRows(credentialCols).
			AddRow(credRowWithWeight("c-drained", "p1", 0)...).
			AddRow(credRowWithWeight("c-live", "p1", 100)...))
	if _, err := l.loadCredentials(context.Background()); err != nil {
		t.Fatalf("loadCredentials: %v", err)
	}

	got, err := l.GetCredentialForProvider(context.Background(), "p1")
	if err != nil {
		t.Fatalf("a usable credential exists but none resolved: %v", err)
	}
	if got.ID != "c-live" {
		t.Errorf("resolved %q, want c-live — the drained row took the slot", got.ID)
	}
}

// The sibling: an ordinary credential still resolves, so "refuse everything"
// would not satisfy the arms above.
func TestGetCredentialForProvider_UsableStillResolves(t *testing.T) {
	mock, l := newMockLayer(t, Config{})
	mock.ExpectQuery(`FROM "Credential"`).
		WillReturnRows(pgxmock.NewRows(credentialCols).
			AddRow(credRowWithWeight("c-live", "p1", 100)...))
	if _, err := l.loadCredentials(context.Background()); err != nil {
		t.Fatalf("loadCredentials: %v", err)
	}
	got, err := l.GetCredentialForProvider(context.Background(), "p1")
	if err != nil || got.ID != "c-live" {
		t.Fatalf("got %+v err %v, want c-live", got, err)
	}
}

// The two lookups must keep agreeing. Asserting them against one snapshot is
// the point: they diverged once, silently, and the divergence WAS the defect.
func TestCredentialUsability_IndexAndListAgree(t *testing.T) {
	mock, l := newMockLayer(t, Config{})
	mock.ExpectQuery(`FROM "Credential"`).
		WillReturnRows(pgxmock.NewRows(credentialCols).
			AddRow(credRowWithWeight("c-drained", "p1", 0)...))
	if _, err := l.loadCredentials(context.Background()); err != nil {
		t.Fatalf("loadCredentials: %v", err)
	}

	list, err := l.ListCredentialsForProvider(context.Background(), "p1")
	if err != nil {
		t.Fatalf("ListCredentialsForProvider: %v", err)
	}
	_, indexErr := l.GetCredentialForProvider(context.Background(), "p1")

	listHasIt := len(list) > 0
	indexHasIt := indexErr == nil
	if listHasIt != indexHasIt {
		t.Errorf("the list says usable=%v and the index says usable=%v for the same snapshot — the resolver falls back from one to the other, so a disagreement is a drained credential serving traffic",
			listHasIt, indexHasIt)
	}
}
