package consumer

import (
	"fmt"
	"github.com/goccy/go-json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// diskDLQFileName is the append-only JSON-Lines file the traffic-event on-disk
// dead-letter queue writes to. One JSON object per line.
const diskDLQFileName = "traffic-event-dlq.jsonl"

// adminAuditDLQFileName is the on-disk dead-letter file for the admin-audit
// consumer, so a message that fails to deserialize is captured durably for
// replay instead of being acked + dropped.
const adminAuditDLQFileName = "admin-audit-dlq.jsonl"

// defaultDiskDLQDir is where the on-disk DLQ is written when the writer is
// constructed without an explicit directory. It is a last-resort durability
// sink used ONLY when the DB-backed traffic_event_dlq insert itself fails
// (i.e. the database is unreachable), so a full DB outage can no longer
// silently drop billing/audit rows at the redelivery cap. Operators replay the
// file into traffic_event_dlq once the DB recovers (see the audit-pipeline
// architecture doc, §10.1).
func defaultDiskDLQDir() string {
	return filepath.Join(os.TempDir(), "nexus-hub-dlq")
}

// diskDLQRecord is one persisted dead-letter entry. Payload is the raw MQ
// message bytes; encoding/json base64-encodes []byte, so binary/SSE payloads
// round-trip cleanly. The shape intentionally mirrors the traffic_event_dlq
// columns (msg_id, subject, payload, delivery_count, last_error) so a replay
// is a straight column map.
type diskDLQRecord struct {
	MsgID         string    `json:"msgId"`
	Subject       string    `json:"subject"`
	Payload       []byte    `json:"payload"`
	DeliveryCount int       `json:"deliveryCount"`
	LastError     string    `json:"lastError,omitempty"`
	WrittenAt     time.Time `json:"writtenAt"`
}

// diskDLQ is an append-only, on-disk dead-letter sink that is independent of
// the database. It exists so a message that hits the redelivery cap during a
// DB outage — when the DB-backed insertDLQ also fails — is still captured
// durably instead of being Nak'd into MaxDeliver exhaustion and purged.
//
// It is deliberately tiny: open-on-first-write, one mutex-guarded *os.File,
// one JSON line per record, fsync-free (the OS page cache plus the broker's
// own retry are the redundancy). Concurrency is safe across the three writer
// goroutines because every append holds the mutex.
type diskDLQ struct {
	dir      string
	fileName string

	mu sync.Mutex
	f  *os.File
}

// newDiskDLQ returns a disk DLQ rooted at dir writing the default traffic
// file name. The directory is created lazily on the first append so
// construction never touches the filesystem.
func newDiskDLQ(dir string) *diskDLQ {
	return newDiskDLQNamed(dir, diskDLQFileName)
}

// newDiskDLQNamed returns a disk DLQ rooted at dir writing to fileName. Each
// consumer that needs a DB-independent dead-letter sink (traffic, admin-audit,
// SIEM) uses a distinct file name so a replay is unambiguous about which
// consumer dropped the message.
func newDiskDLQNamed(dir, fileName string) *diskDLQ {
	if dir == "" {
		dir = defaultDiskDLQDir()
	}
	if fileName == "" {
		fileName = diskDLQFileName
	}
	return &diskDLQ{dir: dir, fileName: fileName}
}

// append durably records one dead-letter entry. Returns an error only when the
// record could not be persisted at all (mkdir/open/marshal/write failure), in
// which case the caller falls back to keeping the message on the broker.
func (d *diskDLQ) append(rec diskDLQRecord) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.f == nil {
		if err := os.MkdirAll(d.dir, 0o700); err != nil {
			return fmt.Errorf("disk-dlq mkdir %s: %w", d.dir, err)
		}
		f, err := os.OpenFile(
			filepath.Join(d.dir, d.fileName),
			os.O_APPEND|os.O_CREATE|os.O_WRONLY,
			0o600,
		)
		if err != nil {
			return fmt.Errorf("disk-dlq open: %w", err)
		}
		d.f = f
	}

	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("disk-dlq marshal: %w", err)
	}
	line = append(line, '\n')
	if _, err := d.f.Write(line); err != nil {
		return fmt.Errorf("disk-dlq write: %w", err)
	}
	return nil
}

// path returns the full path of the JSON-Lines file (for logging/replay).
func (d *diskDLQ) path() string {
	return filepath.Join(d.dir, d.fileName)
}

// close releases the underlying file handle. Safe to call on a diskDLQ that
// never opened a file.
func (d *diskDLQ) close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.f == nil {
		return nil
	}
	err := d.f.Close()
	d.f = nil
	return err
}
