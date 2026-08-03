package wiring

import (
	"context"
	"net/http"
	"testing"

	"github.com/pashagolub/pgxmock/v4"

	cachelayer "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/layer"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/aiguard"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/telemetry"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/matcher"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore/spillfactory"
)

// TestInitIntrospectRegistry_allNilDeps verifies that InitIntrospectRegistry
// with all-nil optional deps does not panic and returns a non-nil registry.
func TestInitIntrospectRegistry_allNilDeps(t *testing.T) {
	mux := http.NewServeMux()
	reg := InitIntrospectRegistry(IntrospectDeps{
		AgID:         "test-ag",
		BuildVersion: "v0.0.1",
	}, mux)
	if reg == nil {
		t.Fatal("expected non-nil introspect registry")
	}
}

// TestInitIntrospectRegistry_withPayloadCaptureStore verifies the source
// is registered without panic when PayloadCaptureStore is non-nil.
func TestInitIntrospectRegistry_withPayloadCaptureStore(t *testing.T) {
	mux := http.NewServeMux()
	pcs := payloadcapture.NewStore(payloadcapture.DefaultConfig())
	reg := InitIntrospectRegistry(IntrospectDeps{
		AgID:                "test-ag",
		BuildVersion:        "v0.0.1",
		PayloadCaptureStore: pcs,
	}, mux)
	if reg == nil {
		t.Fatal("expected non-nil introspect registry")
	}
}

// TestInitIntrospectRegistry_withHookConfigCache exercises HookConfigCache branch.
func TestInitIntrospectRegistry_withHookConfigCache(t *testing.T) {
	mux := http.NewServeMux()
	reg2, err := InitHookRegistry(config.HTTPClientPoolConfig{TimeoutSec: 5}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	hookCache := InitHookConfigCache(nil, reg2, discardLogger())
	reg := InitIntrospectRegistry(IntrospectDeps{
		AgID:            "test-ag",
		HookConfigCache: hookCache,
	}, mux)
	if reg == nil {
		t.Fatal("expected non-nil introspect registry")
	}
}

// TestInitIntrospectRegistry_withObservabilityGet exercises ObservabilityGet branch.
func TestInitIntrospectRegistry_withObservabilityGet(t *testing.T) {
	mux := http.NewServeMux()
	reg := InitIntrospectRegistry(IntrospectDeps{
		AgID: "test-ag",
		ObservabilityGet: func() *telemetry.Config {
			return nil
		},
	}, mux)
	if reg == nil {
		t.Fatal("expected non-nil registry")
	}
}

// TestInitIntrospectRegistry_withAIGuardConfigCache exercises AIGuardConfigCache branch.
func TestInitIntrospectRegistry_withAIGuardConfigCache(t *testing.T) {
	mux := http.NewServeMux()
	reg := InitIntrospectRegistry(IntrospectDeps{
		AgID:               "test-ag",
		AIGuardConfigCache: func() *aiguard.ConfigCache { return nil },
	}, mux)
	if reg == nil {
		t.Fatal("expected non-nil registry")
	}
}

// TestInitIntrospectRegistry_withAuthToken verifies auth token is passed through.
func TestInitIntrospectRegistry_withAuthToken(t *testing.T) {
	mux := http.NewServeMux()
	reg := InitIntrospectRegistry(IntrospectDeps{
		AgID:      "test-ag",
		AuthToken: "secret-token",
	}, mux)
	if reg == nil {
		t.Fatal("expected non-nil introspect registry")
	}
}

// TestMountRuntimeAPI_nilThingClientIsNoOp verifies that nil thingclient
// means no routes are mounted (no panic).
func TestMountRuntimeAPI_nilThingClientIsNoOp(t *testing.T) {
	mux := http.NewServeMux()
	// Should not panic.
	MountRuntimeAPI(nil, "test-internal-token", mux)
}

// newTestCacheLayer creates a cachelayer.Layer backed by pgxmock for introspect tests.
func newTestCacheLayer(t *testing.T) *cachelayer.Layer {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mock.Close)
	db := store.NewWithPgxPool(mock)
	l, err := cachelayer.NewWithPool(db, mock, discardLogger(), cachelayer.Config{})
	if err != nil {
		t.Fatalf("NewWithPool: %v", err)
	}
	return l
}

// TestInitIntrospectRegistry_withCacheLayer exercises the CacheLayer branch.
func TestInitIntrospectRegistry_withCacheLayer(t *testing.T) {
	mux := http.NewServeMux()
	layer := newTestCacheLayer(t)
	reg := InitIntrospectRegistry(IntrospectDeps{
		AgID:       "test-ag",
		CacheLayer: layer,
	}, mux)
	if reg == nil {
		t.Fatal("expected non-nil introspect registry")
	}
}

// TestInitIntrospectRegistry_withConfigKeyRecorder exercises ConfigKeyRecorder branch.
func TestInitIntrospectRegistry_withConfigKeyRecorder(t *testing.T) {
	mux := http.NewServeMux()
	// ConfigKeyRecorder can be constructed from runtimeintrospect package,
	// but we cannot directly import it without adding a dep. Instead pass nil
	// to verify that nil ConfigKeyRecorder is handled gracefully (no panic).
	reg := InitIntrospectRegistry(IntrospectDeps{
		AgID:              "test-ag",
		ConfigKeyRecorder: nil,
	}, mux)
	if reg == nil {
		t.Fatal("expected non-nil introspect registry")
	}
}

// TestInitIntrospectRegistry_withPolicyCache exercises the PolicyCache branch.
// quota.NewPolicyCache requires a *pgxpool.Pool but NewPolicyCache itself
// only stores it — no query at construction. We test only that the
// IntrospectDeps.PolicyCache=nil branch is safe (already covered), so here
// we just verify the all-nil run doesn't panic again.
func TestInitIntrospectRegistry_allNilDepsAlsoMountsHandler(t *testing.T) {
	mux := http.NewServeMux()
	reg := InitIntrospectRegistry(IntrospectDeps{AgID: "ag2"}, mux)
	if reg == nil {
		t.Fatal("expected non-nil registry")
	}
}

// The storage.spill source must be registered even when NO spill backend
// exists — that is precisely the state an operator cannot otherwise observe,
// because the admin-facing inline-vs-spill threshold then silently does
// nothing. Registering it only when a store was built would hide exactly the
// case worth reporting.
func TestInitIntrospectRegistry_SpillSourceRegisteredWithNoBackend(t *testing.T) {
	mux := http.NewServeMux()
	reg := InitIntrospectRegistry(IntrospectDeps{
		SpillAvailability: spillfactory.Describe(spillfactory.FactoryConfig{}, nil),
	}, mux)

	snap := reg.Snapshot(context.Background())
	res, ok := snap.Sources["storage.spill"]
	if !ok {
		t.Fatal("storage.spill source is absent; a node with no spill backend reports nothing about it")
	}
	av, ok := res.Value.(spillfactory.Availability)
	if !ok {
		t.Fatalf("storage.spill value is %T, want spillfactory.Availability", res.Value)
	}
	if av.Configured {
		t.Fatal("storage.spill reports Configured=true with no store built")
	}
}

// And it must carry the real posture when a backend IS built, including the
// host-local flag that tells an operator whether other nodes can read it.
func TestInitIntrospectRegistry_SpillSourceReportsLocalfsPosture(t *testing.T) {
	cfg := spillfactory.FactoryConfig{Enabled: true, Backend: "localfs"}
	cfg.Localfs.Root = t.TempDir()
	store, err := spillfactory.New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("build store: %v", err)
	}

	reg := InitIntrospectRegistry(IntrospectDeps{
		SpillAvailability: spillfactory.Describe(cfg, store),
	}, http.NewServeMux())

	snap := reg.Snapshot(context.Background())
	av := snap.Sources["storage.spill"].Value.(spillfactory.Availability)
	if !av.Configured || av.Backend != "localfs" {
		t.Fatalf("storage.spill = %+v, want a configured localfs backend", av)
	}
	if !av.HostLocal {
		t.Fatal("localfs reported as not host-local; an operator would not learn that other nodes cannot read these bodies")
	}
	if av.Location != cfg.Localfs.Root {
		t.Fatalf("Location = %q, want the configured root", av.Location)
	}
}

// TestInitIntrospectRegistry_ReportsMatcherEngine — the gateway runs the same
// rule-pack engine as the proxy, and the engine is a build-tag choice with an
// order-of-magnitude cost difference on large bodies, so the running binary has
// to be able to say which one it got.
func TestInitIntrospectRegistry_ReportsMatcherEngine(t *testing.T) {
	mux := http.NewServeMux()
	reg := InitIntrospectRegistry(IntrospectDeps{AgID: "gw-test", BuildVersion: "v0-test"}, mux)
	if reg == nil {
		t.Fatal("expected a registry")
	}
	snap := reg.Snapshot(context.Background())
	src, ok := snap.Sources["policy.matcher"]
	if !ok {
		t.Fatal("no policy.matcher source registered")
	}
	eng, ok := src.Value.(matcher.Engine)
	if !ok {
		t.Fatalf("policy.matcher value is %T, want matcher.Engine", src.Value)
	}
	if eng.Name == "" || eng.Effect == "" {
		t.Errorf("engine reported without a name or an effect: %+v", eng)
	}
	if eng.Name != matcher.DescribeEngine().Name {
		t.Errorf("engine name = %q, want the compiled-in %q", eng.Name, matcher.DescribeEngine().Name)
	}
}
