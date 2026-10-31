package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestScanCollectorActivity_Valid(t *testing.T) {
	dir := t.TempDir()
	storageDir := filepath.Join(dir, "storage", "1")
	if err := os.MkdirAll(storageDir, 0755); err != nil {
		t.Fatal(err)
	}

	now := time.Unix(1756805000, 0)
	epoch := int64(1756804000) // 1000s ago (> 600s min_delay)
	content := fmt.Sprintf("CDR_FILE_001.dat %d\n", epoch)
	if err := os.WriteFile(filepath.Join(storageDir, "audit_info"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	act, err := ScanCollectorActivity("/streams", "STREAM_A", "col_01", dir, 600*time.Second, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !act.Success {
		t.Error("expected Success = true")
	}
	if act.LastFile != "CDR_FILE_001.dat" {
		t.Errorf("LastFile: got %q, want CDR_FILE_001.dat", act.LastFile)
	}
	if act.LastActivityTimestamp != float64(epoch) {
		t.Errorf("LastActivityTimestamp: got %g, want %d", act.LastActivityTimestamp, epoch)
	}
	if act.DelaySec != 1000 {
		t.Errorf("DelaySec: got %g, want 1000", act.DelaySec)
	}
}

func TestScanCollectorActivity_MinDelayThreshold(t *testing.T) {
	dir := t.TempDir()
	storageDir := filepath.Join(dir, "storage", "1")
	if err := os.MkdirAll(storageDir, 0755); err != nil {
		t.Fatal(err)
	}

	now := time.Unix(1756805000, 0)
	epoch := int64(1756804800) // 200s ago (<= 600s min_delay)
	content := fmt.Sprintf("CDR_FILE_002.dat %d\n", epoch)
	if err := os.WriteFile(filepath.Join(storageDir, "audit_info"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	act, err := ScanCollectorActivity("/streams", "STREAM_A", "col_01", dir, 600*time.Second, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !act.Success {
		t.Error("expected Success = true")
	}
	if act.DelaySec != 0 {
		t.Errorf("DelaySec under threshold: got %g, want 0", act.DelaySec)
	}
	if act.LastActivityTimestamp != float64(epoch) {
		t.Errorf("LastActivityTimestamp: got %g, want %d", act.LastActivityTimestamp, epoch)
	}
}

func TestScanCollectorActivity_MissingOrCorrupt(t *testing.T) {
	dir := t.TempDir()
	act, err := ScanCollectorActivity("/streams", "STREAM_A", "col_01", dir, 600*time.Second, time.Now())
	if err == nil {
		t.Error("expected error for missing audit_info")
	}
	if act.Success {
		t.Error("expected Success = false")
	}

	// Corrupt content
	storageDir := filepath.Join(dir, "storage", "1")
	os.MkdirAll(storageDir, 0755)
	os.WriteFile(filepath.Join(storageDir, "audit_info"), []byte("invalid_format_single_field"), 0644)
	act, err = ScanCollectorActivity("/streams", "STREAM_A", "col_01", dir, 600*time.Second, time.Now())
	if err == nil {
		t.Error("expected error for malformed audit_info")
	}
	if act.Success {
		t.Error("expected Success = false for malformed audit_info")
	}
}

func TestScanDistributorActivity_TodayMatch(t *testing.T) {
	tracelogDir := t.TempDir()
	now, _ := time.ParseInLocation("2006-01-02 15:04:05", "2026-09-02 12:00:00", time.Local)
	todayStr := now.Format("20060102")

	logContent := `20260902 100000.100 [dist_01].info: Starting distribution
20260902 103000.250 [dist_01].info: Distributed file CDR_OUT_20260902_001.dat to billing
20260902 110000.500 [dist_01].info: Distributed file CDR_OUT_20260902_002.dat to billing
20260902 110500.999 [other_node].info: Distributed file OTHER.dat
`
	traceFile := filepath.Join(tracelogDir, fmt.Sprintf("execution_trace_STREAM_A_%s_001.log", todayStr))
	if err := os.WriteFile(traceFile, []byte(logContent), 0644); err != nil {
		t.Fatal(err)
	}

	act, err := ScanDistributorActivity(tracelogDir, "/streams", "STREAM_A", "dist_01", 7, 600*time.Second, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !act.Success {
		t.Fatal("expected Success = true")
	}
	if act.LastFile != "CDR_OUT_20260902_002.dat" {
		t.Errorf("LastFile: got %q, want CDR_OUT_20260902_002.dat", act.LastFile)
	}

	expectedTime, _ := time.ParseInLocation("2006-01-02 15:04:05", "2026-09-02 11:00:00", time.Local)
	if act.LastActivityTimestamp != float64(expectedTime.Unix()) {
		t.Errorf("LastActivityTimestamp: got %g, want %d", act.LastActivityTimestamp, expectedTime.Unix())
	}
	// Delay is 12:00:00 - 11:00:00 = 3600 seconds
	if act.DelaySec != 3600 {
		t.Errorf("DelaySec: got %g, want 3600", act.DelaySec)
	}
}

func TestScanDistributorActivity_LookbackPreviousDay(t *testing.T) {
	tracelogDir := t.TempDir()
	now, _ := time.ParseInLocation("2006-01-02 15:04:05", "2026-09-02 12:00:00", time.Local)
	yesterdayStr := now.AddDate(0, 0, -1).Format("20060102")

	logContent := `20260901 220000.123 [dist_02].info: Distributed file CDR_OUT_YESTERDAY.dat to archive
`
	traceFile := filepath.Join(tracelogDir, fmt.Sprintf("execution_trace_STREAM_B_%s_001.log", yesterdayStr))
	if err := os.WriteFile(traceFile, []byte(logContent), 0644); err != nil {
		t.Fatal(err)
	}

	act, err := ScanDistributorActivity(tracelogDir, "/streams", "STREAM_B", "dist_02", 7, 600*time.Second, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !act.Success {
		t.Fatal("expected Success = true")
	}
	if act.LastFile != "CDR_OUT_YESTERDAY.dat" {
		t.Errorf("LastFile: got %q, want CDR_OUT_YESTERDAY.dat", act.LastFile)
	}

	expectedTime, _ := time.ParseInLocation("2006-01-02 15:04:05", "2026-09-01 22:00:00", time.Local)
	if act.LastActivityTimestamp != float64(expectedTime.Unix()) {
		t.Errorf("LastActivityTimestamp: got %g, want %d", act.LastActivityTimestamp, expectedTime.Unix())
	}
	expectedDelay := now.Unix() - expectedTime.Unix()
	if act.DelaySec != float64(expectedDelay) {
		t.Errorf("DelaySec: got %g, want %d", act.DelaySec, expectedDelay)
	}
}

func TestDiscoverStreamNodes_AndScanAll(t *testing.T) {
	baseDir := t.TempDir()
	tracelogDir := t.TempDir()

	// Setup stream structure:
	// baseDir/STREAM_1/nodes/col_1
	// baseDir/STREAM_1/nodes/dist_1
	colDir := filepath.Join(baseDir, "STREAM_1", "nodes", "col_1")
	distDir := filepath.Join(baseDir, "STREAM_1", "nodes", "dist_1")
	os.MkdirAll(filepath.Join(colDir, "storage", "1"), 0755)
	os.MkdirAll(filepath.Join(distDir, "control", "1"), 0755)

	now, _ := time.ParseInLocation("2006-01-02 15:04:05", "2026-09-02 12:00:00", time.Local)
	epochCol := now.Add(-2 * time.Hour).Unix()
	os.WriteFile(filepath.Join(colDir, "storage", "1", "audit_info"), []byte(fmt.Sprintf("IN_001.dat %d\n", epochCol)), 0644)
	os.WriteFile(filepath.Join(distDir, "control", "1", "config"), []byte("NodeType \"distributor\"\n"), 0644)

	todayStr := now.Format("20060102")
	traceContent := fmt.Sprintf("%s 113000.000 [dist_1].info: Distributed file OUT_001.dat\n", todayStr)
	os.WriteFile(filepath.Join(tracelogDir, fmt.Sprintf("execution_trace_STREAM_1_%s_01.log", todayStr)), []byte(traceContent), 0644)

	targets := []Target{{Base: baseDir, Label: "/streams"}}
	nodes := DiscoverStreamNodes(targets)
	if len(nodes) != 2 {
		t.Fatalf("expected 2 discovered nodes, got %d", len(nodes))
	}

	results := ScanAllNodeActivities(context.Background(), nodes, tracelogDir, 600*time.Second, 7, 2, now)
	if len(results) != 2 {
		t.Fatalf("expected 2 activity results, got %d", len(results))
	}

	colKey := "/streams/STREAM_1/col_1/collector"
	colAct, exists := results[colKey]
	if !exists {
		t.Fatalf("missing collector result %q", colKey)
	}
	if colAct.LastFile != "IN_001.dat" || colAct.DelaySec != 7200 {
		t.Errorf("collector mismatch: file=%s delay=%g", colAct.LastFile, colAct.DelaySec)
	}

	distKey := "/streams/STREAM_1/dist_1/distributor"
	distAct, exists := results[distKey]
	if !exists {
		t.Fatalf("missing distributor result %q", distKey)
	}
	if distAct.LastFile != "OUT_001.dat" || distAct.DelaySec != 1800 {
		t.Errorf("distributor mismatch: file=%s delay=%g", distAct.LastFile, distAct.DelaySec)
	}
}

func TestRenderMetrics_NodeActivities(t *testing.T) {
	snap := CacheSnapshot{
		Ready:        true,
		WatchedTotal: 1,
		LastScanTime: time.Unix(1756800000, 0),
		NodeActivities: []NodeActivity{
			{
				Base:                  "/streams",
				Stream:                "STREAM_A",
				Node:                  "col_01",
				NodeType:              "collector",
				LastActivityTimestamp: 1756795000,
				DelaySec:              5000,
				LastFile:              "FILE_A.dat",
				Success:               true,
			},
			{
				Base:                  "/streams",
				Stream:                "STREAM_A",
				Node:                  "dist_01",
				NodeType:              "distributor",
				LastActivityTimestamp: 1756798000,
				DelaySec:              2000,
				LastFile:              "FILE_OUT.dat",
				Success:               true,
			},
		},
	}

	var buf bytes.Buffer
	RenderMetrics(&buf, snap)
	out := buf.String()

	expectedSnippets := []string{
		`directory_collector_last_activity_timestamp_seconds{base="/streams",stream="STREAM_A",node="col_01"} 1.756795e+09`,
		`directory_collector_delay_seconds{base="/streams",stream="STREAM_A",node="col_01"} 5000`,
		`directory_distributor_last_activity_timestamp_seconds{base="/streams",stream="STREAM_A",node="dist_01"} 1.756798e+09`,
		`directory_distributor_delay_seconds{base="/streams",stream="STREAM_A",node="dist_01"} 2000`,
		`directory_node_last_file_info{base="/streams",stream="STREAM_A",node="col_01",type="collector",filename="FILE_A.dat"} 1`,
		`directory_node_last_file_info{base="/streams",stream="STREAM_A",node="dist_01",type="distributor",filename="FILE_OUT.dat"} 1`,
	}

	for _, s := range expectedSnippets {
		if !strings.Contains(out, s) {
			t.Errorf("output missing snippet %q\nFull output:\n%s", s, out)
		}
	}
}
