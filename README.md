# ngAX — Image to WebP proxy

ngAX is a small Go service that sits in front of an image origin, fetches the
requested image over HTTPS, converts it to WebP with [libvips](https://www.libvips.org/)
(via [bimg](https://github.com/h2non/bimg)), and caches the result in memory.

The incoming `Host` header selects the upstream origin through the
`allowed_hosts` map in `config.yaml`. Requests from hosts that are not listed
are rejected with `403`.

## Requirements

- Go 1.24 or newer (or just Docker: everything runs via `make`)
- libvips 8.x with development headers (`vips-dev` on Alpine, `libvips-dev` on Debian/Ubuntu)
- Docker (optional, for containerised builds)

## Configuration

Copy `config.yaml.sample` to `config.yaml` and adjust it. The file is read from
the working directory at start-up.

Two keys from older versions are deprecated: `cache.lru_cache` (superseded by
`cache.max_bytes`) and `concurrency.max_goroutines` (superseded by
`concurrency.max_conversions`). Both still decode, so an old `config.yaml`
keeps loading, but both are **ignored** and each logs a warning at start-up.

| Section       | Key                | Purpose                                                        |
|---------------|--------------------|----------------------------------------------------------------|
| `allowed_hosts` | `<host>: <origin>` | Map of accepted `Host` headers to the upstream origin host      |
| `webp`        | `quality`          | Default WebP quality (1-100)                                    |
| `webp`        | `lossless`         | Use lossless WebP encoding                                      |
| `cache`       | `cache_enabled`    | Enable the in-memory cache                                       |
| `cache`       | `nocache_header`   | Request header that bypasses the cache when set to `true`       |
| `cache`       | `max_bytes`        | Memory ceiling for cached images, in bytes (default 1 GiB)      |
| `cache`       | `negative_ttl_seconds` | How long origin 404s are remembered, in seconds; `-1` disables |
| `upstream_scheme` | —              | `https://` (default) or `http://` for local testing              |
| `limits`      | `max_image_bytes`  | Reject upstream images larger than this (default 20 MiB)        |
| `concurrency` | `max_conversions`  | Concurrent libvips conversions; `0` = number of CPUs             |
| `concurrency` | `max_fetches`      | Concurrent origin fetches                                        |
| `http_client` | `*`                | Timeouts for upstream fetches, in seconds                       |
| `http_server` | `bind_ip`, `port`  | Public listener                                                 |
| `http_server` | `cache_control`    | `Cache-Control` header value sent with every image response     |
| `log`         | `level`            | Log verbosity: `debug`, `info`, `warn`, or `error`               |
| `exporter`    | `bind_ip`, `port`, `user`, `password` | Prometheus `/metrics` listener, protected by basic auth |

## Running

```bash
cp config.yaml.sample config.yaml
go run .
```

Or with Docker Compose, which pulls the published image:

```bash
docker compose up
```

## Endpoints

| Listener            | Path        | Description                                          |
|---------------------|-------------|------------------------------------------------------|
| `http_server`       | `/<path>`   | Fetches `https://<origin>/<path>` and returns WebP    |
| `http_server`       | `/health`   | Returns `OK`                                         |
| `exporter`          | `/metrics`  | Prometheus metrics (basic auth)                      |
| `localhost:6060`    | `/debug/pprof/` | Go profiling, local machine only                 |

Per-request options:

- `x-webp-quality: <1-100>` overrides the configured quality for that request.
- `<nocache_header>: true` bypasses the cache for that request.

Example:

```bash
curl -H 'Host: example.com' http://localhost:8080/images/photo.jpg -o photo.webp
```

## Development

Everything runs in the Docker dev image, so no local Go or libvips is needed:

```bash
make test     # go test ./...
make vet      # gofmt -l . && go vet ./...
make bench    # micro-benchmarks (see docs/loadtest.md for the baseline)
make sh       # shell in the dev image
```

`stress-test/test.go` is a standalone load generator; edit the constants at the
top before running it with `go run ./stress-test`.

## Building

```bash
go build -o ngax .
docker build -t ngax .
```

Pushes to `master` and `v*` tags are built and published to
`ghcr.io/mohammad362/ngax/ngax` by GitHub Actions.

## Performance notes

- A cache miss for a given image and quality triggers exactly one origin fetch
  and one conversion, no matter how many clients are waiting (request coalescing).
- The cache is bounded by `cache.max_bytes`. Run the container with
  `GOMEMLIMIT` at roughly 1.5× that value.
- Conversions are CPU-bound and limited to `concurrency.max_conversions`
  (default: number of CPUs). libvips runs single-threaded per operation
  (`VIPS_CONCURRENCY=1`); parallelism comes from concurrent requests.
- Responses carry `ETag` and `Cache-Control`; `If-None-Match` gets a `304`.
- Origin 404s are remembered for `cache.negative_ttl_seconds`.
- Profiles: `go tool pprof http://localhost:6060/debug/pprof/profile?seconds=30`
  (see `docs/loadtest.md`).
