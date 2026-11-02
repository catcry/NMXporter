package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExtractSourceDirectory(t *testing.T) {
	cases := []struct{ in, want string }{
		{`SourceDirectory	"/comptel/elink/install/buffer/401/in"`, "/comptel/elink/install/buffer/401/in"},
		{`SourceDirectory "/data/collector/input"`, "/data/collector/input"},
		{`SourceDirectory: "/data/collector/input"`, "/data/collector/input"},
		{`SourceDirectory: /data/collector/input`, "/data/collector/input"},
		{`SourceDirectory /data/collector/input`, "/data/collector/input"},
	}
	for _, tc := range cases {
		if got := extractSourceDirectory(tc.in); got != tc.want {
			t.Errorf("extractSourceDirectory(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestResolveNodeBufferPaths_CollectorOnly(t *testing.T) {
	// Collector node resolves SourceDirectory
	colDir := t.TempDir()
	os.MkdirAll(filepath.Join(colDir, "control", "1"), 0755)
	cfgCol := "NodeType \"collector\"\nSourceDirectory\t\"/data/collector/in\"\n"
	os.WriteFile(filepath.Join(colDir, "control", "1", "config"), []byte(cfgCol), 0644)

	nt, paths := resolveNodeBufferPaths(colDir)
	if nt != "collector" {
		t.Errorf("nodeType=%q want collector", nt)
	}
	if len(paths) != 1 || paths[0] != "/data/collector/in" {
		t.Errorf("paths=%v want [/data/collector/in]", paths)
	}

	// Non-collector node returns nil paths
	distDir := t.TempDir()
	os.MkdirAll(filepath.Join(distDir, "control", "1"), 0755)
	cfgDist := "NodeType \"distributor\"\nInDataPath\t\"COLLECTED,file:/comptel/elink/buffer\"\n"
	os.WriteFile(filepath.Join(distDir, "control", "1", "config"), []byte(cfgDist), 0644)

	ntDist, pathsDist := resolveNodeBufferPaths(distDir)
	if ntDist != "distributor" {
		t.Errorf("nodeType=%q want distributor", ntDist)
	}
	if len(pathsDist) != 0 {
		t.Errorf("pathsDist=%v want empty for distributor", pathsDist)
	}
}

// Only collector nodes are scanned for buffer files; distributor/general nodes
// are skipped entirely and not added to the buffer metrics.
func TestScanAllNodeBuffers_OnlyCollectorScanned(t *testing.T) {
	base := t.TempDir()

	// Collector node with SourceDirectory
	sourceDir := filepath.Join(base, "collector_in")
	os.MkdirAll(sourceDir, 0755)
	os.WriteFile(filepath.Join(sourceDir, "f1.dat"), []byte("1"), 0644)
	os.WriteFile(filepath.Join(sourceDir, "f2.dat"), []byte("2"), 0644)

	colNodeDir := filepath.Join(base, "CBSDump", "nodes", "Col1")
	os.MkdirAll(filepath.Join(colNodeDir, "control", "1"), 0755)
	os.WriteFile(filepath.Join(colNodeDir, "control", "1", "config"),
		[]byte("NodeType \"collector\"\nSourceDirectory \""+sourceDir+"\"\n"), 0644)

	// Distributor node with input files
	distNodeDir := filepath.Join(base, "CBSDump", "nodes", "Dist1")
	os.MkdirAll(filepath.Join(distNodeDir, "input"), 0755)
	os.WriteFile(filepath.Join(distNodeDir, "input", "d1.dat"), []byte("x"), 0644)
	os.MkdirAll(filepath.Join(distNodeDir, "control", "1"), 0755)
	os.WriteFile(filepath.Join(distNodeDir, "control", "1", "config"),
		[]byte("NodeType \"distributor\"\n"), 0644)

	// Decoder node with InDataPath
	decNodeDir := filepath.Join(base, "CBSDump", "nodes", "Dec1")
	os.MkdirAll(filepath.Join(decNodeDir, "control", "1"), 0755)
	os.WriteFile(filepath.Join(decNodeDir, "control", "1", "config"),
		[]byte("NodeType \"decoder\"\n"), 0644)

	nodes := []StreamNode{
		{Base: "/streams", Stream: "CBSDump", Node: "Col1", NodeDir: colNodeDir, NodeType: "collector"},
		{Base: "/streams", Stream: "CBSDump", Node: "Dist1", NodeDir: distNodeDir, NodeType: "distributor"},
		{Base: "/streams", Stream: "CBSDump", Node: "Dec1", NodeDir: decNodeDir, NodeType: "decoder"},
	}

	buffers := ScanAllNodeBuffers(context.Background(), nodes, nil, nil, 2)

	// Distributor and decoder must NOT be scanned / included in buffers
	if _, ok := buffers["/streams/CBSDump/Dist1/distributor"]; ok {
		t.Errorf("distributor node should not be in buffers map")
	}
	if _, ok := buffers["/streams/CBSDump/Dec1/decoder"]; ok {
		t.Errorf("decoder node should not be in buffers map")
	}

	// Collector must be scanned
	cb, ok := buffers["/streams/CBSDump/Col1/collector"]
	if !ok {
		t.Fatalf("missing collector buffer entry in %v", buffers)
	}
	if cb.FileCount != 2 || !cb.Success {
		t.Errorf("got count=%d success=%v want 2,true", cb.FileCount, cb.Success)
	}
	if cb.BufferPath != sourceDir {
		t.Errorf("got BufferPath=%q want %q", cb.BufferPath, sourceDir)
	}
}

// Collector nodes reuse already-scanned directory_file_count when SourceDirectory
// is part of watched directories.
func TestScanAllNodeBuffers_ReusesWatchedCache(t *testing.T) {
	base := t.TempDir()
	sourceDir := filepath.Join(base, "watched_source")

	colNodeDir := filepath.Join(base, "nodes", "Col1")
	os.MkdirAll(filepath.Join(colNodeDir, "control", "1"), 0755)
	os.WriteFile(filepath.Join(colNodeDir, "control", "1", "config"),
		[]byte("NodeType \"collector\"\nSourceDirectory \""+sourceDir+"\"\n"), 0644)

	nodes := []StreamNode{
		{Base: "/streams", Stream: "S", Node: "Col1", NodeDir: colNodeDir, NodeType: "collector"},
	}
	results := map[string]DirMetrics{
		sourceDir: {FileCount: 42, ScanSuccess: 1},
	}

	buffers := ScanAllNodeBuffers(context.Background(), nodes, results, nil, 2)
	cb, ok := buffers["/streams/S/Col1/collector"]
	if !ok {
		t.Fatalf("missing collector buffer entry in %v", buffers)
	}
	if cb.FileCount != 42 || !cb.Success {
		t.Errorf("got count=%d success=%v want 42,true", cb.FileCount, cb.Success)
	}
}

func TestRenderMetrics_BufferFiles(t *testing.T) {
	snap := CacheSnapshot{
		Ready:        true,
		LastScanTime: time.Now(),
		NodeBuffers: []NodeBuffer{
			{Base: "/streams", Stream: "CBSDump", Node: "Col1", NodeType: "collector", BufferPath: "/source", FileCount: 138, Success: true},
		},
	}
	var buf bytes.Buffer
	RenderMetrics(&buf, snap)
	if !strings.Contains(buf.String(), `directory_buffer_files{base="/streams",stream="CBSDump",node="Col1",type="collector"} 138`) {
		t.Errorf("missing directory_buffer_files line:\n%s", buf.String())
	}
}
