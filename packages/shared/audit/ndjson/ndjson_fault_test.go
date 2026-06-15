package ndjson

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// fakeFile is a spoolFile whose Write/Close errors are programmable, so the
// in-flight write-failure and rotation-close-failure paths — which a real
// filesystem will not produce on demand — can be exercised deterministically.
type fakeFile struct {
	writeErr error
	closeErr error
}

func (f *fakeFile) Write(p []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return len(p), nil
}
func (f *fakeFile) Close() error { return f.closeErr }

func TestWriter_SurfacesHandleWriteError(t *testing.T) {
	orig := openSpool
	t.Cleanup(func() { openSpool = orig })
	openSpool = func(string) (spoolFile, int64, error) {
		return &fakeFile{writeErr: errors.New("disk full")}, 0, nil
	}

	w, err := New(t.TempDir(), "inst", 10, 100, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := w.Write([]byte(`{"id":"x"}`)); err == nil || !strings.Contains(err.Error(), "write") {
		t.Fatalf("want a wrapped write error, got: %v", err)
	}
}

func TestWriter_SurfacesRotationCloseError(t *testing.T) {
	orig := openSpool
	t.Cleanup(func() { openSpool = orig })
	openSpool = func(string) (spoolFile, int64, error) {
		return &fakeFile{closeErr: errors.New("close fail")}, 0, nil
	}

	// 1 MB max file; two ~0.6 MB records force the second Write to rotate,
	// and rotation closes the (close-failing) handle.
	w, err := New(t.TempDir(), "inst", 1, 100, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rec := make([]byte, 600*1024)
	if err := w.Write(rec); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := w.Write(rec); err == nil || !strings.Contains(err.Error(), "rotation") {
		t.Fatalf("want a rotation-close error on the second write, got: %v", err)
	}
}

// badEntry is an os.DirEntry whose Info() fails, exercising the dirSize branch
// that skips an entry it cannot stat (a file deleted between ReadDir and Info).
type badEntry struct{}

func (badEntry) Name() string               { return "vanished.ndjson" }
func (badEntry) IsDir() bool                { return false }
func (badEntry) Type() os.FileMode          { return 0 }
func (badEntry) Info() (os.FileInfo, error) { return nil, errors.New("vanished") }

func TestWriter_DirSizeSkipsUnstattableEntry(t *testing.T) {
	orig := readDir
	t.Cleanup(func() { readDir = orig })
	readDir = func(string) ([]os.DirEntry, error) {
		return []os.DirEntry{badEntry{}}, nil
	}

	w, err := New(t.TempDir(), "inst", 10, 100, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	// dirSize iterates the un-stattable entry (skips it, total stays 0), so the
	// quota gate passes and the write succeeds.
	if err := w.Write([]byte(`{"id":"y"}`)); err != nil {
		t.Fatalf("write should succeed when an entry is skipped: %v", err)
	}
}
