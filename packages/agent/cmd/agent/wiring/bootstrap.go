package wiring

import (
	"log/slog"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/host/updater"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/logging"
)

// InitLogger builds the daemon's structured logger from the agent config's
// log section and installs it as the process default.
func InitLogger(level, format, file string, stackOnError bool) (*slog.Logger, error) {
	logger, err := logging.NewLogger(logging.Config{
		Level:        level,
		Format:       format,
		File:         file,
		StackOnError: stackOnError,
	})
	if err != nil {
		return nil, err
	}
	slog.SetDefault(logger)
	return logger, nil
}

// InitCrashLoopGuard checks for a crash loop before full startup and records
// the new start in the status file (auditDBPath + ".status"). When the
// previous starts crashed quickly in succession, the updater rolls the binary
// back to the prior version and a warning is logged.
func InitCrashLoopGuard(selfPath, auditDBPath string) {
	statusFile := auditDBPath + ".status"
	if updater.DetectCrashLoop(selfPath, statusFile, 30*time.Second) {
		slog.Warn("rolled back to previous version due to crash loop")
	}
	_ = updater.WriteStartStatus(statusFile)
}
