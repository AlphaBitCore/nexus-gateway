package scheduler

import "log/slog"

// slogAdapter implements cron.Logger over our slog handle so the
// SkipIfStillRunning + Recover wrappers from github.com/robfig/cron/v3
// integrate with Hub's structured logging instead of printing to stderr.
type slogAdapter struct {
	inner *slog.Logger
}

func (a slogAdapter) Info(msg string, kv ...any) {
	if a.inner == nil {
		return
	}
	a.inner.Info(msg, kv...)
}

func (a slogAdapter) Error(err error, msg string, kv ...any) {
	if a.inner == nil {
		return
	}
	if err != nil {
		kv = append(kv, "error", err.Error())
	}
	a.inner.Error(msg, kv...)
}
