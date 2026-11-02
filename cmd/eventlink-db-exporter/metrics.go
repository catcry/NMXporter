package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

const prometheusContentType = "text/plain; version=0.0.4; charset=utf-8"

var flowEvents = [2]string{"collected", "distributed"}

// render writes all metrics. Every series has a server label (the
// database's name in db.yml), so one exporter can serve several servers.
func render(w io.Writer, build BuildInfo, dbs []Snapshot) {
	help(w, "eventlink_db_exporter_build_info", "Version of the running eventlink-db-exporter.")
	typ(w, "eventlink_db_exporter_build_info", "gauge")
	fmt.Fprintf(w, "eventlink_db_exporter_build_info{version=%s,revision=%s,goversion=%s} 1\n",
		lv(build.Version), lv(build.Revision), lv(build.GoVersion))

	family := func(name, kind, text string, each func(s Snapshot)) {
		help(w, name, text)
		typ(w, name, kind)
		for _, s := range dbs {
			if s.Ready {
				each(s)
			}
		}
	}
	srv := func(s Snapshot) string { return "server=" + lv(s.Name) }

	family("eventlink_db_up", "gauge", "1 = the last poll of the EventLink database succeeded (all queries), 0 = it failed.", func(s Snapshot) {
		fmt.Fprintf(w, "eventlink_db_up{%s} %d\n", srv(s), b2i(s.Up))
	})
	family("eventlink_db_last_poll_timestamp_seconds", "gauge", "Time the last database poll finished.", func(s Snapshot) {
		fmt.Fprintf(w, "eventlink_db_last_poll_timestamp_seconds{%s} %d\n", srv(s), s.LastPoll.Unix())
	})
	family("eventlink_db_poll_duration_seconds", "gauge", "Duration of the last database poll.", func(s Snapshot) {
		fmt.Fprintf(w, "eventlink_db_poll_duration_seconds{%s} %g\n", srv(s), s.PollDuration.Seconds())
	})
	family("eventlink_db_query_errors_total", "counter", "Failed queries by query name.", func(s Snapshot) {
		for _, q := range sortedKeys(s.Errors) {
			fmt.Fprintf(w, "eventlink_db_query_errors_total{%s,query=%s} %d\n", srv(s), lv(q), s.Errors[q])
		}
	})
	family("eventlink_db_read_budget_hits_total", "counter", "Polls that stopped at max_rows_per_poll; the rest is read next poll.", func(s Snapshot) {
		fmt.Fprintf(w, "eventlink_db_read_budget_hits_total{%s} %d\n", srv(s), s.BudgetHits)
	})
	family("eventlink_db_system_tz_offset_seconds", "gauge", "Offset of EventLink's system time zone (used for *system and reporting columns).", func(s Snapshot) {
		fmt.Fprintf(w, "eventlink_db_system_tz_offset_seconds{%s} %d\n", srv(s), s.TZOffset)
	})

	family("eventlink_nodemanager_up", "gauge", "1 = the EventLink Node Manager is running: el_component_status is R and was updated within nodemanager_grace (EventLink's own rule).", func(s Snapshot) {
		for _, c := range s.Components {
			if c.Name == "EventLink" {
				fmt.Fprintf(w, "eventlink_nodemanager_up{%s,host=%s} %d\n", srv(s), lv(c.Host), b2i(c.Up))
			}
		}
	})
	family("eventlink_db_component_status", "gauge", "Always 1. status is el_component_status.status (R = running, S = stopped).", func(s Snapshot) {
		for _, c := range s.Components {
			fmt.Fprintf(w, "eventlink_db_component_status{%s,component=%s,host=%s,status=%s} 1\n", srv(s), lv(c.Name), lv(c.Host), lv(c.Status))
		}
	})
	family("eventlink_db_component_update_timestamp_seconds", "gauge", "Last update of the component's status row (the Node Manager refreshes it while running).", func(s Snapshot) {
		for _, c := range s.Components {
			fmt.Fprintf(w, "eventlink_db_component_update_timestamp_seconds{%s,component=%s,host=%s} %g\n", srv(s), lv(c.Name), lv(c.Host), c.Updated)
		}
	})

	nodeLbl := func(s Snapshot, k NodeKey) string {
		return fmt.Sprintf("%s,stream=%s,node=%s", srv(s), lv(k.Stream), lv(k.Node))
	}
	family("eventlink_db_node_status", "gauge", "Always 1. status is the node status kept by the Node Manager in el_node_log (RUNNING, STOPPED, FAILED, …); frozen while the Node Manager is down.", func(s Snapshot) {
		for _, k := range sortedNodeKeys(s.Nodes) {
			n := s.Nodes[k]
			fmt.Fprintf(w, "eventlink_db_node_status{%s,host=%s,node_type=%s,status=%s} 1\n", nodeLbl(s, k), lv(n.Host), lv(n.NodeType), lv(n.Status))
		}
	})
	family("eventlink_db_node_last_start_timestamp_seconds", "gauge", "Last start of the node (el_node_log.laststartsystem).", func(s Snapshot) {
		for _, k := range sortedNodeKeys(s.Nodes) {
			if n := s.Nodes[k]; n.LastStart > 0 {
				fmt.Fprintf(w, "eventlink_db_node_last_start_timestamp_seconds{%s} %g\n", nodeLbl(s, k), n.LastStart)
			}
		}
	})

	family("eventlink_db_events_total", "counter", "EventLink events (el_events) since the exporter started, by type: E error, W warning, I info. node=\"\" = stream or system events.", func(s Snapshot) {
		for _, k := range sortedNodeKeys(s.Events) {
			e := s.Events[k]
			for _, t := range sortedKeys(e.Count) {
				fmt.Fprintf(w, "eventlink_db_events_total{%s,type=%s} %d\n", nodeLbl(s, k), lv(t), e.Count[t])
			}
		}
	})
	family("eventlink_db_node_last_event_timestamp_seconds", "gauge", "Newest event of the node by type (seeded at startup from the last seed_days days).", func(s Snapshot) {
		for _, k := range sortedNodeKeys(s.Events) {
			e := s.Events[k]
			for _, t := range sortedKeys(e.Last) {
				fmt.Fprintf(w, "eventlink_db_node_last_event_timestamp_seconds{%s,type=%s} %g\n", nodeLbl(s, k), lv(t), e.Last[t])
			}
		}
	})
	family("eventlink_db_node_last_failure_timestamp_seconds", "gauge", "Newest failure event of the node (failure_keys, e.g. NODEMANAGER03011 aborted and was disabled).", func(s Snapshot) {
		for _, k := range sortedNodeKeys(s.Events) {
			if e := s.Events[k]; e.LastFailure > 0 {
				fmt.Fprintf(w, "eventlink_db_node_last_failure_timestamp_seconds{%s} %g\n", nodeLbl(s, k), e.LastFailure)
			}
		}
	})
	family("eventlink_db_node_last_error_info", "gauge", "Always 1. Key and message of the node's newest error event.", func(s Snapshot) {
		for _, k := range sortedNodeKeys(s.Events) {
			if e := s.Events[k]; e.ErrKey != "" || e.ErrMsg != "" {
				fmt.Fprintf(w, "eventlink_db_node_last_error_info{%s,key=%s,message=%s} 1\n", nodeLbl(s, k), lv(e.ErrKey), lv(e.ErrMsg))
			}
		}
	})

	streamLbl := func(s Snapshot, st string) string { return fmt.Sprintf("%s,stream=%s", srv(s), lv(st)) }
	family("eventlink_db_stream_last_collected_timestamp_seconds", "gauge", "Newest collection of the stream (el_reporting_input_details.collectionend; seeded from the last seed_days days).", func(s Snapshot) {
		for _, st := range sortedKeys(s.Streams) {
			if f := s.Streams[st]; f.LastCollected > 0 {
				fmt.Fprintf(w, "eventlink_db_stream_last_collected_timestamp_seconds{%s} %g\n", streamLbl(s, st), f.LastCollected)
			}
		}
	})
	family("eventlink_db_stream_last_distributed_timestamp_seconds", "gauge", "Newest distribution of the stream (el_reporting_output_details.distributionend; seeded from the last seed_days days).", func(s Snapshot) {
		for _, st := range sortedKeys(s.Streams) {
			if f := s.Streams[st]; f.LastDistributed > 0 {
				fmt.Fprintf(w, "eventlink_db_stream_last_distributed_timestamp_seconds{%s} %g\n", streamLbl(s, st), f.LastDistributed)
			}
		}
	})
	for _, c := range []struct {
		name, text string
		get        func(StreamFlow) [2]uint64
	}{
		{"eventlink_db_stream_files_total", "Files collected / distributed by the stream since the exporter started (reporting tables).", func(f StreamFlow) [2]uint64 { return f.Files }},
		{"eventlink_db_stream_bytes_total", "Bytes collected / distributed by the stream since the exporter started (reporting tables).", func(f StreamFlow) [2]uint64 { return f.Bytes }},
		{"eventlink_db_stream_records_total", "Records collected / distributed by the stream since the exporter started (reporting tables).", func(f StreamFlow) [2]uint64 { return f.Records }},
	} {
		family(c.name, "counter", c.text, func(s Snapshot) {
			for _, st := range sortedKeys(s.Streams) {
				v := c.get(s.Streams[st])
				for i, ev := range flowEvents {
					fmt.Fprintf(w, "%s{%s,event=\"%s\"} %d\n", c.name, streamLbl(s, st), ev, v[i])
				}
			}
		})
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedNodeKeys[V any](m map[NodeKey]V) []NodeKey {
	keys := make([]NodeKey, 0, len(m))
	for k := range m {
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

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func help(w io.Writer, name, text string) {
	fmt.Fprintf(w, "# HELP %s %s\n", name, text)
}

func typ(w io.Writer, name, metricType string) {
	fmt.Fprintf(w, "# TYPE %s %s\n", name, metricType)
}

// lv formats a quoted Prometheus label value (\ → \\, newline → \n, " → \").
func lv(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}
