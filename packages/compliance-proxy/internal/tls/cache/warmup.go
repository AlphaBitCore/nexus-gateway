package cache

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/metrics"
)

// Warmup pre-signs certificates for all provided domains and caches them.
// It records the total duration in the nexus_proxy_cert_prewarm_duration_seconds metric.
func Warmup(ctx context.Context, cache *CertCache, domains []string, logger *slog.Logger) error {
	start := time.Now()
	var failed int

	for _, domain := range domains {
		select {
		case <-ctx.Done():
			return fmt.Errorf("cert warmup: context cancelled: %w", ctx.Err())
		default:
		}

		logger.Info("warming up certificate", slog.String("domain", domain))
		if _, err := cache.GetCertByHostname(domain); err != nil {
			logger.Error("warmup failed for domain",
				slog.String("domain", domain),
				slog.String("error", err.Error()),
			)
			failed++
		}
	}

	elapsed := time.Since(start)
	if metrics.CertPrewarmMs != nil {
		metrics.CertPrewarmMs.With().Set(float64(elapsed.Milliseconds()))
	}

	logger.Info("certificate warmup complete",
		slog.Int("total", len(domains)),
		slog.Int("failed", failed),
		slog.Duration("elapsed", elapsed),
	)

	if failed > 0 {
		return fmt.Errorf("cert warmup: %d of %d domains failed", failed, len(domains))
	}
	return nil
}
