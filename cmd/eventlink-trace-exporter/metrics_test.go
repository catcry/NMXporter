package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/catcry/NMXporter/internal/tracelog"
)

// The directory_* families must render exactly as in directory-exporter
// 1.3.0 so existing dashboards and alerts keep working.
func TestRender_MovedMetricsUnchanged(t *testing.T) {
	var h tracelog.LatencyHistogram
	h.Observe(117)
	h.Observe(1262)
	st := state{
		Base:     "/streams",
		LastPoll: time.Unix(1793433700, 0),
		Build:    BuildInfo{Version: "1.4.0"},
		Trace: &tracelog.Snapshot{
			Nodes: map[tracelog.NodeKey]tracelog.NodeStats{
				{Stream: "el2", Node: "DIS"}: {DistributedFiles: 2, DistributedBytes: 10, LastLatency: 1262, HasLatency: true, Latency: h},
				{Stream: "el2", Node: "COL"}: {CollectedFiles: 5, CollectedBytes: 50, RecordsOut: 7},
			},
			StreamLastTS: map[string]float64{"el2": 1793433681},
			ReadBytes:    100, Lines: 3, FilesTracked: 2,
		},
	}
	var buf bytes.Buffer
	render(&buf, st)
	out := buf.String()
	for _, want := range []string{
		`directory_node_files_total{base="/streams",stream="el2",node="DIS",event="distributed"} 2`,
		`directory_node_files_total{base="/streams",stream="el2",node="COL",event="collected"} 5`,
		`directory_node_records_total{base="/streams",stream="el2",node="COL",direction="out"} 7`,
		`directory_stream_latency_seconds_bucket{base="/streams",stream="el2",node="DIS",le="60"} 0`,
		`directory_stream_latency_seconds_bucket{base="/streams",stream="el2",node="DIS",le="120"} 1`,
		`directory_stream_latency_seconds_bucket{base="/streams",stream="el2",node="DIS",le="1800"} 2`,
		`directory_stream_latency_seconds_bucket{base="/streams",stream="el2",node="DIS",le="+Inf"} 2`,
		`directory_stream_latency_seconds_count{base="/streams",stream="el2",node="DIS"} 2`,
		`directory_stream_last_latency_seconds{base="/streams",stream="el2",node="DIS"} 1262`,
		`directory_tracelog_last_line_timestamp_seconds{base="/streams",stream="el2"} 1.793433681e+09`,
		`directory_tracelog_read_bytes_total 100`,
		`eventlink_trace_exporter_build_info{version="1.4.0",revision="",goversion=""} 1`,
		`eventlink_trace_ready 1`,
		`eventlink_trace_last_poll_timestamp_seconds 1793433700`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestRender_NameMetricsFromTrace(t *testing.T) {
	dir := t.TempDir()
	log := "20261031 040000.000 L NB [COL]  File a:file_1.el.gz : Collected file file_1.el.gz (1793433600x001_001101), 0 bytes\n" +
		"20261031 040001.000 L NB [COL]  File a:file_2.el.gz : Collected file file_2.el.gz (1793433601x001_001101), 0 bytes\n" +
		"20261031 040030.000 L NB [DIS_ENC]  File a:file_1.el.gz 1793433600x001_001101: Distributed file file_1.el.encoded (1793433600x001_001101), 0 bytes\n" +
		"20261031 040045.000 L NB [DIS_MVNE]  File src1:trigger_process 1355885629x001_001172: Distributed file file_1.el.mvne (trigger_process_1), 0 bytes\n"
	if err := os.WriteFile(filepath.Join(dir, "execution_trace_el2_20261031_0.log"), []byte(log), 0644); err != nil {
		t.Fatal(err)
	}
	tl := tracelog.NewTailer(dir, tracelog.Options{Backfill: true})
	tl.Poll(context.Background(), time.Date(2026, 10, 31, 4, 1, 0, 0, time.Local))

	var buf bytes.Buffer
	render(&buf, state{Base: "/streams", Trace: tl.Snapshot()})
	out := buf.String()
	for _, want := range []string{
		`eventlink_name_last_latency_seconds{base="/streams",stream="el2",node="DIS_ENC"} 30`,
		`eventlink_name_last_latency_seconds{base="/streams",stream="el2",node="DIS_MVNE"} 45`,
		`eventlink_name_latency_seconds_bucket{base="/streams",stream="el2",node="DIS_MVNE",le="60"} 1`,
		`eventlink_latency_match_total{base="/streams",stream="el2",node="DIS_MVNE",method="name"} 1`,
		`eventlink_latency_match_total{base="/streams",stream="el2",node="DIS_MVNE",method="none"} 0`,
		`eventlink_pending_files{base="/streams",stream="el2"} 1`,
		`eventlink_tracked_names 2`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	if !strings.Contains(out, `eventlink_oldest_pending_timestamp_seconds{base="/streams",stream="el2"} `) {
		t.Error("missing oldest pending timestamp")
	}
	// The trigger line has no usable ID: the old ID-based family has no
	// sample for DIS_MVNE.
	if strings.Contains(out, `directory_stream_last_latency_seconds{base="/streams",stream="el2",node="DIS_MVNE"}`) {
		t.Error("DIS_MVNE must have no ID-based latency")
	}
}

func TestRender_NotReady(t *testing.T) {
	var buf bytes.Buffer
	render(&buf, state{Base: "/streams"})
	if out := buf.String(); !strings.Contains(out, "eventlink_trace_ready 0") || strings.Contains(out, "directory_node_files_total") {
		t.Errorf("before the first poll only build info and ready=0 are exported:\n%s", out)
	}
}

func TestLoadConfig(t *testing.T) {
	p := filepath.Join(t.TempDir(), "trace.yml")
	os.WriteFile(p, []byte("tracelog_dir: /x\npoll_interval: 15s\nname_ttl: 6h\nmax_names: 1000\nbackfill: true\n"), 0644)
	t.Setenv("CONFIG_FILE", p)
	t.Setenv("LISTEN_ADDR", ":9302")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TracelogDir != "/x" || cfg.PollInterval != 15*time.Second || cfg.NameTTL != 6*time.Hour ||
		cfg.MaxNames != 1000 || !cfg.Backfill || cfg.ListenAddr != ":9302" || cfg.BaseLabel != "/streams" {
		t.Errorf("got %+v", cfg)
	}
	os.WriteFile(p, []byte("tracelog_dir: /x\npoll_interval: soon\n"), 0644)
	if _, err := LoadConfig(); err == nil {
		t.Error("invalid duration must be an error")
	}
}
