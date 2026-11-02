#!/bin/bash
# Stress / failure tests for eventlink-db-exporter (read-only on the database).
#   db-stress.sh <name> <seed_days> <query_timeout> <seed_timeout> <port> <seconds>
set -u
NAME=$1 DAYS=$2 QT=$3 ST=$4 PORT=$5 DUR=$6
R=/tmp/dexp-next/stress
OUT=$R/$NAME
mkdir -p "$OUT" && cd "$OUT"
cat > db.yml <<EOF
listen_addr: ":9206"
poll_interval: "15s"
query_timeout: "$QT"
seed_timeout: "$ST"
seed_days: $DAYS
databases:
  - name: ccelink
    eventlink_parameters: /comptel/elink/install/meta/EVENTLINK_DB/parameters.txt
    port: $PORT
log_file: "$OUT/db.log"
EOF
GOMAXPROCS=1 GOMEMLIMIT=64MiB MAX_RSS_MB=128 CONFIG_FILE=./db.yml nohup nice -n 10 ionice -c 2 -n 7 $R/bin/eventlink-db-exporter > stdout 2>&1 < /dev/null &
PID=$!
HZ=$(getconf CLK_TCK)
echo "t,rss_kb,cpu_ticks" > samples.csv
t0=$(date +%s)
while [ $(( $(date +%s) - t0 )) -lt "$DUR" ] && kill -0 $PID 2>/dev/null; do
  read -r _ _ _ _ _ _ _ _ _ _ _ _ _ ut st _ < /proc/$PID/stat
  echo "$(( $(date +%s) - t0 )),$(awk '/VmRSS/ {print $2}' /proc/$PID/status),$((ut + st))" >> samples.csv
  sleep 1
done
curl -s -o metrics.txt -w "%{time_total}" localhost:9206/metrics > scrape_time.txt
alive=$(kill -0 $PID 2>/dev/null && echo yes || echo NO)
kill $PID; wait $PID 2>/dev/null
{
  echo "== $NAME  seed_days=$DAYS query_timeout=$QT seed_timeout=$ST port=$PORT  alive_at_end=$alive"
  grep -E '^eventlink_db_(up|poll_duration_seconds|query_errors_total)' metrics.txt
  echo "nodes=$(grep -c '^eventlink_db_node_status' metrics.txt) streams_last_collected=$(grep -c '^eventlink_db_stream_last_collected' metrics.txt)"
  grep -o '"msg":"startup seed finished"[^}]*' db.log | head -1
  grep '"query failed"' db.log | sed 's/.*"query":"\([^"]*\)","error":"\([^"]\{0,90\}\).*/\1: \2/' | sort | uniq -c | head -4
  awk -F, -v hz="$HZ" 'NR==2 {c0=$3; t0=$1} NR>1 {if ($2>m) m=$2; c=$3; t=$1} END {printf "rss_max=%.1fMB cpu_avg=%.1f%% of one core over %ds\n", m/1024, 100*(c-c0)/hz/(t-t0), t-t0}' samples.csv
  echo "metrics=$(cat scrape_time.txt)s"
} | tee summary.txt
