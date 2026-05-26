package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

// vkColumns matches the SELECT in vkSelectSQL.
var vkColumns = []string{
	"id", "name", "keyHash", "keyPrefix",
	"projectId", "organization_id",
	"sourceApp", "enabled", "expiresAt",
	"rateLimitRpm", "compareEndpointRateLimitRpm",
	"allowedModels", "ownerId",
	"vkType", "vkStatus",
	"organization_name", "p_name", "u_displayName",
	"organization_timezone",
}

func makeVKRow(id string, allowedModelsJSON []byte) []any {
	exp := time.Now().Add(time.Hour)
	keyHash := "hash-" + id
	keyPrefix := "vk_xx"
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
		id, "vk-name", &keyHash, &keyPrefix,
		&projID, &orgID,
		&src, true, &exp,
		&rpm, &cre,
		allowedModelsJSON, &owner,
		&vkType, &vkStatus,
		&orgName, &projName, &userDisplay,
		&orgTz,
	}
}

func TestGetVirtualKeyByHash(t *testing.T) {
	t.Run("happy with allowedModels", func(t *testing.T) {
		mock, db := newMockDB(t)
		mock.ExpectQuery(`FROM "VirtualKey" vk`).
			WithArgs("hash-1").
			WillReturnRows(pgxmock.NewRows(vkColumns).
				AddRow(makeVKRow("v1", []byte(`[{"providerId":"openai","modelId":"gpt-4o"}]`))...))
		got, err := db.GetVirtualKeyByHash(context.Background(), "hash-1")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if got.ID != "v1" {
			t.Errorf("id mismatch: %+v", got)
		}
		if len(got.AllowedModels) != 1 || got.AllowedModels[0].ModelID != "gpt-4o" {
			t.Errorf("allowed models: %+v", got.AllowedModels)
		}
	})

	t.Run("empty allowedModels", func(t *testing.T) {
		mock, db := newMockDB(t)
		mock.ExpectQuery(`FROM "VirtualKey"`).
			WithArgs("hash-2").
			WillReturnRows(pgxmock.NewRows(vkColumns).
				AddRow(makeVKRow("v2", nil)...))
		got, err := db.GetVirtualKeyByHash(context.Background(), "hash-2")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if got.AllowedModels != nil {
			t.Errorf("allowed models should be nil; got %+v", got.AllowedModels)
		}
	})

	t.Run("corrupt allowedModels JSON propagates", func(t *testing.T) {
		mock, db := newMockDB(t)
		mock.ExpectQuery(`FROM "VirtualKey"`).
			WithArgs("hash-4").
			WillReturnRows(pgxmock.NewRows(vkColumns).
				AddRow(makeVKRow("v4", []byte(`not-json`))...))
		_, err := db.GetVirtualKeyByHash(context.Background(), "hash-4")
		if err == nil || !strings.Contains(err.Error(), "parse allowedModels for vk v4") {
			t.Errorf("expected parse err; got: %v", err)
		}
	})

	t.Run("scan err wraps", func(t *testing.T) {
		mock, db := newMockDB(t)
		want := errors.New("planner err")
		mock.ExpectQuery(`FROM "VirtualKey"`).
			WithArgs("x").
			WillReturnError(want)
		_, err := db.GetVirtualKeyByHash(context.Background(), "x")
		if !errors.Is(err, want) {
			t.Errorf("must wrap; got: %v", err)
		}
		if !strings.Contains(err.Error(), "scan virtual key") {
			t.Errorf("missing prefix: %v", err)
		}
	})
}
