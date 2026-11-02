# EventLink exporters — Installation and Operations Manual

Version 1.7.2 · directory-exporter, eventlink-trace-exporter, eventlink-db-exporter

This manual is for the engineer who installs and runs the exporters on an
EventLink server, and for whoever connects them to Prometheus and Grafana.
It assumes shell access to the EventLink server as the EventLink OS user, and
admin access to Prometheus and Grafana.

---

## Contents

1. [What the package does](#1-what-the-package-does)
2. [Requirements](#2-requirements)
3. [Package contents](#3-package-contents)
4. [Before you install](#4-before-you-install)
5. [Installation](#5-installation)
6. [Prometheus](#6-prometheus)
7. [Grafana](#7-grafana)
8. [Day-to-day operation](#8-day-to-day-operation)
9. [Configuration reference](#9-configuration-reference)
10. [CPU, memory and disk: limits and measured use](#10-cpu-memory-and-disk-limits-and-measured-use)
11. [Security](#11-security)
12. [Troubleshooting](#12-troubleshooting)
13. [Metric reference](#13-metric-reference)
14. [Appendix: where the data comes from](#14-appendix-where-the-data-comes-from)

---

## 1. What the package does

Three small, independent programs read an EventLink installation and publish
what they find as Prometheus metrics. Grafana shows them on one dashboard;
Prometheus raises alerts. Each exporter has one job, so a problem in one of
them (a slow disk, huge trace logs, a database outage) never stops the others.

| Exporter | Default port | Reads | Answers questions like |
|---|---|---|---|
| **directory-exporter** | 9200 | The stream folders: every queue (`input`, `output`, `discarded`, `rejected`), node `control/1/heartbeat`, `control/1/info`, `control/1/config`, collector `audit_info`, distributor TargetDirectory | How many files wait where? How old is the oldest waiting file (live delay)? Which nodes run? Which queue is stuck? |
| **eventlink-trace-exporter** | 9202 | The execution trace logs (`tracelog/execution_trace_<stream>_<date>_<n>.log`) | How many files and records did each node handle? How long from collection to distribution? Which node failed? |
| **eventlink-db-exporter** | 9203 | EventLink's PostgreSQL database, **read-only** | Is the Node Manager running? Which nodes are FAILED according to EventLink, and why? When did each stream last collect and distribute? |

```
 EventLink server                                         Monitoring server
 ┌──────────────────────────────────────────────┐         ┌──────────────────────┐
 │ status/streams/…   ──► directory-exporter :9200 ──┐    │                      │
 │ tracelog/…         ──► eventlink-trace-exp. :9202 ─┼──► │ Prometheus ─► Grafana│
 │ PostgreSQL (r/o)   ──► eventlink-db-exp.    :9203 ─┘    │      └─► alerts       │
 └──────────────────────────────────────────────┘         └──────────────────────┘
```

All three:

- run as the EventLink OS user, need no root and install no agent;
- only **read** EventLink files and the database; they write only their own
  log file (rotated by themselves);
- answer `/metrics` from memory in a few milliseconds, whatever the size of
  the folders (the work is done in the background);
- limit their own CPU and memory (section 10) and run at low CPU and disk
  priority, so EventLink always goes first.

You can install all three or only some (`--only`). Without the database
exporter you lose the Node Manager status, EventLink's own node status and
the event messages; everything else works.

---

## 2. Requirements

**EventLink server**

| | |
|---|---|
| OS | Linux x86_64. The binaries are **ready-built and statically linked**: no Go, no compiler, no libraries and no internet are needed on the server (tested on CentOS 7) |
| User | The EventLink OS user (it can read the stream folders and trace logs) |
| Tools | only what every Linux has: `bash`, `sed`, `awk`, `grep`, `tar`; `crontab` for the watchdog (or root once for systemd). `curl` or `wget` are used if present (else bash's `/dev/tcp`); `ionice` if present |
| Disk | ~30 MB for binaries and logs |
| Memory | normally 10–40 MB in total; see section 10 for the limits |
| Network | Prometheus must reach the exporter ports (default 9200, 9202, 9203) |
| Database | PostgreSQL only, for eventlink-db-exporter: a database account (best a read-only one, section 4.2) and its password in a file. The other two exporters do not use the database; leave the DB exporter out with `--only directory,trace` |

**Monitoring server**

- Prometheus 2.x or 3.x (tested with 3.15).
- Grafana 9 or newer (tested with 13.3).
- Optional: Alertmanager, to send the alerts somewhere.

---

## 3. Package contents

```
eventlink-exporters-1.7.2-linux-amd64/
├── README.txt                 short overview
├── MANUAL.md                  this manual
├── METRICS.md                 every metric of the three exporters
├── DASHBOARD.md               every panel and table column of the dashboard
├── VERSION, REVISION          version and source revision
├── SHA256SUMS                 checksums of every file
├── install.sh                 installer / upgrader (section 5)
├── uninstall.sh               stop, remove cron lines, optionally delete
├── bin/
│   ├── directory-exporter
│   ├── eventlink-trace-exporter
│   ├── eventlink-db-exporter
│   └── exporterctl.sh         control script (installed once per exporter)
├── config/                    templates the installer fills in
│   ├── targets.yml.in         directory-exporter
│   ├── trace.yml.in           eventlink-trace-exporter
│   └── db.yml.in              eventlink-db-exporter
├── systemd/exporter.service.in
├── prometheus/
│   ├── scrape.yml.in          scrape jobs (the installer writes the filled-in copy)
│   └── alerts.yml             alert rules
├── grafana/
│   └── eventlink-overview.json  dashboard
└── tools/stress/              the stress-test scripts used for section 10
```

Check the package after copying it to the server:

```sh
tar xzf eventlink-exporters-1.7.2-linux-amd64.tar.gz
cd eventlink-exporters-1.7.2-linux-amd64
sha256sum -c SHA256SUMS
```

---

## 4. Before you install

### 4.1 Checklist

| # | Decide / find out | Default | Installer option |
|---|---|---|---|
| 1 | The EventLink OS user to install as | — (log in as it) | — |
| 2 | Install directory (must not be inside a folder EventLink cleans up) | `~/eventlink-exporters` | `--prefix` |
| 3 | EventLink base directory (contains `status/streams` and `tracelog`) | `$EVENTLINK_HOME/base`, else `/comptel/elink/install/EventLink/base` | `--eventlink-base` |
| 4 | Name of this server in Grafana / alerts | short host name | `--name` |
| 5 | Address Prometheus uses to reach this server | first IP of the host | `--host` |
| 6 | Free ports | 9200, 9202, 9203 | `--ports` |
| 7 | Database account for eventlink-db-exporter: user, password file, host, port, database name | none for the account; `127.0.0.1`, `5432`, `eventlink` | `--db-user`, `--db-password-file`, `--db-host`, `--db-port`, `--db-name` (or `--parameters`, see below) |
| 8 | Firewall: allow only the Prometheus server to the three ports | — | (OS firewall, root) |

`./install.sh --check` checks all of this and changes nothing.

### 4.2 A read-only database account (recommended)

The installer does not look for EventLink's own database login, so you give
the exporter an account with `--db-user` and `--db-password-file`.
Do not reuse EventLink's own account: it often has full rights (on the
reference server it is a superuser). The exporter never writes: every session
is opened with `default_transaction_read_only=on` and a statement timeout,
and every poll runs in a `READ ONLY` transaction. Still, give it its own
account with SELECT rights only, so a mistake can never change EventLink data.
Ask the DBA to run, as a database administrator:

```sql
CREATE ROLE monitor LOGIN PASSWORD 'choose-a-password';
GRANT CONNECT ON DATABASE eventlink TO monitor;
GRANT USAGE ON SCHEMA public TO monitor;
GRANT SELECT ON el_component_status, el_node_log, el_nodes, el_streams,
                el_stream_log, el_stream_versions, el_events,
                el_reporting_input_details, el_reporting_output_details TO monitor;
```

If the exporter runs on another host than the database, `pg_hba.conf` must
allow that host for this user. Then install with:

```sh
umask 077; printf '%s\n' 'choose-a-password' > ~/monitor.password
./install.sh --db-user monitor --db-password-file ~/monitor.password …
rm ~/monitor.password      # the installer copied it (mode 600)
```

---

## 5. Installation

### 5.0 Isolated servers (no internet)

Nothing is downloaded, compiled or installed from outside: the package holds
the finished programs. Go is only needed on the build machine that makes the
package (`scripts/build.sh`), never on an EventLink server.

1. On a machine with the package, note its checksum (from the `.sha256` file
   next to it, or `sha256sum eventlink-exporters-1.7.2-linux-amd64.tar.gz`).
2. Copy the one file `eventlink-exporters-1.7.2-linux-amd64.tar.gz` to the
   EventLink server by whatever is allowed there (scp through a jump host,
   SFTP, USB / approved transfer medium).
3. On the server, as the EventLink user, compare the checksum, then continue
   with 5.1:

```sh
sha256sum eventlink-exporters-1.7.2-linux-amd64.tar.gz     # must equal the noted value
```

The Grafana dashboard and the alert rules are plain files too; they need no
plugin or download on the monitoring server.

### 5.1 Quick installation

As the EventLink user, on the EventLink server:

```sh
tar xzf eventlink-exporters-1.7.2-linux-amd64.tar.gz
cd eventlink-exporters-1.7.2-linux-amd64
umask 077; printf '%s\n' 'the-db-password' > ~/monitor.password
./install.sh --check --name ccelink-prod --db-user monitor --db-password-file ~/monitor.password
./install.sh --name ccelink-prod --db-user monitor --db-password-file ~/monitor.password --start --cron
rm ~/monitor.password      # the installer copied it (mode 600)
```

Add `--db-host`, `--db-port`, `--db-name` when the database is not
`127.0.0.1:5432/eventlink`. Add `--streams-dir` and `--tracelog-dir` (or
`--eventlink-base`) when `--check` cannot find them; on a different layout it
reports FAIL for each path it cannot find and installs nothing. Without a
database account, install only the other two: `--only directory,trace`.

Expected output of the check (values differ per server):

```
EventLink exporters 1.7.2 — checking ccelink as elink
  [ ok ] Linux x86_64
  [ ok ] curl found
  [ ok ] ionice found
  [ ok ] EventLink base: /comptel/elink/install/EventLink/base
  [ ok ] streams: /comptel/elink/install/EventLink/base/status/streams (30 streams)
  [ ok ] trace logs: /comptel/elink/install/EventLink/base/tracelog (290 files)
  [ ok ] database user: monitor
  [ ok ] password file: /home/elink/monitor.pw
  [ ok ] database 127.0.0.1:5432 answers (database eventlink)
  [ ok ] port 9200 for directory-exporter
  [ ok ] port 9202 for eventlink-trace-exporter
  [ ok ] port 9203 for eventlink-db-exporter
  [ ok ] install directory: /home/elink/eventlink-exporters
  [ ok ] free disk at …: 13000 MB
All checks passed (--check: nothing changed).
```

Then continue with Prometheus (section 6) and Grafana (section 7).

### 5.2 Installer options

| Option | Meaning |
|---|---|
| `--prefix DIR` | Install directory (default `~/eventlink-exporters`) |
| `--eventlink-base DIR` | EventLink base directory; `status/streams` and `tracelog` are taken from it |
| `--streams-dir DIR`, `--tracelog-dir DIR` | Set these two directly (non-standard layouts) |
| `--parameters FILE` | Optional: a `KEY=VALUE` file of your own with `EL_DB_HOST`, `EL_DB_PORT`, `EL_DB_NAME`, `EL_DB_USER`, `EL_DB_PASSWORD`. This is **not** an EventLink file. It is used automatically only if one exists at `<base>/../../meta/EVENTLINK_DB/parameters.txt` |
| `--name NAME` | Name of this EventLink server: the `eventlink` label in Prometheus and the `server` label of the DB exporter |
| `--host ADDR` | Address written into the generated Prometheus scrape config |
| `--ports D,T,B` | Ports of directory, trace and db exporter (default `9200,9202,9203`) |
| `--only LIST` | Install only some: `directory`, `trace`, `db` (comma separated) |
| `--db-user USER`, `--db-password-file FILE` | Database account for the DB exporter. The file holds only the password; it is copied to `db.password` (mode 600) |
| `--db-host H`, `--db-port P`, `--db-name N` | The EventLink database (default `127.0.0.1`, `5432`, `eventlink`). `--check` tests that the port answers |
| `--start` | Start the exporters after installing |
| `--cron` | Add the watchdog to the user's crontab (`@reboot` and every 5 minutes) |
| `--force` | Overwrite existing config files (otherwise the new template is written as `*.new`) |
| `--check` | Only run the checks |

### 5.3 What the installer does

1. Runs the checks of section 5.1 and stops if one fails.
2. For each exporter, creates `<prefix>/<exporter>/` with the binary, the
   control script `<exporter>ctl.sh`, the config file (from `config/*.in`,
   with the paths, ports, server name and a random reload secret filled in,
   mode 600) and a `VERSION` file.
3. Writes `<prefix>/prometheus-scrape.yml` (the scrape jobs for this server)
   and `<prefix>/systemd/<exporter>.service` (for root, optional).
4. With `--start`, starts the exporters; with `--cron`, adds the watchdog.

Result:

```
~/eventlink-exporters/
├── directory-exporter/        directory-exporter, directory-exporterctl.sh, targets.yml, exporter.log
├── eventlink-trace-exporter/  eventlink-trace-exporter, …ctl.sh, trace.yml, trace-exporter.log
├── eventlink-db-exporter/     eventlink-db-exporter, …ctl.sh, db.yml, [db.password], db-exporter.log
├── prometheus-scrape.yml
├── systemd/*.service
└── MANUAL.md
```

### 5.4 Verify

Right after the start each exporter reads once; `ready=200` means it is done
(directory-exporter: first full scan; trace: first poll; db: first query).

```sh
cd ~/eventlink-exporters
directory-exporter/directory-exporterctl.sh status
#   running (pid 1234) port 9200 ready=200 version=1.7.2
eventlink-trace-exporter/eventlink-trace-exporterctl.sh status
eventlink-db-exporter/eventlink-db-exporterctl.sh status

curl -s localhost:9200/metrics | grep -E '^directory_(watched_total|cache_ready|scan_errors_total) '
curl -s localhost:9202/metrics | grep -E '^eventlink_trace_(ready|poll_errors_total) '
curl -s localhost:9203/metrics | grep -E '^eventlink_(db_up|nodemanager_up)'
grep -c '"level":"ERROR"' */*.log          # expect 0 for each
```

`directory_scan_errors_total` above 0 usually means folders the user cannot
read (section 12).

### 5.5 Watchdog: cron or systemd

**cron (no root):** `./install.sh --cron` adds, for each exporter,

```
@reboot      <prefix>/<exporter>/<exporter>ctl.sh start >/dev/null 2>&1 # eventlink-exporters <prefix>
*/5 * * * *  <prefix>/<exporter>/<exporter>ctl.sh start >/dev/null 2>&1 # eventlink-exporters <prefix>
```

`start` does nothing while the exporter runs, so a stopped or crashed
exporter is back within 5 minutes and after a reboot.

**systemd (root, instead of cron):** the installer wrote one unit per exporter
into `<prefix>/systemd/`, with the same CPU and memory limits enforced by the
kernel (`MemoryLimit`, `CPUQuota`, `Nice`, `IOSchedulingPriority`):

```sh
# as root; remove the cron lines first (crontab -e as the EventLink user)
cp ~elink/eventlink-exporters/systemd/*.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now directory-exporter eventlink-trace-exporter eventlink-db-exporter
```

Use one of the two, not both.

---

## 6. Prometheus

### 6.1 Scrape jobs

Append `<prefix>/prometheus-scrape.yml` (written by the installer) under
`scrape_configs:` in `prometheus.yml`, then reload Prometheus
(`curl -X POST http://localhost:9090/-/reload` with `--web.enable-lifecycle`,
or restart it). It looks like this:

```yaml
  - job_name: directory-exporter
    scrape_interval: 30s
    scrape_timeout: 10s
    static_configs:
      - targets: [ "10.0.0.11:9200" ]
        labels:
          eventlink: "ccelink-prod"

  - job_name: eventlink-trace-exporter
    scrape_interval: 30s
    scrape_timeout: 10s
    static_configs:
      - targets: [ "10.0.0.11:9202" ]
        labels:
          eventlink: "ccelink-prod"

  - job_name: eventlink-db-exporter
    scrape_interval: 60s
    scrape_timeout: 10s
    static_configs:
      - targets: [ "10.0.0.11:9203" ]
        labels:
          eventlink: "ccelink-prod"
```

Two rules:

- **Keep the job names.** The dashboard and the alerts use them.
- **Give every EventLink server its own `eventlink` value**, the same on its
  three jobs. The dashboard's "EventLink server" selector and every alert use
  it; without it, two servers in one Prometheus would be added together.

### 6.2 Several EventLink servers

Install the package on each server (with its own `--name`), then add each
server's targets to the same three jobs:

```yaml
  - job_name: directory-exporter
    static_configs:
      - targets: [ "10.0.0.11:9200" ]
        labels: { eventlink: "ccelink-prod" }
      - targets: [ "10.0.0.12:9200" ]
        labels: { eventlink: "ccelink-dr" }
```

(and the same for `eventlink-trace-exporter` and `eventlink-db-exporter`).

One eventlink-db-exporter can also read several databases (several entries
under `databases:` in `db.yml`). It then exports all of them with one
`eventlink` label; prefer one DB exporter per server so the labels match.

Check in Prometheus → Status → Targets: all targets UP.

### 6.3 Alerts

`prometheus/alerts.yml` holds the alert rules. Copy it to the Prometheus
rules folder, reference it and reload:

```yaml
rule_files:
  - /etc/prometheus/rules/eventlink-alerts.yml
```

`promtool check rules alerts.yml` checks the file. Without Alertmanager the
alerts are visible in Prometheus (Alerts page) and in Grafana, but nobody is
notified.

| Alert | Severity | Fires when |
|---|---|---|
| DirectoryExporterDown | critical | directory-exporter cannot be scraped for 5 min |
| DirectoryExporterStale | warning | no finished folder scan for 10 min |
| TraceExporterDown | warning | trace exporter cannot be scraped for 5 min |
| DBExporterDown | warning | DB exporter cannot be scraped, or cannot read the database, for 5 min |
| ExporterMemoryHigh | warning | an exporter uses over 80 % of its hard memory limit for 10 min |
| ExporterRestarting | warning | an exporter restarted more than twice in an hour |
| EventLinkNodeManagerDown | critical | EventLink's Node Manager is not running (EventLink's own rule) for 5 min |
| EventLinkNodeFailedDB | critical | EventLink recorded a node failure ("aborted and was disabled") |
| EventLinkNodeStopped | critical | a node that ran in the last 24 h has no heartbeat for 5 min |
| EventLinkNodeFailed | critical | the trace log reports a node failure |
| EventLinkQueueStuck | warning | files wait over 1 h in the input of a running node |
| EventLinkCollectorIdle | warning | a running collector collected nothing for over 1 h |
| EventLinkLiveDelayHigh | warning | the oldest waiting input file is over 30 min old |
| EventLinkStreamDelayHigh | warning | collected → distributed p95 over 30 min |
| EventLinkTraceLoggingStopped | warning | no trace log line on the server for 10 min (Node Manager probably down) |
| EventLinkDiscardsRising | warning | more than 100 files discarded in a stream in an hour |

Thresholds are in the `expr` lines; adjust them to the server.

---

## 7. Grafana

### 7.1 Import

1. Grafana → Dashboards → New → Import → upload `grafana/eventlink-overview.json`.
2. Choose the Prometheus data source that scrapes the exporters.
3. Import. (Importing again with "Overwrite" updates an existing copy; the
   dashboard uid is `eventlink-dir-overview`.)

### 7.2 Selectors at the top

| Selector | Meaning |
|---|---|
| Data source | the Prometheus to read from |
| EventLink server | the `eventlink` label (section 6.1); "All" adds servers together |
| Stream | one, several or all streams |
| Stuck after | age above which a queue counts as stuck |
| Delay threshold | live delay above which a stream / node counts as delayed |
| Investigate | the stream shown in the "Investigate" rows (defaults to the worst one) |

### 7.3 Sections

| Section | Shows | Source |
|---|---|---|
| Top tiles | exporter status, data age, running / stopped / failed nodes, delay p95, backlog, stuck queues, idle collectors and distributors, discards and rejects | all |
| Delay — live | streams and nodes delayed now; live delay = age of the oldest waiting input file | directory |
| Last collected / distributed | per stream and node, with "Trace logging" (Node Manager writing?) | directory, trace |
| EventLink database | Node Manager up / down, failed nodes with the error message, node status per stream | db |
| Top 3 streams with issues, Investigate | the worst streams and one stream in detail | all |
| Needs attention | stuck queues, idle collectors / distributors, files waiting at the source, discards | directory |
| Flow & node health | delay p95, files per minute, statistics per stream, stopped / failed nodes | trace, directory |
| Backlog & flow, Node activity | backlog per node and stream over time | directory |
| Exporter health | scan duration, data age, errors, and each exporter's CPU, memory and restarts | all |
| About | definitions of every term used | — |

Hover over a panel title (ⓘ) for its exact definition.

---

## 8. Day-to-day operation

### 8.1 Control script

Each exporter has its own copy, named after it:

```sh
cd ~/eventlink-exporters/directory-exporter
./directory-exporterctl.sh start      # does nothing if already running
./directory-exporterctl.sh stop
./directory-exporterctl.sh restart
./directory-exporterctl.sh status     # pid, port, ready, version
```

It finds the running exporter by its working directory, so it never touches
another copy (for example a test copy in another folder) and also finds one
started by hand. It starts the exporter with `nice 10`, `ionice` best-effort
7 and the limits of section 10.

### 8.2 Logs

Each exporter writes JSON lines to its own log and rotates it itself
(`log_max_size_mb`, `log_keep`; default 10 MB × 3):

| Exporter | Log | Crash output |
|---|---|---|
| directory-exporter | `exporter.log` | `directory-exporter.out` |
| eventlink-trace-exporter | `trace-exporter.log` | `eventlink-trace-exporter.out` |
| eventlink-db-exporter | `db-exporter.log` | `eventlink-db-exporter.out` |

```sh
grep '"level":"\(WARN\|ERROR\)"' */*.log | tail
```

### 8.3 Changing the configuration

Edit the YAML file and restart the exporter (`…ctl.sh restart`).
directory-exporter can also re-discover folders and rescan without a restart:

```sh
curl -X POST -H "Authorization: Bearer $(sed -n 's/^reload_secret: *"\(.*\)"/\1/p' targets.yml)" localhost:9200/-/reload
```

### 8.4 Upgrade

Unpack the new package and run its installer with the same options as the
first time:

```sh
./install.sh --name ccelink-prod --db-user monitor --db-password-file ~/monitor.password
```

For each installed exporter it stops it (if running), keeps the old binary as
`<exporter>.prev`, copies the new one, starts it again, and **keeps your
config files** (the new template is written next to them as `*.new`; compare
with `diff targets.yml targets.yml.new`). Re-import the dashboard and copy
the new `alerts.yml` if the release notes say they changed.

### 8.5 Rollback

```sh
cd ~/eventlink-exporters/directory-exporter
./directory-exporterctl.sh stop
mv directory-exporter.prev directory-exporter
./directory-exporterctl.sh start
```

(the same for the other two). Configuration files are not changed by an
upgrade, so they need no rollback.

### 8.6 Uninstall

```sh
./uninstall.sh                 # stop all, remove the cron lines, keep the files
./uninstall.sh --purge         # also delete ~/eventlink-exporters
```

Then remove the scrape jobs from Prometheus and, if installed, the systemd
units (`systemctl disable --now …; rm /etc/systemd/system/<exporter>.service`).

---

## 9. Configuration reference

All durations are Go durations (`30s`, `10m`, `6h`). Values not in the file
use the default. The installer's templates already contain recommended values
with comments.

### 9.1 directory-exporter (`targets.yml`)

| Key | Default | Meaning |
|---|---|---|
| `listen_addr` | `:9200` | HTTP address |
| `scan_interval` | `2m` | how often every watched folder is read |
| `scan_workers` | `2` | folders read in parallel |
| `scan_timeout` | `90s` | a cycle that takes longer publishes partial results |
| `discovery_interval` | `6h` | how often new streams / nodes / queues are found |
| `max_stat_files` | `5000` (template: `0`) | files per folder whose time is read; `0` = all (exact). A sample gives wrong oldest / newest times on big folders |
| `unchanged_rescan_interval` | `10m` | a folder whose file list did not change is not read again for this long (its last result is reused); `0` = always read |
| `max_files_per_dir` | `0` | stop counting a folder after this many files; `0` = count all |
| `reload_secret` | random per start | Bearer token for `POST /-/reload` |
| `targets` | — | folders to watch (below) |
| `tracelog_dir` | — | trace logs, for distributors' last distributed file |
| `trace_lookback_days` | `7` | days of trace logs searched for that |
| `min_delay` | `600s` | collector / distributor idle delay is 0 below this |
| `heartbeat_grace` | `120s` | a node is down when its heartbeat deadline is older than this |
| `log_file` | stdout | log file (relative to the exporter's folder) |
| `log_max_size_mb`, `log_keep` | `10`, `3` | log rotation |

`targets` entries:

| Key | Meaning |
|---|---|
| `base` | folder to start from (the EventLink `status/streams`) |
| `label` | value of the `base` label (`/streams`) |
| `pattern` | folders to watch, relative to `base`; `*` = one path element, `#include` = each value of `include` |
| `include` | values for `#include` |
| `dirs` | instead of `pattern`: an explicit list of folders |
| `max_depth` | depth for auto-discovery without pattern |

The template watches every queue of every node
(`*/nodes/*/#include/*` with `input, output, discarded, rejected`) and the
files directly in `discarded/` and `rejected/` (`*/nodes/*/#include`).

Environment variables override the file: `CONFIG_FILE`, `LISTEN_ADDR`,
`SCAN_INTERVAL`, `SCAN_WORKERS`, `SCAN_TIMEOUT`, `DISCOVERY_INTERVAL`,
`MAX_FILES_PER_DIR`, `MAX_STAT_FILES`, `TRACELOG_DIR`, `MIN_DELAY`,
`TRACE_LOOKBACK_DAYS`, `HEARTBEAT_GRACE`, `RELOAD_SECRET`.

### 9.2 eventlink-trace-exporter (`trace.yml`)

| Key | Default | Meaning |
|---|---|---|
| `listen_addr` | `:9202` | HTTP address |
| `tracelog_dir` | — (required) | trace log folder |
| `base_label` | `/streams` | value of the `base` label (same as directory-exporter's) |
| `poll_interval` | `30s` | how often new trace lines are read |
| `backfill` | `false` | `true` = read existing trace files from the start at startup (counters then include today's history; costs time and memory once) |
| `max_read_mb` | `1024` | trace bytes read per poll at most; the rest next poll |
| `seed_days`, `seed_max_mb` | `7`, `1024` | at startup, read the last days of trace once, only for each node's last collected / distributed time |
| `name_ttl` | `24h` | how long a collected file name is kept to match its distribution |
| `max_names` | `500000` | names kept at most (memory: ~190 MB at 500000; lower it on small servers) |
| `log_file`, `log_max_size_mb`, `log_keep` | stdout, `10`, `3` | log |

Environment: `CONFIG_FILE`, `LISTEN_ADDR`, `TRACELOG_DIR`.

### 9.3 eventlink-db-exporter (`db.yml`)

| Key | Default | Meaning |
|---|---|---|
| `listen_addr` | `:9203` | HTTP address |
| `poll_interval` | `60s` | how often each database is read |
| `query_timeout` | `20s` | statement timeout of the regular queries |
| `seed_days` | `7` (template: `3650`) | at startup, newest event / collection / distribution per node and stream of these days |
| `seed_timeout` | `120s` | statement timeout of those startup queries |
| `reporting_lag` | `5m` | collected / distributed counters stay this far behind now, so late rows are counted |
| `max_rows_per_poll` | `100000` | events read per poll at most |
| `nodemanager_grace` | `150s` | Node Manager is up while its status row is younger than this |
| `failure_keys` | `NODEMANAGER03011`, `NODEMANAGER03023` | event keys counted as node failures |
| `message_max_len` | `200` | longest event message kept |
| `databases` | — (required) | one entry per EventLink database (below) |
| `log_file`, `log_max_size_mb`, `log_keep` | stdout, `10`, `3` | log |

`databases` entries:

| Key | Default | Meaning |
|---|---|---|
| `name` | host | value of the `server` label |
| `eventlink_parameters` | — | optional `KEY=VALUE` file of your own (not an EventLink file); supplies every value not set here |
| `parameters_prefix` | `EL` | prefix in that file (`EL_DB_HOST`, `EL_DB_PORT`, `EL_DB_NAME`, `EL_DB_USER`, `EL_DB_PASSWORD`) |
| `host`, `port`, `dbname`, `user` | `127.0.0.1`, `5432`, `eventlink`, — | connection |
| `password` / `password_env` / `password_file` | — | password, first one set wins, then the parameters file |
| `sslmode` | `disable` | `disable`, `require`, `verify-full`, … |
| `system_tz_offset` | `auto` | zone of EventLink's "system" time columns: `auto` reads it from the events table, or e.g. `+03:30` |

Environment: `CONFIG_FILE` (required), `LISTEN_ADDR`, and the variables named
by `password_env`. The password is never logged or exported.

### 9.4 Limits (control script)

The control script starts each exporter with these environment variables;
change them in `<exporter>.env` next to the script, for example
`directory-exporter/directory-exporter.env`:

```sh
GOMAXPROCS=4          # cores the exporter may use at once
GOMEMLIMIT=256MiB     # soft memory limit: the Go garbage collector works harder above it
MAX_RSS_MB=512        # hard limit: above it the exporter stops; the watchdog restarts it
NICE=10               # CPU priority (19 = lowest)
```

| Exporter | GOMAXPROCS | GOMEMLIMIT | MAX_RSS_MB |
|---|---|---|---|
| directory-exporter | 2 | 128MiB | 256 |
| eventlink-trace-exporter | 2 | 384MiB | 768 |
| eventlink-db-exporter | 1 | 64MiB | 128 |

---

## 10. CPU, memory and disk: limits and measured use

### 10.1 How the exporters protect the server

| Protection | How |
|---|---|
| CPU | `GOMAXPROCS` caps the cores; `nice 10` gives EventLink the CPU first; folder scans use `scan_workers` (2) workers |
| Disk I/O | `ionice` best-effort 7 (lowest of the normal class); unchanged folders are not re-read; trace logs are read only once (only new bytes), at most `max_read_mb` per poll |
| Memory | folders are streamed (memory does not grow with the number of files); trace file names are capped by `max_names`; soft limit `GOMEMLIMIT`; hard limit `MAX_RSS_MB` (the exporter stops itself; tested) |
| Database | read-only sessions and transactions; statement timeouts; each poll reads only new rows through indexes and lets the database do the sums; one connection per database |
| Prometheus | `/metrics` is answered from memory; it never touches disk or database |
| Systemd (optional) | `MemoryLimit`, `CPUQuota`, `Nice`, `IOSchedulingClass` enforced by the kernel |

### 10.2 Measured on the reference server

16 CPUs, 32 GB RAM, XFS, PostgreSQL 9.2; the scripts are in `tools/stress/`.

| Test | Result | CPU | Memory |
|---|---|---|---|
| Folder with **5,000,000 files**, `max_stat_files: 5000` (sample) | count exact; **newest time wrong by 8 min 12 s** | scan 1.2 s | 10 MB |
| Folder with **1,000,000 files**, sample 5000 | count exact; **newest time wrong by 34 min 51 s** | scan 0.3 s | 10 MB |
| 5M files, `max_stat_files: 0` | count, oldest and newest exact | scan 12 s every cycle | 10.6 MB |
| **5M files, `max_stat_files: 0` + `unchanged_rescan_interval: 10m`** (template) | **exact**; first scan 12 s, then 0 s per cycle while the folder does not change | ~2 % of one core | 9.6 MB |
| Trace logs: **1 GB / 6.7 million lines at once** | all read in 10 s (36 s with `max_read_mb: 256`); `/metrics` 5 ms meanwhile | peak 1.2 cores | 176 MB (500,000 names) |
| DB: startup over **all history** (13 million reporting rows) | 4.7 s once; every later poll ~8 ms | ~0 | 6.6 MB |
| DB: query slower than its timeout | PostgreSQL cancels it; `eventlink_db_up 0`, error counted, other queries go on | ~0 | 6.6 MB |
| DB not reachable | `eventlink_db_up 0`, retried every poll, no crash | ~0 | 3.9 MB |
| Hard memory limit set to 100 MB under the 1 GB trace burst | exporter logged "resident memory above MAX_RSS_MB" and stopped after 20 s | — | 169 MB at stop |

In normal operation the three exporters together use about 30–40 MB of
memory and well under 5 % of one core. The dashboard's "Exporter health" row
and the `ExporterMemoryHigh` / `ExporterRestarting` alerts show it live.

### 10.3 Sizing guidance

- **Very large folders that change all the time** (millions of files): each
  cycle reads every file's time. Raise `scan_interval` (e.g. `300s`) or, if
  you accept approximate times, set `max_stat_files` back to a sample
  (counts stay exact; times may be wrong).
- **Busy trace logs**: memory grows with `max_names` (about 380 bytes per
  name). 100000 names ≈ 40 MB.
- **Inodes**: millions of buffer files cost inodes, not disk space; watch
  `df -i` on the EventLink file system.

---

## 11. Security

| Topic | What the package does | What you should do |
|---|---|---|
| Privileges | runs as the EventLink user; no root, no setuid; systemd units set `NoNewPrivileges` | do not run it as root (the installer warns) |
| EventLink data | read-only access to folders and trace logs; it never moves, opens for writing or deletes EventLink files | — |
| Database | read-only sessions (`default_transaction_read_only=on`) and `READ ONLY` transactions; statement timeouts | use a SELECT-only account (section 4.2) |
| Secrets | DB password from a file (mode 600), an environment variable or an optional parameters file; never logged or exported; configs are written mode 600 | keep the install directory private to the EventLink user |
| Network | the exporters listen on their ports without TLS or authentication (as most Prometheus exporters); metrics contain stream, node and file names and event messages | allow only the Prometheus server to the ports with the host firewall; or bind to one address (`listen_addr: "10.0.0.11:9200"`) |
| Control endpoint | only directory-exporter has one: `POST /-/reload`, protected by `reload_secret` (constant-time comparison) | keep the secret in the config file only |
| Resource abuse | CPU, memory and I/O limits of section 10 | keep the limits |

Example firewall rule (firewalld, as root), allowing only Prometheus at 10.0.0.5:

```sh
firewall-cmd --permanent --add-rich-rule='rule family=ipv4 source address=10.0.0.5 port port=9200-9203 protocol=tcp accept'
firewall-cmd --reload
```

---

## 12. Troubleshooting

| Symptom | Cause / check | Fix |
|---|---|---|
| `ready=503` for minutes after start | first full folder scan still running (large installations) | wait; see `scan cycle complete` in `exporter.log` |
| `start` says "exited during startup" | config error or port in use | read the `.out` file and the log; `--ports` |
| `directory_scan_errors_total` > 0 | folders the user cannot read, or that disappeared | run the exporter as the EventLink user; check permissions |
| `scan truncated` warnings | `max_files_per_dir` reached or the cycle hit `scan_timeout` | set `max_files_per_dir: 0`; raise `scan_timeout` or `scan_interval` |
| Times of a big queue look wrong | `max_stat_files` is a sample | `max_stat_files: 0` (section 10.2) |
| Node "running" although EventLink shows it stopped | the node still writes heartbeats; directory-exporter shows what the node does, the DB exporter what the Node Manager recorded | compare "Running (heartbeat)" and EventLink status in the "EventLink database" section |
| Trace values frozen, "Trace logging" red | the EventLink Node Manager stopped (it writes the trace logs) | check `eventlink_nodemanager_up`; restart EventLink's Node Manager |
| DB values frozen although `eventlink_db_up 1` | the Node Manager stopped: it also writes node status, events and reporting | as above |
| `eventlink_db_up 0` | see `eventlink_db_query_errors_total{query=…}` and `db-exporter.log`: `connect` = network, pg_hba or password; `statement timeout` = slow database | fix access; raise `query_timeout` / `seed_timeout` |
| DB times off by hours | wrong system time zone | set `system_tz_offset` (e.g. `"+03:30"`) |
| Exporter restarts every few minutes, log says "resident memory above MAX_RSS_MB" | hard memory limit reached | trace exporter: lower `max_names`; or raise `MAX_RSS_MB` in `<exporter>.env` |
| Dashboard empty | wrong data source, job names changed, or EventLink server selector | keep the job names of section 6.1; choose "All" |
| Two servers' numbers added together | `eventlink` label missing or equal | give each server its own `eventlink` value (section 6.1) |
| Cron watchdog does nothing | `crontab -l` shows the lines? cron's PATH lacks `curl`? | use full paths in `PATH=` at the top of the crontab, or systemd |

Useful commands:

```sh
curl -s localhost:9200/-/ready; echo
curl -s localhost:9200/metrics | grep -E '^directory_(watched_total|scan_errors_total|last_scan_timestamp_seconds) '
curl -s localhost:9200/metrics | grep -E '^process_(resident_memory_bytes|cpu_seconds_total) '
```

---

## 13. Metric reference

Labels used throughout: `base` (target label, `/streams`), `stream`, `node`,
`type` (queue, `<node>/<input|output|discarded|rejected>/<queue>`), and on the
DB exporter `server` (database name in `db.yml`) and `host` (EventLink host).
Prometheus adds `job`, `instance` and the `eventlink` label of section 6.1.

### 13.1 directory-exporter

| Metric | Type | Meaning |
|---|---|---|
| `directory_cache_ready` | gauge | 1 = first scan complete, 0 = cache is still warming up (cold start). |
| `directory_exporter_build_info` | gauge | Always 1. Labels identify the running exporter build. |
| `directory_last_scan_timestamp_seconds` | gauge | Unix timestamp of when the metrics cache was last refreshed. |
| `directory_watched_total` | gauge | Total number of directories currently on the Watch List. |
| `directory_scan_errors_total` | counter | Cumulative count of scan errors (permission denied, vanished directories, etc.). |
| `directory_reload_total` | counter | Cumulative count of /-/reload triggers received by the exporter. |
| `directory_file_count` | gauge | Total number of regular files directly in the monitored directory (non-recursive). |
| `directory_oldest_file_timestamp_seconds` | gauge | Modification time (Unix epoch) of the oldest file. Detects stuck queues. |
| `directory_newest_file_timestamp_seconds` | gauge | Modification time (Unix epoch) of the newest file. Detects halted ingest. |
| `directory_scrape_duration_seconds` | gauge | Wall-clock duration of the last background scan for this directory. |
| `directory_collector_last_activity_timestamp_seconds` | gauge | Unix timestamp (seconds) of the last file collected by the collector node. |
| `directory_collector_delay_seconds` | gauge | Delay in seconds since the last collected file (now - last_activity_timestamp). 0 if delay <= min_delay. |
| `directory_distributor_last_activity_timestamp_seconds` | gauge | Unix timestamp (seconds) of the last file distributed by the distributor node. |
| `directory_distributor_delay_seconds` | gauge | Delay in seconds since the last distributed file (now - last_activity_timestamp). 0 if delay <= min_delay. |
| `directory_node_last_file_info` | gauge | Information metric exposing the last processed (collected/distributed) filename. |
| `directory_buffer_files` | gauge | Number of files waiting in the collector node's SourceDirectory buffer directory (non-recursive, mirrors the old monitoring script BUFFER_INDEX). 0 when empty, missing, or unreadable. |
| `directory_distributor_target_dir_mtime_seconds` | gauge | Modification time of the distributor's local TargetDirectory (changes whenever a file lands there). |
| `directory_node_info` | gauge | Always 1. node_type is NodeType from control/1/config (empty when unknown). |
| `directory_node_up` | gauge | 1 = node running (heartbeat deadline not passed by more than heartbeat_grace), 0 = stopped or no heartbeat. |
| `directory_node_heartbeat_deadline_timestamp_seconds` | gauge | Epoch from control/1/heartbeat: the time by which the node must write its next heartbeat. |
| `directory_node_state` | gauge | Always 1. state is the node state from control/1/info (e.g. IDLE, TIMER). |
| `directory_node_state_since_timestamp_seconds` | gauge | Epoch from control/1/info: when the node entered its current state. |
| `directory_queue_oldest_collected_timestamp_seconds` | gauge | Collection time (from the EventLink file ID in the name) of the oldest file still waiting in this queue. |

### 13.2 eventlink-trace-exporter

| Metric | Type | Meaning |
|---|---|---|
| `eventlink_trace_exporter_build_info` | gauge | Version of the running eventlink-trace-exporter. |
| `eventlink_trace_ready` | gauge | 1 once the first trace log poll has finished. |
| `eventlink_trace_last_poll_timestamp_seconds` | gauge | Time the last trace log poll finished. |
| `eventlink_trace_poll_duration_seconds` | gauge | Duration of the last trace log poll. |
| `eventlink_trace_poll_errors_total` | counter | Trace log polls that failed (trace directory unreadable). |
| `directory_tracelog_read_bytes_total` | counter | Trace log bytes read by the exporter. |
| `directory_tracelog_lines_total` | counter | Trace log lines parsed by the exporter. |
| `directory_tracelog_files_tracked` | gauge | Trace files (today and yesterday) currently followed. |
| `directory_tracelog_read_budget_hits_total` | counter | Scan cycles that stopped reading early because of trace_max_read_mb; the rest is read next cycle. |
| `directory_tracelog_last_line_timestamp_seconds` | gauge | Timestamp of the newest trace line seen for the stream. If it stops moving while files still flow, EventLink stopped logging. |
| `directory_node_files_total` | counter | Files handled by the node according to trace logs (event = collected  |
| `directory_node_bytes_total` | counter | Bytes handled by the node according to trace logs (event = collected  |
| `directory_node_records_total` | counter | Records read (direction=in) and written (direction=out) by the node according to trace logs. |
| `directory_node_failures_total` | counter | 'aborted and was disabled' / 'Node index … failed' trace lines for the node. |
| `directory_node_last_failure_timestamp_seconds` | gauge | Time of the node's last failure trace line. |
| `directory_node_last_execution_timestamp_seconds` | gauge | Time of the node's last 'Scheduled execution … finished' line (status = success  |
| `directory_stream_latency_seconds` | histogram | Collected → distributed latency per distributed file: distribution time minus the collection epoch in the file ID. |
| `directory_stream_last_latency_seconds` | gauge | Collected → distributed latency of the node's last distributed file. |
| `eventlink_name_latency_seconds` | histogram | Collected → distributed latency per distributed file, matched by the file name before the first "." (falls back to the file ID when the collection was not seen). |
| `eventlink_name_last_latency_seconds` | gauge | Collected → distributed latency of the node's last distributed file, matched by name (ID fallback). |
| `eventlink_latency_match_total` | counter | Distributed file lines by how their collection time was found: name, id (name not seen), none (no usable ID either). |
| `eventlink_pending_files` | gauge | Files collected (trace log) with no Distributed file line yet, matched by name. Includes files EventLink filtered or discarded without logging it; the buffer folders (directory-exporter) are the ground truth. |
| `eventlink_oldest_pending_timestamp_seconds` | gauge | Collection time of the oldest pending file of the stream (see eventlink_pending_files). Absent when nothing is pending. |
| `eventlink_pending_evicted_total` | counter | Pending files dropped without a distribution: reason=expired (older than name_ttl) or limit (max_names reached). |
| `eventlink_tracked_names` | gauge | Collected file names kept in memory for matching. |
| `eventlink_node_last_collected_timestamp_seconds` | gauge | Time of the node's newest "Collected file" trace line (seeded at startup from the last seed_days of trace logs). |
| `eventlink_node_last_distributed_timestamp_seconds` | gauge | Time of the node's newest "Distributed file" trace line (seeded at startup from the last seed_days of trace logs). |
| `eventlink_trace_seed_bytes` | gauge | Trace log bytes read at startup for the last collected / distributed times. |
| `eventlink_trace_seed_files` | gauge | Trace files read at startup for the last collected / distributed times. |

### 13.3 eventlink-db-exporter

| Metric | Type | Meaning |
|---|---|---|
| `eventlink_db_exporter_build_info` | gauge | Version of the running eventlink-db-exporter. |
| `eventlink_db_up` | gauge | 1 = the last poll of the EventLink database succeeded (all queries), 0 = it failed. |
| `eventlink_db_last_poll_timestamp_seconds` | gauge | Time the last database poll finished. |
| `eventlink_db_poll_duration_seconds` | gauge | Duration of the last database poll. |
| `eventlink_db_query_errors_total` | counter | Failed queries by query name. |
| `eventlink_db_read_budget_hits_total` | counter | Polls that stopped at max_rows_per_poll; the rest is read next poll. |
| `eventlink_db_system_tz_offset_seconds` | gauge | Offset of EventLink's system time zone (used for *system and reporting columns). |
| `eventlink_nodemanager_up` | gauge | 1 = the EventLink Node Manager is running: el_component_status is R and was updated within nodemanager_grace (EventLink's own rule). |
| `eventlink_db_component_status` | gauge | Always 1. status is el_component_status.status (R = running, S = stopped). |
| `eventlink_db_component_update_timestamp_seconds` | gauge | Last update of the component's status row (the Node Manager refreshes it while running). |
| `eventlink_db_node_status` | gauge | Always 1. status is the node status kept by the Node Manager in el_node_log (RUNNING, STOPPED, FAILED, …); frozen while the Node Manager is down. |
| `eventlink_db_node_last_start_timestamp_seconds` | gauge | Last start of the node (el_node_log.laststartsystem). |
| `eventlink_db_events_total` | counter | EventLink events (el_events) since the exporter started, by type: E error, W warning, I info. node="" = stream or system events. |
| `eventlink_db_node_last_event_timestamp_seconds` | gauge | Newest event of the node by type (seeded at startup from the last seed_days days). |
| `eventlink_db_node_last_failure_timestamp_seconds` | gauge | Newest failure event of the node (failure_keys, e.g. NODEMANAGER03011 aborted and was disabled). |
| `eventlink_db_node_last_error_info` | gauge | Always 1. Key and message of the node's newest error event. |
| `eventlink_db_stream_last_collected_timestamp_seconds` | gauge | Newest collection of the stream (el_reporting_input_details.collectionend; seeded from the last seed_days days). |
| `eventlink_db_stream_last_distributed_timestamp_seconds` | gauge | Newest distribution of the stream (el_reporting_output_details.distributionend; seeded from the last seed_days days). |
| `eventlink_db_stream_files_total` | counter | Files collected / distributed by the stream since the exporter started (reporting tables). |
| `eventlink_db_stream_bytes_total` | counter | Bytes collected / distributed by the stream since the exporter started (reporting tables). |
| `eventlink_db_stream_records_total` | counter | Records collected / distributed by the stream since the exporter started (reporting tables). |

### 13.4 All three exporters

| Metric | Type | Meaning |
|---|---|---|
| `process_cpu_seconds_total` | counter | CPU time used (user + system) |
| `process_resident_memory_bytes` | gauge | resident memory (RSS) |
| `process_virtual_memory_bytes` | gauge | virtual memory |
| `process_threads` | gauge | OS threads |
| `process_open_fds`, `process_max_fds` | gauge | open / allowed file descriptors |
| `process_start_time_seconds` | gauge | start time (changes on every restart) |
| `go_goroutines`, `go_memstats_heap_inuse_bytes`, `go_memstats_sys_bytes`, `go_gc_cycles_total`, `go_gomaxprocs` | | Go runtime |

### 13.5 HTTP endpoints

| Path | Exporters | Meaning |
|---|---|---|
| `GET /metrics` | all | metrics |
| `GET /-/healthy` | all | 200 while the process runs |
| `GET /-/ready` | all | 200 after the first scan / poll, 503 before |
| `POST /-/reload` | directory-exporter | re-discover and rescan now (`Authorization: Bearer <reload_secret>`) |

---

## 14. Appendix: where the data comes from

### 14.1 Files

| Data | Path (under the EventLink base) |
|---|---|
| Queues | `status/streams/<stream>/nodes/<node>/{input,output,discarded,rejected}/<queue>` (usually symlinks into the buffer area) |
| Node type, source / target directory | `…/nodes/<node>/control/1/config` (`NodeType`, `SourceDirectory`, `TargetDirectory`) |
| Node alive | `…/control/1/heartbeat`: a **deadline** (the time by which the node must write again), not a "last seen" time |
| Node state | `…/control/1/info`: `<epoch> <STATE>` (IDLE, TIMER, …) |
| Collector's last file | `…/storage/1/audit_info` |
| Trace logs | `tracelog/execution_trace_<stream>_<YYYYMMDD>_<n>.log`, rotated `_0, _1, …` (lines are out of order across rotations; the exporter handles that) |
| Collection time of a file | the first 10 digits of its EventLink file ID (`1793016004x003_0011282` → 1793016004) in buffer file names and trace lines |
| Same file under several names | matched by the part before the first "." (`file_x.el.gz` = `file_x.el.encoded`) |

The **Node Manager** writes all trace logs (and a line every minute), the node
status, the events and the reporting rows in the database. When it stops,
those freeze; the nodes themselves can keep running and writing heartbeats.

### 14.2 Database tables

| Table | Used for |
|---|---|
| `el_component_status` | Node Manager: status `R` and `updatetime` younger than 2 min 30 s = running (EventLink's own rule in `el_v_host_nm_status`) |
| `el_node_log` + `el_nodes`, `el_stream_log`, `el_stream_versions`, `el_streams` | status per node of the current stream versions; `type` is one letter per position: C collector, D decoder, B business logic, E encoder, D distributor, B BLT, I intermediate, S standalone |
| `el_events` | events (E error, W warning, I info); `NODEMANAGER03011` = node aborted and was disabled |
| `el_reporting_input_details`, `el_reporting_output_details` | one row per collected / distributed file, indexed on `collectionend` / `distributionend` |

Times: `*gmt` columns are UTC, `*local` the database zone, `*system` columns
and the reporting tables EventLink's system zone (offset in
`el_events.eventtimesystemtz`).
