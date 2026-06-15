//go:build darwin

package platformshim

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/cmd/agent/wiring"
	agentcompliance "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/compliance"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/identity/attestation"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/identity/keystore"
	agentproxy "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/network/proxy"
	auditqueue "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/audit/queue"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/backpressure"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/api"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/darwin"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/paths"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	localfsspill "github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore/localfs"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore/spillsweep"
	normalizecore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	streampolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming/policy"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/tlsbump"
)

// DarwinBridgeArgs bundles the dependencies the darwin NE bridge wiring
// needs from main.go. Kept as a struct (not positional args) so adding
// new wire points doesn't churn callers.
type DarwinBridgeArgs struct {
	// Keystore supplies the device-held secret for the spill store's
	// at-rest key. The composition root passes the platform store; tests
	// pass a memory store. Nil skips spill (bodies truncate inline).
	Keystore             keystore.Store
	Logger               *slog.Logger
	BridgeAddr           string
	AgentPipeline        *agentcompliance.AgentPipeline
	PayloadCaptureStore  *payloadcapture.Store
	AuditQueue           *auditqueue.Queue
	StreamingPolicyStore *streampolicy.Store
	// NormalizeRegistry — the Tier 1+2+3 normalize chain shared with
	// ai-gateway and compliance-proxy. Wired into BridgeDeps so
	// runtimeNormalize sees canonical NormalizedPayload for non-standard
	// wire formats (chatgpt-web SSE delta_encoding, cursor protobuf, etc.)
	// that fall through to Tier 2 pattern + Tier 3 verbatim. Nil keeps
	// the legacy adapter-direct path.
	NormalizeRegistry *normalizecore.Registry
	// AttestationSigner — when non-nil, every outbound HTTPS request through
	// the bridge's UpstreamTransport gets the X-Nexus-Attestation header
	// injected via the agent's Ed25519 cert. Nil disables attestation; no
	// per-request signing cost. Wired by cmd_run.go once enrollment confirms
	// an Ed25519 cert is on disk.
	AttestationSigner *attestation.Signer
}

// WireDarwinBackpressure threads the audit-queue backpressure store into
// the platform shim's handleNewFlow gate. Only DarwinPlatform implements
// SetBackpressure; this file only compiles on darwin so the type assertion
// is safe.
func WireDarwinBackpressure(plat api.Platform, store *backpressure.Store) {
	if dwn, ok := plat.(*darwin.DarwinPlatform); ok {
		dwn.SetBackpressure(store)
	}
}

// WireDarwinBridge constructs the shared/tlsbump-backed BridgeDeps for
// the agent's NE bridge ingress, wires them into DarwinPlatform via
// SetBridgeDeps, and starts the bridge listener. Returns an io.Closer
// the caller defers; nil when the bridge isn't applicable (BridgeAddr
// empty, plat is not Darwin, or any setup step fails — each failure
// mode logs WARN, none are fatal because the raw-relay path is the
// fallback).
func WireDarwinBridge(ctx context.Context, plat api.Platform, args DarwinBridgeArgs) io.Closer {
	logger := args.Logger
	if logger == nil {
		logger = slog.Default()
	}

	dwn, ok := plat.(*darwin.DarwinPlatform)
	if !ok || args.BridgeAddr == "" {
		return nil
	}

	paths := paths.DefaultPaths()
	caCertPath := filepath.Join(paths.StateDir, "device-ca.pem")
	caKeyPath := filepath.Join(paths.StateDir, "device-ca.key")
	if loadErr := dwn.LoadTLSEngineFromDisk(caCertPath, caKeyPath); loadErr != nil {
		logger.Warn("bridge: TLS engine load failed; bridge will fail-open to Swift raw-relay", "error", loadErr)
		return nil
	}

	// When an attestation Signer is wired, install it as the request injector
	// so every outbound HTTPS request through this transport carries the
	// X-Nexus-Attestation header. The injector is fail-open — signer errors
	// omit the header but never abort the request.
	upstreamOpts := tlsbump.UpstreamOptions{}
	if args.AttestationSigner != nil {
		upstreamOpts.RequestInjector = args.AttestationSigner.InjectInto
		logger.Info("attestation: request injector installed on UpstreamTransport")
	}
	upstreamTransport, upErr := tlsbump.NewUpstreamTransportWith(100, 90*time.Second, 10*time.Second, upstreamOpts)
	if upErr != nil {
		logger.Warn("bridge: tlsbump.NewUpstreamTransportWith failed; raw-relay path stays active", "error", upErr)
	} else if args.AgentPipeline != nil {
		// Same encrypted localfs store the audit drain reads back from
		// (wiring.NewLocalSpillStore is the single keyed construction point).
		var spill *localfsspill.Store
		spillErr := fmt.Errorf("no keystore supplied")
		if args.Keystore != nil {
			spill, spillErr = wiring.NewLocalSpillStore(args.Keystore)
		}
		if spillErr != nil {
			logger.Warn("bridge: localfs spillstore init failed; oversize bodies will truncate",
				"error", spillErr)
		} else {
			// Sweep the agent's local spill dir so its retention horizon and
			// total-size cap are enforced (hardcoded localfs, default horizon).
			go spillsweep.Run(context.Background(), spill, spillsweep.Options{
				Retention: spillsweep.DefaultRetention,
			}, logger)
		}

		bridgeDeps := &agentproxy.BridgeDeps{
			Logger:              logger,
			TLSEngine:           dwn.TLSEngine(),
			Upstream:            upstreamTransport,
			PolicyResolver:      args.AgentPipeline.Resolver(),
			DomainEngine:        args.AgentPipeline.DomainEngine(),
			AdapterRegistry:     args.AgentPipeline.AdapterRegistry(),
			NormalizeRegistry:   args.NormalizeRegistry,
			PayloadCaptureStore: args.PayloadCaptureStore,
			SpillStore:          spill,
			AuditQueue:          args.AuditQueue,
			StreamingPolicy:     args.StreamingPolicyStore,
			PerHookTimeout:      5 * time.Second,
			TotalTimeout:        30 * time.Second,
		}
		dwn.SetBridgeDeps(bridgeDeps)
		logger.Info("bridge: BridgeDeps wired — agent inspect flows now flow through shared/tlsbump.BumpConnection",
			"have_domain_engine", bridgeDeps.DomainEngine != nil,
			"have_adapter_registry", bridgeDeps.AdapterRegistry != nil,
			"have_spillstore", bridgeDeps.SpillStore != nil,
		)
	}

	br, brErr := dwn.StartBridge(ctx, args.BridgeAddr)
	if brErr != nil {
		logger.Warn("bridge: StartBridge failed; bridge will fail-open to Swift raw-relay", "error", brErr)
		return nil
	}
	if br != nil {
		logger.Info("bridge: macOS NE inspect flows accept loop running", "addr", args.BridgeAddr)
	}
	return br
}
