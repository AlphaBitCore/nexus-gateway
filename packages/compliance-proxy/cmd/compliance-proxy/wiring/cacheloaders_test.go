package wiring

import (
	"context"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/access"
	configcache "github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/config/cache"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/domain"
)

func newMinimalAccessChecker(t *testing.T) *access.Checker {
	t.Helper()
	checker, err := access.NewChecker(nil, nil, nil)
	if err != nil {
		t.Fatalf("access.NewChecker: %v", err)
	}
	return checker
}

func TestRegisterCacheLoaders_NilDBIsNoop(t *testing.T) {
	cacheManager := configcache.NewManager(5*time.Minute, testLogger())
	domainEngine := domain.NewEngine()
	checker := newMinimalAccessChecker(t)

	// No-op when configDB is nil — must not panic.
	RegisterCacheLoaders(nil, cacheManager, domainEngine, checker, testLogger())
}

func TestRegisterCacheLoaders_NilDBDoesNotRegisterLoaders(t *testing.T) {
	cacheManager := configcache.NewManager(5*time.Minute, testLogger())
	domainEngine := domain.NewEngine()
	checker := newMinimalAccessChecker(t)

	RegisterCacheLoaders(nil, cacheManager, domainEngine, checker, testLogger())
	// Attempting a Get on an unregistered category should return an error, not
	// panic — verifying that no loader was registered.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := cacheManager.Get(ctx, configcache.CategoryInterceptionDomains)
	if err == nil {
		t.Error("expected error for unregistered category after nil-DB no-op")
	}
}
