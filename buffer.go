package main

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// NodeBuffer holds the input-buffer backlog for a single stream collector node.
// It mirrors monitoring.sh BUFFER_INDEX (STRAMES_DETAIL[$i, $BUFFER_INDEX]):
// the non-recursive regular-file count of the collector node's SourceDirectory.
type NodeBuffer struct {
	Base       string
	Stream     string
	Node       string
	NodeType   string // "collector"
	BufferPath string // resolved SourceDirectory ("-" when unresolved)
	FileCount  int64
	Success    bool // true when the buffer dir was readable and counted
}

var (
	quotedValueRegex = regexp.MustCompile(`"([^"]+)"`)
)

// isCollector returns true if nodeType indicates a collector node.
func isCollector(nodeType string) bool {
	lower := strings.ToLower(strings.TrimSpace(nodeType))
	return strings.Contains(lower, "collector") || lower == "col"
}

// extractSourceDirectory extracts the filesystem path from a SourceDirectory config line.
// Supports:
//   SourceDirectory "/path/to/dir"
//   SourceDirectory\t"/path/to/dir"
//   SourceDirectory: "/path/to/dir"
//   SourceDirectory: /path/to/dir
//   SourceDirectory /path/to/dir
func extractSourceDirectory(line string) string {
	if m := quotedValueRegex.FindStringSubmatch(line); len(m) > 1 {
		return strings.TrimSpace(m[1])
	}
	trimmed := strings.TrimPrefix(line, "SourceDirectory")
	trimmed = strings.TrimLeft(trimmed, " \t:")
	fields := strings.Fields(trimmed)
	if len(fields) > 0 {
		return strings.Trim(fields[0], `"'`)
	}
	return ""
}

// resolveNodeBufferPaths parses ${nodeDir}/control/1/config and returns the
// node type plus any SourceDirectory paths found when the node is a collector.
// Non-collector nodes return their node type and nil paths.
func resolveNodeBufferPaths(nodeDir string) (string, []string) {
	data, err := os.ReadFile(filepath.Join(nodeDir, "control", "1", "config"))
	if err != nil {
		return "", nil
	}

	var nodeType string
	var sourceDirs []string

	lines := strings.Split(string(data), "\n")
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "NodeType") {
			if m := quotedValueRegex.FindStringSubmatch(line); len(m) > 1 {
				nodeType = m[1]
			}
		} else if strings.HasPrefix(line, "SourceDirectory") {
			if dir := extractSourceDirectory(line); dir != "" {
				sourceDirs = append(sourceDirs, dir)
			}
		}
	}

	if nodeType != "" && !isCollector(nodeType) {
		return nodeType, nil
	}
	return nodeType, sourceDirs
}

// countBufferFiles counts regular files directly inside path (non-recursive),
// mirroring `find <dir> -maxdepth 1 -mindepth 1 -type f | wc -l` and the
// scanDir counting rules (regular files + symlinks resolving to files;
// subdirectories, symlinks to directories, and broken symlinks are skipped).
func countBufferFiles(ctx context.Context, path string) (int64, bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()

	var count int64
	for {
		if ctx.Err() != nil {
			return count, false
		}
		entries, readErr := f.ReadDir(dirChunkSize)
		for _, e := range entries {
			if ctx.Err() != nil {
				return count, false
			}
			if e.Type()&os.ModeSymlink != 0 {
				info, statErr := os.Stat(filepath.Join(path, e.Name()))
				if statErr != nil || info.IsDir() {
					continue
				}
				count++
			} else if e.IsDir() {
				continue
			} else {
				count++
			}
		}
		if readErr != nil {
			break
		}
	}
	return count, true
}

// ScanAllNodeBuffers resolves each collector node's SourceDirectory buffer
// directory and counts files, reusing already-scanned directory_file_count values
// when the buffer path coincides with a watched directory (best performance: zero
// extra I/O). Unwatched external SourceDirectory paths get a cheap count-only scan.
// Non-collector nodes (NodeType != "collector") are skipped entirely.
func ScanAllNodeBuffers(ctx context.Context, nodes []StreamNode, results map[string]DirMetrics, byPath map[string]string, workers int) map[string]NodeBuffer {
	_ = byPath // reserved for future path-alias support
	if len(nodes) == 0 {
		return make(map[string]NodeBuffer)
	}
	if workers <= 0 {
		workers = 2
	}

	// Fast lookup: cleaned buffer path -> cached file count (only for
	// successfully scanned watched dirs).
	cached := make(map[string]DirMetrics, len(results))
	for absPath, dm := range results {
		cached[filepath.Clean(absPath)] = dm
	}

	out := make(map[string]NodeBuffer, len(nodes))
	var mu sync.Mutex
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup

	for _, n := range nodes {
		if ctx.Err() != nil {
			break
		}

		// Fast path: skip nodes that are already known not to be collectors
		if n.NodeType != "" && !isCollector(n.NodeType) {
			continue
		}

		wg.Add(1)
		sem <- struct{}{}
		go func(sn StreamNode) {
			defer wg.Done()
			defer func() { <-sem }()

			nodeType, paths := resolveNodeBufferPaths(sn.NodeDir)
			if nodeType == "" {
				nodeType = sn.NodeType
			}
			if nodeType == "" {
				nodeType = detectNodeType(sn.NodeDir, sn.Node)
			}
			if !isCollector(nodeType) {
				return
			}
			nodeType = "collector"
			key := sn.Base + "/" + sn.Stream + "/" + sn.Node + "/" + nodeType

			var total int64
			okAll := len(paths) > 0
			anyOK := false
			resolved := strings.Join(paths, ",")
			for _, p := range paths {
				clean := filepath.Clean(p)
				if dm, hit := cached[clean]; hit && dm.ScanSuccess == 1 {
					total += dm.FileCount
					anyOK = true
					continue
				}
				c, ok := countBufferFiles(ctx, clean)
				if !ok {
					okAll = false
					continue
				}
				total += c
				anyOK = true
			}

			if len(paths) == 0 {
				resolved = "-"
			}

			mu.Lock()
			out[key] = NodeBuffer{
				Base: sn.Base, Stream: sn.Stream, Node: sn.Node,
				NodeType: nodeType, BufferPath: resolved,
				FileCount: total, Success: anyOK && okAll,
			}
			mu.Unlock()
		}(n)
	}
	wg.Wait()
	return out
}
