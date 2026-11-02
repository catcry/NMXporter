#!/bin/bash
# Installs (or upgrades) the EventLink exporters for the current user.
# Run it as the EventLink OS user (the one that owns the EventLink installation);
# no root needed. See MANUAL.md, "Installation", for every option.
#
#   ./install.sh --check                      # only check this server
#   ./install.sh                              # install all three, do not start
#   ./install.sh --start --cron               # install, start, add watchdog
#   ./install.sh --only directory,trace       # without the database exporter
set -u

PKG=$(cd "$(dirname "$0")" && pwd)
VERSION=$(cat "$PKG/VERSION" 2>/dev/null || echo unknown)

PREFIX="$HOME/eventlink-exporters"
EL_BASE="" STREAMS="" TRACELOG="" PARAMS=""
NAME=$(hostname -s 2>/dev/null || hostname)
HOST=""
PORTS="9200,9202,9203"
ONLY="directory,trace,db"
DB_USER="" DB_PWFILE="" DB_HOST="" DB_PORT="" DB_NAME=""
DO_CRON=0 DO_START=0 FORCE=0 CHECK_ONLY=0

usage() {
	sed -n '2,10p' "$0" | sed 's/^# \{0,1\}//'
	cat <<EOF

Options:
  --prefix DIR            install directory (default: \$HOME/eventlink-exporters)
  --eventlink-base DIR    EventLink base directory (default: detected; contains status/ and tracelog/)
  --streams-dir DIR       stream folders (default: <base>/status/streams)
  --tracelog-dir DIR      trace logs (default: <base>/tracelog)
  --parameters FILE       optional KEY=VALUE file with EL_DB_HOST/PORT/NAME/USER/PASSWORD
                          (used when it exists at <install>/meta/EVENTLINK_DB/parameters.txt)
  --name NAME             name of this EventLink server in Prometheus/Grafana (default: hostname)
  --host ADDR             address Prometheus uses to reach this server (default: first IP)
  --ports D,T,B           ports of directory, trace and db exporter (default: 9200,9202,9203)
  --only LIST             exporters to install: directory,trace,db (default: all)
  --db-user USER          database user for eventlink-db-exporter (default: from the parameters file)
  --db-password-file F    file with that user's password (copied, mode 600)
  --db-host H, --db-port P, --db-name N
                          EventLink database (default: 127.0.0.1, 5432, eventlink)
  --cron                  add the watchdog to the user's crontab (@reboot and every 5 min)
  --start                 start the exporters after installing
  --force                 overwrite existing config files (otherwise new ones are written as *.new)
  --check                 only check the server, change nothing
EOF
	exit "${1:-0}"
}

while [ $# -gt 0 ]; do
	case "$1" in
	--prefix) PREFIX=$2; shift ;;
	--eventlink-base) EL_BASE=$2; shift ;;
	--streams-dir) STREAMS=$2; shift ;;
	--tracelog-dir) TRACELOG=$2; shift ;;
	--parameters) PARAMS=$2; shift ;;
	--name) NAME=$2; shift ;;
	--host) HOST=$2; shift ;;
	--ports) PORTS=$2; shift ;;
	--only) ONLY=$2; shift ;;
	--db-user) DB_USER=$2; shift ;;
	--db-password-file) DB_PWFILE=$2; shift ;;
	--db-host) DB_HOST=$2; shift ;;
	--db-port) DB_PORT=$2; shift ;;
	--db-name) DB_NAME=$2; shift ;;
	--cron) DO_CRON=1 ;;
	--start) DO_START=1 ;;
	--force) FORCE=1 ;;
	--check) CHECK_ONLY=1 ;;
	-h | --help) usage 0 ;;
	*) echo "unknown option: $1"; usage 2 ;;
	esac
	shift
done

ERRORS=0
ok() { printf '  [ ok ] %s\n' "$*"; }
warn() { printf '  [warn] %s\n' "$*"; }
fail() { printf '  [FAIL] %s\n' "$*"; ERRORS=$((ERRORS + 1)); }
want() { case ",$ONLY," in *",$1,"*) return 0 ;; esac; return 1; }

IFS=, read -r PORT_DIR PORT_TRACE PORT_DB <<EOF
$PORTS
EOF
case "$PREFIX" in /*) ;; *) PREFIX="$(pwd)/$PREFIX" ;; esac
# nearest existing directory, for the free space check
existing=$PREFIX
while [ ! -d "$existing" ]; do existing=$(dirname "$existing"); done

# ── Detect EventLink ──────────────────────────────────────────────────────────
if [ -z "$EL_BASE" ]; then
	for c in "${EVENTLINK_HOME:+$EVENTLINK_HOME/base}" /comptel/elink/install/EventLink/base "$HOME/install/EventLink/base"; do
		[ -n "$c" ] && [ -d "$c/status/streams" ] && { EL_BASE=$c; break; }
	done
fi
STREAMS=${STREAMS:-${EL_BASE:+$EL_BASE/status/streams}}
TRACELOG=${TRACELOG:-${EL_BASE:+$EL_BASE/tracelog}}
# The parameters file is not part of every EventLink installation: use the
# usual place only when a file is really there.
if [ -z "$PARAMS" ] && [ -n "$EL_BASE" ]; then
	p=$(cd "$EL_BASE/../.." 2>/dev/null && pwd)/meta/EVENTLINK_DB/parameters.txt
	[ -r "$p" ] && PARAMS=$p
fi
if [ -z "$HOST" ]; then
	HOST=$(hostname -I 2>/dev/null | awk '{print $1}')
	HOST=${HOST:-$(hostname)}
fi

echo "EventLink exporters $VERSION — checking $(hostname) as $(id -un)"
[ "$(uname -s)" = Linux ] && [ "$(uname -m)" = x86_64 ] && ok "Linux x86_64" || fail "needs Linux x86_64 (found $(uname -s) $(uname -m))"
[ "$(id -u)" = 0 ] && warn "running as root: run as the EventLink user instead, so the exporters do not run as root"
if command -v curl >/dev/null 2>&1; then ok "curl found"
elif command -v wget >/dev/null 2>&1; then ok "wget found (no curl; the control script uses wget)"
else warn "neither curl nor wget: the control script uses bash /dev/tcp (works, no install needed)"
fi
command -v bash >/dev/null 2>&1 || fail "bash is needed"
command -v ionice >/dev/null 2>&1 && ok "ionice found" || warn "ionice not found: exporters run without low disk priority"

if want directory || want trace; then
	[ -n "$EL_BASE" ] && ok "EventLink base: $EL_BASE" || warn "EventLink base not found (set --eventlink-base or --streams-dir/--tracelog-dir)"
fi
if want directory; then
	if [ -n "$STREAMS" ] && [ -r "$STREAMS" ] && [ -x "$STREAMS" ]; then
		ok "streams: $STREAMS ($(ls "$STREAMS" | wc -l) streams)"
	else
		fail "streams folder not readable: ${STREAMS:-unset} (--streams-dir)"
	fi
fi
if want directory || want trace; then
	if [ -n "$TRACELOG" ] && [ -r "$TRACELOG" ] && [ -x "$TRACELOG" ]; then
		ok "trace logs: $TRACELOG ($(ls "$TRACELOG" | wc -l) files)"
	else
		fail "trace log folder not readable: ${TRACELOG:-unset} (--tracelog-dir)"
	fi
fi
if want db; then
	if [ -n "$DB_USER" ]; then
		ok "database user: $DB_USER"
		[ -n "$DB_PWFILE" ] && [ -r "$DB_PWFILE" ] && ok "password file: $DB_PWFILE" || fail "--db-user needs a readable --db-password-file"
		PARAMS=""
	elif [ -n "$PARAMS" ] && [ -r "$PARAMS" ]; then
		ok "database parameters: $PARAMS ($(grep -E '^EL_DB_(HOST|PORT|NAME|USER)=' "$PARAMS" | tr '\n' ' '))"
		grep -q '^EL_DB_PASSWORD=' "$PARAMS" || warn "no EL_DB_PASSWORD in $PARAMS"
	elif [ -n "$PARAMS" ]; then
		fail "parameters file not readable: $PARAMS (--parameters)"
	else
		fail "no database account: give --db-user USER --db-password-file FILE (and --db-host/--db-port/--db-name), or --parameters FILE, or leave the DB exporter out with --only directory,trace"
	fi
	# Values from the parameters file, unless given as options.
	if [ -n "$PARAMS" ] && [ -r "$PARAMS" ]; then
		DB_HOST=${DB_HOST:-$(sed -n 's/^EL_DB_HOST=//p' "$PARAMS" | head -1)}
		DB_PORT=${DB_PORT:-$(sed -n 's/^EL_DB_PORT=//p' "$PARAMS" | head -1)}
		DB_NAME=${DB_NAME:-$(sed -n 's/^EL_DB_NAME=//p' "$PARAMS" | head -1)}
	fi
	DB_HOST=${DB_HOST:-127.0.0.1} DB_PORT=${DB_PORT:-5432} DB_NAME=${DB_NAME:-eventlink}
	# Only checks that the port answers (bash /dev/tcp); the exporter tests the login.
	if (exec 3<>"/dev/tcp/$DB_HOST/$DB_PORT") 2>/dev/null; then
		ok "database $DB_HOST:$DB_PORT answers (database $DB_NAME)"
	else
		warn "database $DB_HOST:$DB_PORT does not answer (--db-host / --db-port); eventlink-db-exporter will report eventlink_db_up 0"
	fi
fi

# A port must be free, unless our own exporter from this prefix holds it.
port_busy() {
	hex=$(printf ':%04X' "$1")
	awk -v h="$hex" 'NR>1 && $4=="0A" && substr($2, length($2)-4)==h {f=1} END {exit !f}' /proc/net/tcp /proc/net/tcp6 2>/dev/null
}
own_running() { [ -x "$PREFIX/$1/$1ctl.sh" ] && "$PREFIX/$1/$1ctl.sh" status >/dev/null 2>&1; }
for x in "directory directory-exporter $PORT_DIR" "trace eventlink-trace-exporter $PORT_TRACE" "db eventlink-db-exporter $PORT_DB"; do
	set -- $x
	want "$1" || continue
	if port_busy "$3" && ! own_running "$2"; then
		fail "port $3 for $2 is in use (--ports)"
	else
		ok "port $3 for $2"
	fi
done
avail=$(df -Pm "$existing" 2>/dev/null | awk 'NR==2 {print $4}')
[ -w "$existing" ] && ok "install directory: $PREFIX" || fail "cannot write to $existing (--prefix)"
[ "${avail:-0}" -gt 100 ] && ok "free disk at $PREFIX: ${avail} MB" || fail "less than 100 MB free at $PREFIX"

if [ "$ERRORS" -gt 0 ]; then
	echo "$ERRORS check(s) failed; nothing installed."
	exit 1
fi
[ "$CHECK_ONLY" = 1 ] && { echo "All checks passed (--check: nothing changed)."; exit 0; }

# ── Install ───────────────────────────────────────────────────────────────────
secret=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')
render() { # template output
	db_creds="    host: \"$DB_HOST\"
    port: $DB_PORT
    dbname: \"$DB_NAME\""
	if [ -n "$DB_USER" ]; then
		db_creds="$db_creds
    user: \"$DB_USER\"
    password_file: ./db.password"
	else
		db_creds="$db_creds
    # user and password from the parameters file
    eventlink_parameters: $PARAMS
    parameters_prefix: EL"
	fi
	awk -v creds="$db_creds" '{ if ($0 == "@DB_CREDENTIALS@") print creds; else print }' "$1" | sed \
		-e "s|@PORT_DIR@|$PORT_DIR|g" -e "s|@PORT_TRACE@|$PORT_TRACE|g" -e "s|@PORT_DB@|$PORT_DB|g" \
		-e "s|@STREAMS_DIR@|$STREAMS|g" -e "s|@TRACELOG_DIR@|$TRACELOG|g" -e "s|@PARAMETERS@|$PARAMS|g" \
		-e "s|@SERVER_NAME@|$NAME|g" -e "s|@HOST@|$HOST|g" -e "s|@RELOAD_SECRET@|$secret|g" >"$2"
}

# exporter key, binary, config, template, procs, mem, rss
install_one() {
	key=$1 bin=$2 conf=$3 tpl=$4 procs=$5 mem=$6 rss=$7
	want "$key" || return 0
	d="$PREFIX/$bin"
	mkdir -p "$d"
	was_running=0
	if [ -x "$d/$bin" ]; then
		"$d/${bin}ctl.sh" status >/dev/null 2>&1 && was_running=1
		[ "$was_running" = 1 ] && "$d/${bin}ctl.sh" stop >/dev/null
		cp -p "$d/$bin" "$d/$bin.prev"
		echo "  upgrade $bin ($(cat "$d/VERSION" 2>/dev/null || echo '?') -> $VERSION), previous binary kept as $bin.prev"
	else
		echo "  install $bin -> $d"
	fi
	cp "$PKG/bin/$bin" "$d/$bin.tmp" && mv "$d/$bin.tmp" "$d/$bin"
	cp "$PKG/bin/exporterctl.sh" "$d/${bin}ctl.sh"
	chmod 755 "$d/$bin" "$d/${bin}ctl.sh"
	echo "$VERSION" >"$d/VERSION"
	if [ -f "$d/$conf" ] && [ "$FORCE" = 0 ]; then
		render "$PKG/config/$tpl" "$d/$conf.new"
		echo "    kept $conf (the new template is $conf.new; --force overwrites)"
	else
		render "$PKG/config/$tpl" "$d/$conf"
	fi
	chmod 600 "$d/$conf"
	if [ "$key" = db ] && [ -n "$DB_PWFILE" ]; then
		cp "$DB_PWFILE" "$d/db.password" && chmod 600 "$d/db.password"
	fi
	mkdir -p "$PREFIX/systemd"
	sed -e "s|@NAME@|$bin|g" -e "s|@DIR@|$d|g" -e "s|@CONF@|$conf|g" -e "s|@USER@|$(id -un)|g" \
		-e "s|@GROUP@|$(id -gn)|g" -e "s|@PROCS@|$procs|g" -e "s|@MEM@|$mem|g" -e "s|@RSS@|$rss|g" \
		-e "s|@CPUQUOTA@|$((procs * 100))|g" "$PKG/systemd/exporter.service.in" >"$PREFIX/systemd/$bin.service"
	if [ "$was_running" = 1 ] || [ "$DO_START" = 1 ]; then
		"$d/${bin}ctl.sh" start
	fi
}

echo "Installing into $PREFIX"
install_one directory directory-exporter targets.yml targets.yml.in 2 128MiB 256
install_one trace eventlink-trace-exporter trace.yml trace.yml.in 2 384MiB 768
install_one db eventlink-db-exporter db.yml db.yml.in 1 64MiB 128
render "$PKG/prometheus/scrape.yml.in" "$PREFIX/prometheus-scrape.yml"
cp "$PKG/MANUAL.md" "$PREFIX/" 2>/dev/null

if [ "$DO_CRON" = 1 ]; then
	if command -v crontab >/dev/null 2>&1; then
		mark="# eventlink-exporters $PREFIX"
		# Read the whole crontab first: other jobs of the user must stay.
		current=$(crontab -l 2>/dev/null | grep -vF "$mark")
		{
			[ -n "$current" ] && printf '%s\n' "$current"
			for x in directory:directory-exporter trace:eventlink-trace-exporter db:eventlink-db-exporter; do
				want "${x%%:*}" || continue
				b=${x#*:}
				echo "@reboot $PREFIX/$b/${b}ctl.sh start >/dev/null 2>&1 $mark"
				echo "*/5 * * * * $PREFIX/$b/${b}ctl.sh start >/dev/null 2>&1 $mark"
			done
		} | crontab - && echo "  cron watchdog added (crontab -l)"
	else
		warn "crontab not available: use the systemd units in $PREFIX/systemd (root)"
	fi
fi

cat <<EOF

Done. Next steps (MANUAL.md has the details):
  1. Start / check:   $PREFIX/<exporter>/<exporter>ctl.sh start | status
  2. Prometheus:      add $PREFIX/prometheus-scrape.yml under scrape_configs, reload
  3. Alerts:          prometheus/alerts.yml from the package (optional)
  4. Grafana:         import grafana/eventlink-overview.json, choose the Prometheus data source
EOF
[ "$DO_CRON" = 0 ] && echo "  5. Watchdog:        ./install.sh --cron (or the systemd units in $PREFIX/systemd)"
exit 0
