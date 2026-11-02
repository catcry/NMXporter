#!/bin/bash
# Exporter control script (no root needed), for all three exporters:
#
#   directory-exporterctl.sh       start | stop | restart | status   (./directory-exporter, targets.yml, :9200)
#   eventlink-trace-exporterctl.sh start | stop | restart | status   (./eventlink-trace-exporter, trace.yml, :9202)
#   eventlink-db-exporterctl.sh    start | stop | restart | status   (./eventlink-db-exporter, db.yml, :9203)
#
# The same file is installed under each name, each next to its exporter; the
# name of the script selects the exporter. It runs ./<exporter> from the
# directory the script lives in, with CONFIG_FILE=./<config>. "start" does
# nothing when the exporter is already running, so it can be used as a
# watchdog from cron:
#
#   @reboot      <install dir>/directory-exporter/directory-exporterctl.sh start
#   */5 * * * *  <install dir>/directory-exporter/directory-exporterctl.sh start
#
# (install.sh --cron adds these lines for every installed exporter.)
#
# A running exporter is found by its working directory, so one started by
# hand (without this script) is also detected.

HOME_DIR=$(cd "$(dirname "$0")" && pwd)
NAME=$(basename "$0" ctl.sh)
# Resource limits per exporter (measured use on 102 in brackets):
#   PROCS = GOMAXPROCS, cores Go may use at once
#   MEM   = GOMEMLIMIT, soft limit: the Go GC works harder above it
#   RSS   = MAX_RSS_MB, hard limit: the exporter exits, the cron watchdog restarts it
case "$NAME" in
directory-exporter) CONF=targets.yml; DEFAULT_PORT=9200; PROCS=2; MEM=128MiB; RSS=256 ;;      # ~11 MB, 5M-file dirs
eventlink-trace-exporter) CONF=trace.yml; DEFAULT_PORT=9202; PROCS=2; MEM=384MiB; RSS=768 ;;  # 10 MB, ~190 MB at max_names 500000
eventlink-db-exporter) CONF=db.yml; DEFAULT_PORT=9203; PROCS=1; MEM=64MiB; RSS=128 ;;         # ~8 MB
*) echo "unknown exporter '$NAME': install this script as directory-exporterctl.sh, eventlink-trace-exporterctl.sh or eventlink-db-exporterctl.sh"; exit 2 ;;
esac
# Optional overrides next to the script, e.g. directory-exporter.env with
#   GOMAXPROCS=4  GOMEMLIMIT=256MiB  MAX_RSS_MB=512  NICE=10
[ -f "$HOME_DIR/$NAME.env" ] && . "$HOME_DIR/$NAME.env"
BIN=./$NAME
PIDFILE="$HOME_DIR/$NAME.pid"
OUTFILE="$HOME_DIR/$NAME.out" # panics/stderr only; logs go to log_file

port() {
	p=$(sed -n 's/^listen_addr:[^"]*"[^:]*:\([0-9][0-9]*\)".*/\1/p' "$HOME_DIR/$CONF" | head -1)
	echo "${p:-$DEFAULT_PORT}"
}

# addr is host:port to query: the host of listen_addr when it names one
# (e.g. "10.0.0.11:9200"), else localhost.
addr() {
	h=$(sed -n 's/^listen_addr:[^"]*"\([^:"]*\):[0-9][0-9]*".*/\1/p' "$HOME_DIR/$CONF" | head -1)
	echo "${h:-localhost}:$(port)"
}

find_pid() {
	if [ -f "$PIDFILE" ]; then
		p=$(cat "$PIDFILE")
		if [ -n "$p" ] && [ "$(readlink /proc/"$p"/cwd 2>/dev/null)" = "$HOME_DIR" ]; then
			echo "$p"
			return
		fi
	fi
	for p in $(pgrep -f "$NAME"); do
		if [ "$(readlink /proc/"$p"/cwd 2>/dev/null)" = "$HOME_DIR" ] && tr '\0' ' ' < /proc/"$p"/cmdline | grep -q "^\./$NAME "; then
			echo "$p"
			return
		fi
	done
}

# http <path> code|body: GET from the exporter with curl, else wget, else
# bash's own /dev/tcp (isolated servers may have neither). HTTP_CLIENT=curl|wget|bash forces one.
http() {
	a=$(addr)
	client=${HTTP_CLIENT:-}
	if [ -z "$client" ]; then
		if command -v curl >/dev/null 2>&1; then client=curl
		elif command -v wget >/dev/null 2>&1; then client=wget
		else client=bash
		fi
	fi
	case "$client" in
	curl)
		if [ "$2" = code ]; then curl -s -m 5 -o /dev/null -w '%{http_code}' "http://$a$1" 2>/dev/null
		else curl -s -m 5 "http://$a$1" 2>/dev/null
		fi ;;
	wget)
		if [ "$2" = code ]; then
			c=$(wget -q -T 5 -S -O /dev/null "http://$a$1" 2>&1 | awk '/^ *HTTP\// {c=$2} END {print c}')
			echo "${c:-000}"
		else wget -q -T 5 -O - "http://$a$1" 2>/dev/null
		fi ;;
	*)
		to=""
		command -v timeout >/dev/null 2>&1 && to="timeout 5"
		resp=$($to bash -c 'exec 3<>"/dev/tcp/$0/$1" && printf "GET %s HTTP/1.0\r\nHost: %s\r\n\r\n" "$2" "$0" >&3 && cat <&3' "${a%:*}" "${a##*:}" "$1" 2>/dev/null)
		if [ "$2" = code ]; then
			c=$(printf '%s\n' "$resp" | head -1 | awk '{print $2}')
			echo "${c:-000}"
		else printf '%s\n' "$resp" | sed '1,/^\r*$/d'
		fi ;;
	esac
}

ready() {
	http /-/ready code
}

start() {
	p=$(find_pid)
	if [ -n "$p" ]; then
		echo "$p" > "$PIDFILE"
		echo "already running (pid $p)"
		return 0
	fi
	cd "$HOME_DIR" || exit 1
	# Low CPU and disk priority: EventLink always goes first. nice and ionice
	# exec the exporter, so $! is its pid and its cmdline stays ./<exporter>.
	IONICE=""
	command -v ionice >/dev/null 2>&1 && IONICE="ionice -c 2 -n 7"
	GOMAXPROCS=${GOMAXPROCS:-$PROCS} GOMEMLIMIT=${GOMEMLIMIT:-$MEM} MAX_RSS_MB=${MAX_RSS_MB:-$RSS} \
		CONFIG_FILE=./$CONF nohup nice -n "${NICE:-10}" $IONICE $BIN >> "$OUTFILE" 2>&1 &
	echo $! > "$PIDFILE"
	i=0
	while [ $i -lt 30 ]; do
		[ "$(ready)" = "200" ] && { echo "started (pid $(cat "$PIDFILE")), ready on :$(port)"; return 0; }
		kill -0 "$(cat "$PIDFILE")" 2>/dev/null || { echo "exited during startup, see $OUTFILE and the log file"; return 1; }
		sleep 1
		i=$((i + 1))
	done
	echo "started (pid $(cat "$PIDFILE")) but not ready after 30s"
	return 1
}

stop() {
	p=$(find_pid)
	if [ -z "$p" ]; then
		echo "not running"
		rm -f "$PIDFILE"
		return 0
	fi
	kill "$p"
	i=0
	while kill -0 "$p" 2>/dev/null && [ $i -lt 15 ]; do sleep 1; i=$((i + 1)); done
	kill -0 "$p" 2>/dev/null && { echo "did not stop in 15s, killing"; kill -9 "$p"; }
	rm -f "$PIDFILE"
	echo "stopped (pid $p)"
}

status() {
	p=$(find_pid)
	if [ -z "$p" ]; then
		echo "not running"
		return 3
	fi
	v=$(http /metrics body | sed -n 's/^[a-z_]*exporter_build_info{version="\([^"]*\)".*/\1/p')
	echo "running (pid $p) port $(port) ready=$(ready) version=${v:-unknown}"
}

case "$1" in
start) start ;;
stop) stop ;;
restart) stop && start ;;
status) status ;;
*) echo "usage: $0 start|stop|restart|status"; exit 2 ;;
esac
