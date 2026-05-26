// Package conn provides connection lifecycle management including tracking,
// idle timeout enforcement, graceful shutdown coordination, and buffer pooling.
package conn

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/metrics"
)

// ErrAtCapacity is returned when the maximum concurrent connection limit is reached.
var ErrAtCapacity = errors.New("max concurrent connections reached")

// ConnInfo holds per-connection metadata stored while a connection is active.
type ConnInfo struct {
	ID          string    `json:"id"`
	SourceIP    string    `json:"sourceIp"`
	TargetHost  string    `json:"targetHost"`
	ConnectedAt time.Time `json:"connectedAt"`
}

// Manager tracks active connections and enforces concurrency limits.
type Manager struct {
	active   atomic.Int64
	maxConns int64
	conns    sync.Map // id -> *ConnInfo
}

// NewManager creates a connection manager with the given max concurrent limit.
// maxConns must be > 0; the caller is responsible for validating configuration.
func NewManager(maxConns int) *Manager {
	return &Manager{
		maxConns: int64(maxConns),
	}
}

// Acquire reserves a connection slot. It returns a unique connection ID (UUID v4)
// on success or ErrAtCapacity if the limit has been reached.
// Metadata fields (sourceIP, targetHost) are stored as empty strings; prefer
// AcquireWithInfo when caller has connection context available.
func (m *Manager) Acquire() (string, error) {
	return m.AcquireWithInfo("", "")
}

// AcquireWithInfo reserves a connection slot and stores per-connection metadata.
// sourceIP and targetHost are recorded for the /connections API. Returns a unique
// connection ID (UUID v4) on success or ErrAtCapacity if the limit has been reached.
func (m *Manager) AcquireWithInfo(sourceIP, targetHost string) (string, error) {
	cur := m.active.Add(1)
	if cur > m.maxConns {
		m.active.Add(-1)
		if metrics.ConnectionsTotal != nil {
			metrics.ConnectionsTotal.With("rejected_capacity").Inc()
		}
		return "", fmt.Errorf("%w: %d/%d", ErrAtCapacity, cur-1, m.maxConns)
	}
	id := uuid.New().String()
	m.conns.Store(id, &ConnInfo{
		ID:          id,
		SourceIP:    sourceIP,
		TargetHost:  targetHost,
		ConnectedAt: time.Now(),
	})
	if metrics.ConnectionsActive != nil {
		metrics.ConnectionsActive.With().Set(float64(cur))
	}
	return id, nil
}

// Release releases a connection slot identified by id and updates the active gauge.
// Passing an empty id (from a bare Acquire call) is safe; the map lookup is a no-op.
func (m *Manager) Release(id string) {
	m.conns.Delete(id)
	cur := m.active.Add(-1)
	if metrics.ConnectionsActive != nil {
		metrics.ConnectionsActive.With().Set(float64(cur))
	}
}

// ActiveCount returns the current number of active connections.
func (m *Manager) ActiveCount() int64 {
	return m.active.Load()
}

// ActiveConnections returns a snapshot of all currently active connections.
func (m *Manager) ActiveConnections() []ConnInfo {
	var result []ConnInfo
	m.conns.Range(func(_, value any) bool {
		result = append(result, *value.(*ConnInfo))
		return true
	})
	return result
}
