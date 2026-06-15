package wiring

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/cmd/compliance-proxy/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/testutil"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/tls/issuer"
)

// newWiringTestRedis returns an in-memory miniredis + connected client.
func newWiringTestRedis(t *testing.T) *redis.Client {
	t.Helper()
	s, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(s.Close)
	rdb := redis.NewClient(&redis.Options{Addr: s.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

// TestInitCertIssuer_RemoteSigningMode_BadCACertPath exercises the
// "remote" signing mode path where the CA cert file does not exist. The cert
// read fails before any DEK bootstrap, so deps are not required here.
func TestInitCertIssuer_RemoteSigningMode_BadCACertPath(t *testing.T) {
	cfg := &config.Config{}
	cfg.CA.CertPath = "/nonexistent/ca.pem"
	cfg.CA.KeyPath = "/nonexistent/ca.key"
	cfg.CA.KMS.SigningMode = "remote"
	cfg.CA.KMS.SignCommand = []string{"cat"}

	_, err := InitCertIssuer(cfg, nil, testLogger())
	if err == nil {
		t.Fatal("expected error for remote signing mode with bad CA cert path")
	}
}

// TestInitCertIssuer_RemoteSigningMode_HappyPath drives the full self-
// bootstrapping envelope: valid cert + Redis + KMS commands → the DEK is
// generated, wrapped, and SETNX-persisted, and a working Issuer is returned.
// Encrypt uses `cat` (reads plaintext from stdin; no {file}, F-0299).
// Decrypt uses `cat {file}` (reads ciphertext from temp file, identity passthrough).
// Observable evidence: the wrapped-DEK key now exists in Redis.
func TestInitCertIssuer_RemoteSigningMode_HappyPath(t *testing.T) {
	dir := t.TempDir()
	certPath, _, err := testutil.WriteTestCA(dir)
	if err != nil {
		t.Fatalf("WriteTestCA: %v", err)
	}
	rdb := newWiringTestRedis(t)

	cfg := &config.Config{}
	cfg.CA.CertPath = certPath
	cfg.CA.KMS.SigningMode = "remote"
	cfg.CA.KMS.SignCommand = []string{"cat"}
	cfg.CA.KMS.EncryptCommand = []string{"cat"} // reads plaintext from stdin; no {file} on encrypt path (F-0299)
	cfg.CA.KMS.Command = []string{"cat", "{file}"}
	cfg.CA.KMS.TimeoutSec = 5

	result, err := InitCertIssuer(cfg, rdb, testLogger())
	if err != nil {
		t.Fatalf("InitCertIssuer remote happy path: %v", err)
	}
	if result.Issuer == nil {
		t.Error("expected non-nil Issuer in remote signing mode")
	}
	// The wrapped DEK must now be persisted under the well-known key.
	if n, err := rdb.Exists(context.Background(), issuer.CertCacheDEKRedisKey).Result(); err != nil || n != 1 {
		t.Errorf("wrapped cert-cache DEK must be persisted (exists=%d, err=%v)", n, err)
	}
}

// TestInitCertIssuer_RemoteSigningMode_NoRedis_FailClosed: remote mode without
// a Redis client must abort (the DEK envelope has nowhere to live) — never
// fall back to a CA-derived key.
func TestInitCertIssuer_RemoteSigningMode_NoRedis_FailClosed(t *testing.T) {
	dir := t.TempDir()
	certPath, _, _ := testutil.WriteTestCA(dir)

	cfg := &config.Config{}
	cfg.CA.CertPath = certPath
	cfg.CA.KMS.SigningMode = "remote"
	cfg.CA.KMS.SignCommand = []string{"cat"}
	cfg.CA.KMS.EncryptCommand = []string{"cat"} // reads plaintext from stdin; no {file} on encrypt path (F-0299)
	cfg.CA.KMS.Command = []string{"cat", "{file}"}

	_, err := InitCertIssuer(cfg, nil, testLogger())
	if err == nil {
		t.Fatal("remote mode without Redis must fail-closed")
	}
}

// TestInitCertIssuer_RemoteSigningMode_NoEncryptCommand_FailClosed: remote
// mode requires ca.kms.encryptCommand.
func TestInitCertIssuer_RemoteSigningMode_NoEncryptCommand_FailClosed(t *testing.T) {
	dir := t.TempDir()
	certPath, _, _ := testutil.WriteTestCA(dir)
	rdb := newWiringTestRedis(t)

	cfg := &config.Config{}
	cfg.CA.CertPath = certPath
	cfg.CA.KMS.SigningMode = "remote"
	cfg.CA.KMS.SignCommand = []string{"cat"}
	cfg.CA.KMS.Command = []string{"cat", "{file}"}

	_, err := InitCertIssuer(cfg, rdb, testLogger())
	if err == nil {
		t.Fatal("remote mode without encryptCommand must fail-closed")
	}
}

// TestInitCertIssuer_RemoteSigningMode_NoDecryptCommand_FailClosed: remote
// mode requires ca.kms.command (KMS decrypt).
func TestInitCertIssuer_RemoteSigningMode_NoDecryptCommand_FailClosed(t *testing.T) {
	dir := t.TempDir()
	certPath, _, _ := testutil.WriteTestCA(dir)
	rdb := newWiringTestRedis(t)

	cfg := &config.Config{}
	cfg.CA.CertPath = certPath
	cfg.CA.KMS.SigningMode = "remote"
	cfg.CA.KMS.SignCommand = []string{"cat"}
	cfg.CA.KMS.EncryptCommand = []string{"cat"} // reads plaintext from stdin; no {file} on encrypt path (F-0299)

	_, err := InitCertIssuer(cfg, rdb, testLogger())
	if err == nil {
		t.Fatal("remote mode without KMS decrypt command must fail-closed")
	}
}

// TestRedisDEKStore_GetSetRoundTrip pins the go-redis adapter: GET on an
// absent key reports found=false; SETNX creates it once (won=true) then
// reports won=false on the second call; GET then returns the stored blob.
func TestRedisDEKStore_GetSetRoundTrip(t *testing.T) {
	rdb := newWiringTestRedis(t)
	store := newRedisDEKStore(rdb)
	ctx := context.Background()

	if _, found, err := store.GetWrappedDEK(ctx); err != nil || found {
		t.Errorf("absent key must report found=false, nil err; got found=%v err=%v", found, err)
	}

	blob := []byte("wrapped-dek-bytes")
	won, err := store.SetWrappedDEKIfAbsent(ctx, blob)
	if err != nil || !won {
		t.Fatalf("first SETNX must win; won=%v err=%v", won, err)
	}
	won2, err := store.SetWrappedDEKIfAbsent(ctx, []byte("other"))
	if err != nil || won2 {
		t.Errorf("second SETNX must lose; won=%v err=%v", won2, err)
	}
	got, found, err := store.GetWrappedDEK(ctx)
	if err != nil || !found || string(got) != string(blob) {
		t.Errorf("GET must return the first-written blob; got=%q found=%v err=%v", got, found, err)
	}
}

// TestRedisDEKStore_GetTransportError: a dead Redis surfaces a transport
// error from GetWrappedDEK (NOT a false "absent"), so the bootstrap can
// fail-closed rather than minting a divergent DEK.
func TestRedisDEKStore_GetTransportError(t *testing.T) {
	s, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: s.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	s.Close() // kill the server

	_, found, err := newRedisDEKStore(rdb).GetWrappedDEK(context.Background())
	if err == nil {
		t.Fatal("dead Redis must surface a transport error, not found=false")
	}
	if found {
		t.Error("found must be false on transport error")
	}
}
