package hub

import "sync"

// TitleCache is a bounded per-hub tool-call title cache so a repeated
// snapshot does not re-read session history: only keys MISSING from the
// cache trigger a history read at all. FIFO eviction at titleCacheCap.
//
// Moved verbatim from handlers_livework.go:93-125 at HEAD (sprint 0 S4,
// design §2.4 row 47, §2.2 rename titleCache→TitleCache,
// newTitleCache→NewTitleCache). The titles()/abbreviateToolInput helpers
// that read session history stay in package server (they import
// internal/app, which §2.6 forbids for hub); the cache itself is storage
// only.
type TitleCache struct {
	mu      sync.Mutex
	entries map[TitleKey]string
	fifo    []TitleKey
}

// titleCacheCap bounds TitleCache's FIFO (handlers_livework.go:102 at HEAD).
const titleCacheCap = 4096

// NewTitleCache returns an empty cache (§2.2 rename of newTitleCache).
func NewTitleCache() *TitleCache {
	return &TitleCache{entries: make(map[TitleKey]string)}
}

// TitleKey identifies ONE live-work row's title. tool_call_id is
// NOT unique (providers number calls per response, and a reused id is
// archived and re-claimed), so the key carries the owner session AND the
// row's input_hash: input_hash changes exactly when a row is re-claimed
// with different input, which is the only way the title text can change —
// same hash means the same input, so the cached title is still correct.
// Per-hub (see Hub.liveWorkTitles), never package-global: a global
// id-keyed cache leaked one session's titles into another session's panel.
//
// Exported (design delta, report L4): package-server live-work code builds
// keys against the cache until L5, so the fields must be settable outside
// hub (provenance: liveWorkTitleKey, handlers_livework.go:79-91 at HEAD).
type TitleKey struct {
	Owner      string
	ToolCallID string
	InputHash  string
}

// Get returns the cached title for key (§2.2 delta: exported — the only
// cache readers outside hub are the server live-work helpers until L5;
// provenance: (c *titleCache) get, handlers_livework.go:108 at HEAD).
func (c *TitleCache) Get(key TitleKey) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.entries[key]
	return t, ok
}

// Put caches title for key (provenance: (c *titleCache) put,
// handlers_livework.go:115 at HEAD).
func (c *TitleCache) Put(key TitleKey, title string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.entries[key]; exists {
		return
	}
	c.entries[key] = title
	c.fifo = append(c.fifo, key)
	for len(c.fifo) > titleCacheCap {
		delete(c.entries, c.fifo[0])
		c.fifo = c.fifo[1:]
	}
}
