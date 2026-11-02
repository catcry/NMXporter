package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// An unchanged directory reuses the previous exact result; adding a file,
// the rescan interval or unchanged_rescan_interval: 0 force a real scan.
func TestScanDirCached_Unchanged(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &Config{MaxStatFiles: 0, UnchangedRescan: time.Hour}
	e := NewExporter(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	if r, reused := e.scanDirCached(ctx, dir); reused || r.FileCount != 2 {
		t.Fatalf("first scan: count=%d reused=%v", r.FileCount, reused)
	}
	if r, reused := e.scanDirCached(ctx, dir); !reused || r.FileCount != 2 {
		t.Errorf("unchanged: count=%d reused=%v", r.FileCount, reused)
	}

	time.Sleep(20 * time.Millisecond) // coarse directory mtime on some filesystems
	if err := os.WriteFile(filepath.Join(dir, "c"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	if r, reused := e.scanDirCached(ctx, dir); reused || r.FileCount != 3 {
		t.Errorf("after adding a file: count=%d reused=%v", r.FileCount, reused)
	}

	cfg.UnchangedRescan = time.Nanosecond // result always too old
	time.Sleep(5 * time.Millisecond)      // coarse clocks (Windows)
	if _, reused := e.scanDirCached(ctx, dir); reused {
		t.Error("an expired result must not be reused")
	}
	cfg.UnchangedRescan = 0
	if _, reused := e.scanDirCached(ctx, dir); reused {
		t.Error("unchanged_rescan_interval: 0 must always scan")
	}

	e.forgetUnchanged(nil)
	if len(e.unchanged) != 0 {
		t.Errorf("forgetUnchanged: %d left", len(e.unchanged))
	}
}
