package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// prometheusContentType is the value of the Content-Type header expected
// by Prometheus scrapers for the text exposition format v0.0.4.
const prometheusContentType = "text/plain; version=0.0.4; charset=utf-8"

// RenderMetrics writes the current CacheSnapshot to w using the Prometheus
// text exposition format (https://prometheus.io/docs/instrumenting/exposition_formats/).
//
// Zero disk I/O is performed — all data comes from the snapshot.
//
// Cold-start behaviour: when snap.Ready is false, only directory_cache_ready{0}
// is emitted. This prevents Prometheus alert rules from firing on zero-valued
// metrics during the warm-up window before the first scan completes.
func RenderMetrics(w io.Writer, snap CacheSnapshot) {
	// ── directory_cache_ready (always present) ────────────────────────────────
	help(w, "directory_cache_ready",
		"1 = first scan complete, 0 = cache is still warming up (cold start).")
	typ(w, "directory_cache_ready", "gauge")

	readyVal := 0.0
	if snap.Ready {
		readyVal = 1.0
	}
	fmt.Fprintf(w, "directory_cache_ready %g\n", readyVal)

	help(w, "directory_exporter_build_info",
		"Always 1. Labels identify the running exporter build.")
	typ(w, "directory_exporter_build_info", "gauge")
	fmt.Fprintf(w, "directory_exporter_build_info{version=%s,revision=%s,goversion=%s} 1\n",
		lv(buildInfo.Version), lv(buildInfo.Revision), lv(buildInfo.GoVersion))

	if !snap.Ready {
		// Omit all other metric families until the cache is warm.
		// The /-/ready endpoint will return 503 during this window, so
		// Kubernetes / load balancers can hold traffic accordingly.
		return
	}

	// ── Control-plane metrics ─────────────────────────────────────────────────

	help(w, "directory_last_scan_timestamp_seconds",
		"Unix timestamp of when the metrics cache was last refreshed.")
	typ(w, "directory_last_scan_timestamp_seconds", "gauge")
	fmt.Fprintf(w, "directory_last_scan_timestamp_seconds %g\n",
		float64(snap.LastScanTime.Unix()))

	help(w, "directory_watched_total",
		"Total number of directories currently on the Watch List.")
	typ(w, "directory_watched_total", "gauge")
	fmt.Fprintf(w, "directory_watched_total %d\n", snap.WatchedTotal)

	help(w, "directory_scan_errors_total",
		"Cumulative count of scan errors (permission denied, vanished directories, etc.).")
	typ(w, "directory_scan_errors_total", "counter")
	fmt.Fprintf(w, "directory_scan_errors_total %d\n", snap.ScanErrors)

	help(w, "directory_reload_total",
		"Cumulative count of /-/reload triggers received by the exporter.")
	typ(w, "directory_reload_total", "counter")
	fmt.Fprintf(w, "directory_reload_total %d\n", snap.ReloadTotal)

	if len(snap.Entries) > 0 {
		// ✅ FIX: sort entries by (base, stream, type) so metric output is
		// deterministic across scrapes. snap.Entries comes from map iteration
		// in Snapshot(), which is random in Go. Non-deterministic ordering makes
		// `curl /metrics | diff` noisy and complicates debugging.
		sorted := make([]DirMetrics, len(snap.Entries))
		copy(sorted, snap.Entries)
		sort.Slice(sorted, func(i, j int) bool {
			ki := sorted[i].Labels.Base + "/" + sorted[i].Labels.Stream + "/" + sorted[i].Labels.Type
			kj := sorted[j].Labels.Base + "/" + sorted[j].Labels.Stream + "/" + sorted[j].Labels.Type
			return ki < kj
		})

		// ── Data-plane metrics (one time-series per watched directory) ────────────
		// Each metric family is emitted with a single HELP + TYPE header followed
		// by all instances, as required by the text format specification.

		help(w, "directory_file_count",
			"Total number of regular files directly in the monitored directory (non-recursive).")
		typ(w, "directory_file_count", "gauge")
		for _, dm := range sorted {
			fmt.Fprintf(w, "directory_file_count{base=%s,stream=%s,type=%s} %d\n",
				lv(dm.Labels.Base), lv(dm.Labels.Stream), lv(dm.Labels.Type),
				dm.FileCount)
		}

		// Oldest and newest timestamps are only meaningful when files exist.
		// Omitting them for empty directories prevents false "stuck queue" alerts.
		help(w, "directory_oldest_file_timestamp_seconds",
			"Modification time (Unix epoch) of the oldest file. Detects stuck queues.")
		typ(w, "directory_oldest_file_timestamp_seconds", "gauge")
		for _, dm := range sorted {
			if dm.ScanSuccess == 1 && dm.FileCount > 0 {
				fmt.Fprintf(w, "directory_oldest_file_timestamp_seconds{base=%s,stream=%s,type=%s} %g\n",
					lv(dm.Labels.Base), lv(dm.Labels.Stream), lv(dm.Labels.Type),
					dm.OldestTimestampSec)
			}
		}

		help(w, "directory_newest_file_timestamp_seconds",
			"Modification time (Unix epoch) of the newest file. Detects halted ingest.")
		typ(w, "directory_newest_file_timestamp_seconds", "gauge")
		for _, dm := range sorted {
			if dm.ScanSuccess == 1 && dm.FileCount > 0 {
				fmt.Fprintf(w, "directory_newest_file_timestamp_seconds{base=%s,stream=%s,type=%s} %g\n",
					lv(dm.Labels.Base), lv(dm.Labels.Stream), lv(dm.Labels.Type),
					dm.NewestTimestampSec)
			}
		}

		help(w, "directory_scrape_duration_seconds",
			"Wall-clock duration of the last background scan for this directory.")
		typ(w, "directory_scrape_duration_seconds", "gauge")
		for _, dm := range sorted {
			fmt.Fprintf(w, "directory_scrape_duration_seconds{base=%s,stream=%s,type=%s} %g\n",
				lv(dm.Labels.Base), lv(dm.Labels.Stream), lv(dm.Labels.Type),
				dm.ScanDurationSec)
		}
	}

	// ── Node Activity & Delay Metrics ─────────────────────────────────────────
	if len(snap.NodeActivities) > 0 {		sortedAct := make([]NodeActivity, len(snap.NodeActivities))
		copy(sortedAct, snap.NodeActivities)
		sort.Slice(sortedAct, func(i, j int) bool {
			ki := sortedAct[i].Base + "/" + sortedAct[i].Stream + "/" + sortedAct[i].Node + "/" + sortedAct[i].NodeType
			kj := sortedAct[j].Base + "/" + sortedAct[j].Stream + "/" + sortedAct[j].Node + "/" + sortedAct[j].NodeType
			return ki < kj
		})

		// Collectors Delay Metrics
		hasCollector := false
		for _, act := range sortedAct {
			if act.NodeType == "collector" && act.Success {
				hasCollector = true
				break
			}
		}
		if hasCollector {
			help(w, "directory_collector_last_activity_timestamp_seconds",
				"Unix timestamp (seconds) of the last file collected by the collector node.")
			typ(w, "directory_collector_last_activity_timestamp_seconds", "gauge")
			for _, act := range sortedAct {
				if act.NodeType == "collector" && act.Success && act.LastActivityTimestamp > 0 {
					fmt.Fprintf(w, "directory_collector_last_activity_timestamp_seconds{base=%s,stream=%s,node=%s} %g\n",
						lv(act.Base), lv(act.Stream), lv(act.Node), act.LastActivityTimestamp)
				}
			}

			help(w, "directory_collector_delay_seconds",
				"Delay in seconds since the last collected file (now - last_activity_timestamp). 0 if delay <= min_delay.")
			typ(w, "directory_collector_delay_seconds", "gauge")
			for _, act := range sortedAct {
				if act.NodeType == "collector" && act.Success {
					fmt.Fprintf(w, "directory_collector_delay_seconds{base=%s,stream=%s,node=%s} %g\n",
						lv(act.Base), lv(act.Stream), lv(act.Node), act.DelaySec)
				}
			}
		}

		// Distributors Delay Metrics
		hasDistributor := false
		for _, act := range sortedAct {
			if act.NodeType == "distributor" && act.Success {
				hasDistributor = true
				break
			}
		}
		if hasDistributor {
			help(w, "directory_distributor_last_activity_timestamp_seconds",
				"Unix timestamp (seconds) of the last file distributed by the distributor node.")
			typ(w, "directory_distributor_last_activity_timestamp_seconds", "gauge")
			for _, act := range sortedAct {
				if act.NodeType == "distributor" && act.Success && act.LastActivityTimestamp > 0 {
					fmt.Fprintf(w, "directory_distributor_last_activity_timestamp_seconds{base=%s,stream=%s,node=%s} %g\n",
						lv(act.Base), lv(act.Stream), lv(act.Node), act.LastActivityTimestamp)
				}
			}

			help(w, "directory_distributor_delay_seconds",
				"Delay in seconds since the last distributed file (now - last_activity_timestamp). 0 if delay <= min_delay.")
			typ(w, "directory_distributor_delay_seconds", "gauge")
			for _, act := range sortedAct {
				if act.NodeType == "distributor" && act.Success {
					fmt.Fprintf(w, "directory_distributor_delay_seconds{base=%s,stream=%s,node=%s} %g\n",
						lv(act.Base), lv(act.Stream), lv(act.Node), act.DelaySec)
				}
			}
		}

		// Last Processed File Info
		hasFileInfo := false
		for _, act := range sortedAct {
			if act.Success && act.LastFile != "" && act.LastFile != "-" {
				hasFileInfo = true
				break
			}
		}
		if hasFileInfo {
			help(w, "directory_node_last_file_info",
				"Information metric exposing the last processed (collected/distributed) filename.")
			typ(w, "directory_node_last_file_info", "gauge")
			for _, act := range sortedAct {
				if act.Success && act.LastFile != "" && act.LastFile != "-" {
					fmt.Fprintf(w, "directory_node_last_file_info{base=%s,stream=%s,node=%s,type=%s,filename=%s} 1\n",
						lv(act.Base), lv(act.Stream), lv(act.Node), lv(act.NodeType), lv(act.LastFile))
				}
			}
		}
	}

	// ── Node Buffer Backlog Metrics (monitoring.sh BUFFER_INDEX) ─────────────
	// Non-recursive regular-file count of each collector node's resolved
	// SourceDirectory buffer directory. Reuses directory_file_count when the
	// buffer is already watched, so no extra disk I/O in the common case.
	if len(snap.NodeBuffers) > 0 {
		sortedBuf := make([]NodeBuffer, len(snap.NodeBuffers))
		copy(sortedBuf, snap.NodeBuffers)
		sort.Slice(sortedBuf, func(i, j int) bool {
			ki := sortedBuf[i].Base + "/" + sortedBuf[i].Stream + "/" + sortedBuf[i].Node + "/" + sortedBuf[i].NodeType
			kj := sortedBuf[j].Base + "/" + sortedBuf[j].Stream + "/" + sortedBuf[j].Node + "/" + sortedBuf[j].NodeType
			return ki < kj
		})

		help(w, "directory_buffer_files",
			"Number of files waiting in the collector node's SourceDirectory buffer directory (non-recursive, mirrors monitoring.sh BUFFER_INDEX). 0 when empty, missing, or unreadable.")
		typ(w, "directory_buffer_files", "gauge")
		for _, nb := range sortedBuf {
			fmt.Fprintf(w, "directory_buffer_files{base=%s,stream=%s,node=%s,type=%s} %d\n",
				lv(nb.Base), lv(nb.Stream), lv(nb.Node), lv(nb.NodeType),
				nb.FileCount)
		}
	}

	renderTargetDirMtimes(w, snap.NodeActivities)
	renderNodeStatuses(w, snap.NodeStatuses)
	renderQueueOldestCollected(w, snap.Entries)
}

// renderQueueOldestCollected exposes the collection time of the oldest
// EventLink file still waiting in each queue (taken from file names).
func renderQueueOldestCollected(w io.Writer, entries []DirMetrics) {
	var rows []DirMetrics
	for _, dm := range entries {
		if dm.ScanSuccess == 1 && dm.FileCount > 0 && dm.OldestCollectedSec > 0 {
			rows = append(rows, dm)
		}
	}
	if len(rows) == 0 {
		return
	}
	sort.Slice(rows, func(i, j int) bool {
		ki := rows[i].Labels.Base + "/" + rows[i].Labels.Stream + "/" + rows[i].Labels.Type
		kj := rows[j].Labels.Base + "/" + rows[j].Labels.Stream + "/" + rows[j].Labels.Type
		return ki < kj
	})
	help(w, "directory_queue_oldest_collected_timestamp_seconds",
		"Collection time (from the EventLink file ID in the name) of the oldest file still waiting in this queue.")
	typ(w, "directory_queue_oldest_collected_timestamp_seconds", "gauge")
	for _, dm := range rows {
		fmt.Fprintf(w, "directory_queue_oldest_collected_timestamp_seconds{base=%s,stream=%s,type=%s} %g\n",
			lv(dm.Labels.Base), lv(dm.Labels.Stream), lv(dm.Labels.Type), dm.OldestCollectedSec)
	}
}

// buildInfo is resolved once; main.version is fixed at link time.
var buildInfo = currentBuildInfo()

// renderTargetDirMtimes exposes the distributor TargetDirectory mtime that
// feeds directory_distributor_last_activity_timestamp_seconds.
func renderTargetDirMtimes(w io.Writer, acts []NodeActivity) {
	var rows []NodeActivity
	for _, a := range acts {
		if a.NodeType == "distributor" && a.TargetDirMtime > 0 {
			rows = append(rows, a)
		}
	}
	if len(rows) == 0 {
		return
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].Base+"/"+rows[i].Stream+"/"+rows[i].Node < rows[j].Base+"/"+rows[j].Stream+"/"+rows[j].Node
	})
	help(w, "directory_distributor_target_dir_mtime_seconds",
		"Modification time of the distributor's local TargetDirectory (changes whenever a file lands there).")
	typ(w, "directory_distributor_target_dir_mtime_seconds", "gauge")
	for _, a := range rows {
		fmt.Fprintf(w, "directory_distributor_target_dir_mtime_seconds{base=%s,stream=%s,node=%s} %g\n",
			lv(a.Base), lv(a.Stream), lv(a.Node), a.TargetDirMtime)
	}
}

// renderNodeStatuses writes node identity and liveness read from control/1/.
func renderNodeStatuses(w io.Writer, statuses []NodeStatus) {
	if len(statuses) == 0 {
		return
	}
	sorted := make([]NodeStatus, len(statuses))
	copy(sorted, statuses)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Base+"/"+sorted[i].Stream+"/"+sorted[i].Node < sorted[j].Base+"/"+sorted[j].Stream+"/"+sorted[j].Node
	})

	help(w, "directory_node_info",
		"Always 1. node_type is NodeType from control/1/config (empty when unknown).")
	typ(w, "directory_node_info", "gauge")
	for _, s := range sorted {
		fmt.Fprintf(w, "directory_node_info{base=%s,stream=%s,node=%s,node_type=%s} 1\n",
			lv(s.Base), lv(s.Stream), lv(s.Node), lv(s.NodeType))
	}

	help(w, "directory_node_up",
		"1 = node running (heartbeat deadline not passed by more than heartbeat_grace), 0 = stopped or no heartbeat.")
	typ(w, "directory_node_up", "gauge")
	for _, s := range sorted {
		up := 0
		if s.Up {
			up = 1
		}
		fmt.Fprintf(w, "directory_node_up{base=%s,stream=%s,node=%s} %d\n",
			lv(s.Base), lv(s.Stream), lv(s.Node), up)
	}

	help(w, "directory_node_heartbeat_deadline_timestamp_seconds",
		"Epoch from control/1/heartbeat: the time by which the node must write its next heartbeat.")
	typ(w, "directory_node_heartbeat_deadline_timestamp_seconds", "gauge")
	for _, s := range sorted {
		if s.HeartbeatDeadline > 0 {
			fmt.Fprintf(w, "directory_node_heartbeat_deadline_timestamp_seconds{base=%s,stream=%s,node=%s} %g\n",
				lv(s.Base), lv(s.Stream), lv(s.Node), s.HeartbeatDeadline)
		}
	}

	help(w, "directory_node_state",
		"Always 1. state is the node state from control/1/info (e.g. IDLE, TIMER).")
	typ(w, "directory_node_state", "gauge")
	for _, s := range sorted {
		if s.State != "" {
			fmt.Fprintf(w, "directory_node_state{base=%s,stream=%s,node=%s,state=%s} 1\n",
				lv(s.Base), lv(s.Stream), lv(s.Node), lv(s.State))
		}
	}

	help(w, "directory_node_state_since_timestamp_seconds",
		"Epoch from control/1/info: when the node entered its current state.")
	typ(w, "directory_node_state_since_timestamp_seconds", "gauge")
	for _, s := range sorted {
		if s.StateSince > 0 {
			fmt.Fprintf(w, "directory_node_state_since_timestamp_seconds{base=%s,stream=%s,node=%s} %g\n",
				lv(s.Base), lv(s.Stream), lv(s.Node), s.StateSince)
		}
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func help(w io.Writer, name, text string) {
	fmt.Fprintf(w, "# HELP %s %s\n", name, text)
}

func typ(w io.Writer, name, metricType string) {
	fmt.Fprintf(w, "# TYPE %s %s\n", name, metricType)
}

// lv formats a string as a quoted Prometheus label value, applying the
// required escape rules from the text format spec:
//
//	\  →  \\
//	\n →  \n   (literal backslash-n in output)
//	"  →  \"
func lv(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}
