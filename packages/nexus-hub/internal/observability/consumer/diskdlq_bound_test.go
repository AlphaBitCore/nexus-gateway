package consumer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The disk DLQ file had NO bound: append-only, holding raw MQ payloads
// (request/response bodies, source_ip, end_user_id), with nothing that ever
// truncated or rotated it. A sustained DB outage grew it until the host ran
// out of disk, and every byte written stayed for the life of the host.
//
// Refusing past the cap is the behaviour `append` already documents — an
// error means the caller keeps the message on the BROKER, which is itself
// durable. Rotating instead would silently discard the oldest dead letters,
// which is the one thing this sink exists to prevent.

func TestDiskDLQ_RefusesPastItsSizeCap(t *testing.T) {
	d := newDiskDLQ(t.TempDir())
	d.maxBytes = 512

	rec := diskDLQRecord{
		MsgID:     "m1",
		Subject:   "nexus.event.traffic",
		Payload:   []byte(strings.Repeat("x", 200)),
		WrittenAt: time.Unix(0, 0).UTC(),
	}

	// Non-vacuity: the first write must succeed, or "it refused" would prove
	// nothing about the cap.
	if err := d.append(rec); err != nil {
		t.Fatalf("first append must succeed: %v", err)
	}

	var lastErr error
	writes := 1
	for range 50 {
		if err := d.append(rec); err != nil {
			lastErr = err
			break
		}
		writes++
	}
	if lastErr == nil {
		t.Fatalf("appended %d records with a 512-byte cap and never refused; the file is unbounded", writes)
	}
	if !strings.Contains(lastErr.Error(), "disk-dlq full") {
		t.Errorf("refusal must name the cap so the caller can tell it from an I/O error; got %v", lastErr)
	}

	st, err := os.Stat(filepath.Join(d.dir, d.fileName))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if st.Size() > d.maxBytes {
		t.Errorf("file grew to %d bytes past its %d-byte cap", st.Size(), d.maxBytes)
	}
}

// A restart must not reset the accounting and let an already-full file keep
// growing — the cap has to be adopted from what is on disk.
func TestDiskDLQ_AdoptsTheOnDiskSizeAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	rec := diskDLQRecord{
		MsgID:     "m1",
		Subject:   "nexus.event.traffic",
		Payload:   []byte(strings.Repeat("x", 200)),
		WrittenAt: time.Unix(0, 0).UTC(),
	}

	first := newDiskDLQ(dir)
	first.maxBytes = 512
	for range 50 {
		if err := first.append(rec); err != nil {
			break
		}
	}
	beforeStat, err := os.Stat(filepath.Join(dir, diskDLQFileName))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if beforeStat.Size() == 0 {
		t.Fatal("the first writer wrote nothing; this test cannot show anything")
	}
	_ = first.close()

	// A fresh writer over the same directory, as a restarted process would be.
	second := newDiskDLQ(dir)
	second.maxBytes = 512
	if err := second.append(rec); err == nil {
		afterStat, statErr := os.Stat(filepath.Join(dir, diskDLQFileName))
		if statErr != nil {
			t.Fatalf("stat: %v", statErr)
		}
		t.Errorf("a restarted writer appended to an already-full file (%d -> %d bytes); "+
			"the cap resets on every restart and the file is effectively unbounded",
			beforeStat.Size(), afterStat.Size())
	}
}
