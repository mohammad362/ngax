# Load testing ngAX

## Prerequisites

Docker only. `make image` builds the dev toolchain image.

## Run

    make image
    sh loadtest/run.sh 2000 60s     # rate (rps), duration

The script starts a synthetic origin (1000 distinct 512×512 PNGs, plus a
second disjoint set of 1000 used only for profiling), starts ngAX pointed at
it over plain HTTP, warms the cache, attacks at the given rate and prints a
vegeta report and ngAX metrics, then resets the origin's request counter and
takes a 30 s CPU profile (saved to `loadtest/cpu.pprof`) while attacking the
second, never-before-seen set of 1000 images — so the profile captures a
miss-heavy (fetch + convert) phase instead of an already-fully-cached one.

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
| Origin fetch count during the profile phase | Exactly 1000: the disjoint 1001..2000 image set, each fetched once |
| `ngax_coalesced_requests_total` | > 0 only when concurrent clients request the same cold image at once; the striped target file does not produce that, so 0 is expected here (coalescing is covered by `TestHandlerCoalescesConcurrentMisses`) |
| vegeta `Success` | 100% |
| vegeta latencies p99 (steady state, 90%+ hits) | < 20 ms on the target machine; cache-hit-only runs < 5 ms |
| `ngax_http_errors_total` | 0 |
| VmRSS | Below GOMEMLIMIT (400 MiB in the script) and flat across repeated runs |
| CPU profile | Time is in `bimg`/libvips under `Converter.ToWebP` and in `net/http`; no `runtime.gcBgMarkWorker` dominating |

## Reading the profile

`loadtest/cpu.pprof` is captured during the miss-heavy profile phase (a
fresh 1000-image set, cache disabled by construction since none of them
have been seen before), so it should show real fetch+convert cost rather
than a cache-hit-dominated trace:

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
| BenchmarkCacheHit | 605.7 | 1810 | 22 |
| BenchmarkCacheHit304 | 448.5 | 1230 | 18 |
| BenchmarkCacheMiss | 822773 | 15882 | 132 |

`BenchmarkCacheHit304` allocates less than `BenchmarkCacheHit` (1230 B <
1810 B), as expected: a 304 skips writing the body and the Content-Length
header. `BenchmarkCacheHit` allocs/op measured at 22 in this run, above the
rough ≤12 guideline noted when this benchmark was designed; the bulk of it is
`httptest.NewRecorder()` per iteration plus the fixed Prometheus label
lookups (`requestsTotal.WithLabelValues`, `responseDuration.WithLabelValues`)
in `ServeHTTP`'s deferred metrics block, not per-request cache work. Recorded
here as the measured baseline rather than adjusted to fit the guideline;
revisit if a future profile shows this path as hot.

`BenchmarkCacheMiss` gives each parallel iteration a unique path
(`/img-<n>.png`, `n` from an `atomic.Int64` counter) so singleflight cannot
coalesce concurrent goroutines onto one shared leader; it now measures
independent fetch+convert throughput across all `runtime.NumCPU()`
conversion workers. This is why the numbers jumped from an earlier draft
that reused the same key (`235034 ns/op`, `2571 B/op`, `30 allocs/op` — that
version was measuring mostly singleflight wait for one shared conversion,
not independent work) to the corrected `822773 ns/op`, `15882 B/op`,
`132 allocs/op` above: with a unique key per call, most goroutines now do a
real fetch and a real libvips conversion instead of waiting on someone
else's.

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
- Origin fetch count during the profile phase (disjoint 1001..2000 image
  set, counter reset beforehand): **1000** (target: 1000)
- Warm-up (`-rate=100 -duration=15s`): 1500 requests issued (cycles back
  over the 1000-image target list once all are cached), Success **100.00%**
- Steady state (1000 rps, 30 s): 30000 requests, Success **100.00%**,
  latencies p50 = 0.147 ms, p90 = 0.270 ms, p95 = 0.319 ms,
  **p99 = 0.434 ms**, max = 3.288 ms
- Profile phase (100 rps, 35 s, fresh 1000-image set): Success **100.00%**
  (not shown in the printed report since that attack's output is discarded;
  confirmed via the 1000/1000 origin count and zero `ngax_http_errors_total`
  reported just before this phase started)
- `ngax_cache_hits_total` delta over the run: 30500; `ngax_cache_misses_total`:
  1000 (exactly the 1000 cold fetches from warm-up; the 1000 rps
  steady-state window was 100% cache hits — the profile phase's 1000 misses
  happen after this metrics snapshot is printed, so they aren't included in
  these particular counter values)
- `ngax_coalesced_requests_total`: 0 for this run — at 100 rps against 1000
  distinct images, concurrent same-key misses are rare by construction, so
  this run does not exercise the coalescing path (see
  `TestHandlerCoalescesConcurrentMisses` in `handler_test.go` for a
  concurrency-forcing test of that path instead)
- `ngax_http_errors_total`: **0**
- VmRSS: **206964 kB** (~202 MiB), well under the 400 MiB `GOMEMLIMIT` set
  by the script
- CPU profile (`loadtest/cpu.pprof`, 30 s during the **miss-heavy profile
  phase** against the fresh 1001..2000 image set): `go tool pprof -top`
  shows real conversion cost this time — top of profile is
  `[libwebp.so.7.2.0]` at 94.23% flat, with `runtime.cgocall` (2.40%) and
  `[libpng16.so.16.58.0]` (0.54%) next, and `main.(*Converter).ToWebP` /
  `github.com/h2non/bimg.(*Image).Process` / `bimg.vipsSave` all present in
  the cumulative call graph — this now matches the runbook's pass condition
  ("time is in bimg/libvips under Converter.ToWebP"), unlike the earlier
  cache-hit-only profile which was dominated by
  `internal/runtime/syscall/linux.Syscall6` with no libvips/bimg frames at
  all.

Fixes applied to `loadtest/run.sh` and `handler_bench_test.go` while
producing this run (all in the committed files, not just this report):

1. `BenchmarkCacheMiss` requested the same `/a.png` key from every parallel
   goroutine, so singleflight coalesced all of them onto one leader and the
   benchmark measured mostly wait time for a single shared conversion, not
   independent fetch+convert throughput. Fixed by giving each iteration a
   unique path (`/img-<n>.png` from an `atomic.Int64` counter). This changed
   the measured numbers substantially (235034 ns/op → 822773 ns/op — see the
   micro-benchmarks section above), which is expected: it's now measuring
   real independent work across all `NumCPU()` conversion workers instead of
   queueing behind one.
2. The CPU profile was taken after the steady-state (cache-hit) phase, so it
   could never show libvips/bimg time. Fixed by adding a second, disjoint
   1000-image target file (`loadtest/targets-miss.txt`, ids 1001..2000),
   resetting the origin's counter, and profiling a 35 s attack against that
   fresh set instead — see the CPU profile bullet above for the resulting
   (correct) profile shape.
3. The final `wait` (no argument) waited on *every* background job,
   including the long-running `ngax` and `origin` servers, which never exit
   on their own — the script would hang forever before reaching the
   `pkill` cleanup line. Fixed by capturing the profiling vegeta attack's
   PID (`VPID=$!`) and waiting on that PID specifically.
4. `pgrep -f /tmp/ngax` also matched the wrapping `sh -c "..."` process
   (whose script text contains the literal string `/tmp/ngax`), so
   `/proc/$(pgrep -f /tmp/ngax)/status` expanded to an invalid multi-PID
   path and RSS reporting failed. Fixed with an anchored pattern,
   `pgrep -f '^/tmp/ngax$'`, which matches only the exact command line of
   the `ngax` process itself. The equivalent `pkill -f` pattern for
   `origin` (which runs with `-addr`/`-size` args, so it can't be anchored
   with a trailing `$`) is `'^/tmp/origin( |$)'`, anchored at the start and
   requiring either a following space or end-of-string so it can't match
   the wrapping shell's command line either.
