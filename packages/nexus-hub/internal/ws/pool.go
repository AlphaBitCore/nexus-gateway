package ws

import (
	"log/slog"
	"sync"

	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

// Pool manages active WebSocket connections indexed by Thing ID and type.
// It implements the thingmgr.WSPool interface.
type Pool struct {
	mu     sync.RWMutex
	conns  map[string]*Conn
	byType map[string]map[string]*Conn // type -> id -> conn
	logger *slog.Logger

	connTotal  *opsmetrics.Counter
	connGauge  *opsmetrics.Gauge
	connByType *opsmetrics.Gauge
}

// NewPool creates a connection pool wired to the supplied opsmetrics
// registry. The instruments map onto the spec §6.3 Hub catalog:
//
//   - things.connected{type}      -> connByType (per-thing-type gauge)
//   - ws.reconnects_total          -> connTotal  (every Add increments —
//     "reconnect" here means "new conn accepted", which matches the
//     spec semantics from a Hub perspective: each thing reconnect lands
//     as one Add.)
//
// reg may be nil only for harnesses that don't care about metrics; the Pool
// then operates without instrumentation. Production callers must wire the
// shared registry built in cmd/nexus-hub/main.go.
func NewPool(reg *opsmetrics.Registry, logger *slog.Logger) *Pool {
	p := &Pool{
		conns:  make(map[string]*Conn),
		byType: make(map[string]map[string]*Conn),
		logger: logger.With("component", "ws_pool"),
	}
	if reg != nil {
		p.connTotal = reg.NewCounter("ws.reconnects_total", nil)
		p.connGauge = reg.NewGauge("ws.connections_active", nil)
		p.connByType = reg.NewGauge("things.connected", []string{"type"})
	}
	return p
}

// Add registers a connection. Replaces any existing connection for the same Thing ID.
func (p *Pool) Add(c *Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Replace existing
	if old, ok := p.conns[c.thingID]; ok {
		old.Close()
		p.removeUnlocked(old)
	}

	p.conns[c.thingID] = c
	if p.byType[c.thingType] == nil {
		p.byType[c.thingType] = make(map[string]*Conn)
	}
	p.byType[c.thingType][c.thingID] = c

	if p.connTotal != nil {
		p.connTotal.With().Inc()
	}
	if p.connGauge != nil {
		p.connGauge.With().Inc()
	}
	if p.connByType != nil {
		p.connByType.With(c.thingType).Inc()
	}
}

// Remove removes a connection from the pool.
func (p *Pool) Remove(c *Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.removeUnlocked(c)
}

func (p *Pool) removeUnlocked(c *Conn) {
	if _, ok := p.conns[c.thingID]; !ok {
		return
	}
	delete(p.conns, c.thingID)
	if typeMap, ok := p.byType[c.thingType]; ok {
		delete(typeMap, c.thingID)
		if len(typeMap) == 0 {
			delete(p.byType, c.thingType)
		}
	}
	if p.connGauge != nil {
		p.connGauge.With().Dec()
	}
	if p.connByType != nil {
		p.connByType.With(c.thingType).Dec()
	}
}

// Send sends a message to a specific Thing by ID.
func (p *Pool) Send(thingID string, msg []byte) bool {
	p.mu.RLock()
	c, ok := p.conns[thingID]
	p.mu.RUnlock()
	if !ok {
		return false
	}
	return c.Write(msg) == nil
}

// Broadcast sends a message to all connected Things of a given type.
func (p *Pool) Broadcast(thingType string, msg []byte) int {
	p.mu.RLock()
	typeMap := p.byType[thingType]
	conns := make([]*Conn, 0, len(typeMap))
	for _, c := range typeMap {
		conns = append(conns, c)
	}
	p.mu.RUnlock()

	sent := 0
	for _, c := range conns {
		if c.Write(msg) == nil {
			sent++
		}
	}
	return sent
}

// IsConnected returns true if the Thing has an active connection.
func (p *Pool) IsConnected(thingID string) bool {
	p.mu.RLock()
	_, ok := p.conns[thingID]
	p.mu.RUnlock()
	return ok
}

// Count returns the total number of active connections.
func (p *Pool) Count() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.conns)
}

// CloseAll closes all connections in the pool.
func (p *Pool) CloseAll() {
	p.mu.Lock()
	conns := make([]*Conn, 0, len(p.conns))
	for _, c := range p.conns {
		conns = append(conns, c)
	}
	p.mu.Unlock()

	for _, c := range conns {
		c.Close()
	}
}
