package consumer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDiskDLQ_AppendCreatesDirAndAccumulates verifies the on-disk DLQ lazily
// creates its directory + file on first append and accumulates one JSON line
// per record (the reuse-open-handle path on the second append).
func TestDiskDLQ_AppendCreatesDirAndAccumulates(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "dlq") // not yet created
	d := newDiskDLQ(dir)
	t.Cleanup(func() { _ = d.close() })

	rec := func(id string) diskDLQRecord {
		return diskDLQRecord{MsgID: id, Subject: "nexus.event.gateway", Payload: []byte(`{"id":"` + id + `"}`), DeliveryCount: 3, WrittenAt: time.Now().UTC()}
	}
	if err := d.append(rec("a")); err != nil {
		t.Fatalf("append a: %v", err)
	}
	if err := d.append(rec("b")); err != nil {
		t.Fatalf("append b (handle reuse): %v", err)
	}

	recs := readDiskDLQ(t, d.path())
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2", len(recs))
	}
	if recs[0].MsgID != "a" || recs[1].MsgID != "b" {
		t.Errorf("append order = %q,%q, want a,b", recs[0].MsgID, recs[1].MsgID)
	}
	if string(recs[0].Payload) != `{"id":"a"}` {
		t.Errorf("payload round-trip = %q", recs[0].Payload)
	}
}

// TestDiskDLQ_DefaultDirWhenEmpty pins that an empty dir falls back to the
// package default.
func TestDiskDLQ_DefaultDirWhenEmpty(t *testing.T) {
	d := newDiskDLQ("")
	if d.dir != defaultDiskDLQDir() {
		t.Errorf("dir = %q, want default %q", d.dir, defaultDiskDLQDir())
	}
}

// TestDiskDLQ_CloseIdempotent covers close() both before any file is opened
// (no-op) and after — and a double close.
func TestDiskDLQ_CloseIdempotent(t *testing.T) {
	d := newDiskDLQ(t.TempDir())
	if err := d.close(); err != nil {
		t.Errorf("close before open: got %v, want nil", err)
	}
	if err := d.append(diskDLQRecord{MsgID: "x", WrittenAt: time.Now()}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := d.close(); err != nil {
		t.Errorf("close after open: %v", err)
	}
	if err := d.close(); err != nil {
		t.Errorf("double close: got %v, want nil", err)
	}
}

// TestDiskDLQ_AppendOpenFailure pins the open-failure branch: when the target
// file path is itself a directory, OpenFile fails and append returns an error.
func TestDiskDLQ_AppendOpenFailure(t *testing.T) {
	dir := t.TempDir()
	// Create a DIRECTORY where the JSON-Lines file should be, so O_WRONLY open
	// of that path fails.
	if err := os.MkdirAll(filepath.Join(dir, diskDLQFileName), 0o700); err != nil {
		t.Fatalf("seed dir-as-file: %v", err)
	}
	d := newDiskDLQ(dir)
	err := d.append(diskDLQRecord{MsgID: "x", WrittenAt: time.Now()})
	if err == nil || !strings.Contains(err.Error(), "disk-dlq open") {
		t.Errorf("got err=%v, want wrapped 'disk-dlq open'", err)
	}
}

// TestDiskDLQ_AppendMkdirFailure pins the mkdir-failure branch: the parent of
// the DLQ dir is a regular file, so MkdirAll fails.
func TestDiskDLQ_AppendMkdirFailure(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed blocker: %v", err)
	}
	d := newDiskDLQ(filepath.Join(blocker, "sub"))
	err := d.append(diskDLQRecord{MsgID: "x", WrittenAt: time.Now()})
	if err == nil || !strings.Contains(err.Error(), "disk-dlq mkdir") {
		t.Errorf("got err=%v, want wrapped 'disk-dlq mkdir'", err)
	}
}
