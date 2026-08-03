package config

// Audit-pipeline configuration.
//
// Split out of config.go along the seam the overflow policy created: these three structs decide
// whether an audit record can be lost, which is a different kind of decision from the ports,
// timeouts and feature flags in config.go, and it is the one an operator most needs to read
// carefully. The file-size ratchet forced the split; the seam is real.

// AuditConfig controls audit event persistence and pinning behavior.
type AuditConfig struct {
	Enabled bool             `yaml:"enabled"`
	Batch   AuditBatchConfig `yaml:"batch"`
	NDJSON  NDJSONConfig     `yaml:"ndjson"`
	Pinning PinningCfg       `yaml:"pinning"`

	// LossMode is the audit-overflow policy, from shared/audit/lossmode:
	// "spillblock" (default, no-loss — spool first, back-pressure only when the
	// spool is also saturated), "block" (no-loss back-pressure, never spools),
	// "spill" (durable spool, counted drop only if the spool is unavailable), or
	// "drop" (counted bounded drop; audit-lossy, non-compliance callers only).
	//
	// Empty or unrecognised resolves to the no-loss default — a config typo must
	// never be able to make the audit trail silently start dropping records. The
	// durable sink for every spooling mode is the NDJSON writer below; with it
	// disabled, spillblock degrades to block and spill degrades to drop, and
	// startup says so.
	LossMode string `yaml:"lossMode"`
}

// AuditBatchConfig controls async batch write behavior for the MQ writer.
type AuditBatchConfig struct {
	Size              int `yaml:"size"`
	FlushIntervalMs   int `yaml:"flushIntervalMs"`
	ChannelBufferSize int `yaml:"channelBufferSize"`
}

// NDJSONConfig controls the NDJSON fallback writer.
type NDJSONConfig struct {
	Enabled        bool   `yaml:"enabled"`
	Dir            string `yaml:"dir"`
	MaxFileSizeMB  int    `yaml:"maxFileSizeMB"`
	MaxTotalSizeMB int    `yaml:"maxTotalSizeMB"`
}
