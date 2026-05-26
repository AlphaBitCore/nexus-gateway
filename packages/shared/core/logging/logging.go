// Package logging provides a shared logger factory for all Nexus Gateway services.
// It standardizes logger initialization with configurable level (trace, debug,
// info, warn, error) and format (json, text).
package logging

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
)

// LevelTrace is a custom slog level below Debug, used for very verbose output
// (e.g. per-packet, per-field tracing). slog.LevelDebug is -4; Trace is -8.
const LevelTrace = slog.Level(-8)

// currentLevel is the process-wide hot-swappable log level. Each service
// runs as a single binary so one shared LevelVar is sufficient. slog
// resolves it on every record via the Leveler interface, giving us a
// lock-free hot path.
//
// NewLogger seeds this from cfg.Level on startup. SetLevel mutates it at
// runtime (called by the log_level shadow handler in each service's
// OnConfigChanged or, for Hub, by selfshadow.Manager).
//
// Default zero value is slog.LevelInfo, matching ParseLevel's fallback.
var currentLevel slog.LevelVar

// Config holds logger configuration.
type Config struct {
	Level  string // "trace", "debug", "info", "warn", "error" (default: "info")
	Format string // "json", "text" (default: "json")
	// File is the path to a log file. When non-empty, logs are written to both
	// os.Stdout and this file (append mode). Parent directories are created.
	// Overridden by environment variable LOG_FILE when set.
	File string
	// StackOnError attaches the current goroutine stack to every log record at
	// level error or higher (slog.LevelError). Overridden by LOG_STACK_ON_ERROR
	// when that env var is set to a non-empty value.
	StackOnError bool
}

// NewLogger creates a *slog.Logger from the given Config. The returned logger
// writes to os.Stdout, and also to cfg.File when that path is non-empty (after
// applying the LOG_FILE environment override). Call slog.SetDefault(logger) in
// main to wire it as the process default.
func NewLogger(cfg Config) (*slog.Logger, error) {
	filePath := strings.TrimSpace(cfg.File)
	if e := strings.TrimSpace(os.Getenv("LOG_FILE")); e != "" {
		filePath = e
	}

	stackOnError := cfg.StackOnError
	if v := strings.TrimSpace(os.Getenv("LOG_STACK_ON_ERROR")); v != "" {
		stackOnError = truthyEnv(v)
	}

	currentLevel.Set(ParseLevel(cfg.Level))

	opts := &slog.HandlerOptions{
		// &currentLevel implements slog.Leveler; the handler re-reads it
		// on every record so SetLevel takes effect immediately without
		// rebuilding the handler chain.
		Level:     &currentLevel,
		AddSource: true,
		// Replace the built-in level name with "TRACE" when the level is LevelTrace.
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.LevelKey {
				lvl, ok := a.Value.Any().(slog.Level)
				if ok && lvl == LevelTrace {
					a.Value = slog.StringValue("TRACE")
				}
			}
			return a
		},
	}

	out := io.Writer(os.Stdout)
	if filePath != "" {
		if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(filePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, err
		}
		out = io.MultiWriter(os.Stdout, f)
	}

	var handler slog.Handler
	switch strings.ToLower(cfg.Format) {
	case "text":
		handler = slog.NewTextHandler(out, opts)
	default:
		handler = slog.NewJSONHandler(out, opts)
	}

	if stackOnError {
		handler = &errorStackHandler{inner: handler}
	}

	return slog.New(handler), nil
}

// truthyEnv reports whether v is a conventional "true" for env vars.
func truthyEnv(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}

// errorStackHandler adds a "stack" attribute (goroutine stack) to error-level records.
type errorStackHandler struct {
	inner slog.Handler
}

func (h *errorStackHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *errorStackHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= slog.LevelError && !hasNoStackAttr(r) {
		r = r.Clone()
		r.Add(slog.String("stack", trimmedStack()))
	}
	return h.inner.Handle(ctx, r)
}

// hasNoStackAttr returns true when the record carries `noStack=true`,
// an opt-out for call sites that report data-state failures (e.g.
// scheduled freshness checks) where the goroutine stack is noise.
func hasNoStackAttr(r slog.Record) bool {
	skip := false
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "noStack" && a.Value.Kind() == slog.KindBool && a.Value.Bool() {
			skip = true
			return false
		}
		return true
	})
	return skip
}

// trimmedStack captures the current goroutine stack and strips frames belonging
// to the logging infrastructure (debug.Stack, this package, log/slog internals)
// so the first visible frame is the actual business call site.
func trimmedStack() string {
	raw := string(debug.Stack())
	lines := strings.Split(raw, "\n")
	if len(lines) < 3 {
		return raw
	}
	// lines[0] is the goroutine header; frames start at lines[1] in pairs
	// (function name line, then file:line+offset line).
	result := []string{lines[0]}
	i := 1
	for i+1 < len(lines) {
		fn := lines[i]
		if strings.Contains(fn, "runtime/debug.Stack") ||
			strings.Contains(fn, "shared/logging") ||
			strings.Contains(fn, "log/slog.") {
			i += 2
			continue
		}
		result = append(result, fn, lines[i+1])
		i += 2
	}
	// append any trailing partial line
	for i < len(lines) {
		result = append(result, lines[i])
		i++
	}
	return strings.Join(result, "\n")
}

func (h *errorStackHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &errorStackHandler{inner: h.inner.WithAttrs(attrs)}
}

func (h *errorStackHandler) WithGroup(name string) slog.Handler {
	return &errorStackHandler{inner: h.inner.WithGroup(name)}
}

// SetLevel atomically swaps the process-wide log level. Called by the
// log_level shadow handler in each service's OnConfigChanged (or, for
// Hub, by selfshadow.Manager). Unknown names degrade to Info via the
// same ParseLevel fallback as the YAML config path so a misspelled
// shadow payload cannot break logging.
//
// Returns the slog.Level that ended up being applied so the caller can
// log the transition with the resolved value.
func SetLevel(name string) slog.Level {
	lvl := ParseLevel(name)
	currentLevel.Set(lvl)
	return lvl
}

// CurrentLevel returns the level currently in effect. Useful for
// asserting in tests and for the shadow handler's "applied" log line.
func CurrentLevel() slog.Level {
	return currentLevel.Level()
}

// ParseLevel converts a level string to slog.Level. Unrecognized values
// default to slog.LevelInfo.
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "trace":
		return LevelTrace
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	case "info", "":
		return slog.LevelInfo
	default:
		return slog.LevelInfo
	}
}
