package ws

import (
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
)

// Counts refusals per client so a caller retrying forever costs one line and
// then a periodic one, instead of a line per attempt.
var rejectedUpgrades sync.Map // client string -> *rejectionCounter

type rejectionCounter struct {
	n atomic.Int64
}

// The Hub sits behind an ALB and nginx, so r.RemoteAddr is always the proxy's
// loopback address: 3999 production refusals in two hours all said "127.0.0.1",
// and identifying the real caller took a packet capture. At ~33/min a line per
// attempt also drowns the WARN stream the deploy smoke reads. Hence the
// forwarded client address and a count. The credential is never logged.
func (s *Server) logRejectedUpgrade(r *http.Request, err error) {
	client := clientAddr(r)
	v, _ := rejectedUpgrades.LoadOrStore(client, &rejectionCounter{})
	c := v.(*rejectionCounter)
	n := c.n.Add(1)
	// First, then every 100th.
	if n != 1 && n%100 != 0 {
		return
	}
	s.logger.Warn("ws authenticate failed",
		"error", err,
		"client", client,
		"remote_addr", r.RemoteAddr,
		"user_agent", r.UserAgent(),
		"attempts", n)
}

// The first X-Forwarded-For entry is the one the edge saw; later entries are
// the proxies in between.
func clientAddr(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.Index(xff, ","); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	return r.RemoteAddr
}
