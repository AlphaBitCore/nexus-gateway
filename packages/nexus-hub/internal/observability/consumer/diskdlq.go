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

// defaultDiskDLQMaxBytes bounds one dead-letter file.
//
// The file had NO bound at all: it is append-only, holds raw MQ payloads
// (request/response bodies, source_ip, end_user_id) and nothing ever
// truncated or rotated it, so a sustained DB outage grew it until the host
// ran out of disk — and every byte written stayed for the life of the host.
//
// Refusing past the cap rather than rotating is deliberate, and it is the
// behaviour append already documents: an error means the caller keeps the
// message on the BROKER, which is itself durable and bounded by the broker's
// own retention. Rotating would silently discard the oldest dead letters,
// which is the one thing this sink exists to prevent. So the failure mode is
// backpressure, not loss.
const defaultDiskDLQMaxBytes = 256 << 20 // 256 MiB

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
	maxBytes int64

	mu sync.Mutex
	f  *os.File
	// size tracks the file's byte count so the cap costs one Stat at open
	// rather than one per append.
	size int64
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
	return &diskDLQ{dir: dir, fileName: fileName, maxBytes: defaultDiskDLQMaxBytes}
}

// append durably records one dead-letter entry. Returns an error only when the
// record could not be persisted at all (mkdir/open/marshal/write failure, or
// the file is at its size cap), in which case the caller falls back to keeping
// the message on the broker.
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
		// Adopt the on-disk size so a restart does not reset the cap and let
		// an already-full file keep growing.
		if st, err := f.Stat(); err == nil {
			d.size = st.Size()
		}
	}

	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("disk-dlq marshal: %w", err)
	}
	line = append(line, '\n')
	if d.maxBytes > 0 && d.size+int64(len(line)) > d.maxBytes {
		return fmt.Errorf("disk-dlq full: %s is at its %d-byte cap; message stays on the broker",
			d.path(), d.maxBytes)
	}
	n, err := d.f.Write(line)
	d.size += int64(n)
	if err != nil {
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
