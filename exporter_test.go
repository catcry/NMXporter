package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ── config ────────────────────────────────────────────────────────────────────

func TestLoadConfig_RequiredMissing(t *testing.T) {
	t.Setenv("BASE_PATH", "")
	t.Setenv("RELOAD_SECRET", "")
	if _, err := LoadConfig(nil); err == nil {
		t.Fatal("expected error for missing BASE_PATH")
	}
}

func TestLoadConfig_Defaults(t *testing.T) {
	t.Setenv("BASE_PATH", "/tmp")
	t.Setenv("RELOAD_SECRET", "secret")
	cfg, err := LoadConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Targets) == 0 || cfg.Targets[0].MaxDepth != 1 {
		t.Errorf("fallback target MaxDepth: got %d want 1", cfg.Targets[0].MaxDepth)
	}
	if cfg.ScanWorkers != 2 {
		t.Errorf("ScanWorkers: got %d want 2", cfg.ScanWorkers)
	}
}

func TestLoadConfig_InvalidMaxDepthFallsBack(t *testing.T) {
	t.Setenv("BASE_PATH", "/tmp")
	t.Setenv("RELOAD_SECRET", "secret")
	t.Setenv("MAX_DEPTH", "0")
	cfg, err := LoadConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Targets[0].MaxDepth != 1 {
		t.Errorf("MaxDepth: got %d want 1 (zero env falls back to default)", cfg.Targets[0].MaxDepth)
	}
}

func TestLoadConfig_WithInclude(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "targets.yml")
	yamlContent := `
targets:
  - base: /streams
    label: /streams
    pattern: "*/nodes/*/#include/*"
    include: ["input", "output", "discarded", "rejected"]
`
	if err := os.WriteFile(configFile, []byte(yamlContent), 0644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CONFIG_FILE", configFile)
	t.Setenv("RELOAD_SECRET", "secret")

	cfg, err := LoadConfig(nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(cfg.Targets) != 1 {
		t.Fatalf("expected 1 target, got %d", len(cfg.Targets))
	}

	target := cfg.Targets[0]
	if target.Pattern != "*/nodes/*/#include/*" {
		t.Errorf("Pattern: got %q want */nodes/*/#include/*", target.Pattern)
	}
	if len(target.Include) != 4 {
		t.Fatalf("expected 4 include items, got %d", len(target.Include))
	}
	if target.Include[0] != "input" || target.Include[3] != "rejected" {
		t.Errorf("unexpected include items: %+v", target.Include)
	}
}

func TestLoadConfig_WithTracelogAndDelay(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "targets.yml")
	yamlContent := `
tracelog_dir: /var/log/tracelog
min_delay: 5m
trace_lookback_days: 14
targets:
  - base: /streams
`
	if err := os.WriteFile(configFile, []byte(yamlContent), 0644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CONFIG_FILE", configFile)
	t.Setenv("RELOAD_SECRET", "secret")

	cfg, err := LoadConfig(nil)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.TracelogDir != "/var/log/tracelog" {
		t.Errorf("TracelogDir: got %q want /var/log/tracelog", cfg.TracelogDir)
	}
	if cfg.MinDelay != 5*time.Minute {
		t.Errorf("MinDelay: got %v want 5m", cfg.MinDelay)
	}
	if cfg.TraceLookbackDays != 14 {
		t.Errorf("TraceLookbackDays: got %d want 14", cfg.TraceLookbackDays)
	}
}

// ── scanner ───────────────────────────────────────────────────────────────────

func TestScanDir_Empty(t *testing.T) {
	dir := t.TempDir()
	r := scanDir(context.Background(), dir, 0, 0)
	if r.ScanSuccess != 1 {
		t.Errorf("empty dir: ScanSuccess=%g want 1", r.ScanSuccess)
	}
	if r.FileCount != 0 {
		t.Errorf("empty dir: FileCount=%d want 0", r.FileCount)
	}
	if r.OldestTimestamp != 0 || r.NewestTimestamp != 0 {
		t.Error("empty dir: timestamps should be zero")
	}
}

func TestScanDir_WithFiles(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	r := scanDir(context.Background(), dir, 0, 0)
	if r.ScanSuccess != 1 {
		t.Errorf("ScanSuccess=%g want 1", r.ScanSuccess)
	}
	if r.FileCount != 3 {
		t.Errorf("FileCount=%d want 3", r.FileCount)
	}
	if r.OldestTimestamp == 0 || r.NewestTimestamp == 0 {
		t.Error("timestamps should be non-zero when files exist")
	}
}

func TestScanDir_SubdirNotCounted(t *testing.T) {
	dir := t.TempDir()
	_ = os.Mkdir(filepath.Join(dir, "subdir"), 0755)
	os.WriteFile(filepath.Join(dir, "file.txt"), []byte("x"), 0644)
	r := scanDir(context.Background(), dir, 0, 0)
	if r.FileCount != 1 {
		t.Errorf("FileCount=%d want 1 (subdirs must not be counted)", r.FileCount)
	}
}

func TestScanDir_Nonexistent(t *testing.T) {
	r := scanDir(context.Background(), "/nonexistent/path/that/cannot/exist", 0, 0)
	if r.ScanSuccess != 0 {
		t.Errorf("ScanSuccess=%g want 0 for missing dir", r.ScanSuccess)
	}
	if r.Err == nil {
		t.Error("Err should be non-nil for missing dir")
	}
}

func TestScanDir_SymlinkDirNotCountedAndSymlinkFileCounted(t *testing.T) {
	dir := t.TempDir()
	extDir := t.TempDir()

	// Create a real file in extDir and symlink to it
	realFile := filepath.Join(extDir, "target_file.txt")
	if err := os.WriteFile(realFile, []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realFile, filepath.Join(dir, "symlink_file.txt")); err != nil {
		t.Fatal(err)
	}

	// Create a real dir in extDir and symlink to it
	realSubDir := filepath.Join(extDir, "target_subdir")
	if err := os.MkdirAll(realSubDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realSubDir, filepath.Join(dir, "symlink_dir")); err != nil {
		t.Fatal(err)
	}

	// Create a regular file and regular subdir
	if err := os.WriteFile(filepath.Join(dir, "regular.txt"), []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "regular_subdir"), 0755); err != nil {
		t.Fatal(err)
	}

	// Create a broken symlink
	if err := os.Symlink(filepath.Join(extDir, "nonexistent.txt"), filepath.Join(dir, "broken_link")); err != nil {
		t.Fatal(err)
	}

	r := scanDir(context.Background(), dir, 0, 0)
	if r.ScanSuccess != 1 {
		t.Errorf("ScanSuccess=%g want 1", r.ScanSuccess)
	}
	// Expected files: regular.txt and symlink_file.txt (2 files).
	// symlink_dir, regular_subdir, and broken_link must NOT be counted as files.
	if r.FileCount != 2 {
		t.Errorf("FileCount=%d want 2 (1 regular file + 1 symlink file)", r.FileCount)
	}
	if r.OldestTimestamp == 0 || r.NewestTimestamp == 0 {
		t.Error("timestamps should be non-zero when files exist")
	}
}

func TestScanDir_ScanningSymlinkTargetDir(t *testing.T) {
	base := t.TempDir()
	realDir := filepath.Join(base, "real_storage")
	if err := os.MkdirAll(realDir, 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"file1.txt", "file2.txt", "file3.txt"} {
		if err := os.WriteFile(filepath.Join(realDir, name), []byte("content"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	symlinkDir := filepath.Join(base, "symlink_storage")
	if err := os.Symlink(realDir, symlinkDir); err != nil {
		t.Fatal(err)
	}

	r := scanDir(context.Background(), symlinkDir, 0, 0)
	if r.ScanSuccess != 1 {
		t.Errorf("ScanSuccess=%g want 1", r.ScanSuccess)
	}
	if r.FileCount != 3 {
		t.Errorf("FileCount=%d want 3 for symlinked directory", r.FileCount)
	}
}

// ── watchlist / discovery ─────────────────────────────────────────────────────

func TestDiscoverDirectories_Depth1(t *testing.T) {
	base := t.TempDir()
	os.MkdirAll(filepath.Join(base, "orders"), 0755)
	os.MkdirAll(filepath.Join(base, "payments"), 0755)

	entries, err := discoverDirectories(base, base, 1, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("got %d entries want 2", len(entries))
	}
	for _, e := range entries {
		if e.Labels.Type != "" {
			t.Errorf("depth-1 entry should have empty Type, got %q", e.Labels.Type)
		}
	}
}

func TestDiscoverDirectories_PatternFilter(t *testing.T) {
	base := t.TempDir()
	streamsBase := filepath.Join(base, "streams")
	// Create a mix of directories; only those matching */nodes/*/input should be watched.
	paths := []string{
		"Nokia/nodes/Enc_LIMVNO_1/input",
		"Nokia/nodes/Enc_LIMVNO_2/input",
		"Nokia/nodes/Enc_LIMVNO_1/output", // should be ignored
		"Nokia/buffer",                    // should be ignored
		"Ericsson/nodes/Node_A/input",
	}
	for _, p := range paths {
		os.MkdirAll(filepath.Join(streamsBase, p), 0755)
	}

	entries, err := discoverDirectories(streamsBase, streamsBase, 4, "*/nodes/*/input", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("got %d entries want 3:\n%+v", len(entries), entries)
	}
	for _, e := range entries {
		if !strings.Contains(e.AbsPath, "input") {
			t.Errorf("unexpected watch path %q", e.AbsPath)
		}
	}
}

func TestDiscoverDirectories_SymlinkToDir(t *testing.T) {
	base := t.TempDir()
	streamsBase := filepath.Join(base, "streams")

	// Create a real directory outside the base to serve as the symlink target.
	target := filepath.Join(base, "real_target")
	os.MkdirAll(filepath.Join(target, "data"), 0755)
	os.WriteFile(filepath.Join(target, "data", "file.txt"), []byte("x"), 0644)

	// Create the tree structure with symlinks inside input/.
	os.MkdirAll(filepath.Join(streamsBase, "Nokia", "nodes", "BLN_1", "input"), 0755)
	os.Symlink(target, filepath.Join(streamsBase, "Nokia", "nodes", "BLN_1", "input", "STREAM_A"))

	// Pattern: */nodes/*/input/* should match the symlink target.
	entries, err := discoverDirectories(streamsBase, "/streams", 5, "*/nodes/*/input/*", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries want 1 (symlink-to-dir should be included):\n%+v", len(entries), entries)
	}
	if entries[0].Labels.Stream != "Nokia" {
		t.Errorf("Stream=%q want Nokia", entries[0].Labels.Stream)
	}
	if entries[0].Labels.Type != "BLN_1/STREAM_A" {
		t.Errorf("Type=%q want BLN_1/STREAM_A", entries[0].Labels.Type)
	}
}

func TestDiscoverDirectories_DynamicTypeFromPattern(t *testing.T) {
	base := t.TempDir()
	streamsBase := filepath.Join(base, "streams")
	os.MkdirAll(filepath.Join(streamsBase, "Back_Dump", "nodes", "Back_Dump", "input", "COLLECTED_0_1020"), 0755)

	entries, err := discoverDirectories(streamsBase, "/streams", 5, "*/nodes/*/input/*", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries want 1:\n%+v", len(entries), entries)
	}
	if entries[0].Labels.Stream != "Back_Dump" {
		t.Errorf("Stream=%q want Back_Dump", entries[0].Labels.Stream)
	}
	if entries[0].Labels.Type != "Back_Dump/COLLECTED_0_1020" {
		t.Errorf("Type=%q want Back_Dump/COLLECTED_0_1020", entries[0].Labels.Type)
	}
}

func TestDiscoverDirectories_SymlinkToFileIgnored(t *testing.T) {
	base := t.TempDir()
	streamsBase := filepath.Join(base, "streams")

	// Create a regular file to symlink to (not a directory).
	target := filepath.Join(base, "somefile.txt")
	os.WriteFile(target, []byte("x"), 0644)

	os.MkdirAll(filepath.Join(streamsBase, "Nokia", "nodes", "BLN_1", "input"), 0755)
	os.Symlink(target, filepath.Join(streamsBase, "Nokia", "nodes", "BLN_1", "input", "FILE_LINK"))

	entries, err := discoverDirectories(streamsBase, "/streams", 5, "*/nodes/*/input/*", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("got %d entries want 0 (symlink-to-file should be ignored):\n%+v", len(entries), entries)
	}
}

func TestDiscoverDirectories_Depth2Labels(t *testing.T) {
	base := t.TempDir()
	os.MkdirAll(filepath.Join(base, "orders", "buffer"), 0755)

	entries, err := discoverDirectories(base, base, 2, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Expects two entries: "orders" (depth 1) and "orders/buffer" (depth 2).
	if len(entries) != 2 {
		t.Fatalf("got %d entries want 2", len(entries))
	}
	// Find the depth-2 entry.
	var leaf *WatchEntry
	for i := range entries {
		if entries[i].Labels.Type != "" {
			leaf = &entries[i]
		}
	}
	if leaf == nil {
		t.Fatal("no depth-2 entry found")
	}
	if leaf.Labels.Stream != "orders" {
		t.Errorf("Stream=%q want orders", leaf.Labels.Stream)
	}
	if leaf.Labels.Type != "buffer" {
		t.Errorf("Type=%q want buffer", leaf.Labels.Type)
	}
}

func TestDiscoverDirectories_IntermediateSymlinks(t *testing.T) {
	base := t.TempDir()
	streamsBase := filepath.Join(base, "streams")
	extBase := filepath.Join(base, "external_install")

	// Set up the node directory structure matching the user's example
	nodeDir := filepath.Join(streamsBase, "Nokia", "nodes", "Enc_CPM")
	os.MkdirAll(nodeDir, 0755)

	// Real subdirectories
	os.MkdirAll(filepath.Join(nodeDir, "input"), 0755)
	os.MkdirAll(filepath.Join(nodeDir, "output"), 0755)

	// Symbolic links pointing to external directories
	symlinks := []string{
		"audit", "bin", "control", "discarded", "log",
		"rejected", "reprocess", "status", "storage", "temp",
	}
	for _, name := range symlinks {
		targetDir := filepath.Join(extBase, name)
		os.MkdirAll(targetDir, 0755)
		if err := os.Symlink(targetDir, filepath.Join(nodeDir, name)); err != nil {
			t.Fatal(err)
		}
	}

	// Pattern: */nodes/*/* (depth 4)
	entries, err := discoverDirectories(streamsBase, "/streams", 4, "*/nodes/*/*", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Total expected: 2 real dirs + 10 symlinked dirs = 12 entries
	if len(entries) != 12 {
		t.Fatalf("got %d entries want 12:\n%+v", len(entries), entries)
	}

	foundTypes := make(map[string]bool)
	for _, e := range entries {
		if e.Labels.Stream != "Nokia" {
			t.Errorf("expected Stream=Nokia, got %q", e.Labels.Stream)
		}
		foundTypes[e.Labels.Type] = true
	}

	expectedTypes := []string{
		"Enc_CPM/input", "Enc_CPM/output", "Enc_CPM/audit", "Enc_CPM/bin",
		"Enc_CPM/control", "Enc_CPM/discarded", "Enc_CPM/log", "Enc_CPM/rejected",
		"Enc_CPM/reprocess", "Enc_CPM/status", "Enc_CPM/storage", "Enc_CPM/temp",
	}
	for _, exp := range expectedTypes {
		if !foundTypes[exp] {
			t.Errorf("missing expected type label %q", exp)
		}
	}
}

func TestDiscoverDirectories_DeepSymlinkTraversal(t *testing.T) {
	base := t.TempDir()
	streamsBase := filepath.Join(base, "streams")
	extBase := filepath.Join(base, "external_install")

	nodeDir := filepath.Join(streamsBase, "Nokia", "nodes", "Enc_CPM")
	os.MkdirAll(nodeDir, 0755)

	// discarded is a symlink pointing to an external directory
	discardedTarget := filepath.Join(extBase, "discarded")
	os.MkdirAll(filepath.Join(discardedTarget, "queue_1"), 0755)
	os.MkdirAll(filepath.Join(discardedTarget, "queue_2"), 0755)
	if err := os.Symlink(discardedTarget, filepath.Join(nodeDir, "discarded")); err != nil {
		t.Fatal(err)
	}

	// input is a real directory with a subdirectory
	os.MkdirAll(filepath.Join(nodeDir, "input", "queue_in"), 0755)

	// Pattern: */nodes/*/*/* (depth 5)
	entries, err := discoverDirectories(streamsBase, "/streams", 5, "*/nodes/*/*/*", nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 3 {
		t.Fatalf("got %d entries want 3:\n%+v", len(entries), entries)
	}

	foundTypes := make(map[string]bool)
	for _, e := range entries {
		if e.Labels.Stream != "Nokia" {
			t.Errorf("expected Stream=Nokia, got %q", e.Labels.Stream)
		}
		foundTypes[e.Labels.Type] = true
	}

	for _, exp := range []string{"Enc_CPM/discarded/queue_1", "Enc_CPM/discarded/queue_2", "Enc_CPM/input/queue_in"} {
		if !foundTypes[exp] {
			t.Errorf("missing expected type label %q in %+v", exp, foundTypes)
		}
	}
}

func TestDiscoverDirectories_SymlinkNodeDir(t *testing.T) {
	base := t.TempDir()
	streamsBase := filepath.Join(base, "streams")
	extBase := filepath.Join(base, "external_nodes")

	// Enc_CPM itself is a symlink to an external node directory
	realNodeDir := filepath.Join(extBase, "Enc_CPM")
	os.MkdirAll(filepath.Join(realNodeDir, "input"), 0755)
	os.MkdirAll(filepath.Join(realNodeDir, "output"), 0755)

	// And inside the external node directory, discarded is also a symlink
	discardedTarget := filepath.Join(base, "discarded_data")
	os.MkdirAll(discardedTarget, 0755)
	if err := os.Symlink(discardedTarget, filepath.Join(realNodeDir, "discarded")); err != nil {
		t.Fatal(err)
	}

	nodesDir := filepath.Join(streamsBase, "Nokia", "nodes")
	os.MkdirAll(nodesDir, 0755)
	if err := os.Symlink(realNodeDir, filepath.Join(nodesDir, "Enc_CPM")); err != nil {
		t.Fatal(err)
	}

	// Pattern: */nodes/*/* (depth 4)
	entries, err := discoverDirectories(streamsBase, "/streams", 4, "*/nodes/*/*", nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 3 {
		t.Fatalf("got %d entries want 3:\n%+v", len(entries), entries)
	}

	foundTypes := make(map[string]bool)
	for _, e := range entries {
		foundTypes[e.Labels.Type] = true
	}
	for _, exp := range []string{"Enc_CPM/input", "Enc_CPM/output", "Enc_CPM/discarded"} {
		if !foundTypes[exp] {
			t.Errorf("missing expected type label %q", exp)
		}
	}
}

func TestDiscoverDirectories_SymlinkCycleIgnored(t *testing.T) {
	base := t.TempDir()
	streamsBase := filepath.Join(base, "streams")

	nodeDir := filepath.Join(streamsBase, "Nokia", "nodes", "Enc_CPM")
	os.MkdirAll(filepath.Join(nodeDir, "input"), 0755)

	// Create a recursive symlink cycle
	if err := os.Symlink(nodeDir, filepath.Join(nodeDir, "self_loop")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(streamsBase, filepath.Join(nodeDir, "root_loop")); err != nil {
		t.Fatal(err)
	}

	// Should not hang or loop infinitely
	entries, err := discoverDirectories(streamsBase, "/streams", 4, "*/nodes/*/*", nil)
	if err != nil {
		t.Fatal(err)
	}

	// input is found
	foundInput := false
	for _, e := range entries {
		if e.Labels.Type == "Enc_CPM/input" {
			foundInput = true
		}
	}
	if !foundInput {
		t.Error("expected Enc_CPM/input to be discovered despite symlink cycles")
	}
}

func TestDiscoverDirectories_BrokenSymlinkIgnored(t *testing.T) {
	base := t.TempDir()
	streamsBase := filepath.Join(base, "streams")

	nodeDir := filepath.Join(streamsBase, "Nokia", "nodes", "Enc_CPM")
	os.MkdirAll(filepath.Join(nodeDir, "input"), 0755)

	// Create broken symlink
	if err := os.Symlink(filepath.Join(base, "nonexistent_target"), filepath.Join(nodeDir, "broken")); err != nil {
		t.Fatal(err)
	}

	entries, err := discoverDirectories(streamsBase, "/streams", 4, "*/nodes/*/*", nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 1 {
		t.Fatalf("got %d entries want 1 (broken symlink should be ignored):\n%+v", len(entries), entries)
	}
	if entries[0].Labels.Type != "Enc_CPM/input" {
		t.Errorf("Type=%q want Enc_CPM/input", entries[0].Labels.Type)
	}
}

func TestDiscoverDirectories_PatternWithInclude(t *testing.T) {
	base := t.TempDir()
	streamsBase := filepath.Join(base, "streams")
	extBase := filepath.Join(base, "external_install")

	nodeDir := filepath.Join(streamsBase, "Nokia", "nodes", "Enc_CPM")
	os.MkdirAll(nodeDir, 0755)

	// Create real directories
	os.MkdirAll(filepath.Join(nodeDir, "input", "stream_in"), 0755)
	os.MkdirAll(filepath.Join(nodeDir, "output", "stream_out"), 0755)

	// Create symlinked directories
	allSymlinks := []string{
		"audit", "bin", "control", "discarded", "log",
		"rejected", "reprocess", "status", "storage", "temp",
	}
	for _, name := range allSymlinks {
		targetDir := filepath.Join(extBase, name)
		os.MkdirAll(filepath.Join(targetDir, "sub_queue"), 0755)
		if err := os.Symlink(targetDir, filepath.Join(nodeDir, name)); err != nil {
			t.Fatal(err)
		}
	}

	include := []string{"input", "output", "discarded", "rejected"}
	entries, err := discoverDirectories(streamsBase, "/streams", 5, "*/nodes/*/#include/*", include)
	if err != nil {
		t.Fatal(err)
	}

	// Only input, output, discarded, rejected subqueues should be discovered (4 total)
	if len(entries) != 4 {
		t.Fatalf("got %d entries want 4:\n%+v", len(entries), entries)
	}

	foundTypes := make(map[string]bool)
	for _, e := range entries {
		if e.Labels.Stream != "Nokia" {
			t.Errorf("expected Stream=Nokia, got %q", e.Labels.Stream)
		}
		foundTypes[e.Labels.Type] = true
	}

	expectedTypes := []string{
		"Enc_CPM/input/stream_in",
		"Enc_CPM/output/stream_out",
		"Enc_CPM/discarded/sub_queue",
		"Enc_CPM/rejected/sub_queue",
	}
	for _, exp := range expectedTypes {
		if !foundTypes[exp] {
			t.Errorf("missing expected type label %q, found: %+v", exp, foundTypes)
		}
	}
}

func TestDiscoverDirectories_PatternIncludeDirect(t *testing.T) {
	base := t.TempDir()
	streamsBase := filepath.Join(base, "streams")
	extBase := filepath.Join(base, "external_install")

	nodeDir := filepath.Join(streamsBase, "Nokia", "nodes", "Enc_CPM")
	os.MkdirAll(nodeDir, 0755)

	os.MkdirAll(filepath.Join(nodeDir, "input"), 0755)
	os.MkdirAll(filepath.Join(nodeDir, "output"), 0755)

	for _, name := range []string{"audit", "bin", "discarded", "rejected", "temp"} {
		targetDir := filepath.Join(extBase, name)
		os.MkdirAll(targetDir, 0755)
		if err := os.Symlink(targetDir, filepath.Join(nodeDir, name)); err != nil {
			t.Fatal(err)
		}
	}

	targets := []Target{
		{
			Base:    streamsBase,
			Label:   "/streams",
			Pattern: "*/nodes/*/#include",
			Include: []string{"input", "output", "discarded", "rejected"},
		},
	}

	entries, err := discoverTargets(targets)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 4 {
		t.Fatalf("got %d entries want 4:\n%+v", len(entries), entries)
	}

	foundTypes := make(map[string]bool)
	for _, e := range entries {
		foundTypes[e.Labels.Type] = true
	}

	for _, exp := range []string{"Enc_CPM/input", "Enc_CPM/output", "Enc_CPM/discarded", "Enc_CPM/rejected"} {
		if !foundTypes[exp] {
			t.Errorf("missing expected type %q in %+v", exp, foundTypes)
		}
	}
}

// ── metrics rendering ─────────────────────────────────────────────────────────

func TestRenderMetrics_NotReady(t *testing.T) {
	var buf bytes.Buffer
	RenderMetrics(&buf, CacheSnapshot{Ready: false})
	out := buf.String()
	if !strings.Contains(out, "directory_cache_ready 0") {
		t.Error("expected directory_cache_ready 0 when not ready")
	}
	// No other metric families should appear during cold start.
	if strings.Contains(out, "directory_file_count") {
		t.Error("unexpected directory_file_count in cold-start output")
	}
}

func TestRenderMetrics_Ready(t *testing.T) {
	snap := CacheSnapshot{
		Ready:        true,
		LastScanTime: time.Now(),
		WatchedTotal: 1,
		Entries: []DirMetrics{
			{
				FileCount:          42,
				OldestTimestampSec: 1_700_000_000,
				NewestTimestampSec: 1_700_001_000,
				ScanDurationSec:    0.005,
				ScanSuccess:        1,
				Labels:             DirLabels{Base: "/streams", Stream: "orders", Type: "buffer"},
			},
		},
	}
	var buf bytes.Buffer
	RenderMetrics(&buf, snap)
	out := buf.String()

	checks := []string{
		"directory_cache_ready 1",
		"directory_watched_total 1",
		`directory_file_count{base="/streams",stream="orders",type="buffer"} 42`,
	}
	for _, want := range checks {
		if !strings.Contains(out, want) {
			t.Errorf("missing expected line:\n  want: %s\n  in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "directory_scrape_success") {
		t.Error("unexpected directory_scrape_success in /metrics output")
	}
	if strings.Contains(out, "directory_scan_truncated") {
		t.Error("unexpected directory_scan_truncated in /metrics output")
	}
}

func TestRenderMetrics_EmptyDirOmitsTimestamps(t *testing.T) {
	snap := CacheSnapshot{
		Ready:        true,
		LastScanTime: time.Now(),
		Entries: []DirMetrics{
			{FileCount: 0, ScanSuccess: 1, Labels: DirLabels{Stream: "orders"}},
		},
	}
	var buf bytes.Buffer
	RenderMetrics(&buf, snap)
	out := buf.String()
	if strings.Contains(out, "directory_oldest_file_timestamp_seconds{") {
		t.Error("oldest timestamp should be omitted for empty directory")
	}
	if strings.Contains(out, "directory_newest_file_timestamp_seconds{") {
		t.Error("newest timestamp should be omitted for empty directory")
	}
}

func TestRenderMetrics_SortedOutput(t *testing.T) {
	// Entries added in reverse alphabetical order — output must be sorted.
	snap := CacheSnapshot{
		Ready:        true,
		LastScanTime: time.Now(),
		Entries: []DirMetrics{
			{ScanSuccess: 1, Labels: DirLabels{Stream: "z-stream"}},
			{ScanSuccess: 1, Labels: DirLabels{Stream: "a-stream"}},
		},
	}
	var buf bytes.Buffer
	RenderMetrics(&buf, snap)
	out := buf.String()
	idxA := strings.Index(out, "a-stream")
	idxZ := strings.Index(out, "z-stream")
	if idxA > idxZ {
		t.Error("metric output is not sorted: a-stream should appear before z-stream")
	}
}

// ── label value escaping ──────────────────────────────────────────────────────

func TestLv_Escaping(t *testing.T) {
	cases := []struct{ in, want string }{
		{`plain`, `"plain"`},
		{`with "quotes"`, `"with \"quotes\""`},
		{`with\backslash`, `"with\\backslash"`},
		{"with\nnewline", `"with\nnewline"`},
	}
	for _, tc := range cases {
		if got := lv(tc.in); got != tc.want {
			t.Errorf("lv(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ── log output ────────────────────────────────────────────────────────────────

// logLines parses a multi-line JSON log buffer into a slice of maps so tests
// can assert on specific fields without fragile string matching.
func logLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for _, raw := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if raw == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatalf("log line is not valid JSON: %s\nerror: %v", raw, err)
		}
		lines = append(lines, m)
	}
	return lines
}

// hasLog returns true if any log line has the given key=value pair.
func hasLog(lines []map[string]any, key, value string) bool {
	for _, l := range lines {
		if v, ok := l[key]; ok && fmt.Sprint(v) == value {
			return true
		}
	}
	return false
}

func TestScanAll_LogsScanComplete(t *testing.T) {
	base := t.TempDir()
	os.MkdirAll(filepath.Join(base, "orders"), 0755)
	os.WriteFile(filepath.Join(base, "orders", "a.txt"), []byte("x"), 0644)

	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	cfg := &Config{
		Targets:        []Target{{Base: base, MaxDepth: 1}},
		ScanWorkers:    1,
		ScanTimeout:    10 * time.Second,
		MaxFilesPerDir: 0,
	}
	exp := NewExporter(cfg, log)
	if err := exp.Discover(); err != nil {
		t.Fatal(err)
	}
	exp.ScanAll()

	lines := logLines(t, &buf)
	if !hasLog(lines, "msg", "scan cycle complete") {
		t.Errorf("expected 'scan cycle complete' log line; got:\n%s", buf.String())
	}
}

func TestScanAll_LogsTruncation(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "big")
	os.MkdirAll(dir, 0755)
	for i := range 5 {
		os.WriteFile(filepath.Join(dir, fmt.Sprintf("%d.txt", i)), []byte("x"), 0644)
	}

	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	cfg := &Config{
		Targets:        []Target{{Base: base, MaxDepth: 1}},
		ScanWorkers:    1,
		ScanTimeout:    10 * time.Second,
		MaxFilesPerDir: 2, // cap at 2 out of 5 — must trigger truncation warning
	}
	exp := NewExporter(cfg, log)
	if err := exp.Discover(); err != nil {
		t.Fatal(err)
	}
	exp.ScanAll()

	lines := logLines(t, &buf)
	if !hasLog(lines, "msg", "scan truncated") {
		t.Errorf("expected 'scan truncated' warning; got:\n%s", buf.String())
	}
}

func TestScanAll_MetricsTruncatedAfterCap(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "stream")
	os.MkdirAll(dir, 0755)
	for i := range 10 {
		os.WriteFile(filepath.Join(dir, fmt.Sprintf("%d.txt", i)), []byte("x"), 0644)
	}

	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))

	cfg := &Config{
		Targets:        []Target{{Base: base, MaxDepth: 1}},
		ScanWorkers:    1,
		ScanTimeout:    10 * time.Second,
		MaxFilesPerDir: 3,
	}
	exp := NewExporter(cfg, log)
	if err := exp.Discover(); err != nil {
		t.Fatal(err)
	}
	exp.ScanAll()

	snap := exp.Snapshot()
	if len(snap.Entries) == 0 {
		t.Fatal("no entries in snapshot")
	}
	entry := snap.Entries[0]
	if !entry.Truncated {
		t.Error("expected Truncated=true when MaxFilesPerDir cap is hit")
	}
	if entry.FileCount != 3 {
		t.Errorf("FileCount=%d want 3 (only capped entries counted)", entry.FileCount)
	}

	var metricsBuf bytes.Buffer
	RenderMetrics(&metricsBuf, snap)
	if strings.Contains(metricsBuf.String(), "directory_scan_truncated") {
		t.Error("unexpected directory_scan_truncated in /metrics output")
	}
}

func TestScanAll_MaxStatFilesCountsAllButStatsLimited(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "stream")
	os.MkdirAll(dir, 0755)
	for i := range 10 {
		os.WriteFile(filepath.Join(dir, fmt.Sprintf("%d.txt", i)), []byte("x"), 0644)
	}

	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))

	cfg := &Config{
		Targets:      []Target{{Base: base, MaxDepth: 1}},
		ScanWorkers:  1,
		ScanTimeout:  10 * time.Second,
		MaxStatFiles: 3, // stat only 3 of the 10 files
	}
	exp := NewExporter(cfg, log)
	if err := exp.Discover(); err != nil {
		t.Fatal(err)
	}
	exp.ScanAll()

	snap := exp.Snapshot()
	if len(snap.Entries) == 0 {
		t.Fatal("no entries in snapshot")
	}
	entry := snap.Entries[0]
	// All 10 files must be counted even though only 3 were stat-ed.
	if entry.FileCount != 10 {
		t.Errorf("FileCount=%d want 10 — all files counted regardless of stat cap", entry.FileCount)
	}
	// Truncated should be true because the stat cap was hit.
	if !entry.Truncated {
		t.Error("expected Truncated=true when MaxStatFiles cap is hit")
	}
	// Timestamps must still be present (from the 3 stat-ed files).
	if entry.OldestTimestampSec == 0 || entry.NewestTimestampSec == 0 {
		t.Error("expected non-zero timestamps from stat-ed sample")
	}
}
