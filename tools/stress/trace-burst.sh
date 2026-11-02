#!/bin/bash
# Burst test for eventlink-trace-exporter: read the whole trace folder at
# startup (backfill), as if 1 GB of trace lines arrived at once.
#   trace-burst.sh <name> <max_read_mb> <max_names> <seconds>
set -u
NAME=$1 MB=$2 NAMES=$3 DUR=$4
R=/tmp/dexp-next/stress
OUT=$R/$NAME
mkdir -p "$OUT" && cd "$OUT"
cat > trace.yml <<EOF
listen_addr: ":9205"
tracelog_dir: /comptel/elink/install/EventLink/base/tracelog
poll_interval: "10s"
backfill: true
max_read_mb: $MB
max_names: $NAMES
seed_days: 0
log_file: "$OUT/trace.log"
EOF
GOMAXPROCS=2 GOMEMLIMIT=384MiB MAX_RSS_MB=${MAXRSS:-768} CONFIG_FILE=./trace.yml nohup nice -n 10 ionice -c 2 -n 7 $R/bin/eventlink-trace-exporter > stdout 2>&1 < /dev/null &
PID=$!
HZ=$(getconf CLK_TCK)
echo "t,rss_kb,cpu_ticks,read_bytes" > samples.csv
t0=$(date +%s); done_t=""
while [ $(( $(date +%s) - t0 )) -lt "$DUR" ] && kill -0 $PID 2>/dev/null; do
  t=$(( $(date +%s) - t0 ))
  read -r _ _ _ _ _ _ _ _ _ _ _ _ _ ut st _ < /proc/$PID/stat
  rss=$(awk '/VmRSS/ {print $2}' /proc/$PID/status)
  rb=$(curl -s -m 2 localhost:9205/metrics | awk '/^directory_tracelog_read_bytes_total/ {print $2}')
  echo "$t,$rss,$((ut + st)),${rb:-0}" >> samples.csv
  sleep 1
done
curl -s -o metrics.txt -w "%{time_total}" localhost:9205/metrics > scrape_time.txt
kill $PID; wait $PID 2>/dev/null
{
  echo "== $NAME  max_read_mb=$MB  max_names=$NAMES"
  awk -F, 'NR>1 && $4>0 {last=$4} END {printf "read=%.0fMB\n", last/1048576}' samples.csv
  awk -F, -v tot="$(awk -F, 'NR>1 {l=$4} END {print l}' samples.csv)" 'NR>1 && $4==tot && !d {print "all read after " $1 " s"; d=1}' samples.csv
  awk -F, -v hz="$HZ" 'NR==2 {c0=$3; t0=$1} NR>1 {if ($2>m) m=$2; c=$3; t=$1; r=$2} END {printf "rss_max=%.0fMB rss_end=%.0fMB cpu_avg=%.0f%% of one core over %ds\n", m/1024, r/1024, 100*(c-c0)/hz/(t-t0), t-t0}' samples.csv
  awk -F, -v hz="$HZ" 'NR>2 {d=$3-p; if (d>m) m=d} NR>1 {p=$3} END {printf "cpu_peak_1s=%.0f%% of one core\n", 100*m/hz}' samples.csv
  grep -E '^(directory_tracelog_lines_total|directory_tracelog_read_budget_hits_total|eventlink_tracked_names|eventlink_pending_evicted_total)' metrics.txt
  echo "metrics_after_burst=$(cat scrape_time.txt)s"
  grep -c '"level":"\(WARN\|ERROR\)"' trace.log | sed 's/^/warnings=/'
} | tee summary.txt
