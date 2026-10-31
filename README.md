# Directory Exporter

A Prometheus exporter that monitors filesystem directories and exposes file counts and modification timestamps as metrics. Designed to detect stuck queues, halted ingest pipelines, and inaccessible directories — without modifying the applications that write to those directories.

---

## What Problem It Solves

File-based pipelines — log shippers, stream buffers, ETL queues, media ingest — share the same failure modes:

| Failure | Symptom on disk | Without this exporter |
|---|---|---|
| Consumer stopped | Files pile up, count keeps growing | Silent until disk fills |
| Producer stopped | No new files arriving | Silent until downstream notices |
| Permission error | Directory becomes unreadable | Silent until manual check |
| Stuck file | One very old file never moves | Invisible in standard monitoring |

Application metrics and logs tell you what the app *thinks* is happening. Directory Exporter tells you what is *actually on disk* — a ground-truth signal that requires zero cooperation from the monitored applications.

---

## How It Works

```
┌─────────────────────────────────────────────────────────────────┐
│                        Container                                │
│                                                                 │
│  ┌──────────────┐  scan every      ┌─────────────────────────┐ │
│  │  Scan Loop   │──120s (default)──▶  scanDir() per directory │ │
│  │              │                  │  1. ReadDir (getdents)   │ │
│  │  Discovery   │──every 6h────────▶     → exact file count  │ │
│  │  Loop        │                  │  2. lstat up to N files  │ │
│  └──────┬───────┘                  │     → oldest/newest mtime│ │
│         │ results                  └────────────┬────────────┘ │
│         ▼                                       │              │
│  ┌──────────────┐◀────────────────────────────── ┘             │
│  │  In-Memory   │                                              │
│  │  Cache       │  (metrics served from here — zero disk I/O) │
│  └──────┬───────┘                                              │
│         ▼                                                       │
│  ┌──────────────────────────────────────────────────────────┐  │
│  │  HTTP Server  :9200                                       │  │
│  │  GET  /metrics      →  Prometheus text format            │  │
│  │  GET  /-/healthy    →  liveness probe (always 200)       │  │
│  │  GET  /-/ready      →  readiness probe (503 on cold start)│  │
│  │  POST /-/reload     →  re-discover + re-scan (authed)    │  │
│  └──────────────────────────────────────────────────────────┘  │
│                                                                 │
│  Volume mount (read-only)                                       │
│  /streams  ←──────────── host: /your/actual/path               │
└─────────────────────────────────────────────────────────────────┘
```

**Two-phase scan design:**

1. **Count phase** — reads all directory entries via `getdents` (kernel call, no per-file I/O). File count is always exact and fast.
2. **Stat phase** — calls `lstat` on up to `max_stat_files` entries to get modification times. Capped to keep scans fast on large directories.

Metrics are always served from the in-memory cache. The `/metrics` endpoint never touches disk — response time is sub-millisecond regardless of directory size or file count.

---

## Metrics Reference

All per-directory metrics carry `base`, `stream`, and `type` labels derived from the path:

```
/streams/orders/buffer  →  base="/streams"  stream="orders"  type="buffer"
/streams/logs           →  base="/streams"  stream="logs"    type=""
```

| Metric | Type | Description |
|---|---|---|
| `directory_cache_ready` | Gauge | `1` after first scan completes, `0` during cold start |
| `directory_last_scan_timestamp_seconds` | Gauge | Unix timestamp of the last completed scan cycle |
| `directory_watched_total` | Gauge | Number of directories currently being monitored |
| `directory_scan_errors_total` | Counter | Cumulative scan errors (permission denied, vanished dir, etc.) |
| `directory_reload_total` | Counter | Cumulative `/-/reload` calls received |
| `directory_file_count` | Gauge | Total regular files in the directory — always exact, non-recursive |
| `directory_oldest_file_timestamp_seconds` | Gauge | mtime of the oldest file — detects stuck queues |
| `directory_newest_file_timestamp_seconds` | Gauge | mtime of the newest file — detects halted ingest |
| `directory_scrape_duration_seconds` | Gauge | Wall-clock time of the last scan for this directory |
| `directory_collector_last_activity_timestamp_seconds` | Gauge | Unix timestamp of the last file processed by collector node |
| `directory_collector_delay_seconds` | Gauge | Delay in seconds since last collected file (0 if <= `min_delay`) |
| `directory_distributor_last_activity_timestamp_seconds` | Gauge | Unix timestamp of the last file processed by distributor node |
| `directory_distributor_delay_seconds` | Gauge | Delay in seconds since last distributed file (0 if <= `min_delay`) |
| `directory_node_last_file_info` | Gauge | Value `1` with label `filename` indicating the last processed file |
| `directory_buffer_files` | Gauge | Files waiting in the collector node's `SourceDirectory` buffer dir (non-recursive, per `base`/`stream`/`node`/`type`; mirrors legacy `monitoring.sh` `BUFFER_INDEX`) |


## Components

```
Exporter/
├── main.go           Startup, signal handling, graceful shutdown
├── config.go         Config loading: defaults → YAML file → env vars
├── watchlist.go      Directory discovery (auto and explicit), thread-safe WatchList
├── scanner.go        Low-level dir scan: getdents counting + capped lstat timestamps
├── activity.go       Collector & Distributor delay calculation, audit_info & trace logs
├── exporter.go       Scan loop, discovery loop, parallel worker pool
├── cache.go          Thread-safe in-memory metrics store (atomic counters, RWMutex)
├── metrics.go        Prometheus text format renderer (zero allocations on scrape path)
├── server.go         HTTP handlers: /metrics, /-/reload, /-/healthy, /-/ready
│
├── targets.yml           ← Edit this: what to monitor on this deployment
```


### `targets.yml` — Primary Config File

Mount this file into the container and edit it to control what is monitored. No rebuild required — trigger a reload after editing.


## Building from Source

```bash
# Development build
go build -o directory-exporter .

# Production build (static, stripped)
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -ldflags="-s -w" -trimpath -o directory-exporter .

