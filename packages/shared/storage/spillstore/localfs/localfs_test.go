package localfs_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore/localfs"
)

// filepathSafe builds a unique-per-call event id so test puts don't overwrite
// each other (Put keys by event id + direction).
func filepathSafe(prefix string, i int) string { return fmt.Sprintf("%s-%d", prefix, i) }

func newStore(t *testing.T) (*localfs.Store, string) {
	t.Helper()
	root := t.TempDir()
	s, err := localfs.New(localfs.Options{Root: root, TotalSizeCap: 1 << 20, Retention: time.Hour})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, root
}

func TestStore_PutGetDelete(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()

	payload := []byte("event: delta\ndata: {\"hi\": \"你好\"}\n\n")
	ref, err := s.Put(ctx, bytes.NewReader(payload), int64(len(payload)), spillstore.PutOptions{
		EventID:     "evt-001",
		Direction:   "response",
		ContentType: "text/event-stream",
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if ref.Backend != "localfs" {
		t.Fatalf("backend: got %q want localfs", ref.Backend)
	}
	if ref.Size != int64(len(payload)) {
		t.Fatalf("size: got %d want %d", ref.Size, len(payload))
	}
	expectHash := audit.SHA256Hex(payload)
	if ref.SHA256 != expectHash {
		t.Fatalf("sha256: got %s want %s", ref.SHA256, expectHash)
	}

	rc, err := s.Get(ctx, ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("Get bytes mismatch: got %q want %q", got, payload)
	}

	if err := s.Delete(ctx, ref); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(ctx, ref); !errors.Is(err, spillstore.ErrNotFound) {
		t.Fatalf("Get after Delete: got %v want ErrNotFound", err)
	}
}

func TestStore_EncryptedRoundTrip(t *testing.T) {
	root := t.TempDir()
	key := bytes.Repeat([]byte("K"), 32)
	s, err := localfs.New(localfs.Options{Root: root, TotalSizeCap: 1 << 20, Retention: time.Hour, EncryptionKey: key})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	payload := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"secret prompt 机密"}]}`)

	ref, err := s.Put(ctx, bytes.NewReader(payload), int64(len(payload)), spillstore.PutOptions{
		EventID: "evt-enc", Direction: "request", ContentType: "application/json",
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Ref describes the PLAINTEXT (Hub + uploader hash the plaintext they PUT).
	if ref.Size != int64(len(payload)) {
		t.Errorf("size: got %d want plaintext %d", ref.Size, len(payload))
	}
	if ref.SHA256 != audit.SHA256Hex(payload) {
		t.Errorf("sha256 must be of plaintext, got %s", ref.SHA256)
	}

	// On-disk file must NOT contain the plaintext and must be larger (nonce+tag).
	onDisk, err := os.ReadFile(filepath.Join(root, ref.Key))
	if err != nil {
		t.Fatalf("read on-disk: %v", err)
	}
	if bytes.Contains(onDisk, payload) {
		t.Fatal("on-disk bytes contain plaintext — encryption did not apply")
	}
	if int64(len(onDisk)) <= ref.Size {
		t.Errorf("ciphertext (%d) should exceed plaintext (%d) by nonce+tag", len(onDisk), ref.Size)
	}

	// Get decrypts back to the exact plaintext.
	rc, err := s.Get(ctx, ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(got, payload) {
		t.Fatalf("decrypted mismatch: got %q want %q", got, payload)
	}
}

func TestStore_EncryptedWrongKeyFails(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()
	writer, _ := localfs.New(localfs.Options{Root: root, EncryptionKey: bytes.Repeat([]byte("A"), 32)})
	ref, err := writer.Put(ctx, strings.NewReader("sensitive body"), 14, spillstore.PutOptions{EventID: "evt-wrong", Direction: "r"})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	// A reader with a different key must fail GCM authentication, not return garbage.
	reader, _ := localfs.New(localfs.Options{Root: root, EncryptionKey: bytes.Repeat([]byte("B"), 32)})
	if _, err := reader.Get(ctx, ref); err == nil {
		t.Fatal("Get with wrong key should fail GCM auth, got nil error")
	}
}

func TestNew_BadKeyLength(t *testing.T) {
	if _, err := localfs.New(localfs.Options{Root: t.TempDir(), EncryptionKey: bytes.Repeat([]byte("x"), 16)}); err == nil {
		t.Fatal("New with 16-byte key should error (AES-256 needs 32)")
	}
}

func TestStore_EncryptedGet_ShortCiphertext(t *testing.T) {
	root := t.TempDir()
	s, _ := localfs.New(localfs.Options{Root: root, EncryptionKey: bytes.Repeat([]byte("C"), 32)})
	ctx := context.Background()
	ref, err := s.Put(ctx, strings.NewReader("hello world body"), 16, spillstore.PutOptions{EventID: "evt-short", Direction: "r"})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Corrupt the sealed file to be shorter than a GCM nonce → open must reject.
	if err := os.WriteFile(filepath.Join(root, ref.Key), []byte("xx"), 0o600); err != nil {
		t.Fatalf("corrupt: %v", err)
	}
	if _, err := s.Get(ctx, ref); err == nil {
		t.Fatal("Get on a too-short sealed file should fail, got nil")
	}
}

func TestStore_EncryptedPut_ReadError(t *testing.T) {
	s, _ := localfs.New(localfs.Options{Root: t.TempDir(), EncryptionKey: bytes.Repeat([]byte("D"), 32)})
	r := &failingReader{err: errors.New("synthetic stream failure")}
	if _, err := s.Put(context.Background(), r, 100, spillstore.PutOptions{EventID: "evt-erd", Direction: "r"}); err == nil {
		t.Fatal("encrypted Put with failing reader: want error, got nil")
	}
}

func TestStore_PerObjectCap(t *testing.T) {
	root := t.TempDir()
	s, err := localfs.New(localfs.Options{Root: root, PerObjectCap: 16})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	payload := bytes.Repeat([]byte("X"), 1024)
	ref, err := s.Put(context.Background(), bytes.NewReader(payload), int64(len(payload)), spillstore.PutOptions{EventID: "e", Direction: "request"})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if ref.Size != 16 {
		t.Fatalf("expected truncation to 16, got %d", ref.Size)
	}
}

func TestStore_SweepRetention(t *testing.T) {
	s, root := newStore(t)
	ctx := context.Background()

	for range 3 {
		_, err := s.Put(ctx, strings.NewReader("hello"), 5, spillstore.PutOptions{EventID: "old", Direction: "r"})
		if err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	// Backdate the file mtimes to past retention.
	walked := 0
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil //nolint:nilerr // test fixture — best-effort backdating
		}
		past := time.Now().Add(-48 * time.Hour)
		_ = os.Chtimes(path, past, past)
		walked++
		return nil
	})
	if walked == 0 {
		t.Fatal("no spill files on disk to backdate")
	}

	deleted, err := s.Sweep(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if deleted == 0 {
		t.Fatal("expected at least one deletion under retention sweep")
	}
}

func TestStore_SweepTotalCap(t *testing.T) {
	root := t.TempDir()
	s, err := localfs.New(localfs.Options{Root: root, TotalSizeCap: 12, Retention: 24 * time.Hour})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	for i := range 5 {
		payload := bytes.Repeat([]byte("A"), 8)
		_, err := s.Put(ctx, bytes.NewReader(payload), int64(len(payload)), spillstore.PutOptions{
			EventID:   filepathSafe("evt", i),
			Direction: "x",
		})
		if err != nil {
			t.Fatalf("Put: %v", err)
		}
		// Stagger mtimes so Sweep can pick "oldest first" deterministically.
		time.Sleep(2 * time.Millisecond)
	}

	deleted, err := s.Sweep(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if deleted == 0 {
		t.Fatal("expected total-cap sweep to evict files")
	}
	stat, err := s.Stat(ctx)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if stat.TotalBytes > 12 {
		t.Fatalf("total cap not enforced: %d > 12", stat.TotalBytes)
	}
}

// stubFilter is a spillstore.SweepFilter whose result the test controls. It
// records the candidate keys it was handed so tests can assert the sweep
// presents the right batch.
type stubFilter struct {
	referenced map[string]bool
	err        error
	gotKeys    []string
}

func (s *stubFilter) KeepReferenced(_ context.Context, candidateKeys []string) (map[string]bool, error) {
	s.gotKeys = append([]string(nil), candidateKeys...)
	return s.referenced, s.err
}

// seedAged writes n spill objects, backdates their mtimes past retention, and
// returns their storage keys in put order.
func seedAged(t *testing.T, s *localfs.Store, root string, n int) []string {
	t.Helper()
	ctx := context.Background()
	keys := make([]string, 0, n)
	for i := range n {
		ref, err := s.Put(ctx, strings.NewReader("payload-data"), 12, spillstore.PutOptions{
			EventID:   filepathSafe("evt", i),
			Direction: "r",
		})
		if err != nil {
			t.Fatalf("Put: %v", err)
		}
		keys = append(keys, ref.Key)
	}
	past := time.Now().Add(-48 * time.Hour)
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil //nolint:nilerr // test fixture — best-effort backdating
		}
		_ = os.Chtimes(path, past, past)
		return nil
	})
	return keys
}

// TestSweepFiltered_AllReferenced — every aged blob is still referenced, so
// the reference-checked sweep deletes nothing (F-0187: never orphan a live
// spill).
func TestSweepFiltered_AllReferenced(t *testing.T) {
	s, root := newStore(t)
	keys := seedAged(t, s, root, 3)
	ref := map[string]bool{}
	for _, k := range keys {
		ref[k] = true
	}
	f := &stubFilter{referenced: ref}

	deleted, err := s.SweepFiltered(context.Background(), time.Now().Add(-time.Hour), f)
	if err != nil {
		t.Fatalf("SweepFiltered: %v", err)
	}
	if deleted != 0 {
		t.Errorf("deleted = %d, want 0 (all referenced)", deleted)
	}
	stat, _ := s.Stat(context.Background())
	if stat.ObjectCount != 3 {
		t.Errorf("ObjectCount = %d, want 3 (nothing deleted)", stat.ObjectCount)
	}
}

// TestSweepFiltered_NoneReferenced — no aged blob is referenced, so the sweep
// deletes them all (the age-based behaviour, now reference-confirmed safe).
func TestSweepFiltered_NoneReferenced(t *testing.T) {
	s, root := newStore(t)
	keys := seedAged(t, s, root, 3)
	f := &stubFilter{referenced: map[string]bool{}}

	deleted, err := s.SweepFiltered(context.Background(), time.Now().Add(-time.Hour), f)
	if err != nil {
		t.Fatalf("SweepFiltered: %v", err)
	}
	if deleted != 3 {
		t.Errorf("deleted = %d, want 3 (none referenced)", deleted)
	}
	// The filter must have been handed exactly the three aged keys.
	if len(f.gotKeys) != len(keys) {
		t.Errorf("filter saw %d candidate keys, want %d", len(f.gotKeys), len(keys))
	}
	stat, _ := s.Stat(context.Background())
	if stat.ObjectCount != 0 {
		t.Errorf("ObjectCount = %d, want 0 (all deleted)", stat.ObjectCount)
	}
}

// TestSweepFiltered_Mixed — only the unreferenced subset is deleted; the
// referenced blob survives the age sweep.
func TestSweepFiltered_Mixed(t *testing.T) {
	s, root := newStore(t)
	keys := seedAged(t, s, root, 3)
	keep := keys[1] // mark the middle one as still referenced
	f := &stubFilter{referenced: map[string]bool{keep: true}}

	deleted, err := s.SweepFiltered(context.Background(), time.Now().Add(-time.Hour), f)
	if err != nil {
		t.Fatalf("SweepFiltered: %v", err)
	}
	if deleted != 2 {
		t.Errorf("deleted = %d, want 2 (one referenced)", deleted)
	}
	// The referenced object must still be readable.
	rc, err := s.Get(context.Background(), audit.SpillRef{Backend: "localfs", Key: keep})
	if err != nil {
		t.Fatalf("referenced blob was deleted: %v", err)
	}
	_ = rc.Close()
}

// TestSweepFiltered_DBError — a reference-check failure aborts the sweep with
// zero deletions (fail-safe: a DB hiccup must never be read as "unreferenced").
func TestSweepFiltered_DBError(t *testing.T) {
	s, root := newStore(t)
	seedAged(t, s, root, 3)
	f := &stubFilter{err: errors.New("db unreachable")}

	deleted, err := s.SweepFiltered(context.Background(), time.Now().Add(-time.Hour), f)
	if err == nil {
		t.Fatal("expected reference-check error to abort the sweep")
	}
	if deleted != 0 {
		t.Errorf("deleted = %d, want 0 on reference-check failure", deleted)
	}
	if !strings.Contains(err.Error(), "reference check") {
		t.Errorf("error %q does not mention the reference check", err.Error())
	}
	stat, _ := s.Stat(context.Background())
	if stat.ObjectCount != 3 {
		t.Errorf("ObjectCount = %d, want 3 (nothing deleted on error)", stat.ObjectCount)
	}
}

// TestSweepFiltered_CapSkipsReferenced — the total-size cap pass must also
// skip a still-referenced blob even when evicting oldest-first to fit the cap.
func TestSweepFiltered_CapSkipsReferenced(t *testing.T) {
	root := t.TempDir()
	// Cap of 12 bytes (one object); each object is 8 bytes. Long retention so
	// only the cap pass — not the age pass — does the eviction.
	s, err := localfs.New(localfs.Options{Root: root, TotalSizeCap: 12, Retention: 24 * time.Hour})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	var keys []string
	for i := range 3 {
		payload := bytes.Repeat([]byte("A"), 8)
		ref, perr := s.Put(ctx, bytes.NewReader(payload), int64(len(payload)), spillstore.PutOptions{
			EventID:   filepathSafe("cap", i),
			Direction: "x",
		})
		if perr != nil {
			t.Fatalf("Put: %v", perr)
		}
		keys = append(keys, ref.Key)
		time.Sleep(2 * time.Millisecond) // stagger mtimes for oldest-first
	}
	// Reference the OLDEST object — the one the cap pass would evict first.
	f := &stubFilter{referenced: map[string]bool{keys[0]: true}}

	if _, err := s.SweepFiltered(ctx, time.Now().Add(-time.Hour), f); err != nil {
		t.Fatalf("SweepFiltered: %v", err)
	}
	// The referenced oldest blob must survive even though the cap forced
	// eviction of others.
	rc, err := s.Get(ctx, audit.SpillRef{Backend: "localfs", Key: keys[0]})
	if err != nil {
		t.Fatalf("referenced oldest blob was cap-evicted: %v", err)
	}
	_ = rc.Close()
}

// TestNew_EmptyRoot rejects construction without a Root — the only required
// option. We assert both the error and a nil Store (never partial init).
func TestNew_EmptyRoot(t *testing.T) {
	s, err := localfs.New(localfs.Options{})
	if err == nil {
		t.Fatal("New with empty Root: want error, got nil")
	}
	if s != nil {
		t.Fatalf("New with empty Root: want nil Store, got %#v", s)
	}
	if !strings.Contains(err.Error(), "Root is required") {
		t.Fatalf("New error: want %q, got %q", "Root is required", err.Error())
	}
}

// TestNew_MkdirAllFails forces the os.MkdirAll branch to fail by placing the
// requested Root underneath a regular file (not a directory). os.MkdirAll
// returns ENOTDIR which we surface wrapped as "ensure root".
func TestNew_MkdirAllFails(t *testing.T) {
	dir := t.TempDir()
	notADir := filepath.Join(dir, "blocker")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed blocker file: %v", err)
	}
	// Asking MkdirAll to create blocker/child fails because blocker is a file.
	root := filepath.Join(notADir, "child")
	s, err := localfs.New(localfs.Options{Root: root})
	if err == nil {
		t.Fatal("New with un-creatable Root: want error, got nil")
	}
	if s != nil {
		t.Fatalf("New with un-creatable Root: want nil Store, got %#v", s)
	}
	if !strings.Contains(err.Error(), "ensure root") {
		t.Fatalf("New error: want wrap %q, got %q", "ensure root", err.Error())
	}
}

// TestNew_DefaultsApplied verifies that zero-valued Options fields are
// replaced by the documented defaults. The defaults are load-bearing — they
// control prod-disk usage caps and retention.
func TestNew_DefaultsApplied(t *testing.T) {
	s, err := localfs.New(localfs.Options{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Backend() also doubles as a non-zero-value indicator; assert it here.
	if got := s.Backend(); got != localfs.BackendName {
		t.Fatalf("Backend(): got %q want %q", got, localfs.BackendName)
	}
}

// TestStore_Backend asserts the canonical backend identifier; it's stamped
// into every SpillRef and the Hub mint endpoint dispatches on it.
func TestStore_Backend(t *testing.T) {
	s, _ := newStore(t)
	if got := s.Backend(); got != "localfs" {
		t.Fatalf("Backend(): got %q want %q", got, "localfs")
	}
}

// TestStore_KeyFor verifies the date-prefixed key layout used by Hub's
// presign mint flow. The format <yyyy-mm-dd>/<event>-<direction>.bin is
// load-bearing for retention sweeps (prune by directory).
func TestStore_KeyFor(t *testing.T) {
	s, _ := newStore(t)
	at := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	got := s.KeyFor(at, "evt-42", "request")
	want := filepath.Join("2026-05-17", "evt-42-request.bin")
	if got != want {
		t.Fatalf("KeyFor: got %q want %q", got, want)
	}
}

// TestStore_PresignPut_NotSupported guarantees the sentinel error so Hub
// falls back to the in-Hub /spill/blob/:token handler. If this ever returns
// nil, Hub would hand callers an empty URL.
func TestStore_PresignPut_NotSupported(t *testing.T) {
	s, _ := newStore(t)
	url, err := s.PresignPut(context.Background(), "k", 0, "", time.Minute)
	if !errors.Is(err, spillstore.ErrPresignNotSupported) {
		t.Fatalf("PresignPut: want ErrPresignNotSupported, got %v", err)
	}
	if url != "" {
		t.Fatalf("PresignPut: want empty url, got %q", url)
	}
}

// TestStore_Put_EmptyEventID rejects spilling without an event id — the
// audit row key would collide otherwise.
func TestStore_Put_EmptyEventID(t *testing.T) {
	s, _ := newStore(t)
	_, err := s.Put(context.Background(), strings.NewReader("x"), 1, spillstore.PutOptions{Direction: "r"})
	if err == nil {
		t.Fatal("Put with empty EventID: want error, got nil")
	}
	if !strings.Contains(err.Error(), "EventID is required") {
		t.Fatalf("Put error: want %q, got %q", "EventID is required", err.Error())
	}
}

// TestStore_Put_DefaultDirection covers the `direction == ""` branch where
// the store substitutes "body". The substitution is observable through the
// resulting Ref.Key (the path encodes the direction).
func TestStore_Put_DefaultDirection(t *testing.T) {
	s, _ := newStore(t)
	ref, err := s.Put(context.Background(), strings.NewReader("xx"), 2, spillstore.PutOptions{EventID: "evt-default-dir"})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if !strings.HasSuffix(ref.Key, "-body.bin") {
		t.Fatalf("Put with empty Direction: want key suffix -body.bin, got %q", ref.Key)
	}
}

// TestStore_Put_TruncationFlag asserts Truncated=true is stamped whenever
// the upstream reader had more bytes than the per-object cap. Audit rows
// rely on this flag to flag clipped bodies in the UI.
func TestStore_Put_TruncationFlag(t *testing.T) {
	root := t.TempDir()
	s, err := localfs.New(localfs.Options{Root: root, PerObjectCap: 4})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	payload := []byte("0123456789") // 10 bytes vs cap 4
	ref, err := s.Put(context.Background(), bytes.NewReader(payload), int64(len(payload)), spillstore.PutOptions{EventID: "evt-tr", Direction: "r"})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if !ref.Truncated {
		t.Fatal("Put past cap: want Truncated=true, got false")
	}
	if ref.Size != 4 {
		t.Fatalf("Put past cap: want Size=4 (cap), got %d", ref.Size)
	}
}

// TestStore_Put_CopyError wires an io.Reader that fails mid-read to drive
// the io.Copy error branch. We assert the error is wrapped as "copy:" so
// callers can tell the failure was during the body stream — distinct from
// rename / close failures.
func TestStore_Put_CopyError(t *testing.T) {
	s, _ := newStore(t)
	r := &failingReader{err: errors.New("synthetic stream failure")}
	_, err := s.Put(context.Background(), r, 100, spillstore.PutOptions{EventID: "evt-fail", Direction: "r"})
	if err == nil {
		t.Fatal("Put with failing reader: want error, got nil")
	}
	if !strings.Contains(err.Error(), "copy") {
		t.Fatalf("Put copy error: want %q wrap, got %q", "copy", err.Error())
	}
}

// TestStore_Put_MkdirDayFails forces the per-day os.MkdirAll branch by
// replacing today's day-directory location with a regular file BEFORE Put
// is called. MkdirAll then sees a non-directory at the target path and
// returns ENOTDIR.
func TestStore_Put_MkdirDayFails(t *testing.T) {
	root := t.TempDir()
	s, err := localfs.New(localfs.Options{Root: root})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Block today's date directory by creating a regular file at that path.
	day := time.Now().UTC().Format("2006-01-02")
	blocker := filepath.Join(root, day)
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed day blocker: %v", err)
	}
	_, err = s.Put(context.Background(), strings.NewReader("data"), 4, spillstore.PutOptions{EventID: "evt-mkdir", Direction: "r"})
	if err == nil {
		t.Fatal("Put with blocked day dir: want error, got nil")
	}
	if !strings.Contains(err.Error(), "mkdir") {
		t.Fatalf("Put mkdir error: want %q wrap, got %q", "mkdir", err.Error())
	}
}

// TestStore_Get_BackendMismatch guards against cross-backend ref leakage —
// if a caller passes an s3 ref to a localfs store, we must refuse rather
// than try to open a path that happens to collide.
func TestStore_Get_BackendMismatch(t *testing.T) {
	s, _ := newStore(t)
	_, err := s.Get(context.Background(), audit.SpillRef{Backend: "s3", Key: "whatever"})
	if err == nil {
		t.Fatal("Get with foreign backend: want error, got nil")
	}
	if !strings.Contains(err.Error(), `"s3"`) || !strings.Contains(err.Error(), `"localfs"`) {
		t.Fatalf("Get error: want both backend names, got %q", err.Error())
	}
}

// TestStore_Get_NotFound covers the os.IsNotExist branch when the file
// referenced has never existed (separate path from the Put-then-Delete
// flow already covered in TestStore_PutGetDelete).
func TestStore_Get_NotFound(t *testing.T) {
	s, _ := newStore(t)
	_, err := s.Get(context.Background(), audit.SpillRef{Backend: "localfs", Key: "2026-05-17/never-existed-body.bin"})
	if !errors.Is(err, spillstore.ErrNotFound) {
		t.Fatalf("Get unknown key: want ErrNotFound, got %v", err)
	}
}

// TestStore_Get_OpenError exercises the non-IsNotExist Open error path.
// We point Get at a directory (with a .bin name) — os.Open succeeds but
// fails differently on some platforms; portable trick: write a file then
// chmod its parent to 0 so Open returns EACCES (skipped on Windows + root).
func TestStore_Get_OpenError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission bits")
	}
	root := t.TempDir()
	s, err := localfs.New(localfs.Options{Root: root})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ref, err := s.Put(context.Background(), strings.NewReader("data"), 4, spillstore.PutOptions{EventID: "evt-perm", Direction: "r"})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Remove read permission from the day directory.
	dayDir := filepath.Dir(filepath.Join(root, ref.Key))
	if err := os.Chmod(dayDir, 0o000); err != nil {
		t.Fatalf("chmod day: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dayDir, 0o700) })

	_, err = s.Get(context.Background(), ref)
	if err == nil {
		t.Fatal("Get with locked dir: want error, got nil")
	}
	if errors.Is(err, spillstore.ErrNotFound) {
		t.Fatalf("Get with locked dir: want non-NotFound error, got %v", err)
	}
	if !strings.Contains(err.Error(), "localfs.Get") {
		t.Fatalf("Get error: want %q wrap, got %q", "localfs.Get", err.Error())
	}
}

// TestStore_Delete_BackendMismatch — symmetric with Get's guard.
func TestStore_Delete_BackendMismatch(t *testing.T) {
	s, _ := newStore(t)
	err := s.Delete(context.Background(), audit.SpillRef{Backend: "s3", Key: "k"})
	if err == nil {
		t.Fatal("Delete with foreign backend: want error, got nil")
	}
	if !strings.Contains(err.Error(), `"s3"`) {
		t.Fatalf("Delete error: want s3 in message, got %q", err.Error())
	}
}

// TestStore_Delete_NotFound — deleting a never-existed ref returns
// ErrNotFound (caller treats this as "already gone", not fatal).
func TestStore_Delete_NotFound(t *testing.T) {
	s, _ := newStore(t)
	err := s.Delete(context.Background(), audit.SpillRef{Backend: "localfs", Key: "2026-05-17/ghost-body.bin"})
	if !errors.Is(err, spillstore.ErrNotFound) {
		t.Fatalf("Delete unknown key: want ErrNotFound, got %v", err)
	}
}

// TestStore_Delete_OtherError forces a non-IsNotExist Remove error by
// stripping write permission from the parent directory. Same root-skip
// rationale as TestStore_Get_OpenError.
func TestStore_Delete_OtherError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission bits")
	}
	root := t.TempDir()
	s, err := localfs.New(localfs.Options{Root: root})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ref, err := s.Put(context.Background(), strings.NewReader("data"), 4, spillstore.PutOptions{EventID: "evt-del-perm", Direction: "r"})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	dayDir := filepath.Dir(filepath.Join(root, ref.Key))
	// 0o500 = read+execute, no write — Remove of a child fails with EACCES.
	if err := os.Chmod(dayDir, 0o500); err != nil {
		t.Fatalf("chmod day: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dayDir, 0o700) })

	err = s.Delete(context.Background(), ref)
	if err == nil {
		t.Fatal("Delete with locked dir: want error, got nil")
	}
	if errors.Is(err, spillstore.ErrNotFound) {
		t.Fatalf("Delete with locked dir: want non-NotFound error, got %v", err)
	}
	if !strings.Contains(err.Error(), "localfs.Delete") {
		t.Fatalf("Delete error: want %q wrap, got %q", "localfs.Delete", err.Error())
	}
}

// TestStore_Sweep_PrunesEmptyDayDirs verifies the post-pass that removes
// empty day-directories so `find` / `du` stay tidy after retention.
func TestStore_Sweep_PrunesEmptyDayDirs(t *testing.T) {
	root := t.TempDir()
	s, err := localfs.New(localfs.Options{Root: root, TotalSizeCap: 1 << 20, Retention: time.Hour})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	ref, err := s.Put(ctx, strings.NewReader("hello"), 5, spillstore.PutOptions{EventID: "evt-prune", Direction: "r"})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Backdate so it gets reaped.
	abs := filepath.Join(root, ref.Key)
	past := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(abs, past, past); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	dayDir := filepath.Dir(abs)

	if _, err := s.Sweep(ctx, time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if _, err := os.Stat(dayDir); !os.IsNotExist(err) {
		t.Fatalf("Sweep should have pruned empty day dir; stat err=%v", err)
	}
}

// TestStore_Sweep_KeepsNewer asserts entries newer than `olderThan` survive
// retention sweep AND total-cap sweep (we set cap higher than total bytes).
func TestStore_Sweep_KeepsNewer(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	ref, err := s.Put(ctx, strings.NewReader("fresh"), 5, spillstore.PutOptions{EventID: "evt-keep", Direction: "r"})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	deleted, err := s.Sweep(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("Sweep: fresh file should not be deleted, got deleted=%d", deleted)
	}
	if _, err := s.Get(ctx, ref); err != nil {
		t.Fatalf("Get after sweep: %v", err)
	}
}

// TestStore_Stat_Empty exercises Stat on an empty root — must return a
// zero-valued Stats with Backend stamped but no scan errors.
func TestStore_Stat_Empty(t *testing.T) {
	s, _ := newStore(t)
	st, err := s.Stat(context.Background())
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if st.Backend != "localfs" {
		t.Fatalf("Stat.Backend: got %q want localfs", st.Backend)
	}
	if st.ObjectCount != 0 || st.TotalBytes != 0 {
		t.Fatalf("Stat empty: got count=%d bytes=%d, want zeros", st.ObjectCount, st.TotalBytes)
	}
	if !st.OldestAt.IsZero() || !st.NewestAt.IsZero() {
		t.Fatalf("Stat empty: timestamps should be zero, got Oldest=%v Newest=%v", st.OldestAt, st.NewestAt)
	}
}

// TestStore_Stat_PopulatesTimestamps after a Put, ObjectCount + TotalBytes
// + timestamps must reflect the spilled object.
func TestStore_Stat_PopulatesTimestamps(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	payload := []byte("twelve bytes")
	_, err := s.Put(ctx, bytes.NewReader(payload), int64(len(payload)), spillstore.PutOptions{EventID: "evt-stat", Direction: "r"})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	st, err := s.Stat(ctx)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if st.ObjectCount != 1 {
		t.Fatalf("Stat.ObjectCount: got %d want 1", st.ObjectCount)
	}
	if st.TotalBytes != int64(len(payload)) {
		t.Fatalf("Stat.TotalBytes: got %d want %d", st.TotalBytes, len(payload))
	}
	if st.OldestAt.IsZero() || st.NewestAt.IsZero() {
		t.Fatalf("Stat: expected timestamps set, got Oldest=%v Newest=%v", st.OldestAt, st.NewestAt)
	}
}

// TestStore_Put_RenameFails forces the os.Rename branch to fail by placing a
// non-empty directory at the destination .bin path. Renaming a file over a
// non-empty directory is rejected by both Linux and Darwin (ENOTEMPTY/EISDIR).
func TestStore_Put_RenameFails(t *testing.T) {
	root := t.TempDir()
	s, err := localfs.New(localfs.Options{Root: root})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Pre-create the day dir and a non-empty directory at the target key,
	// matching the format Put would otherwise write.
	day := time.Now().UTC().Format("2006-01-02")
	dayDir := filepath.Join(root, day)
	if err := os.MkdirAll(dayDir, 0o700); err != nil {
		t.Fatalf("mkdir day: %v", err)
	}
	target := filepath.Join(dayDir, "evt-rename-r.bin")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	// Drop a child so the directory is non-empty — required to break rename
	// across both Linux and Darwin semantics.
	if err := os.WriteFile(filepath.Join(target, "child"), []byte("x"), 0o600); err != nil {
		t.Fatalf("seed child: %v", err)
	}
	_, err = s.Put(context.Background(), strings.NewReader("data"), 4, spillstore.PutOptions{EventID: "evt-rename", Direction: "r"})
	if err == nil {
		t.Fatal("Put with non-empty dir at target: want error, got nil")
	}
	if !strings.Contains(err.Error(), "rename") {
		t.Fatalf("Put rename error: want %q wrap, got %q", "rename", err.Error())
	}
}

// Note: the walkErr-return branches in Sweep and Stat are effectively
// dead code on Go's stdlib filepath.Walk — the callback's `if err != nil
// { return nil }` swallows the root-Lstat ENOENT, so Walk itself returns
// nil. Trying to assert the wrap surfaces this stdlib behavior, not a
// real failure mode. Left unverified by design (see localfs.go:241,308).

// TestStore_Sweep_NonDirEntryAtRoot covers the dayEntries `!d.IsDir()` branch
// of the empty-day-dir prune phase. A stray regular file at the root must be
// skipped, not treated as a day directory.
func TestStore_Sweep_NonDirEntryAtRoot(t *testing.T) {
	root := t.TempDir()
	s, err := localfs.New(localfs.Options{Root: root})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	stray := filepath.Join(root, "README.txt")
	if err := os.WriteFile(stray, []byte("stray"), 0o600); err != nil {
		t.Fatalf("seed stray file: %v", err)
	}
	if _, err := s.Sweep(context.Background(), time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	// Stray must NOT be deleted: it has no .bin extension and is not a dir.
	if _, err := os.Stat(stray); err != nil {
		t.Fatalf("stray file vanished after sweep: %v", err)
	}
}

// TestStore_Sweep_EvictionRemoveFails forces the os.Remove "else" branch in
// the total-cap eviction loop. We chmod the day directory to 0o500
// (read+execute, no write) AFTER the files are written so Remove returns
// EACCES. The loop must break rather than spin forever.
func TestStore_Sweep_EvictionRemoveFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission bits")
	}
	root := t.TempDir()
	s, err := localfs.New(localfs.Options{Root: root, TotalSizeCap: 4, Retention: 24 * time.Hour})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	// Two oversized objects (8 bytes each, cap=4) — eviction must engage.
	for i := range 2 {
		_, err := s.Put(ctx, bytes.NewReader(bytes.Repeat([]byte("A"), 8)), 8, spillstore.PutOptions{
			EventID:   filepathSafe("evt-evict", i),
			Direction: "x",
		})
		if err != nil {
			t.Fatalf("Put: %v", err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	day := time.Now().UTC().Format("2006-01-02")
	dayDir := filepath.Join(root, day)
	if err := os.Chmod(dayDir, 0o500); err != nil {
		t.Fatalf("chmod day: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dayDir, 0o700) })

	// Sweep with a far-past olderThan so retention does NOT delete (forcing
	// the path through total-cap eviction). The break-rather-than-spin
	// behavior is what we're asserting; deleted may be 0.
	done := make(chan error, 1)
	go func() {
		_, err := s.Sweep(ctx, time.Unix(0, 0))
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Sweep: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Sweep with unremovable evict candidate looped forever")
	}
}

// failingReader returns err on every Read; used to drive io.Copy failure in
// TestStore_Put_CopyError.
type failingReader struct{ err error }

func (f *failingReader) Read(_ []byte) (int, error) { return 0, f.err }

// TestStore_Put_HonorsExplicitKey is the SEC-M5-01 mechanism test: when
// PutOptions.Key is set (the Hub spill-blob handler passes the node-namespaced,
// token-signed key), Put writes to exactly that key instead of re-deriving the
// shared <day>/<eventId>-<dir>.bin key — so two nodes minting for the same
// eventId land on different objects and cannot overwrite each other.
func TestStore_Put_HonorsExplicitKey(t *testing.T) {
	s, root := newStore(t)
	payload := []byte("forensic body")
	explicit := "node-A/2026-01-02/evt-xyz-response.bin"
	ref, err := s.Put(context.Background(), bytes.NewReader(payload), int64(len(payload)), spillstore.PutOptions{
		EventID:   "evt-xyz",
		Direction: "response",
		Key:       explicit,
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if ref.Key != explicit {
		t.Fatalf("ref.Key = %q; want the explicit node-namespaced key %q", ref.Key, explicit)
	}
	if _, statErr := os.Stat(filepath.Join(root, explicit)); statErr != nil {
		t.Fatalf("blob not written at the explicit key: %v", statErr)
	}
	// The re-derived (shared) key must NOT exist — proving Put did not fall back.
	if _, statErr := os.Stat(filepath.Join(root, s.KeyFor(timeFixed(), "evt-xyz", "response"))); statErr == nil {
		t.Errorf("a blob was written at the re-derived shared key — overwrite vector still open")
	}
}

func timeFixed() time.Time { return time.Now().UTC() }
