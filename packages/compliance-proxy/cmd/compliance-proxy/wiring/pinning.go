package wiring

import (
	"log/slog"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/cmd/compliance-proxy/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/tlsbump"
)

// InitPinningTracker builds the TLS-pinning tracker from config.
func InitPinningTracker(cfg *config.Config) *tlsbump.PinningTracker {
	var pinningExemptions []tlsbump.DomainExemption
	for _, e := range cfg.Audit.Pinning.Exemptions {
		pinningExemptions = append(pinningExemptions, tlsbump.DomainExemption{
			Host:   e.Host,
			Reason: e.Reason,
		})
	}
	tracker := tlsbump.NewPinningTracker(tlsbump.PinningConfig{
		Exemptions: pinningExemptions,
		AutoExempt: tlsbump.AutoExemptConfig{
			Enabled:           cfg.Audit.Pinning.AutoExempt.Enabled,
			FailureThreshold:  cfg.Audit.Pinning.AutoExempt.FailureThreshold,
			WindowSeconds:     cfg.Audit.Pinning.AutoExempt.WindowSeconds,
			ExemptionDuration: time.Duration(cfg.Audit.Pinning.AutoExempt.ExemptionDurationSeconds) * time.Second,
		},
	})
	slog.Info("pinning tracker initialized",
		"configuredExemptions", len(cfg.Audit.Pinning.Exemptions),
		"autoExemptEnabled", cfg.Audit.Pinning.AutoExempt.Enabled,
	)
	return tracker
}
