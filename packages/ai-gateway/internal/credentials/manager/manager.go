// Package credmanager provides cached, decrypt-on-read access to provider
// credentials. It depends on creddecrypt for AES-256-GCM key operations and
// on the platform store for credential records.
package credmanager

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	creddecrypt "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/credentials/decrypt"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
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
	logger         *slog.Logger
	cacheTTL       time.Duration

	mu    sync.RWMutex
	cache map[string]*cachedEntry // keyed by credential ID
	sf    singleflight.Group
}

// NewManager creates a credential manager with a single-key decryptor.
func NewManager(src Source, decryptor *creddecrypt.Decryptor, logger *slog.Logger) *Manager {
	return &Manager{
		db:        src,
		decryptor: decryptor,
		logger:    logger,
		cacheTTL:  defaultCacheTTL,
		cache:     make(map[string]*cachedEntry),
	}
}

// NewMultiKeyManager creates a credential manager with multi-key decryption support.
func NewMultiKeyManager(src Source, md *creddecrypt.MultiDecryptor, logger *slog.Logger) *Manager {
	return &Manager{
		db:             src,
		multiDecryptor: md,
		logger:         logger,
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
	if val := m.cacheGet(credentialID); val != "" {
		return val, nil
	}

	// Cache miss — use singleflight to collapse concurrent fetches for the same ID.
	v, err, _ := m.sf.Do("cred:"+credentialID, func() (any, error) {
		// Double-check inside singleflight in case a concurrent goroutine already populated the cache.
		if val := m.cacheGet(credentialID); val != "" {
			return val, nil
		}
		cred, err := m.db.GetCredentialByID(ctx, credentialID)
		if err != nil {
			return "", fmt.Errorf("credentials: fetch %s: %w", credentialID, err)
		}
		plaintext, err := m.decrypt(cred.EncryptionKeyID, cred.EncryptedKey, cred.EncryptionIv, cred.EncryptionTag)
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
	if val := m.cacheGet(cred.ID); val != "" {
		return val, cred.ID, cred.Name, nil
	}

	// Cache miss — use singleflight to collapse concurrent decryptions for the same credential.
	v, err, _ := m.sf.Do("cred:"+cred.ID, func() (any, error) {
		// Double-check inside singleflight in case a concurrent goroutine already populated the cache.
		if val := m.cacheGet(cred.ID); val != "" {
			return val, nil
		}
		plaintext, err := m.decrypt(cred.EncryptionKeyID, cred.EncryptedKey, cred.EncryptionIv, cred.EncryptionTag)
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

// decrypt dispatches to MultiDecryptor (if set) or falls back to the single Decryptor.
func (m *Manager) decrypt(keyID, ciphertextHex, ivHex, tagHex string) (string, error) {
	if m.multiDecryptor != nil {
		return m.multiDecryptor.Decrypt(keyID, ciphertextHex, ivHex, tagHex)
	}
	return m.decryptor.Decrypt(ciphertextHex, ivHex, tagHex)
}

func (m *Manager) cacheGet(id string) string {
	m.mu.RLock()
	e, ok := m.cache[id]
	if !ok {
		m.mu.RUnlock()
		return ""
	}
	if time.Now().After(e.expiresAt) {
		m.mu.RUnlock()
		// Proactively evict expired entry to prevent unbounded growth.
		m.mu.Lock()
		if e2, ok := m.cache[id]; ok && time.Now().After(e2.expiresAt) {
			delete(m.cache, id)
		}
		m.mu.Unlock()
		return ""
	}
	plaintext := e.plaintext
	m.mu.RUnlock()
	return plaintext
}

func (m *Manager) cacheSet(id, plaintext string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cache[id] = &cachedEntry{
		plaintext: plaintext,
		expiresAt: time.Now().Add(m.cacheTTL),
	}
}
