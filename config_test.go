package main

import (
	"net/http"
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
		{"upstream_scheme", c.UpstreamScheme, "https://"},
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

// concurrency.max_goroutines is a removed knob from the pre-rewrite service.
// It must still decode (so an old config.yaml loads) but must not influence
// the conversion limit, which stays at its own default.
func TestLoadConfigIgnoresDeprecatedMaxGoroutines(t *testing.T) {
	dir := writeConfig(t, minimalYAML+"concurrency:\n  max_goroutines: 500\n")
	config = Config{}
	if err := loadConfig(dir); err != nil {
		t.Fatal(err)
	}
	if config.Concurrency.MaxConversions != runtime.NumCPU() {
		t.Fatalf("max_goroutines must be ignored: want max_conversions %d, got %d",
			runtime.NumCPU(), config.Concurrency.MaxConversions)
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

func TestLoadConfigRejectsBadUpstreamScheme(t *testing.T) {
	dir := writeConfig(t, minimalYAML+"upstream_scheme: \"ftp://\"\n")
	config = Config{}
	if err := loadConfig(dir); err == nil {
		t.Fatal("want error for ftp:// scheme")
	}
}

func TestNewHTTPClientUsesIdleConnLimits(t *testing.T) {
	c := &Config{}
	c.AllowedHosts = map[string]string{"a.test": "origin-a.test", "b.test": "origin-b.test"}
	c.applyDefaults()
	c.HTTPClient.MaxIdleConnsPerHost = 33
	client := newHTTPClient(c)
	tr := client.Transport.(*http.Transport)
	// Two distinct origins: the total pool must be able to hold the per-host
	// pool of each of them, otherwise the global cap silently throttles one.
	if tr.MaxIdleConnsPerHost != 33 || tr.MaxIdleConns != 66 {
		t.Fatalf("want idle limits 33 per host / 66 total, got %d/%d", tr.MaxIdleConnsPerHost, tr.MaxIdleConns)
	}
	if !tr.ForceAttemptHTTP2 {
		t.Fatal("want ForceAttemptHTTP2")
	}
	if client.Timeout != 30*time.Second {
		t.Fatalf("want 30s timeout, got %v", client.Timeout)
	}
}

func TestNewHTTPClientCountsDistinctOriginsNotHosts(t *testing.T) {
	c := &Config{}
	c.AllowedHosts = map[string]string{"a.test": "shared.origin.test", "b.test": "shared.origin.test"}
	c.applyDefaults()
	c.HTTPClient.MaxIdleConnsPerHost = 33
	tr := newHTTPClient(c).Transport.(*http.Transport)
	if tr.MaxIdleConns != 33 {
		t.Fatalf("two hosts sharing one origin: want MaxIdleConns 33, got %d", tr.MaxIdleConns)
	}
}

func TestExporterUnauthenticatedOnPublicBind(t *testing.T) {
	cases := []struct {
		ip, user, pass string
		want           bool
	}{
		{"127.0.0.1", "", "", false},
		{"localhost", "", "", false},
		{"::1", "", "", false},
		{"0.0.0.0", "", "", true},
		{"0.0.0.0", "u", "", true},
		{"0.0.0.0", "u", "p", false},
		{"10.0.0.5", "", "p", true},
	}
	for _, tc := range cases {
		c := &Config{}
		c.Exporter.BindIP, c.Exporter.User, c.Exporter.Password = tc.ip, tc.user, tc.pass
		if got := c.exporterUnauthenticatedOnPublicBind(); got != tc.want {
			t.Errorf("bind=%q user=%q pass=%q: want %v, got %v", tc.ip, tc.user, tc.pass, tc.want, got)
		}
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
