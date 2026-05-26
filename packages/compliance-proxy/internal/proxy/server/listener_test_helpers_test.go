package server

import (
	"context"
	"errors"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/compliance"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/domain"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// noopAuditWriterReal implements audit.Writer with no-op methods.
type noopAuditWriterReal struct{}

func (noopAuditWriterReal) Enqueue(_ audit.AuditEvent)    {}
func (noopAuditWriterReal) Flush(_ context.Context) error { return nil }
func (noopAuditWriterReal) Close(_ context.Context) error { return nil }

// newAuditEmitterForTest returns a *compliance.AuditEmitter backed by a
// no-op writer so production EmitX paths can be exercised without any
// downstream side effects.
//
// The compliance package's AuditEmitter is the same shared type used in
// production (pipeline.AuditEmitter); we alias it through the
// per-service `compliance` package which re-exports it.
func newAuditEmitterForTest() *pipeline.AuditEmitter {
	return pipeline.NewAuditEmitter(noopAuditWriterReal{}, discardLogger())
}

// newPayloadCaptureStoreForTest returns a non-nil *payloadcapture.Store
// primed with the default config so the `if p.payloadCaptureStore != nil`
// branch fires the WithPayloadCapture append.
func newPayloadCaptureStoreForTest() *payloadcapture.Store {
	return payloadcapture.NewStore(payloadcapture.DefaultConfig())
}

// newDomainEngineForTest returns a non-nil *domain.Engine so the
// `if p.domainEngine != nil` branch fires the WithDomainEngine append.
func newDomainEngineForTest() *domain.Engine {
	return domain.NewEngine()
}

// newAdapterRegistryForTest returns a non-nil *traffic.AdapterRegistry so
// the `if p.adapterRegistry != nil` branch fires the WithAdapterRegistry
// append.
func newAdapterRegistryForTest() *traffic.AdapterRegistry {
	return traffic.NewAdapterRegistry("test")
}

// buildConnResolverWithErroringFactory wires a resolver whose only
// connection-stage hook references a factory that returns an error. This
// causes BuildPipeline → ResolveHooks → factory() to return err, exercising
// the fail-open branch in ServeHTTP.
func buildConnResolverWithErroringFactory(t *testing.T, registry *core.HookRegistry) *compliance.PolicyResolver {
	t.Helper()
	// Re-register on a fresh registry so the factory error is deterministic.
	reg := core.NewHookRegistry()
	reg.Register("erroring", func(_ *core.HookConfig) (core.Hook, error) {
		return nil, errors.New("factory boom")
	})
	return compliance.NewPolicyResolver([]core.HookConfig{
		{
			ID:                "h-err",
			ImplementationID:  "erroring",
			Name:              "erroring-hook",
			Stage:             "connection",
			Enabled:           true,
			FailBehavior:      "fail-open",
			ApplicableIngress: []string{"ALL"},
		},
	}, reg, discardLogger())
}
