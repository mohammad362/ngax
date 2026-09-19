# ngAX High-Performance Rewrite — Design Spec

**Date:** 2026-09-19
**Status:** Proposed

## Problem

ngAX converts origin images to WebP on request. It serves thousands of requests
per second. The current single-file implementation (`main.go`, ~400 lines) has
structural limits that hurt throughput, latency and memory under that load:

| Area | Current behaviour | Cost under load |
|------|-------------------|-----------------|
| Cache stampede | N concurrent misses for one URL → N origin fetches and N libvips conversions | CPU and origin bandwidth multiplied by concurrency on every popular new image |
| Cache bounds | LRU bounded by entry *count* (`lru_cache: 300000`), not bytes | Unbounded RSS; OOM kill or swap under real traffic |
| Cache key | URL only; ignores `x-webp-quality` | Wrong-quality images served from cache |
| Concurrency | One semaphore covers both network fetch and CPU conversion | I/O wait holds CPU slots; conversions under-utilise cores or oversubscribe them |
| libvips threading | Default vips concurrency (all cores) per operation × N goroutines | Thread oversubscription, cache thrash |
| Upstream connections | `MaxIdleConnsPerHost` left at Go default of 2 | Connection churn and TLS handshakes on nearly every fetch to the single origin |
| Negative results | Origin 404 fetched again on every request | Origin hammered for missing images |
| Client caching | No `Cache-Control`, no `ETag` / `304` | Every browser and CDN re-downloads full bodies |
| Buffers | `io.ReadAll` per request, GC churn | Allocation pressure, GC pauses |
| Logging | Several JSON Info logs per request to stdout | Synchronous stdout writes on the hot path |
| Router | gorilla/mux regex catch-all | Small but avoidable per-request cost; extra dependency |
| Server timeouts | None (`ReadHeaderTimeout`, `IdleTimeout` unset) | Slowloris exposure, idle connection pile-up |

## Goals

1. **Single origin fetch and single conversion per unique (URL, quality)**, no
   matter how many concurrent clients ask for it.
2. **Byte-bounded cache** with a configured memory ceiling and good hit ratio.
3. **CPU-bound work sized to cores**, I/O-bound work sized independently.
4. **Cheap hot path**: a cache hit does one map lookup, one header compare, one
   write. No allocation beyond the response.
5. **Fewer requests reaching the service**: `Cache-Control`, `ETag`, `304`,
   short negative caching for origin 404s.
6. **Operable**: pprof, Prometheus, load-test runbook, memory limit guidance.

## Non-goals

- Resizing, cropping or format negotiation beyond WebP.
- Disk-backed or distributed cache.
- Changing the `Host`-header-to-origin routing model or the config file format
  beyond adding keys with safe defaults.

## Architecture

The single `main.go` is split into focused files in `package main`:

| File | Responsibility |
|------|----------------|
| `config.go` | `Config` struct, `loadConfig(dir)`, `applyDefaults()`, `validate()`, `newHTTPClient()` |
| `metrics.go` | Prometheus collectors and registration |
| `cache.go` | `ImageCache`: byte-cost bounded positive cache and TTL negative cache (ristretto v2) |
| `fetcher.go` | `Fetcher`: bounded-concurrency origin fetch with size limit and pooled buffers |
| `converter.go` | `Converter`: bounded-concurrency libvips WebP conversion |
| `handler.go` | `Handler`: host mapping, cache lookup, singleflight, conditional responses |
| `server.go` | Public/metrics/pprof `http.Server` construction and graceful shutdown |
| `main.go` | Wiring only |

### Request flow

```
GET /a.jpg  Host: example.com  x-webp-quality: 60  If-None-Match: "abc"
  │
  ├─ method not GET/HEAD ──────────────────────────────► 405
  ├─ Host not in allowed_hosts ────────────────────────► 403
  │
  ├─ key = https://origin/a.jpg | q=60
  ├─ cache.Get(key) hit ─► ETag matches? ─► 304
  │                        else ─────────► 200 body + ETag + Cache-Control
  ├─ cache.GetNegative(key) hit ──────────────────────► 404
  │
  └─ singleflight.Do(key):
        fetcher.Fetch(url)      (semaphore: max_fetches, I/O bound)
        converter.ToWebP(body)  (semaphore: max_conversions = NumCPU, CPU bound)
        cache.Set(key, entry)   (cost = len(bytes))
     followers of the same key receive the same entry; one fetch, one convert.
```

### Key decisions

- **ristretto v2** for the cache: sharded, byte-cost eviction, TinyLFU
  admission (keeps hot items under scan traffic), TTL support for negative
  entries. Sets are asynchronous; callers that must observe a set call `Wait()`
  (tests only).
- **golang.org/x/sync/singleflight** for miss coalescing. The producer runs
  under a detached context with the configured client timeout so one client
  disconnecting does not cancel the work for the others.
- **libvips concurrency = 1** per operation (`VIPS_CONCURRENCY=1`, set before
  `bimg` initialises) with a goroutine pool of `NumCPU` conversions. Parallelism
  comes from concurrent requests, not from nested vips threads. The vips
  operation cache is disabled (`VipsCacheSetMax(0)`): a proxy sees mostly
  unique inputs, so the cache only costs memory.
- **`http.ServeMux`** (Go 1.22 method patterns) replaces gorilla/mux.
- **Pooled `bytes.Buffer`** for origin bodies, sized from `Content-Length` when
  present; buffers above 4 MiB are not returned to the pool.
- **ETag** is the xxhash64 of the WebP bytes, computed once at cache insert.
- **Per-request logging removed**; metrics carry the signal. Log level is
  configurable; default `info` logs start-up, shutdown and errors only.

### Configuration additions (all optional, with defaults)

```yaml
cache:
  max_bytes: 1073741824          # cache memory ceiling (default 1 GiB); replaces lru_cache
  negative_ttl_seconds: 30       # remember origin 404s for this long (-1 disables)
concurrency:
  max_conversions: 0             # 0 = number of CPUs
  max_fetches: 512               # concurrent origin fetches
http_client:
  max_idle_conns_per_host: 256
  idle_conn_timeout: 90
http_server:
  read_header_timeout_seconds: 5
  idle_timeout_seconds: 60
  cache_control: "public, max-age=31536000, immutable"
log:
  level: info                    # debug | info | warn | error
```

`cache.lru_cache` is still read and ignored with a start-up warning.

### Deployment guidance

- Run with `GOMEMLIMIT` ≈ `cache.max_bytes` × 1.5 so the Go runtime collects
  before the container limit is hit.
- Multi-stage Docker image: build in `golang:alpine` with `vips-dev`, run in
  `alpine` with `vips` only, non-root, `-ldflags="-s -w"`, `VIPS_CONCURRENCY=1`.
- pprof stays on `localhost:6060`.

## Acceptance criteria

Measured with the load-test runbook in the plan (vegeta against a local origin
that serves generated PNGs), on the deployment machine:

| Scenario | Target |
|----------|--------|
| 100% cache hit, 5000 rps, 60 s | p99 latency < 5 ms, 0 non-2xx/304, RSS flat |
| 1000 distinct images, 100 concurrent clients each requesting the same new image | origin sees exactly 1 fetch per image (`ngax_http_requests_total` vs origin counter) |
| Cache filled past `max_bytes` | RSS stabilises below `GOMEMLIMIT`; no OOM |
| Mixed 90/10 hit/miss at 2000 rps | CPU saturates on conversion goroutines only (pprof shows vips in `max_conversions` threads), no goroutine pile-up |

Existing tests continue to pass or are migrated to the new files; new
behaviour is covered by tests written first.
