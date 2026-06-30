package cachelayer

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
)

// newMockLayer wires a fresh Layer backed by a pgxmock pool. The same
// mock satisfies both the cachelayer PgxPool seam and the *store.DB
// internal pool, so loader queries and store-routed helpers
// (GetVirtualKeyByHash, GetEnabledRoutingRules) all funnel through one
// expectation set.
func newMockLayer(t *testing.T, cfg Config) (pgxmock.PgxPoolIface, *Layer) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mock.Close)
	db := store.NewWithPgxPool(mock)
	l, err := NewWithPool(db, mock, discardLogger(), cfg)
	if err != nil {
		t.Fatalf("NewWithPool: %v", err)
	}
	return mock, l
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// providerCols / modelCols / credentialCols mirror the exact SELECT lists
// in loaders.go. Drift here means a test will silently rest on an
// undefined-row scan.
var (
	providerCols = []string{
		"id", "name", "displayName", "adapter_type", "baseUrl",
		"pathPrefix", "apiVersion", "region", "enabled", "serves_responses_api",
	}
	modelCols = []string{
		"id", "code", "name", "providerId", "p_name", "p_adapter_type",
		"p_displayName", "p_baseUrl", "providerModelId", "type", "enabled",
		"inputPricePerMillion", "outputPricePerMillion",
		"cachedInputReadPricePerMillion", "cachedInputWritePricePerMillion",
		"features", "maxContextTokens", "maxOutputTokens", "aliases",
		// Capability matrix columns:
		"inputModalities", "outputModalities", "lifecycle", "capabilityJson",
	}
	credentialCols = []string{
		"id", "name", "providerId", "encryptedKey", "encryptionIv", "encryptionTag",
		"encryption_key_id", "enabled", "rotationState", "selectionWeight",
		"status", "createdAt",
	}
	vkCols = []string{
		"id", "name", "keyHash", "keyPrefix",
		"projectId", "organization_id",
		"sourceApp", "enabled", "expiresAt",
		"rateLimitRpm", "compareEndpointRateLimitRpm",
		"allowedModels", "ownerId",
		"vkType", "vkStatus",
		"organization_name", "p_name", "u_displayName",
		"organization_timezone",
	}
)

func strPtr(s string) *string { return &s }

// makeModelRow builds a 19-column row matching the cachelayer loadModels SELECT.
func makeModelRow(id, code, providerID string, enabled bool) []any {
	display := "OpenAI"
	inP := "3.0"
	outP := "12.0"
	crP := "0.3"
	cwP := "3.75"
	return []any{
		id, code, "model-" + id, providerID,
		"openai", "openai", &display, "https://api.openai.com",
		"gpt-4o", "chat", enabled,
		&inP, &outP, &crP, &cwP,
		[]string{"vision"},
		pgtype.Int4{Int32: 128000, Valid: true},
		pgtype.Int4{Int32: 16384, Valid: true},
		[]string{},
		// Capability matrix fields (defaults match schema):
		[]string{"text"},
		[]string{"text"},
		"ga",
		// Pass an empty JSONB literal rather than NULL so pgxmock's Scan
		// projection of []byte does not collapse against the nullable
		// destination. Production reads tolerate either.
		[]byte(`{}`),
	}
}

// makeCredRow builds a 12-column row matching the cachelayer loadCredentials SELECT.
func makeCredRow(id, providerID string, enabled bool, status string) []any {
	createdAt := pgtype.Timestamptz{Time: time.Now(), Valid: true}
	return []any{
		id, "cred-" + id, providerID, "enc-key", "iv", "tag",
		"v1", enabled, "none", 100, status, createdAt,
	}
}

// makeVKRow builds a row matching vkSelectSQL.
func makeVKRow(id, keyHash string) []any {
	exp := time.Now().Add(time.Hour)
	kh := keyHash
	kp := "vk_xx"
	projID := "proj-1"
	orgID := "org-1"
	src := "app"
	rpm := 100
	cre := 60
	owner := "u-1"
	vkType := "application"
	vkStatus := "active"
	orgName := "Acme"
	projName := "Project1"
	userDisplay := "Alice"
	orgTz := "America/Los_Angeles"
	return []any{
		id, "vk-name", &kh, &kp,
		&projID, &orgID,
		&src, true, &exp,
		&rpm, &cre,
		[]byte{}, &owner,
		&vkType, &vkStatus,
		&orgName, &projName, &userDisplay,
		&orgTz,
	}
}
