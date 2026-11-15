package nettransport

import (
	"net/http"
	"sync"
)

// transportCacheCapacity bounds how many distinct resolved network
// configurations keep a live shared transport. Real configurations
// are a handful (one global default plus per-provider overrides), so
// the LRU only protects against unbounded growth from config churn;
// evicting an entry closes its idle connections immediately.
const transportCacheCapacity = 8

// cachedTransports is one cache entry: the transport handed out to
// callers plus the transports its resolver owns (the DoH endpoint's
// HTTP transport is hidden inside the resolver closure and would
// otherwise be unreachable for cleanup).
type cachedTransports struct {
	main     *http.Transport
	resolver []*http.Transport
	lastUsed uint64
}

// transportCache reuses one set of transports per resolved
// NetworkConfig. Entries are immutable after insertion; the mutex only
// protects the map and the LRU clock, mirroring
// boundedModelPairCache's discipline.
type transportCache struct {
	mu      sync.Mutex
	clock   uint64
	entries map[string]*cachedTransports
}

// newTransportCache returns an empty cache with the standard
// capacity.
func newTransportCache() *transportCache {
	return &transportCache{entries: make(map[string]*cachedTransports, transportCacheCapacity)}
}

var sharedTransports = newTransportCache()

// transportCacheKey canonically renders a resolved config. The \x1f
// separators cannot occur inside a URL or authority rendering, so no
// two distinct configs collide.
func transportCacheKey(rs resolved) string {
	var proxy string
	if rs.proxyURL != nil {
		proxy = rs.proxyURL.String()
	}
	return proxy + "\x1f" + rs.dnsServer + "\x1f" + rs.dohURL
}

// get returns the shared transport previously built for key, or nil,
// refreshing its LRU position on the hit.
func (c *transportCache) get(key string) *http.Transport {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[key]
	if !ok {
		return nil
	}
	c.clock++
	entry.lastUsed = c.clock
	return entry.main
}

// put records the entry built for key, evicting the least recently
// used entry when at capacity. Eviction closes the evicted transports'
// idle connections OUTSIDE the mutex; only idle connections are
// closed, so this stays safe even if a transport is still referenced
// elsewhere — in-flight requests finish and future requests re-dial.
func (c *transportCache) put(key string, main *http.Transport, resolver []*http.Transport) {
	var victim *cachedTransports
	c.mu.Lock()
	c.clock++
	if entry, ok := c.entries[key]; ok {
		entry.lastUsed = c.clock
	} else {
		if len(c.entries) >= transportCacheCapacity {
			var oldestKey string
			var oldest uint64
			for candidateKey, candidate := range c.entries {
				if oldest == 0 || candidate.lastUsed < oldest {
					oldestKey = candidateKey
					oldest = candidate.lastUsed
				}
			}
			victim = c.entries[oldestKey]
			delete(c.entries, oldestKey)
		}
		c.entries[key] = &cachedTransports{main: main, resolver: resolver, lastUsed: c.clock}
	}
	c.mu.Unlock()

	if victim != nil {
		victim.main.CloseIdleConnections()
		for _, tr := range victim.resolver {
			tr.CloseIdleConnections()
		}
	}
}

// sharedTransport returns the transport previously built for rs in
// this cache, or nil when none is cached yet.
func (c *transportCache) sharedTransport(rs resolved) *http.Transport {
	return c.get(transportCacheKey(rs))
}

// cacheTransport records main (plus any resolver-owned transports) as
// this cache's entry for rs.
func (c *transportCache) cacheTransport(rs resolved, main *http.Transport, resolver []*http.Transport) {
	c.put(transportCacheKey(rs), main, resolver)
}
