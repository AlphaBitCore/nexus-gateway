package schema

import (
	"os"
	"path/filepath"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/paths"
)

func applyDefaults(cfg *AgentConfig) {
	if cfg.Log.Level == "" {
		cfg.Log.Level = "info"
	}
	if cfg.Log.Format == "" {
		cfg.Log.Format = "json"
	}
	// Environment variable overrides for log level.
	if v := os.Getenv("LOG_LEVEL"); v != "" {
		cfg.Log.Level = v
	}

	// Filesystem path defaults come from platform.DefaultPaths so each OS
	// puts state in the right place (macOS /Library/Application Support,
	// Linux /var/lib, Windows %ProgramData%). YAML overrides win when set;
	// blank fields below fall through to the OS-idiomatic default.
	paths := paths.DefaultPaths()
	// Device cert + key are ISSUED at enroll time — they don't exist on
	// a fresh, never-enrolled install. We MUST leave the cfg fields
	// empty in that state so hub.NewClient's
	//   if cfg.CertFile != "" && cfg.KeyFile != "" { LoadX509KeyPair... }
	// branch skips mTLS instead of failing on a missing file. The
	// daemon's `if !enrollMgr.IsEnrolled() { runPendingEnrollment(...) }`
	// fork then takes over and serves the status IPC so the Dashboard
	// can drive SSO sign-in.
	//
	// Behaviour: only DEFAULT the path when a file is already present
	// at the canonical location. Enrollment writes the cert + key,
	// next daemon start re-reads applyDefaults, the files now exist,
	// and the paths get filled in → full mTLS-Hub client comes up.
	// This pattern matches what we already do for CACertFile below.
	if cfg.CertFile == "" {
		candidate := filepath.Join(paths.StateDir, "device.pem")
		if _, err := os.Stat(candidate); err == nil {
			cfg.CertFile = candidate
		}
	}
	if cfg.KeyFile == "" {
		candidate := filepath.Join(paths.StateDir, "device-key.pem")
		if _, err := os.Stat(candidate); err == nil {
			cfg.KeyFile = candidate
		}
	}
	// CACertFile is OPTIONAL: a Hub-TLS pinning override for air-gapped
	// installs / self-signed dev Hubs. Production Hub uses an AWS-signed
	// public cert chain that's already trusted by the OS cert store, so
	// the canonical prod path is "no CA pin, use system trust" → empty.
	//
	// Only default the path when a file actually exists at that location —
	// operators drop a `gateway-ca.pem` into StateDir to pin, and we
	// auto-discover it. When it isn't there, we leave the field empty
	// rather than setting a path that hub.NewClient would then
	// fatally error on. Earlier behaviour (always default the path)
	// crashed the daemon at startup on every fresh install where the
	// optional pinning file wasn't pre-staged.
	if cfg.CACertFile == "" {
		candidate := filepath.Join(paths.StateDir, "gateway-ca.pem")
		if _, err := os.Stat(candidate); err == nil {
			cfg.CACertFile = candidate
		}
	}
	// HubCACertFile is intentionally left blank-by-default — EffectiveHubCA()
	// falls back to CACertFile, which gives the right behaviour without
	// duplicating the same path in two fields.
	if cfg.AuditDBPath == "" {
		cfg.AuditDBPath = filepath.Join(paths.StateDir, "audit.db")
	}
	if cfg.Log.File == "" {
		cfg.Log.File = filepath.Join(paths.LogDir, "agent.log")
	}

	if cfg.HeartbeatIntervalSec == 0 {
		cfg.HeartbeatIntervalSec = 60
	}
	if cfg.AuditDrainIntervalSec == 0 {
		// 5s, not 30s: paired with the drain loop's drain-while-full
		// behaviour, this bounds how long a small batch (fewer than
		// AuditBatchSize events) sits before upload, so the Dashboard's
		// unsynced count reflects reality within seconds. A backlog is
		// cleared by the back-to-back batching, not by this interval.
		cfg.AuditDrainIntervalSec = 5
	}
	if cfg.AuditBatchSize == 0 {
		cfg.AuditBatchSize = 200
	}
	if cfg.AuditRetentionDays == 0 {
		cfg.AuditRetentionDays = 30
	}
	// The agent's audit-overflow policy has to be defaulted HERE, not left to the
	// shared resolver, and the difference is a host-stall.
	//
	// shared/audit/lossmode.Resolve maps an empty string to the no-loss default
	// (spillblock) — correct for a server, wrong for this process: a no-loss mode
	// blocks the emitting goroutine until the record is durable, and on the agent
	// that goroutine is on the host's own outbound packet path (CLAUDE.md's
	// never-stall-the-host rule). The queue writer's constructor picks Spill for
	// exactly that reason, but the wiring then calls WithLossMode(cfg.AuditLossMode)
	// unconditionally, so an EMPTY config value overrides the safe constructor
	// choice with the blocking one.
	//
	// All three shipped templates carry `auditLossMode: spill`, so a fresh install
	// was fine — but an agent UPGRADED in place keeps its existing agent.yaml, which
	// predates the key, and would have silently switched to blocking. Defaulting
	// here makes the upgrade path match the templates.
	if cfg.AuditLossMode == "" {
		cfg.AuditLossMode = "spill"
	}
	if cfg.DefaultAction == "" {
		cfg.DefaultAction = "passthrough"
	}
	// TrafficUploadLevel defaults to "processed" — only flows where the
	// agent did actual HTTP inspection (TLS bumped, adapter matched, hook
	// evaluated) reach Hub. Silent TCP passthroughs of WhatsApp / WeChat /
	// SSH / git etc. stay local-only. Admins can flip to "all" via the CP
	// Device Defaults page for audit / debugging windows, then back.
	switch cfg.TrafficUploadLevel {
	case "all", "processed", "blocked":
		// valid — keep as-is
	default:
		cfg.TrafficUploadLevel = "processed"
	}
	if cfg.UpdaterCheckSec == 0 {
		cfg.UpdaterCheckSec = 3600
	}
	// macOS NE → Go MITM bridge default. Empty in YAML means
	// "use the canonical loopback port" — operators rarely want to
	// move it, and an unset field should activate the feature.
	if cfg.MitmBridgeAddr == "" {
		cfg.MitmBridgeAddr = "127.0.0.1:9443"
	}
	// QuitAllowed defaults to true. Using *bool allows distinguishing
	// "not set in YAML" (nil → default true) from "explicitly set false".
	if cfg.QuitAllowed == nil {
		t := true
		cfg.QuitAllowed = &t
	}
	// LocalBodyCapture defaults to true — the agent always captures bodies
	// locally so users can inspect their own AI traffic; Hub config only gates
	// upload. *bool distinguishes unset (nil → true) from explicit false.
	if cfg.LocalBodyCapture == nil {
		t := true
		cfg.LocalBodyCapture = &t
	}
	// Cert-pin exemption defaults
	if cfg.ExemptionFailureThreshold == 0 {
		cfg.ExemptionFailureThreshold = 3
	}
	if cfg.ExemptionWindowSec == 0 {
		cfg.ExemptionWindowSec = 60
	}
	if cfg.ExemptionDurationSec == 0 {
		cfg.ExemptionDurationSec = 86400
	}
	if cfg.PlatformBridgeAddress == "" {
		cfg.PlatformBridgeAddress = "127.0.0.1:19080"
	}
	// OTEL defaults
	if cfg.OtelEndpoint == "" {
		cfg.OtelEndpoint = "http://localhost:4318"
	}
	if cfg.OtelServiceName == "" {
		cfg.OtelServiceName = "nexus-agent"
	}
	if cfg.OtelSamplingRate == 0 {
		cfg.OtelSamplingRate = 1.0
	}
}
