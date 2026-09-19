# ngAX High-Performance Rewrite Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Rewrite the ngAX image proxy so that thousands of requests per second are served with one origin fetch and one conversion per unique image, a byte-bounded cache, CPU-sized conversion concurrency, and client-side caching headers.

**Architecture:** Split `main.go` into `config.go`, `metrics.go`, `cache.go`, `fetcher.go`, `converter.go`, `handler.go`, `server.go` and a wiring-only `main.go`. The handler coalesces misses with `singleflight`, stores results in a ristretto byte-cost cache with a TTL negative cache, and separates I/O-bound fetch concurrency from CPU-bound conversion concurrency. Responses carry `ETag` and `Cache-Control` and answer `304` to `If-None-Match`.

**Tech Stack:** Go 1.21+ (`http.ServeMux` method patterns need 1.22; the module will be bumped to 1.22), libvips via `github.com/h2non/bimg`, `github.com/dgraph-io/ristretto/v2`, `golang.org/x/sync/singleflight`, `github.com/cespare/xxhash/v2`, Prometheus client, logrus, viper.

**Spec:** `docs/superpowers/specs/2026-09-19-high-performance-rewrite.md`

## Global Constraints

- Everything stays in `package main`, module name `ngAX`, flat layout (no `internal/` packages).
- Go is **not installed on the dev machine**. Every `go` command in this plan runs inside the dev container built in Task 1: `make test`, `make vet`, `make build`, or `make sh` for a shell. Never run bare `go ...` on the host.
- `go.mod` `go` directive becomes `1.22`. Dependencies added: `github.com/dgraph-io/ristretto/v2`, `golang.org/x/sync`. Dependency removed: `github.com/gorilla/mux`. Do not add any other module.
- Config keys are `snake_case` under existing sections; every new key has a default so an unchanged `config.yaml` keeps working.
- `VIPS_CONCURRENCY=1` is set in-process before any bimg call and in the Dockerfile.
- No per-request `Info` logging. `Debug` is allowed. Errors log at `Error`.
- Existing test names in `main_test.go` that are still meaningful must survive (moved, not deleted): `TestResolveQuality*`, `TestBuildImageURL*`, `TestLoadConfigKeepsDottedHostKeys`, `TestHealthIgnoresHostAllowlist`, `TestMetricsNotServedOnPublicPort`, `TestHandleRequestRecordsActualStatusCode`, `TestHandleRequestSkipsCacheWhenDisabled`.
- Commit after every task with the message given; end commit messages with `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`.
- Run `gofmt -l .` before every commit; it must print nothing.

---

## File Structure (end state)

| File | Responsibility | Created in |
|------|----------------|-----------|
| `Dockerfile.dev`, `Makefile` | Containerised Go + libvips toolchain for tests, vet, build | Task 1 |
| `config.go` | `Config`, `loadConfig`, `applyDefaults`, `validate`, `newHTTPClient`, `resolveQuality`, `buildImageURL` | Task 2 |
| `config_test.go` | Config tests | Task 2 |
| `metrics.go` | Prometheus collectors, `init()` registration, `statusRecorder` | Task 2 |
| `cache.go` / `cache_test.go` | `Entry`, `ImageCache` | Task 3 |
| `fetcher.go` / `fetcher_test.go` | `Fetcher`, `Body`, `UpstreamStatusError`, `isSupportedImageFormat` | Task 4 |
| `converter.go` / `converter_test.go` | `Converter` | Task 5 |
| `handler.go` / `handler_test.go` | `Handler` | Task 6 |
| `server.go` | `newRouter`, `newPublicServer`, `newMetricsServer`, `newPprofServer`, `basicAuthMiddleware`, `healthCheckHandler`, `runServers` | Task 7 |
| `main.go` | Wiring only | Task 7 |
| `main_test.go` | Deleted (tests migrated) | Task 7 |
| `Dockerfile`, `docker-compose.yml`, `config.yaml.sample`, `README.md` | Runtime image and docs | Task 8 |
| `handler_bench_test.go`, `loadtest/origin/main.go`, `loadtest/run.sh`, `docs/loadtest.md` | Benchmarks and load-test runbook | Task 9 |

---

### Task 1: Containerised dev toolchain

Go is not installed locally. Every later task depends on `make test`.

**Files:**
- Create: `Dockerfile.dev`
- Create: `Makefile`
- Modify: `.dockerignore` (add `loadtest/` later; nothing now)

**Interfaces:**
- Produces: `make test`, `make vet`, `make fmt`, `make build`, `make sh`, `make bench` targets used by every subsequent task.

- [ ] **Step 1: Create `Dockerfile.dev`**

```dockerfile
FROM golang:1.22-alpine
RUN apk --no-cache add build-base vips-dev git
ENV GOFLAGS=-mod=mod \
    CGO_ENABLED=1 \
    VIPS_CONCURRENCY=1
WORKDIR /app
```

- [ ] **Step 2: Create `Makefile`**

```makefile
IMAGE := ngax-dev
RUN   := docker run --rm -v "$(CURDIR)":/app -v ngax-gocache:/root/.cache -v ngax-gomod:/go/pkg/mod -w /app $(IMAGE)

.PHONY: image test vet fmt build sh bench tidy

image:
	docker build -q -f Dockerfile.dev -t $(IMAGE) . >/dev/null

test: image
	$(RUN) go test -count=1 ./...

vet: image
	$(RUN) sh -c 'gofmt -l . && go vet ./...'

fmt: image
	$(RUN) gofmt -w .

build: image
	$(RUN) go build -buildvcs=false -ldflags='-s -w' -o ngax .

bench: image
	$(RUN) go test -run '^$$' -bench . -benchmem ./...

tidy: image
	$(RUN) go mod tidy

sh: image
	docker run --rm -it -v "$(CURDIR)":/app -v ngax-gocache:/root/.cache -v ngax-gomod:/go/pkg/mod -w /app $(IMAGE) sh
```

- [ ] **Step 3: Run the existing suite through the container**

Run: `make test`
Expected: `ok  ngAX` and `? ngAX/stress-test [no test files]`.

- [ ] **Step 4: Add the binary to `.gitignore`**

`.gitignore` already lists `ngax`. Confirm with `grep -x ngax .gitignore`. No change if present.

- [ ] **Step 5: Commit**

```bash
git add Dockerfile.dev Makefile
git commit -m "build: containerised Go+libvips toolchain via Makefile

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 2: Split config and metrics out of `main.go`; add new config keys with defaults

Pure move plus new fields. Behaviour of the running binary is unchanged except for new defaults and a deprecation warning.

**Files:**
- Create: `config.go`, `config_test.go`, `metrics.go`
- Modify: `main.go` (remove moved code), `main_test.go` (move config tests out)

**Interfaces:**
- Produces:
  - `type Config struct` with the fields below (all later tasks read it through a `*Config`).
  - `func loadConfig(dir string) error` — fills the package-level `config`, applies defaults, validates.
  - `func (c *Config) applyDefaults()`
  - `func (c *Config) validate() error`
  - `func newHTTPClient(c *Config) *http.Client`
  - `func (c *Config) NegativeTTL() time.Duration`
  - `func resolveQuality(header string, def int) int`, `func buildImageURL(scheme, host, path, rawQuery string) string` (note: `scheme` becomes a parameter; the `upstreamScheme` global is removed in Task 7 — until then `buildImageURL` keeps a 3-arg wrapper, see Step 5).
  - `metrics.go`: `requestsTotal`, `responseDuration`, `cacheHitsTotal`, `cacheMissesTotal`, `negativeHitsTotal`, `coalescedTotal`, `errorsTotal`, `totalImageSizeBeforeConversion`, `totalImageSizeAfterConversion`, `invalidHostsCount`, `cacheBytes` (Gauge), and `type statusRecorder`.

- [ ] **Step 1: Write failing tests in `config_test.go`**

```go
package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func writeConfig(t *testing.T, yaml string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

const minimalYAML = "allowed_hosts:\n  example.com: \"images.example.com\"\n"

func TestLoadConfigKeepsDottedHostKeys(t *testing.T) {
	dir := writeConfig(t, minimalYAML+"webp:\n  quality: 60\n")
	config = Config{}
	if err := loadConfig(dir); err != nil {
		t.Fatal(err)
	}
	if got := config.AllowedHosts["example.com"]; got != "images.example.com" {
		t.Fatalf("want images.example.com, got %q (all: %v)", got, config.AllowedHosts)
	}
	if config.WebP.Quality != 60 {
		t.Fatalf("want quality 60, got %d", config.WebP.Quality)
	}
}

func TestLoadConfigAppliesDefaults(t *testing.T) {
	dir := writeConfig(t, minimalYAML)
	config = Config{}
	if err := loadConfig(dir); err != nil {
		t.Fatal(err)
	}
	c := config
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"webp.quality", c.WebP.Quality, 75},
		{"cache.max_bytes", c.Cache.MaxBytes, int64(1 << 30)},
		{"cache.negative_ttl_seconds", c.Cache.NegativeTTLSeconds, 30},
		{"cache.nocache_header", c.Cache.NoCacheHeader, "X-No-Cache"},
		{"limits.max_image_bytes", c.Limits.MaxImageBytes, int64(20 << 20)},
		{"concurrency.max_conversions", c.Concurrency.MaxConversions, runtime.NumCPU()},
		{"concurrency.max_fetches", c.Concurrency.MaxFetches, 512},
		{"http_client.max_idle_conns_per_host", c.HTTPClient.MaxIdleConnsPerHost, 256},
		{"http_client.timeout_seconds", c.HTTPClient.TimeoutSeconds, 30},
		{"http_server.read_header_timeout_seconds", c.HTTPServer.ReadHeaderTimeoutSeconds, 5},
		{"http_server.idle_timeout_seconds", c.HTTPServer.IdleTimeoutSeconds, 60},
		{"http_server.cache_control", c.HTTPServer.CacheControl, "public, max-age=31536000, immutable"},
		{"http_server.port", c.HTTPServer.Port, 8080},
		{"exporter.port", c.Exporter.Port, 9080},
		{"log.level", c.Log.Level, "info"},
	}
	for _, ck := range checks {
		if ck.got != ck.want {
			t.Errorf("%s: want %v, got %v", ck.name, ck.want, ck.got)
		}
	}
	if c.NegativeTTL() != 30*time.Second {
		t.Errorf("NegativeTTL: want 30s, got %v", c.NegativeTTL())
	}
}

func TestNegativeTTLDisabledByNegativeValue(t *testing.T) {
	c := &Config{}
	c.Cache.NegativeTTLSeconds = -1
	c.applyDefaults()
	if c.NegativeTTL() != 0 {
		t.Fatalf("want 0 (disabled), got %v", c.NegativeTTL())
	}
}

func TestLoadConfigRejectsEmptyAllowedHosts(t *testing.T) {
	dir := writeConfig(t, "webp:\n  quality: 60\n")
	config = Config{}
	if err := loadConfig(dir); err == nil {
		t.Fatal("want error for empty allowed_hosts, got nil")
	}
}

func TestLoadConfigRejectsBadQuality(t *testing.T) {
	dir := writeConfig(t, minimalYAML+"webp:\n  quality: 150\n")
	config = Config{}
	if err := loadConfig(dir); err == nil {
		t.Fatal("want error for quality 150, got nil")
	}
}

func TestNewHTTPClientUsesIdleConnLimits(t *testing.T) {
	c := &Config{}
	c.applyDefaults()
	c.HTTPClient.MaxIdleConnsPerHost = 33
	client := newHTTPClient(c)
	tr := client.Transport.(*http.Transport)
	if tr.MaxIdleConnsPerHost != 33 || tr.MaxIdleConns != 33 {
		t.Fatalf("want idle limits 33/33, got %d/%d", tr.MaxIdleConnsPerHost, tr.MaxIdleConns)
	}
	if !tr.ForceAttemptHTTP2 {
		t.Fatal("want ForceAttemptHTTP2")
	}
	if client.Timeout != 30*time.Second {
		t.Fatalf("want 30s timeout, got %v", client.Timeout)
	}
}

func TestResolveQualityUsesHeaderWithinRange(t *testing.T) {
	if got := resolveQuality("42", 75); got != 42 {
		t.Fatalf("want 42, got %d", got)
	}
}

func TestResolveQualityFallsBackOnEmptyOrInvalid(t *testing.T) {
	for _, h := range []string{"", "abc", "0", "-5"} {
		if got := resolveQuality(h, 75); got != 75 {
			t.Errorf("header %q: want 75, got %d", h, got)
		}
	}
}

func TestResolveQualityClampsAbove100(t *testing.T) {
	if got := resolveQuality("500", 75); got != 100 {
		t.Fatalf("want 100, got %d", got)
	}
}

func TestBuildImageURLPreservesQueryString(t *testing.T) {
	got := buildImageURL("https://", "cdn.example.com", "/a/b.jpg", "v=2&w=10")
	want := "https://cdn.example.com/a/b.jpg?v=2&w=10"
	if got != want {
		t.Fatalf("want %s, got %s", want, got)
	}
}

func TestBuildImageURLWithoutQueryString(t *testing.T) {
	got := buildImageURL("http://", "cdn.example.com", "/a/b.jpg", "")
	want := "http://cdn.example.com/a/b.jpg"
	if got != want {
		t.Fatalf("want %s, got %s", want, got)
	}
}
```

Add `"net/http"` to the import block.

- [ ] **Step 2: Remove the duplicated tests from `main_test.go`**

Delete from `main_test.go`: `TestResolveQuality*` (3 tests), `TestBuildImageURL*` (2 tests), `TestLoadConfigKeepsDottedHostKeys`. Update the remaining test `TestHandleRequestSkipsCacheWhenDisabled` and `testConfig` — they still compile because `upstreamScheme` remains until Task 7. Remove the now-unused `os` and `path/filepath` imports from `main_test.go`.

- [ ] **Step 3: Run tests to verify they fail**

Run: `make test`
Expected: build failure listing `undefined: config.Cache.MaxBytes`, `undefined: newHTTPClient`, `c.applyDefaults undefined`, and a `buildImageURL` arity error (`too many arguments`).

- [ ] **Step 4: Create `metrics.go` (move from `main.go`, add three collectors)**

```go
package main

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	requestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "ngax_http_requests_total", Help: "Total number of HTTP requests."},
		[]string{"status_code", "method"},
	)
	responseDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "ngax_http_response_duration_seconds",
			Help:    "Histogram of HTTP response durations.",
			Buckets: []float64{.0005, .001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
		},
		[]string{"status_code", "method"},
	)
	cacheHitsTotal   = prometheus.NewCounter(prometheus.CounterOpts{Name: "ngax_cache_hits_total", Help: "Total number of cache hits."})
	cacheMissesTotal = prometheus.NewCounter(prometheus.CounterOpts{Name: "ngax_cache_misses_total", Help: "Total number of cache misses."})
	negativeHitsTotal = prometheus.NewCounter(prometheus.CounterOpts{Name: "ngax_negative_cache_hits_total", Help: "Requests answered from the negative (404) cache."})
	coalescedTotal   = prometheus.NewCounter(prometheus.CounterOpts{Name: "ngax_coalesced_requests_total", Help: "Cache misses that waited on an in-flight fetch for the same key instead of fetching."})
	errorsTotal      = prometheus.NewCounter(prometheus.CounterOpts{Name: "ngax_http_errors_total", Help: "Total number of HTTP errors."})
	totalImageSizeBeforeConversion = prometheus.NewCounter(prometheus.CounterOpts{Name: "ngax_total_image_size_before_conversion_bytes", Help: "Total size of images before conversion in bytes."})
	totalImageSizeAfterConversion  = prometheus.NewCounter(prometheus.CounterOpts{Name: "ngax_total_image_size_after_conversion_bytes", Help: "Total size of images after conversion in bytes."})
	invalidHostsCount = prometheus.NewCounter(prometheus.CounterOpts{Name: "ngax_invalid_hosts_count", Help: "Count of unauthorized host access attempts."})
	cacheBytes        = prometheus.NewGauge(prometheus.GaugeOpts{Name: "ngax_cache_bytes", Help: "Approximate bytes held by the image cache."})
)

func init() {
	prometheus.MustRegister(
		requestsTotal, responseDuration, cacheHitsTotal, cacheMissesTotal, negativeHitsTotal,
		coalescedTotal, errorsTotal, totalImageSizeBeforeConversion, totalImageSizeAfterConversion,
		invalidHostsCount, cacheBytes,
	)
}

// statusRecorder captures the status code written by a handler so metrics
// can be recorded with the real outcome.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

func (r *statusRecorder) Status() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}
```

Delete the `var (...)` metrics block, the `init()` function, and the `statusRecorder` type and methods from `main.go`.

- [ ] **Step 5: Create `config.go`**

```go
package main

import (
	"fmt"
	"net"
	"net/http"
	"runtime"
	"strconv"
	"time"

	"github.com/spf13/viper"
)

type Config struct {
	WebP struct {
		Quality      int  `mapstructure:"quality"`
		Lossless     bool `mapstructure:"lossless"`
		NearLossless int  `mapstructure:"near_lossless"`
	} `mapstructure:"webp"`
	Cache struct {
		CacheEnabled       bool   `mapstructure:"cache_enabled"`
		NoCacheHeader      string `mapstructure:"nocache_header"`
		MaxBytes           int64  `mapstructure:"max_bytes"`
		NegativeTTLSeconds int    `mapstructure:"negative_ttl_seconds"`
		LruCache           int    `mapstructure:"lru_cache"` // deprecated, ignored
	} `mapstructure:"cache"`
	Concurrency struct {
		MaxGoroutines  int `mapstructure:"max_goroutines"` // deprecated alias of max_conversions
		MaxConversions int `mapstructure:"max_conversions"`
		MaxFetches     int `mapstructure:"max_fetches"`
	} `mapstructure:"concurrency"`
	HTTPClient struct {
		TimeoutSeconds        int `mapstructure:"timeout_seconds"`
		DialTimeoutSeconds    int `mapstructure:"dial_timeout_seconds"`
		KeepAlive             int `mapstructure:"keep_alive"`
		TLSHandshakeTimeout   int `mapstructure:"TLS_handshake_timeout"`
		ResponseHeaderTimeout int `mapstructure:"response_header_timeout"`
		ExpectContinueTimeout int `mapstructure:"expect_continue_timeout"`
		IdleConnTimeout       int `mapstructure:"idle_conn_timeout"`
		MaxIdleConnsPerHost   int `mapstructure:"max_idle_conns_per_host"`
	} `mapstructure:"http_client"`
	AllowedHosts map[string]string `mapstructure:"allowed_hosts"`
	Limits       struct {
		MaxImageBytes int64 `mapstructure:"max_image_bytes"`
	} `mapstructure:"limits"`
	Exporter struct {
		BindIP   string `mapstructure:"bind_ip"`
		Port     int    `mapstructure:"port"`
		User     string `mapstructure:"user"`
		Password string `mapstructure:"password"`
	} `mapstructure:"exporter"`
	HTTPServer struct {
		BindIP                   string `mapstructure:"bind_ip"`
		Port                     int    `mapstructure:"port"`
		ReadHeaderTimeoutSeconds int    `mapstructure:"read_header_timeout_seconds"`
		IdleTimeoutSeconds       int    `mapstructure:"idle_timeout_seconds"`
		CacheControl             string `mapstructure:"cache_control"`
	} `mapstructure:"http_server"`
	Log struct {
		Level string `mapstructure:"level"`
	} `mapstructure:"log"`
}

// config is the process-wide configuration, filled by loadConfig.
var config Config

// loadConfig reads config.yaml from dir into the global config, applies
// defaults and validates the result.
func loadConfig(dir string) error {
	// Hostnames are map keys in allowed_hosts, so the default "." key
	// delimiter must not be used or viper would split them into nested maps.
	v := viper.NewWithOptions(viper.KeyDelimiter("::"))
	v.SetConfigName("config")
	v.SetConfigType("yaml")
	v.AddConfigPath(dir)
	if err := v.ReadInConfig(); err != nil {
		return fmt.Errorf("reading config file: %w", err)
	}
	if err := v.Unmarshal(&config); err != nil {
		return fmt.Errorf("decoding config: %w", err)
	}
	config.applyDefaults()
	return config.validate()
}

func setDefaultInt(p *int, def int) {
	if *p <= 0 {
		*p = def
	}
}

func setDefaultInt64(p *int64, def int64) {
	if *p <= 0 {
		*p = def
	}
}

func setDefaultString(p *string, def string) {
	if *p == "" {
		*p = def
	}
}

// applyDefaults fills zero values so an older config.yaml keeps working.
func (c *Config) applyDefaults() {
	setDefaultInt(&c.WebP.Quality, 75)

	setDefaultString(&c.Cache.NoCacheHeader, "X-No-Cache")
	setDefaultInt64(&c.Cache.MaxBytes, 1<<30)
	if c.Cache.NegativeTTLSeconds == 0 {
		c.Cache.NegativeTTLSeconds = 30
	}

	if c.Concurrency.MaxConversions <= 0 && c.Concurrency.MaxGoroutines > 0 {
		c.Concurrency.MaxConversions = c.Concurrency.MaxGoroutines
	}
	setDefaultInt(&c.Concurrency.MaxConversions, runtime.NumCPU())
	setDefaultInt(&c.Concurrency.MaxFetches, 512)

	setDefaultInt(&c.HTTPClient.TimeoutSeconds, 30)
	setDefaultInt(&c.HTTPClient.DialTimeoutSeconds, 5)
	setDefaultInt(&c.HTTPClient.KeepAlive, 30)
	setDefaultInt(&c.HTTPClient.TLSHandshakeTimeout, 10)
	setDefaultInt(&c.HTTPClient.ResponseHeaderTimeout, 30)
	setDefaultInt(&c.HTTPClient.ExpectContinueTimeout, 1)
	setDefaultInt(&c.HTTPClient.IdleConnTimeout, 90)
	setDefaultInt(&c.HTTPClient.MaxIdleConnsPerHost, 256)

	setDefaultInt64(&c.Limits.MaxImageBytes, 20<<20)

	setDefaultString(&c.HTTPServer.BindIP, "127.0.0.1")
	setDefaultInt(&c.HTTPServer.Port, 8080)
	setDefaultInt(&c.HTTPServer.ReadHeaderTimeoutSeconds, 5)
	setDefaultInt(&c.HTTPServer.IdleTimeoutSeconds, 60)
	setDefaultString(&c.HTTPServer.CacheControl, "public, max-age=31536000, immutable")

	setDefaultString(&c.Exporter.BindIP, "127.0.0.1")
	setDefaultInt(&c.Exporter.Port, 9080)

	setDefaultString(&c.Log.Level, "info")
}

func (c *Config) validate() error {
	if len(c.AllowedHosts) == 0 {
		return fmt.Errorf("allowed_hosts must contain at least one host mapping")
	}
	if c.WebP.Quality < 1 || c.WebP.Quality > 100 {
		return fmt.Errorf("webp.quality must be in 1..100, got %d", c.WebP.Quality)
	}
	return nil
}

// NegativeTTL is how long origin 404s are remembered. The YAML default is
// 30 s; a negative value disables it (0 is indistinguishable from "unset").
func (c *Config) NegativeTTL() time.Duration {
	if c.Cache.NegativeTTLSeconds < 0 {
		return 0
	}
	return time.Duration(c.Cache.NegativeTTLSeconds) * time.Second
}

// newHTTPClient builds the upstream client. The service talks to a handful
// of origin hosts, so the idle pool per host is the main lever against
// connection churn.
func newHTTPClient(c *Config) *http.Client {
	sec := func(n int) time.Duration { return time.Duration(n) * time.Second }
	return &http.Client{
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   sec(c.HTTPClient.DialTimeoutSeconds),
				KeepAlive: sec(c.HTTPClient.KeepAlive),
			}).DialContext,
			ForceAttemptHTTP2:     true,
			TLSHandshakeTimeout:   sec(c.HTTPClient.TLSHandshakeTimeout),
			ResponseHeaderTimeout: sec(c.HTTPClient.ResponseHeaderTimeout),
			ExpectContinueTimeout: sec(c.HTTPClient.ExpectContinueTimeout),
			IdleConnTimeout:       sec(c.HTTPClient.IdleConnTimeout),
			MaxIdleConns:          c.HTTPClient.MaxIdleConnsPerHost,
			MaxIdleConnsPerHost:   c.HTTPClient.MaxIdleConnsPerHost,
		},
		Timeout: sec(c.HTTPClient.TimeoutSeconds),
	}
}

// resolveQuality returns the WebP quality to use: the header value when it is
// a valid integer in 1..100 (values above 100 are clamped), otherwise def.
func resolveQuality(header string, def int) int {
	q, err := strconv.Atoi(header)
	if err != nil || q <= 0 {
		return def
	}
	if q > 100 {
		return 100
	}
	return q
}

// buildImageURL builds the upstream URL, keeping the query string so that
// distinct variants of an image are fetched and cached separately.
func buildImageURL(scheme, host, path, rawQuery string) string {
	u := scheme + host + path
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	return u
}
```

- [ ] **Step 6: Trim `main.go` to match**

In `main.go`:
- Delete the `type Config struct {...}` block, `loadConfig`, `resolveQuality`, `buildImageURL`, and `config Config` from the `var (...)` block (they now live in `config.go`).
- Delete `defaultMaxImageBytes` and the two default-fixing blocks in `setupRuntime` (defaults now come from `applyDefaults`). `setupRuntime` keeps creating `imgCache`, `httpClient` (call `httpClient = newHTTPClient(&config)`), and `semaphore = make(chan struct{}, config.Concurrency.MaxConversions)`.
- Replace the call `buildImageURL(allowedHost, r.URL.Path, r.URL.RawQuery)` with `buildImageURL(upstreamScheme, allowedHost, r.URL.Path, r.URL.RawQuery)`.
- Replace `imgCache, err = lru.New2Q[string, []byte](config.Cache.LruCache)` with `lru.New2Q[string, []byte](1024)` (temporary; the LRU is removed in Task 7).
- Remove now-unused imports (`runtime`, `github.com/spf13/viper`, `github.com/prometheus/client_golang/prometheus`; keep `promhttp`).

In `main_test.go` `testConfig`: replace `config.Cache.LruCache = 16` with `config.Cache.MaxBytes = 1 << 20` and add `config.applyDefaults()` right after the `config.AllowedHosts = ...` line (so `MaxConversions` and timeouts are set) but **before** `config.HTTPClient.TimeoutSeconds = 2` so the 2 s test timeout survives. Also add `config.Concurrency.MaxConversions = 4`.

- [ ] **Step 7: Run tests**

Run: `make vet && make test`
Expected: `gofmt` prints nothing; `ok  ngAX`.

- [ ] **Step 8: Commit**

```bash
git add config.go config_test.go metrics.go main.go main_test.go
git commit -m "refactor: split config and metrics out of main.go, add tunables with defaults

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 3: Byte-bounded image cache with negative cache (`cache.go`)

**Files:**
- Create: `cache.go`, `cache_test.go`
- Modify: `go.mod` (ristretto v2 via `make tidy`)

**Interfaces:**
- Produces:
  - `type Entry struct { Data []byte; ETag string }`
  - `func NewEntry(data []byte) Entry` — computes `ETag` as `"\"<xxhash64 hex>\""`.
  - `type ImageCache struct`
  - `func NewImageCache(maxBytes int64, negativeTTL time.Duration) (*ImageCache, error)`
  - `func (c *ImageCache) Get(key string) (Entry, bool)`
  - `func (c *ImageCache) Set(key string, e Entry)`
  - `func (c *ImageCache) GetNegative(key string) (int, bool)` — returns the remembered status code.
  - `func (c *ImageCache) SetNegative(key string, status int)` — no-op when TTL <= 0.
  - `func (c *ImageCache) Wait()` — flushes async sets (tests only).
  - `func (c *ImageCache) Close()`
  - `func cacheKey(imageURL string, quality int) string` → `imageURL + "|q=" + itoa(quality)`.

- [ ] **Step 1: Write failing tests in `cache_test.go`**

```go
package main

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

func newTestCache(t *testing.T, maxBytes int64, negTTL time.Duration) *ImageCache {
	t.Helper()
	c, err := NewImageCache(maxBytes, negTTL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func TestNewEntryComputesQuotedETag(t *testing.T) {
	e := NewEntry([]byte("hello"))
	if len(e.ETag) < 3 || e.ETag[0] != '"' || e.ETag[len(e.ETag)-1] != '"' {
		t.Fatalf("ETag must be a quoted string, got %q", e.ETag)
	}
	if e.ETag != NewEntry([]byte("hello")).ETag {
		t.Fatal("ETag must be deterministic")
	}
	if e.ETag == NewEntry([]byte("hellp")).ETag {
		t.Fatal("different data must give different ETag")
	}
}

func TestCacheSetThenGet(t *testing.T) {
	c := newTestCache(t, 1<<20, 0)
	c.Set("k", NewEntry([]byte("img")))
	c.Wait()
	got, ok := c.Get("k")
	if !ok || string(got.Data) != "img" {
		t.Fatalf("want hit with img, got ok=%v data=%q", ok, got.Data)
	}
}

func TestCacheMissReturnsFalse(t *testing.T) {
	c := newTestCache(t, 1<<20, 0)
	if _, ok := c.Get("absent"); ok {
		t.Fatal("want miss")
	}
}

func TestCacheEvictsWhenOverMaxBytes(t *testing.T) {
	const max = 64 << 10 // 64 KiB
	c := newTestCache(t, max, 0)
	blob := make([]byte, 8<<10) // 8 KiB each; 32 entries = 256 KiB inserted
	for i := 0; i < 32; i++ {
		c.Set(fmt.Sprintf("k%d", i), NewEntry(blob))
	}
	c.Wait()
	held := 0
	for i := 0; i < 32; i++ {
		if _, ok := c.Get(fmt.Sprintf("k%d", i)); ok {
			held++
		}
	}
	if held == 0 {
		t.Fatal("cache holds nothing after inserts")
	}
	if int64(held)*int64(len(blob)) > max {
		t.Fatalf("cache holds %d entries (%d bytes) above max %d", held, held*len(blob), max)
	}
}

func TestNegativeCacheRemembersStatusUntilTTL(t *testing.T) {
	c := newTestCache(t, 1<<20, 200*time.Millisecond)
	c.SetNegative("k", http.StatusNotFound)
	c.Wait()
	if code, ok := c.GetNegative("k"); !ok || code != http.StatusNotFound {
		t.Fatalf("want 404 negative hit, got ok=%v code=%d", ok, code)
	}
	time.Sleep(300 * time.Millisecond)
	if _, ok := c.GetNegative("k"); ok {
		t.Fatal("negative entry should have expired")
	}
}

func TestNegativeCacheDisabledWhenTTLZero(t *testing.T) {
	c := newTestCache(t, 1<<20, 0)
	c.SetNegative("k", http.StatusNotFound)
	c.Wait()
	if _, ok := c.GetNegative("k"); ok {
		t.Fatal("negative cache must be disabled when TTL is 0")
	}
}

func TestCacheKeyIncludesQuality(t *testing.T) {
	if cacheKey("https://o/a.jpg", 60) == cacheKey("https://o/a.jpg", 80) {
		t.Fatal("keys for different qualities must differ")
	}
	if cacheKey("https://o/a.jpg", 60) != "https://o/a.jpg|q=60" {
		t.Fatalf("unexpected key format %q", cacheKey("https://o/a.jpg", 60))
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `make test`
Expected: `undefined: NewImageCache`, `undefined: NewEntry`, `undefined: cacheKey`.

- [ ] **Step 3: Create `cache.go`**

```go
package main

import (
	"fmt"
	"strconv"
	"time"

	"github.com/cespare/xxhash/v2"
	"github.com/dgraph-io/ristretto/v2"
)

// Entry is a converted image ready to serve.
type Entry struct {
	Data []byte
	ETag string
}

// NewEntry wraps converted bytes and computes their ETag once.
func NewEntry(data []byte) Entry {
	return Entry{
		Data: data,
		ETag: `"` + strconv.FormatUint(xxhash.Sum64(data), 16) + `"`,
	}
}

func cacheKey(imageURL string, quality int) string {
	return imageURL + "|q=" + strconv.Itoa(quality)
}

// ImageCache holds converted images bounded by total bytes, plus a small
// TTL cache of upstream status codes for images that do not exist.
type ImageCache struct {
	images      *ristretto.Cache[string, Entry]
	negative    *ristretto.Cache[string, int]
	negativeTTL time.Duration
}

// NewImageCache creates a cache holding at most maxBytes of image data.
// negativeTTL <= 0 disables the negative cache.
func NewImageCache(maxBytes int64, negativeTTL time.Duration) (*ImageCache, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("cache max bytes must be > 0, got %d", maxBytes)
	}
	// NumCounters should be ~10x the expected number of items; assume a
	// 32 KiB average image and clamp to a sane range.
	counters := maxBytes / (32 << 10) * 10
	if counters < 1_000 {
		counters = 1_000
	}
	if counters > 100_000_000 {
		counters = 100_000_000
	}
	images, err := ristretto.NewCache(&ristretto.Config[string, Entry]{
		NumCounters: counters,
		MaxCost:     maxBytes,
		BufferItems: 64,
		Metrics:     false,
		OnEvict: func(item *ristretto.Item[Entry]) {
			cacheBytes.Sub(float64(item.Cost))
		},
	})
	if err != nil {
		return nil, fmt.Errorf("creating image cache: %w", err)
	}
	c := &ImageCache{images: images, negativeTTL: negativeTTL}
	if negativeTTL > 0 {
		c.negative, err = ristretto.NewCache(&ristretto.Config[string, int]{
			NumCounters: 100_000,
			MaxCost:     10_000, // number of remembered misses
			BufferItems: 64,
		})
		if err != nil {
			images.Close()
			return nil, fmt.Errorf("creating negative cache: %w", err)
		}
	}
	return c, nil
}

func (c *ImageCache) Get(key string) (Entry, bool) {
	return c.images.Get(key)
}

// Set stores e; the insert is asynchronous and may be dropped under
// contention, which is acceptable for a cache.
func (c *ImageCache) Set(key string, e Entry) {
	cost := int64(len(e.Data))
	if c.images.Set(key, e, cost) {
		cacheBytes.Add(float64(cost))
	}
}

func (c *ImageCache) GetNegative(key string) (int, bool) {
	if c.negative == nil {
		return 0, false
	}
	return c.negative.Get(key)
}

func (c *ImageCache) SetNegative(key string, status int) {
	if c.negative == nil {
		return
	}
	c.negative.SetWithTTL(key, status, 1, c.negativeTTL)
}

// Wait blocks until pending sets are applied. Intended for tests.
func (c *ImageCache) Wait() {
	c.images.Wait()
	if c.negative != nil {
		c.negative.Wait()
	}
}

func (c *ImageCache) Close() {
	c.images.Close()
	if c.negative != nil {
		c.negative.Close()
	}
}
```

Note on `cacheBytes`: ristretto rejects sets whose cost exceeds `MaxCost` and may drop sets when its buffer is full; `Set` returning `true` means "accepted into the buffer", so the gauge is approximate by design. `OnEvict` keeps it from drifting upward. If `go doc github.com/dgraph-io/ristretto/v2 Config` inside `make sh` shows `OnEvict` with a different signature, adapt to what `go doc` prints; the field has existed since v0.1.

- [ ] **Step 4: Add the dependency and run tests**

Run: `make tidy && make test`
Expected: `go.mod` gains `github.com/dgraph-io/ristretto/v2 v2.x.y` and `github.com/cespare/xxhash/v2` moves from indirect to direct; all tests pass. If `TestCacheEvictsWhenOverMaxBytes` reports `held == 0`, ristretto's TinyLFU admission rejected all cold inserts; in that test insert each key twice (second `Set` after the first `Wait()`) so the keys have frequency, then assert — do not raise `max`.

- [ ] **Step 5: Commit**

```bash
git add cache.go cache_test.go go.mod go.sum
git commit -m "feat: byte-bounded ristretto image cache with negative TTL cache

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 4: Bounded, pooled origin fetcher (`fetcher.go`)

**Files:**
- Create: `fetcher.go`, `fetcher_test.go`
- Modify: `main.go` (remove `isSupportedImageFormat`; it moves to `fetcher.go`)

**Interfaces:**
- Consumes: `newHTTPClient(*Config)` from Task 2; `totalImageSizeBeforeConversion` from `metrics.go`.
- Produces:
  - `type UpstreamStatusError struct{ Code int }` with `Error() string`.
  - `type Body struct` with `Bytes() []byte`, `ContentType string`, `Release()`.
  - `type Fetcher struct`; `func NewFetcher(client *http.Client, maxBytes int64, maxInFlight int) *Fetcher`
  - `func (f *Fetcher) Fetch(ctx context.Context, url string) (*Body, error)`
  - `func isSupportedImageFormat(contentType string) bool`

- [ ] **Step 1: Write failing tests in `fetcher_test.go`**

```go
package main

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func pngBytes(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for i := range img.Pix {
		img.Pix[i] = byte(i)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func pngServer(t *testing.T) *httptest.Server {
	t.Helper()
	data := pngBytes(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(data)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchReturnsBodyAndContentType(t *testing.T) {
	srv := pngServer(t)
	f := NewFetcher(srv.Client(), 1<<20, 4)
	body, err := f.Fetch(context.Background(), srv.URL+"/a.png")
	if err != nil {
		t.Fatal(err)
	}
	defer body.Release()
	if body.ContentType != "image/png" {
		t.Fatalf("want image/png, got %q", body.ContentType)
	}
	if !bytes.Equal(body.Bytes(), pngBytes(t)) {
		t.Fatal("body differs from served bytes")
	}
}

func TestFetchReportsUpstreamStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	f := NewFetcher(srv.Client(), 1<<20, 4)
	_, err := f.Fetch(context.Background(), srv.URL+"/missing.png")
	var us *UpstreamStatusError
	if !errors.As(err, &us) || us.Code != http.StatusNotFound {
		t.Fatalf("want UpstreamStatusError 404, got %v", err)
	}
}

func TestFetchRejectsUnsupportedContentType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html>"))
	}))
	defer srv.Close()
	f := NewFetcher(srv.Client(), 1<<20, 4)
	if _, err := f.Fetch(context.Background(), srv.URL+"/x"); err == nil {
		t.Fatal("want error for text/html, got nil")
	}
}

func TestFetchRejectsOversizedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(make([]byte, 4096))
	}))
	defer srv.Close()
	f := NewFetcher(srv.Client(), 512, 4)
	if _, err := f.Fetch(context.Background(), srv.URL+"/big.png"); err == nil {
		t.Fatal("want error for oversized body, got nil")
	}
}

func TestFetchRejectsOversizedContentLengthBeforeReading(t *testing.T) {
	var read atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Length", "4096")
		read.Add(1)
		w.Write(make([]byte, 4096))
	}))
	defer srv.Close()
	f := NewFetcher(srv.Client(), 512, 4)
	_, err := f.Fetch(context.Background(), srv.URL+"/big.png")
	if err == nil {
		t.Fatal("want error, got nil")
	}
	// The handler ran once (the response headers were needed) but the error
	// must come from the Content-Length check, not from reading 4 KiB.
	if !errors.Is(err, ErrImageTooLarge) {
		t.Fatalf("want ErrImageTooLarge, got %v", err)
	}
}

func TestFetchLimitsConcurrentRequests(t *testing.T) {
	var inFlight, peak atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := inFlight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		<-release
		inFlight.Add(-1)
		w.Header().Set("Content-Type", "image/png")
		w.Write(pngBytes(t))
	}))
	defer srv.Close()

	f := NewFetcher(srv.Client(), 1<<20, 2)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if b, err := f.Fetch(context.Background(), srv.URL+"/a.png"); err == nil {
				b.Release()
			}
		}()
	}
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()
	if peak.Load() > 2 {
		t.Fatalf("want at most 2 concurrent fetches, saw %d", peak.Load())
	}
}

func TestFetchHonoursContextWhileWaitingForSlot(t *testing.T) {
	f := NewFetcher(http.DefaultClient, 1<<20, 1)
	f.sem <- struct{}{} // occupy the only slot
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := f.Fetch(ctx, "http://127.0.0.1:1/x.png"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
}

func TestIsSupportedImageFormat(t *testing.T) {
	for ct, want := range map[string]bool{
		"image/jpeg": true, "image/png": true, "image/gif": true, "image/webp": true,
		"image/png; charset=binary": true, "text/html": false, "": false,
	} {
		if got := isSupportedImageFormat(ct); got != want {
			t.Errorf("%q: want %v, got %v", ct, want, got)
		}
	}
}
```

Remove `pngBytes` from `main_test.go` (it now lives in `fetcher_test.go`; same package, so `main_test.go` keeps compiling). Remove the `image` and `image/png` imports from `main_test.go`.

- [ ] **Step 2: Run tests to verify they fail**

Run: `make test`
Expected: `undefined: NewFetcher`, `undefined: UpstreamStatusError`, `undefined: ErrImageTooLarge`.

- [ ] **Step 3: Create `fetcher.go`**

```go
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

// ErrImageTooLarge is returned when the origin body exceeds the configured limit.
var ErrImageTooLarge = errors.New("image exceeds size limit")

// UpstreamStatusError reports a non-200 response from the origin.
type UpstreamStatusError struct {
	Code int
}

func (e *UpstreamStatusError) Error() string {
	return fmt.Sprintf("upstream returned HTTP %d", e.Code)
}

// maxPooledBuffer keeps very large buffers out of the pool so a few huge
// images do not pin memory forever.
const maxPooledBuffer = 4 << 20

// Body is a fetched origin response. Call Release when the bytes are no
// longer needed so the buffer returns to the pool.
type Body struct {
	ContentType string
	buf         *bytes.Buffer
	pool        *sync.Pool
}

func (b *Body) Bytes() []byte { return b.buf.Bytes() }

func (b *Body) Release() {
	if b.buf == nil {
		return
	}
	if b.buf.Cap() <= maxPooledBuffer {
		b.buf.Reset()
		b.pool.Put(b.buf)
	}
	b.buf = nil
}

// Fetcher downloads origin images with a concurrency cap and a size limit.
type Fetcher struct {
	client   *http.Client
	maxBytes int64
	sem      chan struct{}
	pool     sync.Pool
}

func NewFetcher(client *http.Client, maxBytes int64, maxInFlight int) *Fetcher {
	if maxInFlight <= 0 {
		maxInFlight = 1
	}
	return &Fetcher{
		client:   client,
		maxBytes: maxBytes,
		sem:      make(chan struct{}, maxInFlight),
		pool:     sync.Pool{New: func() any { return new(bytes.Buffer) }},
	}
}

// Fetch downloads url. It blocks while all fetch slots are busy, honouring ctx.
func (f *Fetcher) Fetch(ctx context.Context, url string) (*Body, error) {
	select {
	case f.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-f.sem }()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, &UpstreamStatusError{Code: resp.StatusCode}
	}
	contentType := resp.Header.Get("Content-Type")
	if !isSupportedImageFormat(contentType) {
		return nil, fmt.Errorf("unsupported image format %q", contentType)
	}
	if resp.ContentLength > f.maxBytes {
		return nil, fmt.Errorf("%w: content-length %d > %d", ErrImageTooLarge, resp.ContentLength, f.maxBytes)
	}

	buf := f.pool.Get().(*bytes.Buffer)
	buf.Reset()
	if resp.ContentLength > 0 {
		buf.Grow(int(resp.ContentLength))
	}
	n, err := io.Copy(buf, io.LimitReader(resp.Body, f.maxBytes+1))
	if err != nil {
		buf.Reset()
		f.pool.Put(buf)
		return nil, fmt.Errorf("reading image body: %w", err)
	}
	if n > f.maxBytes {
		buf.Reset()
		f.pool.Put(buf)
		return nil, fmt.Errorf("%w: body > %d bytes", ErrImageTooLarge, f.maxBytes)
	}
	totalImageSizeBeforeConversion.Add(float64(n))
	return &Body{ContentType: contentType, buf: buf, pool: &f.pool}, nil
}

func isSupportedImageFormat(contentType string) bool {
	for _, format := range []string{"jpeg", "jpg", "png", "gif", "bmp", "webp", "tiff"} {
		if strings.Contains(contentType, format) {
			return true
		}
	}
	return false
}
```

Delete `isSupportedImageFormat` from `main.go`.

- [ ] **Step 4: Run tests**

Run: `make vet && make test`
Expected: all pass. `TestFetchLimitsConcurrentRequests` depends on `httptest.Server` allowing 6 parallel connections, which it does.

- [ ] **Step 5: Commit**

```bash
git add fetcher.go fetcher_test.go main.go main_test.go
git commit -m "feat: bounded origin fetcher with pooled buffers and size limits

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 5: CPU-bounded WebP converter (`converter.go`)

**Files:**
- Create: `converter.go`, `converter_test.go`

**Interfaces:**
- Consumes: `totalImageSizeAfterConversion` from `metrics.go`.
- Produces:
  - `type Converter struct`
  - `func NewConverter(workers int, lossless bool) *Converter` — sets `VIPS_CONCURRENCY=1` if unset, calls `bimg.Initialize()`, `bimg.VipsCacheSetMax(0)`, `bimg.VipsCacheSetMaxMem(0)`.
  - `func (c *Converter) ToWebP(ctx context.Context, src []byte, quality int) ([]byte, error)`
  - `func (c *Converter) Workers() int`

- [ ] **Step 1: Write failing tests in `converter_test.go`**

```go
package main

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func isWebP(b []byte) bool {
	return len(b) >= 12 && bytes.HasPrefix(b, []byte("RIFF")) && bytes.Equal(b[8:12], []byte("WEBP"))
}

func TestToWebPConvertsPNG(t *testing.T) {
	c := NewConverter(2, false)
	out, err := c.ToWebP(context.Background(), pngBytes(t), 75)
	if err != nil {
		t.Fatal(err)
	}
	if !isWebP(out) {
		t.Fatalf("output is not WebP: %q", out[:min(16, len(out))])
	}
}

func TestToWebPQualityChangesOutput(t *testing.T) {
	c := NewConverter(2, false)
	src := pngBytes(t)
	lo, err := c.ToWebP(context.Background(), src, 10)
	if err != nil {
		t.Fatal(err)
	}
	hi, err := c.ToWebP(context.Background(), src, 100)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(lo, hi) {
		t.Fatal("quality 10 and 100 produced identical output")
	}
}

func TestToWebPRejectsGarbage(t *testing.T) {
	c := NewConverter(1, false)
	if _, err := c.ToWebP(context.Background(), []byte("not an image"), 75); err == nil {
		t.Fatal("want error for garbage input")
	}
}

func TestToWebPHonoursContextWhileWaitingForWorker(t *testing.T) {
	c := NewConverter(1, false)
	c.sem <- struct{}{} // occupy the only worker
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := c.ToWebP(ctx, pngBytes(t), 75)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
}

func TestToWebPLimitsConcurrency(t *testing.T) {
	c := NewConverter(2, false)
	var inFlight, peak atomic.Int32
	src := pngBytes(t)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.sem <- struct{}{}
			n := inFlight.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			inFlight.Add(-1)
			<-c.sem
		}()
	}
	wg.Wait()
	if peak.Load() > 2 {
		t.Fatalf("semaphore admitted %d workers, want <= 2", peak.Load())
	}
	if _, err := c.ToWebP(context.Background(), src, 75); err != nil {
		t.Fatal(err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `make test`
Expected: `undefined: NewConverter`.

- [ ] **Step 3: Create `converter.go`**

```go
package main

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/h2non/bimg"
)

var initVips sync.Once

// Converter turns source images into WebP with at most `workers` conversions
// running at once. Parallelism comes from concurrent requests, so libvips
// itself is pinned to one thread per operation.
type Converter struct {
	sem      chan struct{}
	lossless bool
}

func NewConverter(workers int, lossless bool) *Converter {
	if workers <= 0 {
		workers = 1
	}
	initVips.Do(func() {
		if os.Getenv("VIPS_CONCURRENCY") == "" {
			os.Setenv("VIPS_CONCURRENCY", "1")
		}
		bimg.Initialize()
		// A proxy sees mostly unique inputs; the vips operation cache would
		// only pin memory.
		bimg.VipsCacheSetMax(0)
		bimg.VipsCacheSetMaxMem(0)
	})
	return &Converter{sem: make(chan struct{}, workers), lossless: lossless}
}

func (c *Converter) Workers() int { return cap(c.sem) }

// ToWebP converts src. It waits for a free worker, honouring ctx.
func (c *Converter) ToWebP(ctx context.Context, src []byte, quality int) ([]byte, error) {
	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-c.sem }()

	out, err := bimg.NewImage(src).Process(bimg.Options{
		Quality:  quality,
		Lossless: c.lossless,
		Type:     bimg.WEBP,
	})
	if err != nil {
		return nil, fmt.Errorf("converting image: %w", err)
	}
	totalImageSizeAfterConversion.Add(float64(len(out)))
	return out, nil
}
```

- [ ] **Step 4: Run tests**

Run: `make vet && make test`
Expected: all pass. If `bimg.Initialize` is reported undefined, check `go doc github.com/h2non/bimg Initialize` in `make sh`; bimg v1.1.9 exports it. If `TestToWebPQualityChangesOutput` fails because the 8×8 test image is too small for quality to matter, enlarge the image in `pngBytes` to 64×64 with a gradient (`img.Pix[i] = byte(i * 7)`); do not weaken the assertion.

- [ ] **Step 5: Commit**

```bash
git add converter.go converter_test.go
git commit -m "feat: CPU-bounded libvips WebP converter with single-threaded vips ops

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 6: Coalescing handler with conditional responses (`handler.go`)

**Files:**
- Create: `handler.go`, `handler_test.go`
- Modify: `go.mod` (`golang.org/x/sync` via `make tidy`)

**Interfaces:**
- Consumes: `Config` (Task 2), `ImageCache`/`Entry`/`cacheKey` (Task 3), `Fetcher`/`UpstreamStatusError` (Task 4), `Converter` (Task 5), metrics and `statusRecorder` (Task 2), `resolveQuality`, `buildImageURL`.
- Produces:
  - `type Handler struct`
  - `func NewHandler(cfg *Config, cache *ImageCache, fetcher *Fetcher, conv *Converter, log *logrus.Logger) *Handler`
  - `func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request)`
  - `h.upstreamScheme string` field, default `"https://"`, overridable in tests.
  - `func hostOnly(hostport string) string` — strips a `:port` suffix.

- [ ] **Step 1: Write failing tests in `handler_test.go`**

```go
package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/sirupsen/logrus"
)

type handlerFixture struct {
	h        *Handler
	cache    *ImageCache
	origin   *httptest.Server
	fetches  atomic.Int32
	gate     chan struct{} // when non-nil, origin blocks until closed
	notFound atomic.Bool
}

func newFixture(t *testing.T) *handlerFixture {
	t.Helper()
	fx := &handlerFixture{}
	data := pngBytes(t)
	fx.origin = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fx.fetches.Add(1)
		if fx.gate != nil {
			<-fx.gate
		}
		if fx.notFound.Load() {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Write(data)
	}))
	t.Cleanup(fx.origin.Close)

	cfg := &Config{}
	cfg.AllowedHosts = map[string]string{"cdn.test": strings.TrimPrefix(fx.origin.URL, "http://")}
	cfg.applyDefaults()
	cfg.Cache.CacheEnabled = true
	cfg.Cache.MaxBytes = 8 << 20
	cfg.Cache.NegativeTTLSeconds = 60
	cfg.HTTPClient.TimeoutSeconds = 2

	var err error
	fx.cache, err = NewImageCache(cfg.Cache.MaxBytes, cfg.NegativeTTL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fx.cache.Close)

	log := logrus.New()
	log.Out = &bytes.Buffer{}
	fx.h = NewHandler(cfg, fx.cache, NewFetcher(newHTTPClient(cfg), cfg.Limits.MaxImageBytes, 16), NewConverter(2, false), log)
	fx.h.upstreamScheme = "http://"
	return fx
}

func (fx *handlerFixture) get(path string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = "cdn.test"
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	fx.h.ServeHTTP(rec, req)
	return rec
}

func TestHandlerServesWebPWithETagAndCacheControl(t *testing.T) {
	fx := newFixture(t)
	rec := fx.get("/a.png", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !isWebP(rec.Body.Bytes()) {
		t.Fatal("body is not WebP")
	}
	if rec.Header().Get("Content-Type") != "image/webp" {
		t.Fatalf("Content-Type %q", rec.Header().Get("Content-Type"))
	}
	if rec.Header().Get("ETag") == "" {
		t.Fatal("missing ETag")
	}
	if rec.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Fatalf("Cache-Control %q", rec.Header().Get("Cache-Control"))
	}
}

func TestHandlerReturns304WhenETagMatches(t *testing.T) {
	fx := newFixture(t)
	first := fx.get("/a.png", nil)
	etag := first.Header().Get("ETag")
	fx.cache.Wait()
	second := fx.get("/a.png", map[string]string{"If-None-Match": etag})
	if second.Code != http.StatusNotModified {
		t.Fatalf("want 304, got %d", second.Code)
	}
	if second.Body.Len() != 0 {
		t.Fatal("304 must have empty body")
	}
}

func TestHandlerServesSecondRequestFromCache(t *testing.T) {
	fx := newFixture(t)
	fx.get("/a.png", nil)
	fx.cache.Wait()
	before := testutil.ToFloat64(cacheHitsTotal)
	fx.get("/a.png", nil)
	if fx.fetches.Load() != 1 {
		t.Fatalf("want 1 origin fetch, got %d", fx.fetches.Load())
	}
	if testutil.ToFloat64(cacheHitsTotal)-before != 1 {
		t.Fatal("cache hit counter did not increase")
	}
}

func TestHandlerCoalescesConcurrentMisses(t *testing.T) {
	fx := newFixture(t)
	fx.gate = make(chan struct{})
	const clients = 50
	var wg sync.WaitGroup
	codes := make([]int, clients)
	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = fx.get("/hot.png", nil).Code
		}(i)
	}
	time.Sleep(150 * time.Millisecond) // let every client reach singleflight
	close(fx.gate)
	wg.Wait()
	for i, c := range codes {
		if c != http.StatusOK {
			t.Fatalf("client %d got %d", i, c)
		}
	}
	if fx.fetches.Load() != 1 {
		t.Fatalf("want exactly 1 origin fetch for %d concurrent clients, got %d", clients, fx.fetches.Load())
	}
}

func TestHandlerCacheKeyIncludesQuality(t *testing.T) {
	fx := newFixture(t)
	a := fx.get("/a.png", map[string]string{"x-webp-quality": "20"})
	fx.cache.Wait()
	b := fx.get("/a.png", map[string]string{"x-webp-quality": "95"})
	if a.Header().Get("ETag") == b.Header().Get("ETag") {
		t.Fatal("different qualities served identical bytes; cache key ignores quality")
	}
	if fx.fetches.Load() != 2 {
		t.Fatalf("want 2 fetches, got %d", fx.fetches.Load())
	}
}

func TestHandlerNegativeCachesOrigin404(t *testing.T) {
	fx := newFixture(t)
	fx.notFound.Store(true)
	if rec := fx.get("/gone.png", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rec.Code)
	}
	fx.cache.Wait()
	before := testutil.ToFloat64(negativeHitsTotal)
	if rec := fx.get("/gone.png", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("want 404 from negative cache, got %d", rec.Code)
	}
	if fx.fetches.Load() != 1 {
		t.Fatalf("want 1 origin fetch, got %d", fx.fetches.Load())
	}
	if testutil.ToFloat64(negativeHitsTotal)-before != 1 {
		t.Fatal("negative hit counter did not increase")
	}
}

func TestHandlerRejectsUnknownHost(t *testing.T) {
	fx := newFixture(t)
	before := testutil.ToFloat64(requestsTotal.WithLabelValues("403", "GET"))
	req := httptest.NewRequest(http.MethodGet, "/a.png", nil)
	req.Host = "not-allowed.test"
	rec := httptest.NewRecorder()
	fx.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", rec.Code)
	}
	if testutil.ToFloat64(requestsTotal.WithLabelValues("403", "GET"))-before != 1 {
		t.Fatal("403 was not recorded in requestsTotal")
	}
}

func TestHandlerStripsPortFromHost(t *testing.T) {
	fx := newFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/a.png", nil)
	req.Host = "cdn.test:8080"
	rec := httptest.NewRecorder()
	fx.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 for host with port, got %d", rec.Code)
	}
}

func TestHandlerRejectsNonGetMethods(t *testing.T) {
	fx := newFixture(t)
	req := httptest.NewRequest(http.MethodPost, "/a.png", nil)
	req.Host = "cdn.test"
	rec := httptest.NewRecorder()
	fx.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d", rec.Code)
	}
}

func TestHandlerHeadReturnsHeadersOnly(t *testing.T) {
	fx := newFixture(t)
	fx.get("/a.png", nil)
	fx.cache.Wait()
	req := httptest.NewRequest(http.MethodHead, "/a.png", nil)
	req.Host = "cdn.test"
	rec := httptest.NewRecorder()
	fx.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 || rec.Header().Get("Content-Length") == "" {
		t.Fatalf("HEAD: code=%d body=%d content-length=%q", rec.Code, rec.Body.Len(), rec.Header().Get("Content-Length"))
	}
}

func TestHandlerSkipsCacheWhenDisabled(t *testing.T) {
	fx := newFixture(t)
	fx.h.cfg.Cache.CacheEnabled = false
	fx.get("/a.png", nil)
	fx.get("/a.png", nil)
	fx.cache.Wait()
	if fx.fetches.Load() != 2 {
		t.Fatalf("want 2 fetches with cache disabled, got %d", fx.fetches.Load())
	}
	if _, ok := fx.cache.Get(cacheKey(fx.h.upstreamScheme+fx.h.cfg.AllowedHosts["cdn.test"]+"/a.png", 75)); ok {
		t.Fatal("cache must stay empty when disabled")
	}
}

func TestHandlerNoCacheHeaderBypassesCache(t *testing.T) {
	fx := newFixture(t)
	fx.get("/a.png", nil)
	fx.cache.Wait()
	fx.get("/a.png", map[string]string{"X-No-Cache": "true"})
	if fx.fetches.Load() != 2 {
		t.Fatalf("want 2 fetches, got %d", fx.fetches.Load())
	}
}

func TestHostOnly(t *testing.T) {
	for in, want := range map[string]string{"a.test": "a.test", "a.test:8080": "a.test", "[::1]:80": "::1"} {
		if got := hostOnly(in); got != want {
			t.Errorf("%q: want %q, got %q", in, want, got)
		}
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `make test`
Expected: `undefined: NewHandler`, `undefined: hostOnly`.

- [ ] **Step 3: Create `handler.go`**

```go
package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"golang.org/x/sync/singleflight"
)

// Handler serves converted images. It is safe for concurrent use.
type Handler struct {
	cfg            *Config
	cache          *ImageCache
	fetcher        *Fetcher
	conv           *Converter
	log            *logrus.Logger
	group          singleflight.Group
	upstreamScheme string
}

func NewHandler(cfg *Config, cache *ImageCache, fetcher *Fetcher, conv *Converter, log *logrus.Logger) *Handler {
	return &Handler{
		cfg:            cfg,
		cache:          cache,
		fetcher:        fetcher,
		conv:           conv,
		log:            log,
		upstreamScheme: "https://",
	}
}

// hostOnly strips an optional :port from a Host header value.
func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return strings.TrimSuffix(strings.TrimPrefix(hostport, "["), "]")
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rec := &statusRecorder{ResponseWriter: w}
	defer func() {
		code := strconv.Itoa(rec.Status())
		requestsTotal.WithLabelValues(code, r.Method).Inc()
		responseDuration.WithLabelValues(code, r.Method).Observe(time.Since(start).Seconds())
	}()

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(rec, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	origin, ok := h.cfg.AllowedHosts[hostOnly(r.Host)]
	if !ok {
		invalidHostsCount.Inc()
		http.Error(rec, "Host not allowed", http.StatusForbidden)
		return
	}

	quality := resolveQuality(r.Header.Get("x-webp-quality"), h.cfg.WebP.Quality)
	imageURL := buildImageURL(h.upstreamScheme, origin, r.URL.Path, r.URL.RawQuery)
	key := cacheKey(imageURL, quality)
	useCache := h.cfg.Cache.CacheEnabled && r.Header.Get(h.cfg.Cache.NoCacheHeader) != "true"

	if useCache {
		if e, hit := h.cache.Get(key); hit {
			cacheHitsTotal.Inc()
			h.writeEntry(rec, r, e)
			return
		}
		if code, hit := h.cache.GetNegative(key); hit {
			negativeHitsTotal.Inc()
			http.Error(rec, http.StatusText(code), code)
			return
		}
		cacheMissesTotal.Inc()
	}

	v, err, shared := h.group.Do(key, func() (any, error) {
		return h.produce(imageURL, quality)
	})
	if shared {
		coalescedTotal.Inc()
	}
	if err != nil {
		var us *UpstreamStatusError
		if errors.As(err, &us) && (us.Code == http.StatusNotFound || us.Code == http.StatusGone) {
			if useCache {
				h.cache.SetNegative(key, http.StatusNotFound)
			}
			http.Error(rec, "Not Found", http.StatusNotFound)
			return
		}
		errorsTotal.Inc()
		h.log.WithFields(logrus.Fields{"url": imageURL, "error": err.Error()}).Error("image processing failed")
		http.Error(rec, "Bad Gateway", http.StatusBadGateway)
		return
	}

	e := v.(Entry)
	if useCache {
		h.cache.Set(key, e)
	}
	h.writeEntry(rec, r, e)
}

// produce fetches and converts one image. It runs under a detached context
// so that a client disconnecting does not fail the other coalesced waiters.
func (h *Handler) produce(imageURL string, quality int) (Entry, error) {
	timeout := time.Duration(h.cfg.HTTPClient.TimeoutSeconds) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout+5*time.Second)
	defer cancel()

	body, err := h.fetcher.Fetch(ctx, imageURL)
	if err != nil {
		return Entry{}, err
	}
	defer body.Release()

	data, err := h.conv.ToWebP(ctx, body.Bytes(), quality)
	if err != nil {
		return Entry{}, err
	}
	return NewEntry(data), nil
}

func (h *Handler) writeEntry(w http.ResponseWriter, r *http.Request, e Entry) {
	hdr := w.Header()
	hdr.Set("ETag", e.ETag)
	if cc := h.cfg.HTTPServer.CacheControl; cc != "" {
		hdr.Set("Cache-Control", cc)
	}
	if r.Header.Get("If-None-Match") == e.ETag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	hdr.Set("Content-Type", "image/webp")
	hdr.Set("Content-Length", strconv.Itoa(len(e.Data)))
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.Write(e.Data)
}
```

- [ ] **Step 4: Add the dependency and run tests**

Run: `make tidy && make vet && make test`
Expected: `go.mod` gains `golang.org/x/sync`; all pass. `TestHandlerCoalescesConcurrentMisses` is the key assertion of this whole plan: 50 clients, 1 origin fetch. If it flakes because some clients arrive after the leader finished, raise the sleep to 300 ms; do not relax the fetch count.

- [ ] **Step 5: Commit**

```bash
git add handler.go handler_test.go go.mod go.sum
git commit -m "feat: coalescing handler with ETag/304, Cache-Control and negative caching

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 7: Servers, wiring, and removal of the old code path

**Files:**
- Create: `server.go`
- Rewrite: `main.go`
- Delete: `main_test.go` (its remaining tests are re-homed below)
- Modify: `go.mod` (`go 1.22`, drop gorilla/mux and hashicorp lru via `make tidy`), `Dockerfile.dev` already on 1.22

**Interfaces:**
- Consumes: everything from Tasks 2–6.
- Produces:
  - `func newRouter(images http.Handler) *http.ServeMux` — `GET /health` and `HEAD /health` → `healthCheckHandler`; `/` → `images`.
  - `func newPublicServer(cfg *Config, handler http.Handler) *http.Server`
  - `func newMetricsServer(cfg *Config) *http.Server`
  - `func newPprofServer() *http.Server` — `localhost:6060`, `http.DefaultServeMux`.
  - `func basicAuthMiddleware(user, pass string, next http.Handler) http.Handler`
  - `func healthCheckHandler(w http.ResponseWriter, r *http.Request)`
  - `func runServers(log *logrus.Logger, servers ...*http.Server)` — blocks until SIGINT/SIGTERM or a listener error, then shuts all down within 10 s.
  - `func newLogger(level string) *logrus.Logger`

- [ ] **Step 1: Write failing tests in `server_test.go`**

```go
package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sirupsen/logrus"
)

func TestHealthIgnoresHostAllowlist(t *testing.T) {
	fx := newFixture(t)
	router := newRouter(fx.h)
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Host = "not-allowed.test"
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "OK" {
		t.Fatalf("want 200 OK, got %d %q", rec.Code, rec.Body.String())
	}
}

func TestMetricsNotServedOnPublicPort(t *testing.T) {
	fx := newFixture(t)
	router := newRouter(fx.h)
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Host = "cdn.test"
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if bytes.Contains(rec.Body.Bytes(), []byte("ngax_http_requests_total")) {
		t.Fatal("metrics exposed on the public router")
	}
}

func TestMetricsServerRequiresBasicAuth(t *testing.T) {
	cfg := &Config{}
	cfg.applyDefaults()
	cfg.Exporter.User, cfg.Exporter.Password = "foo", "bar"
	srv := newMetricsServer(cfg)

	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 without credentials, got %d", rec.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.SetBasicAuth("foo", "bar")
	rec = httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte("ngax_")) {
		t.Fatalf("want 200 with metrics, got %d", rec.Code)
	}
}

func TestPublicServerHasTimeouts(t *testing.T) {
	cfg := &Config{}
	cfg.applyDefaults()
	srv := newPublicServer(cfg, http.NotFoundHandler())
	if srv.ReadHeaderTimeout == 0 || srv.IdleTimeout == 0 || srv.MaxHeaderBytes == 0 {
		t.Fatalf("timeouts unset: rht=%v idle=%v mhb=%d", srv.ReadHeaderTimeout, srv.IdleTimeout, srv.MaxHeaderBytes)
	}
}

func TestNewLoggerParsesLevel(t *testing.T) {
	if newLogger("debug").Level != logrus.DebugLevel {
		t.Fatal("debug level not applied")
	}
	if newLogger("nonsense").Level != logrus.InfoLevel {
		t.Fatal("invalid level must fall back to info")
	}
}
```

- [ ] **Step 2: Delete `main_test.go`**

`git rm main_test.go`. Its surviving tests now exist in `config_test.go`, `handler_test.go` (`TestHandlerRejectsUnknownHost` replaces `TestHandleRequestRecordsActualStatusCode`; `TestHandlerSkipsCacheWhenDisabled` replaces `TestHandleRequestSkipsCacheWhenDisabled`) and `server_test.go`.

- [ ] **Step 3: Run tests to verify they fail**

Run: `make test`
Expected: `undefined: newRouter`, `undefined: newMetricsServer`, `undefined: newPublicServer`, `undefined: newLogger` (note: `newLogger` currently exists in `main.go` with no arguments — the arity error also counts as a failure).

- [ ] **Step 4: Create `server.go`**

```go
package main

import (
	"context"
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
)

func newLogger(level string) *logrus.Logger {
	l := logrus.New()
	l.Out = os.Stdout
	l.Formatter = &logrus.JSONFormatter{}
	lvl, err := logrus.ParseLevel(level)
	if err != nil {
		lvl = logrus.InfoLevel
	}
	l.Level = lvl
	return l
}

// newRouter registers fixed routes before the catch-all so they are never
// subject to the host allowlist.
func newRouter(images http.Handler) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", healthCheckHandler)
	mux.HandleFunc("HEAD /health", healthCheckHandler)
	mux.Handle("/", images)
	return mux
}

func healthCheckHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

func newPublicServer(cfg *Config, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              fmt.Sprintf("%s:%d", cfg.HTTPServer.BindIP, cfg.HTTPServer.Port),
		Handler:           handler,
		ReadHeaderTimeout: time.Duration(cfg.HTTPServer.ReadHeaderTimeoutSeconds) * time.Second,
		IdleTimeout:       time.Duration(cfg.HTTPServer.IdleTimeoutSeconds) * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
}

func newMetricsServer(cfg *Config) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.Handler())
	return &http.Server{
		Addr:              fmt.Sprintf("%s:%d", cfg.Exporter.BindIP, cfg.Exporter.Port),
		Handler:           basicAuthMiddleware(cfg.Exporter.User, cfg.Exporter.Password, mux),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}

// newPprofServer exposes net/http/pprof (registered on the default mux by
// the blank import) to the local machine only.
func newPprofServer() *http.Server {
	return &http.Server{
		Addr:              "localhost:6060",
		Handler:           http.DefaultServeMux,
		ReadHeaderTimeout: 5 * time.Second,
	}
}

func basicAuthMiddleware(user, pass string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != user || p != pass {
			w.Header().Set("WWW-Authenticate", `Basic realm="Restricted"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// runServers starts every server, then blocks until SIGINT/SIGTERM or a
// listener failure, and shuts them all down.
func runServers(log *logrus.Logger, servers ...*http.Server) {
	errChan := make(chan error, len(servers))
	for _, s := range servers {
		go func(s *http.Server) {
			log.Infof("listening on http://%s", s.Addr)
			if err := s.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				errChan <- fmt.Errorf("%s: %w", s.Addr, err)
			}
		}(s)
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	select {
	case sig := <-stop:
		log.Infof("received %s, shutting down", sig)
	case err := <-errChan:
		log.Errorf("listener failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, s := range servers {
		if err := s.Shutdown(ctx); err != nil {
			log.Errorf("forced shutdown of %s: %v", s.Addr, err)
		}
	}
	log.Info("servers stopped")
}
```

- [ ] **Step 5: Rewrite `main.go` as wiring only**

Replace the entire file with:

```go
package main

import (
	"github.com/sirupsen/logrus"
)

func main() {
	if err := loadConfig("."); err != nil {
		logrus.Fatal(err)
	}
	log := newLogger(config.Log.Level)
	if config.Cache.LruCache != 0 {
		log.Warn("cache.lru_cache is deprecated and ignored; use cache.max_bytes")
	}

	cache, err := NewImageCache(config.Cache.MaxBytes, config.NegativeTTL())
	if err != nil {
		log.Fatal(err)
	}
	defer cache.Close()

	fetcher := NewFetcher(newHTTPClient(&config), config.Limits.MaxImageBytes, config.Concurrency.MaxFetches)
	conv := NewConverter(config.Concurrency.MaxConversions, config.WebP.Lossless)
	handler := NewHandler(&config, cache, fetcher, conv, log)

	log.WithFields(logrus.Fields{
		"conversion_workers": conv.Workers(),
		"max_fetches":        config.Concurrency.MaxFetches,
		"cache_max_bytes":    config.Cache.MaxBytes,
		"hosts":              len(config.AllowedHosts),
	}).Info("ngax starting")

	runServers(log,
		newPublicServer(&config, newRouter(handler)),
		newMetricsServer(&config),
		newPprofServer(),
	)
}
```

- [ ] **Step 6: Bump Go version and tidy**

In `go.mod` change `go 1.21.4` to `go 1.22`. Run `make tidy`. Expected: `github.com/gorilla/mux` and `github.com/hashicorp/golang-lru/v2` disappear from `go.mod`.

- [ ] **Step 7: Run everything**

Run: `make vet && make test && make build`
Expected: gofmt clean, vet clean, all tests pass, `ngax` binary produced.

- [ ] **Step 8: Smoke test the binary**

```bash
make sh
# inside the container:
cp config.yaml.sample config.yaml
./ngax &
sleep 1
wget -qO- http://127.0.0.1:8080/health; echo
wget -qS --header 'Host: nope' -O /dev/null http://127.0.0.1:8080/x.jpg 2>&1 | head -1
wget -qO- --header "Authorization: Basic $(echo -n foo:bar | base64)" http://127.0.0.1:9080/metrics | grep -c '^ngax_'
kill -INT %1; wait
rm config.yaml
```

Expected: `OK`; `HTTP/1.1 403 Forbidden`; a count ≥ 10; JSON log lines `received interrupt, shutting down` and `servers stopped`.

- [ ] **Step 9: Commit**

```bash
git add server.go server_test.go main.go go.mod go.sum
git rm -q main_test.go
git commit -m "refactor: wire new components, stdlib router, graceful multi-server shutdown

Removes gorilla/mux and the count-bounded LRU.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 8: Runtime image, compose, config sample and README

**Files:**
- Rewrite: `Dockerfile`
- Modify: `docker-compose.yml`, `config.yaml.sample`, `README.md`, `.dockerignore`

**Interfaces:**
- Consumes: config keys from Task 2.

- [ ] **Step 1: Rewrite `Dockerfile` as a multi-stage build**

```dockerfile
# syntax=docker/dockerfile:1
FROM golang:1.22-alpine AS build
RUN apk --no-cache add build-base vips-dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -buildvcs=false -trimpath -ldflags='-s -w' -o /ngax .

FROM alpine:3.20
RUN apk --no-cache add vips ca-certificates tzdata \
    && addgroup -S ngax && adduser -S -G ngax ngax
WORKDIR /app
COPY --from=build /ngax /app/ngax
USER ngax
ENV VIPS_CONCURRENCY=1 \
    MALLOC_ARENA_MAX=2
EXPOSE 8080 9080
HEALTHCHECK --interval=30s --timeout=3s CMD wget -qO- http://127.0.0.1:8080/health || exit 1
CMD ["/app/ngax"]
```

`MALLOC_ARENA_MAX=2` limits glibc/musl arena growth from libvips' C allocations under many threads; it is a documented libvips deployment recommendation.

- [ ] **Step 2: Build the image locally**

Run: `docker build -t ngax:local .`
Expected: success. Then `docker run --rm ngax:local /app/ngax` must exit with the JSON fatal `reading config file` (no config mounted), proving the binary starts and libvips loads. If it fails with a missing shared library, add that package to the runtime `apk add` line (the vips package pulls its codecs, but check `libheif`/`libjxl` names on the chosen Alpine version).

- [ ] **Step 3: Update `docker-compose.yml`**

```yaml
services:
  image_processing:
    image: ghcr.io/mohammad362/ngax/ngax:latest
    container_name: image_processing
    ports:
      - "127.0.0.1:8080:8080"
      - "127.0.0.1:9080:9080"
    environment:
      # Keep ~1.5x cache.max_bytes so the Go GC runs before the container limit.
      GOMEMLIMIT: 1536MiB
    deploy:
      resources:
        limits:
          memory: 2g
    restart: on-failure
    volumes:
      - ./config.yaml:/app/config.yaml:ro
    logging:
      driver: "json-file"
      options:
        max-size: 10m
        max-file: "1"
```

- [ ] **Step 4: Update `config.yaml.sample`**

Replace the `cache:`, `concurrency:`, `http_client:` and `http_server:` sections and add `log:`:

```yaml
# Caching Settings
cache:
  cache_enabled: true
  nocache_header: "X-No-Cache"      # Request header that bypasses the cache when set to "true"
  max_bytes: 1073741824             # Memory ceiling for cached images (1 GiB)
  negative_ttl_seconds: 30          # Remember origin 404s for this long; -1 disables

# Upstream Limits
limits:
  max_image_bytes: 20971520         # Reject upstream images larger than this (20 MiB)

# Concurrency Settings
concurrency:
  max_conversions: 0                # Concurrent libvips conversions; 0 = number of CPUs
  max_fetches: 512                  # Concurrent origin fetches

# HTTP Client Settings (seconds)
http_client:
  timeout_seconds: 30
  dial_timeout_seconds: 5
  keep_alive: 30
  TLS_handshake_timeout: 10
  response_header_timeout: 30
  expect_continue_timeout: 1
  idle_conn_timeout: 90
  max_idle_conns_per_host: 256      # Idle connections kept per origin host

http_server:
  bind_ip: "127.0.0.1"
  port: 8080
  read_header_timeout_seconds: 5
  idle_timeout_seconds: 60
  cache_control: "public, max-age=31536000, immutable"   # Sent with every image response

log:
  level: info                       # debug | info | warn | error
```

Remove `expiration_minutes` and `lru_cache` from the sample.

- [ ] **Step 5: Update README configuration table and add a Performance section**

In `README.md`, replace the configuration table rows for `cache` and `concurrency`, add rows for `limits`, `http_server.cache_control`, `log.level`, and append:

```markdown
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
```

- [ ] **Step 6: Add `loadtest/` to `.dockerignore`**

Append `loadtest/` and `docs/` to `.dockerignore`.

- [ ] **Step 7: Verify and commit**

Run: `make test && docker build -q -t ngax:local . >/dev/null && echo IMAGE_OK`
Expected: tests pass, `IMAGE_OK`.

```bash
git add Dockerfile docker-compose.yml config.yaml.sample README.md .dockerignore
git commit -m "build: multi-stage runtime image, memory limit guidance, updated config sample

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 9: Benchmarks and load-test runbook

**Files:**
- Create: `handler_bench_test.go`, `loadtest/origin/main.go`, `loadtest/run.sh`, `docs/loadtest.md`

**Interfaces:**
- Consumes: `newFixture` from `handler_test.go` is test-only and not reusable in benchmarks (it takes `*testing.T`), so the benchmark builds its own handler.

- [ ] **Step 1: Write `handler_bench_test.go`**

```go
package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
)

func benchHandler(b *testing.B) (*Handler, *httptest.Server) {
	b.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for i := range img.Pix {
		img.Pix[i] = byte(i * 7)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		b.Fatal(err)
	}
	data := buf.Bytes()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(data)
	}))
	b.Cleanup(origin.Close)

	cfg := &Config{}
	cfg.AllowedHosts = map[string]string{"cdn.test": strings.TrimPrefix(origin.URL, "http://")}
	cfg.applyDefaults()
	cfg.Cache.CacheEnabled = true
	cache, err := NewImageCache(cfg.Cache.MaxBytes, cfg.NegativeTTL())
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(cache.Close)
	log := logrus.New()
	log.Out = &bytes.Buffer{}
	h := NewHandler(cfg, cache, NewFetcher(newHTTPClient(cfg), cfg.Limits.MaxImageBytes, 64), NewConverter(0, false), log)
	h.upstreamScheme = "http://"
	return h, origin
}

// BenchmarkCacheHit measures the hot path: one lookup, one write.
func BenchmarkCacheHit(b *testing.B) {
	h, _ := benchHandler(b)
	warm := httptest.NewRequest(http.MethodGet, "/a.png", nil)
	warm.Host = "cdn.test"
	h.ServeHTTP(httptest.NewRecorder(), warm)
	h.cache.Wait()

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		req := httptest.NewRequest(http.MethodGet, "/a.png", nil)
		req.Host = "cdn.test"
		for pb.Next() {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				b.Fatalf("status %d", rec.Code)
			}
		}
	})
}

// BenchmarkCacheHit304 measures a conditional request answered with 304.
func BenchmarkCacheHit304(b *testing.B) {
	h, _ := benchHandler(b)
	warm := httptest.NewRequest(http.MethodGet, "/a.png", nil)
	warm.Host = "cdn.test"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, warm)
	h.cache.Wait()
	etag := rec.Header().Get("ETag")

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		req := httptest.NewRequest(http.MethodGet, "/a.png", nil)
		req.Host = "cdn.test"
		req.Header.Set("If-None-Match", etag)
		for pb.Next() {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusNotModified {
				b.Fatalf("status %d", rec.Code)
			}
		}
	})
}

// BenchmarkCacheMiss measures fetch + convert throughput with caching off.
func BenchmarkCacheMiss(b *testing.B) {
	h, _ := benchHandler(b)
	h.cfg.Cache.CacheEnabled = false
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		req := httptest.NewRequest(http.MethodGet, "/a.png", nil)
		req.Host = "cdn.test"
		for pb.Next() {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				b.Fatalf("status %d", rec.Code)
			}
		}
	})
}
```

Add `"image"` and `"image/png"` to the import block.

- [ ] **Step 2: Run the benchmarks and record the baseline**

Run: `make bench`
Expected output shape (numbers are machine-specific; record them in `docs/loadtest.md` step 5):

```
BenchmarkCacheHit-N        ...  ns/op   ... B/op   ... allocs/op
BenchmarkCacheHit304-N     ...
BenchmarkCacheMiss-N       ...
```

Acceptance: `BenchmarkCacheHit` allocs/op ≤ 12 (the recorder itself accounts for most); `BenchmarkCacheHit304` B/op < `BenchmarkCacheHit` B/op.

- [ ] **Step 3: Write the load-test origin `loadtest/origin/main.go`**

A separate `main` package that serves distinct generated PNGs and counts requests, so the "one fetch per image" claim can be verified end to end.

```go
// Command origin is a synthetic image origin for load testing ngAX.
// It serves /img/<n>.png with content that depends on n and exposes the
// number of requests served at /count.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"image"
	"image/png"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

var (
	count atomic.Int64
	cache sync.Map // n -> []byte
)

func imageFor(n int, size int) []byte {
	if v, ok := cache.Load(n); ok {
		return v.([]byte)
	}
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			i := img.PixOffset(x, y)
			img.Pix[i+0] = byte(x*n) ^ byte(y)
			img.Pix[i+1] = byte(y*n) ^ byte(x)
			img.Pix[i+2] = byte((x + y) * n)
			img.Pix[i+3] = 255
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		log.Fatal(err)
	}
	cache.Store(n, buf.Bytes())
	return buf.Bytes()
}

func main() {
	addr := flag.String("addr", "127.0.0.1:9000", "listen address")
	size := flag.Int("size", 512, "image side in pixels")
	flag.Parse()

	http.HandleFunc("/count", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, count.Load())
	})
	http.HandleFunc("/reset", func(w http.ResponseWriter, r *http.Request) {
		count.Store(0)
		fmt.Fprintln(w, "ok")
	})
	http.HandleFunc("/img/", func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/img/"), ".png")
		n, err := strconv.Atoi(name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Write(imageFor(n, *size))
	})
	log.Printf("origin listening on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}
```

- [ ] **Step 4: Write `loadtest/run.sh`**

The origin runs on the host network inside the dev container; ngAX must be configured with `allowed_hosts: {loadtest.local: "127.0.0.1:9000"}` and, because the fetcher uses `https://`, the load test runs ngAX with the test-only scheme override. Rather than add a production flag for that, the runbook runs a TLS-terminating origin: `run.sh` starts the origin behind a self-signed TLS listener using `openssl` + `socat`? That adds tooling. **Decision: add a documented, off-by-default config key `upstream_scheme` (default `https://`) in `config.go`** so load tests can point at a plain-HTTP origin. Do this now:

In `config.go`, add to `Config`:

```go
	UpstreamScheme string `mapstructure:"upstream_scheme"` // "https://" (default) or "http://" for local testing
```

In `applyDefaults`: `setDefaultString(&c.UpstreamScheme, "https://")`. In `validate`: reject anything other than `"https://"` or `"http://"`. In `NewHandler`: `upstreamScheme: cfg.UpstreamScheme` instead of the literal (tests that set `fx.h.upstreamScheme = "http://"` keep working; `TestLoadConfigAppliesDefaults` gets a row `{"upstream_scheme", c.UpstreamScheme, "https://"}`). Add a test:

```go
func TestLoadConfigRejectsBadUpstreamScheme(t *testing.T) {
	dir := writeConfig(t, minimalYAML+"upstream_scheme: \"ftp://\"\n")
	config = Config{}
	if err := loadConfig(dir); err == nil {
		t.Fatal("want error for ftp:// scheme")
	}
}
```

Run `make test` (fails on the new test), implement, run again (passes).

Then `loadtest/run.sh`:

```bash
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
  vegeta attack -targets=loadtest/targets.txt -rate=200 -duration=5s -max-workers=100 | vegeta report | sed -n '1,8p'
  echo '== origin fetches after warm-up (expect 1000):'; wget -qO- http://127.0.0.1:9000/count
  echo '== steady state at ${RATE} rps for ${DURATION}'
  vegeta attack -targets=loadtest/targets.txt -rate=${RATE} -duration=${DURATION} -max-workers=500 | tee /tmp/results.bin | vegeta report
  echo '== origin fetches total (expect still 1000):'; wget -qO- http://127.0.0.1:9000/count
  echo '== ngax metrics'
  wget -qO- --header 'Authorization: Basic bHQ6bHQ=' http://127.0.0.1:9080/metrics | grep -E '^ngax_(cache_hits_total|cache_misses_total|coalesced_requests_total|cache_bytes|http_errors_total) '
  echo '== RSS (KiB)'; grep VmRSS /proc/\$(pgrep -f /tmp/ngax)/status
  echo '== 30s CPU profile -> loadtest/cpu.pprof'
  vegeta attack -targets=loadtest/targets.txt -rate=${RATE} -duration=35s -max-workers=500 >/dev/null &
  wget -qO loadtest/cpu.pprof 'http://127.0.0.1:6060/debug/pprof/profile?seconds=30'
  wait
  pkill -INT -f /tmp/ngax || true; pkill -f /tmp/origin || true
"
rm -f "$ROOT/loadtest/config.yaml" "$ROOT/loadtest/targets.txt"
echo "Profile saved to loadtest/cpu.pprof; inspect with: make sh -> go tool pprof -top loadtest/cpu.pprof"
```

Make it executable: `chmod +x loadtest/run.sh`. Add `loadtest/cpu.pprof`, `loadtest/config.yaml`, `loadtest/targets.txt` to `.gitignore`.

- [ ] **Step 5: Write `docs/loadtest.md`**

```markdown
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

Baseline recorded on <machine, date>: fill in after Task 9 step 2.

| Benchmark | ns/op | B/op | allocs/op |
|-----------|-------|------|-----------|
| BenchmarkCacheHit | | | |
| BenchmarkCacheHit304 | | | |
| BenchmarkCacheMiss | | | |

## Tuning knobs

- `concurrency.max_conversions`: raise only if CPU is idle while requests
  queue; the default (NumCPU) is right for a dedicated host.
- `concurrency.max_fetches`: raise if origin latency is high and misses queue
  while CPU is idle.
- `cache.max_bytes` and `GOMEMLIMIT`: keep GOMEMLIMIT ≈ 1.5× max_bytes.
- `http_client.max_idle_conns_per_host`: raise toward `max_fetches` if the
  origin shows many new connections.
```

Fill the baseline table with the numbers from Step 2.

- [ ] **Step 6: Run the load test once and fill in the results**

Run: `make image && sh loadtest/run.sh 1000 30s`
Expected: origin count prints `1000` twice; vegeta `Success [ratio] 100.00%`; errors `0`. Paste the p50/p99 and RSS into a "Last run" line under the pass table in `docs/loadtest.md`. If the origin count is above 1000, the coalescing or cache path is broken; stop and fix before committing.

- [ ] **Step 7: Commit**

```bash
git add handler_bench_test.go loadtest/ docs/loadtest.md .gitignore config.go config_test.go handler.go
git commit -m "test: benchmarks, synthetic origin and load-test runbook

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 10: Tag the release

- [ ] **Step 1: Final verification**

Run: `make vet && make test && make bench && docker build -q -t ngax:local . >/dev/null && echo ALL_OK`
Expected: `ALL_OK`.

- [ ] **Step 2: Tag and push**

```bash
git tag -a v2.0.0 -m "v2.0.0: high-performance rewrite (coalescing, byte-bounded cache, ETag/304)"
git push origin master
git push origin v2.0.0
```

The CI workflow (`.github/workflows/main.yml`) runs `go test` in `golang:alpine` and publishes `ghcr.io/mohammad362/ngax/ngax:2.0.0`. The workflow's test job installs `vips-dev` already; no change needed. If the CI Go version is below 1.22, pin the job's `container:` to `golang:1.22-alpine`.

---

## Self-review

**Spec coverage**

| Spec item | Task |
|-----------|------|
| One fetch + one conversion per unique (URL, quality) | 6 (singleflight, `TestHandlerCoalescesConcurrentMisses`), verified end to end in 9 |
| Byte-bounded cache | 3 |
| Cache key includes quality | 3 (`cacheKey`), 6 (`TestHandlerCacheKeyIncludesQuality`) |
| Separate fetch vs conversion concurrency | 4, 5 |
| libvips single-threaded per op, vips cache off | 5, 8 (`VIPS_CONCURRENCY=1` in image) |
| `MaxIdleConnsPerHost`, HTTP/2 | 2 |
| Negative caching | 3, 6 |
| `Cache-Control`, `ETag`, `304`, `HEAD` | 6 |
| Pooled buffers | 4 |
| No per-request Info logging; configurable level | 6 (Error only), 7 (`newLogger`) |
| stdlib router, gorilla removed | 7 |
| Server timeouts | 7 |
| pprof, Prometheus, runbook, GOMEMLIMIT guidance | 7, 8, 9 |
| Old tests survive or migrate | 2, 4, 6, 7 |

**Placeholder scan:** `docs/loadtest.md` intentionally has a baseline table to be filled with measured numbers in Task 9 steps 2 and 6; that is data collection, not a code placeholder. No "TBD"/"similar to" remain.

**Type consistency:** `NewFetcher(client *http.Client, maxBytes int64, maxInFlight int)`, `Fetch(ctx, url) (*Body, error)`, `NewConverter(workers int, lossless bool)`, `ToWebP(ctx, src []byte, quality int) ([]byte, error)`, `NewImageCache(maxBytes int64, negativeTTL time.Duration)`, `NewHandler(cfg, cache, fetcher, conv, log)`, `buildImageURL(scheme, host, path, rawQuery)`, `basicAuthMiddleware(user, pass, next)` are used with those signatures in every task. `Handler.cfg` and `Handler.upstreamScheme` are unexported fields accessed from tests in the same package. `pngBytes(t)` is defined once in `fetcher_test.go` and used by `converter_test.go` and `handler_test.go`; `isWebP` is defined once in `converter_test.go`. `min` builtin in `converter_test.go` requires Go 1.21+, satisfied.
