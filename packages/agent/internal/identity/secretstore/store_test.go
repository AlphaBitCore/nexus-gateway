package secretstore_test

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/identity/secretstore"
)

func TestOpen_ReturnsError_WhenNoBackendAvailable(t *testing.T) {
	// Force fallback to explicit file path to isolate from platform daemons.
	s, err := secretstore.OpenFallback(t.TempDir()+"/s.enc", []byte("test-device-key"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close() //nolint:errcheck

	if err := s.Set("foo", []byte("bar")); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("foo")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "bar" {
		t.Fatalf("got %q", got)
	}
	if err := s.Delete("foo"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("foo"); err == nil {
		t.Fatal("expected not-found")
	}
}
