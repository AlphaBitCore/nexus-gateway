// Package configcache provides two complementary in-memory caching primitives
// for data-plane configuration:
//
//   - SnapshotCache[T] holds a small full table and serves lock-free reads
//     via an atomic pointer. Reload is whole-table replacement. Suited to
//     providers, models, credentials, routing rules, hook configs, quota
//     policies — tables with at most a few hundred rows per customer.
//
//   - KeyCache[K,V] is a per-key lazy LRU with TTL and singleflight
//     coalescing. It does not preload. Suited to virtual_keys, users, orgs,
//     projects — tables that can grow large and where pushing the full
//     table on every change would have unacceptable write amplification.
//
// Both caches are intended to be invalidated by the Hub thingclient
// OnConfigChanged callback. SnapshotCache is invalidated by calling Reload.
// KeyCache is invalidated by calling Invalidate with the affected keys.
package configcache
