package tracelog

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// Without backfill, the first poll seeds only the last collected /
// distributed times from existing files; counters stay at 0 and later lines
// still move the times forward.
func TestSeed_LastCollectedDistributed(t *testing.T) {
	dir := t.TempDir()
	// Three days ago: the only line of the quiet distributor OLD_DIS.
	writeFile(t, filepath.Join(dir, "execution_trace_s_20261028_0.log"),
		"20261028 100000.000 L NB [OLD_DIS]  File a:f0 1793196000x001_001: Distributed file f0.x (1793196000x001_001), 1 bytes\n")
	// Today, rotated out of order: _1 holds an older line than _0.
	writeFile(t, filepath.Join(dir, "execution_trace_s_20261031_0.log"),
		"20261031 040010.000 L NB [COL]  File a:f2 : Collected file f2.gz (1793433610x001_001), 0 bytes\n"+
			"20261031 040020.500 L NB [DIS]  File a:f2 1793433610x001_001: Distributed file f2.el.ict (1793433610x001_001), 0 bytes\n")
	writeFile(t, filepath.Join(dir, "execution_trace_s_20261031_1.log"),
		"20261031 040000.000 L NB [COL]  File a:f1 : Collected file f1.gz (1793433600x001_001), 0 bytes\n")
	// Older than seed_days: ignored.
	writeFile(t, filepath.Join(dir, "execution_trace_s_20261020_0.log"),
		"20261020 100000.000 L NB [GONE]  File a:g : Collected file g (1792504800x001_001), 0 bytes\n")

	now := time.Date(2026, 10, 31, 4, 5, 0, 0, time.Local)
	tl := NewTailer(dir, Options{SeedDays: 7})
	tl.Poll(context.Background(), now)
	s := tl.Snapshot()

	col := s.Nodes[NodeKey{"s", "COL"}]
	if col.LastCollected != localTS(2026, 10, 31, 4, 0, 10, 0) || col.LastCollectedFile != "f2.gz" {
		t.Errorf("COL last collected: %v %q", col.LastCollected, col.LastCollectedFile)
	}
	dis := s.Nodes[NodeKey{"s", "DIS"}]
	if dis.LastDistributed != localTS(2026, 10, 31, 4, 0, 20, 0.5) || dis.LastDistributedFile != "f2.el.ict" {
		t.Errorf("DIS last distributed: %v %q", dis.LastDistributed, dis.LastDistributedFile)
	}
	if old := s.Nodes[NodeKey{"s", "OLD_DIS"}]; old.LastDistributed != localTS(2026, 10, 28, 10, 0, 0, 0) {
		t.Errorf("OLD_DIS from 3 days ago: %v", old.LastDistributed)
	}
	if _, ok := s.Nodes[NodeKey{"s", "GONE"}]; ok {
		t.Error("files older than seed_days must not be read")
	}
	if col.CollectedFiles != 0 || dis.DistributedFiles != 0 || dis.Latency.Count != 0 || s.Pending["s"].Files != 0 {
		t.Errorf("seed must not count: col=%+v dis=%+v pending=%+v", col, dis, s.Pending["s"])
	}
	if s.SeedFiles != 3 || s.StreamLastTS["s"] != localTS(2026, 10, 31, 4, 0, 20, 0.5) {
		t.Errorf("seed files=%d lastTS=%v", s.SeedFiles, s.StreamLastTS["s"])
	}

	// A new line after start counts and moves the time forward.
	writeFile(t, filepath.Join(dir, "execution_trace_s_20261031_2.log"),
		"20261031 040100.000 L NB [COL]  File a:f3 : Collected file f3.gz (1793433660x001_001), 0 bytes\n")
	tl.Poll(context.Background(), now)
	col = tl.Snapshot().Nodes[NodeKey{"s", "COL"}]
	if col.CollectedFiles != 1 || col.LastCollected != localTS(2026, 10, 31, 4, 1, 0, 0) || col.LastCollectedFile != "f3.gz" {
		t.Errorf("after start: %+v", col)
	}
}

// seed_max_mb stops reading a stream's older files once reached.
func TestSeed_MaxBytesPerStream(t *testing.T) {
	dir := t.TempDir()
	line := "20261031 040000.000 L NB [A]  File a:f : Collected file f (1793433600x001_001), 0 bytes\n"
	writeFile(t, filepath.Join(dir, "execution_trace_s_20261031_1.log"), line)
	writeFile(t, filepath.Join(dir, "execution_trace_s_20261031_0.log"),
		"20261031 030000.000 L NB [B]  File a:f : Collected file f (1793430000x001_001), 0 bytes\n")
	tl := NewTailer(dir, Options{SeedDays: 1, SeedMaxBytes: int64(len(line))})
	tl.Poll(context.Background(), time.Date(2026, 10, 31, 4, 5, 0, 0, time.Local))
	s := tl.Snapshot()
	if _, ok := s.Nodes[NodeKey{"s", "B"}]; ok || s.SeedFiles != 1 {
		t.Errorf("must stop after the newest file: files=%d nodes=%v", s.SeedFiles, s.Nodes)
	}
}
