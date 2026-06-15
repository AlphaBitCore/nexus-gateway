package ws

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const (
	pongTimeout  = 10 * time.Second
	writeTimeout = 5 * time.Second
	// maxMessageSize bounds a single inbound WS frame. The largest practical
	// inbound message is a Thing's shadow_report carrying full `reported` plus
	// a `reportedOutcomes` ledger for a fleet node; 64 KiB hard-closed those
	// connections, sending the Thing into a reconnect loop. 1 MiB
	// covers the largest expected shadow_report with a full outcomes ledger
	// while still capping a hostile/runaway peer.
	maxMessageSize = 1 * 1024 * 1024 // 1 MiB
)

// pingInterval is the Hub→Thing ping cadence. It is a var, not a const, so
// tests can shorten it; production code must not mutate it.
var pingInterval = 30 * time.Second

// MessageHandler processes incoming WebSocket messages. thingType is the
// authenticated Thing type captured at upgrade time and is passed alongside
// thingID so handlers (notably opsmetrics dispatch) don't have to look it
// up out-of-band.
type MessageHandler func(thingID, thingType string, data []byte)

// LivenessHandler is invoked on every successful Hub→Thing ping. It is the
// single source of last_seen_at refresh for WS-connected Things.
type LivenessHandler func(thingID string)

// Conn wraps a github.com/coder/websocket connection with read/write pumps.
type Conn struct {
	ws         *websocket.Conn
	thingID    string
	thingType  string
	logger     *slog.Logger
	onMsg      MessageHandler
	onLiveness LivenessHandler
	outCh      chan []byte
	done       chan struct{}
	closeOnce  sync.Once
}

// ConnOption configures a Conn.
type ConnOption func(*Conn)

func newConn(ws *websocket.Conn, thingID, thingType string, onMsg MessageHandler, onLiveness LivenessHandler, logger *slog.Logger) *Conn {
	return &Conn{
		ws:         ws,
		thingID:    thingID,
		thingType:  thingType,
		logger:     logger.With("thing_id", thingID),
		onMsg:      onMsg,
		onLiveness: onLiveness,
		outCh:      make(chan []byte, 64),
		done:       make(chan struct{}),
	}
}

// Run starts read and write pumps. Blocks until the connection is closed.
func (c *Conn) Run(ctx context.Context) {
	c.ws.SetReadLimit(maxMessageSize)

	go c.writePump(ctx)
	c.readPump(ctx) // blocks
}

func (c *Conn) readPump(ctx context.Context) {
	defer c.Close()
	for {
		_, data, err := c.ws.Read(ctx)
		if err != nil {
			if websocket.CloseStatus(err) == websocket.StatusNormalClosure ||
				websocket.CloseStatus(err) == websocket.StatusGoingAway {
				c.logger.Debug("ws closed normally")
			} else {
				c.logger.Debug("ws read error", "error", err)
			}
			return
		}
		if c.onMsg != nil {
			c.onMsg(c.thingID, c.thingType, data)
		}
	}
}

func (c *Conn) writePump(ctx context.Context) {
	// writePump runs as a goroutine; an unrecovered panic here (notably from
	// onLiveness, which makes a TouchLiveness DB call) would crash the entire
	// Hub process. Recover, log, and tear down just this connection so the
	// Thing reconnects. Mirrors the selfshadow dispatchOne precedent.
	defer func() {
		if r := recover(); r != nil {
			c.logger.Error("writePump panic", "panic", r)
			c.Close()
		}
	}()

	pingTicker := time.NewTicker(pingInterval)
	defer pingTicker.Stop()

	for {
		select {
		case <-c.done:
			return
		case <-ctx.Done():
			return
		case msg := <-c.outCh:
			writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := c.ws.Write(writeCtx, websocket.MessageText, msg)
			cancel()
			if err != nil {
				c.logger.Debug("ws write error", "error", err)
				c.Close()
				return
			}
		case <-pingTicker.C:
			pingCtx, cancel := context.WithTimeout(ctx, pongTimeout)
			err := c.ws.Ping(pingCtx)
			cancel()
			if err != nil {
				c.logger.Debug("ws ping failed", "error", err)
				c.Close()
				return
			}
			if c.onLiveness != nil {
				c.onLiveness(c.thingID)
			}
		}
	}
}

// Write queues a message for sending. Non-blocking; drops if buffer full.
func (c *Conn) Write(data []byte) error {
	select {
	case c.outCh <- data:
		return nil
	default:
		return fmt.Errorf("write buffer full for %s", c.thingID)
	}
}

// Close closes the WebSocket connection with the normal close handshake. Safe
// to call multiple times.
func (c *Conn) Close() {
	c.closeOnce.Do(func() {
		close(c.done)
		_ = c.ws.Close(websocket.StatusNormalClosure, "closing")
	})
}

// CloseNow tears the connection down immediately, skipping the WebSocket close
// handshake. Used by the pool's replace path: a superseded connection
// must be evicted without blocking on the old peer's close-frame response —
// when the old peer has vanished (the reconnect race), a graceful Close would
// stall for the close timeout while the pool lock is held, freezing every other
// pool operation and delaying the new connection's handshake reply. Shares the
// closeOnce guard with Close, so the two are mutually idempotent. Safe to call
// multiple times.
func (c *Conn) CloseNow() {
	c.closeOnce.Do(func() {
		close(c.done)
		_ = c.ws.CloseNow()
	})
}

// ThingID returns the Thing's ID.
func (c *Conn) ThingID() string { return c.thingID }

// ThingType returns the Thing's type.
func (c *Conn) ThingType() string { return c.thingType }
