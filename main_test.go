package main

import (
	"bytes"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

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
	got := buildImageURL("cdn.example.com", "/a/b.jpg", "v=2&w=10")
	want := "https://cdn.example.com/a/b.jpg?v=2&w=10"
	if got != want {
		t.Fatalf("want %s, got %s", want, got)
	}
}

func TestBuildImageURLWithoutQueryString(t *testing.T) {
	got := buildImageURL("cdn.example.com", "/a/b.jpg", "")
	want := "https://cdn.example.com/a/b.jpg"
	if got != want {
		t.Fatalf("want %s, got %s", want, got)
	}
}

func testConfig(t *testing.T) {
	t.Helper()
	config = Config{}
	config.Cache.LruCache = 16
	config.Cache.CacheEnabled = true
	config.Cache.NoCacheHeader = "X-No-Cache"
	config.Concurrency.MaxGoroutines = 4
	config.WebP.Quality = 75
	config.Limits.MaxImageBytes = 10 << 20
	config.HTTPClient.TimeoutSeconds = 2
	// Point at a closed local port so unexpected upstream fetches fail fast.
	config.AllowedHosts = map[string]string{"cdn.test": "127.0.0.1:1"}
	upstreamScheme = "http://"
	t.Cleanup(func() { upstreamScheme = "https://" })
	if err := setupRuntime(); err != nil {
		t.Fatal(err)
	}
}

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

func TestHandleRequestRecordsActualStatusCode(t *testing.T) {
	testConfig(t)
	before := testutil.ToFloat64(requestsTotal.WithLabelValues("403", "GET"))

	req := httptest.NewRequest("GET", "/x.jpg", nil)
	req.Host = "not-allowed.test"
	rec := httptest.NewRecorder()
	newRouter().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", rec.Code)
	}
	after := testutil.ToFloat64(requestsTotal.WithLabelValues("403", "GET"))
	if after-before != 1 {
		t.Fatalf("want 403 counter +1, got +%v", after-before)
	}
}

func TestMetricsNotServedOnPublicPort(t *testing.T) {
	testConfig(t)
	req := httptest.NewRequest("GET", "/metrics", nil)
	req.Host = "cdn.test"
	rec := httptest.NewRecorder()
	newRouter().ServeHTTP(rec, req)
	if bytes.Contains(rec.Body.Bytes(), []byte("ngax_http_requests_total")) {
		t.Fatal("metrics exposed on the public router")
	}
}

func TestHealthIgnoresHostAllowlist(t *testing.T) {
	testConfig(t)
	req := httptest.NewRequest("GET", "/health", nil)
	req.Host = "not-allowed.test"
	rec := httptest.NewRecorder()
	newRouter().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "OK" {
		t.Fatalf("want 200 OK, got %d %q", rec.Code, rec.Body.String())
	}
}

func TestFetchAndConvertProducesWebP(t *testing.T) {
	testConfig(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(pngBytes(t))
	}))
	defer srv.Close()

	out, err := fetchAndConvert(srv.URL+"/a.png", 75)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(out, []byte("RIFF")) || !bytes.Contains(out[:16], []byte("WEBP")) {
		t.Fatalf("output is not WebP: %q", out[:16])
	}
}

func TestFetchAndConvertRejectsOversizedBody(t *testing.T) {
	testConfig(t)
	config.Limits.MaxImageBytes = 512
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(make([]byte, 4096))
	}))
	defer srv.Close()

	if _, err := fetchAndConvert(srv.URL+"/big.png", 75); err == nil {
		t.Fatal("want error for oversized body, got nil")
	}
}

func TestHandleRequestSkipsCacheWhenDisabled(t *testing.T) {
	testConfig(t)
	config.Cache.CacheEnabled = false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(pngBytes(t))
	}))
	defer upstream.Close()
	config.AllowedHosts["cdn.test"] = strings.TrimPrefix(upstream.URL, "http://")

	req := httptest.NewRequest("GET", "/a.png", nil)
	req.Host = "cdn.test"
	rec := httptest.NewRecorder()
	newRouter().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if imgCache.Len() != 0 {
		t.Fatalf("cache should be empty when disabled, has %d entries", imgCache.Len())
	}
}

func TestLoadConfigKeepsDottedHostKeys(t *testing.T) {
	dir := t.TempDir()
	yaml := "allowed_hosts:\n  example.com: \"images.example.com\"\nwebp:\n  quality: 60\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
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
