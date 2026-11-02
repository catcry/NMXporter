package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Exporter struct {
	cfg   *Config
	log   *slog.Logger
	cache *Cache
	wl    *WatchList

	scanMu   sync.Mutex
	stopCh   chan struct{}
	wg       sync.WaitGroup
	reloadCh chan struct{}

	unchangedMu sync.Mutex
	unchanged   map[string]unchangedScan // last complete scan per directory
}

// unchangedScan is a scan result kept while the directory does not change.
type unchangedScan struct {
	stamp dirStamp
	res   ScanResult
	at    time.Time
}

// scanDirCached scans path, or reuses the previous result when the
// directory's entry list has not changed since (same dirStamp) and the
// result is younger than unchanged_rescan_interval. A huge queue that does
// not move then costs one stat per cycle instead of reading every entry.
// Returns the result and whether it was reused.
func (e *Exporter) scanDirCached(ctx context.Context, path string) (ScanResult, bool) {
	start := time.Now()
	var stamp dirStamp
	stamped := false
	if e.cfg.UnchangedRescan > 0 {
		if fi, err := os.Stat(path); err == nil {
			stamp, stamped = stampOf(fi)
		}
	}
	if stamped {
		e.unchangedMu.Lock()
		c, ok := e.unchanged[path]
		e.unchangedMu.Unlock()
		if ok && c.stamp == stamp && time.Since(c.at) < e.cfg.UnchangedRescan {
			r := c.res
			r.ScanDuration = time.Since(start).Seconds()
			return r, true
		}
	}
	res := scanDir(ctx, path, e.cfg.MaxFilesPerDir, e.cfg.MaxStatFiles)
	// Keep only complete scans (a stat-sampled one is complete for the count).
	if stamped && res.Err == nil && (!res.Truncated || res.StatLimited) {
		e.unchangedMu.Lock()
		if e.unchanged == nil {
			e.unchanged = make(map[string]unchangedScan)
		}
		e.unchanged[path] = unchangedScan{stamp: stamp, res: res, at: start}
		e.unchangedMu.Unlock()
	}
	return res, false
}

// forgetUnchanged drops kept results of directories no longer watched.
func (e *Exporter) forgetUnchanged(watched []WatchEntry) {
	keep := make(map[string]bool, len(watched))
	for _, w := range watched {
		keep[w.AbsPath] = true
	}
	e.unchangedMu.Lock()
	for p := range e.unchanged {
		if !keep[p] {
			delete(e.unchanged, p)
		}
	}
	e.unchangedMu.Unlock()
}

func NewExporter(cfg *Config, log *slog.Logger) *Exporter {
	return &Exporter{
		cfg:      cfg,
		log:      log,
		cache:    NewCache(),
		wl:       NewWatchList(),
		stopCh:   make(chan struct{}),
		reloadCh: make(chan struct{}, 1),
	}
}

// ValidateTargets checks that every configured base path exists and is readable.
// Logs a detailed error but does not abort startup — missing mounts are reported
// via scrape_success=0 in the metrics rather than crashing the process.
func (e *Exporter) ValidateTargets() {
	for _, t := range e.cfg.Targets {
		info, err := os.Stat(t.Base)
		if err != nil {
			e.log.Error("target base path not accessible — check your volume mount",
				"base", t.Base, "error", err)
			continue
		}
		if !info.IsDir() {
			e.log.Error("target base path is not a directory", "base", t.Base)
			continue
		}
		msg := fmt.Sprintf("base %q OK", t.Base)
		switch {
		case len(t.Dirs) > 0:
			msg += fmt.Sprintf(", watching %d explicit dirs", len(t.Dirs))
		case t.Pattern != "":
			msg += fmt.Sprintf(", auto-discover pattern %q up to depth %d", t.Pattern, t.MaxDepth)
			if len(t.Include) > 0 {
				msg += fmt.Sprintf(" (include: %s)", strings.Join(t.Include, ", "))
			}
		default:
			msg += fmt.Sprintf(", auto-discover up to depth %d", t.MaxDepth)
		}
		e.log.Info("target validated", "detail", msg)
	}
}

func (e *Exporter) Discover() error {
	entries, err := discoverTargets(e.cfg.Targets)
	e.wl.Set(entries)
	e.cache.SetWatchedTotal(len(entries))
	if err != nil {
		e.log.Error("discovery error", "error", err)
	}
	e.log.Info("watch list updated", "directories", len(entries))
	return err
}

func (e *Exporter) TriggerReload() {
	e.cache.IncrReload()
	select {
	case e.reloadCh <- struct{}{}:
	default:
	}
}

func (e *Exporter) Stop() {
	close(e.stopCh)
	e.wg.Wait()
}

func (e *Exporter) IsReady() bool           { return e.cache.IsReady() }
func (e *Exporter) Snapshot() CacheSnapshot { return e.cache.Snapshot() }

func (e *Exporter) RunScanLoop() {
	e.wg.Add(1)
	defer e.wg.Done()

	e.ScanAll()

	ticker := time.NewTicker(e.cfg.ScanInterval)
	defer ticker.Stop()

	for {
		select {
		case <-e.stopCh:
			return
		case <-ticker.C:
			e.ScanAll()
		case <-e.reloadCh:
			e.log.Info("reload signal — re-discovering")
			if err := e.Discover(); err != nil {
				e.log.Error("reload discovery failed", "error", err)
			}
			e.ScanAll()
		}
	}
}

func (e *Exporter) RunDiscoveryLoop() {
	e.wg.Add(1)
	defer e.wg.Done()

	ticker := time.NewTicker(e.cfg.DiscoveryInterval)
	defer ticker.Stop()

	for {
		select {
		case <-e.stopCh:
			return
		case <-ticker.C:
			e.log.Info("periodic re-discovery starting")
			if err := e.Discover(); err != nil {
				e.log.Error("periodic discovery failed", "error", err)
			}
			select {
			case e.reloadCh <- struct{}{}:
			default:
			}
		}
	}
}

// ScanAll fans out workers with a hard wall-clock timeout (SCAN_TIMEOUT).
func (e *Exporter) ScanAll() {
	e.scanMu.Lock()
	defer e.scanMu.Unlock()

	watched := e.wl.Get()
	if len(watched) == 0 {
		nodes := DiscoverStreamNodes(e.cfg.Targets)
		activities := ScanAllNodeActivities(context.Background(), nodes, e.cfg.TracelogDir, e.cfg.MinDelay, e.cfg.TraceLookbackDays, e.cfg.ScanWorkers, time.Now())
		buffers := ScanAllNodeBuffers(context.Background(), nodes, make(map[string]DirMetrics), nil, e.cfg.ScanWorkers)
		e.cache.SetNodeStatuses(ScanAllNodeStatus(nodes, e.cfg.HeartbeatGrace, time.Now()))
		e.cache.SetBatch(make(map[string]DirMetrics), activities, buffers, time.Now(), 0)
		e.log.Warn("watch list is empty — check your targets config and volume mounts")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), e.cfg.ScanTimeout)
	defer cancel()

	cycleStart := time.Now()
	results := make(map[string]DirMetrics, len(watched))
	var resultsMu sync.Mutex
	var errCount atomic.Uint64
	var truncCount atomic.Uint64
	var statLimitedCount atomic.Uint64
	var reusedCount atomic.Uint64
	e.forgetUnchanged(watched)

	var activities map[string]NodeActivity
	var actWg sync.WaitGroup
	nodes := DiscoverStreamNodes(e.cfg.Targets)
	actWg.Add(1)
	go func() {
		defer actWg.Done()
		activities = ScanAllNodeActivities(ctx, nodes, e.cfg.TracelogDir, e.cfg.MinDelay, e.cfg.TraceLookbackDays, e.cfg.ScanWorkers, time.Now())
	}()

	sem := make(chan struct{}, e.cfg.ScanWorkers)
	var wg sync.WaitGroup

	for _, we := range watched {
		if ctx.Err() != nil {
			break
		}

		wg.Add(1)
		sem <- struct{}{}

		go func(entry WatchEntry) {
			defer wg.Done()
			defer func() { <-sem }()

			res, reused := e.scanDirCached(ctx, entry.AbsPath)
			if reused {
				reusedCount.Add(1)
			}
			if res.Err != nil {
				errCount.Add(1)
				e.log.Debug("scan error", "path", entry.AbsPath, "error", res.Err)
			}
			switch {
			case res.StatLimited:
				// Count is exact; only oldest/newest come from the first
				// max_stat_files entries. Expected for huge queues.
				statLimitedCount.Add(1)
				e.log.Debug("timestamps sampled", "path", entry.AbsPath,
					"files", res.FileCount, "max_stat_files", e.cfg.MaxStatFiles)
			case res.Truncated:
				truncCount.Add(1)
				e.log.Warn("scan truncated",
					"path", entry.AbsPath,
					"files_read", res.FileCount,
					"reason", "timeout or MAX_FILES_PER_DIR cap")
			}

			dm := DirMetrics{
				FileCount:          res.FileCount,
				OldestTimestampSec: res.OldestTimestamp,
				NewestTimestampSec: res.NewestTimestamp,
				OldestCollectedSec: res.OldestCollected,
				ScanDurationSec:    res.ScanDuration,
				ScanSuccess:        res.ScanSuccess,
				Truncated:          res.Truncated,
				Labels:             entry.Labels,
			}
			resultsMu.Lock()
			results[entry.AbsPath] = dm
			resultsMu.Unlock()
		}(we)
	}

	wg.Wait()
	actWg.Wait()

	// Buffer backlogs reuse the just-completed directory scan when the input
	// buffer path coincides with a watched directory (zero extra I/O);
	// external SourceDirectory paths get a cheap count-only scan.
	buffers := ScanAllNodeBuffers(ctx, nodes, results, nil, e.cfg.ScanWorkers)
	e.cache.SetNodeStatuses(ScanAllNodeStatus(nodes, e.cfg.HeartbeatGrace, time.Now()))

	elapsed := time.Since(cycleStart)
	timedOut := ctx.Err() != nil
	errors := errCount.Load()
	truncs := truncCount.Load()

	e.cache.SetBatch(results, activities, buffers, time.Now(), errors)

	if timedOut {
		e.log.Warn("scan cycle timed out — partial results written to cache",
			"timeout", e.cfg.ScanTimeout,
			"completed", len(results),
			"total", len(watched),
			"truncated_dirs", truncs,
			"duration", elapsed.Round(time.Millisecond),
		)
	} else {
		e.log.Info("scan cycle complete",
			"directories", len(watched),
			"errors", errors,
			"truncated_dirs", truncs,
			"timestamp_sampled_dirs", statLimitedCount.Load(),
			"unchanged_dirs", reusedCount.Load(),
			"duration", elapsed.Round(time.Millisecond),
		)
	}
}
