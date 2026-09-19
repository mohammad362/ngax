#!/usr/bin/env sh
# Runs ngAX + synthetic origin inside the dev container and drives them with vegeta.
# Usage: make image && sh loadtest/run.sh [rate] [duration]
set -eu
RATE=${1:-2000}
DURATION=${2:-60s}
ROOT=$(cd "$(dirname "$0")/.." && pwd)

cat > "$ROOT/loadtest/config.yaml" <<EOF
upstream_scheme: "http://"
allowed_hosts:
  loadtest.local: "127.0.0.1:9000"
cache:
  cache_enabled: true
  max_bytes: 268435456
  negative_ttl_seconds: 30
http_server:
  bind_ip: "127.0.0.1"
  port: 8080
exporter:
  bind_ip: "127.0.0.1"
  port: 9080
  user: lt
  password: lt
log:
  level: warn
EOF

# 1000 distinct images, repeated so ~90% of requests are cache hits after warm-up.
: > "$ROOT/loadtest/targets.txt"
for i in $(seq 1 1000); do
  echo "GET http://127.0.0.1:8080/img/$i.png" >> "$ROOT/loadtest/targets.txt"
  echo "Host: loadtest.local" >> "$ROOT/loadtest/targets.txt"
  echo "" >> "$ROOT/loadtest/targets.txt"
done

docker run --rm -v "$ROOT":/app -v ngax-gocache:/root/.cache -v ngax-gomod:/go/pkg/mod -w /app ngax-dev sh -c "
  set -e
  go install github.com/tsenart/vegeta/v12@latest >/dev/null
  go build -buildvcs=false -o /tmp/origin ./loadtest/origin
  go build -buildvcs=false -o /tmp/ngax .
  /tmp/origin -addr 127.0.0.1:9000 -size 512 >/tmp/origin.log 2>&1 &
  (cd loadtest && GOMEMLIMIT=400MiB /tmp/ngax >/tmp/ngax.log 2>&1 &)
  sleep 2
  echo '== warm-up (every image once)'
  vegeta attack -targets=loadtest/targets.txt -rate=100 -duration=15s -max-workers=100 | vegeta report | sed -n '1,8p'
  echo '== origin fetches after warm-up (expect 1000):'; wget -qO- http://127.0.0.1:9000/count
  echo '== steady state at ${RATE} rps for ${DURATION}'
  vegeta attack -targets=loadtest/targets.txt -rate=${RATE} -duration=${DURATION} -max-workers=500 | tee /tmp/results.bin | vegeta report
  echo '== origin fetches total (expect still 1000):'; wget -qO- http://127.0.0.1:9000/count
  echo '== ngax metrics'
  wget -qO- --header 'Authorization: Basic bHQ6bHQ=' http://127.0.0.1:9080/metrics | grep -E '^ngax_(cache_hits_total|cache_misses_total|coalesced_requests_total|cache_bytes|http_errors_total) '
  echo '== RSS (KiB)'; grep VmRSS /proc/\$(pgrep -f '^/tmp/ngax\$')/status
  echo '== 30s CPU profile -> loadtest/cpu.pprof'
  vegeta attack -targets=loadtest/targets.txt -rate=${RATE} -duration=35s -max-workers=500 >/dev/null &
  VPID=\$!
  wget -qO loadtest/cpu.pprof 'http://127.0.0.1:6060/debug/pprof/profile?seconds=30'
  wait \$VPID
  pkill -INT -f '^/tmp/ngax\$' || true; pkill -f '^/tmp/origin' || true
"
rm -f "$ROOT/loadtest/config.yaml" "$ROOT/loadtest/targets.txt"
echo "Profile saved to loadtest/cpu.pprof; inspect with: make sh -> go tool pprof -top loadtest/cpu.pprof"
