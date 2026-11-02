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

func TestCollectedEpochFromNameAndScanDir(t *testing.T) {
	for name, want := range map[string]int64{
		"1769850107x001_001898_1769850117x001_893_1": 1769850107,
		"1793016004x003_0011282_1793016217x001_1284": 1793016004,
		"test_1787384582_5136_2603.dat":              0,
		"file_81382":                                 0,
		"12345x":                                     0,
	} {
		if got := collectedEpochFromName(name); got != want {
			t.Errorf("%s: got %d want %d", name, got, want)
		}
	}

	dir := t.TempDir()
	for _, n := range []string{"1769850207x001_1", "1769850107x002_1", "noid.dat"} {
		writeFile(t, filepath.Join(dir, n), "x")
	}
	res := scanDir(context.Background(), dir, 0, 1)
	if res.OldestCollected != 1769850107 {
		t.Errorf("OldestCollected: got %v", res.OldestCollected)
	}
	if !res.StatLimited || res.FileCount != 3 {
		t.Errorf("stat cap 1 of 3: StatLimited=%v count=%d", res.StatLimited, res.FileCount)
	}
	if r := scanDir(context.Background(), dir, 2, 0); r.StatLimited {
		t.Error("a file-count cap is real truncation, not StatLimited")
	}
}

func TestDistributorMemo(t *testing.T) {
	dir := t.TempDir()
	now, _ := time.ParseInLocation("2006-01-02 15:04:05", "2026-10-31 04:10:00", time.Local)
	p := filepath.Join(dir, "execution_trace_memo_20261031_0.log")
	writeFile(t, p, "20261031 040000.000 L NB [DIS]  File a:f1 X: Distributed file f1 (X), 0 bytes\n")
	sn := StreamNode{Base: "/streams", Stream: "memo", Node: "DIS", NodeType: "distributor"}

	a1, _ := scanDistributorMemo(streamTraceSignatures(dir)["memo"], dir, sn, 7, 0, now)
	// Same signature → cached result even though we pass a later "now".
	a2, _ := scanDistributorMemo(streamTraceSignatures(dir)["memo"], dir, sn, 7, 0, now.Add(time.Minute))
	if a1.LastFile != "f1" || a2.LastFile != "f1" || a2.DelaySec != a1.DelaySec+60 {
		t.Errorf("memo: a1=%+v a2=%+v", a1, a2)
	}
	fh, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0644)
	fh.WriteString("20261031 040500.000 L NB [DIS]  File a:f2 X: Distributed file f2 (X), 0 bytes\n")
	fh.Close()
	a3, _ := scanDistributorMemo(streamTraceSignatures(dir)["memo"], dir, sn, 7, 0, now)
	if a3.LastFile != "f2" {
		t.Errorf("changed log must be re-read, got %+v", a3)
	}
}

// Trace log metrics moved to eventlink-trace-exporter; directory-exporter
// keeps the queue age taken from file names.
func TestRenderMetrics_QueueAgeWithoutTraceMetrics(t *testing.T) {
	snap := CacheSnapshot{
		Ready:        true,
		LastScanTime: time.Now(),
		NodeStatuses: []NodeStatus{{Base: "/streams", Stream: "el2", Node: "DIS"}},
		Entries: []DirMetrics{{FileCount: 2, ScanSuccess: 1, OldestCollectedSec: 1769850107,
			Labels: DirLabels{Base: "/streams", Stream: "el2", Type: "DIS/input/Q"}}},
	}
	var buf bytes.Buffer
	RenderMetrics(&buf, snap)
	out := buf.String()
	if want := `directory_queue_oldest_collected_timestamp_seconds{base="/streams",stream="el2",type="DIS/input/Q"} 1.769850107e+09`; !strings.Contains(out, want) {
		t.Errorf("missing %q", want)
	}
	for _, gone := range []string{"directory_tracelog_", "directory_stream_latency_seconds", "directory_node_files_total"} {
		if strings.Contains(out, gone) {
			t.Errorf("%s must not be exported by directory-exporter any more", gone)
		}
	}
}
