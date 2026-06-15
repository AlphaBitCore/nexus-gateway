package access

import (
	"context"
	"net"
	"sync"
	"time"
)

// Every proxied CONNECT runs the private-IP (SSRF) check, which resolves
// the destination host — and net.DefaultResolver does NOT cache, so a
// connection storm to one host issued one full DNS round-trip per
// connection (twice per flow on some paths), adding resolver RTT to every
// request and turning the resolver into a throughput limiter.
//
// cachingResolver fixes both: concurrent lookups for the same host
// collapse onto a single in-flight resolve (no thundering herd), and a
// completed result is reused for a short TTL. The TTL is deliberately
// short so the SSRF check stays close to live DNS (a long cache would
// widen the DNS-rebinding window the check defends against).

const (
	// dnsCacheTTL bounds how long a SUCCESSFUL resolve is reused. Short on
	// purpose: long enough to absorb a connection burst to one host, short
	// enough that the private-IP check tracks live DNS. Failures are NOT
	// cached (see resolve).
	dnsCacheTTL = 10 * time.Second
	// dnsCacheMaxHosts caps distinct cached hosts so a proxy that sees
	// many destinations cannot grow the map without bound. On overflow
	// the expired entries are swept first, then the map is reset.
	dnsCacheMaxHosts = 8192
	// dnsResolveTimeout bounds the shared (caller-independent) resolve so
	// a hung resolver goroutine cannot live forever.
	dnsResolveTimeout = 5 * time.Second
)

// dnsCacheEntry is one host's resolve: in-flight until ready is closed,
// then holding the result + its expiry.
type dnsCacheEntry struct {
	ready   chan struct{}
	addrs   []net.IPAddr
	err     error
	expires time.Time
}

// cachingResolver wraps an upstream Resolver with single-flight +
// short-TTL caching. Safe for concurrent use.
type cachingResolver struct {
	upstream Resolver
	ttl      time.Duration
	now      func() time.Time

	mu      sync.Mutex
	entries map[string]*dnsCacheEntry
}

func newCachingResolver(upstream Resolver, ttl time.Duration) *cachingResolver {
	return &cachingResolver{
		upstream: upstream,
		ttl:      ttl,
		now:      time.Now,
		entries:  make(map[string]*dnsCacheEntry),
	}
}

// LookupIPAddr returns a cached result when fresh, joins an in-flight
// lookup for the same host, or kicks off the resolve — exactly one upstream
// call happens per host per in-flight window regardless of how many
// goroutines ask concurrently. Every caller (including the one that started
// the resolve) waits on the shared result OR its own ctx, so one caller's
// cancellation never affects another's.
func (c *cachingResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	c.mu.Lock()
	e := c.entries[host]
	if e == nil || (isReady(e) && !c.now().Before(e.expires)) {
		// No entry, or a completed-but-expired one: start a fresh resolve.
		e = &dnsCacheEntry{ready: make(chan struct{})}
		c.entries[host] = e
		c.maybeSweepLocked()
		c.mu.Unlock()
		// Resolve on a detached goroutine, NOT under the caller's ctx: the
		// result is shared, so a single caller disconnecting must not
		// cancel (and then poison) it for everyone else.
		go c.resolve(host, e)
	} else {
		c.mu.Unlock()
	}

	select {
	case <-e.ready:
		return e.addrs, e.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// resolve performs the shared upstream lookup under a bounded, caller-
// independent context and publishes the result on e.ready. A SUCCESS is
// cached for the TTL; a FAILURE is NOT negative-cached — the entry is
// removed so the next lookup re-resolves. Negative-caching a transient or
// caller-cancelled error would turn one bad/disconnecting client into a
// TTL-long denial of the host for everyone (the private-IP check would
// serve the cached error and 403 every CONNECT).
func (c *cachingResolver) resolve(host string, e *dnsCacheEntry) {
	rctx, cancel := context.WithTimeout(context.Background(), dnsResolveTimeout)
	defer cancel()

	addrs, err := c.upstream.LookupIPAddr(rctx, host)
	e.addrs, e.err = addrs, err
	if err == nil {
		e.expires = c.now().Add(c.ttl)
	} else {
		c.mu.Lock()
		// Only drop the entry if it is still ours (a later resolve may
		// have replaced it after expiry).
		if c.entries[host] == e {
			delete(c.entries, host)
		}
		c.mu.Unlock()
	}
	close(e.ready)
}

// isReady reports whether the entry's resolve has completed.
func isReady(e *dnsCacheEntry) bool {
	select {
	case <-e.ready:
		return true
	default:
		return false
	}
}

// maybeSweepLocked bounds the map: when over capacity it drops completed
// expired entries, and if still over, resets the map (live in-flight
// entries are dropped from the index but their waiters still complete via
// the closed channel — re-resolution on the next miss is cheap). Caller
// holds c.mu.
func (c *cachingResolver) maybeSweepLocked() {
	if len(c.entries) <= dnsCacheMaxHosts {
		return
	}
	now := c.now()
	for host, e := range c.entries {
		if isReady(e) && !now.Before(e.expires) {
			delete(c.entries, host)
		}
	}
	if len(c.entries) > dnsCacheMaxHosts {
		c.entries = make(map[string]*dnsCacheEntry, dnsCacheMaxHosts)
	}
}
