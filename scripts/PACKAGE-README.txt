EventLink exporters for Prometheus
==================================

Three exporters that monitor an EventLink installation, a Grafana dashboard,
Prometheus alert rules and an installer. Everything runs as the EventLink
user; no root, no agents, read-only on EventLink data and database.

  directory-exporter        :9200  buffers and queues, node status (heartbeat)
  eventlink-trace-exporter  :9202  trace logs: files, records, failures, delay
  eventlink-db-exporter     :9203  EventLink database: Node Manager, node status, events

Quick start (as the EventLink user, on the EventLink server):

  tar xzf eventlink-exporters-<version>-linux-amd64.tar.gz
  cd eventlink-exporters-<version>-linux-amd64
  ./install.sh --check  --db-user monitor --db-password-file pw.txt          # change nothing
  ./install.sh --start --cron --db-user monitor --db-password-file pw.txt    # install, start, watchdog

  pw.txt holds only the database password. Without a database account:
  ./install.sh --check --only directory,trace  (and the same for the install).
  Paths that cannot be found are reported as FAIL: give them with
  --streams-dir / --tracelog-dir (MANUAL.md, section 5).

No Go, compiler or internet is needed on the server: the binaries are
ready-built and statically linked. Copy this one file there and install.

Then add ~/eventlink-exporters/prometheus-scrape.yml to Prometheus and import
grafana/eventlink-overview.json into Grafana.

Read MANUAL.md before installing on a production server.
