package main

import (
	"bytes"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func testConfig(t *testing.T) {
	t.Helper()
	config = Config{}
	config.Cache.CacheEnabled = true
	config.Cache.NoCacheHeader = "X-No-Cache"
	config.WebP.Quality = 75
	config.Limits.MaxImageBytes = 10 << 20
	// Point at a closed local port so unexpected upstream fetches fail fast.
	config.AllowedHosts = map[string]string{"cdn.test": "127.0.0.1:1"}
	config.applyDefaults()
	config.Cache.MaxBytes = 1 << 20
	config.Concurrency.MaxConversions = 4
	config.HTTPClient.TimeoutSeconds = 2
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
