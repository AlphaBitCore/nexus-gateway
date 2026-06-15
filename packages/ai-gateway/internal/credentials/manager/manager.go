// Package credmanager provides cached, decrypt-on-read access to provider
// credentials. It depends on creddecrypt for AES-256-GCM key operations and
// on the platform store for credential records.
//
// Plaintext-key handling caveat: decrypted API keys are held as Go strings
// in cachedEntry.plaintext, in the singleflight result, and on the call
// stack of GetDecrypted / GetForProvider. Go strings are immutable, so the
// plaintext cannot be zeroed after use the way a []byte buffer could — the
// bytes linger in the heap until the garbage collector reclaims and
// eventually overwrites them. Eliminating that window would require migrating
// the entire decrypt → cache → caller chain to []byte and wiping each buffer
// explicitly, a change far broader than the credentials package. This is an
// accepted managed-language tradeoff for the prompt-cache TTL window
// (defaultCacheTTL); the key never touches disk or logs.
package credmanager

import (
	"context"
	"fmt"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	creddecrypt "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/credentials/decrypt"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/keyderive"
)

const defaultCacheTTL = 5 * time.Minute

type cachedEntry struct {
	plaintext string
	expiresAt time.Time
}

// Source is the credential lookup surface the Manager depends on. The
// production runtime injects *cachelayer.Layer to read from in-memory
// snapshots; *store.DB also satisfies it for tests and degraded paths.
type Source interface {
	GetCredentialByID(ctx context.Context, id string) (*store.Credential, error)
	GetCredentialForProvider(ctx context.Context, providerID string) (*store.Credential, error)
	ListCredentialsForProvider(ctx context.Context, providerID string) ([]store.Credential, error)
}

// Manager provides cached, decrypt-on-read access to provider credentials.
type Manager struct {
	db             Source
	decryptor      *creddecrypt.Decryptor
	multiDecryptor *creddecrypt.MultiDecryptor
	cacheTTL       time.Duration

	mu    sync.RWMutex
	cache map[string]*cachedEntry // keyed by credential ID
	sf    singleflight.Group
}

// NewManager creates a credential manager with a single-key decryptor.
func NewManager(src Source, decryptor *creddecrypt.Decryptor) *Manager {
	return &Manager{
		db:        src,
		decryptor: decryptor,
		cacheTTL:  defaultCacheTTL,
		cache:     make(map[string]*cachedEntry),
	}
}

// NewMultiKeyManager creates a credential manager with multi-key decryption support.
func NewMultiKeyManager(src Source, md *creddecrypt.MultiDecryptor) *Manager {
	return &Manager{
		db:             src,
		multiDecryptor: md,
		cacheTTL:       defaultCacheTTL,
		cache:          make(map[string]*cachedEntry),
	}
}

// GetDecrypted returns the decrypted API key for a credential ID.
// Uses an in-memory cache with TTL; falls back to DB + decrypt on miss.
func (m *Manager) GetDecrypted(ctx context.Context, credentialID string) (string, error) {
	if m.db == nil {
		return "", fmt.Errorf("credentials: database not available")
	}

	// Fast path: cache hit.
	if val, ok := m.cacheGet(credentialID); ok {
		return val, nil
	}

	// Cache miss — use singleflight to collapse concurrent fetches for the same ID.
	v, err, _ := m.sf.Do("cred:"+credentialID, func() (any, error) {
		// Double-check inside singleflight in case a concurrent goroutine already populated the cache.
		if val, ok := m.cacheGet(credentialID); ok {
			return val, nil
		}
		cred, err := m.db.GetCredentialByID(ctx, credentialID)
		if err != nil {
			return "", fmt.Errorf("credentials: fetch %s: %w", credentialID, err)
		}
		aad := keyderive.ProviderCredentialAAD(cred.ID, cred.ProviderID)
		plaintext, err := m.decrypt(cred.EncryptionKeyID, cred.EncryptedKey, cred.EncryptionIv, cred.EncryptionTag, aad)
		if err != nil {
			return "", fmt.Errorf("credentials: decrypt %s: %w", credentialID, err)
		}
		m.cacheSet(credentialID, plaintext)
		return plaintext, nil
	})
	if err != nil {
		return "", err
	}
	return v.(string), nil
}

// GetForProvider returns the decrypted API key and credential UUID for the first
// enabled credential belonging to a provider.
func (m *Manager) GetForProvider(ctx context.Context, providerID string) (string, string, string, error) {
	if m.db == nil {
		return "", "", "", fmt.Errorf("credentials: database not available")
	}

	cred, err := m.db.GetCredentialForProvider(ctx, providerID)
	if err != nil {
		return "", "", "", fmt.Errorf("credentials: provider %s: %w", providerID, err)
	}

	// Fast path: cache hit by credential ID.
	if val, ok := m.cacheGet(cred.ID); ok {
		return val, cred.ID, cred.Name, nil
	}

	// Cache miss — use singleflight to collapse concurrent decryptions for the same credential.
	v, err, _ := m.sf.Do("cred:"+cred.ID, func() (any, error) {
		// Double-check inside singleflight in case a concurrent goroutine already populated the cache.
		if val, ok := m.cacheGet(cred.ID); ok {
			return val, nil
		}
		aad := keyderive.ProviderCredentialAAD(cred.ID, cred.ProviderID)
		plaintext, err := m.decrypt(cred.EncryptionKeyID, cred.EncryptedKey, cred.EncryptionIv, cred.EncryptionTag, aad)
		if err != nil {
			return "", fmt.Errorf("credentials: decrypt %s: %w", cred.ID, err)
		}
		m.cacheSet(cred.ID, plaintext)
		return plaintext, nil
	})
	if err != nil {
		return "", "", "", err
	}
	return v.(string), cred.ID, cred.Name, nil
}

// ListForProvider returns all enabled, active credentials for a provider
// without decryption — metadata only. Used by the multi-credential pool selector.
func (m *Manager) ListForProvider(ctx context.Context, providerID string) ([]store.Credential, error) {
	if m.db == nil {
		return nil, fmt.Errorf("credentials: database not available")
	}
	return m.db.ListCredentialsForProvider(ctx, providerID)
}

// Invalidate removes a credential from the cache.
func (m *Manager) Invalidate(credentialID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.cache, credentialID)
}

// ClearCache removes all cached credentials.
func (m *Manager) ClearCache() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cache = make(map[string]*cachedEntry)
}

// decrypt dispatches to MultiDecryptor (if set) or falls back to the single
// Decryptor. aad is the row-identity binding — built by the callers
// from the credential's id + provider — and must match what the Control Plane
// sealed with, so a ciphertext swapped from another credential's row fails.
func (m *Manager) decrypt(keyID, ciphertextHex, ivHex, tagHex string, aad []byte) (string, error) {
	if m.multiDecryptor != nil {
		return m.multiDecryptor.Decrypt(keyID, ciphertextHex, ivHex, tagHex, aad)
	}
	return m.decryptor.Decrypt(ciphertextHex, ivHex, tagHex, aad)
}

// cacheGet returns the cached plaintext and whether a live entry was found.
// The boolean (not an empty-string sentinel) distinguishes a genuine cache
// miss from a credential that legitimately decrypts to "" — without it an
// empty-plaintext credential would re-decrypt on every call and never cache.
func (m *Manager) cacheGet(id string) (string, bool) {
	m.mu.RLock()
	e, ok := m.cache[id]
	if !ok {
		m.mu.RUnlock()
		return "", false
	}
	if time.Now().After(e.expiresAt) {
		m.mu.RUnlock()
		// Proactively evict expired entry to prevent unbounded growth.
		m.mu.Lock()
		if e2, ok := m.cache[id]; ok && time.Now().After(e2.expiresAt) {
			delete(m.cache, id)
		}
		m.mu.Unlock()
		return "", false
	}
	plaintext := e.plaintext
	m.mu.RUnlock()
	return plaintext, true
}

func (m *Manager) cacheSet(id, plaintext string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cache[id] = &cachedEntry{
		plaintext: plaintext,
		expiresAt: time.Now().Add(m.cacheTTL),
	}
}
