package audit

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// NDJSONWriter writes audit events to local NDJSON files as a fallback
// when the primary database writer is unavailable or the queue overflows.
type NDJSONWriter struct {
	dir          string
	instanceID   string
	maxFileSize  int64
	maxTotalSize int64
	logger       *slog.Logger

	mu          sync.Mutex
	currentFile *os.File
	currentSize int64
	sequence    int
}

// NewNDJSONWriter creates an NDJSON writer with the given spool directory and limits.
// It creates the instance-specific subdirectory if it does not exist.
func NewNDJSONWriter(dir, instanceID string, maxFileSizeMB, maxTotalSizeMB int, logger *slog.Logger) (*NDJSONWriter, error) {
	instanceDir := filepath.Join(dir, instanceID)
	if err := os.MkdirAll(instanceDir, 0700); err != nil {
		return nil, fmt.Errorf("audit/ndjson: create spool directory %s: %w", instanceDir, err)
	}

	return &NDJSONWriter{
		dir:          dir,
		instanceID:   instanceID,
		maxFileSize:  int64(maxFileSizeMB) * 1024 * 1024,
		maxTotalSize: int64(maxTotalSizeMB) * 1024 * 1024,
		logger:       logger,
	}, nil
}

// Write appends a single audit event as a JSON line to the current spool file.
// It handles file rotation when the current file exceeds maxFileSize and refuses
// to write when the total directory size exceeds maxTotalSize.
func (w *NDJSONWriter) Write(event AuditEvent) error {
	data, err := json.Marshal(eventToMap(event))
	if err != nil {
		return fmt.Errorf("audit/ndjson: marshal event: %w", err)
	}
	data = append(data, '\n')

	w.mu.Lock()
	defer w.mu.Unlock()

	// Check total directory size quota.
	totalSize, err := w.dirSize()
	if err != nil {
		w.logger.Warn("audit/ndjson: failed to compute directory size", "error", err)
		// Continue anyway; better to write than to lose data over a stat error.
	} else if totalSize >= w.maxTotalSize {
		w.logger.Error("audit/ndjson: total spool quota exceeded, dropping event",
			"totalSize", totalSize, "maxTotalSize", w.maxTotalSize)
		return fmt.Errorf("audit/ndjson: total spool size %d exceeds quota %d", totalSize, w.maxTotalSize)
	}

	// Rotate if the current file exceeds max file size.
	if w.currentFile != nil && w.currentSize+int64(len(data)) > w.maxFileSize {
		if err := w.rotateFile(); err != nil {
			return err
		}
	}

	// Open a new file if needed.
	if w.currentFile == nil {
		if err := w.openNewFile(); err != nil {
			return err
		}
	}

	n, err := w.currentFile.Write(data)
	if err != nil {
		return fmt.Errorf("audit/ndjson: write: %w", err)
	}
	w.currentSize += int64(n)

	if NDJSONWrites != nil {
		NDJSONWrites.With().Inc()
	}
	if NDJSONBytes != nil {
		NDJSONBytes.With().Add(float64(n))
	}

	return nil
}

// Close closes the current file handle.
func (w *NDJSONWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.currentFile != nil {
		err := w.currentFile.Close()
		w.currentFile = nil
		return err
	}
	return nil
}

// openNewFile creates a new NDJSON spool file with the naming convention:
// {dir}/{instanceID}/audit-{YYYYMMDD}-{sequence:04d}.ndjson
func (w *NDJSONWriter) openNewFile() error {
	w.sequence++
	name := fmt.Sprintf("audit-%s-%04d.ndjson", time.Now().UTC().Format("20060102"), w.sequence)
	path := filepath.Join(w.dir, w.instanceID, name)

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return fmt.Errorf("audit/ndjson: open file %s: %w", path, err)
	}

	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("audit/ndjson: stat file %s: %w", path, err)
	}

	w.currentFile = f
	w.currentSize = info.Size()
	return nil
}

// rotateFile closes the current file so the next Write opens a new one.
func (w *NDJSONWriter) rotateFile() error {
	if w.currentFile != nil {
		if err := w.currentFile.Close(); err != nil {
			return fmt.Errorf("audit/ndjson: close for rotation: %w", err)
		}
		w.currentFile = nil
		w.currentSize = 0
	}
	return nil
}

// dirSize calculates the total size of all files in the instance spool directory.
func (w *NDJSONWriter) dirSize() (int64, error) {
	instanceDir := filepath.Join(w.dir, w.instanceID)
	entries, err := os.ReadDir(instanceDir)
	if err != nil {
		return 0, err
	}

	var total int64
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		total += info.Size()
	}
	return total, nil
}

// eventToMap converts an AuditEvent to a map for JSON marshalling,
// preserving all fields including nil-able ones.
func eventToMap(e AuditEvent) map[string]any {
	m := map[string]any{
		"id":                  e.ID,
		"transactionId":       e.TransactionID,
		"connectionId":        e.ConnectionID,
		"trafficSource":       e.TrafficSource,
		"ingressType":         e.IngressType,
		"bumpStatus":          e.BumpStatus,
		"sourceIp":            e.SourceIP,
		"targetHost":          e.TargetHost,
		"method":              e.Method,
		"path":                e.Path,
		"requestHookDecision": e.RequestHookDecision,
		"latencyMs":           e.LatencyMs,
		"timestamp":           e.Timestamp.Format(time.RFC3339Nano),
	}

	if e.StatusCode != nil {
		m["statusCode"] = *e.StatusCode
	}
	if e.RequestHookReason != nil {
		m["requestHookReason"] = *e.RequestHookReason
	}
	if e.RequestHookReasonCode != nil {
		m["requestHookReasonCode"] = *e.RequestHookReasonCode
	}
	if e.RequestHooksPipeline != nil {
		m["requestHooksPipeline"] = json.RawMessage(e.RequestHooksPipeline)
	}
	if e.ResponseHookDecision != nil {
		m["responseHookDecision"] = *e.ResponseHookDecision
	}
	if e.ResponseHookReason != nil {
		m["responseHookReason"] = *e.ResponseHookReason
	}
	if e.ResponseHookReasonCode != nil {
		m["responseHookReasonCode"] = *e.ResponseHookReasonCode
	}
	if e.ResponseHooksPipeline != nil {
		m["responseHooksPipeline"] = json.RawMessage(e.ResponseHooksPipeline)
	}
	if len(e.ComplianceTags) > 0 {
		m["complianceTags"] = e.ComplianceTags
	}
	if e.SubjectID != nil {
		m["subjectId"] = *e.SubjectID
	}
	if e.DSARDeleteRequested != nil {
		m["dsarDeleteRequested"] = *e.DSARDeleteRequested
	}
	if e.UserAgent != nil {
		m["userAgent"] = *e.UserAgent
	}

	return m
}
