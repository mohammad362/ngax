# ngAX — Image to WebP proxy

ngAX is a small Go service that sits in front of an image origin, fetches the
requested image over HTTPS, converts it to WebP with [libvips](https://www.libvips.org/)
(via [bimg](https://github.com/h2non/bimg)), and caches the result in memory.

The incoming `Host` header selects the upstream origin through the
`allowed_hosts` map in `config.yaml`. Requests from hosts that are not listed
are rejected with `403`.

## Requirements

- Go 1.21 or newer
- libvips 8.x with development headers (`vips-dev` on Alpine, `libvips-dev` on Debian/Ubuntu)
- Docker (optional, for containerised builds)

## Configuration

Copy `config.yaml.sample` to `config.yaml` and adjust it. The file is read from
the working directory at start-up.

| Section       | Key                | Purpose                                                        |
|---------------|--------------------|----------------------------------------------------------------|
| `allowed_hosts` | `<host>: <origin>` | Map of accepted `Host` headers to the upstream origin host      |
| `webp`        | `quality`          | Default WebP quality (1-100)                                    |
| `webp`        | `lossless`         | Use lossless WebP encoding                                      |
| `cache`       | `cache_enabled`    | Enable the in-memory LRU cache                                  |
| `cache`       | `nocache_header`   | Request header that bypasses the cache when set to `true`       |
| `cache`       | `lru_cache`        | Maximum number of cached images                                 |
| `limits`      | `max_image_bytes`  | Reject upstream images larger than this (default 20 MiB)        |
| `concurrency` | `max_goroutines`   | Maximum number of concurrent conversions                        |
| `http_client` | `*`                | Timeouts for upstream fetches, in seconds                       |
| `http_server` | `bind_ip`, `port`  | Public listener                                                 |
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

```bash
go vet ./...
go test ./...
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
