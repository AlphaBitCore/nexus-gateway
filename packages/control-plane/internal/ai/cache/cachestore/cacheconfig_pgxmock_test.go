package cachestore

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/cacheconfig"
)

func newMock(t *testing.T) (*Store, pgxmock.PgxPoolIface) {
	t.Helper()
	m, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	t.Cleanup(m.Close)
	return New(m), m
}

func TestGetCacheAdapterConfig(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`FROM cache_adapter_config WHERE adapter_type = \$1`).WithArgs("openai").
		WillReturnRows(pgxmock.NewRows([]string{"config"}).AddRow([]byte(`{}`)))
	_, ok, err := s.GetCacheAdapterConfig(context.Background(), "openai")
	if err != nil || !ok {
		t.Fatalf("GetCacheAdapterConfig found: ok=%v err=%v", ok, err)
	}
	m.ExpectQuery(`cache_adapter_config`).WithArgs("missing").WillReturnError(pgx.ErrNoRows)
	if _, ok, err := s.GetCacheAdapterConfig(context.Background(), "missing"); err != nil || ok {
		t.Fatalf("missing → (zero,false,nil), got ok=%v err=%v", ok, err)
	}
	m.ExpectQuery(`cache_adapter_config`).WithArgs("e").WillReturnError(errors.New("db"))
	if _, _, err := s.GetCacheAdapterConfig(context.Background(), "e"); err == nil {
		t.Fatal("query error should surface")
	}
	m.ExpectQuery(`cache_adapter_config`).WithArgs("bad").WillReturnRows(pgxmock.NewRows([]string{"config"}).AddRow([]byte(`{bad`)))
	if _, ok, err := s.GetCacheAdapterConfig(context.Background(), "bad"); err == nil || !ok {
		t.Fatalf("corrupt JSON → (zero,true,err), got ok=%v err=%v", ok, err)
	}
}

func TestPutCacheAdapterConfig(t *testing.T) {
	s, m := newMock(t)
	m.ExpectExec(`INSERT INTO cache_adapter_config`).WithArgs("openai", pgxmock.AnyArg(), "admin").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	if err := s.PutCacheAdapterConfig(context.Background(), "openai", cacheconfig.AdapterConfig{}, "admin"); err != nil {
		t.Fatalf("PutCacheAdapterConfig: %v", err)
	}
	m.ExpectExec(`INSERT INTO cache_adapter_config`).WithArgs("openai", pgxmock.AnyArg(), "admin").WillReturnError(errors.New("boom"))
	if err := s.PutCacheAdapterConfig(context.Background(), "openai", cacheconfig.AdapterConfig{}, "admin"); err == nil {
		t.Fatal("exec error should surface")
	}
}

func TestListCacheAdapterConfigs(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`SELECT adapter_type, config FROM cache_adapter_config`).
		WillReturnRows(pgxmock.NewRows([]string{"adapter_type", "config"}).
			AddRow("openai", []byte(`{}`)).AddRow("anthropic", []byte(`{}`)))
	out, err := s.ListCacheAdapterConfigs(context.Background())
	if err != nil || len(out) != 2 {
		t.Fatalf("ListCacheAdapterConfigs: %v %v", out, err)
	}
	m.ExpectQuery(`cache_adapter_config`).WillReturnError(errors.New("boom"))
	if _, err := s.ListCacheAdapterConfigs(context.Background()); err == nil {
		t.Fatal("query error should surface")
	}
	// scan error: only one column returned
	s2, m2 := newMock(t)
	m2.ExpectQuery(`cache_adapter_config`).WillReturnRows(pgxmock.NewRows([]string{"adapter_type"}).AddRow("openai"))
	if _, err := s2.ListCacheAdapterConfigs(context.Background()); err == nil {
		t.Fatal("scan error should surface")
	}
	// unmarshal error
	s3, m3 := newMock(t)
	m3.ExpectQuery(`cache_adapter_config`).WillReturnRows(pgxmock.NewRows([]string{"adapter_type", "config"}).AddRow("openai", []byte(`{bad`)))
	if _, err := s3.ListCacheAdapterConfigs(context.Background()); err == nil {
		t.Fatal("unmarshal error should surface")
	}
}

func TestGetCacheProviderConfig(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`FROM cache_provider_config WHERE provider_id = \$1`).WithArgs("p1").
		WillReturnRows(pgxmock.NewRows([]string{"config"}).AddRow([]byte(`{}`)))
	if _, ok, err := s.GetCacheProviderConfig(context.Background(), "p1"); err != nil || !ok {
		t.Fatalf("found: ok=%v err=%v", ok, err)
	}
	m.ExpectQuery(`cache_provider_config`).WithArgs("missing").WillReturnError(pgx.ErrNoRows)
	if _, ok, err := s.GetCacheProviderConfig(context.Background(), "missing"); err != nil || ok {
		t.Fatalf("missing → (zero,false,nil), got ok=%v err=%v", ok, err)
	}
	m.ExpectQuery(`cache_provider_config`).WithArgs("e").WillReturnError(errors.New("db"))
	if _, _, err := s.GetCacheProviderConfig(context.Background(), "e"); err == nil {
		t.Fatal("query error should surface")
	}
	m.ExpectQuery(`cache_provider_config`).WithArgs("bad").WillReturnRows(pgxmock.NewRows([]string{"config"}).AddRow([]byte(`{bad`)))
	if _, ok, err := s.GetCacheProviderConfig(context.Background(), "bad"); err == nil || !ok {
		t.Fatalf("corrupt JSON → (zero,true,err), got ok=%v err=%v", ok, err)
	}
}

func TestPutAndDeleteCacheProviderConfig(t *testing.T) {
	s, m := newMock(t)
	m.ExpectExec(`INSERT INTO cache_provider_config`).WithArgs("p1", pgxmock.AnyArg(), "admin").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	if err := s.PutCacheProviderConfig(context.Background(), "p1", cacheconfig.ProviderConfig{}, "admin"); err != nil {
		t.Fatalf("PutCacheProviderConfig: %v", err)
	}
	m.ExpectExec(`INSERT INTO cache_provider_config`).WithArgs("p1", pgxmock.AnyArg(), "admin").WillReturnError(errors.New("boom"))
	if err := s.PutCacheProviderConfig(context.Background(), "p1", cacheconfig.ProviderConfig{}, "admin"); err == nil {
		t.Fatal("put exec error should surface")
	}
	m.ExpectExec(`DELETE FROM cache_provider_config WHERE provider_id = \$1`).WithArgs("p1").
		WillReturnResult(pgxmock.NewResult("DELETE", 1))
	if err := s.DeleteCacheProviderConfig(context.Background(), "p1"); err != nil {
		t.Fatalf("DeleteCacheProviderConfig: %v", err)
	}
	m.ExpectExec(`DELETE FROM cache_provider_config`).WithArgs("p1").WillReturnError(errors.New("boom"))
	if err := s.DeleteCacheProviderConfig(context.Background(), "p1"); err == nil {
		t.Fatal("delete exec error should surface")
	}
}

func TestListCacheProviderConfigs(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`SELECT provider_id, config FROM cache_provider_config`).
		WillReturnRows(pgxmock.NewRows([]string{"provider_id", "config"}).AddRow("p1", []byte(`{}`)))
	out, err := s.ListCacheProviderConfigs(context.Background())
	if err != nil || len(out) != 1 {
		t.Fatalf("ListCacheProviderConfigs: %v %v", out, err)
	}
	m.ExpectQuery(`cache_provider_config`).WillReturnError(errors.New("boom"))
	if _, err := s.ListCacheProviderConfigs(context.Background()); err == nil {
		t.Fatal("query error should surface")
	}
	s2, m2 := newMock(t)
	m2.ExpectQuery(`cache_provider_config`).WillReturnRows(pgxmock.NewRows([]string{"provider_id"}).AddRow("p1"))
	if _, err := s2.ListCacheProviderConfigs(context.Background()); err == nil {
		t.Fatal("scan error should surface")
	}
	s3, m3 := newMock(t)
	m3.ExpectQuery(`cache_provider_config`).WillReturnRows(pgxmock.NewRows([]string{"provider_id", "config"}).AddRow("p1", []byte(`{bad`)))
	if _, err := s3.ListCacheProviderConfigs(context.Background()); err == nil {
		t.Fatal("unmarshal error should surface")
	}
}

// TestAssembleCacheConfigBlob asserts the two surviving tiers are read and
// combined into the shadow blob, and that an error from either tier aborts the
// assembly. Tier 1 is retired: the mock declares NO cache_global_config
// expectation, so if the assembler still queried the singleton the mock would
// fail on an unexpected query — that is the regression guard for the orphaned
// table being read again.
func TestAssembleCacheConfigBlob(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`SELECT adapter_type, config FROM cache_adapter_config`).
		WillReturnRows(pgxmock.NewRows([]string{"adapter_type", "config"}).
			AddRow("anthropic", []byte(`{"marker_inject_enabled":true}`)))
	m.ExpectQuery(`SELECT provider_id, config FROM cache_provider_config`).
		WillReturnRows(pgxmock.NewRows([]string{"provider_id", "config"}).
			AddRow("p1", []byte(`{"ttl_seconds":7200}`)))

	blob, err := s.AssembleCacheConfigBlob(context.Background())
	if err != nil {
		t.Fatalf("AssembleCacheConfigBlob: %v", err)
	}
	ac, ok := blob.Adapters["anthropic"]
	if !ok || ac.MarkerInjectEnabled == nil || !*ac.MarkerInjectEnabled {
		t.Fatalf("Tier-2 anthropic marker_inject_enabled not assembled: %+v", blob.Adapters)
	}
	pc, ok := blob.Providers["p1"]
	if !ok || pc.TTLSeconds == nil || *pc.TTLSeconds != 7200 {
		t.Fatalf("Tier-3 p1 ttl_seconds not assembled: %+v", blob.Providers)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet/unexpected queries (a cache_global_config read would show here): %v", err)
	}

	// adapter error aborts
	s3, m3 := newMock(t)
	m3.ExpectQuery(`cache_adapter_config`).WillReturnError(errors.New("a"))
	if _, err := s3.AssembleCacheConfigBlob(context.Background()); err == nil {
		t.Fatal("adapter error should abort")
	}
	// provider error aborts
	s4, m4 := newMock(t)
	m4.ExpectQuery(`cache_adapter_config`).WillReturnRows(pgxmock.NewRows([]string{"adapter_type", "config"}))
	m4.ExpectQuery(`cache_provider_config`).WillReturnError(errors.New("p"))
	if _, err := s4.AssembleCacheConfigBlob(context.Background()); err == nil {
		t.Fatal("provider error should abort")
	}
}

func TestGetProviderAdapterType(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`SELECT adapter_type FROM "Provider" WHERE id = \$1`).WithArgs("p1").
		WillReturnRows(pgxmock.NewRows([]string{"adapter_type"}).AddRow("openai"))
	at, ok, err := s.GetProviderAdapterType(context.Background(), "p1")
	if err != nil || !ok || at != "openai" {
		t.Fatalf("GetProviderAdapterType: %q %v %v", at, ok, err)
	}
	m.ExpectQuery(`adapter_type FROM "Provider"`).WithArgs("missing").WillReturnError(pgx.ErrNoRows)
	if _, ok, err := s.GetProviderAdapterType(context.Background(), "missing"); err != nil || ok {
		t.Fatalf("missing → (\"\",false,nil), got ok=%v err=%v", ok, err)
	}
	m.ExpectQuery(`adapter_type FROM "Provider"`).WithArgs("e").WillReturnError(errors.New("db"))
	if _, _, err := s.GetProviderAdapterType(context.Background(), "e"); err == nil {
		t.Fatal("query error should surface")
	}
}

func TestGetProviderName(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`SELECT name FROM "Provider" WHERE id = \$1`).WithArgs("p1").
		WillReturnRows(pgxmock.NewRows([]string{"name"}).AddRow("OpenAI"))
	name, err := s.GetProviderName(context.Background(), "p1")
	if err != nil || name != "OpenAI" {
		t.Fatalf("GetProviderName: %q %v", name, err)
	}
	m.ExpectQuery(`name FROM "Provider"`).WithArgs("missing").WillReturnError(pgx.ErrNoRows)
	if name, err := s.GetProviderName(context.Background(), "missing"); err != nil || name != "" {
		t.Fatalf("missing → (\"\",nil), got %q %v", name, err)
	}
	m.ExpectQuery(`name FROM "Provider"`).WithArgs("e").WillReturnError(errors.New("db"))
	if _, err := s.GetProviderName(context.Background(), "e"); err == nil {
		t.Fatal("query error should surface")
	}
}
