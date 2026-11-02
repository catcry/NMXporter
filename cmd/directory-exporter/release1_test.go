package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestReadNodeConfig(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "control", "1", "config"),
		"NodeType\t\"Collector\"\nOutDataPath\t\"COLLECTED,file:/buf/61/COLLECTED_0_101\"\n"+
			"SourceDirectory\t\"/src/in\"\nFilename\t\"file*, cdr_*.dat\"\nTargetDirectory\t\"/dst/out\"\n")

	nc, ok := readNodeConfig(dir)
	if !ok {
		t.Fatal("expected ok")
	}
	if nc.NodeType != "collector" {
		t.Errorf("NodeType: got %q", nc.NodeType)
	}
	if !reflect.DeepEqual(nc.SourceDirs, []string{"/src/in"}) {
		t.Errorf("SourceDirs: got %v", nc.SourceDirs)
	}
	if !reflect.DeepEqual(nc.FilenameMasks, []string{"file*", "cdr_*.dat"}) {
		t.Errorf("FilenameMasks: got %v", nc.FilenameMasks)
	}
	if nc.TargetDir != "/dst/out" {
		t.Errorf("TargetDir: got %q", nc.TargetDir)
	}

	if _, ok := readNodeConfig(t.TempDir()); ok {
		t.Error("missing config should return ok=false")
	}
}

func TestScanNodeStatus(t *testing.T) {
	now := time.Unix(1793434500, 0)
	grace := 120 * time.Second
	cases := []struct {
		name      string
		heartbeat string // "" = no file
		info      string
		wantUp    bool
		wantState string
	}{
		// EventLink writes a deadline in the future while the node runs.
		{"future deadline (TIMER collector)", "1793438100", "1793434499 TIMER \n", true, "TIMER"},
		{"deadline passed within grace", "1793434450", "1793434412 IDLE", true, "IDLE"},
		{"deadline passed beyond grace", "1793434000", "1793434412 IDLE", false, "IDLE"},
		{"no heartbeat file", "", "", false, ""},
		{"garbage heartbeat", "abc", "", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.heartbeat != "" {
				writeFile(t, filepath.Join(dir, "control", "1", "heartbeat"), tc.heartbeat)
			}
			if tc.info != "" {
				writeFile(t, filepath.Join(dir, "control", "1", "info"), tc.info)
			}
			st := ScanNodeStatus(StreamNode{Base: "/streams", Stream: "S", Node: "N", NodeDir: dir, RawType: "distributor"}, grace, now)
			if st.Up != tc.wantUp {
				t.Errorf("Up: got %v want %v", st.Up, tc.wantUp)
			}
			if st.State != tc.wantState {
				t.Errorf("State: got %q want %q", st.State, tc.wantState)
			}
			if st.NodeType != "distributor" {
				t.Errorf("NodeType: got %q", st.NodeType)
			}
		})
	}
}

func TestSortTraceFilesNewestFirst(t *testing.T) {
	in := []string{
		"/t/execution_trace_S_20261031_0.log",
		"/t/execution_trace_S_20261031_9.log",
		"/t/execution_trace_S_20261031_10.log",
		"/t/execution_trace_S_20261031_99.log",
		"/t/execution_trace_S_20261031_100.log",
		"/t/execution_trace_S_20261031_101.log",
	}
	sortTraceFilesNewestFirst(in)
	var seqs []int
	for _, p := range in {
		seqs = append(seqs, traceSeq(p))
	}
	if want := []int{101, 100, 99, 10, 9, 0}; !reflect.DeepEqual(seqs, want) {
		t.Errorf("got %v want %v", seqs, want)
	}
}

// With a plain string sort the exporter read _9 (early morning) before _100
// and reported a stale distribution time.
func TestScanDistributorActivity_RotatedLogsNumericOrder(t *testing.T) {
	dir := t.TempDir()
	now, _ := time.ParseInLocation("2006-01-02 15:04:05", "2026-10-31 04:10:00", time.Local)
	writeFile(t, filepath.Join(dir, "execution_trace_el2_20261031_9.log"),
		"20261031 010000.100 L NB [DIS]  File a:f1 1793420000x001_001101: Distributed file f1 (1793420000x001_001101), 0 bytes\n")
	writeFile(t, filepath.Join(dir, "execution_trace_el2_20261031_100.log"),
		"20261031 040020.781 L NB [DIS]  File a:f2 1793433504x188_001101: Distributed file f2 (1793433504x188_001101), 0 bytes\n")

	act, err := ScanDistributorActivity(dir, "/streams", "el2", "DIS", 7, 600*time.Second, now)
	if err != nil || !act.Success {
		t.Fatalf("err=%v success=%v", err, act.Success)
	}
	want, _ := time.ParseInLocation("2006-01-02 15:04:05", "2026-10-31 04:00:20", time.Local)
	if act.LastActivityTimestamp != float64(want.Unix()) || act.LastFile != "f2" {
		t.Errorf("got ts=%g file=%s, want ts=%d file=f2", act.LastActivityTimestamp, act.LastFile, want.Unix())
	}
}

func TestScanDistributorActivity_SameSecondLastLineWins(t *testing.T) {
	dir := t.TempDir()
	now, _ := time.ParseInLocation("2006-01-02 15:04:05", "2026-10-31 04:10:00", time.Local)
	writeFile(t, filepath.Join(dir, "execution_trace_el2_20261031_90.log"),
		"20261031 040020.100 L NB [DIS]  File a:f_first X: Distributed file f_first (X), 0 bytes\n"+
			"20261031 040020.785 L NB [DIS]  File a:f_last X: Distributed file f_last (X), 0 bytes\n")

	act, _ := ScanDistributorActivity(dir, "/streams", "el2", "DIS", 7, 600*time.Second, now)
	if act.LastFile != "f_last" {
		t.Errorf("LastFile: got %q want f_last", act.LastFile)
	}
}

func TestApplyTargetDirActivity(t *testing.T) {
	now := time.Unix(1793435000, 0)
	target := t.TempDir()
	mtime := time.Unix(1793434406, 0) // 04:13:26, like el2_javad's destination
	if err := os.Chtimes(target, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	trace := NodeActivity{NodeType: "distributor", LastActivityTimestamp: 1793433620, DelaySec: 1380, LastFile: "f_logged", Success: true}

	got := applyTargetDirActivity(trace, target, 600*time.Second, now)
	if got.LastActivityTimestamp != 1793434406 || got.DelaySec != 0 || got.TargetDirMtime != 1793434406 {
		t.Errorf("newer target dir should win: %+v", got)
	}
	if got.LastFile != "f_logged" {
		t.Errorf("LastFile should stay from trace log, got %q", got.LastFile)
	}

	newerTrace := trace
	newerTrace.LastActivityTimestamp = 1793434900
	if got := applyTargetDirActivity(newerTrace, target, 600*time.Second, now); got.LastActivityTimestamp != 1793434900 {
		t.Errorf("newer trace line should win, got %g", got.LastActivityTimestamp)
	}

	none := NodeActivity{NodeType: "distributor", LastFile: "-", Success: false}
	if got := applyTargetDirActivity(none, target, 600*time.Second, now); !got.Success || got.LastActivityTimestamp != 1793434406 {
		t.Errorf("target dir alone should give activity: %+v", got)
	}

	if got := applyTargetDirActivity(trace, filepath.Join(target, "missing"), 600*time.Second, now); got != trace {
		t.Errorf("missing (remote) target dir must leave activity unchanged: %+v", got)
	}
}

// el2_javad's SourceDirectory holds 13 leftovers that do not match the
// collector's Filename "file*" and will never be collected.
func TestScanAllNodeBuffers_FilenameMask(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "sources")
	for _, n := range []string{"file_1", "file_2", "Axx.txt", "D0x.txt"} {
		writeFile(t, filepath.Join(src, n), "x")
	}
	col := filepath.Join(base, "el2", "nodes", "COL")
	writeFile(t, filepath.Join(col, "control", "1", "config"),
		fmt.Sprintf("NodeType\t\"collector\"\nSourceDirectory\t\"%s\"\nFilename\t\"file*\"\n", src))
	nodes := []StreamNode{{Base: "/streams", Stream: "el2", Node: "COL", NodeDir: col, NodeType: "collector"}}

	// A cached full count of the same directory must not be reused.
	results := map[string]DirMetrics{src: {FileCount: 4, ScanSuccess: 1}}
	b := ScanAllNodeBuffers(context.Background(), nodes, results, nil, 2)["/streams/el2/COL/collector"]
	if b.FileCount != 2 || !b.Success {
		t.Errorf("got count=%d success=%v, want 2,true", b.FileCount, b.Success)
	}
}

func TestRenderMetrics_NodeStatusAndBuildInfo(t *testing.T) {
	snap := CacheSnapshot{
		Ready:        true,
		LastScanTime: time.Now(),
		NodeStatuses: []NodeStatus{
			{Base: "/streams", Stream: "el2", Node: "COL", NodeType: "collector", HeartbeatDeadline: 1793438120, State: "TIMER", StateSince: 1793434519, Up: true},
			{Base: "/streams", Stream: "el2", Node: "OLD", NodeType: "decoder"},
		},
		NodeActivities: []NodeActivity{
			{Base: "/streams", Stream: "el2", Node: "DIS", NodeType: "distributor", LastActivityTimestamp: 1793434406, TargetDirMtime: 1793434406, LastFile: "f", Success: true},
		},
	}
	var buf bytes.Buffer
	RenderMetrics(&buf, snap)
	out := buf.String()
	for _, want := range []string{
		`directory_exporter_build_info{version="dev",`,
		`directory_node_info{base="/streams",stream="el2",node="COL",node_type="collector"} 1`,
		`directory_node_up{base="/streams",stream="el2",node="COL"} 1`,
		`directory_node_up{base="/streams",stream="el2",node="OLD"} 0`,
		`directory_node_heartbeat_deadline_timestamp_seconds{base="/streams",stream="el2",node="COL"} 1.79343812e+09`,
		`directory_node_state{base="/streams",stream="el2",node="COL",state="TIMER"} 1`,
		`directory_distributor_target_dir_mtime_seconds{base="/streams",stream="el2",node="DIS"} 1.793434406e+09`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output", want)
		}
	}
	if strings.Contains(out, `node="OLD",state=`) {
		t.Error("node without info file must not have a state series")
	}
}
