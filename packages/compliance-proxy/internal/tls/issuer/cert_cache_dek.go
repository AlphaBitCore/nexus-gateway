package issuer

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/kms"
)

// CertCacheDEKRedisKey is the well-known Redis key holding the KMS-wrapped
// cert-cache data-encryption key (DEK). Every proxy instance in the
// deployment reads the same key so they derive the same AES key and can read
// each other's cached leaf private keys. Distinct from the per-host cert
// entries under `nexus:proxy:cert:` (cache.redisKeyPrefix).
//
// Rotation is "delete this key": the next instance to boot finds it absent,
// generates a fresh DEK, and re-wraps. Old per-host cache entries encrypted
// under the prior DEK then fail to decrypt and are transparently re-signed.
const CertCacheDEKRedisKey = "nexus:proxy:cert-cache-dek"

// dekLen is the cert-cache DEK length: 32 bytes = AES-256 HKDF input keying
// material. The DEK is the HKDF IKM, not the AES key directly — HKDF still
// derives the on-the-wire AES key (salt "nexus-cert-cache", info
// "aes-256-gcm") so the cache encryption format is unchanged.
const dekLen = 32

// dekRandReader is the entropy source for fresh-DEK generation. Production
// keeps it at crypto/rand.Reader; tests swap it to exercise the
// entropy-failure branch the stdlib never surfaces in normal operation.
// Test-only override; production never reassigns this variable.
var dekRandReader io.Reader = rand.Reader

// CertCacheDEKStore is the minimal Redis surface the DEK bootstrap needs:
// read the wrapped DEK and create-it-once via SETNX. Abstracting it behind
// this interface keeps the issuer package free of a go-redis dependency and
// lets tests drive the first-boot / race-lost / Redis-error branches
// deterministically. The production adapter wraps a go-redis UniversalClient
// (see cmd/compliance-proxy/wiring/cert.go).
type CertCacheDEKStore interface {
	// GetWrappedDEK returns the wrapped DEK blob. found=false (with nil err)
	// signals the key is absent; a non-nil err signals a Redis transport
	// failure (which must NOT be treated as "absent").
	GetWrappedDEK(ctx context.Context) (blob []byte, found bool, err error)
	// SetWrappedDEKIfAbsent atomically creates the key only when absent
	// (SETNX, no expiry). won=true means this caller created it; won=false
	// means another instance won the race and the existing value stands.
	SetWrappedDEKIfAbsent(ctx context.Context, blob []byte) (won bool, err error)
}

// bootstrapCertCacheDEK resolves the 32-byte cert-cache DEK used as the HKDF
// input keying material in remote-signing mode. It is self-bootstrapping and
// multi-instance-safe:
//
//   - If a wrapped DEK already exists in Redis: KMS-Decrypt it.
//   - If absent: generate a fresh DEK, KMS-Encrypt (wrap) it, and persist via
//     SETNX. If SETNX loses the race (another instance wrote first), re-read
//     the winner's blob and KMS-Decrypt that, so every instance converges on
//     the same DEK.
//
// It is fail-closed: any missing dependency, KMS failure, or Redis transport
// failure returns a startup error naming the responsible config field and the
// operator action. It NEVER falls back to deriving a key from public CA bytes.
//
// KMS is invoked at most twice here (Encrypt on first boot, Decrypt on
// subsequent boots / race-loss) — once at startup, never on the runtime cache
// hot path.
func bootstrapCertCacheDEK(ctx context.Context, store CertCacheDEKStore, enc kms.Encryptor, dec kms.KMSProvider) ([]byte, error) {
	if store == nil {
		return nil, fmt.Errorf("cert: remote signing mode requires Redis for the cert-cache DEK envelope (set redis.addrs / REDIS_ADDRS)")
	}
	if enc == nil {
		return nil, fmt.Errorf("cert: remote signing mode requires a KMS encrypt command (set ca.kms.encryptCommand and grant kms:Encrypt)")
	}
	if dec == nil {
		return nil, fmt.Errorf("cert: remote signing mode requires a KMS decrypt command (set ca.kms.command and grant kms:Decrypt)")
	}

	// Existing wrapped DEK → unwrap it.
	wrapped, found, err := store.GetWrappedDEK(ctx)
	if err != nil {
		return nil, fmt.Errorf("cert: read wrapped cert-cache DEK from Redis (key %s): %w", CertCacheDEKRedisKey, err)
	}
	if found {
		return unwrapDEK(ctx, dec, wrapped)
	}

	// Absent → generate a fresh DEK, wrap it, and create-once via SETNX.
	dek := make([]byte, dekLen)
	if _, err := io.ReadFull(dekRandReader, dek); err != nil {
		return nil, fmt.Errorf("cert: generate cert-cache DEK: %w", err)
	}
	wrapped, err = enc.Encrypt(ctx, dek)
	if err != nil {
		return nil, fmt.Errorf("cert: KMS %s encrypt cert-cache DEK failed (check ca.kms.encryptCommand and the kms:Encrypt grant): %w", enc.Name(), err)
	}
	won, err := store.SetWrappedDEKIfAbsent(ctx, wrapped)
	if err != nil {
		return nil, fmt.Errorf("cert: persist wrapped cert-cache DEK via SETNX (key %s): %w", CertCacheDEKRedisKey, err)
	}
	if won {
		return dek, nil
	}

	// Lost the race: another instance wrote first. Re-read the winner's blob
	// and unwrap it so every instance converges on the same DEK.
	winner, found, err := store.GetWrappedDEK(ctx)
	if err != nil {
		return nil, fmt.Errorf("cert: re-read winner cert-cache DEK after SETNX race (key %s): %w", CertCacheDEKRedisKey, err)
	}
	if !found {
		return nil, fmt.Errorf("cert: wrapped cert-cache DEK vanished after losing SETNX race (key %s); retry startup", CertCacheDEKRedisKey)
	}
	return unwrapDEK(ctx, dec, winner)
}

// unwrapDEK KMS-Decrypts a wrapped DEK blob and validates its length.
func unwrapDEK(ctx context.Context, dec kms.KMSProvider, wrapped []byte) ([]byte, error) {
	dek, err := dec.Decrypt(ctx, wrapped)
	if err != nil {
		return nil, fmt.Errorf("cert: KMS %s decrypt cert-cache DEK failed (check ca.kms.command and the kms:Decrypt grant): %w", dec.Name(), err)
	}
	if len(dek) != dekLen {
		return nil, fmt.Errorf("cert: unwrapped cert-cache DEK is %d bytes, want %d (KMS decrypt produced a malformed key)", len(dek), dekLen)
	}
	return dek, nil
}
