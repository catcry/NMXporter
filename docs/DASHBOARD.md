# EventLink Directory Monitoring: dashboard guide (1.7.2)

Grafana dashboard `eventlink-dir-overview`
(`http://<grafana>:3000/d/eventlink-dir-overview`). It is loaded from
`grafana/dashboards/eventlink-overview.json` by file provisioning.

This guide explains every section, panel and table column: what it shows, which metric it comes from, and how it is
calculated. The metrics themselves are described in [METRICS.md](METRICS.md).

How to read the tables: each table runs several Prometheus queries (A, B, C…), one per column. Grafana joins them
on their labels (`stream`, or `stream` + `node`), so **one row = one stream** or **one row = one node**, and each
column is one query's value for that row. "–" or empty = that query returned nothing for the row.

---

## Top bar (variables)

| Variable | Values | Used for |
|---|---|---|
| **Data source** | Prometheus data sources | Which Prometheus to query |
| **EventLink server** | values of the `eventlink` label, or All | Server filter; added to every query as `eventlink=~"$eventlink"`. Keep **All** while one of the jobs has no `eventlink` label: choosing a server hides that job's panels |
| **Stream** | `label_values(directory_file_count, stream)`, or All | Every panel filters `stream=~"$stream"` |
| **Stuck after** | 10m, 30m, 1h, 6h, 1d (default 10m) | When an input queue counts as stuck: oldest file older than this |
| **Delay threshold** | 5m, 10m, 30m, 1h, 6h (default 10m) | When a live delay or p95 counts as "delayed"; also the dashed threshold line in graphs |
| **Investigate (top 3)** | the 3 streams with the highest issue score | Stream shown in the "Investigate" section |

Common building blocks used in many queries:
- **Input queues only**: `type=~"[^/]+/input/.+"`. A queue is the output of one node and the input of the next, so
  counting both would double it.
- **Node name from a folder**: `label_replace(..., "node", "$1", "type", "([^/]+)/.*")` takes the first part of
  `type` (`BLN_1/input/OUT_0_2085` → `BLN_1`).
- **Live delay** of a node: `time() - directory_queue_oldest_collected_timestamp_seconds` (collection time from
  the file ID in the name), or, for queues whose names have no ID, `time() - directory_oldest_file_timestamp_seconds`
  (mtime) for queues that hold files. The maximum over the node's input queues. Counted only for nodes **running
  now or at any time in the last 24h** (`max_over_time(directory_node_up[24h]) == 1`), so nodes switched off long
  ago do not show as delayed.
- **New in 24h** for discarded/rejected: `clamp_min(count now - count 24h ago, 0)`. This is an estimate: files
  that came and were cleaned up within the 24h are not seen.

Colours: green = OK, orange = look at it, red = act.

---

## 1. Top tiles (no section title)

| Tile | Shows | How it is calculated | Colours |
|---|---|---|---|
| **Exporter** | UP / DOWN | `up{job="directory-exporter"}` × `directory_cache_ready`: Prometheus can scrape it **and** its first scan has finished | red DOWN, green UP |
| **Data age** | seconds | `time() - directory_last_scan_timestamp_seconds`: age of the newest scan. The scan runs every 120 s | orange > 240 s, red > 600 s |
| **Running nodes** | count | `sum(directory_node_up)`: nodes whose heartbeat deadline has not passed | – |
| **Stopped (24h)** | count | Nodes down now (`directory_node_up == 0`) that were up at some point in the last 24h. Nodes off for longer are not counted | red ≥ 1 |
| **Failed (24h)** | count | Nodes with a failure trace line in 24h: `increase(directory_node_failures_total[24h]) > 0` | red ≥ 1 |
| **Delay p95 (1h)** | seconds | Collected → distributed, 95th percentile over 1h, by file ID: `histogram_quantile(0.95, rate(directory_stream_latency_seconds_bucket[1h]))`. Empty when nothing was distributed in that hour | orange > 5 min, red > 30 min |
| **Total backlog** | files | `sum(directory_file_count)` over input queues | – |
| **Stuck queues** | count | Input queues whose oldest file is older than *Stuck after* | red ≥ 1 |
| **Collectors idle** | count | Running collectors with no collected file for more than 10 min: `directory_collector_delay_seconds > 0` and node up. No traffic is not always a fault | orange ≥ 1 |
| **Distributors idle** | count | The same for distributors (`directory_distributor_delay_seconds`) | orange ≥ 1 |
| **Discarded · 24h** | files | New files in `discarded/` folders in 24h (see "New in 24h" above) | orange ≥ 1 |
| **Rejected · 24h** | files | New files in `rejected/` folders in 24h. A file count, not a CDR record count | orange ≥ 1 |

---

## 2. Delay — live

The live delay comes from the buffer folders, so it works even when EventLink writes no trace lines. The "p95" and
"last file" values come from the trace logs.

### Tiles

| Tile | How it is calculated | Colours |
|---|---|---|
| **Streams delayed** | Streams whose live delay (the worst node of the stream) is above *Delay threshold* | red ≥ 1 |
| **Nodes delayed** | Nodes whose live delay is above *Delay threshold* | red ≥ 1 |
| **Max live delay** | Largest live delay over all nodes; 0 = nothing waiting | orange > 5 min, red > 30 min |
| **Streams slow (p95 1h)** | Streams whose by-name p95 over 1h (`eventlink_name_latency_seconds`) is above *Delay threshold* | orange ≥ 1 |
| **Matched by name (1h)** | Share of distributed files whose collection was found by name: `rate(eventlink_latency_match_total{method="name"}[1h]) / rate(eventlink_latency_match_total[1h])`. Low = names change completely between input and output, or the trace exporter restarted recently | orange < 80 %, green ≥ 80 % |
| **Trace exporter** | `up{job="eventlink-trace-exporter"}` × `eventlink_trace_ready` | red DOWN, green UP |

### Table: Delay per stream (one row = one stream)

| Column | Query | Meaning | Colours |
|---|---|---|---|
| Stream | label | Stream name | – |
| Delayed | B | 1 when the live delay is above *Delay threshold*, shown as DELAYED; else OK. Streams without input queues show 0 | red DELAYED, green OK |
| Live delay | A | Age of the oldest file waiting in the stream's input queues (nodes running now or in 24h) | orange > 5 min, red > 30 min |
| Files waiting | D | `sum(directory_file_count)` over the stream's input queues | – |
| Last file delay | E | Collected → distributed delay of the last distributed file, by name (`eventlink_name_last_latency_seconds`); only for distributors that sent something in the last hour | orange > 5 min, red > 30 min |
| p95 1h (name) | F | p95 of the by-name delay over 1h | orange > 5 min, red > 30 min |
| p95 1h (ID) | G | p95 of the by-ID delay over 1h, to compare | – |
| Waiting at stopped nodes | C | Live delay of nodes that were **not** running at any time in 24h: data stuck behind switched-off nodes | – |
| Pending (trace) | H | `eventlink_pending_files`: collected with no distribution logged. A hint only: filtered or discarded files are never logged | – |
| Last trace line | I | `time() - directory_tracelog_last_line_timestamp_seconds`. Grows while files still flow = EventLink stopped writing trace lines, so trace columns are frozen | – |

### Table: Delay per node (one row = one node)

Rows: nodes with files waiting, or with distributions in the trace logs.

| Column | Query | Meaning | Colours |
|---|---|---|---|
| Stream, Node | labels | – | – |
| Status | A | `directory_node_up`: Running / Stopped | green / red |
| Delayed | C | Live delay above *Delay threshold* (nodes running now or in 24h) | red DELAYED, green OK |
| Live delay | B | Age of the oldest file in this node's input queues | orange > 5 min, red > 30 min |
| Last file delay | D | By-name delay of the last file this distributor sent | orange > 5 min, red > 30 min |
| p95 1h (name) | E | By-name p95 for this node over 1h | orange > 5 min, red > 30 min |
| Last file delay (ID) | F | `directory_stream_last_latency_seconds`: the same by file ID, to compare | – |
| Name match 1h | G | Share of this node's distributions matched by name in the last hour | – |
| Since last file | H | `time() - directory_distributor_last_activity_timestamp_seconds`: time since the distributor last sent a file | – |

### Graphs

| Panel | Shows |
|---|---|
| **Live delay by stream** | Live delay per stream over time, plus a dashed line at *Delay threshold*. A line that keeps rising = files are not moving |
| **Collected → distributed p95 by stream (by name)** | By-name p95 per stream over 15-minute windows. Only streams that distributed in the window appear |

---

## 3. Last collected / distributed

Three sources are combined, and the **newest** wins:
- **Collected**: the collector's `storage/1/audit_info` (`directory_collector_last_activity_timestamp_seconds`),
  or the newest `Collected file` trace line (`eventlink_node_last_collected_timestamp_seconds`, seeded from 7 days).
- **Distributed**: the newest `Distributed file` trace line, or the change time of the distributor's local
  `TargetDirectory` (`directory_distributor_last_activity_timestamp_seconds`,
  `eventlink_node_last_distributed_timestamp_seconds`).

Nodes whose `node_type` (from `control/1/config`) is known and is not a collector are left out of "collected", and
likewise for distributors. Nodes with an unknown type count.

### Tiles

| Tile | How it is calculated | Colours |
|---|---|---|
| **Trace logging** | `time() - max(directory_tracelog_last_line_timestamp_seconds)`: time since the newest line in **any** trace log. The Node Manager writes a line every minute, so red = the Node Manager stopped and trace values are frozen | red > 10 min |
| **Streams collected · 1h** | Streams whose last collected time is within the last hour | – |
| **Streams distributed · 1h** | Streams whose last distributed time is within the last hour | – |
| **Running, nothing distributed · 24h** | Streams with running nodes but no distribution known in 24h | orange ≥ 1 |

### Table: Last collected / distributed per stream (one row = one stream)

| Column | Query | Meaning | Colours |
|---|---|---|---|
| Stream | label | – | – |
| Running nodes | A | `sum(directory_node_up)` of the stream | – |
| Last collected | B | Date/time of the newest collection (newest of audit_info and trace) | – |
| Since collected | C | `time() -` the same | orange > 1h, red > 24h |
| Last distributed | D | Date/time of the newest distribution (newest of trace and TargetDirectory) | – |
| Since distributed | E | `time() -` the same | orange > 1h, red > 24h |
| Files waiting | F | Files in the stream's input queues | – |

"–" = no source knows a time. Most streams on the reference server never log file events in the trace logs and have no
audit_info / TargetDirectory.

### Table: Last collected / distributed per node (one row = one node)

Rows: every node with a known last collection or distribution.

| Column | Query | Meaning |
|---|---|---|
| Stream, Node | labels | – |
| Type | A (label) | `node_type` from `control/1/config` |
| Status | B | Running / Stopped (heartbeat) |
| Last collected / Since collected | C / D | As in the stream table, for this node |
| Last distributed / Since distributed | E / F | As in the stream table, for this node |

---

## 4. EventLink database

Data from eventlink-db-exporter (read-only). The **Node Manager** writes all of it, so while it is down these values
are frozen.

### Tiles

| Tile | How it is calculated | Colours |
|---|---|---|
| **Node Manager** | `eventlink_nodemanager_up`: status `R` and updated within 150 s (EventLink's own rule) | red DOWN, green UP |
| **Node Manager down for** | Time since the Node Manager's status row was last updated; 0 while it is up | red > 0 |
| **Failed nodes (EventLink)** | `count(eventlink_db_node_status{status="FAILED"})` | red ≥ 1 |
| **Running nodes (EventLink)** | `count(... {status="RUNNING"})`. Compare with **Running nodes** at the top (heartbeat): they differ while the Node Manager is down | – |
| **Errors · 24h** | `increase(eventlink_db_events_total{type="E"}[24h])`: error events. Counted from the exporter start, so less than 24h after a restart | red ≥ 1 |
| **DB exporter** | `up{job="eventlink-db-exporter"}` × `eventlink_db_up` (last poll succeeded) | red DOWN, green UP |

### Table: Failed nodes (EventLink database) (one row = one FAILED node)

| Column | Query | Meaning | Colours |
|---|---|---|---|
| Stream, Node, Type | A (labels) | Nodes with status FAILED; `node_type` decoded from `el_node_log.type` | – |
| Last start | B | `el_node_log.laststartsystem` | – |
| Last failure | C | Newest `NODEMANAGER03011` / `03023` event (aborted and disabled) | – |
| Since last error | D | Time since the node's newest error event (`el_events` type E) | red < 1h, orange < 24h |
| Last error, Message | E (labels) | Key and text of that newest error event | – |

### Table: Node status per stream (EventLink database) (one row = one stream)

| Column | Query | Meaning | Colours |
|---|---|---|---|
| Stream | label | – | – |
| Running / Stopped / Failed | A / B / C | Number of nodes per status in `el_node_log` | Failed red ≥ 1 |
| Running (heartbeat) | D | `sum(directory_node_up)`, the file-based view, to compare | – |
| Last collected (DB) | E | Newest `el_reporting_input_details.collectionend` | – |
| Last distributed (DB) | F | Newest `el_reporting_output_details.distributionend` | – |

---

## 5. Top 3 streams with issues

### Table (one row = one of the 3 worst streams)

**Issue score** = sum of the columns below, and the 3 highest are shown. **Click a stream name** to reload the
whole dashboard filtered to that stream.

| Column | Query | How it is calculated | Colours |
|---|---|---|---|
| Stream | label | Link: focus the dashboard on this stream | – |
| Issue score | A | Sum of the 7 parts below (+ a tiny backlog/10¹² tie-breaker) | orange ≥ 0.5, red ≥ 5 |
| Stopped (24h) | I | Nodes down now that were up in the last 24h | red ≥ 1 |
| Failed (24h) | J | Nodes with a failure trace line in 24h | red ≥ 1 |
| Not consuming | B | Nodes whose oldest input file is older than *Stuck after* | red ≥ 1 |
| Collectors idle | C | Running collectors with no collected file for more than 10 min | orange ≥ 1 |
| Distributors idle | D | Running distributors with no distributed file for more than 10 min | orange ≥ 1 |
| New discards/rejects 24h | E | Nodes with new files in discarded/rejected in 24h | orange ≥ 1 |
| Backlog points | H | `max(0, 2·log10(backlog + 1) − 5)`: 0 below ~300 files, 1 at 1 000, 5 at 100 000, about 8.4 at 5 million | – |
| Backlog (files) | F | Files in the stream's input queues | – |
| Oldest waiting | G | Age of the oldest file in the stream's input queues (mtime) | orange > 10 min, red > 1h |

---

## 6. Investigate: \<stream\>

Focuses on the stream in **Investigate (top 3)**.

### Stat panel: \<stream\>: health

One tile per number:

| Tile | How it is calculated |
|---|---|
| Nodes | `count(directory_node_info)`: all nodes of the stream |
| Running / Down | Nodes with heartbeat up / down |
| Failed 24h | Nodes with a failure trace line in 24h |
| Backlog | Files in input queues |
| Not consuming | Nodes whose oldest input file is older than *Stuck after* |
| Oldest waiting | Age of the oldest input file (mtime) |
| Delay p95 | By-ID collected → distributed p95 over 1h |
| Collectors idle / Distributors idle | As in the top tiles, for this stream |
| Discards+rejects 24h | New files in discarded/rejected in 24h |

### Table: \<stream\>: nodes (one row = one node, worst first)

| Column | Query | Meaning | Colours |
|---|---|---|---|
| Node | label | – | – |
| Type | H (label) | `node_type` from `control/1/config` | – |
| Status | A | The worst condition that applies, in this order: **Failed (24h)** (a failure trace line) > **Stopped** (heartbeat deadline passed) > **Not consuming** (input older than *Stuck after*) > **Idle** (collector/distributor last file older than 10 min) > **OK** | purple, dark red, red, orange, green |
| State | I (label) | `IDLE` / `TIMER` … from `control/1/info` | – |
| Files waiting | B | Files in the node's input queues | – |
| Oldest waiting | C | Age of the oldest input file | orange > 10 min, red > 1h |
| Since last file | E | Time since the collector collected / the distributor distributed its last file | orange > 10 min, red > 1h |
| Delay | J | By-ID collected → distributed delay of the node's last distributed file | orange > 5 min, red > 30 min |
| Last output | D | Time since the node last wrote a file to one of its output queues, seen within 1 day (newest mtime in its output folders) | – |
| Last file | G (label) | Name of the last collected / distributed file | – |
| Disc/rej 24h | F | New files in the node's discarded/rejected folders in 24h | orange ≥ 1 |

### Table: \<stream\>: where data is piling up (one row = one queue holding files)

| Column | Query | Meaning |
|---|---|---|
| Producer | C (label) | Node whose **output** is this queue |
| Queue | label | Queue folder name |
| Consumer | A (label) | Node whose **input** is this queue |
| Files | A | Files in the queue (rows with 0 are hidden) |
| Oldest waiting | B | Age of the oldest file (orange > 10 min, red > 1h) |

### Graphs

| Panel | Shows |
|---|---|
| **backlog per node** | Files in each node's input queues over time. A rising line = that node is not keeping up |
| **files per minute** | Collected vs distributed per minute (trace logs): `rate(directory_node_files_total[5m]) * 60`. The gap is what accumulates |
| **collected → distributed delay** | p50 and p95 of the by-ID delay over 15-minute windows |

---

## 7. Needs attention

### Table: Stuck queues (one row = one stuck input queue)

| Column | Query | Meaning | Colours |
|---|---|---|---|
| Stream, Node, Queue | labels | Node and queue taken from `type` | – |
| Files | A | Files in the queue | – |
| Oldest waiting | B | Age of the oldest file; only queues older than *Stuck after* are listed | orange, red > 1h |
| Last arrival | C | Time since the newest file landed. Small = files still arrive but are not consumed; large = nothing moves at all | – |

### Table: Last file collected (one row = one collector)

| Column | Meaning | Colours |
|---|---|---|
| Stream, Node | – | – |
| Since last file | `time() - directory_collector_last_activity_timestamp_seconds`, from the collector's `storage/1/audit_info` | green < 10 min, orange < 1h, red ≥ 1h |
| Last collected file | File name from audit_info (`directory_node_last_file_info{type="collector"}`) | – |

This replaces monitoring.sh "Collectors Delay" + "Last Collected File".

### Table: Last file distributed (one row = one distributor)

| Column | Meaning | Colours |
|---|---|---|
| Stream, Node | – | – |
| Since last file | `time() - directory_distributor_last_activity_timestamp_seconds`: newest of the last `Distributed file` trace line and the last change of the local TargetDirectory | green < 10 min, orange < 1h, red ≥ 1h |
| Last distributed file | File name from the trace line | – |

These two tables show the time since the last file, not a processing delay. A quiet source shows red here although
nothing is wrong. Check **Waiting at source** to tell the two apart.

### Table: Waiting at source (collector SourceDirectory) (one row = one collector with files waiting)

| Column | Meaning |
|---|---|
| Stream, Collector | – |
| Files | `directory_buffer_files > 0`: files in the collector's SourceDirectory, **not yet collected** |

Reading it together with **Last file collected**: files here **and** a long time since the last collection = the
collector is stuck; nothing here **and** a long time = the source sent nothing.

### Table: Discards & rejects (one row = one discarded/rejected folder holding files)

| Column | Query | Meaning | Colours |
|---|---|---|---|
| Stream, Node | labels | – | – |
| Kind | label | `discarded` or `rejected` | – |
| Folder / reason | label | Subfolder (for rejected: the reason) | – |
| Files | A | Files in it now | – |
| New 24h | B | New files in 24h | orange ≥ 1 |

---

## 8. Flow & node health

| Panel | Type | How it is calculated |
|---|---|---|
| **Collected → distributed delay p95 by stream** | graph | By-ID p95 per stream, 15-minute windows; red line at 30 min |
| **Files per minute: collected vs distributed** | graph | `sum by (event) (rate(directory_node_files_total[5m])) * 60` for all selected streams. A widening gap = files accumulating |

### Table: Statistic per stream (24h) (one row = one stream)

Replaces the monitoring.sh "Statistic" table. All values are from the trace logs.

| Column | Query | Meaning | Colours |
|---|---|---|---|
| Stream | label | – | – |
| Collected | A | `increase(directory_node_files_total{event="collected"}[24h])`: files collected in 24h | – |
| Distributed | B | The same with `event="distributed"` | – |
| Records in | C | `increase(directory_node_records_total{direction="in"}[24h])` | – |
| Records out | D | The same with `direction="out"` | – |
| Delay p95 | E | By-ID p95 over 24h | orange > 5 min, red > 30 min |
| Last trace line | F | Time since the stream's newest trace line | – |

Counters start when the trace exporter starts, so during the first 24h after a restart they cover less than 24h.

### Table: Stopped or failed nodes (24h) (one row = one node)

| Column | Query | Meaning | Colours |
|---|---|---|---|
| Stream, Node | labels | Nodes down now that were up in 24h, or with failures in 24h | – |
| Down for | A | `time() - directory_node_heartbeat_deadline_timestamp_seconds`: time since the heartbeat deadline passed | – |
| Failures 24h | B | Failure trace lines in 24h | red ≥ 1 |
| Last failure | C | Time of the last failure line | – |

---

## 9. Backlog & flow

| Panel | Type | How it is calculated |
|---|---|---|
| **Top 15 nodes by backlog** | bar gauge | `topk(15, sum by (stream,node) (directory_file_count))` over input queues, only nodes with files |
| **Backlog by stream** | graph | Files in input queues per stream over time. A line that only rises = the consumer is not keeping up |

---

## 10. Node activity (all nodes) (collapsed)

### Table: Node activity (one row = one node; every node type, not only collectors/distributors)

| Column | Query | Meaning | Colours |
|---|---|---|---|
| Stream, Node | labels | – | – |
| Files waiting | A | Files in the node's input queues | – |
| Oldest waiting | B | Age of its oldest input file | orange > 10 min, red > 1h |
| Last output | C | Time since it last wrote a file to an output queue, seen within 1 day. Empty = nothing written in 24h, or files were consumed between two scans | – |

---

## 11. Exporter health (collapsed)

| Panel | How it is calculated |
|---|---|
| **Data age** | `time() - directory_last_scan_timestamp_seconds`; red line at 240 s |
| **Slowest directory scans** | `topk(5, directory_scrape_duration_seconds)`: the 5 folders that take longest to read |
| **Watched directories** | `directory_watched_total`. A step = nodes or queues added or removed |
| **Scan errors (per 10m)** | `increase(directory_scan_errors_total[10m])`: folders that could not be read (like monitoring.sh link status NOT_EXIST) |
| **Exporter CPU (cores)** | `rate(process_cpu_seconds_total[5m])` per exporter, with its GOMAXPROCS as a reference line |
| **Exporter memory (RSS)** | `process_resident_memory_bytes` per exporter. The three dashed limit lines (256 / 768 / 128 MB) do not show in this version: their query is invalid PromQL. To be fixed in 1.7.2 |
| **Exporter restarts and file descriptors** | `changes(process_start_time_seconds[1h])` = restarts in the last hour, and `process_open_fds` |

---

## 12. About this dashboard (collapsed)

A short text panel with the colour rules, the terms used, the data sources and the caveats.

One caveat there is out of date. It says "Oldest/newest file times come from at most 5000 files per folder". With
the packaged configuration (`max_stat_files: 0`) every file is checked, so these times are exact.

---

## Terms

| Term | Meaning |
|---|---|
| Live delay | Age of the oldest file still waiting in a node's input queues (collection time from the file ID, else mtime). Keeps rising while files do not move |
| Delay (collected → distributed) | Distribution time minus collection time of a file, from the trace logs. "By ID" = the epoch in the EventLink file ID; "by name" = the time of the `Collected file` line with the same name before the first "." |
| Delayed | Live delay above *Delay threshold*, for nodes running now or in the last 24h |
| Stuck / Not consuming | An input queue whose oldest file is older than *Stuck after* |
| Idle / Since last file | Time since a collector or distributor last handled a file. No traffic is not always a fault |
| Stopped | The heartbeat deadline in `control/1/heartbeat` has passed (+120 s grace) |
| Failed | A failure trace line (`aborted and was disabled`, `Node index … failed`), or status FAILED in the database |
| Backlog | Files in input queues |
