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
  max_bytes: 134217728
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
# At ~93 KiB of WebP per image these 1000 fit inside the 128 MiB cache, so the
# warm-up and steady-state phases stay a clean "one fetch per image" / 100%-hit
# measurement; the disjoint 1001..2000 set below then pushes the working set
# (~186 MiB) past the ceiling, which is what exercises eviction.
: > "$ROOT/loadtest/targets.txt"
for i in $(seq 1 1000); do
  echo "GET http://127.0.0.1:8080/img/$i.png" >> "$ROOT/loadtest/targets.txt"
  echo "Host: loadtest.local" >> "$ROOT/loadtest/targets.txt"
  echo "" >> "$ROOT/loadtest/targets.txt"
done

# A second, disjoint set of 1000 distinct images (never requested above) so
# the CPU profile can be captured during a miss-heavy (fetch+convert) phase
# instead of a fully-cached one.
: > "$ROOT/loadtest/targets-miss.txt"
for i in $(seq 1001 2000); do
  echo "GET http://127.0.0.1:8080/img/$i.png" >> "$ROOT/loadtest/targets-miss.txt"
  echo "Host: loadtest.local" >> "$ROOT/loadtest/targets-miss.txt"
  echo "" >> "$ROOT/loadtest/targets-miss.txt"
done

# A single target, hammered by many workers at once, so that one cold image is
# requested concurrently by hundreds of clients: the request-coalescing check.
{
  echo "GET http://127.0.0.1:8080/img/3001.png"
  echo "Host: loadtest.local"
  echo ""
} > "$ROOT/loadtest/targets-burst.txt"

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
  echo '== cache bytes (expect <= 134217728):'
  wget -qO- --header 'Authorization: Basic bHQ6bHQ=' http://127.0.0.1:9080/metrics | awk '/^ngax_cache_bytes /{print \$2}'
  echo '== RSS (KiB)'; grep VmRSS /proc/\$(pgrep -f '^/tmp/ngax\$')/status
  echo '== same-key burst: 200 workers on one cold image for 3s'
  wget -qO- http://127.0.0.1:9000/reset >/dev/null
  vegeta attack -targets=loadtest/targets-burst.txt -rate=0 -workers=200 -max-workers=200 -duration=3s | vegeta report | sed -n '1,8p'
  echo '== origin fetches during same-key burst (expect 1; 2 is also correct --'
  echo '   ristretto admits the entry asynchronously, so a request arriving after'
  echo '   the leader finished but before the Set is visible starts one more):'
  wget -qO- http://127.0.0.1:9000/count
  echo '== ngax_coalesced_requests_total after burst (expect > 0):'
  wget -qO- --header 'Authorization: Basic bHQ6bHQ=' http://127.0.0.1:9080/metrics | awk '/^ngax_coalesced_requests_total /{print \$2}'
  echo '== resetting origin counter for the profile phase'
  wget -qO- http://127.0.0.1:9000/reset >/dev/null
  echo '== 30s CPU profile during a miss-heavy phase (1000 new images) -> loadtest/cpu.pprof'
  vegeta attack -targets=loadtest/targets-miss.txt -rate=100 -duration=35s -max-workers=500 >/dev/null &
  VPID=\$!
  wget -qO loadtest/cpu.pprof 'http://127.0.0.1:6060/debug/pprof/profile?seconds=30'
  wait \$VPID
  echo '== origin fetches during profile phase (expect >= 1000; above 1000 once the'
  echo '   cache ceiling binds and evicted images have to be fetched again):'
  wget -qO- http://127.0.0.1:9000/count
  echo '== cache bytes after the ceiling phase (expect <= 134217728):'
  wget -qO- --header 'Authorization: Basic bHQ6bHQ=' http://127.0.0.1:9080/metrics | awk '/^ngax_cache_bytes /{print \$2}'
  echo '== RSS after the ceiling phase (KiB)'; grep VmRSS /proc/\$(pgrep -f '^/tmp/ngax\$')/status
  pkill -INT -f '^/tmp/ngax\$' || true; pkill -f '^/tmp/origin( |\$)' || true
"
rm -f "$ROOT/loadtest/config.yaml" "$ROOT/loadtest/targets.txt" "$ROOT/loadtest/targets-miss.txt" "$ROOT/loadtest/targets-burst.txt"
echo "Profile saved to loadtest/cpu.pprof; inspect with: make sh -> go tool pprof -top loadtest/cpu.pprof"
