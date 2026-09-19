package main

import (
	"bytes"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"runtime"
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
	h := NewHandler(cfg, cache, NewFetcher(newHTTPClient(cfg), cfg.Limits.MaxImageBytes, 64), NewConverter(runtime.NumCPU(), false), log)
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
