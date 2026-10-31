package main

import (
	"sync"
	"sync/atomic"
	"time"
)

// DirLabels holds the Prometheus label values derived deterministically
// from a directory's relative path under BasePath.
//
//	/data/streams/orders/buffer  →  stream="orders"  type="buffer"
//	/data/streams/orders         →  stream="orders"  type=""
type DirLabels struct {
	Base   string // the configured BASE_PATH root (e.g. "/data/streams")
	Stream string // first path component below base  (e.g. "orders")
	Type   string // remaining component(s)           (e.g. "buffer")
}

// DirMetrics holds aggregated scan output for a single monitored directory.
// Per-file os.FileInfo objects are never retained here — only the aggregates.
type DirMetrics struct {
	FileCount          int64
	OldestTimestampSec float64 // Unix epoch; 0.0 when the directory is empty
	NewestTimestampSec float64 // Unix epoch; 0.0 when the directory is empty
	ScanDurationSec    float64
	ScanSuccess        float64 // 1.0 = readable, 0.0 = permission error / vanished
	Truncated          bool    // true when MAX_FILES_PER_DIR cap or scan timeout was hit
	Labels             DirLabels
}

// CacheSnapshot is an immutable, point-in-time copy of the cache state.
// It is safe to read without holding any lock.
type CacheSnapshot struct {
	Entries        []DirMetrics
	NodeActivities []NodeActivity
	NodeBuffers    []NodeBuffer
	LastScanTime   time.Time
	Ready          bool
	ScanErrors     uint64
	ReloadTotal    uint64
	WatchedTotal   int32
}

// Cache is the central, thread-safe metrics store.
//
// Write path: background scan engine → SetBatch (exclusive lock on entries map).
// Read path:  /metrics handler        → Snapshot (shared lock, then copies).
//
// Counters (ScanErrors, ReloadTotal) use sync/atomic so scan workers can
// increment them without contending on the entries mutex.
type Cache struct {
	mu             sync.RWMutex
	entries        map[string]DirMetrics
	nodeActivities map[string]NodeActivity
	nodeBuffers    map[string]NodeBuffer
	lastScanTime   time.Time

	ready        atomic.Bool
	scanErrors   atomic.Uint64
	reloadTotal  atomic.Uint64
	watchedTotal atomic.Int32
}

// NewCache returns an empty cache. directory_cache_ready will be 0 until
// the first successful SetBatch call.
func NewCache() *Cache {
	return &Cache{
		entries:        make(map[string]DirMetrics),
		nodeActivities: make(map[string]NodeActivity),
		nodeBuffers:    make(map[string]NodeBuffer),
	}
}

// SetBatch atomically swaps the entries, node activities, and node buffer
// maps, records the scan timestamp, adds newErrors to the cumulative error
// counter, and marks the cache as ready (flipping directory_cache_ready
// from 0 → 1 on the first call).
func (c *Cache) SetBatch(entries map[string]DirMetrics, activities map[string]NodeActivity, buffers map[string]NodeBuffer, scanTime time.Time, newErrors uint64) {
	c.mu.Lock()
	c.entries = entries
	c.nodeActivities = activities
	c.nodeBuffers = buffers
	c.lastScanTime = scanTime
	c.mu.Unlock()

	if newErrors > 0 {
		c.scanErrors.Add(newErrors)
	}
	c.ready.Store(true)
}

// IncrReload bumps the cumulative /-/reload counter.
func (c *Cache) IncrReload() { c.reloadTotal.Add(1) }

// SetWatchedTotal stores the current watch-list size.
func (c *Cache) SetWatchedTotal(n int) { c.watchedTotal.Store(int32(n)) }

// IsReady reports whether the first scan cycle has finished.
func (c *Cache) IsReady() bool { return c.ready.Load() }

// Snapshot returns a deep, read-only copy of the cache state. The caller
// may inspect the returned struct freely without acquiring any lock.
func (c *Cache) Snapshot() CacheSnapshot {
	c.mu.RLock()
	entries := make([]DirMetrics, 0, len(c.entries))
	for _, v := range c.entries {
		entries = append(entries, v)
	}
	activities := make([]NodeActivity, 0, len(c.nodeActivities))
	for _, v := range c.nodeActivities {
		activities = append(activities, v)
	}
	buffers := make([]NodeBuffer, 0, len(c.nodeBuffers))
	for _, v := range c.nodeBuffers {
		buffers = append(buffers, v)
	}
	scanTime := c.lastScanTime
	c.mu.RUnlock()

	return CacheSnapshot{
		Entries:        entries,
		NodeActivities: activities,
		NodeBuffers:    buffers,
		LastScanTime:   scanTime,
		Ready:          c.ready.Load(),
		ScanErrors:     c.scanErrors.Load(),
		ReloadTotal:    c.reloadTotal.Load(),
		WatchedTotal:   c.watchedTotal.Load(),
	}
}
