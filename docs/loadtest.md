# Load testing ngAX

## Prerequisites

Docker only. `make image` builds the dev toolchain image.

## Run

    make image
    sh loadtest/run.sh 2000 60s     # rate (rps), duration

The script starts a synthetic origin (1000 distinct 512×512 PNGs), starts
ngAX pointed at it over plain HTTP, warms the cache, then attacks at the
given rate and prints a vegeta report, ngAX metrics, RSS and saves a 30 s CPU
profile to `loadtest/cpu.pprof`.

The warm-up phase issues 1000 first-time (cold) fetch+convert requests at a
rate tuned to this repo's 12-CPU dev/CI hardware: `-rate=100 -duration=15s`.
Converting a fresh 512×512 PNG through libvips takes tens of ms even with
`concurrency.max_conversions` at `NumCPU()`, so the originally-drafted
`-rate=200 -duration=5s` warm-up could not finish converting all 1000 images
within 5 s (it landed around 700-750/1000) — this is a genuine compute
ceiling, not a bug, so per the runbook's own rule the fix is to lower/slow
the warm-up rate rather than weaken the "exactly 1000" acceptance check. If
you run on a smaller machine, lower `-rate` and/or raise `-duration` further
in `loadtest/run.sh`.

## What to check

| Output | Pass condition |
|--------|----------------|
| Origin fetch count after warm-up and after steady state | Exactly 1000 both times: one fetch per image, none repeated |
| `ngax_coalesced_requests_total` | > 0 during warm-up (concurrent clients waited on in-flight work) |
| vegeta `Success` | 100% |
| vegeta latencies p99 (steady state, 90%+ hits) | < 20 ms on the target machine; cache-hit-only runs < 5 ms |
| `ngax_http_errors_total` | 0 |
| VmRSS | Below GOMEMLIMIT (400 MiB in the script) and flat across repeated runs |
| CPU profile | Time is in `bimg`/libvips under `Converter.ToWebP` and in `net/http`; no `runtime.gcBgMarkWorker` dominating |

## Reading the profile

    make sh
    go tool pprof -top loadtest/cpu.pprof | head -30

Goroutine dump while under load (look for pile-ups in `singleflight` or the
fetch/convert semaphores):

    wget -qO- 'http://127.0.0.1:6060/debug/pprof/goroutine?debug=1' | head -50

## Micro-benchmarks

    make bench

Baseline recorded on Docker dev image (`golang:alpine` + libvips), host CPU
`Intel(R) Core(TM) i7-10750H CPU @ 2.60GHz` (12 logical CPUs, `GOMAXPROCS=12`),
2026-09-19:

| Benchmark | ns/op | B/op | allocs/op |
|-----------|-------|------|-----------|
| BenchmarkCacheHit | 605.6 | 1810 | 22 |
| BenchmarkCacheHit304 | 457.6 | 1230 | 18 |
| BenchmarkCacheMiss | 235034 | 2571 | 30 |

`BenchmarkCacheHit304` allocates less than `BenchmarkCacheHit` (1230 B <
1810 B), as expected: a 304 skips writing the body and the Content-Length
header. `BenchmarkCacheHit` allocs/op measured at 22 in this run, above the
rough ≤12 guideline noted when this benchmark was designed; the bulk of it is
`httptest.NewRecorder()` per iteration plus the fixed Prometheus label
lookups (`requestsTotal.WithLabelValues`, `responseDuration.WithLabelValues`)
in `ServeHTTP`'s deferred metrics block, not per-request cache work. Recorded
here as the measured baseline rather than adjusted to fit the guideline;
revisit if a future profile shows this path as hot.

## Tuning knobs

- `concurrency.max_conversions`: raise only if CPU is idle while requests
  queue; the default (NumCPU) is right for a dedicated host.
- `concurrency.max_fetches`: raise if origin latency is high and misses queue
  while CPU is idle.
- `cache.max_bytes` and `GOMEMLIMIT`: keep GOMEMLIMIT ≈ 1.5× max_bytes.
- `http_client.max_idle_conns_per_host`: raise toward `max_fetches` if the
  origin shows many new connections.

## Last run

Command: `make image && sh loadtest/run.sh 1000 30s` (rate chosen modestly
because the 12-core dev box also runs vegeta and the synthetic origin inside
the same `docker run`; see the CPU note above for why the warm-up rate was
lowered too).

- Origin fetch count after warm-up: **1000** (target: 1000)
- Origin fetch count after steady state: **1000** (target: 1000, unchanged
  — confirms zero duplicate/repeated origin fetches)
- Warm-up (`-rate=100 -duration=15s`): 1500 requests issued (cycles back
  over the 1000-image target list once all are cached), Success 100.00%
- Steady state (1000 rps, 30 s): 30000 requests, Success **100.00%**,
  latencies p50 = 0.143 ms, p90 = 0.263 ms, p95 = 0.310 ms,
  **p99 = 0.418 ms**, max = 3.53 ms
- `ngax_cache_hits_total` delta over the run: 30500; `ngax_cache_misses_total`:
  1000 (exactly the 1000 cold fetches, all absorbed during warm-up; the
  1000 rps steady-state window was 100% cache hits)
- `ngax_coalesced_requests_total`: 0 for this run — at 100 rps against 1000
  distinct images, concurrent same-key misses are rare by construction, so
  this run does not exercise the coalescing path (see
  `TestHandlerCoalescesConcurrentMisses` in `handler_test.go` for a
  concurrency-forcing test of that path instead)
- `ngax_http_errors_total`: **0**
- VmRSS: **211424 kB** (~206 MiB), well under the 400 MiB `GOMEMLIMIT` set
  by the script
- CPU profile (`loadtest/cpu.pprof`, 30 s during the steady-state window):
  only 2.52 s of samples (8.4% busy) because that window was ~100% cache
  hits; top of profile is `internal/runtime/syscall/linux.Syscall6` (network
  I/O) under `main.(*Handler).ServeHTTP`, not libvips — expected, since no
  conversions ran during that window. Re-profile during a cold/miss-heavy
  window (e.g. immediately after `/reset` on the origin) to see the
  `bimg`/`Converter.ToWebP` cost.

Two script bugs found and fixed while producing this run (both are in the
committed `loadtest/run.sh`, not just this report):

1. The final `wait` (no argument) waited on *every* background job,
   including the long-running `ngax` and `origin` servers, which never exit
   on their own — the script would hang forever before reaching the
   `pkill` cleanup line. Fixed by capturing the profiling vegeta attack's
   PID (`VPID=$!`) and waiting on that PID specifically.
2. `pgrep -f /tmp/ngax` also matched the wrapping `sh -c "..."` process
   (whose script text contains the literal string `/tmp/ngax`), so
   `/proc/$(pgrep -f /tmp/ngax)/status` expanded to an invalid multi-PID
   path and RSS reporting failed. Fixed with an anchored pattern,
   `pgrep -f '^/tmp/ngax$'`, which matches only the exact command line of
   the `ngax` process itself.
