package tracelog

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"
)

// Real lines from 172.29.11.102 (SMSC, 2026-09-20), shortened. One input is
// distributed under several names (.sms.mvno, .sms.kish); Dis_KISH_LI's line
// carries the ID of a 2012 trigger file instead of the input's ID.
const smscTrace = `20260920 161250.000 L NB [Z_SSZB0]  File Z_SSZB0:Z_SSZB0_ZX3118_20260920150643.sms : Collected file Z_SSZB0_ZX3118_20260920150643.sms (1789904570x001_001263), 10485774 bytes, 15:12:50 - 15:12:50
20260920 161252.000 L NB [Dec_1]  File Z_SBEA0:Z_SBEA0_ZX7696_20260920135247.sms 1789904572x001_001267: Collected file - (1789904572x001_001267), 0 bytes, 03:30:00
20260920 161257.404 L NB [Dis_BonyanMed_WBMS]  File Z_SBEA0:Z_SBEA0_ZX7696_20260920135247.sms 1789904572x001_001267: Distributed file Z_SBEA0_ZX7696_20260920135247.sms.mvno (1789904572x425774_001267_1789904577x001_4263), 453 bytes, 15:12:57 - 15:12:57
20260920 161300.286 L NB [Dis_KISH_LI]  File src1:trigger_process 1355885629x001_001172: Distributed file Z_SSZB0_ZX3118_20260920150643.sms.kish (trigger_process_1789904580080), 381 bytes, 15:13:00 - 15:13:00
20260920 161300.389 L NB [Dis_KISH_LI]  File src1:trigger_process 1355885629x001_001172: Distributed file Z_SSZB0_ZX3119_20260920150649.sms.kish (trigger_process_1789904580286), 1145 bytes, 15:13:00 - 15:13:00
20260920 161301.000 L NB [Dis_MVNE_82]  File Z_SSZB0:Z_SSZB0_ZX3118_20260920150643.sms 1789904570x001_001263: Distributed file Z_SSZB0_ZX3118_20260920150643.sms.mvne (1789904570x001_001263_1789904580x001_4263), 2100 bytes, 15:13:01 - 15:13:01
20260920 161305.984 L NB [Z_SSZB0]  File Z_SSZB0:Z_SSZB0_ZX3121_20260920150701.sms : Collected file Z_SSZB0_ZX3121_20260920150701.sms (1789904585x001_001263), 10485774 bytes, 15:13:00 - 15:13:05
`

func TestBaseNameAndFileNames(t *testing.T) {
	for in, want := range map[string]string{
		"file_20261031101050.el.gz":                         "file_20261031101050",
		"file_20261031101050.el.encoded":                    "file_20261031101050",
		"file_745367":                                       "file_745367",
		"1785310088x001_0011282_1785310088x001_1284_200003": "1785310088x001_0011282_1785310088x001_1284_200003",
	} {
		if got := baseName(in); got != want {
			t.Errorf("baseName(%s) = %s, want %s", in, got, want)
		}
	}
	dash := []byte("20260920 161252.000 L NB [Dec_1]  File Z_SBEA0:Z_SBEA0_ZX7696_20260920135247.sms 1789904572x001_001267: Collected file - (1789904572x001_001267), 0 bytes")
	if got := collectedFileName(dash); got != "Z_SBEA0_ZX7696_20260920135247.sms" {
		t.Errorf("'Collected file -' must take the name from 'File <src>:NAME', got %q", got)
	}
	el2 := []byte("20261031 040006.352 L NB [EL_CFG_FTP_COLLECTOR]  File el_coll_test1_javad:file_745367 : Collected file file_745367 (1793433606x167_001101), 0 bytes")
	if got := collectedFileName(el2); got != "file_745367" {
		t.Errorf("got %q", got)
	}
}

func TestNameLatency_SMSC(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "execution_trace_SMSC_20260920_0.log"), smscTrace)
	tl := newTestTailer(dir, true, 0)
	tl.Poll(context.Background(), time.Date(2026, 9, 20, 16, 14, 0, 0, time.Local))
	s := tl.Snapshot()

	// Dis_KISH_LI: the ID belongs to a 2012 trigger file → no ID latency,
	// but the name finds the collection at 16:12:50 → 10.286 s.
	kish := s.Nodes[NodeKey{"SMSC", "Dis_KISH_LI"}]
	if kish.Latency.Count != 0 {
		t.Errorf("Dis_KISH_LI must have no ID latency, got %d", kish.Latency.Count)
	}
	if kish.Matches[MatchName] != 1 || kish.Matches[MatchNone] != 1 {
		t.Errorf("Dis_KISH_LI matches: %v (want 1 by name, 1 none)", kish.Matches)
	}
	if want := 10.286; math.Abs(kish.NameLatency.Sum-want) > 0.001 {
		t.Errorf("Dis_KISH_LI name latency: got %v want %v", kish.NameLatency.Sum, want)
	}
	// The newest line (ZX3119) has no collection: the last latency stays
	// that of the last matched file.
	if !kish.HasNameLatency || math.Abs(kish.NameLastLatency-10.286) > 0.001 {
		t.Errorf("Dis_KISH_LI last name latency: %v %v", kish.HasNameLatency, kish.NameLastLatency)
	}

	// Renamed output (.sms.mvno) found by name, collected via "Collected file -".
	wbms := s.Nodes[NodeKey{"SMSC", "Dis_BonyanMed_WBMS"}]
	if wbms.Matches[MatchName] != 1 || math.Abs(wbms.NameLastLatency-5.404) > 0.001 {
		t.Errorf("WBMS: matches=%v last=%v", wbms.Matches, wbms.NameLastLatency)
	}
	// Second distributor of the same input is measured too.
	mvne := s.Nodes[NodeKey{"SMSC", "Dis_MVNE_82"}]
	if mvne.Matches[MatchName] != 1 || math.Abs(mvne.NameLastLatency-11) > 0.001 {
		t.Errorf("MVNE_82: matches=%v last=%v", mvne.Matches, mvne.NameLastLatency)
	}

	// ZX3121 collected, not distributed yet → pending.
	p := s.Pending["SMSC"]
	if p.Files != 1 || p.Oldest != localTS(2026, 9, 20, 16, 13, 5, 0.984) {
		t.Errorf("pending: %+v", p)
	}
}

func TestNameTracker_OutOfOrderAndReuse(t *testing.T) {
	n := newNameTracker(time.Hour, 100)
	// Distribution read before its collection (rotated files out of order).
	if _, ok := n.distributed("s", "f1", 1000); ok {
		t.Fatal("unknown name must not match")
	}
	n.collected("s", "f1", 990)
	if p := n.pending()["s"]; p.Files != 0 {
		t.Errorf("late collection of a distributed file must not be pending: %+v", p)
	}
	// The name is reused by a new file later on.
	n.collected("s", "f1", 2000)
	if p := n.pending()["s"]; p.Files != 1 || p.Oldest != 2000 {
		t.Errorf("reused name must start a new pending file: %+v", p)
	}
	if lat, ok := n.distributed("s", "f1", 2030); !ok || lat != 30 {
		t.Errorf("reused name latency: %v %v", lat, ok)
	}
	// Same name in another stream is another file.
	n.collected("t", "f1", 2000)
	if p := n.pending()["t"]; p.Files != 1 {
		t.Errorf("streams must be separate: %+v", p)
	}
}

func TestNameTracker_ExpiryAndLimit(t *testing.T) {
	n := newNameTracker(100*time.Second, 3)
	n.collected("s", "a", 10)
	n.collected("s", "b", 20)
	n.collected("s", "c", 30)
	n.collected("s", "d", 40) // over the limit: "a" goes
	p := n.pending()["s"]
	if p.Files != 3 || p.EvictedLimit != 1 || p.Oldest != 20 {
		t.Errorf("limit: %+v", p)
	}
	n.distributed("s", "b", 50)
	n.expire(135) // cutoff 35: "c" expires (pending), "b" expires silently
	p = n.pending()["s"]
	if p.Files != 1 || p.EvictedExpired != 1 || p.Oldest != 40 {
		t.Errorf("expiry: %+v", p)
	}
	if len(n.byKey) != 1 {
		t.Errorf("tracked names: %d", len(n.byKey))
	}
}

// Without backfill, a file collected before the exporter started falls back
// to the ID; one collected after start is matched by name.
func TestNameLatency_FallbackToID(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "execution_trace_el2_20261031_0.log")
	writeFile(t, p, "20261031 035900.000 L NB [COL]  File a:file_1 : Collected file file_1 (1793433540x001_001101), 0 bytes\n")
	now := time.Date(2026, 10, 31, 4, 5, 0, 0, time.Local)
	tl := newTestTailer(dir, false, 0)
	tl.Poll(context.Background(), now)
	writeFile(t, p, "20261031 035900.000 L NB [COL]  File a:file_1 : Collected file file_1 (1793433540x001_001101), 0 bytes\n"+
		"20261031 040000.000 L NB [COL]  File a:file_2 : Collected file file_2 (1793433600x001_001101), 0 bytes\n"+
		"20261031 040010.000 L NB [DIS]  File a:file_1 1793433540x001_001101: Distributed file file_1 (1793433540x001_001101), 0 bytes\n"+
		"20261031 040012.500 L NB [DIS]  File a:file_2 1793433600x001_001101: Distributed file file_2 (1793433600x001_001101), 0 bytes\n")
	tl.Poll(context.Background(), now)
	d := tl.Snapshot().Nodes[NodeKey{"el2", "DIS"}]
	if d.Matches[MatchID] != 1 || d.Matches[MatchName] != 1 {
		t.Errorf("matches: %v", d.Matches)
	}
	if d.NameLatency.Count != 2 || math.Abs(d.NameLastLatency-12.5) > 0.001 {
		t.Errorf("name latency: count=%d last=%v", d.NameLatency.Count, d.NameLastLatency)
	}
	if got := tl.Snapshot().Pending["el2"].Files; got != 0 {
		t.Errorf("pending: %d", got)
	}
}
