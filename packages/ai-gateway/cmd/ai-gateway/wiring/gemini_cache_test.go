package wiring

import (
	"sync"
	"testing"
)

// sharedGeminiMgrSet is built once per test binary because
// geminicache.NewMetrics(prometheus.DefaultRegisterer) panics on duplicate
// registration when InitGeminiCacheMgrSet is called more than once.
var (
	sharedGeminiMgrSet     interface{}
	sharedGeminiMgrSetOnce sync.Once
)

func getSharedGeminiMgrSet(t *testing.T) interface{} {
	t.Helper()
	sharedGeminiMgrSetOnce.Do(func() {
		// nil rdb + nil cacheLayer + nil credMgr → degraded but non-nil ManagerSet.
		sharedGeminiMgrSet = InitGeminiCacheMgrSet(nil, nil, nil, discardLogger())
	})
	return sharedGeminiMgrSet
}

// TestInitGeminiCacheMgrSet_nilDepsReturnsNonNil verifies that InitGeminiCacheMgrSet
// with all-nil deps returns a non-nil ManagerSet (degraded mode).
func TestInitGeminiCacheMgrSet_nilDepsReturnsNonNil(t *testing.T) {
	ms := getSharedGeminiMgrSet(t)
	if ms == nil {
		t.Fatal("expected non-nil ManagerSet with nil deps")
	}
}
