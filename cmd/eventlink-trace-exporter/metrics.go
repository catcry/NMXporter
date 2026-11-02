package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/catcry/NMXporter/internal/tracelog"
)

const prometheusContentType = "text/plain; version=0.0.4; charset=utf-8"

// state is what one /metrics response is rendered from.
type state struct {
	Trace        *tracelog.Snapshot // nil until the first poll finished
	Base         string             // base label, same as directory-exporter's
	LastPoll     time.Time
	PollDuration time.Duration
	PollErrors   uint64
	Build        BuildInfo
}

// render writes all metrics. The directory_* families keep the names and
// labels they had in directory-exporter 1.3.0, so dashboards and alerts
// built on them keep working; the eventlink_* families are new.
func render(w io.Writer, st state) {
	help(w, "eventlink_trace_exporter_build_info", "Version of the running eventlink-trace-exporter.")
	typ(w, "eventlink_trace_exporter_build_info", "gauge")
	fmt.Fprintf(w, "eventlink_trace_exporter_build_info{version=%s,revision=%s,goversion=%s} 1\n",
		lv(st.Build.Version), lv(st.Build.Revision), lv(st.Build.GoVersion))

	ready := 0
	if st.Trace != nil {
		ready = 1
	}
	help(w, "eventlink_trace_ready", "1 once the first trace log poll has finished.")
	typ(w, "eventlink_trace_ready", "gauge")
	fmt.Fprintf(w, "eventlink_trace_ready %d\n", ready)
	if st.Trace == nil {
		return
	}
	help(w, "eventlink_trace_last_poll_timestamp_seconds", "Time the last trace log poll finished.")
	typ(w, "eventlink_trace_last_poll_timestamp_seconds", "gauge")
	fmt.Fprintf(w, "eventlink_trace_last_poll_timestamp_seconds %d\n", st.LastPoll.Unix())
	help(w, "eventlink_trace_poll_duration_seconds", "Duration of the last trace log poll.")
	typ(w, "eventlink_trace_poll_duration_seconds", "gauge")
	fmt.Fprintf(w, "eventlink_trace_poll_duration_seconds %g\n", st.PollDuration.Seconds())
	help(w, "eventlink_trace_poll_errors_total", "Trace log polls that failed (trace directory unreadable).")
	typ(w, "eventlink_trace_poll_errors_total", "counter")
	fmt.Fprintf(w, "eventlink_trace_poll_errors_total %d\n", st.PollErrors)

	renderTrace(w, st.Trace, st.Base)
	renderNames(w, st.Trace, st.Base)
	renderLast(w, st.Trace, st.Base)
}

// renderLast writes the time of each node's last collected and last
// distributed file, including what the startup seed found.
func renderLast(w io.Writer, tr *tracelog.Snapshot, base string) {
	keys := sortedNodeKeys(tr)
	lbl := func(k tracelog.NodeKey) string {
		return fmt.Sprintf("base=%s,stream=%s,node=%s", lv(base), lv(k.Stream), lv(k.Node))
	}
	help(w, "eventlink_node_last_collected_timestamp_seconds", "Time of the node's newest \"Collected file\" trace line (seeded at startup from the last seed_days of trace logs).")
	typ(w, "eventlink_node_last_collected_timestamp_seconds", "gauge")
	for _, k := range keys {
		if s := tr.Nodes[k]; s.LastCollected > 0 {
			fmt.Fprintf(w, "eventlink_node_last_collected_timestamp_seconds{%s} %g\n", lbl(k), s.LastCollected)
		}
	}
	help(w, "eventlink_node_last_distributed_timestamp_seconds", "Time of the node's newest \"Distributed file\" trace line (seeded at startup from the last seed_days of trace logs).")
	typ(w, "eventlink_node_last_distributed_timestamp_seconds", "gauge")
	for _, k := range keys {
		if s := tr.Nodes[k]; s.LastDistributed > 0 {
			fmt.Fprintf(w, "eventlink_node_last_distributed_timestamp_seconds{%s} %g\n", lbl(k), s.LastDistributed)
		}
	}
	help(w, "eventlink_trace_seed_bytes", "Trace log bytes read at startup for the last collected / distributed times.")
	typ(w, "eventlink_trace_seed_bytes", "gauge")
	fmt.Fprintf(w, "eventlink_trace_seed_bytes %d\n", tr.SeedBytes)
	help(w, "eventlink_trace_seed_files", "Trace files read at startup for the last collected / distributed times.")
	typ(w, "eventlink_trace_seed_files", "gauge")
	fmt.Fprintf(w, "eventlink_trace_seed_files %d\n", tr.SeedFiles)
}

// renderTrace writes the trace log engine metrics. Counters start at 0 when
// the exporter starts (unless trace_backfill is on).
func renderTrace(w io.Writer, tr *tracelog.Snapshot, base string) {
	if tr == nil {
		return
	}
	help(w, "directory_tracelog_read_bytes_total", "Trace log bytes read by the exporter.")
	typ(w, "directory_tracelog_read_bytes_total", "counter")
	fmt.Fprintf(w, "directory_tracelog_read_bytes_total %d\n", tr.ReadBytes)
	help(w, "directory_tracelog_lines_total", "Trace log lines parsed by the exporter.")
	typ(w, "directory_tracelog_lines_total", "counter")
	fmt.Fprintf(w, "directory_tracelog_lines_total %d\n", tr.Lines)
	help(w, "directory_tracelog_files_tracked", "Trace files (today and yesterday) currently followed.")
	typ(w, "directory_tracelog_files_tracked", "gauge")
	fmt.Fprintf(w, "directory_tracelog_files_tracked %d\n", tr.FilesTracked)
	help(w, "directory_tracelog_read_budget_hits_total", "Scan cycles that stopped reading early because of trace_max_read_mb; the rest is read next cycle.")
	typ(w, "directory_tracelog_read_budget_hits_total", "counter")
	fmt.Fprintf(w, "directory_tracelog_read_budget_hits_total %d\n", tr.BudgetHits)

	streams := make([]string, 0, len(tr.StreamLastTS))
	for s := range tr.StreamLastTS {
		streams = append(streams, s)
	}
	sort.Strings(streams)
	help(w, "directory_tracelog_last_line_timestamp_seconds",
		"Timestamp of the newest trace line seen for the stream. If it stops moving while files still flow, EventLink stopped logging.")
	typ(w, "directory_tracelog_last_line_timestamp_seconds", "gauge")
	for _, s := range streams {
		fmt.Fprintf(w, "directory_tracelog_last_line_timestamp_seconds{base=%s,stream=%s} %g\n",
			lv(base), lv(s), tr.StreamLastTS[s])
	}

	keys := make([]tracelog.NodeKey, 0, len(tr.Nodes))
	for k := range tr.Nodes {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Stream != keys[j].Stream {
			return keys[i].Stream < keys[j].Stream
		}
		return keys[i].Node < keys[j].Node
	})
	lbl := func(k tracelog.NodeKey) string {
		return fmt.Sprintf("base=%s,stream=%s,node=%s", lv(base), lv(k.Stream), lv(k.Node))
	}

	help(w, "directory_node_files_total", "Files handled by the node according to trace logs (event = collected | distributed).")
	typ(w, "directory_node_files_total", "counter")
	for _, k := range keys {
		s := tr.Nodes[k]
		if s.CollectedFiles > 0 {
			fmt.Fprintf(w, "directory_node_files_total{%s,event=\"collected\"} %d\n", lbl(k), s.CollectedFiles)
		}
		if s.DistributedFiles > 0 {
			fmt.Fprintf(w, "directory_node_files_total{%s,event=\"distributed\"} %d\n", lbl(k), s.DistributedFiles)
		}
	}
	help(w, "directory_node_bytes_total", "Bytes handled by the node according to trace logs (event = collected | distributed).")
	typ(w, "directory_node_bytes_total", "counter")
	for _, k := range keys {
		s := tr.Nodes[k]
		if s.CollectedFiles > 0 {
			fmt.Fprintf(w, "directory_node_bytes_total{%s,event=\"collected\"} %d\n", lbl(k), s.CollectedBytes)
		}
		if s.DistributedFiles > 0 {
			fmt.Fprintf(w, "directory_node_bytes_total{%s,event=\"distributed\"} %d\n", lbl(k), s.DistributedBytes)
		}
	}
	help(w, "directory_node_records_total", "Records read (direction=in) and written (direction=out) by the node according to trace logs.")
	typ(w, "directory_node_records_total", "counter")
	for _, k := range keys {
		s := tr.Nodes[k]
		if s.RecordsIn > 0 {
			fmt.Fprintf(w, "directory_node_records_total{%s,direction=\"in\"} %d\n", lbl(k), s.RecordsIn)
		}
		if s.RecordsOut > 0 {
			fmt.Fprintf(w, "directory_node_records_total{%s,direction=\"out\"} %d\n", lbl(k), s.RecordsOut)
		}
	}
	help(w, "directory_node_failures_total", "'aborted and was disabled' / 'Node index … failed' trace lines for the node.")
	typ(w, "directory_node_failures_total", "counter")
	help(w, "directory_node_last_failure_timestamp_seconds", "Time of the node's last failure trace line.")
	typ(w, "directory_node_last_failure_timestamp_seconds", "gauge")
	for _, k := range keys {
		s := tr.Nodes[k]
		if s.Failures > 0 {
			fmt.Fprintf(w, "directory_node_failures_total{%s} %d\n", lbl(k), s.Failures)
			fmt.Fprintf(w, "directory_node_last_failure_timestamp_seconds{%s} %g\n", lbl(k), s.LastFailure)
		}
	}
	help(w, "directory_node_last_execution_timestamp_seconds", "Time of the node's last 'Scheduled execution … finished' line (status = success | other).")
	typ(w, "directory_node_last_execution_timestamp_seconds", "gauge")
	for _, k := range keys {
		s := tr.Nodes[k]
		if s.LastExecution > 0 {
			status := "other"
			if s.LastExecutionOK {
				status = "success"
			}
			fmt.Fprintf(w, "directory_node_last_execution_timestamp_seconds{%s,status=\"%s\"} %g\n", lbl(k), status, s.LastExecution)
		}
	}

	help(w, "directory_stream_latency_seconds", "Collected → distributed latency per distributed file: distribution time minus the collection epoch in the file ID.")
	typ(w, "directory_stream_latency_seconds", "histogram")
	for _, k := range keys {
		h := tr.Nodes[k].Latency
		if h.Count == 0 {
			continue
		}
		var cum uint64
		for i, ub := range tracelog.LatencyBuckets {
			cum += h.Counts[i]
			fmt.Fprintf(w, "directory_stream_latency_seconds_bucket{%s,le=\"%g\"} %d\n", lbl(k), ub, cum)
		}
		fmt.Fprintf(w, "directory_stream_latency_seconds_bucket{%s,le=\"+Inf\"} %d\n", lbl(k), h.Count)
		fmt.Fprintf(w, "directory_stream_latency_seconds_sum{%s} %g\n", lbl(k), h.Sum)
		fmt.Fprintf(w, "directory_stream_latency_seconds_count{%s} %d\n", lbl(k), h.Count)
	}
	help(w, "directory_stream_last_latency_seconds", "Collected → distributed latency of the node's last distributed file.")
	typ(w, "directory_stream_last_latency_seconds", "gauge")
	for _, k := range keys {
		s := tr.Nodes[k]
		if s.HasLatency {
			fmt.Fprintf(w, "directory_stream_last_latency_seconds{%s} %g\n", lbl(k), s.LastLatency)
		}
	}
}

func sortedNodeKeys(tr *tracelog.Snapshot) []tracelog.NodeKey {
	keys := make([]tracelog.NodeKey, 0, len(tr.Nodes))
	for k := range tr.Nodes {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Stream != keys[j].Stream {
			return keys[i].Stream < keys[j].Stream
		}
		return keys[i].Node < keys[j].Node
	})
	return keys
}

// renderNames writes the latencies matched by file name (part before the
// first ".") and the files still waiting for a distribution.
func renderNames(w io.Writer, tr *tracelog.Snapshot, base string) {
	keys := sortedNodeKeys(tr)
	lbl := func(k tracelog.NodeKey) string {
		return fmt.Sprintf("base=%s,stream=%s,node=%s", lv(base), lv(k.Stream), lv(k.Node))
	}

	help(w, "eventlink_name_latency_seconds", "Collected → distributed latency per distributed file, matched by the file name before the first \".\" (falls back to the file ID when the collection was not seen).")
	typ(w, "eventlink_name_latency_seconds", "histogram")
	for _, k := range keys {
		h := tr.Nodes[k].NameLatency
		if h.Count == 0 {
			continue
		}
		var cum uint64
		for i, ub := range tracelog.LatencyBuckets {
			cum += h.Counts[i]
			fmt.Fprintf(w, "eventlink_name_latency_seconds_bucket{%s,le=\"%g\"} %d\n", lbl(k), ub, cum)
		}
		fmt.Fprintf(w, "eventlink_name_latency_seconds_bucket{%s,le=\"+Inf\"} %d\n", lbl(k), h.Count)
		fmt.Fprintf(w, "eventlink_name_latency_seconds_sum{%s} %g\n", lbl(k), h.Sum)
		fmt.Fprintf(w, "eventlink_name_latency_seconds_count{%s} %d\n", lbl(k), h.Count)
	}
	help(w, "eventlink_name_last_latency_seconds", "Collected → distributed latency of the node's last distributed file, matched by name (ID fallback).")
	typ(w, "eventlink_name_last_latency_seconds", "gauge")
	for _, k := range keys {
		if s := tr.Nodes[k]; s.HasNameLatency {
			fmt.Fprintf(w, "eventlink_name_last_latency_seconds{%s} %g\n", lbl(k), s.NameLastLatency)
		}
	}
	help(w, "eventlink_latency_match_total", "Distributed file lines by how their collection time was found: name, id (name not seen), none (no usable ID either).")
	typ(w, "eventlink_latency_match_total", "counter")
	for _, k := range keys {
		s := tr.Nodes[k]
		if s.DistributedFiles == 0 {
			continue
		}
		for m, name := range tracelog.MatchNames {
			fmt.Fprintf(w, "eventlink_latency_match_total{%s,method=\"%s\"} %d\n", lbl(k), name, s.Matches[m])
		}
	}

	streams := make([]string, 0, len(tr.Pending)+len(tr.StreamLastTS))
	seen := make(map[string]bool)
	for s := range tr.StreamLastTS {
		streams, seen[s] = append(streams, s), true
	}
	for s := range tr.Pending {
		if !seen[s] {
			streams = append(streams, s)
		}
	}
	sort.Strings(streams)
	slbl := func(s string) string { return fmt.Sprintf("base=%s,stream=%s", lv(base), lv(s)) }

	help(w, "eventlink_pending_files", "Files collected (trace log) with no Distributed file line yet, matched by name. Includes files EventLink filtered or discarded without logging it; the buffer folders (directory-exporter) are the ground truth.")
	typ(w, "eventlink_pending_files", "gauge")
	for _, s := range streams {
		fmt.Fprintf(w, "eventlink_pending_files{%s} %d\n", slbl(s), tr.Pending[s].Files)
	}
	help(w, "eventlink_oldest_pending_timestamp_seconds", "Collection time of the oldest pending file of the stream (see eventlink_pending_files). Absent when nothing is pending.")
	typ(w, "eventlink_oldest_pending_timestamp_seconds", "gauge")
	for _, s := range streams {
		if p := tr.Pending[s]; p.Files > 0 {
			fmt.Fprintf(w, "eventlink_oldest_pending_timestamp_seconds{%s} %g\n", slbl(s), p.Oldest)
		}
	}
	help(w, "eventlink_pending_evicted_total", "Pending files dropped without a distribution: reason=expired (older than name_ttl) or limit (max_names reached).")
	typ(w, "eventlink_pending_evicted_total", "counter")
	for _, s := range streams {
		p := tr.Pending[s]
		if p.EvictedExpired > 0 {
			fmt.Fprintf(w, "eventlink_pending_evicted_total{%s,reason=\"expired\"} %d\n", slbl(s), p.EvictedExpired)
		}
		if p.EvictedLimit > 0 {
			fmt.Fprintf(w, "eventlink_pending_evicted_total{%s,reason=\"limit\"} %d\n", slbl(s), p.EvictedLimit)
		}
	}
	help(w, "eventlink_tracked_names", "Collected file names kept in memory for matching.")
	typ(w, "eventlink_tracked_names", "gauge")
	fmt.Fprintf(w, "eventlink_tracked_names %d\n", tr.TrackedNames)
}

func help(w io.Writer, name, text string) {
	fmt.Fprintf(w, "# HELP %s %s\n", name, text)
}

func typ(w io.Writer, name, metricType string) {
	fmt.Fprintf(w, "# TYPE %s %s\n", name, metricType)
}

// lv formats a quoted Prometheus label value (\ → \, newline → \n, " → \").
func lv(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}
