# Load testing ngAX

## Prerequisites

Docker only. `make image` builds the dev toolchain image.

## Run

    make image
    sh loadtest/run.sh 1000 30s     # rate (rps), duration

The script runs four phases against a synthetic origin (1000 distinct
512x512 PNGs, a second disjoint set of 1000, and one further image used only
for the burst) with ngAX pointed at it over plain HTTP:

1. **Warm-up** — every one of the 1000 images once, `-rate=100 -duration=15s`.
   Each request is a cold fetch + convert, so the origin must end on exactly
   1000.
2. **Steady state** — the same 1000 images at the requested rate/duration.
   After the warm-up they are all cached, so this phase is a ~100% cache-hit
   latency measurement and the origin counter must not move.
3. **Same-key burst** — 200 workers with `-rate=0` (as fast as they can) for
   3 s against a single never-before-seen image, with the origin counter
   reset first. This is the request-coalescing check: hundreds of concurrent
   clients, one origin fetch.
4. **Cache ceiling + CPU profile** — the disjoint 1001..2000 set at
   `-rate=100 -duration=35s`, with a 30 s CPU profile captured into
   `loadtest/cpu.pprof`. Every image here is new, so the profile captures a
   miss-heavy (fetch + convert) phase, and the extra 1000 images push the
   working set past the cache ceiling.

The generated `loadtest/config.yaml` sets `cache.max_bytes` to **128 MiB**
(`134217728`) and `GOMEMLIMIT=400MiB`. At ~93 KiB of WebP per image the 1000
warm images are ~93 MiB and fit, which keeps phases 1-3 a clean "one fetch
per image" / 100%-hit measurement; the full 2000-image working set is
~186 MiB and does not fit, so phase 4 is where eviction happens and
`ngax_cache_bytes` gets pinned at the ceiling. Sizing the cache below the
warm set instead (e.g. 32 MiB) makes *every* phase miss-heavy and destroys
the 100%-cache-hit latency evidence, which is why the ceiling is set to bind
on the 2000-image set rather than the 1000-image one.

The warm-up rate is tuned to this repo's 12-CPU dev/CI hardware:
`-rate=100 -duration=15s`. Converting a fresh 512x512 PNG through libvips
takes tens of ms even with `concurrency.max_conversions` at `NumCPU()`, so
the originally-drafted `-rate=200 -duration=5s` warm-up could not finish
converting all 1000 images within 5 s (it landed around 700-750/1000) — this
is a genuine compute ceiling, not a bug, so per the runbook's own rule the
fix is to lower/slow the warm-up rate rather than weaken the "exactly 1000"
acceptance check. If you run on a smaller machine, lower `-rate` and/or
raise `-duration` further in `loadtest/run.sh`.

## What to check

| Output | Pass condition |
|--------|----------------|
| Origin fetch count after warm-up | Exactly 1000: one fetch per image, none repeated |
| Origin fetch count after steady state | Still exactly 1000: the steady-state window fetched nothing |
| `ngax_cache_misses_total` after steady state | 1000 — the warm-up's cold fetches and nothing else |
| vegeta `Success` | 100% in every phase |
| vegeta p99, steady state (100% hits) | < 5 ms |
| `ngax_http_errors_total` | 0 |
| `ngax_cache_bytes` after steady state | <= `cache.max_bytes` (134217728), and roughly the size of the 1000 cached images |
| VmRSS after steady state | Below `GOMEMLIMIT` (400 MiB in the script) and flat across repeated runs |
| **Burst:** origin fetch count during the same-key burst | **1** — hundreds of concurrent clients on one cold image collapse to a single origin fetch. **2 is also correct**: ristretto admits the entry asynchronously, so a request arriving after the leader returned but before the `Set` is visible starts one more fetch. Anything larger means coalescing is broken |
| **Burst:** `ngax_coalesced_requests_total` | **> 0**, and in practice ~`workers - 1` (199 of 200): every client but the leader waited on the in-flight fetch |
| **Ceiling:** origin fetch count during the ceiling/profile phase | **>= 1000**. Exactly 1000 would mean nothing was evicted; above 1000 is the eviction signature, i.e. images dropped to stay under `max_bytes` and were fetched again |
| **Ceiling:** `ngax_cache_bytes` after the ceiling phase | **<= 134217728**, and pressed right up against it — this is criterion 3: the cache is full and stays bounded |
| **Ceiling:** VmRSS after the ceiling phase | Still below `GOMEMLIMIT`, no OOM |
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

Command: `make image && sh loadtest/run.sh 1000 30s`, 2026-09-19. Rate chosen
modestly because the 12-core dev box also runs vegeta and the synthetic origin
inside the same `docker run`; see the CPU note above for why the warm-up rate
was lowered too.

**Phase 1 — warm-up** (`-rate=100 -duration=15s`)

- 1500 requests issued (the target list cycles once all 1000 are cached),
  Success **100.00%**, p99 88.9 ms (every request is a cold fetch+convert)
- Origin fetch count: **1000** (target: exactly 1000)

**Phase 2 — steady state** (1000 rps, 30 s, 100% cache hits)

- 30000 requests, Success **100.00%**, latencies p50 = 0.146 ms,
  p90 = 0.268 ms, p95 = 0.323 ms, **p99 = 0.433 ms**, max = 3.35 ms
  (target: p99 < 5 ms)
- Origin fetch count: **1000**, unchanged — zero duplicate origin fetches
- `ngax_cache_hits_total` 30500, `ngax_cache_misses_total` **1000** (exactly
  the warm-up's cold fetches), `ngax_http_errors_total` **0**
- `ngax_cache_bytes`: **93 437 030** (~89 MiB, under the 128 MiB ceiling — the
  1000 warm images fit, as designed)
- VmRSS: **213 272 kB** (~208 MiB), well under the 400 MiB `GOMEMLIMIT`

**Phase 3 — same-key burst** (200 workers, `-rate=0`, 3 s, one cold image,
origin counter reset first)

- 41323 requests in 3 s (**13 775 rps**), Success **100.00%**, p99 = 0.643 ms
- Origin fetch count: **2** (target: 1). The first request coalesced 199
  followers onto one fetch; the second fetch is the asynchronous-admission
  window described in the table above — after the leader returned, its
  singleflight entry was already gone while ristretto had not yet made the
  cached entry visible, so one later request missed and fetched again. Both
  runs of this phase produced exactly 2. It is not a coalescing failure:
  199/200 of the concurrent clients were served from the single in-flight
  fetch.
- `ngax_coalesced_requests_total`: **199** (target: > 0) — was 0 before this
  phase, so all 199 come from the burst

**Phase 4 — cache ceiling + CPU profile** (1000 fresh images 1001..2000,
100 rps, 35 s, origin counter reset first)

- Origin fetch count: **2283** (target: >= 1000). The 1283 fetches above 1000
  are the eviction signature: the 2000-image working set is ~186 MiB against a
  128 MiB ceiling, so images were evicted and re-fetched.
- `ngax_cache_bytes` after the phase: **134 217 338** against the
  `cache.max_bytes` ceiling of **134 217 728** — 390 bytes of headroom. The
  cache is completely full and stays bounded (criterion 3).
- VmRSS after the phase: **325 744 kB** (~318 MiB), still under the 400 MiB
  `GOMEMLIMIT`, no OOM
- CPU profile (`loadtest/cpu.pprof`, 30 s, 112.99 s of samples at 376% CPU):
  `go tool pprof -top` top line is **`106.52s 94.27% [libwebp.so.7.2.0]`**,
  then `runtime.cgocall` 2.64%, `[libpng16.so.16.58.0]` 0.91%, with
  `bimg.(*Image).Process` and `main.(*Converter).ToWebP` in the cumulative
  graph — matching the pass condition ("time is in bimg/libvips under
  `Converter.ToWebP`").

### Notes on earlier fixes to this harness

1. `BenchmarkCacheMiss` requested the same `/a.png` key from every parallel
   goroutine, so singleflight coalesced all of them onto one leader and the
   benchmark measured mostly wait time for a single shared conversion, not
   independent fetch+convert throughput. Fixed by giving each iteration a
   unique path (`/img-<n>.png` from an `atomic.Int64` counter). This changed
   the measured numbers substantially (235034 ns/op -> 822773 ns/op — see the
   micro-benchmarks section above), which is expected: it is now measuring
   real independent work across all `NumCPU()` conversion workers instead of
   queueing behind one.
2. The CPU profile was taken after the steady-state (cache-hit) phase, so it
   could never show libvips/bimg time. Fixed by adding a second, disjoint
   1000-image target file (`loadtest/targets-miss.txt`, ids 1001..2000),
   resetting the origin's counter, and profiling an attack against that fresh
   set instead.
3. The final `wait` (no argument) waited on *every* background job, including
   the long-running `ngax` and `origin` servers, which never exit on their
   own — the script would hang forever before reaching the `pkill` cleanup
   line. Fixed by capturing the profiling vegeta attack's PID (`VPID=$!`) and
   waiting on that PID specifically.
4. `pgrep -f /tmp/ngax` also matched the wrapping `sh -c "..."` process (whose
   script text contains the literal string `/tmp/ngax`), so
   `/proc/$(pgrep -f /tmp/ngax)/status` expanded to an invalid multi-PID path
   and RSS reporting failed. Fixed with an anchored pattern,
   `pgrep -f '^/tmp/ngax$'`. The equivalent `pkill -f` pattern for `origin`
   (which runs with `-addr`/`-size` args, so it cannot be anchored with a
   trailing `$`) is `'^/tmp/origin( |$)'`.
5. The cache was originally sized at 256 MiB, larger than the whole
   2000-image working set, so no phase ever exercised eviction. The ceiling
   is now 128 MiB, which the 1000 warm images fit inside and the full
   2000-image set does not — see the Run section for why it is not sized
   below the warm set.

## Follow-ups

Not blocking the merge; recorded here so they are not lost.

- **Hot-path allocations.** `BenchmarkCacheHit` sits at 22 allocs/op. Two easy
  wins: cache the `Content-Length` string on `Entry` next to the ETag instead
  of calling `strconv.Itoa(len(e.Data))` per response, and memoise the
  Prometheus `(status, method)` children so `ServeHTTP`'s deferred block stops
  doing two `WithLabelValues` map lookups per request.
- **Per-server shutdown contexts.** `runServers` shares one 10 s context
  across every `Shutdown`, so a slow public listener eats the budget of the
  metrics and pprof servers. Give each its own deadline and shut them down in
  parallel.
- **Separate conversion deadline.** `produce` runs fetch and convert under one
  `http_client.timeout_seconds + 5s` budget, so a slow origin can leave almost
  no time for libvips. Give the conversion its own deadline.
- **Pin the base images.** `Dockerfile`, `Dockerfile.dev` and the CI container
  all use floating `golang:alpine` / `alpine`; pin them (e.g. `alpine:3.20`)
  so a libvips or toolchain bump cannot silently change the numbers in this
  runbook.
- **No-cache requests do not skip coalescing.** A request carrying the
  `nocache_header` still joins the singleflight group for its key, so it can
  be served bytes produced by someone else's fetch. That is usually what you
  want, but it is not what "no cache" says; decide and document, or give
  bypass requests their own group key.
- **`ServeMux` 301 redirects are invisible to metrics.** The stdlib mux
  answers path-cleaning redirects (e.g. `/a//b.png` -> `/a/b.png`) itself,
  before the image handler runs, so those responses never reach
  `ngax_http_requests_total`. Wrap the mux if that traffic needs counting.
