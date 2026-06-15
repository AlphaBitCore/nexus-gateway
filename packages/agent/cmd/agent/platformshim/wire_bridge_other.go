//go:build !darwin

package platformshim

import (
	"context"
	"io"
	"log/slog"

	agentcompliance "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/compliance"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/identity/attestation"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/identity/keystore"
	auditqueue "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/audit/queue"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/backpressure"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/api"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	normalizecore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	streampolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming/policy"
)

// darwinBridgeArgs is the cross-platform stub for the darwin-only
// bundle in wire_bridge_darwin.go. Same field shape so main.go
// compiles uniformly; the values are simply ignored on non-darwin.
//
// MUST stay field-for-field aligned with the darwin variant — any
// new field added to wire_bridge_darwin.go's DarwinBridgeArgs has
// to be mirrored here or non-darwin cross-compiles break with
// "unknown field …" at cmd_run.go's struct literal site.
type DarwinBridgeArgs struct {
	Logger               *slog.Logger
	BridgeAddr           string
	AgentPipeline        *agentcompliance.AgentPipeline
	PayloadCaptureStore  *payloadcapture.Store
	AuditQueue           *auditqueue.Queue
	StreamingPolicyStore *streampolicy.Store
	NormalizeRegistry    *normalizecore.Registry
	AttestationSigner    *attestation.Signer
	Keystore             keystore.Store
}

// wireDarwinBackpressure is a no-op on non-darwin: linux/windows
// platform shims don't yet implement the bridge ingress, so there's
// nothing to wire backpressure into.
func WireDarwinBackpressure(_ api.Platform, _ *backpressure.Store) {}

// wireDarwinBridge is a no-op on non-darwin: the NE bridge ingress
// is darwin-only today. Returns nil so main.go's `if bridgeCloser
// != nil` defer is naturally skipped.
func WireDarwinBridge(_ context.Context, _ api.Platform, _ DarwinBridgeArgs) io.Closer {
	return nil
}
