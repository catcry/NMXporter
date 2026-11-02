package tracelog

import (
	"context"
	"math"
	"os"
	"path/filepath"
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

func newTestTailer(dir string, backfill bool, maxPerPoll int64) *Tailer {
	return NewTailer(dir, Options{Backfill: backfill, MaxReadPerPoll: maxPerPoll})
}

// Real lines from 172.29.11.102 (Manual_TAP_Process, 2026-10-26).
const tapTrace = `20261026 080004.699 L NB [COLL_TAP]  File TAP:CDIRN11IRNMI11993 : Collected file CDIRN11IRNMI11993 (1793016004x003_0011282), 58220225 bytes, 08:00:04 - 08:00:04
20261026 080340.240 L NB [DEC_TAP]  File TAP:CDIRN11IRNMI11993 1793016004x003_0011282: Read 200004 records: gprsCall/gprsCall 117374
20261026 080340.240 L NB [DEC_TAP]  File TAP:CDIRN11IRNMI11993 1793016004x003_0011282: Wrote 200004 records to DECODED: /comptel/elink/install/buffer/521/DECODED_0_1284/1793016004x003_0011282_1793016217x001_1284_200004: gprsCall/g
20261026 080340.240 L NB [DEC_TAP]  File TAP:CDIRN11IRNMI11993 1793016004x003_0011282: Read 122 bytes
20261026 082106.217 L NB [ENC_TAP]  File TAP:CDIRN11IRNMI11993 1793016004x003_0011282: Wrote 200004 records to TAP_ENCODED: /comptel/elink/install/buffer/521/TAP_ENCODED_0_1281/c.final_1793017263x001_1281_2: batchControlInfo/batch
20261026 082106.273 L NB [DIS_TAP]  File TAP:CDIRN11IRNMI11993 1793016004x003_0011282: Distributed file CDIRN11IRNMI11993 (c.final_1793017263x001_1281), 58220210 bytes, 08:21:06 - 08:21:06
20261026 082106.273 L NB [DIS_TAP]  File TAP:CDIRN11IRNMI11993 1793016004x003_0011282: Performance: real 0.0290s (0 ER/s), CPU 0.0291s (0 ER/s)
`

func localTS(y, mo, d, h, mi, s int, frac float64) float64 {
	return float64(time.Date(y, time.Month(mo), d, h, mi, s, 0, time.Local).Unix()) + frac
}

func TestTraceName(t *testing.T) {
	cases := []struct {
		in, stream, date string
		seq              int
		ok               bool
	}{
		{"execution_trace_el2_javad_20261031_101.log", "el2_javad", "20261031", 101, true},
		{"execution_trace_tap3_outbound_NR_20261031_0.log", "tap3_outbound_NR", "20261031", 0, true},
		{"execution_trace_SMSC_20260920_88.log", "SMSC", "20260920", 88, true},
		{"execution_trace_x_2026103_1.log", "", "", 0, false},
		{"other.log", "", "", 0, false},
	}
	for _, c := range cases {
		s, d, n, ok := TraceName(c.in)
		if ok != c.ok || ok && (s != c.stream || d != c.date || n != c.seq) {
			t.Errorf("%s: got %q %q %d %v", c.in, s, d, n, ok)
		}
	}
}

func TestTraceTailer_ParsesCountersAndLatency(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "execution_trace_Manual_TAP_Process_20261026_0.log"), tapTrace)
	now := time.Date(2026, 10, 26, 9, 0, 0, 0, time.Local)

	tl := newTestTailer(dir, true, 0)
	if err := tl.Poll(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	s := tl.Snapshot()

	col := s.Nodes[NodeKey{"Manual_TAP_Process", "COLL_TAP"}]
	if col.CollectedFiles != 1 || col.CollectedBytes != 58220225 {
		t.Errorf("collector: %+v", col)
	}
	dec := s.Nodes[NodeKey{"Manual_TAP_Process", "DEC_TAP"}]
	if dec.RecordsIn != 200004 || dec.RecordsOut != 200004 {
		t.Errorf("decoder records: in=%d out=%d", dec.RecordsIn, dec.RecordsOut)
	}
	dis := s.Nodes[NodeKey{"Manual_TAP_Process", "DIS_TAP"}]
	if dis.DistributedFiles != 1 || dis.DistributedBytes != 58220210 || dis.LastDistributedFile != "CDIRN11IRNMI11993" {
		t.Errorf("distributor: %+v", dis)
	}
	// 08:21:06.273 - collection epoch 1793016004 (the first ID on the line,
	// not the encoder's c.final_1793017263x001 ID).
	want := localTS(2026, 10, 26, 8, 21, 6, 0.273) - 1793016004
	if !dis.HasLatency || math.Abs(dis.LastLatency-want) > 0.001 {
		t.Errorf("latency: got %v want %v", dis.LastLatency, want)
	}
	if dis.Latency.Count != 1 || math.Abs(dis.Latency.Sum-want) > 0.001 {
		t.Errorf("histogram: %+v", dis.Latency)
	}
	if s.Lines != 7 {
		t.Errorf("lines: got %d want 7", s.Lines)
	}
	if got := s.StreamLastTS["Manual_TAP_Process"]; math.Abs(got-localTS(2026, 10, 26, 8, 21, 6, 0.273)) > 0.001 {
		t.Errorf("stream last ts: %v", got)
	}
}

func TestTraceTailer_IncrementalAndRotation(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 31, 4, 0, 0, 0, time.Local)
	f0 := filepath.Join(dir, "execution_trace_el2_20261031_0.log")
	old := "20261031 035900.000 L NB [DIS]  File a:f0 1793433000x001_001101: Distributed file f0 (1793433000x001_001101), 0 bytes\n"
	writeFile(t, f0, old)

	tl := newTestTailer(dir, false, 0)
	tl.Poll(context.Background(), now) // startup: existing content is skipped
	if n := tl.Snapshot().Nodes[NodeKey{"el2", "DIS"}].DistributedFiles; n != 0 {
		t.Fatalf("history must not be read without backfill, got %d", n)
	}

	fh, _ := os.OpenFile(f0, os.O_APPEND|os.O_WRONLY, 0644)
	fh.WriteString("20261031 040020.781 L NB [DIS]  File a:f1 1793433504x188_001101: Distributed file f1 (1793433504x188_001101), 0 bytes\n")
	fh.WriteString("20261031 040021.000 L NB [DIS]  File a:f2 1793433504x189_001101: Distributed fi") // partial line
	fh.Close()
	tl.Poll(context.Background(), now)
	dis := tl.Snapshot().Nodes[NodeKey{"el2", "DIS"}]
	if dis.DistributedFiles != 1 || dis.LastDistributedFile != "f1" {
		t.Fatalf("after append: %+v", dis)
	}
	want := localTS(2026, 10, 31, 4, 0, 20, 0.781) - 1793433504
	if math.Abs(dis.LastLatency-want) > 0.001 {
		t.Errorf("latency: got %v want %v", dis.LastLatency, want)
	}

	// Finish the partial line, then a rotation creates _1.
	fh, _ = os.OpenFile(f0, os.O_APPEND|os.O_WRONLY, 0644)
	fh.WriteString("le f2 (1793433504x189_001101), 0 bytes\n")
	fh.Close()
	writeFile(t, filepath.Join(dir, "execution_trace_el2_20261031_1.log"),
		"20261031 040030.000 L NB [DIS]  File a:f3 1793433510x001_001101: Distributed file f3 (1793433510x001_001101), 0 bytes\n")
	tl.Poll(context.Background(), now)
	dis = tl.Snapshot().Nodes[NodeKey{"el2", "DIS"}]
	if dis.DistributedFiles != 3 || dis.LastDistributedFile != "f3" {
		t.Errorf("after rotation: files=%d last=%s", dis.DistributedFiles, dis.LastDistributedFile)
	}

	// Nothing new: nothing re-read.
	before := tl.Snapshot().ReadBytes
	tl.Poll(context.Background(), now)
	if tl.Snapshot().ReadBytes != before {
		t.Error("unchanged files must not be re-read")
	}
}

func TestTraceTailer_TruncatedFileRestarts(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 31, 4, 0, 0, 0, time.Local)
	p := filepath.Join(dir, "execution_trace_S_20261031_0.log")
	line := "20261031 040000.000 L NB [COL]  File a:x : Collected file x (1793433600x001_1), 10 bytes\n"
	writeFile(t, p, line+line)
	tl := newTestTailer(dir, true, 0)
	tl.Poll(context.Background(), now)
	writeFile(t, p, line) // replaced by a shorter file
	tl.Poll(context.Background(), now)
	if n := tl.Snapshot().Nodes[NodeKey{"S", "COL"}].CollectedFiles; n != 3 {
		t.Errorf("got %d collected, want 3", n)
	}
}

func TestTraceTailer_ReadBudget(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 31, 4, 0, 0, 0, time.Local)
	line := "20261031 040000.000 L NB [COL]  File a:x : Collected file x (1793433600x001_1), 10 bytes\n"
	writeFile(t, filepath.Join(dir, "execution_trace_S_20261031_0.log"), strings.Repeat(line, 100))
	tl := newTestTailer(dir, true, int64(len(line)*30))
	polls := 0
	for tl.Snapshot().Nodes[NodeKey{"S", "COL"}].CollectedFiles < 100 && polls < 10 {
		tl.Poll(context.Background(), now)
		polls++
	}
	s := tl.Snapshot()
	if s.Nodes[NodeKey{"S", "COL"}].CollectedFiles != 100 || polls != 4 {
		t.Errorf("collected=%d polls=%d (want 100 in 4 polls)", s.Nodes[NodeKey{"S", "COL"}].CollectedFiles, polls)
	}
}

func TestTraceTailer_FailuresScheduledAndOldDaysIgnored(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 30, 16, 0, 0, 0, time.Local)
	writeFile(t, filepath.Join(dir, "execution_trace_SVC_20261030_0.log"),
		"20261030 154400.564 L NB [V_SVCEFB0]  Scheduled execution id 2095_1793389440 finished successfully at 15:44:00\n"+
			"20261030 154500.000 L E [BLN_1]  Node BLN_1 aborted and was disabled\n"+
			"20261030 154600.000 L E [BLN_1]  Node index 3 of BLN_1 failed\n")
	writeFile(t, filepath.Join(dir, "execution_trace_SVC_20261020_0.log"),
		"20261020 100000.000 L E [BLN_1]  Node BLN_1 aborted and was disabled\n")

	tl := newTestTailer(dir, true, 0)
	tl.Poll(context.Background(), now)
	s := tl.Snapshot()
	v := s.Nodes[NodeKey{"SVC", "V_SVCEFB0"}]
	if !v.LastExecutionOK || v.LastExecution != localTS(2026, 10, 30, 15, 44, 0, 0.564) {
		t.Errorf("scheduled: %+v", v)
	}
	b := s.Nodes[NodeKey{"SVC", "BLN_1"}]
	if b.Failures != 2 || b.LastFailure != localTS(2026, 10, 30, 15, 46, 0, 0) {
		t.Errorf("failures: %+v (files older than yesterday must be ignored)", b)
	}
}

// EventLink writes lines out of order across rotations; "last" must follow
// the newest timestamp, and the last latency must belong to that same line.
func TestTraceTailer_OutOfOrderLines(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 31, 5, 0, 0, 0, time.Local)
	writeFile(t, filepath.Join(dir, "execution_trace_el2_20261031_100.log"),
		"20261031 040045.852 L NB [DIS]  File a:file_706345 1793433522x007_001101: Distributed file file_706345 (1793433522x007_001101), 0 bytes\n")
	writeFile(t, filepath.Join(dir, "execution_trace_el2_20261031_101.log"),
		"20261031 040020.785 L NB [DIS]  File a:file_697945 1793433504x189_001101: Distributed file file_697945 (1793433504x189_001101), 0 bytes\n")
	tl := newTestTailer(dir, true, 0)
	tl.Poll(context.Background(), now)
	d := tl.Snapshot().Nodes[NodeKey{"el2", "DIS"}]
	wantLat := localTS(2026, 10, 31, 4, 0, 45, 0.852) - 1793433522
	if d.LastDistributedFile != "file_706345" || math.Abs(d.LastLatency-wantLat) > 0.001 || d.Latency.Count != 2 {
		t.Errorf("got file=%s lat=%v count=%d, want file_706345 lat=%v count=2", d.LastDistributedFile, d.LastLatency, d.Latency.Count, wantLat)
	}
}
