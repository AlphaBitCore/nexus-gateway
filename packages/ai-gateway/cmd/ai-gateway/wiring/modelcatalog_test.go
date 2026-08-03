package wiring

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
	cachelayer "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/layer"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/ingress/models"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
)

// stubVKAuth admits every request so the catalog handler's body can be
// asserted without building an HMAC keyring.
type stubVKAuth struct{}

func (stubVKAuth) Authenticate(context.Context, *http.Request) (*vkauth.VKMeta, error) {
	return &vkauth.VKMeta{ID: "vk-1"}, nil
}

// primeCatalogLayer builds a cachelayer and loads its snapshots once, so
// the Model snapshot holds exactly one enabled model ("snapshot-only-gpt").
// Every registered expectation is consumed by that load, so any FURTHER
// query the pool receives is an error the caller can detect.
func primeCatalogLayer(t *testing.T) (pgxmock.PgxPoolIface, *cachelayer.Layer) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mock.Close)
	db := store.NewWithPgxPool(mock)
	l, err := cachelayer.NewWithPool(db, mock, discardLogger(), cachelayer.Config{VKTTL: time.Minute})
	if err != nil {
		t.Fatalf("cachelayer.NewWithPool: %v", err)
	}

	mock.MatchExpectationsInOrder(false)
	mock.ExpectQuery(`FROM "Provider"`).WillReturnRows(pgxmock.NewRows(catalogProviderCols))
	mock.ExpectQuery(`FROM "Credential"`).WillReturnRows(pgxmock.NewRows(catalogCredentialCols))
	mock.ExpectQuery(`FROM "Model"`).WillReturnRows(
		pgxmock.NewRows(catalogModelCols).AddRow(catalogModelRow("snapshot-only-gpt")...))
	if err := l.Start(context.Background()); err != nil {
		t.Fatalf("cachelayer.Start: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("snapshot load did not consume its expectations: %v", err)
	}
	return mock, l
}

// TestSelectModelCatalog_PrefersSnapshotOverDB asserts the observable
// consequence of the wiring choice: with a CacheLayer present the catalog
// endpoint answers WITHOUT touching Postgres. The snapshot load already
// consumed every registered expectation, so any query the request issues
// fails and surfaces as a 500 the assertions below catch.
func TestSelectModelCatalog_PrefersSnapshotOverDB(t *testing.T) {
	mock, layer := primeCatalogLayer(t)
	db := store.NewWithPgxPool(mock)

	catalog := selectModelCatalog(RouteDeps{CacheLayer: layer, DB: db})
	if catalog == nil {
		t.Fatal("catalog must not be nil when CacheLayer is present")
	}

	h := models.ModelsHandler(catalog, stubVKAuth{}, discardLogger())
	rr := httptest.NewRecorder()
	h(rr, httptest.NewRequest(http.MethodGet, "/v1/models", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s (a 500 here means the DB was consulted)", rr.Code, rr.Body.String())
	}
	// The advertised catalog is the snapshot's content, not the table's.
	if !strings.Contains(rr.Body.String(), "snapshot-only-gpt") {
		t.Errorf("catalog did not come from the snapshot: %s", rr.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("snapshot path must issue zero further queries: %v", err)
	}
}

// TestSelectModelCatalog_FallsBackToDB covers the degraded boot where the
// cache layer never came up: the endpoint must still be served, from the
// raw table.
func TestSelectModelCatalog_FallsBackToDB(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	db := store.NewWithPgxPool(mock)

	catalog := selectModelCatalog(RouteDeps{DB: db})
	if catalog == nil {
		t.Fatal("catalog must fall back to DB when CacheLayer is absent")
	}

	mock.ExpectQuery(`FROM "Model"`).WillReturnRows(pgxmock.NewRows(catalogModelCols))
	h := models.ModelsHandler(catalog, stubVKAuth{}, discardLogger())
	rr := httptest.NewRecorder()
	h(rr, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("DB fallback must query the Model table: %v", err)
	}
}

// TestSelectModelCatalog_BothNilYieldsUntypedNil is the typed-nil-in-
// interface guard. A nil concrete pointer stored in an interface is not
// == nil, so it would slip past the handler's nil check and panic on the
// first method call. The handler must answer 500 instead.
func TestSelectModelCatalog_BothNilYieldsUntypedNil(t *testing.T) {
	catalog := selectModelCatalog(RouteDeps{})
	if catalog != nil {
		t.Fatalf("want an untyped nil interface, got %#v", catalog)
	}

	h := models.ModelsHandler(catalog, stubVKAuth{}, discardLogger())
	rr := httptest.NewRecorder()
	h(rr, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 with no catalog wired", rr.Code)
	}
}

// catalogModelRow builds a single enabled Model row whose customer-facing
// code is `code`, matching the cachelayer loadModels SELECT list.
func catalogModelRow(code string) []any {
	display := "OpenAI"
	return []any{
		"m-" + code, code, "Snapshot Model", "p1",
		"openai", "openai", &display, "https://api.openai.com",
		"gpt-4o", "chat", true,
		(*string)(nil), (*string)(nil), (*string)(nil), (*string)(nil),
		(*string)(nil), (*string)(nil), (*string)(nil),
		[]string{}, pgtype.Int4{}, pgtype.Int4{}, []string{},
		[]string{"text"}, []string{"text"}, "ga", []byte(`{}`),
	}
}

var catalogProviderCols = []string{
	"id", "name", "displayName", "adapter_type", "baseUrl",
	"pathPrefix", "apiVersion", "region", "enabled", "serves_responses_api",
}

var catalogCredentialCols = []string{
	"id", "name", "providerId", "encryptedKey", "encryptionIv", "encryptionTag",
	"encryption_key_id", "enabled", "rotationState", "selectionWeight",
	"status", "createdAt",
}

// catalogModelCols mirrors the SELECT list of store.DB.ListEnabledModels
// and cachelayer.loadModels (identical column sets).
var catalogModelCols = []string{
	"id", "code", "name", "providerId", "p_name", "p_adapter_type",
	"p_displayName", "p_baseUrl", "providerModelId", "type", "enabled",
	"inputPricePerMillion", "outputPricePerMillion",
	"cachedInputReadPricePerMillion", "cachedInputWritePricePerMillion",
	"audioInputPricePerMillion", "audioOutputPricePerMillion",
	"cachedAudioInputReadPricePerMillion",
	"features", "maxContextTokens", "maxOutputTokens", "aliases",
	"inputModalities", "outputModalities", "lifecycle", "capabilityJson",
}
