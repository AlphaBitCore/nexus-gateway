// Package spillfactory wires the SpillStore abstraction to concrete backend
// implementations. It lives one level under shared/spillstore (rather than
// inside it) to break the import cycle — localfs imports the spillstore
// interface package, so the factory cannot live alongside the interface.
package spillfactory

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore/async"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore/localfs"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore/s3"
)

// FactoryConfig is the YAML shape services use to construct a SpillStore.
// Marshalled as part of the per-service config (e.g. ai-gateway.config.yaml
// has a top-level `spill:` block matching this struct). The same shape is
// consumed by Hub's read endpoint and by Control Plane's body-resolver.
//
// A zero-value FactoryConfig (Enabled=false, no backend set) means the
// service does NOT spill — every captured body is emitted inline,
// regardless of size. This is the default for tests and for deployments
// that haven't opted into spill yet.
//
// The inline-vs-spill cutoff is NOT in this struct: it lives on
// payloadcapture.Config.MaxInlineBodyBytes (admin-tunable at runtime via
// the payload_capture shadow). FactoryConfig owns only backend selection
// and storage policy (`PerObjectCap`, retention) — operator concerns,
// not admin concerns.
type FactoryConfig struct {
	// Enabled gates the entire spill subsystem. When false, every captured
	// body stays inline. Default: false.
	Enabled bool `yaml:"enabled"`

	// Backend selects the SpillStore implementation. Supported:
	// "localfs" (default), "s3".
	Backend string `yaml:"backend"`

	// Localfs holds the localfs-backend options. Required when
	// Backend=="localfs". Other backends ignore this block.
	Localfs LocalfsOptions `yaml:"localfs"`

	// S3 holds the S3-backend options. Required when Backend=="s3".
	// Other backends ignore this block. Authentication is via the AWS
	// SDK default credential chain (IAM role / env vars / profile);
	// keys are intentionally NOT plumbed through YAML.
	S3 S3Options `yaml:"s3"`

	// Async wraps the chosen backend with shared/storage/spillstore/async
	// so Put returns immediately (key + sha + size computed inline; the
	// actual PutObject lands on a background goroutine). Recommended for
	// S3 deployments where PutObject is 50-500 ms in production —
	// per-request blocking adds up to user-visible page slow-down.
	//
	// Cost: queued uploads live in memory until the worker drains them
	// (default 256-entry channel, ~MB-level per entry for large bodies).
	// On crash, queued-but-unuploaded bodies are lost; the audit row's
	// SpillRef will point at a missing object. Hub reads via presigned
	// URL will see 404 in this case and can retry later, matching the
	// at-most-once guarantee the audit pipeline already provides.
	//
	// Default false (sync) to preserve existing behaviour; opt-in per
	// service via yaml.
	Async bool `yaml:"async"`

	// AsyncQueueCapacity overrides async.Options.QueueCapacity when
	// Async=true. 0 = use the async package default (256).
	AsyncQueueCapacity int `yaml:"asyncQueueCapacity"`
}

// S3Options surfaces S3-backend tuning to YAML callers.
type S3Options struct {
	// Bucket is the S3 bucket name. Required.
	Bucket string `yaml:"bucket"`

	// Region is the AWS region (e.g. "us-east-1"). For non-AWS
	// S3-compatible stores set Endpoint as well; Region is still
	// required by the SDK's request signer.
	Region string `yaml:"region"`

	// Endpoint is the optional custom S3 endpoint URL for
	// S3-compatible stores (MinIO, Ceph RGW, R2). Empty = AWS S3.
	Endpoint string `yaml:"endpoint"`

	// Prefix is the per-deployment key prefix inside the bucket
	// (e.g. "nexus/spill/"). Useful when sharing a bucket between
	// environments. Leading / trailing slashes are normalised.
	Prefix string `yaml:"prefix"`

	// UsePathStyle picks between path-style and virtual-host-style
	// addressing. Set true for MinIO / Ceph; false (default) for AWS S3.
	UsePathStyle bool `yaml:"usePathStyle"`

	// PerObjectCap caps a single Put's persisted size. 0 = use the
	// s3-backend default (256 MiB).
	PerObjectCap int64 `yaml:"perObjectCap"`

	// TotalSizeCap is the soft total-bytes ceiling enforced by Sweep.
	// 0 = use the s3-backend default.
	TotalSizeCap int64 `yaml:"totalSizeCap"`

	// RetentionDays is the rolling sweep horizon. 0 = use the
	// s3-backend default.
	RetentionDays int `yaml:"retentionDays"`
}

// LocalfsOptions surfaces localfs-backend tuning to YAML callers.
type LocalfsOptions struct {
	// Root is the directory under which spilled bodies live. All services
	// participating in the same deployment must point at the same Root
	// (typically a shared volume) so the read path can resolve refs
	// produced by any service. Required.
	Root string `yaml:"root"`

	// PerObjectCap caps a single Put's persisted size. 0 = use the
	// localfs default (256 MiB).
	PerObjectCap int64 `yaml:"perObjectCap"`

	// TotalSizeCap is the soft total-bytes ceiling enforced by Sweep. 0 =
	// use the localfs default.
	TotalSizeCap int64 `yaml:"totalSizeCap"`

	// RetentionDays is the rolling sweep horizon. 0 = use the localfs
	// default. Smaller values free disk faster; larger values keep more
	// audit history.
	RetentionDays int `yaml:"retentionDays"`
}

// New constructs a SpillStore from the YAML FactoryConfig. Returns nil + nil
// when cfg.Enabled is false — callers should then keep emitting bodies
// inline (the existing code path). Returns an error if the backend name is
// unknown or the backend constructor itself fails.
//
// When cfg.Async==true the returned store is an async.AsyncStore wrapping
// the chosen backend; callers SHOULD type-assert it to async.AsyncStore
// (or io.Closer) and Close on shutdown to drain the in-flight upload
// queue. Failure to Close does not leak goroutines beyond process exit
// but does mean some queued uploads land in S3 without their Hub-side
// audit row catching up.
func New(cfg FactoryConfig, logger *slog.Logger) (spillstore.SpillStore, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	var inner spillstore.SpillStore
	switch cfg.Backend {
	case "localfs", "":
		// localfs as the default when Backend is unset so a minimal
		// `spill: { enabled: true, localfs: { root: ... } }` works
		// without naming the backend.
		retention := time.Duration(cfg.Localfs.RetentionDays) * 24 * time.Hour
		store, err := localfs.New(localfs.Options{
			Root:         cfg.Localfs.Root,
			PerObjectCap: cfg.Localfs.PerObjectCap,
			TotalSizeCap: cfg.Localfs.TotalSizeCap,
			Retention:    retention,
		})
		if err != nil {
			return nil, fmt.Errorf("spillstore: construct localfs backend: %w", err)
		}
		logger.Info("spillstore initialised", "backend", "localfs", "root", cfg.Localfs.Root, "async", cfg.Async)
		inner = store
	case "s3":
		retention := time.Duration(cfg.S3.RetentionDays) * 24 * time.Hour
		store, err := s3.New(context.Background(), s3.Options{
			Bucket:       cfg.S3.Bucket,
			Region:       cfg.S3.Region,
			Endpoint:     cfg.S3.Endpoint,
			Prefix:       cfg.S3.Prefix,
			UsePathStyle: cfg.S3.UsePathStyle,
			PerObjectCap: cfg.S3.PerObjectCap,
			TotalSizeCap: cfg.S3.TotalSizeCap,
			Retention:    retention,
		})
		if err != nil {
			return nil, fmt.Errorf("spillstore: construct s3 backend: %w", err)
		}
		logger.Info("spillstore initialised",
			"backend", "s3",
			"bucket", cfg.S3.Bucket,
			"region", cfg.S3.Region,
			"endpoint", cfg.S3.Endpoint,
			"async", cfg.Async,
			"authMode", "AWS SDK default credential chain (IAM role / env / profile)")
		inner = store
	default:
		return nil, fmt.Errorf("spillstore: unknown backend %q (supported: localfs, s3)", cfg.Backend)
	}

	if !cfg.Async {
		return inner, nil
	}
	// async.New only errors when inner doesn't implement Presigner.
	// Both localfs.Store and s3.Store implement Presigner (verified by
	// the package's TestNew_*Wrap tests below), so this wrap is
	// guaranteed to succeed for the inputs we construct above. If a
	// future backend is added that does NOT implement Presigner, the
	// per-backend switch above MUST explicitly skip the async wrap
	// (or panic loudly) rather than reach here.
	asyncStore, _ := async.New(inner, async.Options{
		QueueCapacity: cfg.AsyncQueueCapacity,
		Logger:        logger,
	})
	logger.Info("spillstore async wrapper active",
		"backend", inner.Backend(),
		"queue_capacity", cfg.AsyncQueueCapacity,
	)
	return asyncStore, nil
}

// PerObjectCap returns the active per-object hard ceiling for the
// configured backend (256 MiB default when neither block sets it
// explicitly). Used by the producer-side streaming capture tee to bound
// in-memory growth on long SSE responses, independent of the admin
// MaxInlineBodyBytes (which only governs inline-vs-spill, not capture).
func (c FactoryConfig) PerObjectCap() int64 {
	const fallback int64 = 256 * 1024 * 1024 // 256 MiB
	switch c.Backend {
	case "s3":
		if c.S3.PerObjectCap > 0 {
			return c.S3.PerObjectCap
		}
	default: // "localfs" or empty
		if c.Localfs.PerObjectCap > 0 {
			return c.Localfs.PerObjectCap
		}
	}
	return fallback
}
