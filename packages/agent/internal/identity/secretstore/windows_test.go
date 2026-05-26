//go:build windows

package secretstore_test

import (
	"errors"
	"runtime"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/identity/secretstore"
)

// TestWindows_WinCredRoundtrip exercises the Set/Get/Delete lifecycle against
// the Windows Credential Manager. It is a no-op on non-windows hosts and is
// compiled only when the //go:build windows constraint matches.
func TestWindows_WinCredRoundtrip(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip()
	}
	s, err := secretstore.OpenWinCred("ai.nexus.agent.test")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	defer s.Delete("k")

	if err := s.Set("k", []byte("v")); err != nil {
		t.Fatal(err)
	}
	v, err := s.Get("k")
	if err != nil || string(v) != "v" {
		t.Fatalf("Get after Set: err=%v value=%q", err, v)
	}
	if err := s.Delete("k"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("k"); !errors.Is(err, secretstore.ErrNotFound) {
		t.Fatalf("Get after Delete: expected ErrNotFound, got %v", err)
	}
	// Delete of a missing key must be idempotent.
	if err := s.Delete("k"); err != nil {
		t.Fatalf("Delete of missing key: %v", err)
	}
}
