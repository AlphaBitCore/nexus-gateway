package wiring

import (
	"testing"
)

// TestInitRouter_nilDeps verifies InitRouter with nil optional deps does not
// panic and returns non-nil strategy registry, health ranker, and capability
// cache.
func TestInitRouter_nilDeps(t *testing.T) {
	stratReg, healthRanker, resolver, capCache := InitRouter(nil, nil, nil, nil, discardLogger())
	if stratReg == nil {
		t.Error("expected non-nil strategy registry")
	}
	if healthRanker == nil {
		t.Error("expected non-nil health ranker")
	}
	if resolver == nil {
		t.Error("expected non-nil routing resolver")
	}
	if capCache == nil {
		t.Error("expected non-nil capability cache")
	}
}
