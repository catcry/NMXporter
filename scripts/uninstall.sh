#!/bin/bash
# Stops the EventLink exporters, removes their cron lines and, with --purge,
# deletes the install directory (configs and logs included).
#   ./uninstall.sh [--prefix DIR] [--purge]
set -u
PREFIX="$HOME/eventlink-exporters"
PURGE=0
while [ $# -gt 0 ]; do
	case "$1" in
	--prefix) PREFIX=$2; shift ;;
	--purge) PURGE=1 ;;
	-h | --help) sed -n '2,4p' "$0"; exit 0 ;;
	*) echo "unknown option: $1"; exit 2 ;;
	esac
	shift
done
[ -d "$PREFIX" ] || { echo "nothing installed at $PREFIX"; exit 1; }
PREFIX=$(cd "$PREFIX" && pwd)
for b in directory-exporter eventlink-trace-exporter eventlink-db-exporter; do
	[ -x "$PREFIX/$b/${b}ctl.sh" ] && "$PREFIX/$b/${b}ctl.sh" stop
done
mark="# eventlink-exporters $PREFIX"
if command -v crontab >/dev/null 2>&1 && crontab -l 2>/dev/null | grep -qF "$mark"; then
	# Read the whole crontab first: other jobs of the user must stay.
	current=$(crontab -l | grep -vF "$mark")
	if [ -n "$current" ]; then
		printf '%s\n' "$current" | crontab -
	else
		crontab -r
	fi
	echo "cron lines removed"
fi
if [ "$PURGE" = 1 ]; then
	rm -rf "$PREFIX" && echo "removed $PREFIX"
else
	echo "stopped; files kept in $PREFIX (--purge deletes them)"
fi
echo "Remove the jobs from prometheus.yml and the systemd units (if installed) by hand."
