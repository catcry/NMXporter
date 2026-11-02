#!/bin/bash
# Stress harness for directory-exporter (runs on the EventLink server, read-only on the data).
#   stress.sh <name> <dir> <max_stat_files> <seconds> [scan_interval]
# Starts a separate test instance on :9204 that watches only <dir>, samples its
# CPU / RSS / threads every second and /metrics latency every 5 s, then compares
# count / oldest / newest with ground truth from find. Results in $R/<name>/.
set -u
NAME=$1 DIR=$2 MSF=$3 DUR=$4 INT=${5:-30s}
R=/tmp/dexp-next/stress
BIN=${BIN:-/tmp/dexp-next/demo/directory-exporter/directory-exporter}
UNCH=${UNCH:-0}   # unchanged_rescan_interval (0 = off, as in 1.5.0)
OUT=$R/$NAME
mkdir -p "$OUT"
cat > "$OUT/targets.yml" <<EOF
listen_addr: ":9204"
scan_interval: "$INT"
scan_workers: 2
scan_timeout: "90s"
discovery_interval: "6h"
max_stat_files: $MSF
max_files_per_dir: 0
unchanged_rescan_interval: "$UNCH"
reload_secret: "stress-test-only"
targets:
  - base: $(dirname "$DIR")
    label: /stress
    dirs: [ "$(basename "$DIR")" ]
log_file: "$OUT/exporter.log"
EOF
cd "$OUT"
GOMAXPROCS=2 GOMEMLIMIT=128MiB MAX_RSS_MB=256 CONFIG_FILE=./targets.yml nohup nice -n 10 ionice -c 2 -n 7 "$BIN" > "$OUT/stdout" 2>&1 < /dev/null &
PID=$!
echo "pid $PID"
HZ=$(getconf CLK_TCK)
echo "t,rss_kb,cpu_ticks,threads" > samples.csv
echo "t,metrics_ms,bytes" > scrapes.csv
t0=$(date +%s)
while [ $(( $(date +%s) - t0 )) -lt "$DUR" ] && kill -0 $PID 2>/dev/null; do
  t=$(( $(date +%s) - t0 ))
  read -r _ _ _ _ _ _ _ _ _ _ _ _ _ ut st _ < /proc/$PID/stat
  rss=$(awk '/VmRSS/ {print $2}' /proc/$PID/status)
  thr=$(awk '/Threads/ {print $2}' /proc/$PID/status)
  echo "$t,$rss,$((ut + st)),$thr" >> samples.csv
  if [ $((t % 5)) -eq 0 ]; then
    curl -s -o metrics.txt -w "$t,%{time_total},%{size_download}\n" localhost:9204/metrics >> scrapes.csv
  fi
  sleep 1
done
curl -s localhost:9204/metrics > metrics.txt
kill $PID; wait $PID 2>/dev/null

# Ground truth (all files, no sampling).
gt0=$(date +%s.%N)
find "$DIR/" -maxdepth 1 -type f -printf '%T@\n' | sort -n | awk 'NR==1 {o=$1} {n=$1; c++} END {printf "count=%d oldest=%d newest=%d\n", c, o, n}' > truth.txt
echo "find_seconds=$(echo "$(date +%s.%N) - $gt0" | bc)" >> truth.txt

{
  echo "== $NAME  dir=$DIR  max_stat_files=$MSF  scan_interval=$INT  unchanged_rescan=$UNCH  bin=$BIN"
  cat truth.txt
  grep -E '^directory_(file_count|oldest_file_timestamp_seconds|newest_file_timestamp_seconds|queue_oldest_collected_timestamp_seconds|scrape_duration_seconds)\{' metrics.txt | sed 's/{.*}//'
  grep '"scan cycle complete"' exporter.log | sed 's/.*"duration":\([0-9]*\).*/\1/' | awk '{s+=$1; if ($1>m) m=$1; n++} END {printf "scan cycles=%d avg=%.2fs max=%.2fs\n", n, s/n/1e9, m/1e9}'
  awk -F, -v hz="$HZ" 'NR==2 {c0=$3; t0=$1} NR>1 {if ($2>m) m=$2; c=$3; t=$1} END {printf "rss_max=%.1fMB cpu_avg=%.1f%% of one core over %ds\n", m/1024, 100*(c-c0)/hz/(t-t0), t-t0}' samples.csv
  awk -F, -v hz="$HZ" 'NR>2 {d=$3-p; if (d>m) m=d} NR>1 {p=$3} END {printf "cpu_peak_1s=%.0f%% of one core\n", 100*m/hz}' samples.csv
  awk -F, 'NR>1 {if ($2>m) m=$2; s+=$2; n++} END {printf "metrics: scrapes=%d avg=%.1fms max=%.1fms\n", n, 1000*s/n, 1000*m}' scrapes.csv
  grep -c '"level":"WARN"' exporter.log | sed 's/^/warnings=/'
} | tee summary.txt
