# Changelog

## 1.7.2 (2026-11-02)

- **Installer:** the database login no longer depends on a `parameters.txt` file. That file is not part of
  EventLink: before, a missing file made the database exporter fail at startup. New options `--db-host`,
  `--db-port` and `--db-name`, next to `--db-user` and `--db-password-file`. `--check` tests that the database
  port answers. A `parameters.txt` is still used when one exists.
- **Dashboard:** "Last file collected" and "Last file distributed" panels (renamed from "Collectors / Distributors:
  time since last … file (idle)").
- **Docs:** metrics reference (`docs/METRICS.md`) and dashboard guide (`docs/DASHBOARD.md`); both are also in the
  package.
- **Repository:** one Go module (`github.com/catcry/NMXporter`) with the three exporters under `cmd/`.

Known issues:
- Dashboard "Exporter memory (RSS)": the dashed memory-limit lines do not show (invalid query). The memory line
  itself works.
- Dashboard "About" panel: says oldest/newest file times are sampled from 5000 files. With the packaged
  configuration (`max_stat_files: 0`) they are exact.
- eventlink-db-exporter: if the startup seed takes longer than `seed_timeout`, it is retried every poll. On very
  large databases set `seed_days: 30`.
- `exporterctl.sh start` waits 30 s for the exporter to be ready. The first exact scan of a server with millions
  of waiting files takes longer, so it prints "started … but not ready after 30s" although the exporter is fine;
  `status` shows `ready=200` shortly after.

## 1.7.1

- The control script needs no `curl`: it uses `wget`, else bash `/dev/tcp`, for servers where nothing can be
  installed.
- Manual section "Isolated servers".

## 1.7.0

- Stress-test fixes: folders whose file list did not change are not re-read (`unchanged_rescan_interval`), and
  exact timestamps on huge folders (`max_stat_files: 0`).
- Resource limits per exporter (GOMAXPROCS, GOMEMLIMIT, hard RSS limit), low CPU and disk priority, and process
  metrics.
- Release package with installer, uninstaller, manual, systemd template, alert rules, and an EventLink-server
  filter on the dashboard.

## 1.6.0

- New `eventlink-db-exporter`: Node Manager status, node status, events and reporting from the EventLink
  database, read-only. Dashboard section "EventLink database".

## 1.5.0

- Last collected / last distributed time per stream and node (trace-log seed at startup). Dashboard section
  "Last collected / distributed".

## 1.4.0

- Trace-log parsing moved into its own exporter, `eventlink-trace-exporter`. Collected → distributed delay
  matched by file name.

## 1.3.0 and earlier

- `directory-exporter`: file counts and oldest/newest file per queue, node heartbeat status, collector and
  distributor activity, live delay from EventLink file IDs.
