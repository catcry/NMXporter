# NMXporter: Prometheus exporters for EventLink

Three small exporters that monitor an EventLink mediation server, plus a Grafana dashboard, Prometheus alert
rules and an installer. They run as the EventLink user with no root and no agents. They only read EventLink data:
folders, trace logs and the database.

| Exporter | Port | Reads | Answers |
|---|---|---|---|
| `directory-exporter` | 9200 | stream folders: queues, node control files, collector/distributor files | backlog, live delay, stuck queues, running/stopped nodes, last collected/distributed file |
| `eventlink-trace-exporter` | 9202 | EventLink trace logs | files and records per node, failures, collected → distributed delay |
| `eventlink-db-exporter` | 9203 | EventLink PostgreSQL database (read-only) | Node Manager up/down, node status (RUNNING / STOPPED / FAILED), events, files per stream |

```
EventLink server                                   monitoring server
 status/streams/…  ──► directory-exporter  :9200 ─┐
 tracelog/…        ──► eventlink-trace-exp. :9202 ─┼──► Prometheus ──► Grafana
 PostgreSQL        ──► eventlink-db-exporter :9203 ─┘
```

Current version: **1.7.2**. See [CHANGELOG.md](CHANGELOG.md).

## Install on an EventLink server

The release package holds ready-built, statically linked Linux x86_64 binaries. **No Go, compiler or internet is
needed on the server**: copy one file and run the installer.

```sh
tar xzf eventlink-exporters-1.7.2-linux-amd64.tar.gz
cd eventlink-exporters-1.7.2-linux-amd64
./install.sh --check --name <server> --db-user <user> --db-password-file <file>   # changes nothing
./install.sh         --name <server> --db-user <user> --db-password-file <file> --start --cron
```

- `--check` lists what it found and reports FAIL for anything it cannot find. Give paths with `--streams-dir` and
  `--tracelog-dir`, and the database with `--db-host`, `--db-port` and `--db-name`.
- Without a database account, install only the first two exporters: `--only directory,trace`.
- Then add the generated `~/eventlink-exporters/prometheus-scrape.yml` to Prometheus and import
  `grafana/eventlink-overview.json` into Grafana.

Everything else (requirements, a read-only database account, Prometheus, alerts, upgrade, uninstall, limits,
security, troubleshooting) is in the [manual](docs/MANUAL.md).

## Documentation

| Document | For |
|---|---|
| [docs/MANUAL.md](docs/MANUAL.md) | Installing and operating the exporters |
| [docs/METRICS.md](docs/METRICS.md) | Every metric: meaning, source, labels |
| [docs/DASHBOARD.md](docs/DASHBOARD.md) | Every dashboard section, panel and table column, and how it is calculated |

## Repository layout

```
cmd/
  directory-exporter/         folders, queues, node status
  eventlink-trace-exporter/   trace logs
  eventlink-db-exporter/      EventLink database (read-only)
internal/                     shared code: trace log parser, log rotation, process metrics
config/                       config templates the installer fills in (targets.yml, trace.yml, db.yml)
grafana/                      the dashboard
prometheus/                   alert rules and the scrape-config template
systemd/                      service template (optional, needs root)
scripts/                      install.sh, uninstall.sh, exporterctl.sh, build.sh
tools/                        stress tests; addfilter (adds the EventLink-server filter to dashboard queries)
docs/                         manual, metrics reference, dashboard guide
```

## Build

Needs Go 1.22 or newer, on any OS:

```sh
go test ./...                       # unit tests (some tests need Linux)
bash scripts/build.sh               # -> dist/eventlink-exporters-<VERSION>-linux-amd64.tar.gz
```

`build.sh` cross-compiles static Linux binaries (`CGO_ENABLED=0`) and writes the package with `SHA256SUMS` and a
`.sha256` file for the tarball.

## Resource use

Measured on a server with 31 streams, 389 nodes, 1615 queue folders and one queue holding 5 million files:

| Exporter | RAM | CPU |
|---|---|---|
| directory-exporter | ~20 MB | 0.2 s per scan cycle; a full re-read of a 5-million-file folder takes ~23 s every 10 min |
| eventlink-trace-exporter | ~10 MB (up to ~190 MB with 500 000 tracked file names) | 1 GB of trace logs read in 10 s |
| eventlink-db-exporter | ~10 MB | a few ms per poll |

Each exporter runs at low CPU and disk priority, with a soft memory limit and a hard memory limit; the cron
watchdog restarts it if it exits. See the manual, section 10.
