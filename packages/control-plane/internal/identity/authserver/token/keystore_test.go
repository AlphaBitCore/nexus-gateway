package token_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/token"
)

func TestKeystore_GenerateAndLoad(t *testing.T) {
	dir := t.TempDir()
	ks, err := token.OpenKeystore(dir)
	if err != nil {
		t.Fatal(err)
	}

	kid, err := ks.Generate()
	if err != nil {
		t.Fatal(err)
	}

	// Second call reuses existing keys.
	ks2, err := token.OpenKeystore(dir)
	if err != nil {
		t.Fatal(err)
	}
	keys := ks2.All()
	if len(keys) != 1 || keys[0].KID != kid {
		t.Fatalf("expected 1 key with kid=%s, got %+v", kid, keys)
	}
	if keys[0].Priv == nil {
		t.Fatal("expected RSA private key, got nil")
	}

	// File on disk 0600
	info, err := os.Stat(filepath.Join(dir, kid+".pem"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("want 0600, got %o", info.Mode().Perm())
	}
}

func TestKeystore_ActiveKID_NewestNotExpired(t *testing.T) {
	dir := t.TempDir()
	ks, _ := token.OpenKeystore(dir)
	_, _ = ks.Generate()
	kid2, _ := ks.Generate()
	if ks.ActiveKID() != kid2 {
		t.Fatalf("active kid should be most-recent; got %s want %s", ks.ActiveKID(), kid2)
	}
}
