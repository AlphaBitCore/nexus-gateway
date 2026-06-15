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

	connTotal   *opsmetrics.Counter
	connGauge   *opsmetrics.Gauge
	connByType  *opsmetrics.Gauge
	sendDropped *opsmetrics.Counter
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
		// ws.send_dropped_total counts config/broadcast pushes dropped because
		// the per-conn write buffer was full. A non-zero rate means a
		// Thing's outbound buffer backed up and its config push was lost; the
		// pool closes the conn so the Thing reconnects and rebuilds full state.
		p.sendDropped = reg.NewCounter("ws.send_dropped_total", []string{"type"})
	}
	return p
}

// Add registers a connection. Replaces any existing connection for the same Thing ID.
func (p *Pool) Add(c *Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Replace existing. Use CloseNow (not Close) so a vanished old peer's
	// missing close-frame response cannot stall this Add while it holds the
	// pool lock — see Conn.CloseNow.
	if old, ok := p.conns[c.thingID]; ok {
		old.CloseNow()
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

// Remove removes a connection from the pool, but ONLY if it is still the
// registered connection for its Thing ID. Returns true when this exact
// connection was evicted; false when a newer connection has already replaced
// it (reconnect race) or it was never present. Callers use the return value to
// decide whether to fire side effects like MarkOffline (see ws.Server).
func (p *Pool) Remove(c *Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.removeUnlocked(c)
}

// removeUnlocked deletes c from the pool only when c is the connection
// currently registered for its Thing ID. The identity guard (cur != c) is
// load-bearing: on an ungraceful disconnect the dead connection
// lingers until the read/ping timeout (~tens of seconds) while the Thing
// reconnects within ~1-2s. Add installs the new connection; when the stale
// connection's read loop finally unblocks and its defer calls Remove, a
// thingID-only delete would evict the LIVE new connection and fire a spurious
// MarkOffline — black-holing every subsequent config push (pool.Send returns
// false) and defeating drift auto-repair. Mirrors the client-side guard in
// shared/transport/thingclient (`if c.wsConn == conn`). Returns whether c was
// actually removed.
func (p *Pool) removeUnlocked(c *Conn) bool {
	if cur, ok := p.conns[c.thingID]; !ok || cur != c {
		return false
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
	return true
}

// Send sends a message to a specific Thing by ID. Returns true only when the
// message was queued onto the Thing's write buffer.
func (p *Pool) Send(thingID string, msg []byte) bool {
	p.mu.RLock()
	c, ok := p.conns[thingID]
	p.mu.RUnlock()
	if !ok {
		return false
	}
	return p.writeOrDrop(c, msg)
}

// Broadcast sends a message to all connected Things of a given type. Returns
// the number of Things the message was successfully queued for.
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
		if p.writeOrDrop(c, msg) {
			sent++
		}
	}
	return sent
}

// writeOrDrop queues msg onto c's write buffer and, on a full-buffer drop, logs
// a WARN, increments ws.send_dropped_total, and closes the connection so the
// Thing reconnects and rebuilds full state rather than silently missing a
// config push. Returns true when the message was queued.
func (p *Pool) writeOrDrop(c *Conn, msg []byte) bool {
	if err := c.Write(msg); err != nil {
		p.logger.Warn("ws send dropped: write buffer full; closing connection so thing reconnects",
			"thing_id", c.thingID, "thing_type", c.thingType, "error", err)
		if p.sendDropped != nil {
			p.sendDropped.With(c.thingType).Inc()
		}
		c.Close()
		return false
	}
	return true
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
