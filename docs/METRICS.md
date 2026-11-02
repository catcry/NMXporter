# EventLink exporters: metrics reference (1.7.2)

Every metric the three exporters publish: what it means, where the value comes from, and its labels.
For the Grafana panels built on them, see [DASHBOARD.md](DASHBOARD.md).

| Exporter | Port | Reads | Prometheus job |
|---|---|---|---|
| directory-exporter | 9200 | Stream folders: queues, node control files, collector/distributor files | `directory-exporter` |
| eventlink-trace-exporter | 9202 | EventLink trace logs (`tracelog/execution_trace_*.log`) | `eventlink-trace-exporter` |
| eventlink-db-exporter | 9203 | EventLink PostgreSQL database, read-only | `eventlink-db-exporter` |

Every exporter answers `GET /metrics` (Prometheus format) and `GET /-/ready` (200 once the first scan, poll or seed
has finished). Prometheus adds `job` and `instance` to every series, and `eventlink="<server>"` where the scrape
config sets it.

Conventions:
- `*_timestamp_seconds` = a Unix time (seconds since 1970, UTC). The dashboard shows `time() - value` as "since …"
  or `1000 * value` as a date.
- `*_total` = a counter. It only rises and restarts at 0 when the exporter restarts. Use `rate()` / `increase()`.
- An "info" metric (value always 1) carries text in its labels: a file name, a state, a status.

---

## Common labels

| Label | Exporter | Meaning | Example |
|---|---|---|---|
| `base` | directory, trace | Name of the watched root (`label:` in targets.yml) | `/streams` |
| `stream` | all | EventLink stream: the first folder under `status/streams/`, or the stream name in the trace or DB | `SVC` |
| `type` | directory | Path below the stream: `<node>/<input\|output\|discarded\|rejected>/<queue>`, or `<node>/discarded` for files directly in that folder | `BLN_1/input/OUT_0_2085` |
| `node` | all | EventLink node name | `EL_CFG_FTP_COLLECTOR` |
| `server` | db | Name of the database in `db.yml` | `elink-102` |
| `eventlink` | Prometheus | Name of the EventLink server, set in the scrape config | `elink-102` |

How a stream folder maps to `type`:
```
status/streams/<stream>/nodes/<node>/input/<queue>      ->  type="<node>/input/<queue>"
status/streams/<stream>/nodes/<node>/output/<queue>     ->  type="<node>/output/<queue>"
status/streams/<stream>/nodes/<node>/discarded/<sub>    ->  type="<node>/discarded/<sub>"
status/streams/<stream>/nodes/<node>/discarded          ->  type="<node>/discarded"   (files lying there directly)
```
In EventLink, one queue is the **output** of one node and the **input** of the next. On the reference server both folders are
symbolic links to the same `install/buffer/<n>/<queue>`. Every queue therefore appears twice. To count files once, sum only
`type=~".*/input/.*"`.

---

## 1. directory-exporter (:9200)

Every `scan_interval` (120 s) the exporter lists every watched folder and reads the nodes' control files. Workers
(`scan_workers`, 2) read folders in parallel, and the cycle stops after `scan_timeout` (90 s). `/metrics` always
answers from the result of the last finished cycle, so scraping costs nothing.

### 1.1 Exporter state

| Metric | Type | Meaning / how it is made |
|---|---|---|
| `directory_exporter_build_info{version,revision,goversion}` | gauge (1) | Version of the running binary |
| `directory_cache_ready` | gauge | 1 once the first scan cycle has finished. Before that, `/-/ready` answers 503 |
| `directory_last_scan_timestamp_seconds` | gauge | When the last scan cycle finished. `time() - value` > 240 s = data is stale |
| `directory_watched_total` | gauge | Number of folders on the watch list. Discovery runs every `discovery_interval` (6 h) and on `POST /-/reload`. A step = nodes or queues were added or removed |
| `directory_scan_errors_total` | counter | Folders that could not be read: vanished, permission denied, broken link |
| `directory_reload_total` | counter | `POST /-/reload` calls received (bearer `reload_secret`) |

### 1.2 Per folder (labels `base, stream, type`)

| Metric | Type | Meaning / how it is made |
|---|---|---|
| `directory_file_count` | gauge | Regular files directly in the folder. Not recursive; subfolders are separate series. Exact, because `max_files_per_dir: 0` = count everything |
| `directory_oldest_file_timestamp_seconds` | gauge | Modification time (mtime) of the oldest file. With `max_stat_files: 0` every file is checked, so the value is exact. Absent when the folder is empty |
| `directory_newest_file_timestamp_seconds` | gauge | mtime of the newest file: when the last file landed |
| `directory_queue_oldest_collected_timestamp_seconds` | gauge | **Collection time** of the oldest waiting file, read from the EventLink **file ID in its name**. Example: `1793016004x003_0011282` gives 1793016004, when the collector picked the file up. This is the true collected time; mtime only says when the file landed in this queue. Uses all file names. Absent when no file has an ID (e.g. SVC `file_81382`; the dashboard then uses mtime) |
| `directory_scrape_duration_seconds` | gauge | Time taken to read this one folder in the last cycle. Large = many files or a slow disk |

**Unchanged folders.** When a folder's own inode, size, mtime and ctime are the same as in the last cycle, its file
list did not change. The previous result is reused without listing the folder again, for at most
`unchanged_rescan_interval` (10 min); then it is read in full. The scan cycle log line counts these as
`unchanged_dirs`.

### 1.3 Node status (labels `base, stream, node`)

From the node folder `status/streams/<stream>/nodes/<node>/`:

| Metric | Type | Source | Meaning |
|---|---|---|---|
| `directory_node_info{node_type}` | gauge (1) | `control/1/config`: the `NodeType "..."` line | Lists every node. `node_type` e.g. `collector`, `distributor`, `decoder`; empty when unknown |
| `directory_node_heartbeat_deadline_timestamp_seconds` | gauge | `control/1/heartbeat` | A running node writes a **deadline** here: the time by which it must write the next heartbeat (now + interval). It is not a "last seen" time |
| `directory_node_up` | gauge | from the deadline | 1 = the deadline has not passed by more than `heartbeat_grace` (120 s); 0 = stopped, or no heartbeat file at all |
| `directory_node_state{state}` | gauge (1) | `control/1/info` = `<epoch> <STATE>` | Node state, e.g. `IDLE`, `TIMER` |
| `directory_node_state_since_timestamp_seconds` | gauge | `control/1/info` | When the node entered that state |

Only running nodes have `heartbeat`/`info` files (on the reference server: 70 of 399 nodes).

### 1.4 Collectors and distributors (labels `base, stream, node`)

| Metric | Type | Source | Meaning |
|---|---|---|---|
| `directory_collector_last_activity_timestamp_seconds` | gauge | `storage/1/audit_info` of the collector: `<file name> <epoch> …` | When the collector last collected a file |
| `directory_collector_delay_seconds` | gauge | now − the time above | Time since the last collected file, **0 when it is under `min_delay`** (600 s). This is idle time, not processing delay |
| `directory_distributor_last_activity_timestamp_seconds` | gauge | The newer of: the last `Distributed file` line of this node in the trace logs (looked back `trace_lookback_days`, 7), and the mtime of the distributor's local `TargetDirectory` | When the distributor last sent a file. The TargetDirectory part keeps working while EventLink writes no trace lines |
| `directory_distributor_delay_seconds` | gauge | now − the time above | Same as for collectors: 0 under `min_delay` |
| `directory_distributor_target_dir_mtime_seconds` | gauge | mtime of the `TargetDirectory` from the node config | Changes whenever a file lands there |
| `directory_node_last_file_info{type,filename}` | gauge (1) | audit_info or the trace line | Name of the last collected (`type="collector"`) or distributed (`type="distributor"`) file |
| `directory_buffer_files{type="collector"}` | gauge | Files directly in the collector's `SourceDirectory` | Files waiting at the source that are **not yet collected**. 0 when empty, missing or unreadable |

### 1.5 Process (all three exporters)

| Metric | Meaning |
|---|---|
| `process_cpu_seconds_total` | CPU time used (user + system). `rate(...[5m])` = cores in use |
| `process_resident_memory_bytes` | RAM in use (RSS). The control script stops the exporter above `MAX_RSS_MB` (directory 256, trace 768, db 128); the cron watchdog starts it again |
| `process_virtual_memory_bytes` | Virtual size (reserved address space, not real RAM) |
| `process_threads`, `process_open_fds`, `process_max_fds` | OS threads, open files and the open-file limit |
| `process_start_time_seconds` | Start time. `changes(...[1h])` = restarts |
| `go_goroutines`, `go_memstats_heap_inuse_bytes`, `go_memstats_sys_bytes`, `go_gc_cycles_total` | Go runtime internals |
| `go_gomaxprocs` | Cores the exporter may use at once (GOMAXPROCS: directory 2, trace 2, db 1) |

---

## 2. eventlink-trace-exporter (:9202)

Every `poll_interval` (30 s) it reads the **new bytes** of today's and yesterday's trace files
(`execution_trace_<day>_<n>.log`, rotated `_0` … `_101`, about 10 MB each). It parses each line and keeps counters
in memory. At most `max_read_mb` (1024 MB) is read per poll; the rest follows in the next poll.

What it looks for in a line (example: `20261026 082106.273 L NB [DIS_TAP]  File TAP:CDIR… 1793016004x003_0011282: Distributed file …`):
- the time at the start of the line and the node in `[...]`;
- `Collected file` / `Distributed file`, the size (`, <n> bytes`), and `Read <n> records` / `Wrote <n> records`;
- the EventLink file ID `\d{10}x\d{3}_\d+`: its first 10 digits = the collection epoch;
- failures: `aborted and was disabled` or `Node index … failed`;
- `Scheduled execution … finished`.

Counters start at 0 when the exporter starts (`backfill: false`). Only the **last collected / distributed times**
are seeded at startup from the last `seed_days` (7) of trace logs, at most `seed_max_mb` (1024 MB).

**Who writes the trace logs:** the EventLink **Node Manager**. While it is down, no trace lines are written, even
though the nodes keep running and files keep flowing, so every trace value is frozen (see `Trace logging` on the
dashboard).

### 2.1 Exporter state

| Metric | Type | Meaning |
|---|---|---|
| `eventlink_trace_exporter_build_info{version,revision,goversion}` | gauge (1) | Version |
| `eventlink_trace_ready` | gauge | 1 once the first poll has finished |
| `eventlink_trace_last_poll_timestamp_seconds` | gauge | When the last poll finished |
| `eventlink_trace_poll_duration_seconds` | gauge | How long the last poll took |
| `eventlink_trace_poll_errors_total` | counter | Polls that failed (trace folder unreadable) |
| `eventlink_trace_seed_bytes`, `eventlink_trace_seed_files` | gauge | Bytes and files read by the startup seed |
| `directory_tracelog_read_bytes_total`, `directory_tracelog_lines_total` | counter | Trace bytes and lines read since start |
| `directory_tracelog_files_tracked` | gauge | Trace files (today and yesterday) being followed |
| `directory_tracelog_read_budget_hits_total` | counter | Polls that stopped at `max_read_mb`; the rest is read next poll |
| `directory_tracelog_last_line_timestamp_seconds{base,stream}` | gauge | Time of the newest trace line of the stream. If it stops moving while files still flow, EventLink stopped logging |

### 2.2 Per node, from trace lines (labels `base, stream, node`)

The metric names start with `directory_` because these counters used to live in directory-exporter; the names were
kept so existing queries keep working.

| Metric | Type | Lines counted | Meaning |
|---|---|---|---|
| `directory_node_files_total{event}` | counter | `Collected file` (`event="collected"`), `Distributed file` (`event="distributed"`) | Files handled. `rate(...[5m]) * 60` = files per minute |
| `directory_node_bytes_total{event}` | counter | the `, <n> bytes` of those lines | Bytes handled |
| `directory_node_records_total{direction}` | counter | `Read <n> records` (`in`), `Wrote <n> records` (`out`) | Records read and written |
| `directory_node_failures_total` | counter | `aborted and was disabled`, `Node index … failed` | Node failures. `increase(...[24h]) > 0` = failed in the last day |
| `directory_node_last_failure_timestamp_seconds` | gauge | the same lines | Time of the last failure |
| `directory_node_last_execution_timestamp_seconds{status}` | gauge | `Scheduled execution … finished` | Last scheduled run; `status` = `success` or `other` |
| `eventlink_node_last_collected_timestamp_seconds` | gauge | newest `Collected file` line (plus the seed) | When the node last collected a file |
| `eventlink_node_last_distributed_timestamp_seconds` | gauge | newest `Distributed file` line (plus the seed) | When the node last distributed a file |

### 2.3 Collected → distributed delay (labels `base, stream, node` of the distributor)

For each `Distributed file` line, the exporter works out when that file was collected, in one of two ways:

1. **By ID**: distribution time minus the 10-digit epoch in the file ID.
2. **By name**: distribution time minus the time of the `Collected file` line with the same name before the first
   `.`, because outputs are renamed (`file_x.el.gz` → `file_x.el.encoded` / `.mvne` / `.ict`). If the name was not
   seen (e.g. it was collected before the exporter started), the ID is used instead.

| Metric | Type | Meaning |
|---|---|---|
| `directory_stream_latency_seconds` (`_bucket{le}`, `_sum`, `_count`) | histogram | Delay per distributed file, **by ID**. Buckets (s): 1, 5, 15, 30, 60, 120, 300, 600, 1800, 3600, 7200, 21600, 86400. `histogram_quantile(0.95, sum by (le) (rate(..._bucket[1h])))` = p95 |
| `directory_stream_last_latency_seconds` | gauge | Delay of the node's last distributed file, by ID |
| `eventlink_name_latency_seconds` (`_bucket`, `_sum`, `_count`) | histogram | Delay per distributed file, **by name** (ID fallback). Same buckets |
| `eventlink_name_last_latency_seconds` | gauge | Delay of the node's last distributed file, by name |
| `eventlink_latency_match_total{method}` | counter | Distributed lines by how the collection time was found: `name`, `id` (name not seen), `none` (no ID either) |

Trigger distributors (e.g. SMSC `Dis_KISH_LI`) carry an old trigger ID, so ID matching gives nonsense there. The
by-name metrics handle them.

### 2.4 Pending files (labels `base, stream`)

| Metric | Type | Meaning |
|---|---|---|
| `eventlink_pending_files` | gauge | Files with a `Collected file` line and no `Distributed file` line yet, matched by name. **A hint only**: EventLink does not log files it filters or discards, so these stay "pending". The buffer folders (directory-exporter) are the ground truth |
| `eventlink_oldest_pending_timestamp_seconds` | gauge | Collection time of the oldest pending file. Absent when nothing is pending |
| `eventlink_pending_evicted_total{reason}` | counter | Pending names dropped: `expired` (older than `name_ttl`, 24 h) or `limit` (`max_names`, 500 000, reached) |
| `eventlink_tracked_names` | gauge | Collected names held in memory for matching (the main memory cost) |

---

## 3. eventlink-db-exporter (:9203)

Every `poll_interval` (60 s) it opens **one read-only transaction**: `default_transaction_read_only=on`,
`READ ONLY`, and a `statement_timeout` of 20 s per query. It runs the queries below, each inside a savepoint, so
one failing query does not stop the others. The connection (host, port, database, user, password file) is set in
`db.yml`, by the installer's `--db-*` options. An optional `KEY=VALUE` file of your own (`EL_DB_*`) can supply it
instead; that file is not part of EventLink.

**At startup**, a one-time seed (timeout `seed_timeout`, 120 s) reads the newest event per node and type and the
newest collection/distribution per stream over the last `seed_days`. Every later poll reads only rows added since the
previous poll.

**Who writes the database:** also the **Node Manager**. While it is down, node status, events and reporting rows
are frozen.

### 3.1 Exporter state (label `server`)

| Metric | Type | Meaning |
|---|---|---|
| `eventlink_db_exporter_build_info{version,revision,goversion}` | gauge (1) | Version |
| `eventlink_db_up` | gauge | 1 = every query of the last poll succeeded; 0 = at least one failed or the DB is unreachable |
| `eventlink_db_last_poll_timestamp_seconds`, `eventlink_db_poll_duration_seconds` | gauge | When the last poll finished and how long it took (ms in normal operation; seconds for the first poll with the seed) |
| `eventlink_db_query_errors_total{query}` | counter | Failed queries by name: `timezone`, `names`, `components`, `nodes`, `seed_events`, `seed_reporting`, `events`, `collected`, `distributed` |
| `eventlink_db_read_budget_hits_total` | counter | Polls that stopped at `max_rows_per_poll` (100 000); the rest is read next poll |
| `eventlink_db_system_tz_offset_seconds` | gauge | EventLink's system time zone (`el_events.eventtimesystemtz`, +03:30 = 12600 on the reference server). The `*system` and reporting columns are in this zone; the exporter converts them to UTC |

### 3.2 Node Manager (labels `server, component, host`)

Source: `el_component_status`.

| Metric | Type | Meaning |
|---|---|---|
| `eventlink_nodemanager_up{host}` | gauge | 1 = `status` is `R` **and** `updatetime` is under `nodemanager_grace` (150 s) old. This is EventLink's own rule (view `el_v_host_nm_status`). A Node Manager that hangs stops refreshing the row and counts as down |
| `eventlink_db_component_status{component,host,status}` | gauge (1) | Raw status: `R` running, `S` stopped |
| `eventlink_db_component_update_timestamp_seconds{component,host}` | gauge | Last refresh of that row |

### 3.3 Nodes (labels `server, stream, node`)

Source: `el_nodes` ⋈ `el_node_log` ⋈ `el_stream_log` ⋈ `el_stream_versions` ⋈ `el_streams`, current stream
versions only, deleted streams left out (the same as EventLink's `el_v_legacy_node_status`).

| Metric | Type | Meaning |
|---|---|---|
| `eventlink_db_node_status{host,node_type,status}` | gauge (1) | Status as EventLink keeps it: `RUNNING`, `STOPPED`, `FAILED`, … `node_type` is decoded from `el_node_log.type`, a position code: C collector, D decoder, B business logic, E encoder, D distributor, B BLT, I intermediate, S standalone |
| `eventlink_db_node_last_start_timestamp_seconds` | gauge | `el_node_log.laststartsystem` |

Status here can differ from `directory_node_up` (heartbeat). While the Node Manager is down, this status is frozen
and the heartbeat is the truth.

### 3.4 Events (labels `server, stream, node`)

Source: `el_events`, read incrementally by `eventid`. `node=""` = stream or system events.

| Metric | Type | Meaning |
|---|---|---|
| `eventlink_db_events_total{type}` | counter | Events since the exporter started: `E` error, `W` warning, `I` info |
| `eventlink_db_node_last_event_timestamp_seconds{type}` | gauge | Newest event of the node by type (seeded) |
| `eventlink_db_node_last_failure_timestamp_seconds` | gauge | Newest event whose key is in `failure_keys`: `NODEMANAGER03011` (node aborted and was disabled), `NODEMANAGER03023` |
| `eventlink_db_node_last_error_info{key,message}` | gauge (1) | Key and message of the node's newest error event (message cut to `message_max_len`, 200 characters) |

### 3.5 Reporting per stream (labels `server, stream`)

Sources: `el_reporting_input_details` (one row per collected file, `collectionend`) and
`el_reporting_output_details` (one row per distributed file, `distributionend`). Both columns are indexed.

| Metric | Type | Meaning |
|---|---|---|
| `eventlink_db_stream_last_collected_timestamp_seconds` | gauge | Newest `collectionend` of the stream (seeded over `seed_days`) |
| `eventlink_db_stream_last_distributed_timestamp_seconds` | gauge | Newest `distributionend` |
| `eventlink_db_stream_files_total{event}` | counter | Files collected / distributed since the exporter started |
| `eventlink_db_stream_bytes_total{event}` | counter | Bytes |
| `eventlink_db_stream_records_total{event}` | counter | Records |

The counters read the table in time slices that end `reporting_lag` (5 min) before now, because EventLink writes a
row only after the file is finished. Each row is counted exactly once.

**Performance note:** with `seed_days: 3650` the startup seed reads the whole reporting tables (7M rows on the reference server:
about 4 s). On a much larger database, set `seed_days: 30` so the seed uses the index (0.3 s for 7 days there).

---

## 4. Which source answers which question

| Question | Best metric | Why |
|---|---|---|
| Is a node running? | `directory_node_up` | Heartbeat files are written by the node itself, so they keep working while the Node Manager is down |
| Has a node failed? | `eventlink_db_node_status{status="FAILED"}`, `directory_node_failures_total` | DB status, plus the failure lines in the trace logs |
| Is the Node Manager running? | `eventlink_nodemanager_up` | EventLink's own rule |
| How many files are waiting? | `directory_file_count{type=~".*/input/.*"}` | Folders are the ground truth |
| How long has the oldest file been waiting (live delay)? | `directory_queue_oldest_collected_timestamp_seconds`, else `directory_oldest_file_timestamp_seconds` | Collection time from the file ID; mtime when names carry no ID |
| How long from collection to distribution? | `eventlink_name_latency_seconds`, `directory_stream_latency_seconds` | Trace logs; needs the Node Manager running |
| When was the last file collected / distributed? | `directory_collector_last_activity_…`, `eventlink_node_last_collected_…`, `eventlink_db_stream_last_collected_…` (and `distributed`) | Three independent sources; the dashboard takes the newest |
